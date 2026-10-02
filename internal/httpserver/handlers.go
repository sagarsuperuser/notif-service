package httpserver

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"notif/internal/domain"
	"notif/internal/service"
	"notif/internal/util"

	"github.com/gorilla/mux"
)

type API struct {
	Svc   *service.NotificationService
	IDGen func() string
}

func (a *API) Register(mux *mux.Router) {
	mux.HandleFunc("/v1/sms/messages", a.handleSendSMS).Methods(http.MethodPost)
	mux.HandleFunc("/v1/messages/{id}", a.handleGetMessage).Methods(http.MethodGet)
}

// maxSendBody bounds a send request. The largest legitimate body — every field
// at its domain limit — is a few KiB; without a bound, one oversized request is
// read whole into memory and then fails the queue batch it lands in, taking its
// batch-mates down with it.
const maxSendBody = 64 << 10

func (a *API) handleSendSMS(w http.ResponseWriter, r *http.Request) {
	var req domain.SendSMSRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxSendBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, ErrBodyTooLarge, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, ErrInvalidJSON, http.StatusBadRequest)
		return
	}
	if err := req.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := a.Svc.CreateAndEnqueueSMS(r.Context(), req, a.IDGen(), util.NowUTC())
	if err != nil {
		slog.Error("create and enqueue sms failed",
			"err", err,
			"tenant_id", req.TenantID,
			"idempotency_key", req.IdempotencyKey,
			"to", req.To,
			"template_id", req.TemplateID,
		)
		// Fixed messages only: the raw error carries SQL and SQS internals.
		switch {
		case errors.Is(err, service.ErrIdempotencyConflict):
			http.Error(w, ErrIdempotencyConflict, http.StatusConflict)
		default:
			// The database or the queue is unavailable. 503 (not 502) with a
			// Retry-After: the request is safe to repeat with the same
			// idempotency key, and the enqueue-failure path is built for that.
			w.Header().Set("Retry-After", "1")
			http.Error(w, ErrDependency, http.StatusServiceUnavailable)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *API) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		http.Error(w, ErrMissingID, http.StatusBadRequest)
		return
	}
	msg, found, err := a.Svc.GetMessage(r.Context(), id)
	if err != nil {
		slog.Error("get message failed", "err", err, "id", id)
		w.Header().Set("Retry-After", "1")
		http.Error(w, ErrDependency, http.StatusServiceUnavailable)
		return
	}
	if !found {
		http.Error(w, ErrNotFound, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}
