package main

import (
	"testing"
	"time"
)

// TestClaimStaleAfter_LeavesRoomBeforeRedelivery pins the relationship between
// the two timeouts, which is the whole reason the helper exists.
//
// The stale window must expire BEFORE SQS redelivers, or a crashed worker's
// message cannot be re-claimed and is acknowledged instead of retried. The two
// used to be equal, which left no room at all: a claim is written some interval
// after the message is received, so the row went stale that same interval after
// the redelivery was already due.
func TestClaimStaleAfter_LeavesRoomBeforeRedelivery(t *testing.T) {
	for _, visibility := range []int32{30, 60, 120, 300} {
		v := time.Duration(visibility) * time.Second
		got := claimStaleAfter(visibility)

		if got >= v {
			t.Errorf("visibility %v: stale window %v is not shorter than the visibility timeout — a redelivery "+
				"would arrive before the row looks stale, and the message would be dropped", v, got)
		}
		// The margin has to absorb the receive-to-claim latency, which grows
		// under load. A window a hair under the timeout would technically pass
		// the check above while still losing the race in practice.
		if margin := v - got; margin < v/4 {
			t.Errorf("visibility %v: only %v of margin; too little to absorb claim latency under load", v, margin)
		}
	}

	// A misconfigured zero must not produce a zero window, which would let any
	// worker steal any in-flight claim immediately.
	if got := claimStaleAfter(0); got <= 0 {
		t.Errorf("claimStaleAfter(0) = %v; a non-positive window makes every claim instantly stealable", got)
	}
}
