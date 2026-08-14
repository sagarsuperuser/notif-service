//go:build integration
// +build integration

package integration

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"notif/internal/store"
	"notif/internal/store/pg"
	"notif/internal/util"
)

// roundTripCounter counts every query pgx sends to the server. This is the
// quantity the whole exercise is about: at a fixed request rate, statements per
// request is what sets the database's CPU, so it is measured here rather than
// inferred from a load test where it would be tangled up with everything else.
//
// pgx routes BEGIN and COMMIT through the same trace hook as queries, so an
// explicit transaction is counted at its true cost — two extra round-trips on
// top of the statements inside it.
type roundTripCounter struct{ n atomic.Int64 }

func (c *roundTripCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}
func (c *roundTripCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *roundTripCounter) reset() { c.n.Store(0) }
func (c *roundTripCounter) count() int {
	return int(c.n.Load())
}

// countedPool opens a second pool against the schema an existing test pool is
// using, with the counter attached. A separate pool keeps setup and assertion
// traffic out of the measurement.
func countedPool(t *testing.T, schema string) (*pgxpool.Pool, *roundTripCounter) {
	t.Helper()
	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("DB_DSN")
	}
	dbDSN, err := withSearchPath(dsn, schema)
	if err != nil {
		t.Fatalf("build dsn: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dbDSN)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	c := &roundTripCounter{}
	cfg.ConnConfig.Tracer = c
	// One connection, opened before the measurement starts, so connection setup
	// never lands inside a count.
	cfg.MinConns, cfg.MaxConns = 1, 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect counted pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool, c
}

// TestAcceptPathCostsOneRoundTrip is the regression gate on statement
// amplification. The accept path used to issue seven statements per request and
// nine when the daily cap was spent; every one of them was a network round-trip
// holding a pooled connection. Collapsing them into one statement is the reason
// the same database serves several times the request rate, so the count is
// asserted rather than left to drift back up.
//
// The legacy sequence is measured alongside it, on the same counter, so the
// ratio in the commit message is a measurement and not an estimate.
func TestAcceptPathCostsOneRoundTrip(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	schema := schemaOf(t, db)
	counted, c := countedPool(t, schema)

	ctx := context.Background()
	now := util.NowUTC()
	tenant, phone := "rt-tenant", "+15550006666"
	seedTenantOptedIn(t, db, tenant, phone)

	s := pg.New(counted)

	c.reset()
	if _, err := s.CreateMessage(ctx, store.CreateMessageInput{
		ID: "rt-1", TenantID: tenant, IdemKey: "rt-idem-1", To: phone,
		TemplateID: "tpl", Vars: map[string]string{"n": "1"},
		Day: now, MaxPerDay: 10, Now: now,
	}); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	accepted := c.count()

	// The rejected path must not be more expensive than the accepted one — the
	// old code's worst case was the cap-exceeded path, which paid for a
	// compensating decrement and a state update on top of everything else.
	c.reset()
	if _, err := s.CreateMessage(ctx, store.CreateMessageInput{
		ID: "rt-2", TenantID: tenant, IdemKey: "rt-idem-2", To: phone,
		TemplateID: "tpl", Vars: map[string]string{"n": "1"},
		Day: now, MaxPerDay: 0, Now: now,
	}); err != nil {
		t.Fatalf("CreateMessage (cap spent): %v", err)
	}
	capSpent := c.count()

	c.reset()
	legacyCreate(t, counted, "rt-legacy", tenant, "rt-idem-legacy", phone, 10, now)
	legacy := c.count()

	t.Logf("round-trips per accept: new=%d (cap spent: %d), legacy=%d", accepted, capSpent, legacy)

	if accepted != 1 {
		t.Errorf("accepted send cost %d round-trips, want 1", accepted)
	}
	if capSpent != 1 {
		t.Errorf("cap-exceeded send cost %d round-trips, want 1", capSpent)
	}
	if legacy <= accepted {
		t.Fatalf("legacy sequence measured %d round-trips, which is not more than the new %d — "+
			"the comparison is broken, not the code", legacy, accepted)
	}
}

// TestWebhookPathCostsOneRoundTrip is the same gate for the provider callback,
// which previously issued an event INSERT and a message UPDATE — and retried
// the UPDATE up to ten times while holding its connection.
func TestWebhookPathCostsOneRoundTrip(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	schema := schemaOf(t, db)
	counted, c := countedPool(t, schema)

	ctx := context.Background()
	now := util.NowUTC()
	seedSubmittedMessage(t, db, "wh-rt-1", "SM-rt-1")

	s := pg.New(counted)
	c.reset()
	matched, err := s.RecordDeliveryEvent(ctx, store.DeliveryEventRecord{
		Provider: "twilio", ProviderMsgID: "SM-rt-1", VendorStatus: "delivered",
		NewState: "delivered", Now: now,
	})
	if err != nil {
		t.Fatalf("RecordDeliveryEvent: %v", err)
	}
	got := c.count()
	t.Logf("round-trips per callback: %d (matched=%v)", got, matched)
	if got != 1 {
		t.Errorf("callback cost %d round-trips, want 1", got)
	}
	if !matched {
		t.Errorf("callback did not match the seeded message")
	}
}

func schemaOf(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	var schema string
	if err := db.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	return schema
}
