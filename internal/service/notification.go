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
	FindMessageByIdempotency(ctx context.Context, tenantID, idemKey string) (store.IdempotencyResult, error)
	InsertMessage(ctx context.Context, in store.MessageInsert) error
	MarkMessageState(ctx context.Context, in store.MessageStateUpdate) error
	GetMessage(ctx context.Context, msgID string) (store.Message, bool, error)
	IsSuppressed(ctx context.Context, tenantID, phone string) (bool, error)
	IsOptedIn(ctx context.Context, tenantID, phone string) (bool, error)
	IncrementDailyCap(ctx context.Context, tenantID, phone string, day time.Time, maxPerDay int) (allowed bool, newCount int, err error)
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

	// 1) idempotency
	if res, err := s.Store.FindMessageByIdempotency(ctx, req.TenantID, req.IdempotencyKey); err != nil {
		return domain.CreateResponse{}, err
	} else if res.Found {
		return domain.CreateResponse{MessageID: res.MessageID, State: res.State}, nil
	}

	// 2) create message row
	if err := s.Store.InsertMessage(ctx, store.MessageInsert{
		ID:         messageID,
		TenantID:   req.TenantID,
		IdemKey:    req.IdempotencyKey,
		To:         req.To,
		TemplateID: req.TemplateID,
		Vars:       req.Vars,
		CampaignID: req.CampaignID,
		State:      string(domain.StateQueued),
		Now:        now,
	}); err != nil {
		return domain.CreateResponse{}, err
	}

	// 3) suppression
	isSup, err := s.Store.IsSuppressed(ctx, req.TenantID, req.To)
	if err != nil {
		return domain.CreateResponse{}, err
	} else if isSup {
		if err := s.Store.MarkMessageState(ctx, store.MessageStateUpdate{
			ID:        messageID,
			State:     string(domain.StateSuppressed),
			LastError: "suppressed",
			Now:       now,
		}); err != nil {
			return domain.CreateResponse{}, err
		}
		return domain.CreateResponse{MessageID: messageID, State: string(domain.StateSuppressed)}, nil
	}

	// 4) consent
	if ok, err := s.Store.IsOptedIn(ctx, req.TenantID, req.To); err != nil {
		return domain.CreateResponse{}, err
	} else if !ok {
		if err := s.Store.MarkMessageState(ctx, store.MessageStateUpdate{
			ID:        messageID,
			State:     string(domain.StateSuppressed),
			LastError: "not_opted_in",
			Now:       now,
		}); err != nil {
		}
		return domain.CreateResponse{MessageID: messageID, State: string(domain.StateSuppressed)}, nil
	}

	// 5) caps
	allowed, _, err := s.Store.IncrementDailyCap(ctx, req.TenantID, req.To, now, s.MaxPerDay)
	if err != nil {
		return domain.CreateResponse{}, err
	}
	if !allowed {
		if err := s.Store.MarkMessageState(ctx, store.MessageStateUpdate{
			ID:        messageID,
			State:     string(domain.StateSuppressed),
			LastError: "cap_exceeded",
			Now:       now,
		}); err != nil {
		}
		return domain.CreateResponse{MessageID: messageID, State: string(domain.StateSuppressed)}, nil
	}

	// 6) enqueue
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
