package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"notif/internal/awsutil"
	"notif/internal/config"
	"notif/internal/httpapi"
	"notif/internal/logging"
	"notif/internal/observability"
	"notif/internal/providers/twilio"
	sqsqueue "notif/internal/queue/sqs"
	"notif/internal/store/pg"
	workerproc "notif/internal/worker"

	"github.com/sony/gobreaker"
	"golang.org/x/time/rate"
)

func main() {
	logging.Init("worker")

	cfg := config.LoadWorker()

	// Use a root ctx we can cancel
	ctx, cancel := context.WithCancel(context.Background())

	db, err := pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		slog.Error("worker db connect failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	store := pg.New(db)

	sqsClient, err := awsutil.NewSQSClient(ctx, cfg.AWSRegion)
	if err != nil {
		slog.Error("worker sqs client init failed", "err", err)
		os.Exit(1)
	}

	startupCtx, startupCancel := context.WithTimeout(ctx, 3*time.Second)
	defer startupCancel()

	if err := db.Ping(startupCtx); err != nil {
		slog.Error("db not reachable", "err", err)
		os.Exit(1)
	}
	if _, err := sqsClient.GetQueueAttributes(startupCtx, &sqs.GetQueueAttributesInput{
		QueueUrl:       &cfg.SQSQueueURL,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	}); err != nil {
		slog.Error("sqs not reachable", "err", err)
		os.Exit(1)
	}

	reg := prometheus.DefaultRegisterer
	observability.Register(reg)

	consumer := &sqsqueue.Consumer{
		SQS: sqsClient, QueueURL: cfg.SQSQueueURL,
		WaitTimeSeconds:   cfg.SQSWaitTime,
		MaxMessages:       cfg.SQSMaxMsgs,
		VisibilityTimeout: cfg.SQSVizTimeout,
	}

	// health server (liveness + readiness)
	healthMux := httpapi.New().Mux
	healthMux.HandleFunc("/healthz", httpapi.Healthz())
	healthMux.HandleFunc("/readyz", httpapi.Readyz(2*time.Second,
		func(c context.Context) error { return db.Ping(c) },
		func(c context.Context) error {
			_, err := sqsClient.GetQueueAttributes(c, &sqs.GetQueueAttributesInput{
				QueueUrl:       &cfg.SQSQueueURL,
				AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
			})
			return err
		},
	))

	healthSrv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: httpapi.Logging(healthMux),
	}

	healthErrCh := make(chan error, 1)
	go func() {
		slog.Info("worker health listening", "port", cfg.Port)
		healthErrCh <- healthSrv.ListenAndServe()
	}()

	// Twilio + limiter/breaker + processor
	tw := &twilio.Client{
		AccountSID:          cfg.TwilioAccountSID,
		AuthToken:           cfg.TwilioAuthToken,
		HTTP:                &http.Client{Timeout: 8 * time.Second},
		MessagingServiceSID: cfg.TwilioMessagingServiceSID,
		FromNumber:          cfg.TwilioFromNumber,
	}
	limiter := rate.NewLimiter(rate.Limit(cfg.TwilioRPSPerPod), cfg.TwilioBurst)
	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "twilio",
		MaxRequests: 3,
		Timeout:     20 * time.Second,
		ReadyToTrip: func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= 10 },
	})
	templates := map[string]string{
		"txn_confirm_v1": "Hi {name}, your request is confirmed. Ref: {ref}. Thanks.",
	}
	processor := &workerproc.Processor{
		Store:     store,
		Sender:    tw,
		Templates: templates,
		Limiter:   limiter,
		Breaker:   cb,
	}

	// start polling
	pollErrCh := make(chan error, 1)
	go func() {
		slog.Info("worker starting poll", "queue_url", cfg.SQSQueueURL)
		pollErrCh <- consumer.PollConcurrent(ctx, cfg.WorkerConcurrency, func(ctx context.Context, job sqsqueue.SMSJob) (err error) {
			start := time.Now()
			slog.Info("worker job start", "message_id", job.MessageID)
			defer func() {
				if err != nil {
					slog.Info("worker job finish",
						"message_id", job.MessageID,
						"status", "error",
						"duration", time.Since(start),
						"err", err,
					)
				} else {
					slog.Info("worker job finish",
						"message_id", job.MessageID,
						"status", "ok",
						"duration", time.Since(start),
					)
				}
			}()
			err = processor.Process(ctx, job)
			return err
		})
	}()

	// shutdown wiring
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-pollErrCh:
		if err != nil && err != context.Canceled {
			slog.Error("worker poll failed", "err", err)
			os.Exit(1)
		}
	case err := <-healthErrCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("worker health server failed", "err", err)
			os.Exit(1)
		}
	case sig := <-sigCh:
		slog.Info("worker shutdown", "signal", sig.String())
	}

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = healthSrv.Shutdown(shutdownCtx)

	select {
	case <-pollErrCh:
	case <-time.After(10 * time.Second):
		slog.Info("worker shutdown timeout waiting for poll loop")
	}
}
