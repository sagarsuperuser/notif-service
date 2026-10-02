package worker

import (
	"errors"
	"testing"
)

// TestIsPermanentRejection pins what the circuit breaker counts as healthy. A
// permanent rejection is the provider answering correctly about one bad
// message; counted as a failure, a run of invalid numbers would open the
// breaker and push every healthy message onto the redelivery path.
func TestIsPermanentRejection(t *testing.T) {
	boom := errors.New("twilio send failed")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"400 bad request", twilioCallError{err: boom, httpStatus: 400}, true},
		{"422 unprocessable", twilioCallError{err: boom, httpStatus: 422}, true},
		{"429 throttled is provider health", twilioCallError{err: boom, httpStatus: 429}, false},
		{"503 is provider health", twilioCallError{err: boom, httpStatus: 503}, false},
		{"401 is configuration, not the message", twilioCallError{err: boom, httpStatus: 401}, false},
		{"transport failure", twilioCallError{err: boom, httpStatus: 0}, false},
		{"unrelated error", boom, false},
	}
	for _, tc := range cases {
		if got := IsPermanentRejection(tc.err); got != tc.want {
			t.Errorf("%s: IsPermanentRejection = %v, want %v", tc.name, got, tc.want)
		}
	}
}
