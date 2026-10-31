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

// FailureNotifier is optional. When implemented, transport failures and
// irreversible API errors are reported to the administrator.
type FailureNotifier interface {
	Failure(event notify.Event, action string, cause error, attempt int, retry time.Duration)
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

	// touchLogged rate-limits the "extended block" info line so a busy
	// torrent does not flood journald.
	touchLogged map[string]time.Time

	// detectionTimes holds the recent torrent-tagged line timestamps per
	// identity for the DetectionThreshold sliding window.
	detectionTimes map[string][]time.Time
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
		cfg:            cfg,
		store:          store,
		panel:          panelClient,
		notifier:       notifier,
		now:            time.Now,
		logger:         log.New(log.Writer(), "blocker: ", log.LstdFlags),
		identityCache:  make(map[string]cacheEntry),
		touchLogged:    make(map[string]time.Time),
		detectionTimes: make(map[string][]time.Time),
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
	// every new detection extends the timer by BlockDuration, but only the
	// journal is touched: repeated lines do not call the panel again.
	if cacheKey := m.detectionCacheKey(det.Identity); cacheKey != "" {
		if entry, ok := m.identityCache[cacheKey]; ok {
			if now.Before(entry.Until) {
				if record, found := m.store.Get(entry.Key); found && record.State == storage.StateActive {
					if updated, extended := m.extendBlockLocked(record, det, now, duration); extended {
						m.logExtended(updated, now)
						return nil
					}
				}
				delete(m.identityCache, cacheKey)
			} else {
				delete(m.identityCache, cacheKey)
			}
		}
	}

	// A missed cache entry (for example a username-keyed unresolved record)
	// must not make an active block wait for the threshold again.
	if record, found := m.findRecordForIdentity(det.Identity); found && record.State == storage.StateActive && now.Before(record.BlockedUntil) {
		if updated, extended := m.extendBlockLocked(record, det, now, duration); extended {
			m.logExtended(updated, now)
			return nil
		}
	}

	if !m.allowDetectionLocked(det, now) {
		return nil
	}

	user, err := m.resolve(ctx, det.Identity)
	if err != nil {
		return m.handleResolveFailure(ctx, det, now, duration, err)
	}
	key, err := userKey(user)
	if err != nil {
		return err
	}

	existing, existed := m.findRecordForUser(user)
	if existed {
		if existing.State == storage.StatePending {
			if now.Before(existing.BlockedUntil) {
				m.logf("user %s (id %d) has a pending block until %s; waiting for reconciliation",
					displayName(user), user.ID, existing.BlockedUntil.Format(time.RFC3339))
				return nil
			}
			m.reconcilePending(ctx, existing)
			return nil
		} else {
			if now.Before(existing.BlockedUntil) {
				if updated, extended := m.extendBlockLocked(existing, det, now, duration); extended {
					m.logExtended(updated, now)
					return nil
				}
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

			if err := m.store.Remove(existing.Key); err != nil {
				return fmt.Errorf("remove expired block record for %s: %w", displayName(user), err)
			}
			m.forgetBlockLocked(existing)
		}
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
			m.notifyFailure(record, "block", err, 0, 0)
			return fmt.Errorf("disable user %s: %w", displayName(user), err)
		}
		m.recordFailure(record, "block", fmt.Errorf("disable user: %w", err))
		return fmt.Errorf("disable user %s (outcome unknown; will reconcile): %w", displayName(user), err)
	}
	if updated == nil {
		cause := errors.New("panel returned an empty response")
		m.recordFailure(record, "block", cause)
		return fmt.Errorf("disable user %s: %w", displayName(user), cause)
	}
	if updated.Status != panel.StatusDisabled {
		cause := fmt.Errorf("panel returned status %q", updated.Status)
		m.recordFailure(record, "block", cause)
		return fmt.Errorf("disable user %s: %w", displayName(user), cause)
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

// handleResolveFailure keeps a durable pending record when the panel cannot be
// reached before a block can be resolved. It deliberately does not drop the
// detection: the expiry worker will retry resolution and apply the block (or
// discard it if the window has already elapsed).
func (m *Manager) handleResolveFailure(ctx context.Context, det logwatch.Detection, now time.Time, duration time.Duration, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, userID, username, err := m.provisionalIdentityKey(det.Identity)
	if err != nil {
		return fmt.Errorf("resolve user %q: %w", det.Identity, cause)
	}

	if existing, ok := m.store.Get(key); ok {
		if existing.State == storage.StateActive {
			if now.Before(existing.BlockedUntil) {
				if updated, extended := m.extendBlockLocked(existing, det, now, duration); extended {
					m.logExtended(updated, now)
					return nil
				}
			}
			return fmt.Errorf("resolve user %q: %w", det.Identity, cause)
		}
		if now.Before(existing.BlockedUntil) {
			m.logf("pending block for %s; resolve will be retried after backoff", displayName(recordUser(existing)))
			return nil
		}
		m.reconcilePending(ctx, existing)
		return nil
	}

	record := storage.Block{
		Key:          key,
		UserID:       userID,
		Username:     username,
		IP:           det.IP,
		TelegramID:   det.TelegramID,
		BlockedAt:    now,
		BlockedUntil: now.Add(duration),
		State:        storage.StatePending,
		Unresolved:   true,
	}

	// A definitive 401/403/404/422 will not improve by retrying. Report it,
	// but do not create a record that can never succeed.
	if panel.IsDefinitive(cause) {
		m.notifyFailure(record, "block", cause, 0, 0)
		return fmt.Errorf("resolve user %q: %w", det.Identity, cause)
	}

	if err := m.store.Put(record); err != nil {
		return fmt.Errorf("persist unresolved block for %q: %w", det.Identity, err)
	}
	m.recordFailure(record, "block", cause)
	return fmt.Errorf("resolve user %q (will retry): %w", det.Identity, cause)
}

// provisionalIdentityKey builds the journal key used before the panel has
// resolved an identity to a user ID.
func (m *Manager) provisionalIdentityKey(identity string) (string, int, string, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return "", 0, "", errors.New("empty identity")
	}
	if m.cfg.PasarGuard.Identifier != "username" {
		if id, err := parseUserID(identity); err == nil {
			return "id:" + strconv.Itoa(id), id, "", nil
		}
		if m.cfg.PasarGuard.Identifier == "id" {
			return "", 0, "", fmt.Errorf("identity %q is not numeric", identity)
		}
	}
	return "username:" + identity, 0, identity, nil
}

// findRecordForUser finds a record under either the numeric-ID or username
// key. This handles an unresolved username record that later resolves to an ID
// without creating a second record for the same account.
func (m *Manager) findRecordForUser(user *panel.User) (storage.Block, bool) {
	if user == nil {
		return storage.Block{}, false
	}
	if user.ID > 0 {
		if record, ok := m.store.Get("id:" + strconv.Itoa(user.ID)); ok {
			return record, true
		}
	}
	if user.Username != "" {
		if record, ok := m.store.Get("username:" + user.Username); ok {
			return record, true
		}
	}
	return storage.Block{}, false
}

// Run reconciles pending records and lifts expired blocks until ctx is done.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	m.ProcessExpired(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	flushTicker := time.NewTicker(time.Second)
	defer flushTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := m.store.Flush(); err != nil {
				m.logf("could not flush extended blocks on shutdown: %v", err)
			}
			return
		case <-ticker.C:
			m.ProcessExpired(ctx)
		case <-flushTicker.C:
			if err := m.store.Flush(); err != nil {
				m.logf("could not flush extended blocks: %v", err)
			}
		}
	}
}

// ProcessExpired reconciles pending records and unblocks expired accounts.
func (m *Manager) ProcessExpired(ctx context.Context) {
	// Extensions are coalesced; persist them whenever the expiry worker runs.
	defer func() {
		if err := m.store.Flush(); err != nil {
			m.logf("could not flush extended blocks: %v", err)
		}
	}()

	m.mu.Lock()
	m.pruneDetectionWindowLocked(m.now())
	m.mu.Unlock()

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
	now := m.now()
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
		m.recordFailure(record, "block", fmt.Errorf("reconcile pending block: %w", err))
		return
	}

	record.UserID = firstNonZero(user.ID, record.UserID)
	record.Username = firstNonEmpty(user.Username, record.Username)

	if user.Status == panel.StatusDisabled {
		if record.Unresolved {
			// We never reached the disable call, so this account was already
			// disabled by someone else. Never enable it.
			record.PreExistingDisabled = true
		}
		record.Unresolved = false
		record.State = storage.StateActive
		if record.PreviousStatus == "" {
			record.PreviousStatus = panel.StatusDisabled
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
		m.logf("reconciled pending block for user %s (id %d) until %s (pre-existing disabled: %t)",
			displayName(user), user.ID, record.BlockedUntil.Format(time.RFC3339), record.PreExistingDisabled)
		return
	}

	// The account is not disabled. If the intended block window already
	// elapsed, do not disable it late.
	if !now.Before(record.BlockedUntil) {
		if removeErr := m.store.Remove(record.Key); removeErr != nil {
			m.logf("could not remove expired pending record for %s: %v", displayName(user), removeErr)
		} else {
			m.forgetBlockLocked(record)
		}
		m.logf("pending block for %s expired before it took effect (panel status %q); record removed",
			displayName(user), user.Status)
		return
	}

	// The panel did not apply the pending disable, or the earlier request
	// failed. Retry the disable instead of dropping the block.
	updated, err := m.setDisabled(ctx, user, true)
	if err != nil {
		if panel.IsDefinitive(err) {
			if removeErr := m.store.Remove(record.Key); removeErr != nil {
				m.logf("could not remove failed pending record for %s: %v", displayName(user), removeErr)
			} else {
				m.forgetBlockLocked(record)
			}
			m.notifyFailure(record, "block", err, 0, 0)
			return
		}
		m.recordFailure(record, "block", fmt.Errorf("retry pending disable: %w", err))
		return
	}
	if updated == nil || updated.Status != panel.StatusDisabled {
		m.recordFailure(record, "block", errors.New("panel did not report disabled after retried disable"))
		return
	}

	record.State = storage.StateActive
	record.Unresolved = false
	record.PreExistingDisabled = false
	if record.PreviousStatus == "" {
		record.PreviousStatus = user.Status
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
	m.logf("reconciled pending block for user %s (id %d) until %s after retrying disable",
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
		m.recordFailure(record, "unblock", fmt.Errorf("load user for unblock: %w", err))
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
		m.recordFailure(record, "unblock", fmt.Errorf("enable user: %w", err))
		return
	}
	if updated == nil || updated.Status == panel.StatusDisabled {
		m.recordFailure(record, "unblock", errors.New("panel still reports status disabled after enable request"))
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

func (m *Manager) recordFailure(record storage.Block, action string, cause error) {
	record.Attempts++
	delay := retryDelay(record.Attempts)
	record.NextAttempt = m.now().Add(delay)
	if err := m.store.Put(record); err != nil {
		m.logf("could not persist retry state for %s: %v", displayName(recordUser(record)), err)
	}
	m.logf("%s operation for %s failed (attempt %d, retry in %s): %v",
		action, displayName(recordUser(record)), record.Attempts, delay.Round(time.Second), cause)
	if !errors.Is(cause, context.Canceled) {
		m.notifyFailure(record, action, cause, record.Attempts, delay)
	}
}

func (m *Manager) notifyFailure(record storage.Block, action string, cause error, attempt int, retry time.Duration) {
	notifier, ok := m.notifier.(FailureNotifier)
	if !ok {
		return
	}
	event := notify.Event{
		Username:        record.Username,
		UserID:          record.UserID,
		IP:              record.IP,
		Hostname:        m.cfg.Hostname,
		DurationMinutes: m.cfg.BlockDuration,
		Timestamp:       m.now(),
		TelegramID:      record.TelegramID,
	}
	notifier.Failure(event, action, cause, attempt, retry)
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

// extendBlockLocked applies a sliding expiry. The write is coalesced in the
// store and flushed by Run's short ticker, so a busy torrent does not fsync the
// journal for every connection.
func (m *Manager) extendBlockLocked(record storage.Block, det logwatch.Detection, now time.Time, duration time.Duration) (storage.Block, bool) {
	updated, ok := m.store.Extend(record.Key, now.Add(duration), det.IP, det.TelegramID)
	if !ok {
		return storage.Block{}, false
	}
	m.rememberBlockLocked(updated)
	return updated, true
}

// logExtended rate-limits the extension message to once per minute per user.
func (m *Manager) logExtended(record storage.Block, now time.Time) {
	if last, ok := m.touchLogged[record.Key]; ok && now.Sub(last) < time.Minute {
		return
	}
	m.touchLogged[record.Key] = now
	m.logf("extended block for %s (id %d) until %s (new torrent detection)",
		displayName(recordUser(record)), record.UserID, record.BlockedUntil.Format(time.RFC3339))
}

func (m *Manager) forgetBlockLocked(record storage.Block) {
	for key, entry := range m.identityCache {
		if entry.Key == record.Key {
			delete(m.identityCache, key)
		}
	}
	delete(m.touchLogged, record.Key)
}

// findRecordForIdentity looks up an active/pending record using the same key
// as the duplicate cache, without calling the panel.
func (m *Manager) findRecordForIdentity(identity string) (storage.Block, bool) {
	key := m.detectionCacheKey(identity)
	if key == "" {
		return storage.Block{}, false
	}
	return m.store.Get(key)
}

// allowDetectionLocked implements the per-identity detection threshold. It
// returns false while fewer than DetectionThreshold torrent-tagged lines have
// arrived inside the sliding window. The count is cleared when the threshold
// is reached, so a failed/aborted block attempt starts a fresh burst.
func (m *Manager) allowDetectionLocked(det logwatch.Detection, now time.Time) bool {
	threshold := m.cfg.DetectionThreshold
	if threshold <= 1 {
		return true
	}
	window := time.Duration(m.cfg.DetectionWindowMinutes) * time.Minute
	if window <= 0 {
		window = 5 * time.Minute
	}

	key := m.detectionCacheKey(det.Identity)
	if key == "" {
		key = "identity:" + strings.TrimSpace(det.Identity)
	}
	if m.detectionTimes == nil {
		m.detectionTimes = make(map[string][]time.Time)
	}
	times := m.detectionTimes[key]
	cutoff := now.Add(-window)
	first := 0
	for first < len(times) && !times[first].After(cutoff) {
		first++
	}
	times = times[first:]
	times = append(times, now)

	if len(times) < threshold {
		m.detectionTimes[key] = times
		return false
	}
	delete(m.detectionTimes, key)
	m.logf("detection threshold reached for %s: %d torrent-tagged lines within %s",
		det.Identity, len(times), window)
	return true
}

// pruneDetectionWindowLocked drops stale per-identity counters. Called by the
// expiry worker so a user who never reaches the threshold does not leave
// timestamps in memory forever.
func (m *Manager) pruneDetectionWindowLocked(now time.Time) {
	threshold := m.cfg.DetectionThreshold
	if threshold <= 1 {
		return
	}
	window := time.Duration(m.cfg.DetectionWindowMinutes) * time.Minute
	if window <= 0 {
		window = 5 * time.Minute
	}
	cutoff := now.Add(-window)
	for key, times := range m.detectionTimes {
		first := 0
		for first < len(times) && !times[first].After(cutoff) {
			first++
		}
		if first == len(times) {
			delete(m.detectionTimes, key)
			continue
		}
		if first > 0 {
			m.detectionTimes[key] = times[first:]
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
