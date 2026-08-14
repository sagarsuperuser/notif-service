//go:build integration
// +build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"

	"notif/internal/httpserver"
	"notif/internal/service"
	"notif/internal/store/pg"
	"notif/internal/util"
)

// recordingQueue counts enqueues per message so a test can assert that an
// idempotent retry does not hand the worker a second copy of the same send.
type recordingQueue struct {
	mu sync.Mutex
	by map[string]int
}

func newRecordingQueue() *recordingQueue { return &recordingQueue{by: map[string]int{}} }

func (q *recordingQueue) EnqueueSMS(ctx context.Context, tenantID, messageID, idempotencyKey, to, templateID string, vars map[string]string, campaignID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.by[messageID]++
	return nil
}

func (q *recordingQueue) total() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, c := range q.by {
		n += c
	}
	return n
}

// newAPI wires the real router, handler, service and Postgres store — the same
// objects cmd/api builds — so these tests exercise the accept path a load
// generator will hit, not a slice of it. Message ids are handed out serially so
// a test can tell which request created which row.
func newAPI(t *testing.T, q service.Queue, maxPerDay int) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	var mu sync.Mutex
	n := 0
	api := &httpserver.API{
		Svc: &service.NotificationService{
			Store:     pg.New(db),
			Queue:     q,
			MaxPerDay: maxPerDay,
		},
		IDGen: func() string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return fmt.Sprintf("e2e-%d", n)
		},
	}
	r := mux.NewRouter()
	api.Register(r)
	return r, db
}

func postSMS(t *testing.T, h http.Handler, body map[string]any) (int, map[string]string) {
	t.Helper()
	b, _ := json.Marshal(body)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/v1/sms/messages", bytes.NewReader(b)))
	var out map[string]string
	_ = json.Unmarshal(rw.Body.Bytes(), &out)
	return rw.Code, out
}

func countMessages(t *testing.T, db *pgxpool.Pool, tenant, idemKey string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM messages WHERE tenant_id=$1 AND idempotency_key=$2`,
		tenant, idemKey).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// TestAPI_AcceptPathEndToEnd walks a send from HTTP request to persisted row to
// enqueue, then repeats it with the same idempotency key. The retry is the part
// that matters: it must return the original message id, must not write a second
// row, must not consume a second unit of the daily cap, and must not enqueue a
// second copy — four separate ways the collapsed statement could regress while
// still answering 202.
func TestAPI_AcceptPathEndToEnd(t *testing.T) {
	q := newRecordingQueue()
	h, db := newAPI(t, q, 10)
	now := util.NowUTC()

	tenant, phone := "e2e-tenant", "+15550009999"
	seedTenantOptedIn(t, db, tenant, phone)

	body := map[string]any{
		"tenantId": tenant, "idempotencyKey": "e2e-idem", "to": phone,
		"templateId": "tpl", "vars": map[string]string{"n": "1"},
	}

	code, resp := postSMS(t, h, body)
	if code != http.StatusAccepted {
		t.Fatalf("first POST: got %d, want 202 (body %v)", code, resp)
	}
	if resp["state"] != "queued" {
		t.Fatalf("first POST state = %q, want queued", resp["state"])
	}
	first := resp["messageId"]

	if got, _ := messageRow(t, db, first); got != "queued" {
		t.Fatalf("stored state = %q, want queued", got)
	}
	if n := capCount(t, db, tenant, phone, now); n != 1 {
		t.Fatalf("cap after first send = %d, want 1", n)
	}

	code, resp = postSMS(t, h, body)
	if code != http.StatusAccepted {
		t.Fatalf("retry POST: got %d, want 202", code)
	}
	if resp["messageId"] != first {
		t.Errorf("retry returned a different message id: %q then %q", first, resp["messageId"])
	}
	if n := capCount(t, db, tenant, phone, now); n != 1 {
		t.Errorf("retry consumed daily allowance: cap = %d, want 1", n)
	}
	if n := q.total(); n != 1 {
		t.Errorf("retry enqueued a duplicate send: %d enqueues, want 1", n)
	}
	if n := countMessages(t, db, tenant, "e2e-idem"); n != 1 {
		t.Errorf("idempotency key produced %d rows, want 1", n)
	}

	// The message must be readable through the API by the id it returned.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/v1/messages/"+first, nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET /v1/messages/%s: got %d, want 200", first, rw.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if got["State"] != "queued" {
		t.Errorf("GET returned state %v, want queued (body %s)", got["State"], rw.Body.String())
	}
	if got["ID"] != first {
		t.Errorf("GET returned id %v, want %s", got["ID"], first)
	}
}

// TestAPI_RejectedSendIsNotEnqueued pins the boundary the handler owns: a
// recipient the gates reject still gets a 202 with the reason in the body, but
// nothing reaches the queue. Answering 4xx would be wrong — the request was
// well-formed and the outcome is a business decision, not a client error.
func TestAPI_RejectedSendIsNotEnqueued(t *testing.T) {
	q := newRecordingQueue()
	h, db := newAPI(t, q, 10)
	now := util.NowUTC()

	tenant, phone := "e2e-rej-tenant", "+15550008888"
	seedTenantOptedIn(t, db, tenant, phone)
	seedSuppressed(t, db, tenant, phone)

	code, resp := postSMS(t, h, map[string]any{
		"tenantId": tenant, "idempotencyKey": "e2e-rej-idem", "to": phone,
		"templateId": "tpl", "vars": map[string]string{"n": "1"},
	})
	if code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", code)
	}
	if resp["state"] != "suppressed" {
		t.Errorf("state = %q, want suppressed", resp["state"])
	}
	if n := q.total(); n != 0 {
		t.Errorf("suppressed send was enqueued %d times, want 0", n)
	}
	if n := capCount(t, db, tenant, phone, now); n != 0 {
		t.Errorf("suppressed send consumed %d of the daily cap, want 0", n)
	}
}

// TestAPI_ConcurrentDuplicatesEnqueueOnce is the same idempotency guarantee
// under the condition that actually breaks it: sixteen identical requests in
// flight at once, where the old path's plain INSERT raced the preceding SELECT.
func TestAPI_ConcurrentDuplicatesEnqueueOnce(t *testing.T) {
	q := newRecordingQueue()
	h, db := newAPI(t, q, 100)

	tenant, phone := "e2e-conc-tenant", "+15550007777"
	seedTenantOptedIn(t, db, tenant, phone)

	body := map[string]any{
		"tenantId": tenant, "idempotencyKey": "e2e-conc-idem", "to": phone,
		"templateId": "tpl", "vars": map[string]string{"n": "1"},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]int{}
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			code, resp := postSMS(t, h, body)
			mu.Lock()
			defer mu.Unlock()
			if code == http.StatusAccepted {
				seen[resp["messageId"]]++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(seen) != 1 {
		t.Errorf("callers resolved to %d different message ids, want 1: %v", len(seen), seen)
	}
	if n := q.total(); n != 1 {
		t.Errorf("%d enqueues for one idempotency key, want 1", n)
	}
	if n := countMessages(t, db, tenant, "e2e-conc-idem"); n != 1 {
		t.Errorf("concurrent duplicates produced %d rows, want 1", n)
	}
	if n := capCount(t, db, tenant, phone, util.NowUTC()); n != 1 {
		t.Errorf("concurrent duplicates consumed %d of the daily cap, want 1", n)
	}
}
