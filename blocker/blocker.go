// Package blocker applies and lifts PasarGuard account blocks.
package blocker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"tblocker/config"
	"tblocker/logwatch"
	"tblocker/notify"
	"tblocker/panel"
	"tblocker/storage"
)

// Panel is the subset of the PasarGuard API used by the blocker.
type Panel interface {
	GetUserByID(ctx context.Context, id int) (*panel.User, error)
	GetUserByUsername(ctx context.Context, username string) (*panel.User, error)
	SetDisabledByID(ctx context.Context, id int, disabled bool) (*panel.User, error)
	SetDisabledByUsername(ctx context.Context, username string, disabled bool) (*panel.User, error)
}

// Notifier receives block and unblock events.
type Notifier interface {
	Block(event notify.Event)
	Unblock(event notify.Event)
}

// Manager owns the block lifecycle. All state transitions are serialised by mu
// so that a log line and the expiry worker cannot race on the same account.
type Manager struct {
	cfg      *config.Config
	store    *storage.Store
	panel    Panel
	notifier Notifier
	now      func() time.Time
	logger   *log.Logger
	mu       sync.Mutex

	// identityCache short-circuits repeated detections for a user that is
	// already blocked. It is keyed by "id:<n>" and "username:<name>" and is
	// rebuilt from the journal at startup.
	identityCache map[string]cacheEntry
}

type cacheEntry struct {
	Key   string
	Until time.Time
}

// Option customises a Manager.
type Option func(*Manager)

// WithClock injects a clock (tests).
func WithClock(now func() time.Time) Option {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

// WithLogger injects a logger.
func WithLogger(logger *log.Logger) Option {
	return func(m *Manager) {
		if logger != nil {
			m.logger = logger
		}
	}
}

// New creates a Manager.
func New(cfg *config.Config, store *storage.Store, panelClient Panel, notifier Notifier, opts ...Option) *Manager {
	m := &Manager{
		cfg:           cfg,
		store:         store,
		panel:         panelClient,
		notifier:      notifier,
		now:           time.Now,
		logger:        log.New(log.Writer(), "blocker: ", log.LstdFlags),
		identityCache: make(map[string]cacheEntry),
	}
	for _, opt := range opts {
		opt(m)
	}
	m.rebuildIdentityCache()
	return m
}

// Handle applies a block for one torrent detection. It is safe to call from a
// single log-reading goroutine; internal locking also makes scheduler overlap
// safe.
func (m *Manager) Handle(ctx context.Context, det logwatch.Detection) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	duration := time.Duration(m.cfg.BlockDuration) * time.Minute

	// A torrent client opens many connections. Once an account is blocked,
	// skip repeats without asking the panel again.
	if cacheKey := m.detectionCacheKey(det.Identity); cacheKey != "" {
		if entry, ok := m.identityCache[cacheKey]; ok {
			if now.Before(entry.Until) {
				m.logf("identity %s is already blocked until %s; skipping detection",
					det.Identity, entry.Until.Format(time.RFC3339))
				return nil
			}
			delete(m.identityCache, cacheKey)
		}
	}

	user, err := m.resolve(ctx, det.Identity)
	if err != nil {
		return fmt.Errorf("resolve user %q: %w", det.Identity, err)
	}
	key, err := userKey(user)
	if err != nil {
		return err
	}

	if existing, ok := m.store.Get(key); ok {
		if now.Before(existing.BlockedUntil) {
			m.logf("user %s (id %d) is already blocked until %s; skipping duplicate detection",
				displayName(user), user.ID, existing.BlockedUntil.Format(time.RFC3339))
			return nil
		}

		if user.Status == panel.StatusDisabled {
			// The previous block expired but the account is still disabled,
			// either because the unblock call failed or because an
			// administrator disabled it. Re-arm without another API call.
			existing.BlockedAt = now
			existing.BlockedUntil = now.Add(duration)
			existing.IP = det.IP
			existing.State = storage.StateActive
			existing.Notified = true
			existing.Attempts = 0
			existing.NextAttempt = time.Time{}
			if det.TelegramID != "" {
				existing.TelegramID = det.TelegramID
			}
			if err := m.store.Put(existing); err != nil {
				return fmt.Errorf("persist re-armed block for %s: %w", displayName(user), err)
			}
			m.rememberBlockLocked(existing)
			m.notifyBlock(existing, user)
			m.logf("re-armed block for user %s (id %d) until %s (already disabled in panel)",
				displayName(user), user.ID, existing.BlockedUntil.Format(time.RFC3339))
			return nil
		}

		if err := m.store.Remove(key); err != nil {
			return fmt.Errorf("remove expired block record for %s: %w", displayName(user), err)
		}
		m.forgetBlockLocked(existing)
	}

	if user.Status == panel.StatusDisabled {
		telegramID := telegramIDFor(m.cfg, det, user)
		record := storage.Block{
			Key:                 key,
			UserID:              user.ID,
			Username:            user.Username,
			IP:                  det.IP,
			TelegramID:          telegramID,
			BlockedAt:           now,
			BlockedUntil:        now.Add(duration),
			PreviousStatus:      user.Status,
			PreExistingDisabled: true,
			State:               storage.StateActive,
			Notified:            true,
		}
		if err := m.store.Put(record); err != nil {
			return fmt.Errorf("persist block for already-disabled user %s: %w", displayName(user), err)
		}
		m.rememberBlockLocked(record)
		m.notifyBlock(record, user)
		m.logf("user %s (id %d) was already disabled; recorded block until %s without changing the panel",
			displayName(user), user.ID, record.BlockedUntil.Format(time.RFC3339))
		return nil
	}

	telegramID := telegramIDFor(m.cfg, det, user)
	record := storage.Block{
		Key:            key,
		UserID:         user.ID,
		Username:       user.Username,
		IP:             det.IP,
		TelegramID:     telegramID,
		BlockedAt:      now,
		BlockedUntil:   now.Add(duration),
		PreviousStatus: user.Status,
		State:          storage.StatePending,
	}

	// Journal the intent before calling the API. If the process dies after the
	// API call but before the response is persisted, the pending record is
	// reconciled on the next start instead of leaving the user disabled.
	if err := m.store.Put(record); err != nil {
		return fmt.Errorf("persist pending block for %s: %w", displayName(user), err)
	}

	updated, err := m.setDisabled(ctx, user, true)
	if err != nil {
		if panel.IsDefinitive(err) {
			if removeErr := m.store.Remove(key); removeErr != nil {
				m.logf("could not remove failed block record for %s: %v", displayName(user), removeErr)
			} else {
				m.forgetBlockLocked(record)
			}
			return fmt.Errorf("disable user %s: %w", displayName(user), err)
		}
		return fmt.Errorf("disable user %s (outcome unknown; will reconcile): %w", displayName(user), err)
	}
	if updated == nil {
		return fmt.Errorf("disable user %s: panel returned an empty response", displayName(user))
	}
	if updated.Status != panel.StatusDisabled {
		return fmt.Errorf("disable user %s: panel returned status %q", displayName(user), updated.Status)
	}
	record.State = storage.StateActive
	record.Notified = true
	record.Username = firstNonEmpty(updated.Username, record.Username)
	record.UserID = firstNonZero(updated.ID, record.UserID)
	if err := m.store.Put(record); err != nil {
		return fmt.Errorf("persist active block for %s: %w", displayName(user), err)
	}

	m.rememberBlockLocked(record)
	m.notifyBlock(record, user)
	m.logf("blocked user %s (id %d, ip %s) until %s",
		displayName(user), user.ID, displayIP(det.IP), record.BlockedUntil.Format(time.RFC3339))
	return nil
}

// Run reconciles pending records and lifts expired blocks until ctx is done.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	m.ProcessExpired(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.ProcessExpired(ctx)
		}
	}
}

// ProcessExpired reconciles pending records and unblocks expired accounts.
func (m *Manager) ProcessExpired(ctx context.Context) {
	// Snapshot keys first; each record is re-read under the lock so a block
	// created while this loop runs is never lifted accidentally.
	for _, snapshot := range m.store.List() {
		if ctx.Err() != nil {
			return
		}
		m.processOne(ctx, snapshot.Key)
	}
}

func (m *Manager) processOne(ctx context.Context, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	record, ok := m.store.Get(key)
	if !ok {
		return
	}
	now := m.now()

	if record.State == storage.StatePending {
		if !record.NextAttempt.IsZero() && now.Before(record.NextAttempt) {
			return
		}
		m.reconcilePending(ctx, record)
		return
	}
	if now.Before(record.BlockedUntil) {
		return
	}
	if !record.NextAttempt.IsZero() && now.Before(record.NextAttempt) {
		return
	}
	m.unblock(ctx, record)
}

func (m *Manager) reconcilePending(ctx context.Context, record storage.Block) {
	user, err := m.getRecordUser(ctx, record)
	if err != nil {
		if panel.IsNotFound(err) {
			if removeErr := m.store.Remove(record.Key); removeErr != nil {
				m.logf("could not remove pending record for %s: %v", displayName(recordUser(record)), removeErr)
			} else {
				m.forgetBlockLocked(record)
			}
			m.logf("pending block for %s: user no longer exists; record removed", displayName(recordUser(record)))
			return
		}
		m.recordFailure(record, fmt.Errorf("reconcile pending block: %w", err))
		return
	}

	if user.Status != panel.StatusDisabled {
		if removeErr := m.store.Remove(record.Key); removeErr != nil {
			m.logf("could not remove ineffective pending record for %s: %v", displayName(user), removeErr)
		} else {
			m.forgetBlockLocked(record)
		}
		m.logf("pending block for %s did not take effect (panel status %q); record removed",
			displayName(user), user.Status)
		return
	}

	record.State = storage.StateActive
	record.UserID = firstNonZero(user.ID, record.UserID)
	record.Username = firstNonEmpty(user.Username, record.Username)
	if record.PreviousStatus == "" {
		record.PreviousStatus = panel.StatusActive
	}
	record.Notified = true
	record.Attempts = 0
	record.NextAttempt = time.Time{}
	if err := m.store.Put(record); err != nil {
		m.logf("could not persist reconciled block for %s: %v", displayName(user), err)
		return
	}
	m.rememberBlockLocked(record)
	m.notifyBlock(record, user)
	m.logf("reconciled pending block for user %s (id %d) until %s",
		displayName(user), user.ID, record.BlockedUntil.Format(time.RFC3339))
}

func (m *Manager) unblock(ctx context.Context, record storage.Block) {
	if record.PreExistingDisabled {
		if err := m.store.Remove(record.Key); err != nil {
			m.logf("could not remove expired pre-existing-disabled record for %s: %v", displayName(recordUser(record)), err)
			return
		}
		m.forgetBlockLocked(record)
		m.logf("block expired for %s, which was already disabled before the block; leaving it disabled",
			displayName(recordUser(record)))
		return
	}

	if !m.cfg.PasarGuard.ReenableOnUnblockEnabled() {
		if err := m.store.Remove(record.Key); err != nil {
			m.logf("could not remove expired record for %s: %v", displayName(recordUser(record)), err)
			return
		}
		m.forgetBlockLocked(record)
		m.logf("block expired for %s; ReenableOnUnblock is false, leaving the account disabled",
			displayName(recordUser(record)))
		return
	}

	user, err := m.getRecordUser(ctx, record)
	if err != nil {
		if panel.IsNotFound(err) {
			if removeErr := m.store.Remove(record.Key); removeErr != nil {
				m.logf("could not remove record for deleted user %s: %v", displayName(recordUser(record)), removeErr)
			} else {
				m.forgetBlockLocked(record)
			}
			m.logf("block expired for %s, but the user no longer exists; record removed", displayName(recordUser(record)))
			return
		}
		m.recordFailure(record, fmt.Errorf("load user for unblock: %w", err))
		return
	}

	if user.Status != panel.StatusDisabled {
		if removeErr := m.store.Remove(record.Key); removeErr != nil {
			m.logf("could not remove stale record for %s: %v", displayName(user), removeErr)
		} else {
			m.forgetBlockLocked(record)
		}
		m.logf("block expired for %s, but panel status is %q (changed externally); not re-enabling",
			displayName(user), user.Status)
		return
	}

	updated, err := m.setDisabled(ctx, user, false)
	if err != nil {
		m.recordFailure(record, fmt.Errorf("enable user: %w", err))
		return
	}
	if updated == nil || updated.Status == panel.StatusDisabled {
		m.recordFailure(record, errors.New("panel still reports status disabled after enable request"))
		return
	}

	if err := m.store.Remove(record.Key); err != nil {
		m.logf("user %s was enabled but the block record could not be removed: %v", displayName(user), err)
		return
	}
	m.forgetBlockLocked(record)

	event := notify.Event{
		Username:        firstNonEmpty(user.Username, record.Username),
		UserID:          firstNonZero(user.ID, record.UserID),
		IP:              record.IP,
		Hostname:        m.cfg.Hostname,
		DurationMinutes: m.cfg.BlockDuration,
		Timestamp:       m.now(),
		TelegramID:      record.TelegramID,
	}
	m.notifier.Unblock(event)
	m.logf("unblocked user %s (id %d) after %d minutes; panel status is now %q",
		displayName(user), user.ID, m.cfg.BlockDuration, updated.Status)
}

func (m *Manager) getRecordUser(ctx context.Context, record storage.Block) (*panel.User, error) {
	// In username mode the username is the configured identity. Otherwise the
	// numeric ID is authoritative. Do not fall back from a 404 by ID to the
	// username: IDs are not reused, but usernames can be, and enabling a
	// recreated account would be worse than leaving a deleted one alone.
	if m.cfg.PasarGuard.Identifier == "username" && record.Username != "" {
		return m.panel.GetUserByUsername(ctx, record.Username)
	}
	if record.UserID > 0 {
		return m.panel.GetUserByID(ctx, record.UserID)
	}
	if record.Username != "" {
		return m.panel.GetUserByUsername(ctx, record.Username)
	}
	return nil, errors.New("block record has neither a user ID nor a username")
}

func (m *Manager) setDisabled(ctx context.Context, user *panel.User, disabled bool) (*panel.User, error) {
	if m.cfg.PasarGuard.Identifier == "username" && user.Username != "" {
		return m.panel.SetDisabledByUsername(ctx, user.Username, disabled)
	}
	if user.ID > 0 {
		return m.panel.SetDisabledByID(ctx, user.ID, disabled)
	}
	if user.Username != "" {
		return m.panel.SetDisabledByUsername(ctx, user.Username, disabled)
	}
	return nil, errors.New("cannot change status: user has neither an ID nor a username")
}

func (m *Manager) resolve(ctx context.Context, identity string) (*panel.User, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return nil, errors.New("empty identity")
	}

	mode := m.cfg.PasarGuard.Identifier
	if mode == "id" {
		id, err := parseUserID(identity)
		if err != nil {
			return nil, fmt.Errorf("identifier is not a numeric user ID: %w", err)
		}
		return m.panel.GetUserByID(ctx, id)
	}

	if mode == "auto" {
		if id, err := parseUserID(identity); err == nil {
			user, err := m.panel.GetUserByID(ctx, id)
			if err == nil {
				return user, nil
			}
			if !panel.IsNotFound(err) {
				return nil, err
			}
			// A numeric PasarGuard username is legal, so fall back to it when
			// the ID does not exist.
		}
	}

	return m.panel.GetUserByUsername(ctx, identity)
}

func (m *Manager) notifyBlock(record storage.Block, user *panel.User) {
	event := notify.Event{
		Username:        firstNonEmpty(user.Username, record.Username),
		UserID:          firstNonZero(user.ID, record.UserID),
		IP:              record.IP,
		Hostname:        m.cfg.Hostname,
		DurationMinutes: m.cfg.BlockDuration,
		Timestamp:       m.now(),
		TelegramID:      record.TelegramID,
	}
	m.notifier.Block(event)
}

func (m *Manager) recordFailure(record storage.Block, cause error) {
	record.Attempts++
	delay := retryDelay(record.Attempts)
	record.NextAttempt = m.now().Add(delay)
	if err := m.store.Put(record); err != nil {
		m.logf("could not persist retry state for %s: %v", displayName(recordUser(record)), err)
	}
	m.logf("block operation for %s failed (attempt %d, retry in %s): %v",
		displayName(recordUser(record)), record.Attempts, delay.Round(time.Second), cause)
}

// rebuildIdentityCache repopulates the duplicate fast-path after a restart.
// Pending records are deliberately excluded: they may not have been applied.
func (m *Manager) rebuildIdentityCache() {
	for _, record := range m.store.List() {
		if record.State == storage.StateActive {
			m.rememberBlockLocked(record)
		}
	}
}

func (m *Manager) rememberBlockLocked(record storage.Block) {
	entry := cacheEntry{Key: record.Key, Until: record.BlockedUntil}
	if record.UserID > 0 {
		m.identityCache["id:"+strconv.Itoa(record.UserID)] = entry
	}
	if record.Username != "" {
		m.identityCache["username:"+record.Username] = entry
	}
}

func (m *Manager) forgetBlockLocked(record storage.Block) {
	for key, entry := range m.identityCache {
		if entry.Key == record.Key {
			delete(m.identityCache, key)
		}
	}
}

func (m *Manager) detectionCacheKey(identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return ""
	}
	switch m.cfg.PasarGuard.Identifier {
	case "username":
		return "username:" + identity
	case "id":
		id, err := parseUserID(identity)
		if err != nil {
			return ""
		}
		return "id:" + strconv.Itoa(id)
	default: // auto
		if id, err := parseUserID(identity); err == nil {
			return "id:" + strconv.Itoa(id)
		}
		return "username:" + identity
	}
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 15 * time.Second
	for i := 1; i < attempt && delay < 10*time.Minute; i++ {
		delay *= 2
	}
	if delay > 10*time.Minute {
		delay = 10 * time.Minute
	}
	return delay
}

func (m *Manager) logf(format string, args ...any) {
	m.logger.Printf(format, args...)
}

func userKey(user *panel.User) (string, error) {
	if user == nil {
		return "", errors.New("nil panel user")
	}
	if user.ID > 0 {
		return "id:" + strconv.Itoa(user.ID), nil
	}
	if user.Username != "" {
		return "username:" + user.Username, nil
	}
	return "", errors.New("panel returned a user without id or username")
}

func parseUserID(value string) (int, error) {
	if value == "" {
		return 0, errors.New("empty value")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q contains non-digit characters", value)
		}
	}
	id, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	if id <= 0 {
		return 0, fmt.Errorf("user ID must be positive (got %d)", id)
	}
	return id, nil
}

func telegramIDFor(cfg *config.Config, det logwatch.Detection, user *panel.User) string {
	if det.TelegramID != "" {
		return det.TelegramID
	}
	if id := logwatch.MatchTid(cfg.TidRE, user.Username); id != "" {
		return id
	}
	if id := logwatch.MatchTid(cfg.TidRE, user.Note); id != "" {
		return id
	}
	return ""
}

func displayName(user *panel.User) string {
	if user == nil {
		return "unknown"
	}
	if user.Username != "" {
		return user.Username
	}
	if user.ID > 0 {
		return fmt.Sprintf("id-%d", user.ID)
	}
	return "unknown"
}

func recordUser(record storage.Block) *panel.User {
	return &panel.User{ID: record.UserID, Username: record.Username}
}

func displayIP(ip string) string {
	if ip == "" {
		return "unknown"
	}
	return ip
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
