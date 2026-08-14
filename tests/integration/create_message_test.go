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

	"notif/internal/store"
	"notif/internal/store/pg"
	"notif/internal/util"
)

// legacyCreate replays the PREVIOUS accept path exactly: idempotency SELECT,
// message INSERT as 'queued', suppression SELECT, consent SELECT, then the cap
// transaction (BEGIN, increment, compensating decrement when over, COMMIT), and
// a state UPDATE on every rejection. Seven statements on the happy path, nine
// when the cap is spent. Kept here rather than in production code so the old
// behaviour is pinned as a baseline without leaving a second write path alive.
func legacyCreate(t *testing.T, db *pgxpool.Pool, id, tenantID, idemKey, phone string, maxPerDay int, now time.Time) (state, lastErr string) {
	t.Helper()
	ctx := context.Background()

	var foundID, foundState string
	err := db.QueryRow(ctx, `SELECT id, state FROM messages WHERE tenant_id=$1 AND idempotency_key=$2`,
		tenantID, idemKey).Scan(&foundID, &foundState)
	if err == nil {
		return foundState, ""
	}

	vars, _ := json.Marshal(map[string]string{"n": "1"})
	if _, err := db.Exec(ctx, `
		INSERT INTO messages (id, tenant_id, idempotency_key, to_phone, template_id, vars_json, state, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'tpl',$5,'queued',$6,$6)`, id, tenantID, idemKey, phone, vars, now); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}

	mark := func(reason string) (string, string) {
		if _, err := db.Exec(ctx, `UPDATE messages SET state='suppressed', last_error=$2, updated_at=$3 WHERE id=$1`,
			id, reason, now); err != nil {
			t.Fatalf("legacy mark %s: %v", reason, err)
		}
		return "suppressed", reason
	}

	var one int
	if err := db.QueryRow(ctx, `SELECT 1 FROM suppression_list WHERE tenant_id=$1 AND phone=$2`,
		tenantID, phone).Scan(&one); err == nil {
		return mark("suppressed")
	}

	var st string
	if err := db.QueryRow(ctx, `SELECT status FROM consents WHERE tenant_id=$1 AND phone=$2 AND channel='sms'`,
		tenantID, phone).Scan(&st); err != nil || st != "opted_in" {
		return mark("not_opted_in")
	}

	day := now.UTC().Truncate(24 * time.Hour)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("legacy begin: %v", err)
	}
	var newCount int
	if err := tx.QueryRow(ctx, `
		INSERT INTO send_caps_daily (tenant_id, phone, day, count, updated_at)
		VALUES ($1,$2,$3,1,now())
		ON CONFLICT (tenant_id, phone, day) DO UPDATE SET count = send_caps_daily.count + 1, updated_at=now()
		RETURNING count`, tenantID, phone, day).Scan(&newCount); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("legacy cap: %v", err)
	}
	allowed := newCount <= maxPerDay
	if !allowed {
		if _, err := tx.Exec(ctx, `
			UPDATE send_caps_daily SET count = count - 1, updated_at=now()
			WHERE tenant_id=$1 AND phone=$2 AND day=$3`, tenantID, phone, day); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("legacy decrement: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("legacy commit: %v", err)
	}
	if !allowed {
		return mark("cap_exceeded")
	}
	return "queued", ""
}

func capCount(t *testing.T, db *pgxpool.Pool, tenantID, phone string, now time.Time) int {
	t.Helper()
	day := now.UTC().Truncate(24 * time.Hour)
	var c int
	err := db.QueryRow(context.Background(),
		`SELECT count FROM send_caps_daily WHERE tenant_id=$1 AND phone=$2 AND day=$3`,
		tenantID, phone, day).Scan(&c)
	if err != nil {
		return 0
	}
	return c
}

func seedSuppressed(t *testing.T, db *pgxpool.Pool, tenantID, phone string) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO suppression_list (tenant_id, phone, reason) VALUES ($1,$2,'test')`,
		tenantID, phone); err != nil {
		t.Fatalf("seed suppression: %v", err)
	}
}

func seedOptedOut(t *testing.T, db *pgxpool.Pool, tenantID, phone string) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO consents (tenant_id, phone, channel, status) VALUES ($1,$2,'sms','opted_out')`,
		tenantID, phone); err != nil {
		t.Fatalf("seed opted_out: %v", err)
	}
}

// TestCreateMessage_MatchesLegacySequence is the differential proof for
// collapsing 7-9 statements into one: for every gate outcome, the single
// statement must produce the same message state, the same last_error, and the
// same daily-cap side effect as the old sequence. The cap column matters as
// much as the message row — the old path reached the same answer by
// incrementing and then compensating, so a divergence there would be invisible
// from the response alone.
func TestCreateMessage_MatchesLegacySequence(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)
	ctx := context.Background()
	now := util.NowUTC()

	cases := []struct {
		name  string
		seed  func(t *testing.T, db *pgxpool.Pool, tenant, phone string)
		max   int
		prime int // sends to burn before the compared call
	}{
		{"happy path", seedTenantOptedIn, 10, 0},
		{"suppressed", func(t *testing.T, db *pgxpool.Pool, tn, p string) {
			seedTenantOptedIn(t, db, tn, p)
			seedSuppressed(t, db, tn, p)
		}, 10, 0},
		{"opted out", func(t *testing.T, db *pgxpool.Pool, tn, p string) {
			insertTenant(t, db, tn)
			seedOptedOut(t, db, tn, p)
		}, 10, 0},
		{"no consent row", func(t *testing.T, db *pgxpool.Pool, tn, p string) { insertTenant(t, db, tn) }, 10, 0},
		{"cap exceeded", seedTenantOptedIn, 2, 2},
		{"cap of zero", seedTenantOptedIn, 0, 0},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldTenant := fmt.Sprintf("t-old-%d", i)
			newTenant := fmt.Sprintf("t-new-%d", i)
			phone := "+1555000" + fmt.Sprintf("%04d", i)
			tc.seed(t, db, oldTenant, phone)
			tc.seed(t, db, newTenant, phone)

			// Burn allowance identically on both sides before the compared call.
			for p := 0; p < tc.prime; p++ {
				legacyCreate(t, db, fmt.Sprintf("m-old-p%d-%d", i, p), oldTenant,
					fmt.Sprintf("idem-p%d", p), phone, tc.max, now)
				if _, err := s.CreateMessage(ctx, store.CreateMessageInput{
					ID: fmt.Sprintf("m-new-p%d-%d", i, p), TenantID: newTenant,
					IdemKey: fmt.Sprintf("idem-p%d", p), To: phone, TemplateID: "tpl",
					Vars: map[string]string{"n": "1"}, Day: now, MaxPerDay: tc.max, Now: now,
				}); err != nil {
					t.Fatalf("prime new: %v", err)
				}
			}

			wantState, wantErr := legacyCreate(t, db, fmt.Sprintf("m-old-%d", i), oldTenant, "idem-x", phone, tc.max, now)

			got, err := s.CreateMessage(ctx, store.CreateMessageInput{
				ID: fmt.Sprintf("m-new-%d", i), TenantID: newTenant, IdemKey: "idem-x", To: phone,
				TemplateID: "tpl", Vars: map[string]string{"n": "1"}, Day: now, MaxPerDay: tc.max, Now: now,
			})
			if err != nil {
				t.Fatalf("CreateMessage: %v", err)
			}

			if got.State != wantState || got.LastError != wantErr {
				t.Errorf("decision diverged: legacy=(%s,%q) new=(%s,%q)", wantState, wantErr, got.State, got.LastError)
			}
			// The persisted row must agree with what was returned.
			dbState, dbErr := messageRow(t, db, fmt.Sprintf("m-new-%d", i))
			if dbState != got.State || dbErr != got.LastError {
				t.Errorf("returned (%s,%q) but stored (%s,%q)", got.State, got.LastError, dbState, dbErr)
			}
			if oldCap, newCap := capCount(t, db, oldTenant, phone, now), capCount(t, db, newTenant, phone, now); oldCap != newCap {
				t.Errorf("daily cap diverged: legacy=%d new=%d", oldCap, newCap)
			}
		})
	}
}

// TestCreateMessage_IdempotentRetryConsumesNoAllowance: a retry must return the
// first decision AND must not spend a second unit of the recipient's daily cap.
// The old path got this by returning before the cap transaction; the new one
// needs the NOT EXISTS guard inside the cap CTE, so it is asserted explicitly.
func TestCreateMessage_IdempotentRetryConsumesNoAllowance(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)
	ctx := context.Background()
	now := util.NowUTC()

	tenant, phone := "t-idem", "+15550009999"
	seedTenantOptedIn(t, db, tenant, phone)

	in := store.CreateMessageInput{
		ID: "m-idem-1", TenantID: tenant, IdemKey: "same-key", To: phone, TemplateID: "tpl",
		Vars: map[string]string{"n": "1"}, Day: now, MaxPerDay: 10, Now: now,
	}
	first, err := s.CreateMessage(ctx, in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Existing || first.State != "queued" {
		t.Fatalf("first call: got %+v, want fresh queued", first)
	}
	afterFirst := capCount(t, db, tenant, phone, now)

	in.ID = "m-idem-2" // a new id, same idempotency key
	second, err := s.CreateMessage(ctx, in)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.Existing {
		t.Error("retry should report Existing=true")
	}
	if second.MessageID != first.MessageID {
		t.Errorf("retry resolved to %s, want the original %s", second.MessageID, first.MessageID)
	}
	if got := capCount(t, db, tenant, phone, now); got != afterFirst {
		t.Errorf("retry consumed allowance: cap %d -> %d", afterFirst, got)
	}
	var rows int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM messages WHERE tenant_id=$1 AND idempotency_key=$2`,
		tenant, "same-key").Scan(&rows)
	if rows != 1 {
		t.Errorf("expected exactly 1 message row for the key, got %d", rows)
	}
}

// TestCreateMessage_CapBoundaryUnderConcurrency is the race the conditional
// upsert exists for. N goroutines send to one recipient whose cap is max; the
// database must admit EXACTLY max of them. Over-admitting means billing a
// customer past their limit; under-admitting means dropping paid traffic.
func TestCreateMessage_CapBoundaryUnderConcurrency(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)
	now := util.NowUTC()

	const attempts, max = 40, 12
	tenant, phone := "t-cap-race", "+15550008888"
	seedTenantOptedIn(t, db, tenant, phone)

	var mu sync.Mutex
	queued := 0
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := s.CreateMessage(context.Background(), store.CreateMessageInput{
				ID: fmt.Sprintf("m-race-%d", i), TenantID: tenant, IdemKey: fmt.Sprintf("k-%d", i),
				To: phone, TemplateID: "tpl", Vars: map[string]string{"n": "1"},
				Day: now, MaxPerDay: max, Now: now,
			})
			if err != nil {
				t.Errorf("concurrent create: %v", err)
				return
			}
			if res.State == "queued" {
				mu.Lock()
				queued++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if queued != max {
		t.Errorf("admitted %d of %d attempts, want exactly %d", queued, attempts, max)
	}
	if got := capCount(t, db, tenant, phone, now); got != max {
		t.Errorf("cap counter = %d, want %d (it must never overshoot)", got, max)
	}
}

// TestCreateMessage_ConcurrentDuplicateKeys: the same idempotency key fired
// concurrently must still yield exactly one message row. The old path used a
// bare INSERT and would fail one caller with a unique-violation error.
func TestCreateMessage_ConcurrentDuplicateKeys(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	s := pg.New(db)
	now := util.NowUTC()

	tenant, phone := "t-dup", "+15550007777"
	seedTenantOptedIn(t, db, tenant, phone)

	const n = 16
	var wg sync.WaitGroup
	ids := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := s.CreateMessage(context.Background(), store.CreateMessageInput{
				ID: fmt.Sprintf("m-dup-%d", i), TenantID: tenant, IdemKey: "one-key",
				To: phone, TemplateID: "tpl", Vars: map[string]string{"n": "1"},
				Day: now, MaxPerDay: 100, Now: now,
			})
			if err != nil {
				t.Errorf("concurrent duplicate: %v", err)
				return
			}
			ids <- res.MessageID
		}(i)
	}
	wg.Wait()
	close(ids)

	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Errorf("callers resolved to %d different message ids, want 1: %v", len(seen), seen)
	}
	var rows int
	_ = db.QueryRow(context.Background(),
		`SELECT count(*) FROM messages WHERE tenant_id=$1 AND idempotency_key=$2`, tenant, "one-key").Scan(&rows)
	if rows != 1 {
		t.Errorf("expected exactly 1 message row, got %d", rows)
	}
}
