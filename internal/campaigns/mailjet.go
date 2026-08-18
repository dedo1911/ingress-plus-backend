package campaigns

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	mailjetSendURL    = "https://api.mailjet.com/v3.1/send"
	mailjetBatchSize  = 50
	mailjetMaxRetries = 3
	mailjetRetryDelay = 2 * time.Second
)

// MailjetRecipient is one recipient's already-personalized message, built by
// the caller via RenderPlaceholders before it reaches MailjetClient - this
// package's Mailjet integration doesn't know about %username%/%faction%/
// %userEmail% or campaign records at all.
type MailjetRecipient struct {
	Email   string
	Name    string
	Subject string
	HTML    string
}

type mailjetAddress struct {
	Email string `json:"Email"`
	Name  string `json:"Name,omitempty"`
}

type mailjetMessage struct {
	From     mailjetAddress   `json:"From"`
	To       []mailjetAddress `json:"To"`
	Subject  string           `json:"Subject"`
	HTMLPart string           `json:"HTMLPart"`
}

type mailjetSendRequest struct {
	Messages []mailjetMessage `json:"Messages"`
}

type mailjetMessageResult struct {
	Status string `json:"Status"`
	Errors []struct {
		ErrorMessage string `json:"ErrorMessage"`
	} `json:"Errors"`
}

type mailjetSendResponse struct {
	Messages []mailjetMessageResult `json:"Messages"`
}

// MailjetClient sends campaign emails via Mailjet's bulk Send API (v3.1)
// instead of SMTP, batching up to 50 personalized messages per HTTP call.
// Mailjet's own limits (per their docs: 50 messages/request, ~500
// requests/10s) make this comfortably fast for a 1000+-recipient campaign -
// roughly 20 requests instead of 1000 individual SMTP connections, which
// also shrinks the window in which a backend crash could leave a campaign
// stuck mid-send (see the status-machine comments in guard.go/dispatch.go).
type MailjetClient struct {
	APIKey    string
	APISecret string

	// BaseURL overrides the real Mailjet endpoint; used by tests to point
	// at an httptest.Server instead.
	BaseURL string

	// HTTPClient is overridable for tests; defaults to http.DefaultClient.
	HTTPClient *http.Client

	// RetryDelay overrides mailjetRetryDelay; used by tests so retry
	// coverage doesn't have to actually wait several seconds.
	RetryDelay time.Duration
}

func (c *MailjetClient) retryDelay() time.Duration {
	if c.RetryDelay > 0 {
		return c.RetryDelay
	}
	return mailjetRetryDelay
}

func (c *MailjetClient) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return mailjetSendURL
}

func (c *MailjetClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// SendBatches sends the given recipients in batches of up to 50 and returns
// how many were confirmed sent by Mailjet. Every recipient not confirmed
// sent - whether Mailjet reported an error for that specific message, or an
// entire batch failed outright after retries - is reported once via
// onFailure with a human-readable reason, so the caller can log/count
// failures the same way the previous per-recipient SMTP loop did.
func (c *MailjetClient) SendBatches(fromEmail, fromName string, recipients []MailjetRecipient, onFailure func(recipient MailjetRecipient, reason string)) int {
	from := mailjetAddress{Email: fromEmail, Name: fromName}

	sent := 0
	for start := 0; start < len(recipients); start += mailjetBatchSize {
		end := min(start+mailjetBatchSize, len(recipients))
		batch := recipients[start:end]

		results, err := c.sendBatch(from, batch)
		if err != nil {
			for _, r := range batch {
				onFailure(r, err.Error())
			}
			continue
		}

		for i, r := range batch {
			if i >= len(results) {
				onFailure(r, "Mailjet response was missing a result for this recipient")
				continue
			}
			if results[i].Status == "success" {
				sent++
				continue
			}
			reason := "Mailjet reported the send as failed"
			if len(results[i].Errors) > 0 {
				reason = results[i].Errors[0].ErrorMessage
			}
			onFailure(r, reason)
		}
	}

	return sent
}

// sendBatch sends a single request (<=50 messages), retrying transient
// failures (network errors and 429 rate limits) with a fixed delay between
// attempts, per Mailjet's own guidance to "properly take into account any
// 429 error (wait and retry)". Anything else - a genuine 4xx/5xx from
// Mailjet, or a malformed response body - is returned immediately rather
// than retried, since those won't resolve themselves.
func (c *MailjetClient) sendBatch(from mailjetAddress, batch []MailjetRecipient) ([]mailjetMessageResult, error) {
	messages := make([]mailjetMessage, len(batch))
	for i, r := range batch {
		messages[i] = mailjetMessage{
			From:     from,
			To:       []mailjetAddress{{Email: r.Email, Name: r.Name}},
			Subject:  r.Subject,
			HTMLPart: r.HTML,
		}
	}

	body, err := json.Marshal(mailjetSendRequest{Messages: messages})
	if err != nil {
		return nil, fmt.Errorf("failed to encode Mailjet request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= mailjetMaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(c.retryDelay())
		}

		results, retryable, sendErr := c.attemptSendBatch(body)
		if sendErr == nil {
			return results, nil
		}
		if !retryable {
			return nil, sendErr
		}
		lastErr = sendErr
	}

	return nil, fmt.Errorf("mailjet send failed after %d attempts: %w", mailjetMaxRetries+1, lastErr)
}

// attemptSendBatch makes a single HTTP attempt. retryable is true for
// network errors and HTTP 429 (rate limited).
func (c *MailjetClient) attemptSendBatch(body []byte) (results []mailjetMessageResult, retryable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.baseURL(), bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("failed to build Mailjet request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.APIKey, c.APISecret)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("mailjet request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, true, fmt.Errorf("rate limited (429)")
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read Mailjet response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, false, fmt.Errorf("mailjet returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed mailjetSendResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, false, fmt.Errorf("failed to decode Mailjet response: %w", err)
	}

	return parsed.Messages, false, nil
}
