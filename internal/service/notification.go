package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"notif/internal/domain"
	"notif/internal/observability"
	"notif/internal/store"
	"notif/internal/util"
)

type Store interface {
	CreateMessage(ctx context.Context, in store.CreateMessageInput) (store.CreateMessageResult, error)
	SetEnqueueFailed(ctx context.Context, id string, failed bool, now time.Time) error
	GetMessage(ctx context.Context, msgID string) (store.Message, bool, error)
}

type Queue interface {
	EnqueueSMS(ctx context.Context, tenantID, messageID, idempotencyKey, to, templateID string, vars map[string]string, campaignID string) error
}

// ErrIdempotencyConflict means the idempotency key is already bound to a
// different request. Answering with the original row would tell the caller a
// send it never made was accepted.
var ErrIdempotencyConflict = errors.New("idempotency key already used for a different request")

// ErrEnqueue means the message was accepted but could not be handed to the
// queue. The row stays 'queued' and marked, so a retry with the same
// idempotency key enqueues it rather than returning a dead message.
var ErrEnqueue = errors.New("message accepted but not queued; retry with the same idempotency key")

type NotificationService struct {
	Store     Store
	Queue     Queue
	MaxPerDay int
}

func (s *NotificationService) CreateAndEnqueueSMS(ctx context.Context, req domain.SendSMSRequest, messageID string, now time.Time) (domain.CreateResponse, error) {
	req.To = util.NormalizePhone(req.To)

	// 1) One round-trip decides everything the database owns: idempotency,
	// suppression, consent, the daily cap, and the message row itself.
	res, err := s.Store.CreateMessage(ctx, store.CreateMessageInput{
		ID:         messageID,
		TenantID:   req.TenantID,
		IdemKey:    req.IdempotencyKey,
		To:         req.To,
		TemplateID: req.TemplateID,
		Vars:       req.Vars,
		CampaignID: req.CampaignID,
		Day:        now,
		MaxPerDay:  s.MaxPerDay,
		Now:        now,
	})
	if err != nil {
		return domain.CreateResponse{}, err
	}

	if res.Existing {
		if !sameRequest(res, req) {
			return domain.CreateResponse{}, ErrIdempotencyConflict
		}
		// An idempotent retry returns whatever the first request decided —
		// with one exception. If the first request's enqueue failed, the row
		// was left 'queued' and marked, and this retry is the client doing
		// exactly what the 503 asked: enqueue it now. Without this the key
		// would be poisoned — every retry answering 'queued' for a message
		// nothing will ever send. Concurrent duplicates of a healthy request
		// carry no mark, so they still enqueue nothing.
		if !(res.State == string(domain.StateQueued) && res.LastError == store.LastErrorEnqueueFailed) {
			return domain.CreateResponse{MessageID: res.MessageID, State: res.State}, nil
		}
		messageID = res.MessageID
	} else if res.State != string(domain.StateQueued) {
		return domain.CreateResponse{MessageID: res.MessageID, State: res.State}, nil
	}

	// 2) enqueue
	if err := s.Queue.EnqueueSMS(ctx, req.TenantID, messageID, req.IdempotencyKey, req.To, req.TemplateID, req.Vars, req.CampaignID); err != nil {
		observability.Enqueues.WithLabelValues("error").Inc()
		// The row stays 'queued' — NOT failed. Whether the job reached SQS is
		// unknown (a timed-out batch may still have landed it), and the worker's
		// claim makes a second job harmless, so the safe move is to let the
		// client retry. The mark is written on a context of its own: the
		// request's may already be cancelled, which is often why this failed.
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if merr := s.Store.SetEnqueueFailed(mctx, messageID, true, now); merr != nil {
			slog.Error("could not mark enqueue failure; message stays queued unmarked",
				"message_id", messageID, "err", merr)
		}
		return domain.CreateResponse{}, fmt.Errorf("%w: %v", ErrEnqueue, err)
	}
	observability.Enqueues.WithLabelValues("ok").Inc()
	if res.Existing {
		// Best effort: a leftover mark only means one extra, deduplicated job
		// should the client retry again.
		_ = s.Store.SetEnqueueFailed(ctx, messageID, false, now)
	}

	return domain.CreateResponse{MessageID: messageID, State: string(domain.StateQueued)}, nil
}

// sameRequest reports whether a retry carries the request the idempotency key
// was first bound to. Phone numbers are compared normalised, as stored.
func sameRequest(res store.CreateMessageResult, req domain.SendSMSRequest) bool {
	if res.To != req.To || res.TemplateID != req.TemplateID || res.CampaignID != req.CampaignID {
		return false
	}
	if len(res.Vars) != len(req.Vars) {
		return false
	}
	for k, v := range req.Vars {
		if got, ok := res.Vars[k]; !ok || got != v {
			return false
		}
	}
	return true
}

func (s *NotificationService) GetMessage(ctx context.Context, msgID string) (store.Message, bool, error) {
	return s.Store.GetMessage(ctx, msgID)
}
