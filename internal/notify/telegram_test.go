package notify_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dedo1911/ingress-plus-backend/internal/notify"
)

type sendMessageIn struct {
	ChatID          string `json:"chat_id"`
	MessageThreadID int64  `json:"message_thread_id"`
	ParseMode       string `json:"parse_mode"`
	Text            string `json:"text"`
}

func TestSend_PostsToTheTopic(t *testing.T) {
	var path string
	var in sendMessageIn

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := &notify.Telegram{
		Token:   "123:abc",
		ChatID:  "-100999",
		Topics:  notify.Topics{Badges: 43},
		BaseURL: server.URL,
	}

	if err := client.Send(context.Background(), client.Topics.Badges, "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want := "/bot123:abc/sendMessage"; path != want {
		t.Errorf("expected path %q, got %q", want, path)
	}
	if in.ChatID != "-100999" {
		t.Errorf("expected chat_id -100999, got %q", in.ChatID)
	}
	if in.MessageThreadID != 43 {
		t.Errorf("expected message_thread_id 43, got %d", in.MessageThreadID)
	}
	if in.ParseMode != "HTML" {
		t.Errorf("expected parse_mode HTML, got %q", in.ParseMode)
	}
	if in.Text != "hello" {
		t.Errorf("expected text hello, got %q", in.Text)
	}
}

func TestSend_NoopWhenNotConfigured(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()

	scenarios := []notify.Telegram{
		{Token: "", ChatID: "-100999", BaseURL: server.URL},
		{Token: "123:abc", ChatID: "", BaseURL: server.URL},
	}

	for _, client := range scenarios {
		if client.Enabled() {
			t.Errorf("expected client with token %q / chat %q to be disabled", client.Token, client.ChatID)
		}
		if err := client.Send(context.Background(), 0, "hello"); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}

	if called {
		t.Error("expected no request to be sent by a client without configuration")
	}
}

// The bot token travels in the URL path, so anything that echoes the
// request back - net/http's own error strings, or Telegram's error body -
// can carry the secret straight into the logs.
func TestSend_RedactsTheTokenFromErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"ok":false,"description":"Unauthorized: bot123:abc"}`))
	}))
	defer server.Close()

	client := &notify.Telegram{Token: "123:abc", ChatID: "-100999", BaseURL: server.URL}

	err := client.Send(context.Background(), 0, "hello")
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected the error to mention the status, got %q", err)
	}
	if strings.Contains(err.Error(), "123:abc") {
		t.Errorf("expected the token to be redacted, got %q", err)
	}
}

func TestSend_ErrorOnUnreachableHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close() // nothing is listening anymore

	client := &notify.Telegram{Token: "123:abc", ChatID: "-100999", BaseURL: url}

	err := client.Send(context.Background(), 0, "hello")
	if err == nil {
		t.Fatal("expected an error when the API is unreachable")
	}
	if strings.Contains(err.Error(), "123:abc") {
		t.Errorf("expected the token to be redacted, got %q", err)
	}
}

func TestMessage_EscapesTheValue(t *testing.T) {
	got := notify.Message("Badge created", "Fire & <Ice>")
	want := "<b>Badge created:</b> Fire &amp; &lt;Ice&gt;"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestMediaMessage_EscapesEveryField(t *testing.T) {
	got := notify.MediaMessage("A & B", "<id>", "Agent&1")
	want := "<b>New media received:</b>\nMedia: A &amp; B\nMedia ID: &lt;id&gt;\nUploader: Agent&amp;1"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestTelegramFromEnv(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:abc")
	t.Setenv("TELEGRAM_CHAT_ID", "-100999")
	t.Setenv("TELEGRAM_TOPIC_BADGES", "43")
	t.Setenv("TELEGRAM_TOPIC_BUG_REPORTS", "11")
	t.Setenv("TELEGRAM_TOPIC_MEDIA", "not-a-number")

	client := notify.TelegramFromEnv()

	if !client.Enabled() {
		t.Error("expected a fully configured client to be enabled")
	}
	if client.Topics.Badges != 43 || client.Topics.BugReports != 11 {
		t.Errorf("unexpected topics: %+v", client.Topics)
	}
	// An unparsable topic falls back to the group's General thread rather
	// than dropping the notification.
	if client.Topics.Media != 0 {
		t.Errorf("expected an invalid topic id to fall back to 0, got %d", client.Topics.Media)
	}
}
