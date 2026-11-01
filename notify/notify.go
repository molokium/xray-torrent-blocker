// Package notify sends Telegram and webhook notifications.
package notify

import (
	"encoding/json"
	"fmt"
	"html"
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
	Inbound         string
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

// Failure notifies the administrator that a block or unblock could not be
// applied and whether it will be retried. It is deliberately admin-only: a
// transport failure is not something the end user should be told about.
func (d *Dispatcher) Failure(event Event, action string, cause error, attempt int, retry time.Duration) {
	if !d.cfg.SendAdminMessage {
		return
	}
	username := event.Username
	if username == "" {
		username = fmt.Sprintf("user-%d", event.UserID)
	}
	reason := "unknown error"
	if cause != nil {
		reason = cause.Error()
	}
	retryText := "no automatic retry"
	if retry > 0 {
		retryText = retry.Round(time.Second).String()
	}
	attemptText := fmt.Sprintf("%d", attempt)
	if attempt == 0 {
		attemptText = "n/a"
	}
	text := fmt.Sprintf(
		"<b>tblocker: %s failed</b>\nUser: <code>%s</code>\nID: %d\nError: <code>%s</code>\nAttempt: %s\nRetry in: %s",
		html.EscapeString(action),
		html.EscapeString(username),
		event.UserID,
		html.EscapeString(shorten(reason, 500)),
		html.EscapeString(attemptText),
		html.EscapeString(retryText),
	)
	if event.Inbound != "" {
		text += "\nInbound: <code>" + html.EscapeString(event.Inbound) + "</code>"
	}
	go d.sendTelegram(d.cfg.AdminBotToken, d.cfg.AdminChatID, text, event)
}

// Failsafe reports the result of the optional all-node reconnect. It is
// admin-only: the end user does not need to know that the core was restarted.
func (d *Dispatcher) Failsafe(event Event, reconnected, failed []string, cause error) {
	if !d.cfg.SendAdminMessage {
		return
	}
	username := event.Username
	if username == "" {
		username = fmt.Sprintf("user-%d", event.UserID)
	}
	status := "completed"
	if cause != nil {
		status = "FAILED"
	}
	lines := []string{
		fmt.Sprintf("<b>tblocker: failsafe reconnect %s</b>", html.EscapeString(status)),
		fmt.Sprintf("User: <code>%s</code>", html.EscapeString(username)),
		fmt.Sprintf("ID: %d", event.UserID),
	}
	if event.Inbound != "" {
		lines = append(lines, fmt.Sprintf("Inbound: <code>%s</code>", html.EscapeString(event.Inbound)))
	}
	lines = append(lines,
		fmt.Sprintf("Nodes reconnected: %s", html.EscapeString(nodeList(reconnected))),
	)
	if len(failed) > 0 {
		lines = append(lines, fmt.Sprintf("Nodes failed: %s", html.EscapeString(nodeList(failed))))
	}
	if cause != nil {
		lines = append(lines, fmt.Sprintf("Error: <code>%s</code>", html.EscapeString(shorten(cause.Error(), 500))))
	}
	text := strings.Join(lines, "\n")
	go d.sendTelegram(d.cfg.AdminBotToken, d.cfg.AdminChatID, text, event)
}

func nodeList(nodes []string) string {
	if len(nodes) == 0 {
		return "none"
	}
	return strings.Join(nodes, ", ")
}

func shorten(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
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
	text := fmt.Sprintf(template, username, event.IP, event.Hostname, username)
	if event.Inbound != "" {
		text += "\n<b>Inbound :</b> " + html.EscapeString(event.Inbound)
	}
	return text
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
