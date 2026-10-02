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

	db, err := pg.NewPool(ctx, cfg.DBDSN, pg.PoolOptions{
		MaxConns:          cfg.DBPoolMaxConns,
		MinConns:          cfg.DBPoolMinConns,
		MaxConnLifetime:   cfg.DBPoolMaxConnLifetime,
		MaxConnIdleTime:   cfg.DBPoolMaxConnIdleTime,
		HealthCheckPeriod: cfg.DBPoolHealthCheckPeriod,
		Tracer:            &observability.QueryTracer{Service: "api"},
	})
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
	observability.RegisterDB(prometheus.DefaultRegisterer)
	// Pool statistics are how pool exhaustion is told apart from a slow
	// database; without them the two look identical from the outside.
	go observability.SamplePool(ctx, "api", db, 5*time.Second)

	store := pg.New(db)
	sendDelay, err := time.ParseDuration(cfg.SQSSendBatchDelay)
	if err != nil {
		slog.Error("invalid SQS_SEND_BATCH_DELAY", "err", err, "value", cfg.SQSSendBatchDelay)
		os.Exit(1)
	}
	producer := &sqsqueue.Producer{
		SQS:      sqsClient,
		QueueURL: cfg.SQSQueueURL,
		MaxBatch: cfg.SQSSendBatchSize,
		MaxDelay: sendDelay,
	}

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
		// Without these a slow or idle client holds a connection and a goroutine
		// indefinitely. The accept path itself is milliseconds.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
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

	// Shutdown order matters, and main must WAIT for it.
	//
	// srv.Shutdown makes ListenAndServe return ErrServerClosed at once, before
	// in-flight handlers finish. The previous version returned from main on
	// that, closing the database and exiting while requests were still between
	// "row committed" and "job enqueued" — leaving 'queued' rows with no job.
	// So: stop accepting and drain handlers, then flush the producer, then
	// close the database, and only then return.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sig := <-sigCh
		slog.Info("api shutdown", "signal", sig.String())
		// Within terminationGracePeriodSeconds (20s), leaving room for the
		// producer flush and the database close that follow.
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("api drain did not complete", "err", err)
		}
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	select {
	case err := <-serverErrCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("api server failed", "err", err)
			os.Exit(1)
		}
		<-shutdownDone
	case err := <-metricsErrCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("api metrics server failed", "err", err)
			os.Exit(1)
		}
		<-shutdownDone
	}

	producer.Close() // flushes anything still batched
	cancel()
	db.Close()
}
