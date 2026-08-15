package observability

import "github.com/prometheus/client_golang/prometheus"

// Worker metrics, rewritten after an audit found the originals measured
// something other than their names.
//
// Two rules produced these, both learned the hard way:
//
//  1. A timer measures exactly the thing it is named after. The metric this
//     replaces, twilio_send_latency_seconds, started its clock before a
//     token-bucket rate limiter and a three-attempt retry loop, so a sample was
//     local queueing plus retries plus the call. It was quoted as provider
//     latency and it never was that.
//
//  2. An outcome label is set on every exit path, never defaulted. The metric
//     this replaces initialised its label to "success" and had five error
//     returns that left it there, so failures were counted as successes.
var (
	// ProviderCallSeconds wraps ONLY the HTTP call to the provider — no limiter
	// wait, no backoff, no retry. One observation per attempt, so a message that
	// retries contributes several, which is what makes the histogram a statement
	// about the provider rather than about our own scheduling.
	ProviderCallSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "notif_provider_call_seconds",
			Help:    "Duration of a single HTTP call to the provider, excluding rate-limiter wait and retries",
			Buckets: []float64{.005, .01, .025, .05, .1, .2, .4, .8, 1.6, 3.2, 6.4},
		},
		[]string{"outcome"},
	)

	// MessageOutcome is one increment per message the worker CLAIMED and
	// finished, with the outcome named explicitly at each exit. A message the
	// worker skipped because another worker held it is not an outcome and is
	// counted by ClaimResult instead — conflating the two is what made the old
	// counter's denominator wrong.
	MessageOutcome = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "notif_message_outcome_total",
			Help: "Messages the worker claimed and finished, by explicit outcome",
		},
		[]string{"outcome"},
	)

	// ClaimResult separates "did we get this message" from "what happened to
	// it". Duplicate SQS deliveries land here as skipped, so they stop
	// inflating per-message ratios computed against the outcome counter.
	ClaimResult = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "notif_worker_claim_total",
			Help: "Outcome of attempting to claim a delivered message: claimed, skipped, or missing",
		},
		[]string{"result"},
	)

	// ProviderAttempts counts calls, so retries are visible rather than folded
	// into the message outcome.
	ProviderAttempts = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "notif_provider_attempts_total",
			Help: "HTTP calls made to the provider, by outcome and status",
		},
		[]string{"outcome", "http_status"},
	)
)

func RegisterWorkerMetrics(reg prometheus.Registerer) {
	reg.MustRegister(
		ProviderCallSeconds,
		MessageOutcome, ClaimResult, ProviderAttempts,
	)
}
