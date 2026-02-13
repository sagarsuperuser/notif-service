package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/mux"

	"notif/internal/observability"
	"notif/internal/store"
	"notif/internal/util"
)

type WebhookStore interface {
	InsertDeliveryEvent(ctx context.Context, in store.DeliveryEvent) error
	UpdateMessageByProviderMsgID(ctx context.Context, in store.ProviderMsgUpdate) (bool, error)
}

type Webhook struct {
	Store           WebhookStore
	VerifySignature func(authToken, fullURL, provided string, form url.Values) bool
	AuthToken       string
	PublicURL       string
}

func (w *Webhook) Register(mux *mux.Router) {
	mux.HandleFunc("/v1/webhooks/twilio/status", w.handleTwilioStatus).Methods(http.MethodPost)
}

func (w *Webhook) handleTwilioStatus(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, ErrBadForm, http.StatusBadRequest)
		return
	}
	if w.VerifySignature == nil || !w.VerifySignature(w.AuthToken, w.PublicURL, r.Header.Get("X-Twilio-Signature"), r.PostForm) {
		http.Error(rw, ErrInvalidSignature, http.StatusUnauthorized)
		return
	}

	msgSid := r.PostForm.Get("MessageSid")
	status := r.PostForm.Get("MessageStatus")
	errCode := r.PostForm.Get("ErrorCode")

	observability.WebhookEvents.WithLabelValues(status).Inc()

	if err := w.Store.InsertDeliveryEvent(r.Context(), store.DeliveryEvent{
		Provider:      "twilio",
		ProviderMsgID: msgSid,
		VendorStatus:  status,
		ErrorCode:     errCode,
		Payload:       r.PostForm,
		OccurredAt:    nil,
	}); err != nil {
		slog.Error("webhook insert delivery event failed", "err", err, "message_sid", msgSid, "status", status)
		http.Error(rw, ErrDependency, http.StatusInternalServerError)
		return
	}

	newState := ""
	switch status {
	case "delivered":
		newState = "delivered"
	case "failed", "undelivered":
		newState = "failed"
	default:
		rw.WriteHeader(http.StatusOK)
		return
	}

	// Webhooks can arrive before the worker has persisted provider_msg_id into messages.
	// If we update once and drop the result, messages can get stuck in "submitted" forever.
	// Retry briefly; if still not found, return non-2xx so the provider can retry delivery.
	var updated bool
	var lastUpdateErr error
	for attempt := 0; attempt < 10; attempt++ {
		updated, lastUpdateErr = w.Store.UpdateMessageByProviderMsgID(r.Context(), store.ProviderMsgUpdate{
			Provider:      "twilio",
			ProviderMsgID: msgSid,
			NewState:      newState,
			LastError:     errCode,
			Now:           util.NowUTC(),
		})
		if lastUpdateErr != nil {
			break
		}
		if updated {
			rw.WriteHeader(http.StatusOK)
			return
		}

		// Backoff: 25ms, 50ms, 75ms, ... up to 250ms. Total worst-case ~1.4s.
		sleep := time.Duration(25*(attempt+1)) * time.Millisecond
		t := time.NewTimer(sleep)
		select {
		case <-r.Context().Done():
			t.Stop()
			http.Error(rw, ErrDependency, http.StatusServiceUnavailable)
			return
		case <-t.C:
		}
	}

	if lastUpdateErr != nil {
		slog.Error("webhook update message failed", "err", lastUpdateErr, "message_sid", msgSid, "status", status, "new_state", newState)
		http.Error(rw, ErrDependency, http.StatusInternalServerError)
		return
	}

	// Not updated after retries: ask the provider to retry (prevents stuck "submitted").
	slog.Warn("webhook message not found for provider msg id (retry later)",
		"provider", "twilio",
		"message_sid", msgSid,
		"status", status,
		"new_state", newState,
	)
	http.Error(rw, ErrDependency, http.StatusServiceUnavailable)
}
