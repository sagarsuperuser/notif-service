//go:build integration
// +build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"notif/internal/observability"
	"notif/internal/store"
	"notif/internal/store/pg"
	"notif/internal/util"
	"notif/internal/verify"
)

// seedCleanRun writes the state a correct run leaves behind: opted-in
// recipients, delivered messages each with exactly one successful attempt, and
// cap counters that agree with the sends.
func seedCleanRun(t *testing.T, db *pgxpool.Pool, n int) {
	t.Helper()
	ctx := context.Background()
	insertTenant(t, db, "vr")

	for i := 0; i < n; i++ {
		phone := "+1555100" + pad(i)
		if _, err := db.Exec(ctx,
			`INSERT INTO consents (tenant_id, phone, channel, status) VALUES ('vr',$1,'sms','opted_in')`,
			phone); err != nil {
			t.Fatalf("consent: %v", err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO messages (id, tenant_id, idempotency_key, to_phone, template_id, vars_json,
			                      state, provider, provider_msg_id, created_at, updated_at)
			VALUES ($1,'vr',$2,$3,'tpl','{}'::jsonb,'delivered','twilio',$4, now(), now())
		`, "vm-"+pad(i), "vk-"+pad(i), phone, "SM-"+pad(i)); err != nil {
			t.Fatalf("message: %v", err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO provider_attempts (message_id, provider, provider_msg_id, http_status)
			VALUES ($1,'twilio',$2,201)`, "vm-"+pad(i), "SM-"+pad(i)); err != nil {
			t.Fatalf("attempt: %v", err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO send_caps_daily (tenant_id, phone, day, count, updated_at)
			VALUES ('vr',$1,now()::date,1,now())`, phone); err != nil {
			t.Fatalf("cap: %v", err)
		}
	}
}

func pad(i int) string {
	s := "0000"
	d := ""
	for i > 0 {
		d = string(rune('0'+i%10)) + d
		i /= 10
	}
	if d == "" {
		d = "0"
	}
	return s[:4-len(d)] + d
}

func runChecks(t *testing.T, db *pgxpool.Pool, maxPerDay int) map[string]verify.Result {
	t.Helper()
	opts := verify.Options{
		Since:           time.Now().Add(-time.Hour),
		MaxPerDay:       maxPerDay,
		ClaimStaleAfter: 2 * time.Minute,
	}
	results, err := verify.Run(context.Background(), db, verify.Checks(opts))
	if err != nil {
		t.Fatalf("verify.Run: %v", err)
	}
	byName := make(map[string]verify.Result, len(results))
	for _, r := range results {
		byName[r.Name] = r
	}
	return byName
}

// TestVerify_CleanRunPasses is the control. Without it, a checker that always
// reports failure would look just as good as one that works.
func TestVerify_CleanRunPasses(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	seedCleanRun(t, db, 25)

	for name, r := range runChecks(t, db, 5) {
		if !r.OK {
			t.Errorf("clean run failed %q: %d violations, e.g. %s", name, r.Violations, r.Sample)
		}
	}
}

// TestVerify_EachInvariantCatchesItsViolation is the point of the harness. A
// check that cannot fail is worse than no check: it turns an unexamined run
// into one that looks examined. Every invariant is given the exact corruption
// it exists to detect, and must be the one that fires.
func TestVerify_EachInvariantCatchesItsViolation(t *testing.T) {
	cases := []struct {
		invariant string
		maxPerDay int
		corrupt   func(t *testing.T, db *pgxpool.Pool)
	}{
		{
			invariant: "no recipient was sent the same message twice",
			corrupt: func(t *testing.T, db *pgxpool.Pool) {
				exec(t, db, `INSERT INTO provider_attempts (message_id, provider, provider_msg_id, http_status)
				             VALUES ('vm-0001','twilio','SM-DUPE',201)`)
			},
		},
		{
			invariant: "every sent message has a provider id",
			corrupt: func(t *testing.T, db *pgxpool.Pool) {
				exec(t, db, `UPDATE messages SET provider_msg_id = NULL WHERE id='vm-0002'`)
			},
		},
		{
			invariant: "no message is stuck mid-flight",
			corrupt: func(t *testing.T, db *pgxpool.Pool) {
				exec(t, db, `UPDATE messages SET state='processing', updated_at = now() - interval '1 hour'
				              WHERE id='vm-0003'`)
			},
		},
		{
			// last_error is what distinguishes the two queued checks, so each
			// corruption must set it deliberately. Corrupting only the state
			// would trip whichever check happens to match and leave the other
			// unproven.
			invariant: "no message was left queued without being attempted",
			corrupt: func(t *testing.T, db *pgxpool.Pool) {
				exec(t, db, `UPDATE messages SET state='queued', last_error=NULL WHERE id='vm-0004'`)
			},
		},
		{
			invariant: "no message was parked after repeated provider failures",
			corrupt: func(t *testing.T, db *pgxpool.Pool) {
				exec(t, db, `UPDATE messages SET state='queued', last_error='twilio_retry_exhausted'
				              WHERE id='vm-0004'`)
			},
		},
		{
			invariant: "suppressed messages were never sent",
			corrupt: func(t *testing.T, db *pgxpool.Pool) {
				exec(t, db, `UPDATE messages SET state='suppressed' WHERE id='vm-0005'`)
			},
		},
		{
			invariant: "no recipient exceeded their daily cap",
			maxPerDay: 5,
			corrupt: func(t *testing.T, db *pgxpool.Pool) {
				exec(t, db, `UPDATE send_caps_daily SET count = 9 WHERE phone='+15551000006'`)
			},
		},
		{
			invariant: "the cap counter matches the sends that consumed it",
			maxPerDay: 5,
			corrupt: func(t *testing.T, db *pgxpool.Pool) {
				exec(t, db, `UPDATE send_caps_daily SET count = 3 WHERE phone='+15551000007'`)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.invariant, func(t *testing.T) {
			db, cleanup := setupTestDB(t)
			defer cleanup()
			seedCleanRun(t, db, 25)

			maxPerDay := tc.maxPerDay
			if maxPerDay == 0 {
				maxPerDay = 5
			}
			before := runChecks(t, db, maxPerDay)
			if r, ok := before[tc.invariant]; !ok {
				t.Fatalf("no invariant named %q — the harness does not check what this test claims", tc.invariant)
			} else if !r.OK {
				t.Fatalf("invariant %q already failing before corruption; the control is broken", tc.invariant)
			}

			tc.corrupt(t, db)

			after := runChecks(t, db, maxPerDay)
			r := after[tc.invariant]
			if r.OK {
				t.Errorf("invariant %q did NOT detect its violation — this check cannot fail, "+
					"so it makes an unexamined run look examined", tc.invariant)
			}
			if r.Sample == "" {
				t.Errorf("invariant %q fired without a sample; a failure must be diagnosable without a second query", tc.invariant)
			}
			if r.Why == "" || !strings.Contains(r.Why, " ") {
				t.Errorf("invariant %q has no explanation of why it matters", tc.invariant)
			}
		})
	}
}

// TestVerify_DuplicateIdempotencyKeyIsUnreachable checks the one invariant that
// cannot be corrupted by an UPDATE: the database refuses it. That refusal IS
// the guarantee, so the test asserts the constraint rejects the write rather
// than pretending to smuggle a duplicate past it.
func TestVerify_DuplicateIdempotencyKeyIsUnreachable(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	seedCleanRun(t, db, 3)

	_, err := db.Exec(context.Background(), `
		INSERT INTO messages (id, tenant_id, idempotency_key, to_phone, template_id, vars_json, state, created_at, updated_at)
		VALUES ('vm-clash','vr','vk-0000','+15551000000','tpl','{}'::jsonb,'queued', now(), now())`)
	if err == nil {
		t.Fatal("the database accepted a duplicate (tenant_id, idempotency_key); the unique constraint is missing")
	}
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("expected a unique-violation, got: %v", err)
	}

	if r := runChecks(t, db, 5)["idempotency keys are unique per tenant"]; !r.OK {
		t.Errorf("the duplicate check reports %d violations after a rejected insert", r.Violations)
	}
}

// TestVerify_MaxPerDayZeroSkipsCapChecks pins the deliberate omission: without
// the configured cap there is nothing to compare a counter against, so the cap
// checks are absent rather than silently passing.
func TestVerify_MaxPerDayZeroSkipsCapChecks(t *testing.T) {
	checks := verify.Checks(verify.Options{Since: time.Now().Add(-time.Hour)})
	for _, c := range checks {
		if strings.Contains(c.Name, "cap") {
			t.Errorf("cap check %q is present with MaxPerDay unset; it would pass without checking anything", c.Name)
		}
	}
	withCap := verify.Checks(verify.Options{Since: time.Now().Add(-time.Hour), MaxPerDay: 5})
	if len(withCap) <= len(checks) {
		t.Errorf("setting MaxPerDay added no checks (%d vs %d)", len(withCap), len(checks))
	}
}

func exec(t *testing.T, db *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
}

// TestQueryTracer_CountsWhatTheBenchmarkWillReport ties the production metric
// to the claim it is meant to evidence. notif_db_roundtrips_total divided by
// the request rate is statements per request, so the counter must move by
// exactly one per accepted send — otherwise the benchmark's headline number is
// measuring the instrument rather than the system.
func TestQueryTracer_CountsWhatTheBenchmarkWillReport(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	schema := schemaOf(t, db)

	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("DB_DSN")
	}
	traced, err := withSearchPath(dsn, schema)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(observability.DBQueryCalls)
	t.Cleanup(func() { observability.DBQueryCalls.Reset() })
	observability.DBQueryCalls.Reset()

	pool, err := pg.NewPool(context.Background(), traced, pg.PoolOptions{
		MaxConns: 1, MinConns: 1,
		Tracer: &observability.QueryTracer{Service: "test"},
	})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	before := counterValue(t, observability.DBQueryCalls, "test", "ok")

	s := pg.New(pool)
	now := util.NowUTC()
	seedTenantOptedIn(t, db, "tr-tenant", "+15559990000")
	if _, err := s.CreateMessage(context.Background(), store.CreateMessageInput{
		ID: "tr-1", TenantID: "tr-tenant", IdemKey: "tr-idem", To: "+15559990000",
		TemplateID: "tpl", Vars: map[string]string{"n": "1"},
		Day: now, MaxPerDay: 10, Now: now,
	}); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	after := counterValue(t, observability.DBQueryCalls, "test", "ok")
	if delta := after - before; delta != 1 {
		t.Errorf("one accepted send moved notif_db_roundtrips_total by %v, want 1", delta)
	}
}

func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("metric: %v", err)
	}
	if err := c.Write(m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}
