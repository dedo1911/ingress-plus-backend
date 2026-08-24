package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	telegramAPIBase = "https://api.telegram.org"
	// The notifications are fire-and-forget side effects of a request or of
	// a record hook, so a Telegram outage must never keep a caller waiting:
	// http.DefaultClient has no timeout at all, which would leave the
	// goroutine (and, for the synchronous Send, the request) hanging on a
	// connection that is accepted and then never answered.
	telegramTimeout = 10 * time.Second
)

// Topics maps each kind of notification to the Telegram forum topic
// (message thread) it belongs to, mirroring the message_thread_id values
// the old pb_hooks/telegram.pb.js had hardcoded in its URLs.
type Topics struct {
	Badges     int64
	BugReports int64
	Media      int64
}

// Telegram posts short HTML notifications into the topics of a single
// Telegram supergroup. The bot token is a deploy-time secret rather than
// something an admin edits at runtime, so unlike the Mailjet credentials
// (which come from the PocketBase SMTP settings) this is configured
// entirely through the environment - see TelegramFromEnv.
type Telegram struct {
	Token  string
	ChatID string
	Topics Topics

	// BaseURL overrides the real Telegram Bot API endpoint; used by tests
	// to point at an httptest.Server instead.
	BaseURL string

	// HTTPClient is overridable for tests; defaults to a client with a
	// telegramTimeout deadline.
	HTTPClient *http.Client
}

// TelegramFromEnv builds a Telegram client from TELEGRAM_BOT_TOKEN,
// TELEGRAM_CHAT_ID and the TELEGRAM_TOPIC_* thread ids. It always returns
// a usable client: with the token or chat id unset, Enabled reports false
// and every send becomes a no-op, so a local run without the secret behaves
// like the old deployments that simply had no pb_hooks mounted.
func TelegramFromEnv() *Telegram {
	return &Telegram{
		Token:  os.Getenv("TELEGRAM_BOT_TOKEN"),
		ChatID: os.Getenv("TELEGRAM_CHAT_ID"),
		Topics: Topics{
			Badges:     envInt64("TELEGRAM_TOPIC_BADGES"),
			BugReports: envInt64("TELEGRAM_TOPIC_BUG_REPORTS"),
			Media:      envInt64("TELEGRAM_TOPIC_MEDIA"),
		},
	}
}

func envInt64(name string) int64 {
	value, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// Enabled reports whether the client has enough configuration to send.
func (t *Telegram) Enabled() bool {
	return t != nil && t.Token != "" && t.ChatID != ""
}

func (t *Telegram) baseURL() string {
	if t.BaseURL != "" {
		return t.BaseURL
	}
	return telegramAPIBase
}

func (t *Telegram) httpClient() *http.Client {
	if t.HTTPClient != nil {
		return t.HTTPClient
	}
	return &http.Client{Timeout: telegramTimeout}
}

type sendMessageRequest struct {
	ChatID          string `json:"chat_id"`
	MessageThreadID int64  `json:"message_thread_id,omitempty"`
	ParseMode       string `json:"parse_mode"`
	Text            string `json:"text"`
}

// Send posts text (Telegram-flavoured HTML, see Message/MediaMessage) to
// the given topic. A zero topic posts to the group's General thread.
// It is a no-op when the client is not configured.
func (t *Telegram) Send(ctx context.Context, topic int64, text string) error {
	if !t.Enabled() {
		return nil
	}

	body, err := json.Marshal(sendMessageRequest{
		ChatID:          t.ChatID,
		MessageThreadID: topic,
		ParseMode:       "HTML",
		Text:            text,
	})
	if err != nil {
		return fmt.Errorf("failed to encode Telegram request: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", t.baseURL(), t.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to build Telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.httpClient().Do(req)
	if err != nil {
		// The token is part of the URL, and net/http puts the URL in its
		// error strings - strip it so a failed send can't leak the secret
		// into the logs.
		return fmt.Errorf("telegram request failed: %s", t.redact(err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("telegram returned status %d: %s", resp.StatusCode, t.redact(string(respBody)))
	}

	return nil
}

// SendAsync sends in the background, logging failures instead of
// propagating them: these notifications are informational, and neither an
// upload nor a badge edit should fail because Telegram is down. The
// context is deliberately not inherited from the caller, since the
// request/hook it belongs to is finished by the time this runs.
func (t *Telegram) SendAsync(logger *slog.Logger, topic int64, text string) {
	if !t.Enabled() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), telegramTimeout)
		defer cancel()
		if err := t.Send(ctx, topic, text); err != nil && logger != nil {
			logger.Error("Telegram notification failed", slog.Any("error", err))
		}
	}()
}

func (t *Telegram) redact(s string) string {
	if t.Token == "" {
		return s
	}
	return strings.ReplaceAll(s, t.Token, "[REDACTED]")
}

// Message formats a "<b>Label:</b> value" notification, escaping the
// value. The JS hooks concatenated raw record fields into parse_mode=HTML
// text, so a badge title containing "&" or "<" made Telegram reject the
// whole message.
func Message(label, value string) string {
	return "<b>" + html.EscapeString(label) + ":</b> " + html.EscapeString(value)
}

// MediaMessage formats the new-media notification sent on upload.
func MediaMessage(description, mediaID, uploader string) string {
	return "<b>New media received:</b>\nMedia: " + html.EscapeString(description) +
		"\nMedia ID: " + html.EscapeString(mediaID) +
		"\nUploader: " + html.EscapeString(uploader)
}
