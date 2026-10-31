// Package config loads and validates the tblocker configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v2"
)

const (
	DefaultTorrentTag       = "TORRENT"
	DefaultUsernameRegex    = `\semail:\s+(\S+)`
	DefaultStorageDir       = "/opt/tblocker"
	DefaultTelegramAPIURL   = "https://api.telegram.org"
	DefaultTimeoutSeconds   = 10
	DefaultCheckIntervalSec = 15
	// DefaultDetectionThreshold preserves the historical immediate-block
	// behaviour. Set it to a higher value (for example 10) to require a burst
	// of torrent-tagged lines before blocking.
	DefaultDetectionThreshold = 1
	// DefaultDetectionWindowMinutes is the sliding window for the threshold.
	DefaultDetectionWindowMinutes = 5
)

const (
	DefaultUserMessageTemplate = `⚠️ <b>Замечено использование торрентов</b> ⚠️

Возможно вы забыли отключить торрент-клиент и сейчас качаете или раздаете торренты через VPN. 
У нас запрещено скачивать или раздавать торренты.

Мы заблокируем ваше подключение на сервере на %d минут, выключите пожалуйста торрент-клиент полностью. 
Через %d минут вас разблокирует, но если вы продолжите качать торренты, вас снова заблокирует и вы получите данное сообщение снова.`

	DefaultAdminBlockTemplate = `⛔️ <b>#Blocked</b>
➖➖➖➖➖➖➖➖➖
<b>Username :</b> %s
<b>IP :</b> %s
<b>Server :</b> %s
➖➖➖➖➖➖➖➖➖
<b>User tag :</b> #%s`

	DefaultAdminUnblockTemplate = `☑️ <b>#Unblocked</b>
➖➖➖➖➖➖➖➖➖
<b>Username :</b> %s
<b>IP :</b> %s
<b>Server :</b> %s
➖➖➖➖➖➖➖➖➖
<b>User tag :</b> #%s`

	DefaultWebhookTemplate = `{"username":"%s","ip":"%s","server":"%s","action":"%s","timestamp":"%s"}`
)

// PasarGuardConfig describes how to reach the PasarGuard panel API.
type PasarGuardConfig struct {
	// BaseURL is the panel root, for example https://panel.example.com.
	// A path prefix (https://example.com/panel) is supported.
	BaseURL string `yaml:"BaseURL"`

	// APIKey is a PasarGuard API key (pg_key_...) or an admin JWT.
	APIKey string `yaml:"APIKey"`

	// Identifier decides how the value captured from the log's email field is
	// interpreted: "auto", "id", or "username".
	Identifier string `yaml:"Identifier"`

	// TimeoutSeconds bounds every API request.
	TimeoutSeconds int `yaml:"TimeoutSeconds"`

	// InsecureSkipVerify disables TLS certificate verification. Do not use it
	// unless the panel is reached over a trusted private network.
	InsecureSkipVerify bool `yaml:"InsecureSkipVerify"`

	// ReenableOnUnblock controls whether the account is enabled again when the
	// block expires. Default: true.
	ReenableOnUnblock *bool `yaml:"ReenableOnUnblock"`

	// StartupCheck probes the API at startup and logs an error when the URL,
	// key, or permissions are wrong. Default: true.
	StartupCheck *bool `yaml:"StartupCheck"`
}

// ReenableOnUnblockEnabled reports the resolved default (true).
func (p PasarGuardConfig) ReenableOnUnblockEnabled() bool {
	return p.ReenableOnUnblock == nil || *p.ReenableOnUnblock
}

// StartupCheckEnabled reports the resolved default (true).
func (p PasarGuardConfig) StartupCheckEnabled() bool {
	return p.StartupCheck == nil || *p.StartupCheck
}

// Config is the validated runtime configuration.
type Config struct {
	LogFile       string `yaml:"LogFile"`
	BlockDuration int    `yaml:"BlockDuration"`
	TorrentTag    string `yaml:"TorrentTag"`
	// DetectionThreshold is the number of torrent-tagged lines required
	// within DetectionWindowMinutes before a block is applied. 1 disables
	// the tolerance and blocks on the first line.
	DetectionThreshold int `yaml:"DetectionThreshold"`
	// DetectionWindowMinutes is the sliding window for DetectionThreshold.
	DetectionWindowMinutes int    `yaml:"DetectionWindowMinutes"`
	StorageDir             string `yaml:"StorageDir"`
	Hostname               string `yaml:"Hostname"`
	CheckIntervalSeconds   int    `yaml:"CheckIntervalSeconds"`
	// UsePolling switches the log follower from inotify to stat polling.
	// Polling is slower but works on filesystems with unreliable inotify.
	UsePolling bool `yaml:"UsePolling"`

	SendAdminMessage     bool   `yaml:"SendAdminMessage"`
	AdminBotToken        string `yaml:"AdminBotToken"`
	AdminChatID          string `yaml:"AdminChatID"`
	AdminBlockTemplate   string `yaml:"AdminBlockTemplate"`
	AdminUnblockTemplate string `yaml:"AdminUnblockTemplate"`

	SendUserMessage     bool   `yaml:"SendUserMessage"`
	BotToken            string `yaml:"BotToken"`
	TidRegex            string `yaml:"TidRegex"`
	UserMessageTemplate string `yaml:"UserMessageTemplate"`
	TelegramAPIURL      string `yaml:"TelegramAPIURL"`

	SendWebhook     bool              `yaml:"SendWebhook"`
	WebhookURL      string            `yaml:"WebhookURL"`
	WebhookTemplate string            `yaml:"WebhookTemplate"`
	WebhookHeaders  map[string]string `yaml:"WebhookHeaders"`

	UsernameRegex string `yaml:"UsernameRegex"`

	PasarGuard PasarGuardConfig `yaml:"PasarGuard"`

	// UsernameRE extracts the PasarGuard identity from a log line. Capture
	// group 1 is the value (numeric user ID or username).
	UsernameRE *regexp.Regexp `yaml:"-"`
	// TidRE extracts a Telegram chat ID from the identity value, username, or
	// note. The raw log line is deliberately not searched: destination text is
	// attacker-influenced and could inject an arbitrary chat ID.
	TidRE *regexp.Regexp `yaml:"-"`
	// WebhookTemplateHasDuration is true when the template uses the optional
	// sixth %d argument (block duration in minutes).
	WebhookTemplateHasDuration bool `yaml:"-"`
}

// Load reads, defaults, and validates a configuration file. The returned
// warnings are non-fatal migration hints meant to be logged by the caller.
func Load(path string) (*Config, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	warnings := legacyKeyWarnings(data)
	const legacyDefaultUsernameRegex = `email:\s*(\S+)`
	if cfg.UsernameRegex == legacyDefaultUsernameRegex {
		// pg.1-pg.3 shipped this unanchored default; it could match a
		// destination such as "tcp:email:5" and block the wrong account.
		cfg.UsernameRegex = DefaultUsernameRegex
		warnings = append(warnings, "UsernameRegex used the legacy unanchored default; switched to the whitespace-anchored default")
	}

	applyDefaults(&cfg)

	var errs []error
	if err := validate(&cfg); err != nil {
		errs = append(errs, err)
	}
	if err := compileRegexes(&cfg); err != nil {
		errs = append(errs, err)
	}
	if err := validateTemplates(&cfg); err != nil {
		errs = append(errs, err)
	}
	if err := validateURLs(&cfg); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, warnings, fmt.Errorf("invalid configuration:\n%w", errors.Join(errs...))
	}

	warnings = append(warnings, securityWarnings(&cfg)...)
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
		// The example file intentionally ships world-readable and contains
		// placeholders; only warn when a real-looking key is present.
		if !strings.Contains(cfg.PasarGuard.APIKey, "00000000-0000-4000-8000-000000000000") {
			warnings = append(warnings, fmt.Sprintf("%s is readable by group/others; it contains API keys and bot tokens (chmod 600 recommended)", path))
		}
	}
	return &cfg, warnings, nil
}

// securityWarnings flags configurations that put credentials or user data on
// the wire in cleartext. Local endpoints (loopback) are intentionally exempt:
// a local Bot API server or panel on 127.0.0.1 is a normal deployment.
func securityWarnings(cfg *Config) []string {
	var warnings []string

	warnHTTP := func(name, raw string) {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" {
			return
		}
		if isLoopbackHost(u.Hostname()) {
			return
		}
		warnings = append(warnings, fmt.Sprintf("%s uses http://; credentials and user data are sent in cleartext", name))
	}

	warnHTTP("PasarGuard.BaseURL", cfg.PasarGuard.BaseURL)
	if cfg.SendWebhook {
		warnHTTP("WebhookURL", cfg.WebhookURL)
	}
	if cfg.SendAdminMessage || cfg.SendUserMessage {
		warnHTTP("TelegramAPIURL", cfg.TelegramAPIURL)
	}
	if cfg.PasarGuard.InsecureSkipVerify {
		warnings = append(warnings, "PasarGuard.InsecureSkipVerify is enabled; the panel TLS certificate is not verified")
	}
	return warnings
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func applyDefaults(cfg *Config) {
	if strings.TrimSpace(cfg.TorrentTag) == "" {
		cfg.TorrentTag = DefaultTorrentTag
	}
	if cfg.DetectionThreshold <= 0 {
		cfg.DetectionThreshold = DefaultDetectionThreshold
	}
	if cfg.DetectionWindowMinutes <= 0 {
		cfg.DetectionWindowMinutes = DefaultDetectionWindowMinutes
	}
	if strings.TrimSpace(cfg.StorageDir) == "" {
		cfg.StorageDir = DefaultStorageDir
	}
	if strings.TrimSpace(cfg.UsernameRegex) == "" {
		cfg.UsernameRegex = DefaultUsernameRegex
	}
	if strings.TrimSpace(cfg.AdminBlockTemplate) == "" {
		cfg.AdminBlockTemplate = DefaultAdminBlockTemplate
	}
	if strings.TrimSpace(cfg.AdminUnblockTemplate) == "" {
		cfg.AdminUnblockTemplate = DefaultAdminUnblockTemplate
	}
	if strings.TrimSpace(cfg.UserMessageTemplate) == "" {
		cfg.UserMessageTemplate = DefaultUserMessageTemplate
	}
	if strings.TrimSpace(cfg.WebhookTemplate) == "" {
		cfg.WebhookTemplate = DefaultWebhookTemplate
	}
	if strings.TrimSpace(cfg.TelegramAPIURL) == "" {
		cfg.TelegramAPIURL = DefaultTelegramAPIURL
	}
	if cfg.CheckIntervalSeconds == 0 {
		cfg.CheckIntervalSeconds = DefaultCheckIntervalSec
	}
	if strings.TrimSpace(cfg.PasarGuard.Identifier) == "" {
		cfg.PasarGuard.Identifier = "auto"
	}
	cfg.PasarGuard.Identifier = strings.ToLower(strings.TrimSpace(cfg.PasarGuard.Identifier))
	if cfg.PasarGuard.TimeoutSeconds == 0 {
		cfg.PasarGuard.TimeoutSeconds = DefaultTimeoutSeconds
	}
	cfg.PasarGuard.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.PasarGuard.BaseURL), "/")
	cfg.PasarGuard.APIKey = strings.TrimSpace(cfg.PasarGuard.APIKey)
	cfg.WebhookHeaders = cloneHeaders(cfg.WebhookHeaders)

	if strings.TrimSpace(cfg.Hostname) == "" {
		if hostname, err := os.Hostname(); err == nil && hostname != "" {
			cfg.Hostname = hostname
		} else {
			cfg.Hostname = "unknown"
		}
	}
}

func cloneHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func validate(cfg *Config) error {
	var errs []error

	if strings.TrimSpace(cfg.LogFile) == "" {
		errs = append(errs, errors.New("LogFile is required"))
	}
	if cfg.BlockDuration <= 0 {
		errs = append(errs, errors.New("BlockDuration must be greater than 0 minutes"))
	}
	if cfg.BlockDuration > 525600 {
		errs = append(errs, errors.New("BlockDuration must not exceed 525600 minutes (one year)"))
	}
	if strings.TrimSpace(cfg.TorrentTag) == "" {
		errs = append(errs, errors.New("TorrentTag must not be empty"))
	}
	if cfg.DetectionThreshold < 1 || cfg.DetectionThreshold > 1000 {
		errs = append(errs, errors.New("DetectionThreshold must be between 1 and 1000"))
	}
	if cfg.DetectionWindowMinutes < 1 || cfg.DetectionWindowMinutes > 1440 {
		errs = append(errs, errors.New("DetectionWindowMinutes must be between 1 and 1440"))
	}
	if cfg.CheckIntervalSeconds < 5 || cfg.CheckIntervalSeconds > 3600 {
		errs = append(errs, errors.New("CheckIntervalSeconds must be between 5 and 3600"))
	}

	switch cfg.PasarGuard.Identifier {
	case "auto", "id", "username":
	default:
		errs = append(errs, fmt.Errorf("PasarGuard.Identifier must be one of auto, id, username (got %q)", cfg.PasarGuard.Identifier))
	}
	if cfg.PasarGuard.TimeoutSeconds < 1 || cfg.PasarGuard.TimeoutSeconds > 120 {
		errs = append(errs, errors.New("PasarGuard.TimeoutSeconds must be between 1 and 120"))
	}

	if cfg.SendAdminMessage {
		if strings.TrimSpace(cfg.AdminBotToken) == "" {
			errs = append(errs, errors.New("AdminBotToken is required when SendAdminMessage is true"))
		}
		if strings.TrimSpace(cfg.AdminChatID) == "" {
			errs = append(errs, errors.New("AdminChatID is required when SendAdminMessage is true"))
		}
	}
	if cfg.SendUserMessage {
		if strings.TrimSpace(cfg.BotToken) == "" {
			errs = append(errs, errors.New("BotToken is required when SendUserMessage is true"))
		}
	}
	if cfg.SendWebhook && strings.TrimSpace(cfg.WebhookURL) == "" {
		errs = append(errs, errors.New("WebhookURL is required when SendWebhook is true"))
	}

	return errors.Join(errs...)
}

func compileRegexes(cfg *Config) error {
	var errs []error

	re, err := regexp.Compile(cfg.UsernameRegex)
	if err != nil {
		errs = append(errs, fmt.Errorf("invalid UsernameRegex: %w", err))
	} else if re.NumSubexp() < 1 {
		errs = append(errs, errors.New("UsernameRegex must contain at least one capture group, for example: email:\\s*(\\S+)"))
	} else {
		cfg.UsernameRE = re
	}

	if strings.TrimSpace(cfg.TidRegex) != "" {
		tidRE, err := regexp.Compile(cfg.TidRegex)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid TidRegex: %w", err))
		} else if tidRE.NumSubexp() < 1 {
			errs = append(errs, errors.New("TidRegex must contain at least one capture group"))
		} else {
			cfg.TidRE = tidRE
		}
	}

	return errors.Join(errs...)
}

func validateURLs(cfg *Config) error {
	var errs []error

	if cfg.PasarGuard.BaseURL == "" {
		errs = append(errs, errors.New("PasarGuard.BaseURL is required"))
	} else if u, err := url.Parse(cfg.PasarGuard.BaseURL); err != nil {
		errs = append(errs, fmt.Errorf("invalid PasarGuard.BaseURL: %w", err))
	} else if u.Scheme != "http" && u.Scheme != "https" {
		errs = append(errs, errors.New("PasarGuard.BaseURL must use http or https"))
	} else if u.Host == "" {
		errs = append(errs, errors.New("PasarGuard.BaseURL must include a host"))
	} else if u.User != nil {
		errs = append(errs, errors.New("PasarGuard.BaseURL must not contain credentials"))
	} else if u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, errors.New("PasarGuard.BaseURL must not contain a query or fragment"))
	}

	if strings.TrimSpace(cfg.PasarGuard.APIKey) == "" {
		errs = append(errs, errors.New("PasarGuard.APIKey is required"))
	}

	if cfg.SendWebhook {
		if u, err := url.Parse(cfg.WebhookURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			errs = append(errs, errors.New("WebhookURL must be an absolute http(s) URL without embedded credentials"))
		}
	}

	if cfg.SendAdminMessage || cfg.SendUserMessage {
		if u, err := url.Parse(cfg.TelegramAPIURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			errs = append(errs, errors.New("TelegramAPIURL must be an absolute http(s) URL without embedded credentials"))
		}
	}

	return errors.Join(errs...)
}

func validateTemplates(cfg *Config) error {
	var errs []error

	check := func(name, tmpl string, args ...any) {
		out := fmt.Sprintf(tmpl, args...)
		if strings.Contains(out, "%!") {
			errs = append(errs, fmt.Errorf("%s contains invalid or mismatched format verbs", name))
		}
	}

	if cfg.SendAdminMessage {
		check("AdminBlockTemplate", cfg.AdminBlockTemplate, "user", "1.2.3.4", "host", "user")
		check("AdminUnblockTemplate", cfg.AdminUnblockTemplate, "user", "1.2.3.4", "host", "user")
	}
	if cfg.SendUserMessage {
		check("UserMessageTemplate", cfg.UserMessageTemplate, cfg.BlockDuration, cfg.BlockDuration)
	}
	if cfg.SendWebhook {
		hasDuration, err := webhookTemplateMode(cfg.WebhookTemplate)
		if err != nil {
			errs = append(errs, err)
		} else {
			cfg.WebhookTemplateHasDuration = hasDuration
		}
	}

	return errors.Join(errs...)
}

// webhookTemplateMode accepts either the v0.3.1 five-argument template
// (username, IP, server, action, timestamp) or the later six-argument form
// with a %d duration. It returns true for the six-argument form.
func webhookTemplateMode(template string) (bool, error) {
	base := []any{"user", "1.2.3.4", "host", "block", "2026-01-01T00:00:00Z"}

	check := func(args []any) bool {
		out := fmt.Sprintf(template, args...)
		return !strings.Contains(out, "%!") && json.Valid([]byte(out))
	}

	if check(base) {
		return false, nil
	}
	six := append(append([]any{}, base...), 10)
	if check(six) {
		return true, nil
	}
	return false, errors.New("WebhookTemplate must produce valid JSON with five %s arguments (username, IP, server, action, timestamp) or six arguments with a final %d duration")
}

func legacyKeyWarnings(data []byte) []string {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil
	}

	var warnings []string
	if _, ok := raw["BlockMode"]; ok {
		warnings = append(warnings, "BlockMode is ignored: firewall blocking was removed; users are blocked through the PasarGuard API")
	}
	if _, ok := raw["BypassIPS"]; ok {
		warnings = append(warnings, "BypassIPS is ignored: this build does not touch the host firewall")
	}
	if _, ok := raw["IgnoreEmail"]; ok {
		warnings = append(warnings, "IgnoreEmail is not supported: without the email field a log line cannot be mapped to a PasarGuard user")
	}
	if _, ok := raw["EnablePerformanceMetrics"]; ok {
		warnings = append(warnings, "EnablePerformanceMetrics is not supported by this build")
	}
	return warnings
}
