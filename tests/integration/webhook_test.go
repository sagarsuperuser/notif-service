//go:build integration
// +build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"notif/internal/httpserver"
	"notif/internal/providers/twilio"
	"notif/internal/store"
	"notif/internal/store/pg"
	"notif/internal/util"
)

// seedSubmittedMessage inserts a message already advanced to 'submitted' with a
// provider id, i.e. the state a message is in when its callbacks arrive.
func seedSubmittedMessage(t *testing.T, db *pgxpool.Pool, id, sid string) {
	t.Helper()
	_, err := db.Exec(context.Background(), `
		INSERT INTO messages (id, tenant_id, idempotency_key, to_phone, template_id, vars_json,
		                      state, provider, provider_msg_id, created_at, updated_at)
		VALUES ($1,'t-wh',$1,'+15550000000','tpl','{}'::jsonb,'submitted','twilio',$2,now(),now())
	`, id, sid)
	if err != nil {
		t.Fatalf("seed message %s: %v", id, err)
	}
}

func messageRow(t *testing.T, db *pgxpool.Pool, id string) (state, lastErr string) {
	t.Helper()
	err := db.QueryRow(context.Background(),
		`SELECT state, COALESCE(last_error,'') FROM messages WHERE id=$1`, id).Scan(&state, &lastErr)
	if err != nil {
		t.Fatalf("read message %s: %v", id, err)
	}
	return state, lastErr
}

// runReconcile executes the real reconcile-submitted.sql the CronJob runs, so
// the compensator the handler now depends on is exercised by the same file that
// ships, not a paraphrase of it.
func runReconcile(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	sqlBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "jobs", "sql", "reconcile-submitted.sql"))
	if err != nil {
		t.Fatalf("read reconcile sql: %v", err)
	}
	if _, err := db.Exec(context.Background(), string(sqlBytes)); err != nil {
		t.Fatalf("run reconcile: %v", err)
	}
}

type eventRow struct {
	VendorStatus string
	ErrorCode    string
	Payload      string
}

func eventRows(t *testing.T, db *pgxpool.Pool, sid string) []eventRow {
	t.Helper()
	rows, err := db.Query(context.Background(), `
		SELECT vendor_status, COALESCE(error_code,''), COALESCE(payload_json::text,'')
		FROM delivery_events WHERE provider_msg_id=$1 ORDER BY id
	`, sid)
	if err != nil {
		t.Fatalf("read events %s: %v", sid, err)
	}
	defer rows.Close()
	var out []eventRow
	for rows.Next() {
		var e eventRow
		if err := rows.Scan(&e.VendorStatus, &e.ErrorCode, &e.Payload); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		out = append(out, e)
	}
	return out
}

// legacyApply replays the PREVIOUS handler's database work: insert the event,
// then a separate unguarded UPDATE. Kept in the test rather than in production
// code so the old behaviour is pinned as a comparison baseline without leaving
// a second write path alive in the service.
func legacyApply(t *testing.T, db *pgxpool.Pool, sid, status, errCode, newState string, payload any) {
	t.Helper()
	ctx := context.Background()
	b, _ := json.Marshal(payload)
	var ec any
	if errCode != "" {
		ec = errCode
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO delivery_events (provider, provider_msg_id, vendor_status, error_code, payload_json, occurred_at)
		VALUES ('twilio',$1,$2,$3,$4,NULL)
	`, sid, status, ec, b); err != nil {
		t.Fatalf("legacy insert event: %v", err)
	}
	if newState == "" {
		return
	}
	if _, err := db.Exec(ctx, `
		UPDATE messages SET state=$2, last_error=$3, updated_at=$4
		WHERE provider='twilio' AND provider_msg_id=$1
	`, sid, newState, ec, util.NowUTC()); err != nil {
		t.Fatalf("legacy update: %v", err)
	}
}

// TestRecordDeliveryEvent_MatchesLegacySequence is the differential proof for
// collapsing two statements into one CTE: for every case whose behaviour is NOT
// deliberately changed, the single statement must leave the database in exactly
// the state the old insert-then-update sequence did. The cases that DO differ on
// purpose (terminal-state guard, missing-message handling) are asserted
// separately below, so an unintended divergence cannot hide behind an intended
// one.
func TestRecordDeliveryEvent_MatchesLegacySequence(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)
	ctx := context.Background()

	cases := []struct {
		name     string
		status   string
		errCode  string
		newState string
	}{
		{"non-terminal queued", "queued", "", ""},
		{"non-terminal sent", "sent", "", ""},
		{"terminal delivered", "delivered", "", "delivered"},
		{"terminal failed with code", "failed", "30008", "failed"},
		{"terminal undelivered", "undelivered", "30003", "failed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldID, oldSID := "m-old-"+tc.status, "SM-old-"+tc.status
			newID, newSID := "m-new-"+tc.status, "SM-new-"+tc.status
			seedSubmittedMessage(t, db, oldID, oldSID)
			seedSubmittedMessage(t, db, newID, newSID)

			payload := map[string][]string{"MessageStatus": {tc.status}}

			legacyApply(t, db, oldSID, tc.status, tc.errCode, tc.newState, payload)

			if _, err := s.RecordDeliveryEvent(ctx, store.DeliveryEventRecord{
				Provider: "twilio", ProviderMsgID: newSID, VendorStatus: tc.status,
				ErrorCode: tc.errCode, Payload: payload, NewState: tc.newState, Now: util.NowUTC(),
			}); err != nil {
				t.Fatalf("RecordDeliveryEvent: %v", err)
			}

			oldState, oldErr := messageRow(t, db, oldID)
			newState, newErr := messageRow(t, db, newID)
			if oldState != newState || oldErr != newErr {
				t.Errorf("message diverged: legacy=(%s,%q) new=(%s,%q)", oldState, oldErr, newState, newErr)
			}

			oldEvents, newEvents := eventRows(t, db, oldSID), eventRows(t, db, newSID)
			if len(oldEvents) != len(newEvents) {
				t.Fatalf("event count diverged: legacy=%d new=%d", len(oldEvents), len(newEvents))
			}
			for i := range oldEvents {
				if oldEvents[i] != newEvents[i] {
					t.Errorf("event %d diverged: legacy=%+v new=%+v", i, oldEvents[i], newEvents[i])
				}
			}
		})
	}
}

// TestRecordDeliveryEvent_GuardsTerminalState pins an INTENDED divergence: a
// second terminal callback (a provider redelivery, or two terminals arriving out
// of order) must not rewrite a message that already reached a terminal state.
// The legacy sequence had no guard and would clobber it — asserted here in both
// directions so the guard is proven, not assumed.
func TestRecordDeliveryEvent_GuardsTerminalState(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)
	ctx := context.Background()

	seedSubmittedMessage(t, db, "m-guard", "SM-guard")
	seedSubmittedMessage(t, db, "m-legacy-guard", "SM-legacy-guard")

	rec := func(sid, status, errCode, newState string) {
		if _, err := s.RecordDeliveryEvent(ctx, store.DeliveryEventRecord{
			Provider: "twilio", ProviderMsgID: sid, VendorStatus: status,
			ErrorCode: errCode, Payload: map[string][]string{}, NewState: newState, Now: util.NowUTC(),
		}); err != nil {
			t.Fatalf("record %s: %v", status, err)
		}
	}

	rec("SM-guard", "delivered", "", "delivered")
	rec("SM-guard", "undelivered", "30003", "failed") // late, out of order

	if state, _ := messageRow(t, db, "m-guard"); state != "delivered" {
		t.Errorf("terminal state was overwritten: got %s, want delivered", state)
	}
	// Both events must still be recorded — the guard protects the message row,
	// never the audit trail.
	if got := len(eventRows(t, db, "SM-guard")); got != 2 {
		t.Errorf("expected both events stored, got %d", got)
	}

	// The legacy path is the control: without the guard it clobbers.
	legacyApply(t, db, "SM-legacy-guard", "delivered", "", "delivered", map[string][]string{})
	legacyApply(t, db, "SM-legacy-guard", "undelivered", "30003", "failed", map[string][]string{})
	if state, _ := messageRow(t, db, "m-legacy-guard"); state != "failed" {
		t.Fatalf("control invalid: legacy path was expected to clobber to failed, got %s", state)
	}
}

// TestRecordDeliveryEvent_PersistsEventWhenNoMessageMatches covers the race the
// old retry loop existed for: a callback can arrive before the worker has
// written provider_msg_id. The event must still be stored — the reconcile job
// reads delivery_events, so an event it never sees is one it can never repair.
func TestRecordDeliveryEvent_PersistsEventWhenNoMessageMatches(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)
	ctx := context.Background()

	matched, err := s.RecordDeliveryEvent(ctx, store.DeliveryEventRecord{
		Provider: "twilio", ProviderMsgID: "SM-orphan", VendorStatus: "delivered",
		Payload: map[string][]string{}, NewState: "delivered", Now: util.NowUTC(),
	})
	if err != nil {
		t.Fatalf("RecordDeliveryEvent: %v", err)
	}
	if matched {
		t.Error("matched should be false when no message row exists")
	}
	if got := len(eventRows(t, db, "SM-orphan")); got != 1 {
		t.Fatalf("event must persist even with no message: got %d rows, want 1", got)
	}

	// And the reconcile job must then be able to repair it once the message
	// exists — this is the compensation path the handler now relies on instead
	// of retrying while holding a connection.
	seedSubmittedMessage(t, db, "m-orphan", "SM-orphan")
	if _, err := db.Exec(ctx,
		`UPDATE messages SET updated_at = now() - interval '1 minute' WHERE id='m-orphan'`); err != nil {
		t.Fatalf("age message: %v", err)
	}
	runReconcile(t, db)
	if state, _ := messageRow(t, db, "m-orphan"); state != "delivered" {
		t.Errorf("reconcile did not apply the stored event: state=%s, want delivered", state)
	}
}

// TestRecordDeliveryEvent_ConcurrentCallbacks: providers redeliver, and the mock
// fires three callbacks per message, so concurrent writes for one provider id
// are normal. Every event must be recorded and the message must land on exactly
// one terminal state.
func TestRecordDeliveryEvent_ConcurrentCallbacks(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)

	seedSubmittedMessage(t, db, "m-conc", "SM-conc")

	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, newState := "sent", ""
			if i%2 == 0 {
				status, newState = "delivered", "delivered"
			}
			_, err := s.RecordDeliveryEvent(context.Background(), store.DeliveryEventRecord{
				Provider: "twilio", ProviderMsgID: "SM-conc", VendorStatus: status,
				Payload: map[string][]string{}, NewState: newState, Now: util.NowUTC(),
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent record failed: %v", err)
	}

	if got := len(eventRows(t, db, "SM-conc")); got != n {
		t.Errorf("expected all %d events stored, got %d", n, got)
	}
	if state, _ := messageRow(t, db, "m-conc"); state != "delivered" {
		t.Errorf("message state = %s, want delivered", state)
	}
}

// TestWebhookHandler_Returns200WhenMessageMissing pins the amplification fix.
// The old handler answered 503 when no message matched, which made the provider
// redeliver the entire event — so a benign race generated more load, which
// caused more races. It must now answer 200 and let reconcile do the repair.
func TestWebhookHandler_Returns200WhenMessageMissing(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	authToken := "testtoken"
	publicURL := "https://example.com/v1/webhooks/twilio/status"
	form := url.Values{
		"MessageSid":    []string{"SM-missing"},
		"MessageStatus": []string{"delivered"},
		"ErrorCode":     []string{""},
	}
	req := httptest.NewRequest(http.MethodPost, publicURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", twilioSignature(authToken, publicURL, form))

	s := httpserver.New()
	(&httpserver.Webhook{
		Store:           pg.New(db),
		VerifySignature: twilio.VerifySignature,
		AuthToken:       authToken,
		PublicURL:       publicURL,
	}).Register(s.Mux)

	rr := httptest.NewRecorder()
	start := time.Now()
	s.Mux.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for an unmatched callback, got %d", rr.Code)
	}
	// The old handler slept through ten backoffs (~1.4s worst case) while
	// holding a pooled connection before giving up. Nothing may sleep here.
	if elapsed > 500*time.Millisecond {
		t.Errorf("handler took %v — it must not retry/sleep on a miss", elapsed)
	}
	if got := len(eventRows(t, db, "SM-missing")); got != 1 {
		t.Errorf("event should still be recorded: got %d rows, want 1", got)
	}
}
