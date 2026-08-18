package campaigns_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dedo1911/ingress-plus-backend/internal/campaigns"
)

func makeRecipients(n int) []campaigns.MailjetRecipient {
	recipients := make([]campaigns.MailjetRecipient, n)
	for i := range recipients {
		recipients[i] = campaigns.MailjetRecipient{
			Email:   fmt.Sprintf("agent%d@example.com", i),
			Name:    fmt.Sprintf("Agent%d", i),
			Subject: "Hello",
			HTML:    "<p>Hi</p>",
		}
	}
	return recipients
}

// mailjetMessagesIn is the minimal shape needed to inspect what a test
// server received, mirroring the wire format documented for Mailjet's
// v3.1 Send API.
type mailjetMessagesIn struct {
	Messages []struct {
		To []struct {
			Email string `json:"Email"`
		} `json:"To"`
	} `json:"Messages"`
}

func TestSendBatches_SplitsIntoBatchesOf50(t *testing.T) {
	var mu atomic.Int32
	var batchSizes []int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Add(1)
		var in mailjetMessagesIn
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		batchSizes = append(batchSizes, len(in.Messages))

		results := make([]map[string]string, len(in.Messages))
		for i := range results {
			results[i] = map[string]string{"Status": "success"}
		}
		json.NewEncoder(w).Encode(map[string]any{"Messages": results})
	}))
	defer server.Close()

	client := &campaigns.MailjetClient{APIKey: "key", APISecret: "secret", BaseURL: server.URL}
	recipients := makeRecipients(120) // 3 batches: 50, 50, 20

	var failures int
	sent := client.SendBatches("from@example.com", "Sender", recipients, func(campaigns.MailjetRecipient, string) {
		failures++
	})

	if sent != 120 {
		t.Errorf("sent = %d, want 120", sent)
	}
	if failures != 0 {
		t.Errorf("failures = %d, want 0", failures)
	}
	if mu.Load() != 3 {
		t.Errorf("made %d requests, want 3", mu.Load())
	}
	want := []int{50, 50, 20}
	for i, size := range batchSizes {
		if size != want[i] {
			t.Errorf("batch %d size = %d, want %d", i, size, want[i])
		}
	}
}

func TestSendBatches_SetsBasicAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "the-key" || pass != "the-secret" {
			t.Errorf("BasicAuth() = (%q, %q, %v), want (\"the-key\", \"the-secret\", true)", user, pass, ok)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"Messages": []map[string]string{{"Status": "success"}},
		})
	}))
	defer server.Close()

	client := &campaigns.MailjetClient{APIKey: "the-key", APISecret: "the-secret", BaseURL: server.URL}
	client.SendBatches("from@example.com", "Sender", makeRecipients(1), func(campaigns.MailjetRecipient, string) {})
}

func TestSendBatches_ReportsPerRecipientFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"Messages": []map[string]any{
				{"Status": "success"},
				{"Status": "error", "Errors": []map[string]string{{"ErrorMessage": "invalid recipient"}}},
				{"Status": "success"},
			},
		})
	}))
	defer server.Close()

	client := &campaigns.MailjetClient{APIKey: "key", APISecret: "secret", BaseURL: server.URL}
	recipients := makeRecipients(3)

	var failedEmails []string
	var failedReasons []string
	sent := client.SendBatches("from@example.com", "Sender", recipients, func(r campaigns.MailjetRecipient, reason string) {
		failedEmails = append(failedEmails, r.Email)
		failedReasons = append(failedReasons, reason)
	})

	if sent != 2 {
		t.Errorf("sent = %d, want 2", sent)
	}
	if len(failedEmails) != 1 || failedEmails[0] != recipients[1].Email {
		t.Errorf("failedEmails = %v, want [%s]", failedEmails, recipients[1].Email)
	}
	if len(failedReasons) != 1 || failedReasons[0] != "invalid recipient" {
		t.Errorf("failedReasons = %v, want [\"invalid recipient\"]", failedReasons)
	}
}

func TestSendBatches_RetriesOn429ThenSucceeds(t *testing.T) {
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"Messages": []map[string]string{{"Status": "success"}},
		})
	}))
	defer server.Close()

	client := &campaigns.MailjetClient{
		APIKey: "key", APISecret: "secret", BaseURL: server.URL,
		RetryDelay: time.Millisecond,
	}

	var failures int
	sent := client.SendBatches("from@example.com", "Sender", makeRecipients(1), func(campaigns.MailjetRecipient, string) {
		failures++
	})

	if sent != 1 {
		t.Errorf("sent = %d, want 1 (should have succeeded on the 3rd attempt)", sent)
	}
	if failures != 0 {
		t.Errorf("failures = %d, want 0", failures)
	}
	if attempts.Load() != 3 {
		t.Errorf("made %d attempts, want 3 (2 rate-limited + 1 success)", attempts.Load())
	}
}

func TestSendBatches_GivesUpOnPersistent429(t *testing.T) {
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := &campaigns.MailjetClient{
		APIKey: "key", APISecret: "secret", BaseURL: server.URL,
		RetryDelay: time.Millisecond,
	}
	recipients := makeRecipients(2)

	var failedEmails []string
	sent := client.SendBatches("from@example.com", "Sender", recipients, func(r campaigns.MailjetRecipient, reason string) {
		failedEmails = append(failedEmails, r.Email)
	})

	if sent != 0 {
		t.Errorf("sent = %d, want 0", sent)
	}
	if len(failedEmails) != 2 {
		t.Errorf("failedEmails = %v, want both recipients reported failed", failedEmails)
	}
	// mailjetMaxRetries = 3 -> 4 total attempts (the initial try + 3 retries)
	if attempts.Load() != 4 {
		t.Errorf("made %d attempts, want 4", attempts.Load())
	}
}

func TestSendBatches_DoesNotRetryNonRetryableError(t *testing.T) {
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"ErrorMessage":"malformed request"}`))
	}))
	defer server.Close()

	client := &campaigns.MailjetClient{
		APIKey: "key", APISecret: "secret", BaseURL: server.URL,
		RetryDelay: time.Millisecond,
	}

	var failures int
	sent := client.SendBatches("from@example.com", "Sender", makeRecipients(1), func(campaigns.MailjetRecipient, string) {
		failures++
	})

	if sent != 0 {
		t.Errorf("sent = %d, want 0", sent)
	}
	if failures != 1 {
		t.Errorf("failures = %d, want 1", failures)
	}
	if attempts.Load() != 1 {
		t.Errorf("made %d attempts, want 1 (a 400 shouldn't be retried)", attempts.Load())
	}
}
