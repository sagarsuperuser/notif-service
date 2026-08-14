//go:build integration
// +build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReconcileUsesTheTerminalEventIndex is the check that the index added for
// reconcile-submitted is actually the one the planner picks.
//
// An index is a claim about a query plan, and a claim about a query plan is
// worth exactly as much as the EXPLAIN that backs it. This seeds enough rows
// for the planner to prefer an index over a scan, then reads the plan for the
// real reconcile SQL — the file the CronJob runs, not a paraphrase of it.
func TestReconcileUsesTheTerminalEventIndex(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// A sequential scan is cheaper than an index on a tiny table, and correctly
	// so. Seed enough rows that the planner has a real decision to make.
	insertTenant(t, db, "sch-tenant")
	const n = 5000
	if _, err := db.Exec(ctx, `
		INSERT INTO messages (id, tenant_id, idempotency_key, to_phone, template_id, vars_json,
		                      state, provider, provider_msg_id, created_at, updated_at)
		SELECT 'sm-'||i, 'sch-tenant', 'sk-'||i, '+15550000000', 'tpl', '{}'::jsonb,
		       'submitted', 'twilio', 'SM-'||i, now() - interval '1 hour', now() - interval '1 hour'
		  FROM generate_series(1,$1) AS i
	`, n); err != nil {
		t.Fatalf("seed messages: %v", err)
	}
	// Three events per message: two non-terminal, one terminal. The partial
	// index should hold only the terminal third.
	if _, err := db.Exec(ctx, `
		INSERT INTO delivery_events (provider, provider_msg_id, vendor_status, payload_json, received_at)
		SELECT 'twilio', 'SM-'||i, s.status, '{}'::jsonb, now() - (i || ' seconds')::interval
		  FROM generate_series(1,$1) AS i,
		       (VALUES ('queued'),('sent'),('delivered')) AS s(status)
	`, n); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	if _, err := db.Exec(ctx, `ANALYZE delivery_events; ANALYZE messages;`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	sqlBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "jobs", "sql", "reconcile-submitted.sql"))
	if err != nil {
		t.Fatalf("read reconcile sql: %v", err)
	}

	rows, err := db.Query(ctx, "EXPLAIN "+string(sqlBytes))
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	got := plan.String()
	t.Logf("reconcile plan:\n%s", got)

	if !strings.Contains(got, "idx_delivery_events_terminal") {
		t.Errorf("reconcile does not use idx_delivery_events_terminal; the index does not match the query it was added for.\nPlan:\n%s", got)
	}
	if strings.Contains(got, "Seq Scan on delivery_events") {
		t.Errorf("reconcile sequentially scans delivery_events, which grows without bound.\nPlan:\n%s", got)
	}
}

// TestMessagesCarriesOnlyIndexesSomethingReads guards the hottest table against
// index creep. Every secondary index here is maintained on every accepted send,
// so one that no query uses is a permanent tax. Adding an index is fine — this
// test just requires that it arrives with a reader.
func TestMessagesCarriesOnlyIndexesSomethingReads(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	rows, err := db.Query(context.Background(), `
		SELECT indexname FROM pg_indexes
		 WHERE schemaname = current_schema() AND tablename = 'messages'
		 ORDER BY indexname
	`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name)
	}

	// Each of these is named by the query that needs it.
	want := map[string]string{
		"messages_pkey":                          "ClaimAndLoad and RecordAttempt look messages up by id",
		"messages_tenant_id_idempotency_key_key": "CreateMessage's idempotency gate, and the constraint that makes duplicates impossible",
		"idx_messages_provider_msg_id":           "the webhook path updates by (provider, provider_msg_id)",
	}

	for _, name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("messages carries index %q with no reader named for it. "+
				"Every index here is maintained on every accepted send — add the query that needs it, "+
				"or drop the index.", name)
		}
	}
	for name, why := range want {
		found := false
		for _, g := range got {
			if g == name {
				found = true
			}
		}
		if !found {
			t.Errorf("index %q is missing; it is needed because %s", name, why)
		}
	}
	fmt.Fprintf(os.Stderr, "messages indexes: %v\n", got)
}
