package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Webhook delivery counters.
//
// The simulator retries a callback that fails, which is what a real provider
// does — but until now it counted nothing, so a run that produced 300,578
// callbacks for 100,000 messages offered no way to tell whether the extra 578
// were retries, duplicates, or a counting error. Attempts and retries are
// separated from outcomes, and a transport failure is separated from an HTTP
// rejection, because they have different causes and different fixes.
var (
	mockWebhookAttempts = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mock_webhook_attempts_total",
		Help: "Callback POST attempts, including retries",
	})
	mockWebhookRetries = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mock_webhook_retries_total",
		Help: "Callback POST attempts that were retries of an earlier attempt",
	})
	mockWebhookTransportErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mock_webhook_transport_errors_total",
		Help: "Callback POSTs that never received a response (connect, reset, timeout)",
	})
	mockWebhookHTTPErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mock_webhook_http_errors_total",
		Help: "Callback POSTs answered with a non-2xx status",
	}, []string{"status"})
)
