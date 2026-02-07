package httpserver

import (
	"encoding/json"
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

func (a *API) handleSendSMS(w http.ResponseWriter, r *http.Request) {
	var req domain.SendSMSRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		http.Error(w, err.Error(), http.StatusBadGateway)
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
		http.Error(w, ErrDependency, http.StatusBadGateway)
		return
	}
	if !found {
		http.Error(w, ErrNotFound, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}
