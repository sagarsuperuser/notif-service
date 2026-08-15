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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"notif/internal/awsutil"
	"notif/internal/config"
	"notif/internal/httpserver"
	"notif/internal/httpx"
	"notif/internal/logging"
	"notif/internal/observability"
	"notif/internal/providers/twilio"
	sqsqueue "notif/internal/queue/sqs"
	"notif/internal/store/pg"
	"notif/internal/util"
	workerproc "notif/internal/worker"

	"github.com/sony/gobreaker"
	"golang.org/x/time/rate"
)

// providerIdleConns sizes the provider transport pool. Zero means match worker
// concurrency, which is the correct default: every handler may hold a
// connection at once. A non-zero value is for experiments that need to vary
// this alone — notably reproducing Go's two-connection default, which forces a
// TLS handshake on most calls.
func providerIdleConns(cfg config.WorkerConfig) int {
	if cfg.ProviderMaxIdleConns > 0 {
		return cfg.ProviderMaxIdleConns
	}
	return cfg.WorkerConcurrency + 20
}

func main() {
	cfg := config.LoadWorker()
	logging.Init("worker", cfg.LogFormat)

	// Use a root ctx we can cancel
	ctx, cancel := context.WithCancel(context.Background())

	// Handlers run under this one so that stopping the receivers does not abort
	// a send that is already in flight. Cancelled only after the drain, below.
	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()

	db, err := pg.NewPool(ctx, cfg.DBDSN, pg.PoolOptions{
		MaxConns:          cfg.DBPoolMaxConns,
		MinConns:          cfg.DBPoolMinConns,
		MaxConnLifetime:   cfg.DBPoolMaxConnLifetime,
		MaxConnIdleTime:   cfg.DBPoolMaxConnIdleTime,
		HealthCheckPeriod: cfg.DBPoolHealthCheckPeriod,
		Tracer:            &observability.QueryTracer{Service: "worker"},
	})
	if err != nil {
		slog.Error("worker db connect failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	store := pg.New(db)

	sqsClient, err := awsutil.NewSQSClient(ctx, cfg.AWSRegion, cfg.LocalstackEndpoint)
	if err != nil {
		slog.Error("worker sqs client init failed", "err", err)
		os.Exit(1)
	}

	reg := prometheus.DefaultRegisterer
	observability.RegisterWorker(reg)
	observability.RegisterDB(reg)
	// Pool statistics are how pool exhaustion is told apart from a slow
	// database; without them the two look identical from the outside.
	go observability.SamplePool(ctx, "worker", db, 5*time.Second)

	deleteWait, err := time.ParseDuration(cfg.SQSDeleteBatchWait)
	if err != nil {
		slog.Error("invalid SQS_DELETE_BATCH_WAIT", "err", err, "value", cfg.SQSDeleteBatchWait)
		os.Exit(1)
	}
	consumer := &sqsqueue.Consumer{
		SQS:               sqsClient,
		QueueURL:          cfg.SQSQueueURL,
		WaitTimeSeconds:   cfg.SQSWaitTime,
		MaxMessages:       cfg.SQSMaxMsgs,
		VisibilityTimeout: cfg.SQSVizTimeout,
		Receivers:         cfg.SQSReceivers,
		DeleteBatchDelay:  deleteWait,
	}

	// health server (dependency checks)
	healthMux := httpserver.New().Mux
	healthMux.Use(httpserver.Logging)
	healthMux.HandleFunc("/healthz", httpserver.Readyz(2*time.Second,
		func(c context.Context) error { return db.Ping(c) },
		func(c context.Context) error {
			_, err := sqsClient.GetQueueAttributes(c, &sqs.GetQueueAttributesInput{
				QueueUrl:       &cfg.SQSQueueURL,
				AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
			})
			return err
		},
	)).Methods(http.MethodGet)

	healthSrv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: healthMux,
	}
	metricsSrv := &http.Server{
		Addr:    ":" + cfg.MetricsPort,
		Handler: promhttp.Handler(),
	}

	healthErrCh := make(chan error, 1)
	go func() {
		slog.Info("worker health listening", "port", cfg.Port)
		healthErrCh <- healthSrv.ListenAndServe()
	}()
	metricsErrCh := make(chan error, 1)
	go func() {
		slog.Info("worker metrics listening", "port", cfg.MetricsPort)
		metricsErrCh <- metricsSrv.ListenAndServe()
	}()

	// Twilio + limiter/breaker + processor
	sender := &twilio.Client{
		AccountSID: cfg.TwilioAccountSID,
		AuthToken:  cfg.TwilioAuthToken,
		// Sized to worker concurrency: every handler may hold a connection to
		// the provider at once, and the default pool of two would make the rest
		// pay a TCP and TLS handshake per send.
		HTTP:                httpx.Client(8*time.Second, providerIdleConns(cfg)),
		MessagingServiceSID: cfg.TwilioMessagingServiceSID,
		FromNumber:          cfg.TwilioFromNumber,
		BaseURL:             cfg.TwilioBaseURL,
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
		Store:           store,
		Sender:          sender,
		Templates:       templates,
		Limiter:         limiter,
		Breaker:         cb,
		ClaimStaleAfter: claimStaleAfter(cfg.SQSVizTimeout),
	}

	// start polling
	pollErrCh := make(chan error, 1)
	go func() {
		slog.Info("worker starting poll", "queue_url", cfg.SQSQueueURL)
		pollErrCh <- consumer.PollConcurrent(ctx, cfg.WorkerConcurrency, func(ctx context.Context, job sqsqueue.SMSJob) (err error) {
			start := util.NowUTC()
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
			// drainCtx, not ctx: cancelling ctx stops the RECEIVERS pulling new
			// work, and a message already being sent must be allowed to finish.
			// Handing the receive context to the handler meant SIGTERM aborted
			// every in-flight provider call, so the handler returned an error,
			// the message was never deleted, and SQS redelivered it. Five
			// deploys during one campaign was enough to push a message past
			// maxReceiveCount and into the dead-letter queue.
			err = processor.Process(drainCtx, job)
			return err
		})
	}()

	// shutdown wiring
	//
	// Two contexts, because "stop taking new work" and "abandon work in
	// progress" are different instructions and shutdown needs the first one.
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
	case err := <-metricsErrCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("worker metrics server failed", "err", err)
			os.Exit(1)
		}
	case sig := <-sigCh:
		slog.Info("worker shutdown", "signal", sig.String())
	}

	// Stop receiving. In-flight handlers keep drainCtx and run to completion;
	// the consumer then closes its job channel, waits for them, and flushes the
	// pending delete batch before returning.
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = healthSrv.Shutdown(shutdownCtx)
	_ = metricsSrv.Shutdown(shutdownCtx)

	// Long enough for a provider call (6s timeout) plus its retries and the
	// delete-batch linger. Must stay under terminationGracePeriodSeconds or the
	// kubelet sends SIGKILL mid-drain and the whole exercise is pointless.
	drainDeadline := 45 * time.Second
	select {
	case <-pollErrCh:
		slog.Info("worker drained in-flight messages before exit")
	case <-time.After(drainDeadline):
		slog.Warn("worker drain deadline hit; some in-flight messages will be redelivered",
			"deadline", drainDeadline)
		slog.Info("worker shutdown timeout waiting for poll loop")
	}
}

// claimStaleAfter is how long a message may sit in 'processing' before another
// worker may take it over, and it must be STRICTLY shorter than the SQS
// visibility timeout.
//
// It used to be exactly equal, which made recovery from a crashed worker a
// coin flip. A claim writes updated_at some interval d after the message was
// received, so the row is considered stale at receive+d+window while the
// redelivery arrives at receive+visibility. With window == visibility, the
// redelivery is early by exactly d and the row does not look stale yet — the
// worker then counts the delivery as "held by someone else", returns nil, and
// the consumer deletes the receipt. A message whose worker died is silently
// dropped, and because nothing errors it never reaches the DLQ either.
//
// d is the receive-to-claim latency, which grows precisely when the system is
// loaded, so the failure is likeliest under the conditions that cause worker
// restarts in the first place.
//
// Half the visibility timeout leaves d tens of seconds of headroom. Shortening
// the window cannot cause a duplicate send: SQS will not hand the same message
// to a second consumer before the visibility timeout regardless, so no other
// claimant exists during the interval this shortens.
func claimStaleAfter(visibilityTimeoutSeconds int32) time.Duration {
	v := time.Duration(visibilityTimeoutSeconds) * time.Second
	if v <= 0 {
		return 30 * time.Second
	}
	return v / 2
}
