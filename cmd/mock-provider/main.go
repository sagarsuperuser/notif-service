package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/kelseyhightower/envconfig"
)

type config struct {
	AccountSID          string  `envconfig:"TWILIO_ACCOUNT_SID" default:"mock_sid"`
	AuthToken           string  `envconfig:"TWILIO_AUTH_TOKEN" default:"mock_token"`
	Port                string  `envconfig:"PORT" default:"8080"`
	OutcomeMode         string  `envconfig:"MOCK_OUTCOME_MODE" default:"fixed"`
	OutcomesRaw         string  `envconfig:"MOCK_OUTCOMES" default:"ok"`
	SuccessRate         float64 `envconfig:"MOCK_SUCCESS_RATE" default:"0.95"`
	FailureTypesRaw     string  `envconfig:"MOCK_FAILURE_TYPES" default:"failed"`
	FailureWeightsRaw   string  `envconfig:"MOCK_FAILURE_WEIGHTS" default:""`
	DelayMs             int     `envconfig:"MOCK_DELAY_MS" default:"0"`
	TimeoutDelayMs      int     `envconfig:"MOCK_TIMEOUT_DELAY_MS" default:"12000"`
	DefaultWebhookURL   string  `envconfig:"MOCK_WEBHOOK_URL" default:""`
	WebhookDelayMs      int     `envconfig:"MOCK_WEBHOOK_DELAY_MS" default:"500"`
	WebhookSentDelayMs  int     `envconfig:"MOCK_WEBHOOK_SENT_DELAY_MS" default:"300"`
	WebhookQueueDelayMs int     `envconfig:"MOCK_WEBHOOK_QUEUE_DELAY_MS" default:"0"`
	IncludeQueuedFirst  bool    `envconfig:"MOCK_WEBHOOK_INCLUDE_QUEUED" default:"true"`
	// Delay ranges (ms). If both min/max are >= 0, they take precedence over the fixed delay above.
	WebhookDelayMinMs      int `envconfig:"MOCK_WEBHOOK_DELAY_MS_MIN" default:"-1"`
	WebhookDelayMaxMs      int `envconfig:"MOCK_WEBHOOK_DELAY_MS_MAX" default:"-1"`
	WebhookSentDelayMinMs  int `envconfig:"MOCK_WEBHOOK_SENT_DELAY_MS_MIN" default:"-1"`
	WebhookSentDelayMaxMs  int `envconfig:"MOCK_WEBHOOK_SENT_DELAY_MS_MAX" default:"-1"`
	WebhookQueueDelayMinMs int `envconfig:"MOCK_WEBHOOK_QUEUE_DELAY_MS_MIN" default:"-1"`
	WebhookQueueDelayMaxMs int `envconfig:"MOCK_WEBHOOK_QUEUE_DELAY_MS_MAX" default:"-1"`

	// Webhook retry knobs. Retries happen on non-2xx responses and on retryable errors.
	WebhookMaxRetries      int `envconfig:"MOCK_WEBHOOK_MAX_RETRIES" default:"8"`
	WebhookRetryBaseMs     int `envconfig:"MOCK_WEBHOOK_RETRY_BASE_MS" default:"250"`
	WebhookRetryMaxMs      int `envconfig:"MOCK_WEBHOOK_RETRY_MAX_MS" default:"10000"`
	WebhookRetryJitterPct  int `envconfig:"MOCK_WEBHOOK_RETRY_JITTER_PCT" default:"20"`

	Outcomes          []string
	FailureTypes      []string
	FailureWeights    []weightedOutcome
	Delay             time.Duration
	TimeoutDelay      time.Duration
	WebhookDelay      time.Duration
	WebhookSentDelay  time.Duration
	WebhookQueueDelay time.Duration

	WebhookDelayMin      time.Duration
	WebhookDelayMax      time.Duration
	WebhookSentDelayMin  time.Duration
	WebhookSentDelayMax  time.Duration
	WebhookQueueDelayMin time.Duration
	WebhookQueueDelayMax time.Duration

	WebhookRetryBase    time.Duration
	WebhookRetryMax     time.Duration
}

type sendResponse struct {
	Sid       string `json:"sid"`
	Status    string `json:"status"`
	ErrorCode *int   `json:"error_code"`
	Message   string `json:"message"`
}

type weightedOutcome struct {
	Kind   string
	Weight float64
}

type server struct {
	cfg    config
	idx    uint64
	rng    *rand.Rand
	rngMu  sync.Mutex
	client *http.Client
}

func main() {
	cfg := loadConfig()
	loggingInit()

	s := &server{
		cfg:    cfg,
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())),
		client: &http.Client{Timeout: 5 * time.Second},
	}

	router := mux.NewRouter()
	router.HandleFunc("/2010-04-01/Accounts/{AccountSid}/Messages.json", s.handleSend).Methods(http.MethodPost)

	slog.Info("mock provider listening", "port", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, loggingMiddleware(router)); err != nil {
		slog.Error("mock provider server failed", "err", err)
		os.Exit(1)
	}
}

func loggingInit() {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(h))
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("mock provider request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func loadConfig() config {
	var cfg config
	if err := envconfig.Process("", &cfg); err != nil {
		slog.Error("mock provider config load failed", "err", err)
		os.Exit(1)
	}
	cfg.OutcomeMode = strings.ToLower(cfg.OutcomeMode)
	cfg.Outcomes = parseCSV(cfg.OutcomesRaw)
	cfg.FailureTypes = parseCSV(cfg.FailureTypesRaw)
	cfg.FailureWeights = parseWeightedOutcomes(cfg.FailureWeightsRaw)
	cfg.Delay = time.Duration(cfg.DelayMs) * time.Millisecond
	cfg.TimeoutDelay = time.Duration(cfg.TimeoutDelayMs) * time.Millisecond
	cfg.DefaultWebhookURL = strings.TrimSpace(cfg.DefaultWebhookURL)
	cfg.WebhookDelay = time.Duration(cfg.WebhookDelayMs) * time.Millisecond
	cfg.WebhookSentDelay = time.Duration(cfg.WebhookSentDelayMs) * time.Millisecond
	cfg.WebhookQueueDelay = time.Duration(cfg.WebhookQueueDelayMs) * time.Millisecond

	cfg.WebhookDelayMin = time.Duration(cfg.WebhookDelayMinMs) * time.Millisecond
	cfg.WebhookDelayMax = time.Duration(cfg.WebhookDelayMaxMs) * time.Millisecond
	cfg.WebhookSentDelayMin = time.Duration(cfg.WebhookSentDelayMinMs) * time.Millisecond
	cfg.WebhookSentDelayMax = time.Duration(cfg.WebhookSentDelayMaxMs) * time.Millisecond
	cfg.WebhookQueueDelayMin = time.Duration(cfg.WebhookQueueDelayMinMs) * time.Millisecond
	cfg.WebhookQueueDelayMax = time.Duration(cfg.WebhookQueueDelayMaxMs) * time.Millisecond

	if cfg.WebhookMaxRetries < 0 {
		cfg.WebhookMaxRetries = 0
	}
	if cfg.WebhookRetryBaseMs <= 0 {
		cfg.WebhookRetryBaseMs = 250
	}
	if cfg.WebhookRetryMaxMs <= 0 {
		cfg.WebhookRetryMaxMs = 10000
	}
	if cfg.WebhookRetryJitterPct < 0 {
		cfg.WebhookRetryJitterPct = 0
	}
	cfg.WebhookRetryBase = time.Duration(cfg.WebhookRetryBaseMs) * time.Millisecond
	cfg.WebhookRetryMax = time.Duration(cfg.WebhookRetryMaxMs) * time.Millisecond

	// If a range is set, sanitize it.
	cfg.WebhookDelayMin, cfg.WebhookDelayMax = sanitizeRange(cfg.WebhookDelayMin, cfg.WebhookDelayMax)
	cfg.WebhookSentDelayMin, cfg.WebhookSentDelayMax = sanitizeRange(cfg.WebhookSentDelayMin, cfg.WebhookSentDelayMax)
	cfg.WebhookQueueDelayMin, cfg.WebhookQueueDelayMax = sanitizeRange(cfg.WebhookQueueDelayMin, cfg.WebhookQueueDelayMax)

	if len(cfg.Outcomes) == 0 {
		cfg.Outcomes = []string{"ok"}
	}
	if len(cfg.FailureTypes) == 0 {
		cfg.FailureTypes = []string{"failed"}
	}
	if len(cfg.FailureWeights) == 0 {
		cfg.FailureWeights = make([]weightedOutcome, 0, len(cfg.FailureTypes))
		for _, t := range cfg.FailureTypes {
			cfg.FailureWeights = append(cfg.FailureWeights, weightedOutcome{Kind: t, Weight: 1})
		}
	}
	return cfg
}

func sanitizeRange(min, max time.Duration) (time.Duration, time.Duration) {
	// Both must be set to activate range behavior.
	if min < 0 || max < 0 {
		return -1, -1
	}
	if max < min {
		return max, min
	}
	return min, max
}

func (s *server) handleSend(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if !s.checkBasicAuth(r) {
		s.maybeDelayResponse(r.Context(), start)
		writeError(w, http.StatusUnauthorized, 20003, "Authentication Error")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.maybeDelayResponse(r.Context(), start)
		writeError(w, http.StatusBadRequest, 21620, "Invalid form data")
		return
	}

	if r.Form.Get("To") == "" || r.Form.Get("Body") == "" {
		s.maybeDelayResponse(r.Context(), start)
		writeError(w, http.StatusBadRequest, 21602, "Missing required parameter")
		return
	}
	if r.Form.Get("MessagingServiceSid") == "" && r.Form.Get("From") == "" {
		s.maybeDelayResponse(r.Context(), start)
		writeError(w, http.StatusBadRequest, 21606, "From or MessagingServiceSid is required")
		return
	}

	if s.cfg.Delay > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(s.cfg.Delay):
		}
	}

	outcome := s.nextOutcome()
	finalStatus, errorCode, sendQueued, sendSent, httpStatus, callErr := classifyOutcome(outcome)

	if callErr != nil {
		if errors.Is(callErr, context.DeadlineExceeded) {
			time.Sleep(s.cfg.TimeoutDelay)
			writeError(w, http.StatusGatewayTimeout, 20429, "Request timed out")
			return
		}
		s.maybeDelayResponse(r.Context(), start)
		writeError(w, httpStatus, errorCode, callErr.Error())
		return
	}

	sid := fmtSID(atomic.AddUint64(&s.idx, 1) - 1)
	resp := sendResponse{Sid: sid, Status: "queued"}
	s.maybeDelayResponse(r.Context(), start)
	writeJSON(w, http.StatusCreated, resp)

	cb := r.Form.Get("StatusCallback")
	if cb == "" {
		cb = s.cfg.DefaultWebhookURL
	}
	s.maybeWebhookSequence(cb, sid, finalStatus, errorCode, sendQueued, sendSent)
}

func (s *server) maybeWebhookSequence(callbackURL, msgSid, finalStatus string, errorCode int, sendQueued, sendSent bool) {
	if callbackURL == "" {
		return
	}
	go func() {
		post := func(status string, code int) {
			form := url.Values{}
			form.Set("MessageSid", msgSid)
			form.Set("MessageStatus", status)
			if code != 0 {
				form.Set("ErrorCode", strconv.Itoa(code))
			} else {
				form.Set("ErrorCode", "")
			}

			sig := twilioSignature(s.cfg.AuthToken, callbackURL, form)
			_ = s.postWebhookWithRetry(context.Background(), callbackURL, sig, form)
		}

		if s.cfg.IncludeQueuedFirst && sendQueued {
			s.sleepMaybeRange(s.cfg.WebhookQueueDelayMin, s.cfg.WebhookQueueDelayMax, s.cfg.WebhookQueueDelay)
			post("queued", 0)
		}
		if sendSent {
			s.sleepMaybeRange(s.cfg.WebhookSentDelayMin, s.cfg.WebhookSentDelayMax, s.cfg.WebhookSentDelay)
			post("sent", 0)
		}
		s.sleepMaybeRange(s.cfg.WebhookDelayMin, s.cfg.WebhookDelayMax, s.cfg.WebhookDelay)
		post(finalStatus, errorCode)
	}()
}

func (s *server) sleepMaybeRange(min, max, fallback time.Duration) {
	d := fallback
	if min >= 0 && max >= 0 {
		d = s.randDuration(min, max)
	}
	if d <= 0 {
		return
	}
	time.Sleep(d)
}

func (s *server) randDuration(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	span := int64(max - min)
	s.rngMu.Lock()
	n := s.rng.Int63n(span + 1)
	s.rngMu.Unlock()
	return min + time.Duration(n)
}

func (s *server) postWebhookWithRetry(ctx context.Context, callbackURL, sig string, form url.Values) error {
	maxAttempts := s.cfg.WebhookMaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, callbackURL, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Twilio-Signature", sig)

		resp, err := s.client.Do(req)
		retryAfter := time.Duration(0)
		if resp != nil {
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}

		if err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			_ = resp.Body.Close()
			return nil
		}

		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}

		// No more attempts left.
		if attempt == maxAttempts-1 {
			if err != nil {
				slog.Error("mock webhook post failed", "url", callbackURL, "attempt", attempt+1, "err", err)
				return err
			}
			slog.Error("mock webhook post failed", "url", callbackURL, "attempt", attempt+1, "status", status)
			return fmt.Errorf("webhook post failed: status=%d", status)
		}

		// Retry only on retryable outcomes.
		if err == nil && !isRetryableStatus(status) {
			slog.Error("mock webhook post non-retryable", "url", callbackURL, "attempt", attempt+1, "status", status)
			return fmt.Errorf("webhook post non-retryable: status=%d", status)
		}

		wait := retryAfter
		if wait <= 0 {
			wait = s.retryBackoff(attempt)
		}
		slog.Warn("mock webhook post retrying", "url", callbackURL, "attempt", attempt+1, "status", status, "wait_ms", wait.Milliseconds())
		time.Sleep(wait)
	}

	return nil
}

func (s *server) retryBackoff(attempt int) time.Duration {
	base := s.cfg.WebhookRetryBase
	max := s.cfg.WebhookRetryMax
	if base <= 0 {
		base = 250 * time.Millisecond
	}
	if max <= 0 {
		max = 10 * time.Second
	}

	// Exponential: base * 2^attempt, capped.
	wait := base * time.Duration(1<<attempt)
	if wait > max {
		wait = max
	}

	jp := s.cfg.WebhookRetryJitterPct
	if jp <= 0 {
		return wait
	}
	if jp > 100 {
		jp = 100
	}

	// Jitter: +/- jp%.
	delta := int64(wait) * int64(jp) / 100
	if delta <= 0 {
		return wait
	}
	s.rngMu.Lock()
	j := s.rng.Int63n(2*delta+1) - delta
	s.rngMu.Unlock()
	return time.Duration(int64(wait) + j)
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	// Handle seconds form.
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	// Ignore HTTP-date for now (not needed for our mock/webhook path).
	return 0
}

func (s *server) checkBasicAuth(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return user == s.cfg.AccountSID && pass == s.cfg.AuthToken
}

func (s *server) nextOutcome() string {
	switch s.cfg.OutcomeMode {
	case "round_robin":
		idx := atomic.AddUint64(&s.idx, 1) - 1
		return s.cfg.Outcomes[int(idx)%len(s.cfg.Outcomes)]
	case "weighted":
		s.rngMu.Lock()
		ok := s.rng.Float64() <= s.cfg.SuccessRate
		r := s.rng.Float64()
		s.rngMu.Unlock()
		if ok {
			return "ok"
		}
		return pickWeighted(r, s.cfg.FailureWeights)
	case "random":
		s.rngMu.Lock()
		i := s.rng.Intn(len(s.cfg.Outcomes))
		s.rngMu.Unlock()
		return s.cfg.Outcomes[i]
	default:
		return s.cfg.Outcomes[0]
	}
}

func (s *server) maybeDelayResponse(ctx context.Context, start time.Time) {
	const (
		min = 100 * time.Millisecond
		max = 500 * time.Millisecond
	)

	elapsed := time.Since(start)
	if elapsed >= min {
		return
	}

	// Pick a target total latency in [min, max] and sleep the remaining time.
	s.rngMu.Lock()
	target := min + time.Duration(s.rng.Int63n(int64(max-min)+1))
	s.rngMu.Unlock()

	remain := target - elapsed
	if remain <= 0 {
		return
	}

	t := time.NewTimer(remain)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return
	case <-t.C:
		return
	}
}

func classifyOutcome(raw string) (finalStatus string, errorCode int, sendQueued, sendSent bool, httpStatus int, callErr error) {
	token := strings.TrimSpace(raw)
	if token == "" {
		token = "ok"
	}
	parts := strings.Split(token, ":")
	kind := parts[0]
	if len(parts) > 1 {
		if v, err := strconv.Atoi(parts[1]); err == nil {
			errorCode = v
		}
	}

	switch kind {
	case "ok", "success":
		return "delivered", 0, true, true, http.StatusCreated, nil
	case "undelivered":
		if errorCode == 0 {
			errorCode = 30003
		}
		return "undelivered", errorCode, true, true, http.StatusCreated, nil
	case "failed":
		if errorCode == 0 {
			errorCode = 30008
		}
		return "failed", errorCode, true, false, http.StatusCreated, nil
	case "rate_limit", "429":
		if errorCode == 0 {
			errorCode = 20429
		}
		return "", errorCode, false, false, http.StatusTooManyRequests, errors.New("rate limited")
	case "bad_request", "400":
		if errorCode == 0 {
			errorCode = 21606
		}
		return "", errorCode, false, false, http.StatusBadRequest, errors.New("bad request")
	case "server_error", "500":
		if errorCode == 0 {
			errorCode = 20500
		}
		return "", errorCode, false, false, http.StatusInternalServerError, errors.New("server error")
	case "timeout":
		if errorCode == 0 {
			errorCode = 20429
		}
		return "", errorCode, false, false, http.StatusGatewayTimeout, context.DeadlineExceeded
	default:
		if errorCode == 0 {
			errorCode = 30008
		}
		return "", errorCode, false, false, http.StatusInternalServerError, errors.New("mock error: " + kind)
	}
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
	mac.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func writeError(w http.ResponseWriter, status int, code int, msg string) {
	resp := sendResponse{Status: "failed", Message: msg}
	if code != 0 {
		resp.ErrorCode = &code
	}
	writeJSON(w, status, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)

}

func fmtSID(i uint64) string {
	return "SM" + fmt.Sprintf("%06d", i)
}

func parseCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return []string{"ok"}
	}
	return out
}

func parseWeightedOutcomes(s string) []weightedOutcome {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]weightedOutcome, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		kv := strings.Split(p, ":")
		if len(kv) != 2 {
			continue
		}
		w, err := strconv.ParseFloat(strings.TrimSpace(kv[1]), 64)
		if err != nil || w <= 0 {
			continue
		}
		kind := strings.TrimSpace(kv[0])
		if kind == "" {
			continue
		}
		out = append(out, weightedOutcome{Kind: kind, Weight: w})
	}
	return out
}

func pickWeighted(r float64, items []weightedOutcome) string {
	if len(items) == 0 {
		return "failed"
	}
	var total float64
	for _, it := range items {
		total += it.Weight
	}
	if total <= 0 {
		return items[0].Kind
	}
	target := r * total
	var cumulative float64
	for _, it := range items {
		cumulative += it.Weight
		if target <= cumulative {
			return it.Kind
		}
	}
	return items[len(items)-1].Kind
}
