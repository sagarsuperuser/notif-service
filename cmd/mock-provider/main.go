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
	AccountSID          string `envconfig:"TWILIO_ACCOUNT_SID" default:"mock_sid"`
	AuthToken           string `envconfig:"TWILIO_AUTH_TOKEN" default:"mock_token"`
	Port                string `envconfig:"PORT" default:"8080"`
	OutcomeMode         string `envconfig:"MOCK_OUTCOME_MODE" default:"fixed"`
	OutcomesRaw         string `envconfig:"MOCK_OUTCOMES" default:"ok"`
	SuccessRate         float64 `envconfig:"MOCK_SUCCESS_RATE" default:"0.95"`
	FailureTypesRaw     string  `envconfig:"MOCK_FAILURE_TYPES" default:"failed"`
	FailureWeightsRaw   string  `envconfig:"MOCK_FAILURE_WEIGHTS" default:""`
	DelayMs             int    `envconfig:"MOCK_DELAY_MS" default:"0"`
	TimeoutDelayMs      int    `envconfig:"MOCK_TIMEOUT_DELAY_MS" default:"12000"`
	TimeoutMaxPerSec    int    `envconfig:"MOCK_TIMEOUT_MAX_PER_SEC" default:"2"`
	DefaultWebhookURL   string `envconfig:"MOCK_WEBHOOK_URL" default:""`
	WebhookDelayMs      int    `envconfig:"MOCK_WEBHOOK_DELAY_MS" default:"500"`
	WebhookSentDelayMs  int    `envconfig:"MOCK_WEBHOOK_SENT_DELAY_MS" default:"300"`
	WebhookQueueDelayMs int    `envconfig:"MOCK_WEBHOOK_QUEUE_DELAY_MS" default:"0"`
	IncludeQueuedFirst  bool   `envconfig:"MOCK_WEBHOOK_INCLUDE_QUEUED" default:"true"`

	Outcomes          []string
	FailureTypes      []string
	FailureWeights    []weightedOutcome
	Delay             time.Duration
	TimeoutDelay      time.Duration
	WebhookDelay      time.Duration
	WebhookSentDelay  time.Duration
	WebhookQueueDelay time.Duration
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
	client *http.Client
	// rate limit for timeout outcomes
	timeoutMu          sync.Mutex
	timeoutWindowStart time.Time
	timeoutCount       int
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

func (s *server) handleSend(w http.ResponseWriter, r *http.Request) {
	if !s.checkBasicAuth(r) {
		writeError(w, http.StatusUnauthorized, 20003, "Authentication Error")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, 21620, "Invalid form data")
		return
	}

	if r.Form.Get("To") == "" || r.Form.Get("Body") == "" {
		writeError(w, http.StatusBadRequest, 21602, "Missing required parameter")
		return
	}
	if r.Form.Get("MessagingServiceSid") == "" && r.Form.Get("From") == "" {
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
	if outcome == "timeout" && !s.allowTimeout() {
		outcome = "ok"
	}
	finalStatus, errorCode, sendQueued, sendSent, httpStatus, callErr := classifyOutcome(outcome)

	if callErr != nil {
		if errors.Is(callErr, context.DeadlineExceeded) {
			time.Sleep(s.cfg.TimeoutDelay)
			writeError(w, http.StatusGatewayTimeout, 20429, "Request timed out")
			return
		}
		writeError(w, httpStatus, errorCode, callErr.Error())
		return
	}

	sid := fmtSID(atomic.AddUint64(&s.idx, 1) - 1)
	resp := sendResponse{Sid: sid, Status: "queued"}
	writeJSON(w, http.StatusCreated, resp)

	cb := r.Form.Get("StatusCallback")
	if cb == "" {
		cb = s.cfg.DefaultWebhookURL
	}
	s.maybeWebhookSequence(cb, sid, finalStatus, errorCode, sendQueued, sendSent)
}

func (s *server) allowTimeout() bool {
	if s.cfg.TimeoutMaxPerSec <= 0 {
		return false
	}
	s.timeoutMu.Lock()
	defer s.timeoutMu.Unlock()
	now := time.Now().Truncate(time.Minute)
	if s.timeoutWindowStart.IsZero() || !s.timeoutWindowStart.Equal(now) {
		s.timeoutWindowStart = now
		s.timeoutCount = 0
	}
	if s.timeoutCount >= s.cfg.TimeoutMaxPerSec {
		return false
	}
	s.timeoutCount++
	return true
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
			req, _ := http.NewRequest(http.MethodPost, callbackURL, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-Twilio-Signature", sig)
			_, _ = s.client.Do(req)
		}

		if s.cfg.IncludeQueuedFirst && sendQueued {
			if s.cfg.WebhookQueueDelay > 0 {
				time.Sleep(s.cfg.WebhookQueueDelay)
			}
			post("queued", 0)
		}
		if sendSent {
			if s.cfg.WebhookSentDelay > 0 {
				time.Sleep(s.cfg.WebhookSentDelay)
			}
			post("sent", 0)
		}
		if s.cfg.WebhookDelay > 0 {
			time.Sleep(s.cfg.WebhookDelay)
		}
		post(finalStatus, errorCode)
	}()
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
		if s.rng.Float64() <= s.cfg.SuccessRate {
			return "ok"
		}
		return pickWeighted(s.rng.Float64(), s.cfg.FailureWeights)
	case "random":
		return s.cfg.Outcomes[s.rng.Intn(len(s.cfg.Outcomes))]
	default:
		return s.cfg.Outcomes[0]
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
