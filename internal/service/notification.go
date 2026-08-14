package service

import (
	"context"
	"time"

	"notif/internal/domain"
	"notif/internal/observability"
	"notif/internal/store"
	"notif/internal/util"
)

type Store interface {
	CreateMessage(ctx context.Context, in store.CreateMessageInput) (store.CreateMessageResult, error)
	MarkMessageState(ctx context.Context, in store.MessageStateUpdate) error
	GetMessage(ctx context.Context, msgID string) (store.Message, bool, error)
}

type Queue interface {
	EnqueueSMS(ctx context.Context, tenantID, messageID, idempotencyKey, to, templateID string, vars map[string]string, campaignID string) error
}

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

	// An idempotent retry returns whatever the first request decided — including
	// re-enqueueing nothing, since the original already did.
	if res.Existing {
		return domain.CreateResponse{MessageID: res.MessageID, State: res.State}, nil
	}
	if res.State != string(domain.StateQueued) {
		return domain.CreateResponse{MessageID: res.MessageID, State: res.State}, nil
	}

	// 2) enqueue
	if err := s.Queue.EnqueueSMS(ctx, req.TenantID, messageID, req.IdempotencyKey, req.To, req.TemplateID, req.Vars, req.CampaignID); err != nil {
		observability.Enqueues.WithLabelValues("error").Inc()
		if err := s.Store.MarkMessageState(ctx, store.MessageStateUpdate{
			ID:        messageID,
			State:     string(domain.StateFailed),
			LastError: "enqueue_failed",
			Now:       now,
		}); err != nil {
		}
		return domain.CreateResponse{}, err
	}
	observability.Enqueues.WithLabelValues("ok").Inc()

	return domain.CreateResponse{MessageID: messageID, State: string(domain.StateQueued)}, nil
}

func (s *NotificationService) GetMessage(ctx context.Context, msgID string) (store.Message, bool, error) {
	return s.Store.GetMessage(ctx, msgID)
}
