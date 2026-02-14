package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"notif/internal/awsutil"
	"notif/internal/config"
	"notif/internal/httpserver"
	"notif/internal/logging"
	sqsqueue "notif/internal/queue/sqs"
	"notif/internal/store/pg"
	"notif/internal/store"
	"notif/internal/util"
)

func main() {
	cfg := config.LoadWebhookProcessor()
	logging.Init("webhook-processor", cfg.LogFormat)

	ctx, cancel := context.WithCancel(context.Background())

	db, err := pg.NewPool(ctx, cfg.DBDSN, pg.PoolOptions{
		MaxConns:          cfg.DBPoolMaxConns,
		MinConns:          cfg.DBPoolMinConns,
		MaxConnLifetime:   cfg.DBPoolMaxConnLifetime,
		MaxConnIdleTime:   cfg.DBPoolMaxConnIdleTime,
		HealthCheckPeriod: cfg.DBPoolHealthCheckPeriod,
	})
	if err != nil {
		slog.Error("webhook-processor db connect failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	dbStore := pg.New(db)

	sqsClient, err := awsutil.NewSQSClient(ctx, cfg.AWSRegion, cfg.LocalstackEndpoint)
	if err != nil {
		slog.Error("webhook-processor sqs client init failed", "err", err)
		os.Exit(1)
	}

	consumer := &sqsqueue.WebhookConsumer{
		SQS:               sqsClient,
		QueueURL:          cfg.WebhookEventsQueueURL,
		WaitTimeSeconds:   cfg.SQSWaitTime,
		MaxMessages:       cfg.SQSMaxMsgs,
		VisibilityTimeout: cfg.SQSVizTimeout,
	}

	// health + metrics servers
	healthMux := httpserver.New().Mux
	healthMux.Use(httpserver.Logging)
	healthMux.HandleFunc("/healthz", httpserver.Readyz(2*time.Second,
		func(c context.Context) error { return db.Ping(c) },
		func(c context.Context) error {
			_, err := sqsClient.GetQueueAttributes(c, &sqs.GetQueueAttributesInput{
				QueueUrl:       &cfg.WebhookEventsQueueURL,
				AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
			})
			return err
		},
	)).Methods(http.MethodGet)

	healthSrv := &http.Server{Addr: ":" + cfg.Port, Handler: healthMux}
	metricsSrv := &http.Server{Addr: ":" + cfg.MetricsPort, Handler: promhttp.Handler()}

	healthErrCh := make(chan error, 1)
	go func() {
		slog.Info("webhook-processor health listening", "port", cfg.Port)
		healthErrCh <- healthSrv.ListenAndServe()
	}()
	metricsErrCh := make(chan error, 1)
	go func() {
		slog.Info("webhook-processor metrics listening", "port", cfg.MetricsPort)
		metricsErrCh <- metricsSrv.ListenAndServe()
	}()

	// start polling
	pollErrCh := make(chan error, 1)
	go func() {
		slog.Info("webhook-processor starting poll", "queue_url", cfg.WebhookEventsQueueURL)
		pollErrCh <- consumer.PollConcurrent(ctx, cfg.ProcessorConcurrency, func(ctx context.Context, ev sqsqueue.WebhookEvent) error {
			return processWebhookEvent(ctx, dbStore, ev)
		})
	}()

	// shutdown wiring
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-pollErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("webhook-processor poll failed", "err", err)
			os.Exit(1)
		}
	case err := <-healthErrCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("webhook-processor health server failed", "err", err)
			os.Exit(1)
		}
	case err := <-metricsErrCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("webhook-processor metrics server failed", "err", err)
			os.Exit(1)
		}
	case sig := <-sigCh:
		slog.Info("webhook-processor shutdown", "signal", sig.String())
	}

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = healthSrv.Shutdown(shutdownCtx)
	_ = metricsSrv.Shutdown(shutdownCtx)

	select {
	case <-pollErrCh:
	case <-time.After(10 * time.Second):
		slog.Info("webhook-processor shutdown timeout waiting for poll loop")
	}
}

func processWebhookEvent(ctx context.Context, st *pg.Store, ev sqsqueue.WebhookEvent) error {
	newState := ""
	switch ev.Status {
	case "delivered":
		newState = "delivered"
	case "failed", "undelivered":
		newState = "failed"
	}

	// Make DB work bounded. Errors should cause SQS redrive.
	dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// For terminal events, we prefer "eventually apply" to messages. If the message is not found yet
	// (worker hasn't persisted provider_msg_id), returning an error lets SQS retry later.
	if newState != "" {
		updated, err := st.UpdateMessageByProviderMsgID(dbCtx, store.ProviderMsgUpdate{
			Provider:      ev.Provider,
			ProviderMsgID: ev.ProviderMsgID,
			NewState:      newState,
			LastError:     ev.ErrorCode,
			Now:           util.NowUTC(),
		})
		if err != nil {
			return err
		}
		if !updated {
			return errors.New("message not found for provider_msg_id")
		}
	}

	// Persist the event (payload omitted to reduce DB load).
	return st.InsertDeliveryEvent(dbCtx, store.DeliveryEvent{
		Provider:      ev.Provider,
		ProviderMsgID: ev.ProviderMsgID,
		VendorStatus:  ev.Status,
		ErrorCode:     ev.ErrorCode,
		Payload:       nil,
		OccurredAt:    nil,
	})
}
