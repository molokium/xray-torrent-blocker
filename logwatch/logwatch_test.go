package logwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"tblocker/config"
)

func testConfig() *config.Config {
	return &config.Config{
		TorrentTag:    "TORRENT",
		UsernameRegex: config.DefaultUsernameRegex,
		UsernameRE:    regexp.MustCompile(config.DefaultUsernameRegex),
		TidRE:         regexp.MustCompile(`tgid-(-?[0-9]+)`),
	}
}

func TestParseLine(t *testing.T) {
	cases := []struct {
		name           string
		line           string
		cfg            func() *config.Config
		wantIdentity   string
		wantIP         string
		wantTelegramID string
		wantErr        error
	}{
		{
			name:         "ipv4 numeric user id",
			line:         `2026/10/31 12:00:00 from 203.0.113.9:4567 accepted tcp:tracker.example.com:443 [vless-in >> TORRENT] email: 42`,
			wantIdentity: "42",
			wantIP:       "203.0.113.9",
		},
		{
			name:           "username with telegram id",
			line:           `2026/10/31 12:00:00 from 198.51.100.4:1234 accepted tcp:tracker.example.com:443 [in >> TORRENT] email: alice_tgid-777`,
			wantIdentity:   "alice_tgid-777",
			wantIP:         "198.51.100.4",
			wantTelegramID: "777",
		},
		{
			name:           "negative telegram id",
			line:           `from 198.51.100.4:1234 [TORRENT] email: alice_tgid--100123`,
			wantIdentity:   "alice_tgid--100123",
			wantIP:         "198.51.100.4",
			wantTelegramID: "-100123",
		},
		{
			name:         "ipv6 source",
			line:         `2026/10/31 12:00:00 from [2001:db8::1]:443 accepted tcp:x:1 [in >> TORRENT] email: 9`,
			wantIdentity: "9",
			wantIP:       "2001:db8::1",
		},
		{
			name:         "tcp prefixed source",
			line:         `2026/10/31 12:00:00 from tcp:192.0.2.10:9999 accepted tcp:x:1 [in >> TORRENT] email: 10`,
			wantIdentity: "10",
			wantIP:       "192.0.2.10",
		},
		{
			name:         "missing source ip still identifies user",
			line:         `2026/10/31 12:00:00 accepted tcp:x:1 [in >> TORRENT] email: 11`,
			wantIdentity: "11",
		},
		{
			name:         "crlf",
			line:         "2026/10/31 12:00:00 from 203.0.113.1:1 accepted [TORRENT] email: 12\r\n",
			wantIdentity: "12",
			wantIP:       "203.0.113.1",
		},
		{
			name:    "no torrent tag",
			line:    `2026/10/31 12:00:00 from 203.0.113.1:1 accepted [in >> direct] email: 12`,
			wantErr: ErrNoTorrentTag,
		},
		{
			name:    "torrent tag but no email",
			line:    `2026/10/31 12:00:00 from 203.0.113.1:1 accepted [in >> TORRENT]`,
			wantErr: ErrNoIdentity,
		},
		{
			name:    "empty email",
			line:    `2026/10/31 12:00:00 [TORRENT] email: `,
			wantErr: ErrNoIdentity,
		},
		{
			name: "custom regex",
			cfg: func() *config.Config {
				c := testConfig()
				c.UsernameRegex = `user=(\S+)`
				c.UsernameRE = regexp.MustCompile(c.UsernameRegex)
				return c
			},
			line:         `from 203.0.113.5:1 [TORRENT] user=charlie`,
			wantIdentity: "charlie",
			wantIP:       "203.0.113.5",
		},
		{
			name:         "quoted email",
			line:         `from 203.0.113.5:1 [TORRENT] email: "dave"`,
			wantIdentity: "dave",
			wantIP:       "203.0.113.5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			if tc.cfg != nil {
				cfg = tc.cfg()
			}
			det, err := ParseLine(cfg, tc.line)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLine: %v", err)
			}
			if det.Identity != tc.wantIdentity {
				t.Errorf("Identity = %q, want %q", det.Identity, tc.wantIdentity)
			}
			if det.IP != tc.wantIP {
				t.Errorf("IP = %q, want %q", det.IP, tc.wantIP)
			}
			if det.TelegramID != tc.wantTelegramID {
				t.Errorf("TelegramID = %q, want %q", det.TelegramID, tc.wantTelegramID)
			}
		})
	}
}

func TestParseLineIPFallback(t *testing.T) {
	cfg := testConfig()
	// A malformed "from" token falls back to the first valid IPv4 literal.
	det, err := ParseLine(cfg, `from not-an-address accepted [TORRENT] email: 5 for 198.51.100.77:443`)
	if err != nil {
		t.Fatal(err)
	}
	if det.IP != "198.51.100.77" {
		t.Errorf("IP = %q", det.IP)
	}
}

func TestMatchTid(t *testing.T) {
	re := regexp.MustCompile(`tgid-(-?[0-9]+)`)
	cases := map[string]string{
		"tgid-123":                      "123",
		"tgid--100123":                  "-100123",
		"prefix tgid-9 x":               "9",
		"tgid-abc":                      "",
		"tgid-999999999999999999999999": "",
		"":                              "",
	}
	for input, want := range cases {
		if got := MatchTid(re, input); got != want {
			t.Errorf("MatchTid(%q) = %q, want %q", input, got, want)
		}
	}
	if got := MatchTid(nil, "tgid-1"); got != "" {
		t.Errorf("MatchTid(nil) = %q", got)
	}
}

func TestFollowAppends(t *testing.T) {
	for _, poll := range []bool{false, true} {
		t.Run(fmt.Sprintf("poll=%t", poll), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "access.log")
			if err := os.WriteFile(path, []byte("old line\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lines := make(chan string, 16)
			done := make(chan error, 1)
			go func() {
				done <- Follow(ctx, path, poll, func(line string) {
					lines <- line
				})
			}()

			// Give the watcher a moment to attach, then append.
			time.Sleep(300 * time.Millisecond)
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = file.WriteString("first new line\nsecond new line\n")
			_ = file.Close()

			for i := 0; i < 2; i++ {
				select {
				case got := <-lines:
					if !strings.Contains(got, "new line") {
						t.Errorf("line %d = %q", i, got)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("timed out waiting for line %d", i)
				}
			}

			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Follow returned %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Follow did not stop after cancellation")
			}
		})
	}
}

func TestFollowWaitsForNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "later.log")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- Follow(ctx, path, false, func(line string) { lines <- line })
	}()

	time.Sleep(500 * time.Millisecond)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	// tail seeks to the end when it opens the newly created file, so append
	// after it has had time to attach.
	time.Sleep(1 * time.Second)
	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("created now\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	select {
	case got := <-lines:
		if !strings.Contains(got, "created now") {
			t.Errorf("line = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a newly created file")
	}

	cancel()
	<-done
}

func TestFastEmailValueEquivalence(t *testing.T) {
	re := regexp.MustCompile(config.DefaultUsernameRegex)
	lines := []string{
		`2026/10/31 12:00:00 from 203.0.113.9:4567 accepted tcp:x:443 [in >> TORRENT] email: 42`,
		`... [TORRENT] email: alice_tgid-777`,
		`... [TORRENT] email:"quoted"`,
		`... [TORRENT] email:`,
		`... [TORRENT]`,
		`email: 7`,
	}
	for _, line := range lines {
		want := ""
		if match := re.FindStringSubmatch(line); len(match) >= 2 {
			want = strings.Trim(strings.TrimSpace(match[1]), `"'`)
		}
		if got := fastEmailValue(line); got != want {
			t.Errorf("fastEmailValue(%q) = %q, want %q", line, got, want)
		}
	}
}

func BenchmarkParseLineDefault(b *testing.B) {
	cfg := testConfig()
	line := `2026/10/31 12:00:00 from 203.0.113.9:4567 accepted tcp:tracker.example.com:443 [vless-in >> TORRENT] email: 42`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseLine(cfg, line); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseLineCustomRegex(b *testing.B) {
	cfg := testConfig()
	cfg.UsernameRegex = `^.*email:\s*(\S+)$`
	cfg.UsernameRE = regexp.MustCompile(cfg.UsernameRegex)
	line := `2026/10/31 12:00:00 from 203.0.113.9:4567 accepted tcp:tracker.example.com:443 [vless-in >> TORRENT] email: 42`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseLine(cfg, line); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFastEmailValue(b *testing.B) {
	line := `2026/10/31 12:00:00 from 203.0.113.9:4567 accepted tcp:tracker.example.com:443 [vless-in >> TORRENT] email: 42`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if fastEmailValue(line) == "" {
			b.Fatal("no value")
		}
	}
}

func BenchmarkDefaultEmailRegex(b *testing.B) {
	re := regexp.MustCompile(config.DefaultUsernameRegex)
	line := `2026/10/31 12:00:00 from 203.0.113.9:4567 accepted tcp:tracker.example.com:443 [vless-in >> TORRENT] email: 42`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if len(re.FindStringSubmatch(line)) < 2 {
			b.Fatal("no match")
		}
	}
}

func FuzzParseLine(f *testing.F) {
	cfg := testConfig()
	f.Add(`2026/10/31 12:00:00 from 203.0.113.9:4567 accepted tcp:x:443 [in >> TORRENT] email: 42`)
	f.Add(`from [2001:db8::1]:443 [TORRENT] email: alice`)
	f.Add(`[TORRENT] email: `)
	f.Add(`no tag`)
	f.Fuzz(func(t *testing.T, line string) {
		_, _ = ParseLine(cfg, line)
	})
}

func TestFollowHandlesTruncation(t *testing.T) {
	for _, poll := range []bool{false, true} {
		t.Run(fmt.Sprintf("poll=%t", poll), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "access.log")
			if err := os.WriteFile(path, []byte("before start\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lines := make(chan string, 8)
			done := make(chan error, 1)
			go func() {
				done <- Follow(ctx, path, poll, func(line string) { lines <- line })
			}()

			time.Sleep(300 * time.Millisecond)
			appendLine := func(text string) {
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = file.WriteString(text)
				_ = file.Close()
			}

			appendLine("first\n")
			select {
			case got := <-lines:
				if !strings.Contains(got, "first") {
					t.Fatalf("line = %q", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the pre-truncation line")
			}

			if err := os.Truncate(path, 0); err != nil {
				t.Fatal(err)
			}
			// Let the watcher observe the size drop before new content arrives.
			time.Sleep(500 * time.Millisecond)
			appendLine("second\n")

			select {
			case got := <-lines:
				if !strings.Contains(got, "second") {
					t.Fatalf("post-truncation line = %q", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for a line after truncation")
			}

			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Follow returned %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Follow did not stop")
			}
		})
	}
}

func TestFollowDoesNotEmitPartialLine(t *testing.T) {
	for _, poll := range []bool{false, true} {
		t.Run(fmt.Sprintf("poll=%t", poll), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "access.log")
			if err := os.WriteFile(path, []byte("start\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lines := make(chan string, 8)
			done := make(chan error, 1)
			go func() {
				done <- Follow(ctx, path, poll, func(line string) { lines <- line })
			}()
			time.Sleep(300 * time.Millisecond)

			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("... email: 4"); err != nil {
				t.Fatal(err)
			}
			_ = file.Close()

			select {
			case got := <-lines:
				t.Fatalf("partial line was emitted: %q", got)
			case <-time.After(500 * time.Millisecond):
			}

			file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("2\n"); err != nil {
				t.Fatal(err)
			}
			_ = file.Close()

			select {
			case got := <-lines:
				if !strings.Contains(got, "email: 42") {
					t.Fatalf("completed line = %q", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the completed line")
			}

			cancel()
			<-done
		})
	}
}

func TestParseLineIgnoresEmailInDestination(t *testing.T) {
	cfg := testConfig()

	// A destination host literally named "email" on port 5 produces
	// "tcp:email:5"; the real user field comes later and must win.
	line := `2026/10/31 12:00:00 from 203.0.113.9:4567 accepted tcp:email:5 [in >> TORRENT] email: 42`
	det, err := ParseLine(cfg, line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if det.Identity != "42" {
		t.Fatalf("Identity = %q, want 42", det.Identity)
	}

	// Without a real user field, the destination must not become an identity.
	line = `2026/10/31 12:00:00 from 203.0.113.9:4567 accepted tcp:email:5 [in >> TORRENT]`
	if _, err := ParseLine(cfg, line); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("error = %v, want ErrNoIdentity", err)
	}
}

func TestTelegramIDComesFromIdentityNotDestination(t *testing.T) {
	cfg := testConfig()

	line := `2026/10/31 12:00:00 from 203.0.113.9:1 accepted tcp:tgid-999:443 [in >> TORRENT] email: 42`
	det, err := ParseLine(cfg, line)
	if err != nil {
		t.Fatal(err)
	}
	if det.TelegramID != "" {
		t.Fatalf("TelegramID was taken from the destination: %q", det.TelegramID)
	}

	line = `2026/10/31 12:00:00 from 203.0.113.9:1 accepted tcp:x:443 [in >> TORRENT] email: alice_tgid-777`
	det, err = ParseLine(cfg, line)
	if err != nil {
		t.Fatal(err)
	}
	if det.TelegramID != "777" {
		t.Fatalf("TelegramID = %q, want 777", det.TelegramID)
	}
}
