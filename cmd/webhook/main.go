package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"notif/internal/config"
	"notif/internal/httpserver"
	"notif/internal/logging"
	"notif/internal/observability"
	"notif/internal/providers/twilio"
	"notif/internal/store/pg"
)

func main() {
	cfg := config.LoadWebhook()
	logging.Init("webhook", cfg.LogFormat)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := pg.NewPool(ctx, cfg.DBDSN, pg.PoolOptions{
		MaxConns:          cfg.DBPoolMaxConns,
		MinConns:          cfg.DBPoolMinConns,
		MaxConnLifetime:   cfg.DBPoolMaxConnLifetime,
		MaxConnIdleTime:   cfg.DBPoolMaxConnIdleTime,
		HealthCheckPeriod: cfg.DBPoolHealthCheckPeriod,
	})
	if err != nil {
		slog.Error("webhook db connect failed", "err", err)
		os.Exit(1)
	}
	dbStore := pg.New(db)

	reg := prometheus.DefaultRegisterer
	observability.RegisterWebhook(reg)

	s := httpserver.New()
	s.Mux.Use(httpserver.Metrics(observability.WebhookRequests))
	s.Mux.Use(httpserver.Logging)

	s.Mux.HandleFunc("/healthz", httpserver.Healthz()).Methods(http.MethodGet)
	s.Mux.HandleFunc("/readyz", httpserver.Readyz(2*time.Second, func(ctx context.Context) error {
		return db.Ping(ctx)
	})).Methods(http.MethodGet)

	webhook := &httpserver.Webhook{
		Store:           dbStore,
		VerifySignature: twilio.VerifySignature,
		AuthToken:       cfg.TwilioAuthToken,
		PublicURL:       cfg.PublicWebhookURL,
	}
	webhook.Register(s.Mux)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: s.Mux,
	}
	metricsSrv := &http.Server{
		Addr:    ":" + cfg.MetricsPort,
		Handler: promhttp.Handler(),
	}

	metricsErrCh := make(chan error, 1)
	go func() {
		slog.Info("webhook metrics listening", "port", cfg.MetricsPort)
		metricsErrCh <- metricsSrv.ListenAndServe()
	}()

	slog.Info("webhook listening", "port", cfg.Port)
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("webhook shutdown", "signal", sig.String())
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	select {
	case err := <-serverErrCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("webhook server failed", "err", err)
			os.Exit(1)
		}
	case err := <-metricsErrCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("webhook metrics server failed", "err", err)
			os.Exit(1)
		}
	}
	db.Close()
}
