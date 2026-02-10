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
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"notif/internal/awsutil"
	"notif/internal/config"
	"notif/internal/httpserver"
	"notif/internal/logging"
	"notif/internal/observability"
	sqsqueue "notif/internal/queue/sqs"
	"notif/internal/service"
	"notif/internal/store/pg"
	"notif/internal/util"
)

func main() {
	cfg := config.LoadAPI()
	logging.Init("api", cfg.LogFormat)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		slog.Error("api db connect failed", "err", err)
		os.Exit(1)
	}

	sqsClient, err := awsutil.NewSQSClient(ctx, cfg.AWSRegion, cfg.LocalstackEndpoint)
	if err != nil {
		slog.Error("api sqs client init failed", "err", err)
		os.Exit(1)
	}

	observability.RegisterAPI(prometheus.DefaultRegisterer)

	store := pg.New(db)
	producer := &sqsqueue.Producer{SQS: sqsClient, QueueURL: cfg.SQSQueueURL, GroupBuckets: cfg.SQSGroupBuckets}

	svc := &service.NotificationService{
		Store:     store,
		Queue:     producer,
		MaxPerDay: cfg.MaxSMSPerDay,
	}

	s := httpserver.New()
	s.Mux.Use(httpserver.Metrics(observability.APIRequests))
	s.Mux.Use(httpserver.Logging)
	api := &httpserver.API{
		Svc:   svc,
		IDGen: util.NewMessageID,
	}
	api.Register(s.Mux)

	s.Mux.HandleFunc("/healthz", httpserver.Healthz()).Methods(http.MethodGet)
	s.Mux.HandleFunc("/readyz", httpserver.Readyz(2*time.Second, func(ctx context.Context) error {
		return db.Ping(ctx)
	})).Methods(http.MethodGet)

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
		slog.Info("api metrics listening", "port", cfg.MetricsPort)
		metricsErrCh <- metricsSrv.ListenAndServe()
	}()

	slog.Info("api listening", "port", cfg.Port)
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("api shutdown", "signal", sig.String())
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	select {
	case err := <-serverErrCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("api server failed", "err", err)
			os.Exit(1)
		}
	case err := <-metricsErrCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("api metrics server failed", "err", err)
			os.Exit(1)
		}
	}

	db.Close()
}
