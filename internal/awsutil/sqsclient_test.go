package awsutil

import (
	"net/http"
	"testing"
	"time"
)

// TestHTTPClientHasNoBlanketTimeout guards the failure this file already
// caused once.
//
// http.Client.Timeout bounds the entire request, including the time spent
// waiting for the response body. SQS long polling deliberately holds the
// connection open for up to WaitTimeSeconds — 20 by default — so any client
// timeout at or below that kills every empty poll before it can return.
//
// The failure is nastier than it sounds because it is asymmetric: SendMessage
// returns in milliseconds and keeps working, so the API looks healthy and only
// the consumer starves. It took a deployment against real SQS to see it; the
// integration suite runs with WaitTimeSeconds of 0 or 1 against a local
// endpoint and cannot reproduce it.
//
// A single number here cannot serve both a 10ms send and a 20s long poll, so
// the correct value is none — per-request contexts carry the deadlines.
func TestHTTPClientHasNoBlanketTimeout(t *testing.T) {
	c := newHTTPClient()

	if c.Timeout != 0 {
		t.Errorf("http client has a blanket Timeout of %v; long-polling ReceiveMessage holds the "+
			"connection for up to WaitTimeSeconds (20 by default), so any blanket timeout at or "+
			"below that starves the consumer while the producer keeps working", c.Timeout)
	}
}

// TestHTTPClientBoundsConnectionSetup checks the other half: dropping the
// blanket timeout must not leave connection establishment unbounded, or a
// blackholed endpoint hangs a caller forever.
func TestHTTPClientBoundsConnectionSetup(t *testing.T) {
	tr, ok := newHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", newHTTPClient().Transport)
	}

	if tr.TLSHandshakeTimeout <= 0 || tr.TLSHandshakeTimeout > 30*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v; connection setup must stay bounded", tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout = %v; a long poll returns no headers until a message "+
			"arrives, so bounding it reintroduces the same starvation", tr.ResponseHeaderTimeout)
	}
}

// TestHTTPClientPoolsConnections pins the reason this transport is custom at
// all. Go's default keeps two idle connections per host, so every SQS call
// beyond the second pays a TCP and TLS handshake and then discards the
// connection.
func TestHTTPClientPoolsConnections(t *testing.T) {
	tr := newHTTPClient().Transport.(*http.Transport)

	if tr.MaxIdleConnsPerHost < 100 {
		t.Errorf("MaxIdleConnsPerHost = %d; the whole point of this transport is to keep "+
			"connections warm for hundreds of concurrent calls to one host", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConns (%d) below MaxIdleConnsPerHost (%d) silently caps the per-host pool",
			tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
}
