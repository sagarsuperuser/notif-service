package sqsqueue

import "testing"

func TestMessageGroupIDBucketed(t *testing.T) {
	tenant := "t1"
	to := "+19990000001"

	got1 := messageGroupIDBucketed(tenant, to, 2000)
	got2 := messageGroupIDBucketed(tenant, to, 2000)
	if got1 != got2 {
		t.Fatalf("expected stable group id, got %q vs %q", got1, got2)
	}
	if len(got1) == 0 {
		t.Fatalf("expected non-empty group id")
	}

	// buckets<=0 should use default.
	got3 := messageGroupIDBucketed(tenant, to, 0)
	if got3 == "" {
		t.Fatalf("expected non-empty group id for default buckets")
	}
}
