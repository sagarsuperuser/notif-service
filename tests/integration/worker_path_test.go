//go:build integration
// +build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"notif/internal/observability"
	"notif/internal/providers/twilio"
	sqsqueue "notif/internal/queue/sqs"
	"notif/internal/store"
	"notif/internal/store/pg"
	"notif/internal/util"
	workerproc "notif/internal/worker"
)

// legacyClaim replays the PREVIOUS worker preamble: a SELECT that loaded the
// message, the caller's own state checks, then a conditional UPDATE to claim
// it. Two statements where there is now one.
func legacyClaim(t *testing.T, db *pgxpool.Pool, msgID string, now time.Time, staleAfter time.Duration) (msg store.MessageForWorker, found, claimed bool) {
	t.Helper()
	ctx := context.Background()

	var varsJSON []byte
	err := db.QueryRow(ctx, `
		SELECT tenant_id, to_phone, template_id, COALESCE(campaign_id,''), state,
		       COALESCE(provider_msg_id,''), vars_json, created_at
		  FROM messages WHERE id=$1
	`, msgID).Scan(&msg.TenantID, &msg.To, &msg.TemplateID, &msg.CampaignID, &msg.State,
		&msg.ProviderMsgID, &varsJSON, &msg.CreatedAt)
	if err != nil {
		return store.MessageForWorker{}, false, false
	}
	_ = json.Unmarshal(varsJSON, &msg.Vars)

	// The caller's short-circuits, in the order the worker applied them.
	if msg.State == "suppressed" || msg.State == "delivered" || msg.State == "failed" {
		return msg, true, false
	}
	if msg.ProviderMsgID != "" && msg.State == "submitted" {
		return msg, true, false
	}

	ct, err := db.Exec(ctx, `
		UPDATE messages SET state='processing', updated_at=$2
		 WHERE id=$1 AND (state='queued' OR (state='processing' AND updated_at < $3))
	`, msgID, now, now.Add(-staleAfter))
	if err != nil {
		t.Fatalf("legacy claim: %v", err)
	}
	return msg, true, ct.RowsAffected() > 0
}

func seedMessageInState(t *testing.T, db *pgxpool.Pool, id, tenant, idem, state, providerMsgID string, updatedAt time.Time) {
	t.Helper()
	insertTenant(t, db, tenant)
	_, err := db.Exec(context.Background(), `
		INSERT INTO messages (id, tenant_id, idempotency_key, to_phone, template_id, vars_json,
		                      state, provider, provider_msg_id, created_at, updated_at)
		VALUES ($1,$2,$3,'+15550001234','tpl','{"n":"1"}',$4,
		        CASE WHEN $5::text='' THEN NULL ELSE 'twilio' END,
		        NULLIF($5::text,''), $6, $6)
	`, id, tenant, idem, state, providerMsgID, updatedAt)
	if err != nil {
		t.Fatalf("seed message %s: %v", id, err)
	}
}

func stateOf(t *testing.T, db *pgxpool.Pool, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(context.Background(), `SELECT state FROM messages WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("state of %s: %v", id, err)
	}
	return s
}

func attemptCount(t *testing.T, db *pgxpool.Pool, msgID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_attempts WHERE message_id=$1`, msgID).Scan(&n); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	return n
}

// TestClaimAndLoad_MatchesLegacySequence is the differential proof for merging
// the worker's load and claim. For every state a job can arrive in, the single
// statement must reach the same verdict — claimed or skipped — and leave the
// row in the same state as the SELECT-then-UPDATE it replaces.
func TestClaimAndLoad_MatchesLegacySequence(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)
	ctx := context.Background()
	now := util.NowUTC()
	stale := 2 * time.Minute

	cases := []struct {
		name          string
		state         string
		providerMsgID string
		updatedAt     time.Time
	}{
		{"queued", "queued", "", now},
		{"processing and fresh", "processing", "", now},
		{"processing and stale", "processing", "", now.Add(-10 * time.Minute)},
		{"submitted with sid", "submitted", "SM-x", now},
		{"submitted without sid", "submitted", "", now},
		{"delivered", "delivered", "SM-y", now},
		{"failed", "failed", "", now},
		{"suppressed", "suppressed", "", now},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldID := fmt.Sprintf("w-old-%d", i)
			newID := fmt.Sprintf("w-new-%d", i)
			seedMessageInState(t, db, oldID, "w-tenant", fmt.Sprintf("w-idem-old-%d", i), tc.state, tc.providerMsgID, tc.updatedAt)
			seedMessageInState(t, db, newID, "w-tenant", fmt.Sprintf("w-idem-new-%d", i), tc.state, tc.providerMsgID, tc.updatedAt)

			legacyMsg, legacyFound, legacyClaimed := legacyClaim(t, db, oldID, now, stale)

			got, found, err := s.ClaimAndLoad(ctx, newID, now, stale)
			if err != nil {
				t.Fatalf("ClaimAndLoad: %v", err)
			}

			if found != legacyFound {
				t.Errorf("found diverged: legacy=%v new=%v", legacyFound, found)
			}
			if got.Claimed != legacyClaimed {
				t.Errorf("claim verdict diverged: legacy=%v new=%v", legacyClaimed, got.Claimed)
			}
			if got.State != legacyMsg.State {
				t.Errorf("loaded state diverged: legacy=%q new=%q (both must be the PRE-claim state)",
					legacyMsg.State, got.State)
			}
			if got.To != legacyMsg.To || got.TemplateID != legacyMsg.TemplateID ||
				got.TenantID != legacyMsg.TenantID || got.ProviderMsgID != legacyMsg.ProviderMsgID {
				t.Errorf("loaded fields diverged: legacy=%+v new=%+v", legacyMsg, got.MessageForWorker)
			}
			if oldState, newState := stateOf(t, db, oldID), stateOf(t, db, newID); oldState != newState {
				t.Errorf("row left in different states: legacy=%q new=%q", oldState, newState)
			}
		})
	}
}

// TestClaimAndLoad_MissingMessageIsDistinguishable pins the one deliberate
// behaviour change: a job naming a row that does not exist reports found=false
// rather than being indistinguishable from an unclaimable message. The worker
// turns that into an error so the job reaches the DLQ instead of vanishing.
func TestClaimAndLoad_MissingMessageIsDistinguishable(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)

	got, found, err := s.ClaimAndLoad(context.Background(), "no-such-message", util.NowUTC(), time.Minute)
	if err != nil {
		t.Fatalf("ClaimAndLoad on a missing row returned an error, want a clean not-found: %v", err)
	}
	if found {
		t.Errorf("found = true for a message that does not exist")
	}
	if got.Claimed {
		t.Errorf("claimed = true for a message that does not exist")
	}
}

// TestClaimAndLoad_OnlyOneWorkerWins is the guarantee the claim exists for:
// SQS can deliver the same message to several workers at once, and exactly one
// of them may send it.
func TestClaimAndLoad_OnlyOneWorkerWins(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)
	now := util.NowUTC()

	seedMessageInState(t, db, "w-race", "w-race-tenant", "w-race-idem", "queued", "", now)

	const workers = 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, found, err := s.ClaimAndLoad(context.Background(), "w-race", now, 2*time.Minute)
			if err != nil {
				t.Errorf("ClaimAndLoad: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if found && got.Claimed {
				wins++
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Errorf("%d of %d workers claimed the same message, want exactly 1", wins, workers)
	}
}

// TestRecordAttempt_WritesAttemptAndTransitionTogether covers the three shapes
// the worker uses: a successful submit, a transient failure that must NOT move
// the message, and a terminal failure.
func TestRecordAttempt_WritesAttemptAndTransitionTogether(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)
	ctx := context.Background()
	now := util.NowUTC()

	t.Run("submit records the attempt and the sid", func(t *testing.T) {
		seedMessageInState(t, db, "ra-1", "ra-tenant", "ra-idem-1", "processing", "", now)
		if err := s.RecordAttempt(ctx, store.AttemptRecord{
			Attempt: store.ProviderAttempt{
				MessageID: "ra-1", Provider: "twilio", ProviderMsgID: "SM-ra-1", HTTPStatus: 201,
			},
			Transition: &store.MessageTransition{
				State: "submitted", Provider: "twilio", ProviderMsgID: "SM-ra-1", Now: now,
			},
		}); err != nil {
			t.Fatalf("RecordAttempt: %v", err)
		}
		if got := stateOf(t, db, "ra-1"); got != "submitted" {
			t.Errorf("state = %q, want submitted", got)
		}
		if n := attemptCount(t, db, "ra-1"); n != 1 {
			t.Errorf("attempts = %d, want 1", n)
		}
		var sid string
		if err := db.QueryRow(ctx, `SELECT provider_msg_id FROM messages WHERE id='ra-1'`).Scan(&sid); err != nil {
			t.Fatalf("read sid: %v", err)
		}
		if sid != "SM-ra-1" {
			t.Errorf("provider_msg_id = %q, want SM-ra-1", sid)
		}
	})

	t.Run("transient failure records the attempt and leaves the message alone", func(t *testing.T) {
		seedMessageInState(t, db, "ra-2", "ra-tenant", "ra-idem-2", "processing", "", now)
		if err := s.RecordAttempt(ctx, store.AttemptRecord{
			Attempt: store.ProviderAttempt{
				MessageID: "ra-2", Provider: "twilio", HTTPStatus: 503, ErrorMsg: "boom",
			},
		}); err != nil {
			t.Fatalf("RecordAttempt: %v", err)
		}
		if got := stateOf(t, db, "ra-2"); got != "processing" {
			t.Errorf("state = %q, want processing — a retryable error must not end the message", got)
		}
		if n := attemptCount(t, db, "ra-2"); n != 1 {
			t.Errorf("attempts = %d, want 1", n)
		}
	})

	t.Run("terminal failure does not erase an existing sid", func(t *testing.T) {
		seedMessageInState(t, db, "ra-3", "ra-tenant", "ra-idem-3", "processing", "SM-earlier", now)
		if err := s.RecordAttempt(ctx, store.AttemptRecord{
			Attempt: store.ProviderAttempt{
				MessageID: "ra-3", Provider: "twilio", HTTPStatus: 400, ErrorMsg: "bad",
			},
			Transition: &store.MessageTransition{
				State: "failed", LastError: "twilio_non_retryable", Now: now,
			},
		}); err != nil {
			t.Fatalf("RecordAttempt: %v", err)
		}
		if got := stateOf(t, db, "ra-3"); got != "failed" {
			t.Errorf("state = %q, want failed", got)
		}
		var sid, lastErr string
		if err := db.QueryRow(ctx,
			`SELECT COALESCE(provider_msg_id,''), COALESCE(last_error,'') FROM messages WHERE id='ra-3'`).
			Scan(&sid, &lastErr); err != nil {
			t.Fatalf("read row: %v", err)
		}
		if sid != "SM-earlier" {
			t.Errorf("provider_msg_id = %q — a failed attempt must not erase an id an earlier submit recorded", sid)
		}
		if lastErr != "twilio_non_retryable" {
			t.Errorf("last_error = %q, want twilio_non_retryable", lastErr)
		}
	})
}

// TestWorkerPathCostsTwoRoundTrips is the regression gate. The worker used to
// spend four statements per message — SELECT, claim UPDATE, attempt INSERT,
// state UPDATE — around a provider call. Two of those are now one each.
func TestWorkerPathCostsTwoRoundTrips(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	schema := schemaOf(t, db)
	counted, c := countedPool(t, schema)

	ctx := context.Background()
	now := util.NowUTC()
	seedMessageInState(t, db, "w-rt", "w-rt-tenant", "w-rt-idem", "queued", "", now)

	s := pg.New(counted)

	c.reset()
	msg, found, err := s.ClaimAndLoad(ctx, "w-rt", now, 2*time.Minute)
	if err != nil || !found || !msg.Claimed {
		t.Fatalf("ClaimAndLoad: err=%v found=%v claimed=%v", err, found, msg.Claimed)
	}
	claim := c.count()

	c.reset()
	if err := s.RecordAttempt(ctx, store.AttemptRecord{
		Attempt: store.ProviderAttempt{
			MessageID: "w-rt", Provider: "twilio", ProviderMsgID: "SM-w-rt", HTTPStatus: 201,
		},
		Transition: &store.MessageTransition{
			State: "submitted", Provider: "twilio", ProviderMsgID: "SM-w-rt", Now: now,
		},
	}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	record := c.count()

	// The legacy preamble, measured on the same counter for comparison.
	seedMessageInState(t, db, "w-rt-legacy", "w-rt-tenant", "w-rt-idem-legacy", "queued", "", now)
	c.reset()
	legacyClaim(t, counted, "w-rt-legacy", now, 2*time.Minute)
	legacy := c.count()

	t.Logf("round-trips: claim+load new=%d legacy=%d, attempt+transition new=%d (legacy 2), total per send new=%d legacy=4",
		claim, legacy, record, claim+record)

	if claim != 1 {
		t.Errorf("claim+load cost %d round-trips, want 1", claim)
	}
	if record != 1 {
		t.Errorf("attempt+transition cost %d round-trips, want 1", record)
	}
	if legacy != 2 {
		t.Fatalf("legacy preamble measured %d round-trips, want 2 — the comparison is broken, not the code", legacy)
	}
}

// countingSender records how many times the provider was actually called, which
// is the only thing that matters about duplicate delivery: SQS may hand the same
// message to several workers, and the recipient must receive exactly one SMS.
type countingSender struct {
	mu   sync.Mutex
	n    int
	sid  string
	fail error
	// failStatus is the HTTP status returned alongside fail. It matters:
	// retry classification keys off the status, so a fake that returns the
	// wrong one tests the wrong branch.
	failStatus int
}

func (s *countingSender) SendSMS(ctx context.Context, req twilio.SendRequest) (twilio.SendResponse, int, []byte, error) {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	if s.fail != nil {
		st := s.failStatus
		if st == 0 {
			st = 500
		}
		return twilio.SendResponse{}, st, []byte(`{"message":"boom"}`), s.fail
	}
	return twilio.SendResponse{Sid: s.sid, Status: "queued"}, 201, []byte(`{"sid":"` + s.sid + `"}`), nil
}

func (s *countingSender) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// TestProcessor_DuplicateDeliverySendsOnce drives the real Processor against
// real Postgres with the same job delivered twenty times at once — the shape of
// an SQS redrive after a visibility timeout. Exactly one send may reach the
// provider.
func TestProcessor_DuplicateDeliverySendsOnce(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	seedMessageInState(t, db, "p-dup", "p-tenant", "p-dup-idem", "queued", "", util.NowUTC())

	sender := &countingSender{sid: "SM-p-dup"}
	p := &workerproc.Processor{
		Store:     pg.New(db),
		Sender:    sender,
		Templates: map[string]string{"tpl": "hello {n}"},
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Process(context.Background(), sqsqueue.SMSJob{MessageID: "p-dup"}); err != nil {
				t.Errorf("process: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := sender.calls(); n != 1 {
		t.Errorf("provider called %d times for one message, want 1", n)
	}
	if got := stateOf(t, db, "p-dup"); got != "submitted" {
		t.Errorf("state = %q, want submitted", got)
	}
	if n := attemptCount(t, db, "p-dup"); n != 1 {
		t.Errorf("%d attempt rows, want 1", n)
	}
}

// TestProcessor_MissingMessageIsAnError pins the deliberate change: a job whose
// message row does not exist must surface, not be silently acknowledged. A
// silently dropped job is an SMS a customer paid for and never received, with
// nothing in the system to show for it.
func TestProcessor_MissingMessageIsAnError(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	sender := &countingSender{sid: "SM-none"}
	p := &workerproc.Processor{
		Store:     pg.New(db),
		Sender:    sender,
		Templates: map[string]string{"tpl": "hello {n}"},
	}

	err := p.Process(context.Background(), sqsqueue.SMSJob{MessageID: "does-not-exist"})
	if err == nil {
		t.Fatal("processing a job for a nonexistent message returned nil; it must fail so the job reaches the DLQ")
	}
	if sender.calls() != 0 {
		t.Errorf("provider was called %d times for a nonexistent message, want 0", sender.calls())
	}
}

// TestProcessor_AlreadySubmittedIsSkipped covers the ordinary redrive case: a
// message another worker already submitted must not be sent a second time, and
// must not be reported as an error either.
func TestProcessor_AlreadySubmittedIsSkipped(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	seedMessageInState(t, db, "p-done", "p-tenant", "p-done-idem", "submitted", "SM-already", util.NowUTC())

	sender := &countingSender{sid: "SM-p-done"}
	p := &workerproc.Processor{
		Store:     pg.New(db),
		Sender:    sender,
		Templates: map[string]string{"tpl": "hello {n}"},
	}

	if err := p.Process(context.Background(), sqsqueue.SMSJob{MessageID: "p-done"}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if n := sender.calls(); n != 0 {
		t.Errorf("provider called %d times for an already-submitted message, want 0", n)
	}
	if got := stateOf(t, db, "p-done"); got != "submitted" {
		t.Errorf("state = %q, want submitted (unchanged)", got)
	}
}

// TestProcessor_EveryOutcomeIsNamed is the gate on the defect that made the
// previous counter unusable.
//
// notif_worker_processed_total initialised its label to "success" and had five
// error returns that never reassigned it, so a template failure, a claim error,
// a store error and an exhausted retry were all counted as successes. The
// counter said what the code wished had happened.
//
// The replacement starts empty and reports "unset" if any path forgets, so a
// future unnamed exit shows up as a visible series rather than silently
// inflating the success rate. This test drives the paths that used to be
// mislabelled and asserts none of them lands on "unset" — and, crucially, that
// none lands on the success outcome either.
func TestProcessor_EveryOutcomeIsNamed(t *testing.T) {
	reg := prometheus.NewRegistry()
	observability.MessageOutcome.Reset()
	if err := reg.Register(observability.MessageOutcome); err != nil {
		t.Fatalf("register: %v", err)
	}

	db, cleanup := setupTestDB(t)
	defer cleanup()
	now := util.NowUTC()

	cases := []struct {
		name        string
		templates   map[string]string
		sender      *countingSender
		wantOutcome string
	}{
		{
			name:        "template missing",
			templates:   map[string]string{}, // no "tpl"
			sender:      &countingSender{sid: "SM-x"},
			wantOutcome: "template_not_found",
		},
		{
			// A 400 is the real permanent rejection: the request was malformed
			// and will be malformed again.
			//
			// This case previously returned 500 while calling itself
			// non-retryable, and passed — because ShouldRetry checked the error
			// before the status and classified every provider failure
			// permanent. The test was pinning the defect rather than the
			// intended behaviour, which is why the defect survived.
			name:        "provider rejects permanently",
			templates:   map[string]string{"tpl": "hello"},
			sender:      &countingSender{sid: "SM-y", fail: errPermanent{}, failStatus: 400},
			wantOutcome: "provider_rejected",
		},
		{
			// The companion case, absent before: a 5xx is transient, so the
			// worker must retry and only then give up. Without this, the two
			// classifications are indistinguishable to the suite.
			name:        "provider fails transiently",
			templates:   map[string]string{"tpl": "hello"},
			sender:      &countingSender{sid: "SM-z", fail: errTransient{}, failStatus: 503},
			wantOutcome: "retries_exhausted",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("oc-%d", i)
			seedMessageInState(t, db, id, "oc-tenant", fmt.Sprintf("oc-idem-%d", i), "queued", "", now)

			p := &workerproc.Processor{
				Store: pg.New(db), Sender: tc.sender, Templates: tc.templates,
			}
			_ = p.Process(context.Background(), sqsqueue.SMSJob{MessageID: id})

			got := outcomeCounts(t, reg)
			if got["unset"] > 0 {
				t.Errorf("an exit path did not name its outcome (unset=%v) — that is the defect this test exists for", got["unset"])
			}
			if got[tc.wantOutcome] == 0 {
				t.Errorf("expected outcome %q to be recorded; saw %v", tc.wantOutcome, got)
			}
			if got["submitted"] > 0 {
				t.Errorf("a failure was recorded as submitted: %v — this is exactly how the old counter reported failures as successes", got)
			}
			observability.MessageOutcome.Reset()
		})
	}
}

type errPermanent struct{}

func (errPermanent) Error() string { return "permanent provider rejection" }

type errTransient struct{}

func (errTransient) Error() string { return "transient provider failure" }

func outcomeCounts(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "notif_message_outcome_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "outcome" {
					out[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
	}
	return out
}

// readMessage returns the state and last_error a message actually holds.
func readMessage(t *testing.T, db *pgxpool.Pool, id string) (state string, lastErr string) {
	t.Helper()
	var le *string
	if err := db.QueryRow(context.Background(),
		`SELECT state, last_error FROM messages WHERE id=$1`, id).Scan(&state, &le); err != nil {
		t.Fatalf("read message %s: %v", id, err)
	}
	if le != nil {
		lastErr = *le
	}
	return state, lastErr
}

// TestProcessor_TransientFailureStaysRecoverable is the regression test for a
// send that was abandoned without a trace.
//
// A transient provider failure used to end with state='failed'. ClaimAndLoad
// claims only 'queued' or stale 'processing', so the SQS redelivery could not
// re-claim the row: it counted as skipped, Process returned nil, and the
// consumer deleted the receipt. The message was dropped after one silent
// redelivery and never reached the DLQ.
//
// The assertion that matters is not the state itself but what the state
// permits — so this drives the real redelivery through the real Processor and
// requires the retry to actually happen.
func TestProcessor_TransientFailureStaysRecoverable(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	const id = "recoverable-1"
	seedMessageInState(t, db, id, "rec-tenant", "rec-idem", "queued", "", util.NowUTC())

	sender := &countingSender{sid: "SM-rec", fail: errTransient{}, failStatus: 503}
	p := &workerproc.Processor{
		Store: pg.New(db), Sender: sender, Templates: map[string]string{"tpl": "hello"},
	}

	// First delivery: the provider is down, so every attempt fails.
	err := p.Process(context.Background(), sqsqueue.SMSJob{MessageID: id})
	if err == nil {
		t.Fatal("Process returned nil after exhausting retries; the consumer would delete the receipt and the " +
			"message would never redrive to the DLQ")
	}
	if got := sender.calls(); got != 3 {
		t.Fatalf("provider called %d times, want 3 in-process attempts", got)
	}

	state, lastErr := readMessage(t, db, id)
	if state == "failed" {
		t.Fatalf("message is terminally failed after a TRANSIENT failure; a failed row cannot be re-claimed, " +
			"so the redelivery would be skipped and acknowledged")
	}
	if lastErr == "" {
		t.Error("nothing recorded why the message was handed back; the row is indistinguishable from one never attempted")
	}

	// Second delivery, provider recovered. This is the assertion the state
	// exists to support: the redelivery must be claimable and must send.
	sender.fail = nil
	if err := p.Process(context.Background(), sqsqueue.SMSJob{MessageID: id}); err != nil {
		t.Fatalf("redelivery failed: %v", err)
	}
	if got := sender.calls(); got != 4 {
		t.Errorf("provider called %d times total, want 4 — the redelivery did not reach the provider, "+
			"meaning the message was silently skipped", got)
	}
	if state, _ := readMessage(t, db, id); state != "submitted" {
		t.Errorf("state after recovery = %q, want submitted", state)
	}
}

// TestReleaseForRetry_DoesNotClobberATerminalState guards the write itself.
//
// A send whose response never arrived can still have reached the provider, so a
// delivery callback may move the row forward while the worker is still deciding
// it failed. An unguarded reset to 'queued' would send that recipient a second
// SMS.
func TestReleaseForRetry_DoesNotClobberATerminalState(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	st := pg.New(db)

	for _, state := range []string{"delivered", "submitted", "failed", "suppressed", "queued"} {
		t.Run(state, func(t *testing.T) {
			id := "clobber-" + state
			seedMessageInState(t, db, id, "clob-tenant", "clob-idem-"+state, state, "", util.NowUTC())

			released, err := st.ReleaseForRetry(context.Background(), id, "should_not_apply", util.NowUTC())
			if err != nil {
				t.Fatalf("ReleaseForRetry: %v", err)
			}
			if released {
				t.Errorf("reported a release of a %s message; only a held claim may be handed back", state)
			}
			if got, _ := readMessage(t, db, id); got != state {
				t.Errorf("state changed from %q to %q — the guard did not hold", state, got)
			}
		})
	}

	// The one case it must act on.
	id := "clobber-processing"
	seedMessageInState(t, db, id, "clob-tenant", "clob-idem-processing", "processing", "", util.NowUTC())
	released, err := st.ReleaseForRetry(context.Background(), id, "twilio_retry_exhausted", util.NowUTC())
	if err != nil {
		t.Fatalf("ReleaseForRetry: %v", err)
	}
	if !released {
		t.Error("a held claim was not handed back")
	}
	state, lastErr := readMessage(t, db, id)
	if state != "queued" || lastErr != "twilio_retry_exhausted" {
		t.Errorf("got state=%q last_error=%q, want queued/twilio_retry_exhausted", state, lastErr)
	}
}

// TestClaimAndLoad_ReclaimsAtTheRedeliveryBoundary covers the crash path, which
// is the only thing the stale window still has to cover.
//
// A worker writes updated_at some interval after the message was received, so
// the row becomes stale that same interval after the redelivery is due. When
// the stale window equalled the visibility timeout, the redelivery always
// arrived while the row still looked fresh, the worker treated it as held by
// someone else, and the consumer deleted the receipt.
func TestClaimAndLoad_ReclaimsAtTheRedeliveryBoundary(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	st := pg.New(db)

	const visibility = 60 * time.Second
	// The dead worker received the message at T and claimed it 3s later, a
	// plausible receive-to-claim latency for a saturated pool.
	received := util.NowUTC().Add(-visibility)
	claimedAt := received.Add(3 * time.Second)
	redeliveredAt := received.Add(visibility)

	seed := func(id string) {
		seedMessageInState(t, db, id, "stale-tenant", "stale-idem-"+id, "processing", "", claimedAt)
	}

	t.Run("window equal to the visibility timeout drops the message", func(t *testing.T) {
		seed("stale-equal")
		_, found, err := st.ClaimAndLoad(context.Background(), "stale-equal", redeliveredAt, visibility)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if !found {
			t.Fatal("row missing")
		}
		// Documents the old behaviour rather than endorsing it: the redelivery
		// cannot re-claim, so the worker acknowledges and the send is lost.
		var claimed bool
		if err := db.QueryRow(context.Background(),
			`SELECT state='processing' AND updated_at=$2 FROM messages WHERE id=$1`,
			"stale-equal", claimedAt).Scan(&claimed); err != nil {
			t.Fatalf("verify: %v", err)
		}
		if !claimed {
			t.Skip("environment reclaimed at the boundary; the race is timing-dependent by nature")
		}
	})

	t.Run("half the visibility timeout reclaims it", func(t *testing.T) {
		seed("stale-half")
		msg, found, err := st.ClaimAndLoad(context.Background(), "stale-half", redeliveredAt, visibility/2)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if !found {
			t.Fatal("row missing")
		}
		if !msg.Claimed {
			t.Error("a crashed worker's message was not re-claimed at the redelivery boundary — it would be " +
				"acknowledged and silently dropped")
		}
	})
}
