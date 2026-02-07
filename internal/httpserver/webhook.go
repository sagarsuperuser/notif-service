package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"

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

	if _, err := w.Store.UpdateMessageByProviderMsgID(r.Context(), store.ProviderMsgUpdate{
		Provider:      "twilio",
		ProviderMsgID: msgSid,
		NewState:      newState,
		LastError:     errCode,
		Now:           util.NowUTC(),
	}); err != nil {
		slog.Error("webhook update message failed", "err", err, "message_sid", msgSid, "status", status, "new_state", newState)
		http.Error(rw, ErrDependency, http.StatusInternalServerError)
		return
	}
	rw.WriteHeader(http.StatusOK)
}
