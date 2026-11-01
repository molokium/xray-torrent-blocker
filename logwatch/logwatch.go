// Package logwatch parses Xray access log lines and follows the log file.
package logwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strings"

	"github.com/nxadm/tail"

	"tblocker/config"
)

// ErrNoTorrentTag means the line is unrelated to torrent traffic.
var ErrNoTorrentTag = errors.New("line does not contain TorrentTag")

// ErrNoIdentity means the torrent line had no usable email field.
var ErrNoIdentity = errors.New("line has no PasarGuard identity (email) field")

// Detection is one torrent event extracted from the access log.
type Detection struct {
	// Identity is the captured email value: a PasarGuard user ID or username.
	Identity string
	// IP is the source address, when it could be parsed. It is informational.
	IP string
	// Inbound is the Xray inbound tag from the "[inbound >> outbound]" field.
	// It is informational and may be empty on an unrecognized log format.
	Inbound string
	// TelegramID is an optional chat ID found in the identity/email value.
	// Panel-controlled username and note are checked later as fallbacks.
	TelegramID string
	Raw        string
}

var (
	ipv4Pattern = regexp.MustCompile(`(?:[0-9]{1,3}\.){3}[0-9]{1,3}`)
	tidPattern  = regexp.MustCompile(`^-?[0-9]{1,20}$`)
)

// ParseLine extracts a torrent detection from one access log line.
func ParseLine(cfg *config.Config, line string) (Detection, error) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.Contains(line, cfg.TorrentTag) {
		return Detection{}, ErrNoTorrentTag
	}

	identity := ""
	if cfg.UsernameRE != nil {
		// The default PasarGuard pattern is a simple "email: <token>" lookup.
		// Avoid the regexp engine on the hot path; custom patterns still use it.
		if cfg.UsernameRE.String() == config.DefaultUsernameRegex {
			identity = fastEmailValue(line)
		} else if match := cfg.UsernameRE.FindStringSubmatch(line); len(match) >= 2 {
			identity = strings.Trim(strings.TrimSpace(match[1]), `"'`)
		}
	}
	if identity == "" {
		return Detection{Raw: line}, ErrNoIdentity
	}

	det := Detection{
		Identity:   identity,
		IP:         sourceIP(line),
		Inbound:    inboundTag(line, cfg.TorrentTag),
		TelegramID: MatchTid(cfg.TidRE, identity),
		Raw:        line,
	}
	return det, nil
}

// fastEmailValue extracts the first whitespace-delimited "email:" field
// without invoking the regexp engine. It mirrors the default UsernameRegex,
// including the requirement for whitespace on both sides, so a destination
// such as "tcp:email:5" cannot be mistaken for the user field.
func fastEmailValue(line string) string {
	const marker = "email:"
	for offset := 0; offset < len(line); {
		rel := strings.Index(line[offset:], marker)
		if rel < 0 {
			return ""
		}
		idx := offset + rel
		if idx > 0 && (line[idx-1] == ' ' || line[idx-1] == '	') {
			i := idx + len(marker)
			if i < len(line) && (line[i] == ' ' || line[i] == '	') {
				for i < len(line) && (line[i] == ' ' || line[i] == '	') {
					i++
				}
				start := i
				for i < len(line) && line[i] != ' ' && line[i] != '	' && line[i] != '\r' && line[i] != '\n' {
					i++
				}
				if i > start {
					return strings.Trim(line[start:i], `"'`)
				}
			}
		}
		offset = idx + len(marker)
	}
	return ""
}

// MatchTid applies a Telegram-ID regexp to text and returns only a plausible
// numeric chat ID.
func MatchTid(re *regexp.Regexp, text string) string {
	if re == nil || text == "" {
		return ""
	}
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return ""
	}
	value := strings.TrimSpace(match[1])
	if !tidPattern.MatchString(value) {
		return ""
	}
	return value
}

// inboundTag extracts the inbound name from Xray's routing field:
//
//	[vless-in >> TORRENT]
//
// The outbound must contain TorrentTag. Bracket fields such as an IPv6 source
// address are skipped because they have no ">>" separator.
func inboundTag(line, torrentTag string) string {
	for offset := 0; offset < len(line); {
		open := strings.IndexByte(line[offset:], '[')
		if open < 0 {
			return ""
		}
		open += offset
		close := strings.IndexByte(line[open+1:], ']')
		if close < 0 {
			return ""
		}
		close += open + 1
		inner := line[open+1 : close]
		if sep := strings.Index(inner, ">>"); sep >= 0 {
			inbound := strings.TrimSpace(inner[:sep])
			outbound := strings.TrimSpace(inner[sep+2:])
			if inbound != "" && strings.Contains(outbound, torrentTag) {
				return strings.Trim(strings.TrimSpace(inbound), `"'`)
			}
		}
		offset = close + 1
	}
	return ""
}

// sourceIP finds the Xray "from <addr>:<port>" source address. Because the
// address is only used in notifications, a missing value is not fatal.
func sourceIP(line string) string {
	const marker = " from "
	if idx := strings.Index(line, marker); idx >= 0 {
		rest := strings.TrimLeft(line[idx+len(marker):], " \t")
		field := rest
		if end := strings.IndexAny(field, " \t"); end >= 0 {
			field = field[:end]
		}
		field = strings.TrimSuffix(field, "\r")
		field = strings.TrimPrefix(field, "tcp:")
		field = strings.TrimPrefix(field, "udp:")
		if ip := parseAddr(field); ip != "" {
			return ip
		}
	}

	// Fallback: the first valid IPv4 literal. Xray puts the source first, so
	// this is normally the same address.
	for _, candidate := range ipv4Pattern.FindAllString(line, -1) {
		if parsed := net.ParseIP(candidate); parsed != nil {
			return parsed.String()
		}
	}
	return ""
}

func parseAddr(value string) string {
	if value == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return host
		}
	}
	trimmed := strings.Trim(value, "[]")
	if ip := net.ParseIP(trimmed); ip != nil {
		return trimmed
	}
	return ""
}

// Follow streams new lines appended to path until ctx is cancelled. When poll
// is true it uses stat polling instead of inotify, which is slower but works on
// filesystems where inotify events are unreliable (some bind mounts and network
// filesystems).
func Follow(ctx context.Context, path string, poll bool, onLine func(string)) error {
	t, err := tail.TailFile(path, tail.Config{
		Follow:        true,
		ReOpen:        true,
		Poll:          poll,
		CompleteLines: true, // never hand a half-line (e.g. a cut email) to the parser
		Location:      &tail.SeekInfo{Offset: 0, Whence: io.SeekEnd},
		MustExist:     false,
		Logger:        log.New(log.Writer(), "tail: ", log.LstdFlags),
	})
	if err != nil {
		return fmt.Errorf("open log file %s: %w", path, err)
	}
	defer func() {
		_ = t.Stop()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-t.Lines:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("log tail stopped unexpectedly")
			}
			if line.Err != nil {
				log.Printf("logwatch: read error: %v", line.Err)
				continue
			}
			onLine(line.Text)
		}
	}
}
