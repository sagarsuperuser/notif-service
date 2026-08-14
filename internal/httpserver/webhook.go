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
	RecordDeliveryEvent(ctx context.Context, in store.DeliveryEventRecord) (bool, error)
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

// handleTwilioStatus ingests one provider callback with exactly one database
// round-trip and answers 200 for every outcome the provider can act on.
//
// Two deliberate departures from the previous implementation, both of which
// were the cause of database timeouts under load rather than a symptom:
//
//   - No retry loop. The previous version retried the message UPDATE up to ten
//     times with backoff sleeps totalling ~1.4s while HOLDING a pooled
//     connection. At webhook rates a few times the send rate, that exhausts a
//     20-connection pool almost immediately; requests then queue waiting for a
//     connection and time out. The database itself is barely involved.
//   - No 503 when the message row is missing. A callback legitimately races the
//     worker's provider_msg_id write, and answering 503 made the provider
//     redeliver the whole event — so the failure generated more load, which
//     caused more failures. The event is recorded regardless and the reconcile
//     job repairs the message, so a miss costs one reconcile interval instead
//     of a redelivery storm.
//
// 5xx is now reserved for a genuine store failure, where a provider retry is
// the correct response.
func (w *Webhook) handleTwilioStatus(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, ErrBadForm, http.StatusBadRequest)
		return
	}
	if w.VerifySignature == nil || !w.VerifySignature(w.AuthToken, w.PublicURL, r.Header.Get("X-Twilio-Signature"), r.PostForm) {
		http.Error(rw, ErrInvalidSignature, http.StatusUnauthorized)
		return
	}
	if w.Store == nil {
		http.Error(rw, ErrDependency, http.StatusInternalServerError)
		return
	}

	msgSid := r.PostForm.Get("MessageSid")
	status := r.PostForm.Get("MessageStatus")
	errCode := r.PostForm.Get("ErrorCode")

	observability.WebhookEvents.WithLabelValues(status).Inc()

	newState := ""
	switch status {
	case "delivered":
		newState = "delivered"
	case "failed", "undelivered":
		newState = "failed"
	}

	// Not bound to the client connection: the provider can time out and hang up
	// while we still want the event persisted.
	dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	matched, err := w.Store.RecordDeliveryEvent(dbCtx, store.DeliveryEventRecord{
		Provider:      "twilio",
		ProviderMsgID: msgSid,
		VendorStatus:  status,
		ErrorCode:     errCode,
		Payload:       r.PostForm,
		OccurredAt:    nil,
		NewState:      newState,
		Now:           util.NowUTC(),
	})
	if err != nil {
		slog.Error("webhook record delivery event failed",
			"err", err, "message_sid", msgSid, "status", status)
		http.Error(rw, ErrDependency, http.StatusServiceUnavailable)
		return
	}

	// A terminal event that matched no message row: the callback beat the
	// worker's write, or the message predates this provider id. The event is
	// already stored, so reconcile-submitted will apply it. Counted so the rate
	// is visible rather than inferred.
	if newState != "" && !matched {
		observability.WebhookMessageUpdateNotFound.WithLabelValues(status).Inc()
		slog.Warn("webhook event stored but no message matched; reconcile will apply it",
			"provider", "twilio", "message_sid", msgSid, "status", status, "new_state", newState)
	}

	rw.WriteHeader(http.StatusOK)
}
