// Package httpx supplies an HTTP client sized for talking to one host a lot.
package httpx

import (
	"net"
	"net/http"
	"time"
)

// Client returns a client whose connection pool matches the concurrency it will
// actually see.
//
// Go's default transport keeps TWO idle connections per host. Every call beyond
// that opens a fresh TCP connection, completes a TLS handshake before sending a
// byte, and then discards the connection because the idle pool is full. At a few
// hundred calls a second to a single host that dominates latency and churns
// through ephemeral ports, and the failed connects surface as transport errors
// rather than HTTP responses — so they read as the far end misbehaving.
//
// This defect appeared independently in three places here: the SQS client, the
// worker's provider client, and the simulator's webhook client. Two carried
// enough traffic to matter. Hence one builder rather than a fourth copy.
//
// timeout is the whole-request ceiling. Pass 0 for calls that legitimately
// block — SQS long polling, for one — where a blanket timeout kills the very
// request it is meant to bound.
func Client(timeout time.Duration, maxIdlePerHost int) *http.Client {
	if maxIdlePerHost < 2 {
		maxIdlePerHost = 2
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          maxIdlePerHost * 2,
			MaxIdleConnsPerHost:   maxIdlePerHost,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}
