package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"notif/internal/config"
	"notif/internal/httpapi"
	"notif/internal/logging"
	"notif/internal/observability"
	"notif/internal/providers/twilio"
	"notif/internal/store/pg"
)

func main() {
	logging.Init("webhook")

	cfg := config.LoadWebhook()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		slog.Error("webhook db connect failed", "err", err)
		os.Exit(1)
	}
	store := pg.New(db)

	reg := prometheus.DefaultRegisterer
	observability.Register(reg)

	s := httpapi.New()
	s.Mux.HandleFunc("/v1/webhooks/twilio/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", 400)
			return
		}
		sig := r.Header.Get("X-Twilio-Signature")
		if !twilio.VerifySignature(cfg.TwilioAuthToken, cfg.PublicWebhookURL, sig, r.PostForm) {
			http.Error(w, "invalid signature", 401)
			return
		}

		msgSid := r.PostForm.Get("MessageSid")
		status := r.PostForm.Get("MessageStatus")
		errCode := r.PostForm.Get("ErrorCode")

		observability.WebhookEvents.WithLabelValues(status).Inc()

		if err := store.InsertDeliveryEvent(r.Context(), "twilio", msgSid, status, errCode, r.PostForm, nil); err != nil {
		}

		// map Twilio status -> our state
		newState := ""
		switch status {
		case "delivered":
			newState = "delivered"
		case "failed", "undelivered":
			newState = "failed"
		default:
			// intermediate; do not downgrade state
			w.WriteHeader(200)
			return
		}

		if _, err := store.UpdateMessageByProviderMsgID(r.Context(), "twilio", msgSid, newState, errCode, time.Now()); err != nil {
		}
		w.WriteHeader(200)
	})

	s.Mux.HandleFunc("/healthz", httpapi.Healthz())
	s.Mux.HandleFunc("/readyz", httpapi.Readyz(2*time.Second, func(ctx context.Context) error {
		return db.Ping(ctx)
	}))

	handler := httpapi.Logging(s.Mux)
	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: handler,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("webhook shutdown", "signal", sig.String())
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("webhook listening", "port", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("webhook server failed", "err", err)
		os.Exit(1)
	}
	db.Close()
}
