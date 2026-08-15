package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	AccountSID string
	AuthToken  string
	HTTP       *http.Client

	MessagingServiceSID string
	FromNumber          string
	BaseURL             string
}

type SendRequest struct {
	To                string
	Body              string
	StatusCallbackURL string
}

type SendResponse struct {
	Sid       string `json:"sid"`
	Status    string `json:"status"`
	ErrorCode *int   `json:"error_code"`
	Message   string `json:"message"`
}

func (c *Client) SendSMS(ctx context.Context, req SendRequest) (SendResponse, int, []byte, error) {
	form := url.Values{}
	form.Set("To", req.To)
	form.Set("Body", req.Body)
	if req.StatusCallbackURL != "" {
		form.Set("StatusCallback", req.StatusCallbackURL)
	}
	if c.MessagingServiceSID != "" {
		form.Set("MessagingServiceSid", c.MessagingServiceSID)
	} else {
		form.Set("From", c.FromNumber)
	}

	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.twilio.com"
	}
	endpoint := baseURL + "/2010-04-01/Accounts/" + c.AccountSID + "/Messages.json"
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.SetBasicAuth(c.AccountSID, c.AuthToken)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return SendResponse{}, 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	var out SendResponse
	_ = json.Unmarshal(b, &out)

	// Twilio returns 201 for created; treat 2xx as success
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if out.Message != "" {
			return out, resp.StatusCode, b, errors.New(out.Message)
		}
		return out, resp.StatusCode, b, errors.New("twilio send failed")
	}
	return out, resp.StatusCode, b, nil
}

// ShouldRetry classifies a failed send as transient or permanent.
//
// The status is checked FIRST, and this ordering is the whole point. SendSMS
// returns a non-nil error alongside every non-2xx response, so an earlier
// version that tested `err != nil` first returned false for every provider
// response and the status branches below were unreachable. A 429 — which the
// provider documents as "not processed and safe to retry after backing off" —
// was classified permanent, and the worker wrote the message terminally failed
// on its first attempt. Because a terminal row cannot be re-claimed, the
// redelivered queue message was then acknowledged and deleted, so the send was
// abandoned without ever reaching the dead-letter queue.
//
// A status of zero means the request never got a response at all (SendSMS
// returns 0 on transport failure), which is the only case where the error value
// carries the information.
func ShouldRetry(err error, httpStatus int) bool {
	if httpStatus > 0 {
		// The provider answered. Its status decides, whatever error accompanies it.
		switch {
		case httpStatus == 429, httpStatus == 408:
			return true // throttled or request timeout: back off and retry
		case httpStatus >= 500 && httpStatus <= 599:
			return true // provider-side fault
		default:
			return false // 4xx other than the above is our fault and will not improve
		}
	}

	// No response: transport failure. Only timeouts are worth retrying; a
	// refused connection or DNS failure will not fix itself within the budget.
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return true
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return true
		}
	}
	return false
}

func Backoff(attempt int) time.Duration {
	// 200ms, 600ms, 1400ms approx (with small jitter)
	base := []time.Duration{200 * time.Millisecond, 600 * time.Millisecond, 1400 * time.Millisecond}
	if attempt <= 0 {
		return base[0]
	}
	if attempt >= len(base) {
		return base[len(base)-1]
	}
	return base[attempt]
}
