package awsutil

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	configv2 "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// newHTTPClient returns a transport sized for a service that keeps hundreds of
// SQS calls in flight against a single host.
//
// Go's default transport allows two idle connections per host. Every call
// beyond that opens a fresh connection and pays a TCP handshake plus a TLS
// handshake — one to two round-trips to the SQS endpoint — before it sends a
// byte, and then throws the connection away. Under load that turns a queue
// call from ~10ms into ~40ms and burns local ports. Raising the idle pool to
// match the concurrency lets connections be reused, which is the difference
// between the queue being a rounding error in the latency budget and being the
// budget.
func newHTTPClient() *http.Client {
	return &http.Client{
		// Deliberately no client-level Timeout.
		//
		// http.Client.Timeout covers the whole request including the time spent
		// reading the response, so it is a ceiling on how long ANY call may take.
		// ReceiveMessage with long polling deliberately holds the connection open
		// for up to WaitTimeSeconds — 20 by default — waiting for a message to
		// arrive. A 15-second client timeout therefore killed every empty poll
		// before it could return, and the worker could not consume at all while
		// the API, whose SendMessage returns immediately, looked perfectly
		// healthy.
		//
		// Per-request deadlines are the right place for this: the SDK sets its
		// own, callers pass contexts, and the dial and TLS timeouts below still
		// bound connection setup. A single number here cannot be correct for both
		// a 10ms send and a 20s long poll.
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   512,
			MaxConnsPerHost:       0,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func NewSQSClient(ctx context.Context, region, endpoint string) (*sqs.Client, error) {
	opts := []func(*configv2.LoadOptions) error{
		configv2.WithRegion(region),
		configv2.WithHTTPClient(newHTTPClient()),
	}

	// LocalStack accepts these dummy credentials.
	if endpoint != "" {
		opts = append(opts, configv2.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", ""),
		))
	}

	cfg, err := configv2.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}

	if endpoint != "" {
		return sqs.NewFromConfig(cfg, func(o *sqs.Options) {
			o.BaseEndpoint = aws.String(endpoint)
		}), nil
	}
	return sqs.NewFromConfig(cfg), nil
}
