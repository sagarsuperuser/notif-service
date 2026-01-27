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

	"notif/internal/awsutil"
	"notif/internal/config"
	"notif/internal/httpapi"
	"notif/internal/logging"
	"notif/internal/observability"
	sqsqueue "notif/internal/queue/sqs"
	"notif/internal/service"
	"notif/internal/store/pg"
	"notif/internal/util"
)

func main() {
	logging.Init("api")

	cfg := config.LoadAPI()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		slog.Error("api db connect failed", "err", err)
		os.Exit(1)
	}

	sqsClient, err := awsutil.NewSQSClient(ctx, cfg.AWSRegion)
	if err != nil {
		slog.Error("api sqs client init failed", "err", err)
		os.Exit(1)
	}

	observability.Register(prometheus.DefaultRegisterer)

	store := pg.New(db)
	producer := &sqsqueue.Producer{SQS: sqsClient, QueueURL: cfg.SQSQueueURL}

	svc := &service.NotificationService{
		Store:     store,
		Queue:     producer,
		MaxPerDay: cfg.MaxSMSPerDay,
	}

	s := httpapi.New()
	api := &httpapi.API{
		Svc:   svc,
		IDGen: util.NewMessageID,
	}
	api.Register(s.Mux)

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
		slog.Info("api shutdown", "signal", sig.String())
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("api listening", "port", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("api server failed", "err", err)
		os.Exit(1)
	}

	db.Close()
}
