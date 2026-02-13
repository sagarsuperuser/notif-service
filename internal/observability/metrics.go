package observability

import "github.com/prometheus/client_golang/prometheus"

var (
	APIRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "notif_api_requests_total", Help: "API requests"},
		[]string{"endpoint", "status"},
	)
	WebhookRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "notif_webhook_requests_total", Help: "Webhook requests"},
		[]string{"endpoint", "status"},
	)
	Enqueues = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "notif_enqueue_total", Help: "SQS enqueue results"},
		[]string{"result"},
	)
	TwilioSend = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "twilio_send_total", Help: "Twilio send outcomes"},
		[]string{"result", "http_status"},
	)
	TwilioLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "twilio_send_latency_seconds",
			Help:    "Twilio send latency",
			Buckets: []float64{0.05, 0.1, 0.2, 0.5, 1, 2, 3, 5, 8, 13, 21},
		},
	)
	EndToEndLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "notif_end_to_end_latency_seconds",
			Help:    "API accepted to provider attempt result",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 900, 1200, 1800},
		},
	)
	WorkerProcessed = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "notif_worker_processed_total", Help: "Worker processed results"},
		[]string{"result"},
	)
	WorkerProcessingSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "notif_worker_processing_seconds",
			Help:    "Worker processing duration",
			Buckets: []float64{0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 180},
		},
	)
	WebhookEvents = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "twilio_webhook_events_total", Help: "Webhook events"},
		[]string{"status"},
	)
	WebhookMessageUpdateNotFound = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "notif_webhook_message_update_not_found_total",
			Help: "Terminal webhook events that could not be applied to messages (no matching provider_msg_id yet)",
		},
		[]string{"status"},
	)
)

func RegisterAPI(reg prometheus.Registerer) {
	reg.MustRegister(
		APIRequests,
		Enqueues,
	)
}

func RegisterWorker(reg prometheus.Registerer) {
	reg.MustRegister(
		TwilioSend,
		TwilioLatency,
		EndToEndLatency,
		WorkerProcessed,
		WorkerProcessingSeconds,
	)
}

func RegisterWebhook(reg prometheus.Registerer) {
	reg.MustRegister(
		WebhookRequests,
		WebhookEvents,
		WebhookMessageUpdateNotFound,
	)
}
