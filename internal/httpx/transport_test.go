package httpx

import (
	"net/http"
	"testing"
	"time"
)

// TestClient_PoolsPerHost guards the defect this package exists for. Go's
// default is two idle connections per host, which is the wrong number for any
// caller that talks to one host hundreds of times a second.
func TestClient_PoolsPerHost(t *testing.T) {
	c := Client(5*time.Second, 256)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.Transport)
	}
	if tr.MaxIdleConnsPerHost != 256 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 256", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConns (%d) below per-host (%d) silently caps the pool",
			tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	if c.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", c.Timeout)
	}
}

// TestClient_ZeroTimeoutIsHonoured matters for long polling: a blanket timeout
// at or below the poll duration kills every empty poll, which starved the SQS
// consumer once already.
func TestClient_ZeroTimeoutIsHonoured(t *testing.T) {
	if c := Client(0, 64); c.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 — callers that block need no blanket ceiling", c.Timeout)
	}
}

// TestClient_FloorsATinyPool keeps a careless caller from making things worse
// than the default.
func TestClient_FloorsATinyPool(t *testing.T) {
	tr := Client(time.Second, 0).Transport.(*http.Transport)
	if tr.MaxIdleConnsPerHost < 2 {
		t.Errorf("MaxIdleConnsPerHost = %d, want at least the Go default of 2", tr.MaxIdleConnsPerHost)
	}
}
