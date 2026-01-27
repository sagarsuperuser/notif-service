package observability

import "github.com/prometheus/client_golang/prometheus"

var (
	APIRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "notif_api_requests_total", Help: "API requests"},
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
	WebhookEvents = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "twilio_webhook_events_total", Help: "Webhook events"},
		[]string{"status"},
	)
	Suppressed = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "notif_suppressed_total", Help: "Suppressed messages"},
		[]string{"reason"},
	)
)

func Register(reg prometheus.Registerer) {
	reg.MustRegister(APIRequests, Enqueues, TwilioSend, TwilioLatency, WebhookEvents, Suppressed)
}
