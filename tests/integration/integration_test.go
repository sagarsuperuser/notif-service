//go:build integration
// +build integration

package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"notif/internal/domain"
	"notif/internal/providers/twilio"
	sqsqueue "notif/internal/queue/sqs"
	"notif/internal/service"
	"notif/internal/store/pg"
	workerproc "notif/internal/worker"
)

type noopQueue struct{}

func (noopQueue) EnqueueSMS(ctx context.Context, tenantID, messageID, idempotencyKey, to, templateID string, vars map[string]string, campaignID string) error {
	return nil
}

func TestConsentOptedOutSuppressed(t *testing.T) {
	ctx := context.Background()
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := pg.New(db)

	tenantID := "t1"
	phone := "+15551234567"

	// ensure tenant exists
	insertTenant(t, db, tenantID)

	_, err := db.Exec(ctx, `
		INSERT INTO consents (tenant_id, phone, channel, status)
		VALUES ($1, $2, 'sms', 'opted_out')
	`, tenantID, phone)
	if err != nil {
		t.Fatalf("insert consent: %v", err)
	}

	svc := &service.NotificationService{
		Store:     store,
		Queue:     noopQueue{},
		MaxPerDay: 10,
	}

	resp, err := svc.CreateAndEnqueueSMS(ctx, domain.SendSMSRequest{
		TenantID:       tenantID,
		IdempotencyKey: "idem-1",
		To:             phone,
		TemplateID:     "tpl-1",
		Vars:           map[string]string{"name": "a"},
	}, "msg-1", time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.State != string(domain.StateSuppressed) {
		t.Fatalf("expected suppressed, got %s", resp.State)
	}

	// assert via DB
	assertMessageStateDB(t, db, "msg-1", string(domain.StateSuppressed))
}

func TestCapExceededSuppressed(t *testing.T) {
	ctx := context.Background()
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := pg.New(db)

	tenantID := "t2"
	phone := "+15557654321"

	seedTenantOptedIn(t, db, tenantID, phone)

	// day column is DATE -> use CURRENT_DATE from Postgres
	_, err := db.Exec(ctx, `
		INSERT INTO send_caps_daily (tenant_id, phone, day, count)
		VALUES ($1, $2, CURRENT_DATE, 1)
	`, tenantID, phone)
	if err != nil {
		t.Fatalf("insert cap: %v", err)
	}

	svc := &service.NotificationService{
		Store:     store,
		Queue:     noopQueue{},
		MaxPerDay: 1,
	}

	resp, err := svc.CreateAndEnqueueSMS(ctx, domain.SendSMSRequest{
		TenantID:       tenantID,
		IdempotencyKey: "idem-2",
		To:             phone,
		TemplateID:     "tpl-2",
		Vars:           map[string]string{"name": "b"},
	}, "msg-2", time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.State != string(domain.StateSuppressed) {
		t.Fatalf("expected suppressed, got %s", resp.State)
	}

	assertMessageStateDB(t, db, "msg-2", string(domain.StateSuppressed))
}

func TestHappyPathQueuedSubmittedDelivered(t *testing.T) {
	ctx := context.Background()
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := pg.New(db)

	tenantID := "t3"
	phone := "+15550001111"

	seedTenantOptedIn(t, db, tenantID, phone)

	svc := &service.NotificationService{
		Store:     store,
		Queue:     noopQueue{},
		MaxPerDay: 10,
	}

	resp, err := svc.CreateAndEnqueueSMS(ctx, domain.SendSMSRequest{
		TenantID:       tenantID,
		IdempotencyKey: "idem-3",
		To:             phone,
		TemplateID:     "tpl-3",
		Vars:           map[string]string{"name": "c"},
	}, "msg-3", time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.State != string(domain.StateQueued) {
		t.Fatalf("expected queued, got %s", resp.State)
	}

	assertMessageStateDB(t, db, "msg-3", string(domain.StateQueued))

	if err := store.SetProviderDetails(ctx, "msg-3", "twilio", "SM123", "submitted", time.Now()); err != nil {
		t.Fatalf("set provider details: %v", err)
	}
	assertMessageStateDB(t, db, "msg-3", string(domain.StateSubmitted))

	// Webhook simulation with real signature verification
	authToken := "testtoken"
	publicURL := "https://example.com/v1/webhooks/twilio/status"
	form := url.Values{
		"MessageSid":    []string{"SM123"},
		"MessageStatus": []string{"delivered"},
		"ErrorCode":     []string{""},
	}
	sig := twilioSignature(authToken, publicURL, form)

	req := httptest.NewRequest(http.MethodPost, publicURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", sig)

	rr := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", 400)
			return
		}

		gotSig := r.Header.Get("X-Twilio-Signature")
		if !twilio.VerifySignature(authToken, publicURL, gotSig, r.PostForm) {
			http.Error(w, "invalid signature", 401)
			return
		}

		msgSid := r.PostForm.Get("MessageSid")
		status := r.PostForm.Get("MessageStatus")
		errCode := r.PostForm.Get("ErrorCode")

		_ = store.InsertDeliveryEvent(r.Context(), "twilio", msgSid, status, errCode, r.PostForm, nil)

		newState := ""
		switch status {
		case "delivered":
			newState = "delivered"
		case "failed", "undelivered":
			newState = "failed"
		default:
			w.WriteHeader(200)
			return
		}

		_, _ = store.UpdateMessageByProviderMsgID(r.Context(), "twilio", msgSid, newState, errCode, time.Now())
		w.WriteHeader(200)
	})

	handler.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	assertMessageStateDB(t, db, "msg-3", string(domain.StateDelivered))
}

func TestWorkerQueuedToSubmitted(t *testing.T) {
	ctx := context.Background()
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := pg.New(db)

	tenantID := "t4"
	phone := "+15550002222"
	seedTenantOptedIn(t, db, tenantID, phone)

	msgID := "msg-4"
	templateID := "tpl-worker"
	varsJSON := `{"name":"d","ref":"R1"}`

	_, err := db.Exec(ctx, `
		INSERT INTO messages (id, tenant_id, idempotency_key, to_phone, template_id, vars_json, campaign_id, state, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now(),now())
	`, msgID, tenantID, "idem-4", phone, templateID, varsJSON, nil, "queued")
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	p := &workerproc.Processor{
		Store:     store,
		Sender:    fakeTwilioSender{sid: "SM999"},
		Templates: map[string]string{templateID: "Hi {name}, ref {ref}"},
	}

	err = p.Process(ctx, sqsqueue.SMSJob{MessageID: msgID})
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	assertMessageStateDB(t, db, msgID, "submitted")
}

type fakeTwilioSender struct {
	sid string
}

func (f fakeTwilioSender) SendSMS(ctx context.Context, req twilio.SendRequest) (twilio.SendResponse, int, []byte, error) {
	return twilio.SendResponse{Sid: f.sid, Status: "queued"}, 201, []byte(`{"sid":"` + f.sid + `"}`), nil
}

func insertTenant(t *testing.T, db *pgxpool.Pool, tenantID string) {
	t.Helper()
	_, err := db.Exec(context.Background(), `
		INSERT INTO tenants (id, name) VALUES ($1, $2)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, tenantID)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
}

func seedTenantOptedIn(t *testing.T, db *pgxpool.Pool, tenantID, phone string) {
	t.Helper()
	insertTenant(t, db, tenantID)
	_, err := db.Exec(context.Background(), `
		INSERT INTO consents (tenant_id, phone, channel, status)
		VALUES ($1, $2, 'sms', 'opted_in')
	`, tenantID, phone)
	if err != nil {
		t.Fatalf("insert consent: %v", err)
	}
}

func assertMessageStateDB(t *testing.T, db *pgxpool.Pool, msgID, want string) {
	t.Helper()
	var got string
	err := db.QueryRow(context.Background(), `SELECT state FROM messages WHERE id=$1`, msgID).Scan(&got)
	if err != nil {
		t.Fatalf("select state: %v", err)
	}
	if got != want {
		t.Fatalf("expected state %s, got %s", want, got)
	}
}

func setupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("DB_DSN")
	}
	if dsn == "" {
		t.Skip("TEST_DB_DSN or DB_DSN not set")
	}

	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect admin db: %v", err)
	}

	_, err = admin.Exec(context.Background(), "CREATE SCHEMA "+schema)
	if err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	dbDSN, err := withSearchPath(dsn, schema)
	if err != nil {
		admin.Close()
		t.Fatalf("build dsn: %v", err)
	}

	db, err := pgxpool.New(context.Background(), dbDSN)
	if err != nil {
		admin.Close()
		t.Fatalf("connect test db: %v", err)
	}

	sqlPath := filepath.Join("..", "..", "migrations", "001_init.sql")
	sqlBytes, err := os.ReadFile(sqlPath)
	if err != nil {
		db.Close()
		admin.Close()
		t.Fatalf("read migrations: %v", err)
	}

	if _, err := db.Exec(context.Background(), string(sqlBytes)); err != nil {
		db.Close()
		admin.Close()
		t.Fatalf("run migrations: %v", err)
	}

	cleanup := func() {
		db.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	}

	return db, cleanup
}

func withSearchPath(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	q := u.Query()
	opts := q.Get("options")
	if opts != "" {
		opts = opts + " -c search_path=" + schema
	} else {
		opts = "-c search_path=" + schema
	}
	q.Set("options", opts)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func twilioSignature(authToken, fullURL string, form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(fullURL)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(form.Get(k))
	}

	mac := hmac.New(sha1.New, []byte(authToken))
	_, _ = mac.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// TODO: add worker-level integration test for queued -> submitted using a fake Twilio sender.
