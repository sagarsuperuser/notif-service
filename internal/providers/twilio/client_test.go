package twilio

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestShouldRetry_ClassifiesRealProviderResponses drives the REAL client
// against a real server, because the defect this guards was invisible to any
// test that called ShouldRetry directly with hand-made arguments.
//
// SendSMS returns a non-nil error alongside every non-2xx response. An earlier
// version of ShouldRetry tested `err != nil` first and returned false, making
// the status branches unreachable: 429 and 503 were classified permanent, the
// worker marked the message terminally failed on its first attempt, and since a
// terminal row cannot be re-claimed the redelivered queue message was
// acknowledged and deleted. Sends were abandoned without reaching the DLQ.
//
// Calling ShouldRetry(nil, 429) would have passed the whole time. Only the
// round trip exposes it.
func TestShouldRetry_ClassifiesRealProviderResponses(t *testing.T) {
	cases := []struct {
		status    int
		wantRetry bool
		why       string
	}{
		{429, true, "throttled: documented as not processed and safe to retry"},
		{408, true, "request timeout"},
		{500, true, "provider fault"},
		{503, true, "provider unavailable"},
		{502, true, "bad gateway"},
		{400, false, "malformed request will not improve on retry"},
		{401, false, "bad credentials will not improve on retry"},
		{404, false, "wrong endpoint"},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"simulated","code":20429}`))
			}))
			defer srv.Close()

			c := &Client{AccountSID: "AC", AuthToken: "t", HTTP: srv.Client(),
				BaseURL: srv.URL, FromNumber: "+15550000000"}
			_, gotStatus, _, err := c.SendSMS(context.Background(), SendRequest{To: "+15551234567", Body: "x"})

			if gotStatus != tc.status {
				t.Fatalf("status = %d, want %d", gotStatus, tc.status)
			}
			if err == nil {
				t.Fatal("SendSMS returned nil error for a non-2xx; the classifier's inputs have changed")
			}
			if got := ShouldRetry(err, gotStatus); got != tc.wantRetry {
				t.Errorf("ShouldRetry(err, %d) = %v, want %v — %s", tc.status, got, tc.wantRetry, tc.why)
			}
		})
	}
}

// TestShouldRetry_TransportFailures covers the case where no response arrived
// at all, and every one of them must be transient.
//
// An earlier version of this test asserted the opposite for a refused
// connection — "it will not fix itself inside the retry budget". That reasoning
// was about the three in-process attempts, but the code path it justified marks
// the message permanently failed, which also cancels the SQS redelivery that
// exists for exactly this. A provider outage presents as connection-refused, so
// the belief this test used to pin would discard every message in flight during
// one. Caught by running an outage rather than by reading the code.
func TestShouldRetry_TransportFailures(t *testing.T) {
	t.Run("connection refused is transient", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close() // nothing is listening now

		c := &Client{AccountSID: "AC", AuthToken: "t", HTTP: &http.Client{Timeout: 2 * time.Second},
			BaseURL: url, FromNumber: "+15550000000"}
		_, status, _, err := c.SendSMS(context.Background(), SendRequest{To: "+15551234567", Body: "x"})

		if status != 0 {
			t.Fatalf("status = %d, want 0 — a transport failure has no status", status)
		}
		if err == nil {
			t.Fatal("expected a transport error")
		}
		if !ShouldRetry(err, status) {
			t.Error("a refused connection was classified permanent; that ends the message and cancels the SQS " +
				"redelivery, so a provider outage would discard everything in flight")
		}
	})

	t.Run("timeout is retryable", func(t *testing.T) {
		if ShouldRetry(context.DeadlineExceeded, 0) != true {
			t.Error("a deadline exceeded with no status must be retryable")
		}
		var ne net.Error = &net.DNSError{IsTimeout: true}
		if ShouldRetry(ne, 0) != true {
			t.Error("a net.Error reporting Timeout with no status must be retryable")
		}
	})

	t.Run("other transport failures are transient too", func(t *testing.T) {
		// None of these describe the message; they all describe the connection.
		for _, err := range []error{
			errors.New("dial tcp: lookup provider: no such host"),
			errors.New("read: connection reset by peer"),
			errors.New("http2: server sent GOAWAY"),
		} {
			if !ShouldRetry(err, 0) {
				t.Errorf("%v was classified permanent; nothing at the transport layer can prove a message "+
					"undeliverable", err)
			}
		}
	})

	t.Run("no error and no status is not retryable", func(t *testing.T) {
		if ShouldRetry(nil, 0) {
			t.Error("nothing to retry when there is neither an error nor a status")
		}
	})
}
