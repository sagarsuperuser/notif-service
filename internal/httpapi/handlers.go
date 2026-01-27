package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"notif/internal/domain"
	"notif/internal/observability"
	"notif/internal/service"
)

type API struct {
	Svc   *service.NotificationService
	IDGen func() string
}

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/sms/messages", a.handleSendSMS)
	mux.HandleFunc("/v1/messages/", a.handleGetMessage) // /v1/messages/{id}
}

func (a *API) handleSendSMS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req domain.SendSMSRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		observability.APIRequests.WithLabelValues("/v1/sms/messages", "400").Inc()
		http.Error(w, "invalid json", 400)
		return
	}
	if req.TenantID == "" || req.IdempotencyKey == "" || req.To == "" || req.TemplateID == "" {
		observability.APIRequests.WithLabelValues("/v1/sms/messages", "400").Inc()
		http.Error(w, "missing fields", 400)
		return
	}

	resp, err := a.Svc.CreateAndEnqueueSMS(r.Context(), req, a.IDGen(), time.Now())
	if err != nil {
		slog.Error("create and enqueue sms failed",
			"err", err,
			"tenant_id", req.TenantID,
			"idempotency_key", req.IdempotencyKey,
			"to", req.To,
			"template_id", req.TemplateID,
		)
		observability.APIRequests.WithLabelValues("/v1/sms/messages", "502").Inc()
		http.Error(w, err.Error(), 502)
		return
	}

	observability.APIRequests.WithLabelValues("/v1/sms/messages", "202").Inc()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *API) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/messages/")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}
	msg, found, err := a.Svc.GetMessage(r.Context(), id)
	if err != nil {
		slog.Error("get message failed", "err", err, "id", id)
		http.Error(w, "db error", 502)
		return
	}
	if !found {
		http.Error(w, "not found", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}
