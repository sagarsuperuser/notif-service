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

// Retry decision for transient errors
func ShouldRetry(err error, httpStatus int) bool {
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return true
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return true
		}
		return false
	}
	if httpStatus == 429 || httpStatus == 408 {
		return true
	}
	if httpStatus >= 500 && httpStatus <= 599 {
		return true
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
