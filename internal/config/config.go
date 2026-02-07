package config

import "github.com/kelseyhightower/envconfig"

type APIConfig struct {
	DBDSN       string `envconfig:"DB_DSN" required:"true"`
	Port        string `envconfig:"PORT" default:"8080"`
	MetricsPort string `envconfig:"METRICS_PORT" default:"9090"`
	LogFormat   string `envconfig:"LOG_FORMAT" default:"json"`

	// multi-tenant rails
	MaxSMSPerDay int `envconfig:"MAX_SMS_PER_DAY" default:"2"`

	// AWS / SQS
	AWSRegion          string `envconfig:"AWS_REGION" required:"true"`
	SQSQueueURL        string `envconfig:"SQS_QUEUE_URL" required:"true"`
	LocalstackEndpoint string `envconfig:"LOCALSTACK_ENDPOINT"`
}

type WorkerConfig struct {
	DBDSN       string `envconfig:"DB_DSN" required:"true"`
	Port        string `envconfig:"PORT" default:"8080"`
	MetricsPort string `envconfig:"METRICS_PORT" default:"9090"`
	LogFormat   string `envconfig:"LOG_FORMAT" default:"json"`

	// AWS / SQS
	AWSRegion          string `envconfig:"AWS_REGION" required:"true"`
	SQSQueueURL        string `envconfig:"SQS_QUEUE_URL" required:"true"`
	LocalstackEndpoint string `envconfig:"LOCALSTACK_ENDPOINT"`
	SQSWaitTime        int32  `envconfig:"SQS_WAIT_TIME" default:"20"`
	SQSMaxMsgs         int32  `envconfig:"SQS_MAX_MSGS" default:"10"`
	SQSVizTimeout      int32  `envconfig:"SQS_VISIBILITY_TIMEOUT" default:"60"`

	WorkerConcurrency int `envconfig:"WORKER_CONCURRENCY" default:"20"`

	// Twilio
	TwilioAccountSID          string  `envconfig:"TWILIO_ACCOUNT_SID" required:"true"`
	TwilioAuthToken           string  `envconfig:"TWILIO_AUTH_TOKEN" required:"true"`
	TwilioMessagingServiceSID string  `envconfig:"TWILIO_MESSAGING_SERVICE_SID"`
	TwilioFromNumber          string  `envconfig:"TWILIO_FROM_NUMBER"`
	TwilioBaseURL             string  `envconfig:"TWILIO_BASE_URL" default:"https://api.twilio.com"`
	TwilioRPSPerPod           float64 `envconfig:"TWILIO_RPS_PER_POD" default:"5"`
	TwilioBurst               int     `envconfig:"TWILIO_BURST" default:"10"`

}

type WebhookConfig struct {
	DBDSN       string `envconfig:"DB_DSN" required:"true"`
	Port        string `envconfig:"PORT" default:"8080"`
	MetricsPort string `envconfig:"METRICS_PORT" default:"9090"`
	LogFormat   string `envconfig:"LOG_FORMAT" default:"json"`

	// Webhook signature verification
	TwilioAuthToken  string `envconfig:"TWILIO_AUTH_TOKEN" required:"true"`
	PublicWebhookURL string `envconfig:"PUBLIC_WEBHOOK_URL" required:"true"` // must match EXACT URL configured in Twilio
}

func LoadAPI() APIConfig {
	var cfg APIConfig
	if err := envconfig.Process("", &cfg); err != nil {
		panic(err)
	}
	return cfg
}

func LoadWorker() WorkerConfig {
	var cfg WorkerConfig
	if err := envconfig.Process("", &cfg); err != nil {
		panic(err)
	}
	return cfg
}

func LoadWebhook() WebhookConfig {
	var cfg WebhookConfig
	if err := envconfig.Process("", &cfg); err != nil {
		panic(err)
	}
	return cfg
}
