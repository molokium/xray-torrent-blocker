package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tblocker/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Hostname:             "node-1",
		BlockDuration:        10,
		SendAdminMessage:     true,
		AdminBotToken:        "111:admin-token",
		AdminChatID:          "999",
		AdminBlockTemplate:   config.DefaultAdminBlockTemplate,
		AdminUnblockTemplate: config.DefaultAdminUnblockTemplate,
		SendUserMessage:      true,
		BotToken:             "222:user-token",
		UserMessageTemplate:  config.DefaultUserMessageTemplate,
		TelegramAPIURL:       "https://api.telegram.org",
		SendWebhook:          false,
		WebhookTemplate:      config.DefaultWebhookTemplate,
		WebhookHeaders:       map[string]string{},
	}
}

type telegramRequest struct {
	path string
	form map[string][]string
}

func TestTelegramBlockSendsAdminAndUser(t *testing.T) {
	requests := make(chan telegramRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		requests <- telegramRequest{path: r.URL.Path, form: r.PostForm}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.TelegramAPIURL = server.URL
	dispatcher := New(cfg)

	dispatcher.Block(Event{
		Username:        "alice",
		UserID:          42,
		IP:              "203.0.113.9",
		Hostname:        "node-1",
		DurationMinutes: 10,
		Timestamp:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		TelegramID:      "777",
	})

	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(seen) < 2 {
		select {
		case req := <-requests:
			chatID := first(req.form["chat_id"])
			seen[chatID] = true
			if !strings.Contains(req.path, "/bot") || !strings.HasSuffix(req.path, "/sendMessage") {
				t.Errorf("path = %q", req.path)
			}
			if first(req.form["parse_mode"]) != "HTML" {
				t.Errorf("parse_mode = %q", first(req.form["parse_mode"]))
			}
			text := first(req.form["text"])
			switch chatID {
			case "777":
				if !strings.Contains(text, "10 минут") || !strings.Contains(text, "торрент") {
					t.Errorf("user message = %q", text)
				}
			case "999":
				if !strings.Contains(text, "alice") || !strings.Contains(text, "203.0.113.9") || !strings.Contains(text, "node-1") {
					t.Errorf("admin message = %q", text)
				}
			default:
				t.Errorf("unexpected chat_id %q", chatID)
			}
		case <-deadline:
			t.Fatalf("timed out; seen chats: %v", seen)
		}
	}
}

func TestTelegramUserMessageSkippedWithoutID(t *testing.T) {
	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.TelegramAPIURL = server.URL
	cfg.SendAdminMessage = false
	dispatcher := New(cfg)
	dispatcher.Block(Event{Username: "alice"})

	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("requests = %d, want 0", calls)
	}
}

func TestTelegramAPIFailureIsLoggedAndRedacted(t *testing.T) {
	var buf bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.TelegramAPIURL = server.URL
	dispatcher := New(cfg)
	dispatcher.logger = log.New(&buf, "", 0)

	dispatcher.sendTelegram(cfg.AdminBotToken, cfg.AdminChatID, "hello", Event{Username: "alice"})

	logged := buf.String()
	if !strings.Contains(logged, "Unauthorized") {
		t.Errorf("log = %q", logged)
	}
	if strings.Contains(logged, cfg.AdminBotToken) {
		t.Errorf("bot token leaked into log: %q", logged)
	}
}

func TestWebhookBlockAndUnblock(t *testing.T) {
	requests := make(chan map[string]any, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		requests <- payload
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.SendAdminMessage = false
	cfg.SendUserMessage = false
	cfg.SendWebhook = true
	cfg.WebhookURL = server.URL
	cfg.WebhookHeaders = map[string]string{"Authorization": "Bearer secret"}
	dispatcher := New(cfg)

	event := Event{
		Username:  "alice",
		IP:        "203.0.113.9",
		Hostname:  "node-1",
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	dispatcher.Block(event)
	dispatcher.Unblock(event)

	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(seen) < 2 {
		select {
		case payload := <-requests:
			action, _ := payload["action"].(string)
			seen[action] = true
			if payload["username"] != "alice" || payload["server"] != "node-1" || payload["ip"] != "203.0.113.9" {
				t.Errorf("payload = %#v", payload)
			}
			if payload["timestamp"] != "2026-01-01T00:00:00Z" {
				t.Errorf("timestamp = %v", payload["timestamp"])
			}
		case <-deadline:
			t.Fatalf("timed out; seen: %v", seen)
		}
	}
	if !seen["block"] || !seen["unblock"] {
		t.Errorf("actions = %v", seen)
	}
}

func TestWebhookDurationArgument(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.SendAdminMessage = false
	cfg.SendUserMessage = false
	cfg.SendWebhook = true
	cfg.WebhookURL = server.URL
	cfg.WebhookTemplate = `{"username":"%s","ip":"%s","server":"%s","action":"%s","timestamp":"%s","duration":%d}`
	cfg.WebhookTemplateHasDuration = true
	dispatcher := New(cfg)

	dispatcher.sendWebhook(Event{
		Username:        "alice",
		IP:              "203.0.113.9",
		Hostname:        "node-1",
		DurationMinutes: 10,
		Timestamp:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}, "block")

	if payload["duration"] != float64(10) {
		t.Errorf("payload = %#v", payload)
	}
}

func TestWebhookFailureDoesNotPanic(t *testing.T) {
	var buf bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.SendWebhook = true
	cfg.WebhookURL = server.URL
	dispatcher := New(cfg)
	dispatcher.logger = log.New(&buf, "", 0)

	dispatcher.sendWebhook(Event{Username: "alice"}, "block")
	if !strings.Contains(buf.String(), "HTTP 500") {
		t.Errorf("log = %q", buf.String())
	}
}

func TestUserMessageWithoutFormatVerbs(t *testing.T) {
	cfg := testConfig()
	cfg.UserMessageTemplate = "plain message"
	dispatcher := New(cfg)
	if got := dispatcher.userMessage(Event{DurationMinutes: 10}); got != "plain message" {
		t.Errorf("message = %q", got)
	}
}

func TestAdminMessageFallbackName(t *testing.T) {
	cfg := testConfig()
	dispatcher := New(cfg)
	got := dispatcher.adminMessage(cfg.AdminBlockTemplate, Event{UserID: 42, Hostname: "node"})
	if !strings.Contains(got, "user-42") {
		t.Errorf("message = %q", got)
	}
}

func TestRedact(t *testing.T) {
	cfg := testConfig()
	dispatcher := New(cfg)
	got := dispatcher.redact("url /bot" + cfg.BotToken + "/sendMessage " + cfg.AdminBotToken)
	if strings.Contains(got, cfg.BotToken) || strings.Contains(got, cfg.AdminBotToken) {
		t.Errorf("not redacted: %q", got)
	}
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func TestWebhookDoesNotFollowRedirect(t *testing.T) {
	var hit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	cfg := testConfig()
	cfg.SendAdminMessage = false
	cfg.SendUserMessage = false
	cfg.SendWebhook = true
	cfg.WebhookURL = source.URL
	var buf bytes.Buffer
	dispatcher := New(cfg)
	dispatcher.logger = log.New(&buf, "", 0)

	dispatcher.sendWebhook(Event{Username: "alice"}, "block")

	if hit.Load() {
		t.Fatal("webhook headers were forwarded to the redirect target")
	}
	if !strings.Contains(buf.String(), "HTTP 302") {
		t.Errorf("log = %q", buf.String())
	}
}

func TestTelegramFailureSendsAdmin(t *testing.T) {
	requests := make(chan telegramRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		requests <- telegramRequest{path: r.URL.Path, form: r.PostForm}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.TelegramAPIURL = server.URL
	cfg.SendUserMessage = false
	dispatcher := New(cfg)

	dispatcher.Failure(Event{Username: "alice", UserID: 42}, "block", errors.New("network down"), 2, 30*time.Second)

	select {
	case req := <-requests:
		if got := first(req.form["chat_id"]); got != "999" {
			t.Errorf("chat_id = %q", got)
		}
		text := first(req.form["text"])
		for _, want := range []string{"block failed", "alice", "network down", "Attempt: 2", "30s"} {
			if !strings.Contains(text, want) {
				t.Errorf("text %q missing %q", text, want)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for failure notification")
	}
}

func TestTelegramFailsafeSendsAdminOnly(t *testing.T) {
	requests := make(chan telegramRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		requests <- telegramRequest{path: r.URL.Path, form: r.PostForm}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.TelegramAPIURL = server.URL
	dispatcher := New(cfg)
	dispatcher.Failsafe(Event{Username: "alice", UserID: 42}, []string{"node-a", "node-b"}, nil, nil)

	select {
	case req := <-requests:
		if got := first(req.form["chat_id"]); got != "999" {
			t.Errorf("chat_id = %q, want admin", got)
		}
		text := first(req.form["text"])
		if !strings.Contains(text, "failsafe") || !strings.Contains(text, "node-a") {
			t.Errorf("text = %q", text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for failsafe notification")
	}
}

func TestAdminMessageIncludesInbound(t *testing.T) {
	cfg := testConfig()
	dispatcher := New(cfg)
	got := dispatcher.adminMessage(cfg.AdminBlockTemplate, Event{
		Username: "alice",
		IP:       "203.0.113.9",
		Inbound:  "vless-in",
		Hostname: "node-1",
	})
	if !strings.Contains(got, "<b>Inbound :</b> vless-in") {
		t.Errorf("message = %q", got)
	}
}

func TestAdminMessageEscapesInbound(t *testing.T) {
	cfg := testConfig()
	dispatcher := New(cfg)
	got := dispatcher.adminMessage(cfg.AdminBlockTemplate, Event{Username: "alice", Inbound: `vless<&`})
	if strings.Contains(got, "vless<&") || !strings.Contains(got, "vless&lt;&amp;") {
		t.Errorf("message = %q", got)
	}
}
