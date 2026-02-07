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
		prometheus.HistogramOpts{Name: "twilio_send_latency_seconds", Help: "Twilio send latency"},
	)
	EndToEndLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{Name: "notif_end_to_end_latency_seconds", Help: "API accepted to provider attempt result"},
	)
	WorkerProcessed = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "notif_worker_processed_total", Help: "Worker processed results"},
		[]string{"result"},
	)
	WorkerProcessingSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{Name: "notif_worker_processing_seconds", Help: "Worker processing duration"},
	)
	WebhookEvents = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "twilio_webhook_events_total", Help: "Webhook events"},
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
	)
}
