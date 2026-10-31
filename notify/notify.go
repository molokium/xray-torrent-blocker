// Package notify sends Telegram and webhook notifications.
package notify

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tblocker/config"
)

const maxResponseBody = 64 << 10

// Event describes one block or unblock.
type Event struct {
	Username        string
	UserID          int
	IP              string
	Hostname        string
	DurationMinutes int
	Timestamp       time.Time
	TelegramID      string
}

// Dispatcher sends notifications without blocking the blocking pipeline.
type Dispatcher struct {
	cfg      *config.Config
	telegram *http.Client
	webhook  *http.Client
	logger   *log.Logger
}

// New builds a Dispatcher from configuration.
func New(cfg *config.Config) *Dispatcher {
	return &Dispatcher{
		cfg: cfg,
		telegram: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// The bot token is part of the URL; never forward it.
				return http.ErrUseLastResponse
			},
		},
		webhook: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				// Do not forward Authorization/custom headers to another host.
				return http.ErrUseLastResponse
			},
		},
		logger: log.New(log.Writer(), "notify: ", log.LstdFlags),
	}
}

// Block sends the notifications for a newly blocked user.
func (d *Dispatcher) Block(event Event) {
	if d.cfg.SendUserMessage {
		if event.TelegramID == "" {
			d.logger.Printf("user %s has no Telegram ID; user message skipped", event.Username)
		} else {
			chatID := event.TelegramID
			text := d.userMessage(event)
			go d.sendTelegram(d.cfg.BotToken, chatID, text, event)
		}
	}
	if d.cfg.SendAdminMessage {
		text := d.adminMessage(d.cfg.AdminBlockTemplate, event)
		go d.sendTelegram(d.cfg.AdminBotToken, d.cfg.AdminChatID, text, event)
	}
	if d.cfg.SendWebhook {
		go d.sendWebhook(event, "block")
	}
}

// Unblock sends the notifications for a user that was enabled again.
func (d *Dispatcher) Unblock(event Event) {
	if d.cfg.SendAdminMessage {
		text := d.adminMessage(d.cfg.AdminUnblockTemplate, event)
		go d.sendTelegram(d.cfg.AdminBotToken, d.cfg.AdminChatID, text, event)
	}
	if d.cfg.SendWebhook {
		go d.sendWebhook(event, "unblock")
	}
}

func (d *Dispatcher) userMessage(event Event) string {
	template := d.cfg.UserMessageTemplate
	if !strings.Contains(template, "%") {
		return template
	}
	return fmt.Sprintf(template, event.DurationMinutes, event.DurationMinutes)
}

func (d *Dispatcher) adminMessage(template string, event Event) string {
	username := event.Username
	if username == "" {
		username = fmt.Sprintf("user-%d", event.UserID)
	}
	return fmt.Sprintf(template, username, event.IP, event.Hostname, username)
}

func (d *Dispatcher) sendTelegram(token, chatID, text string, event Event) {
	if token == "" || chatID == "" {
		return
	}
	endpoint := strings.TrimRight(d.cfg.TelegramAPIURL, "/") + "/bot" + token + "/sendMessage"
	form := url.Values{}
	form.Set("chat_id", chatID)
	form.Set("text", text)
	form.Set("parse_mode", "HTML")
	form.Set("disable_web_page_preview", "true")

	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		d.logger.Printf("telegram: create request for user %s: %s", event.Username, d.redact(err.Error()))
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := d.telegram.Do(req)
	if err != nil {
		d.logger.Printf("telegram: send for user %s failed: %s", event.Username, d.redact(err.Error()))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))

	var result struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(body, &result)

	if resp.StatusCode != http.StatusOK || !result.OK {
		description := result.Description
		if description == "" {
			description = strings.TrimSpace(string(body))
		}
		d.logger.Printf("telegram: send for user %s failed: HTTP %d, error %d: %s",
			event.Username, resp.StatusCode, result.ErrorCode, d.redact(description))
	}
}

func (d *Dispatcher) sendWebhook(event Event, action string) {
	username := event.Username
	if username == "" {
		username = fmt.Sprintf("user-%d", event.UserID)
	}
	timestamp := event.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	args := []any{
		username,
		event.IP,
		event.Hostname,
		action,
		timestamp.Format(time.RFC3339),
	}
	if d.cfg.WebhookTemplateHasDuration {
		args = append(args, event.DurationMinutes)
	}
	payload := fmt.Sprintf(d.cfg.WebhookTemplate, args...)

	req, err := http.NewRequest(http.MethodPost, d.cfg.WebhookURL, strings.NewReader(payload))
	if err != nil {
		d.logger.Printf("webhook: create %s request: %s", action, d.redact(err.Error()))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tblocker")
	for key, value := range d.cfg.WebhookHeaders {
		req.Header.Set(key, value)
	}

	resp, err := d.webhook.Do(req)
	if err != nil {
		d.logger.Printf("webhook: %s request failed: %s", action, d.redact(err.Error()))
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		d.logger.Printf("webhook: %s request returned HTTP %d", action, resp.StatusCode)
	}
}

func (d *Dispatcher) redact(message string) string {
	for _, token := range []string{d.cfg.BotToken, d.cfg.AdminBotToken, d.cfg.WebhookURL} {
		if token != "" {
			message = strings.ReplaceAll(message, token, "<redacted>")
		}
	}
	return message
}
