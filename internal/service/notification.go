package service

import (
	"context"
	"time"

	"notif/internal/domain"
	"notif/internal/observability"
	"notif/internal/util"
)

type Store interface {
	FindMessageByIdempotency(ctx context.Context, tenantID, idemKey string) (msgID, state string, found bool, err error)
	InsertMessage(ctx context.Context, msgID, tenantID, idemKey, to, templateID string, vars map[string]string, campaignID string, state string, now time.Time) error
	MarkMessageState(ctx context.Context, msgID, state, lastError string, now time.Time) error
	GetMessage(ctx context.Context, msgID string) (any, bool, error)
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
	if mid, st, found, err := s.Store.FindMessageByIdempotency(ctx, req.TenantID, req.IdempotencyKey); err != nil {
		return domain.CreateResponse{}, err
	} else if found {
		return domain.CreateResponse{MessageID: mid, State: st}, nil
	}

	// 2) create message row
	if err := s.Store.InsertMessage(ctx, messageID, req.TenantID, req.IdempotencyKey, req.To, req.TemplateID, req.Vars, req.CampaignID, string(domain.StateQueued), now); err != nil {
		return domain.CreateResponse{}, err
	}

	// 3) suppression
	if sup, err := s.Store.IsSuppressed(ctx, req.TenantID, req.To); err != nil {
		return domain.CreateResponse{}, err
	} else if sup {
		observability.Suppressed.WithLabelValues("suppression_list").Inc()
		if err := s.Store.MarkMessageState(ctx, messageID, string(domain.StateSuppressed), "suppressed", now); err != nil {
		}
		return domain.CreateResponse{MessageID: messageID, State: string(domain.StateSuppressed)}, nil
	}

	// 4) consent
	if ok, err := s.Store.IsOptedIn(ctx, req.TenantID, req.To); err != nil {
		return domain.CreateResponse{}, err
	} else if !ok {
		observability.Suppressed.WithLabelValues("not_opted_in").Inc()
		if err := s.Store.MarkMessageState(ctx, messageID, string(domain.StateSuppressed), "not_opted_in", now); err != nil {
		}
		return domain.CreateResponse{MessageID: messageID, State: string(domain.StateSuppressed)}, nil
	}

	// 5) caps
	allowed, _, err := s.Store.IncrementDailyCap(ctx, req.TenantID, req.To, now, s.MaxPerDay)
	if err != nil {
		return domain.CreateResponse{}, err
	}
	if !allowed {
		observability.Suppressed.WithLabelValues("cap_exceeded").Inc()
		if err := s.Store.MarkMessageState(ctx, messageID, string(domain.StateSuppressed), "cap_exceeded", now); err != nil {
		}
		return domain.CreateResponse{MessageID: messageID, State: string(domain.StateSuppressed)}, nil
	}

	// 6) enqueue
	if err := s.Queue.EnqueueSMS(ctx, req.TenantID, messageID, req.IdempotencyKey, req.To, req.TemplateID, req.Vars, req.CampaignID); err != nil {
		observability.Enqueues.WithLabelValues("error").Inc()
		if err := s.Store.MarkMessageState(ctx, messageID, string(domain.StateFailed), "enqueue_failed", now); err != nil {
		}
		return domain.CreateResponse{}, err
	}
	observability.Enqueues.WithLabelValues("ok").Inc()

	return domain.CreateResponse{MessageID: messageID, State: string(domain.StateQueued)}, nil
}

func (s *NotificationService) GetMessage(ctx context.Context, msgID string) (any, bool, error) {
	return s.Store.GetMessage(ctx, msgID)
}
