package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const minimalConfig = `
LogFile: /var/log/xray/access.log
BlockDuration: 10
PasarGuard:
  BaseURL: https://panel.example.com
  APIKey: pg_key_00000000-0000-4000-8000-000000000000
`

func TestLoadDefaults(t *testing.T) {
	cfg, warnings, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	if cfg.TorrentTag != DefaultTorrentTag {
		t.Errorf("TorrentTag = %q, want %q", cfg.TorrentTag, DefaultTorrentTag)
	}
	if cfg.StorageDir != DefaultStorageDir {
		t.Errorf("StorageDir = %q, want %q", cfg.StorageDir, DefaultStorageDir)
	}
	if cfg.UsernameRegex != DefaultUsernameRegex {
		t.Errorf("UsernameRegex = %q, want %q", cfg.UsernameRegex, DefaultUsernameRegex)
	}
	if cfg.UsernameRE == nil {
		t.Fatal("UsernameRE was not compiled")
	}
	if cfg.PasarGuard.Identifier != "auto" {
		t.Errorf("Identifier = %q, want auto", cfg.PasarGuard.Identifier)
	}
	if cfg.PasarGuard.TimeoutSeconds != DefaultTimeoutSeconds {
		t.Errorf("TimeoutSeconds = %d, want %d", cfg.PasarGuard.TimeoutSeconds, DefaultTimeoutSeconds)
	}
	if cfg.CheckIntervalSeconds != DefaultCheckIntervalSec {
		t.Errorf("CheckIntervalSeconds = %d, want %d", cfg.CheckIntervalSeconds, DefaultCheckIntervalSec)
	}
	if !cfg.PasarGuard.ReenableOnUnblockEnabled() {
		t.Error("ReenableOnUnblock should default to true")
	}
	if !cfg.PasarGuard.StartupCheckEnabled() {
		t.Error("StartupCheck should default to true")
	}
	if cfg.Hostname == "" {
		t.Error("Hostname should default to the system hostname")
	}
	if cfg.AdminBlockTemplate == "" || cfg.AdminUnblockTemplate == "" || cfg.UserMessageTemplate == "" || cfg.WebhookTemplate == "" {
		t.Error("default templates were not applied")
	}
}

func TestLoadCustomValues(t *testing.T) {
	body := `
LogFile: /tmp/access.log
BlockDuration: 30
TorrentTag: "P2P"
StorageDir: /tmp/tblocker
Hostname: node-7
CheckIntervalSeconds: 30
UsernameRegex: 'user=(\S+)'
TidRegex: 'tg=(-?\d+)'
SendAdminMessage: false
SendUserMessage: false
SendWebhook: false
PasarGuard:
  BaseURL: https://panel.example.com/prefix/
  APIKey: pg_key_abc
  Identifier: username
  TimeoutSeconds: 20
  InsecureSkipVerify: true
  ReenableOnUnblock: false
  StartupCheck: false
UsePolling: true
`
	cfg, _, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.UsePolling {
		t.Error("UsePolling was not retained")
	}

	if cfg.BlockDuration != 30 || cfg.TorrentTag != "P2P" || cfg.StorageDir != "/tmp/tblocker" {
		t.Errorf("unexpected scalar values: %+v", cfg)
	}
	if cfg.Hostname != "node-7" {
		t.Errorf("Hostname = %q", cfg.Hostname)
	}
	if cfg.PasarGuard.BaseURL != "https://panel.example.com/prefix" {
		t.Errorf("BaseURL = %q", cfg.PasarGuard.BaseURL)
	}
	if cfg.PasarGuard.Identifier != "username" {
		t.Errorf("Identifier = %q", cfg.PasarGuard.Identifier)
	}
	if cfg.PasarGuard.ReenableOnUnblockEnabled() {
		t.Error("ReenableOnUnblock should be false")
	}
	if cfg.PasarGuard.StartupCheckEnabled() {
		t.Error("StartupCheck should be false")
	}
	if cfg.TidRE == nil {
		t.Error("TidRE was not compiled")
	}
	if got := cfg.TidRE.FindStringSubmatch("x tg=-100123 x"); len(got) != 2 || got[1] != "-100123" {
		t.Errorf("TidRE match = %v", got)
	}
}

func TestLoadMissingRequiredFields(t *testing.T) {
	body := `
BlockDuration: 10
PasarGuard:
  APIKey: pg_key_x
`
	_, _, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"LogFile is required", "PasarGuard.BaseURL is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "zero duration",
			body: strings.Replace(minimalConfig, "BlockDuration: 10", "BlockDuration: 0", 1),
			want: "BlockDuration must be greater than 0",
		},
		{
			name: "bad identifier",
			body: minimalConfig + "  Identifier: email\n",
			want: "Identifier must be one of",
		},
		{
			name: "bad base url",
			body: strings.Replace(minimalConfig, "https://panel.example.com", "ftp://panel", 1),
			want: "must use http or https",
		},
		{
			name: "missing api key",
			body: strings.Replace(minimalConfig, "  APIKey: pg_key_00000000-0000-4000-8000-000000000000\n", "", 1),
			want: "PasarGuard.APIKey is required",
		},
		{
			name: "bad timeout",
			body: minimalConfig + "  TimeoutSeconds: 500\n",
			want: "TimeoutSeconds must be between",
		},
		{
			name: "bad check interval",
			body: minimalConfig + "CheckIntervalSeconds: 1\n",
			want: "CheckIntervalSeconds must be between",
		},
		{
			name: "regex without group",
			body: minimalConfig + "UsernameRegex: 'email: \\S+'\n",
			want: "at least one capture group",
		},
		{
			name: "invalid regex",
			body: minimalConfig + "UsernameRegex: '('\n",
			want: "invalid UsernameRegex",
		},
		{
			name: "admin token missing",
			body: minimalConfig + "SendAdminMessage: true\nAdminChatID: '1'\n",
			want: "AdminBotToken is required",
		},
		{
			name: "user token missing",
			body: minimalConfig + "SendUserMessage: true\n",
			want: "BotToken is required",
		},
		{
			name: "webhook url missing",
			body: minimalConfig + "SendWebhook: true\n",
			want: "WebhookURL is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadValidatesTemplates(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "admin template wrong verb",
			body: minimalConfig + `
SendAdminMessage: true
AdminBotToken: 123:abc
AdminChatID: "1"
AdminBlockTemplate: "%d"
`,
			want: "AdminBlockTemplate contains invalid",
		},
		{
			name: "webhook template invalid json",
			body: minimalConfig + `
SendWebhook: true
WebhookURL: https://example.com/hook
WebhookTemplate: '{"user":"%s","ip":"%s","server":"%s","action":"%s","ts":"%s"'
`,
			want: "WebhookTemplate must produce valid JSON",
		},
		{
			name: "user template wrong verb",
			body: minimalConfig + `
SendUserMessage: true
BotToken: 123:abc
TidRegex: 'tgid-(\d+)'
UserMessageTemplate: "%s"
`,
			want: "UserMessageTemplate contains invalid",
		},
		{
			name: "webhook verb mismatch",
			body: minimalConfig + `
SendWebhook: true
WebhookURL: https://example.com/hook
WebhookTemplate: '{"a":"%d"}'
`,
			want: "WebhookTemplate must produce valid JSON",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestWebhookTemplateDuration(t *testing.T) {
	body := minimalConfig + `
SendWebhook: true
WebhookURL: https://example.com/hook
WebhookTemplate: '{"username":"%s","ip":"%s","server":"%s","action":"%s","timestamp":"%s","duration":%d}'
`
	cfg, _, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.WebhookTemplateHasDuration {
		t.Error("six-argument webhook template was not detected")
	}

	cfg, _, err = Load(writeConfig(t, minimalConfig+`
SendWebhook: true
WebhookURL: https://example.com/hook
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebhookTemplateHasDuration {
		t.Error("five-argument default was reported as duration-aware")
	}
}

func TestLoadLegacyWarnings(t *testing.T) {
	body := minimalConfig + `
BlockMode: ufw
BypassIPS:
  - 1.2.3.4
IgnoreEmail: true
EnablePerformanceMetrics: true
`
	_, warnings, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warnings) != 4 {
		t.Fatalf("got %d warnings: %v", len(warnings), warnings)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"BlockMode is ignored", "BypassIPS is ignored", "IgnoreEmail is not supported", "EnablePerformanceMetrics"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings do not contain %q: %v", want, warnings)
		}
	}
}

func TestLoadTemplatePercentLiteral(t *testing.T) {
	// Go's fmt requires %% for a literal percent sign; a single % must be
	// rejected rather than producing a broken message at runtime.
	body := minimalConfig + `
SendAdminMessage: true
AdminBotToken: 123:abc
AdminChatID: "1"
AdminBlockTemplate: "100% blocked: %s %s %s %s"
`
	_, _, err := Load(writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "AdminBlockTemplate") {
		t.Fatalf("error = %v, want AdminBlockTemplate error", err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, warnings, err := Load("../config.yaml.example")
	if err != nil {
		t.Fatalf("config.yaml.example does not load: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("example config produced warnings: %v", warnings)
	}
	if cfg.PasarGuard.Identifier != "auto" {
		t.Errorf("example identifier = %q", cfg.PasarGuard.Identifier)
	}
	if !cfg.SendAdminMessage {
		t.Error("example should enable admin messages")
	}
}

func TestHostnameFallbackIsNotEmpty(t *testing.T) {
	cfg, _, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname == "" {
		t.Fatal("hostname must not be empty")
	}
}

func TestSecurityWarnings(t *testing.T) {
	httpConfig := strings.Replace(minimalConfig, "https://panel.example.com", "http://panel.example.com", 1)
	_, warnings, err := Load(writeConfig(t, httpConfig))
	if err != nil {
		t.Fatal(err)
	}
	if !containsWarning(warnings, "PasarGuard.BaseURL uses http://") {
		t.Errorf("missing cleartext warning: %v", warnings)
	}

	loopback := strings.Replace(minimalConfig, "https://panel.example.com", "http://127.0.0.1:8001", 1)
	_, warnings, err = Load(writeConfig(t, loopback))
	if err != nil {
		t.Fatal(err)
	}
	if containsWarning(warnings, "cleartext") {
		t.Errorf("loopback endpoint should not warn: %v", warnings)
	}

	insecure := minimalConfig + "  InsecureSkipVerify: true\n"
	_, warnings, err = Load(writeConfig(t, insecure))
	if err != nil {
		t.Fatal(err)
	}
	if !containsWarning(warnings, "InsecureSkipVerify") {
		t.Errorf("missing TLS warning: %v", warnings)
	}
}

func TestEmbeddedCredentialsRejected(t *testing.T) {
	body := strings.Replace(minimalConfig, "https://panel.example.com", "https://user:pass@panel.example.com", 1)
	if _, _, err := Load(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "must not contain credentials") {
		t.Fatalf("error = %v", err)
	}
}

func containsWarning(warnings []string, needle string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, needle) {
			return true
		}
	}
	return false
}

func TestConfigPermissionWarning(t *testing.T) {
	body := strings.Replace(minimalConfig, "pg_key_00000000-0000-4000-8000-000000000000", "pg_key_real_looking_key", 1)
	path := writeConfig(t, body)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, warnings, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !containsWarning(warnings, "readable by group/others") {
		t.Errorf("missing permission warning: %v", warnings)
	}
}

func TestLegacyDefaultRegexMigrated(t *testing.T) {
	body := minimalConfig + "UsernameRegex: 'email:\\s*(\\S+)'\n"
	cfg, warnings, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UsernameRegex != DefaultUsernameRegex {
		t.Fatalf("UsernameRegex = %q, want %q", cfg.UsernameRegex, DefaultUsernameRegex)
	}
	if !containsWarning(warnings, "legacy unanchored default") {
		t.Errorf("missing migration warning: %v", warnings)
	}
}

func TestDetectionToleranceDefaults(t *testing.T) {
	cfg, _, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DetectionThreshold != DefaultDetectionThreshold {
		t.Errorf("DetectionThreshold = %d, want %d", cfg.DetectionThreshold, DefaultDetectionThreshold)
	}
	if cfg.DetectionWindowMinutes != DefaultDetectionWindowMinutes {
		t.Errorf("DetectionWindowMinutes = %d, want %d", cfg.DetectionWindowMinutes, DefaultDetectionWindowMinutes)
	}
}

func TestDetectionToleranceConfigured(t *testing.T) {
	body := minimalConfig + "DetectionThreshold: 10\nDetectionWindowMinutes: 5\n"
	cfg, _, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DetectionThreshold != 10 || cfg.DetectionWindowMinutes != 5 {
		t.Errorf("tolerance = %d/%d", cfg.DetectionThreshold, cfg.DetectionWindowMinutes)
	}
}

func TestDetectionToleranceValidation(t *testing.T) {
	body := minimalConfig + "DetectionThreshold: 1001\nDetectionWindowMinutes: 1441\n"
	if _, _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestFailsafeDefaults(t *testing.T) {
	cfg, _, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Failsafe.Enabled {
		t.Error("Failsafe must default to disabled")
	}
	if cfg.Failsafe.AfterMinutes != DefaultFailsafeAfterMinutes {
		t.Errorf("AfterMinutes = %d", cfg.Failsafe.AfterMinutes)
	}
	if cfg.Failsafe.CooldownMinutes != DefaultFailsafeCooldownMinutes {
		t.Errorf("CooldownMinutes = %d", cfg.Failsafe.CooldownMinutes)
	}
	if len(cfg.Whitelist) != 0 {
		t.Errorf("Whitelist = %v", cfg.Whitelist)
	}
}

func TestFailsafeConfigured(t *testing.T) {
	body := minimalConfig + `Failsafe:
  Enabled: true
  AfterMinutes: 20
  CooldownMinutes: 45
  IncludePreExisting: true
Whitelist:
  - "42"
  - " alice "
  - ""
`
	cfg, _, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Failsafe.Enabled || cfg.Failsafe.AfterMinutes != 20 || cfg.Failsafe.CooldownMinutes != 45 || !cfg.Failsafe.IncludePreExisting {
		t.Errorf("Failsafe = %+v", cfg.Failsafe)
	}
	if len(cfg.Whitelist) != 2 || cfg.Whitelist[0] != "42" || cfg.Whitelist[1] != "alice" {
		t.Errorf("Whitelist = %#v", cfg.Whitelist)
	}
}

func TestFailsafeValidation(t *testing.T) {
	body := minimalConfig + `Failsafe:
  Enabled: true
  AfterMinutes: 1441
  CooldownMinutes: 10081
`
	if _, _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected validation error")
	}
}
