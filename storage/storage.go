// Package storage persists active and pending user blocks so that a restart
// neither forgets to re-enable a user nor re-enables one too early.
package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Block states.
const (
	// StatePending means the disable request may or may not have reached the
	// panel. Pending records are reconciled against the panel on the next tick.
	StatePending = "pending"
	// StateActive means the panel confirmed the account is disabled and the
	// block will be lifted at BlockedUntil.
	StateActive = "active"
)

const fileName = "blocks.json"

// Block describes one blocked PasarGuard account.
type Block struct {
	Key                 string    `json:"key"`
	UserID              int       `json:"user_id"`
	Username            string    `json:"username"`
	IP                  string    `json:"ip"`
	TelegramID          string    `json:"telegram_id,omitempty"`
	BlockedAt           time.Time `json:"blocked_at"`
	BlockedUntil        time.Time `json:"blocked_until"`
	PreviousStatus      string    `json:"previous_status"`
	PreExistingDisabled bool      `json:"pre_existing_disabled"`
	// FailsafeTriggered records that the all-node reconnect last resort has
	// already run for this block. It prevents a reconnect storm from every
	// subsequent torrent line.
	FailsafeTriggered bool      `json:"failsafe_triggered,omitempty"`
	Unresolved        bool      `json:"unresolved,omitempty"`
	State             string    `json:"state"`
	Notified          bool      `json:"notified"`
	Attempts          int       `json:"unblock_attempts"`
	NextAttempt       time.Time `json:"next_attempt,omitempty"`
}

// Store is a concurrency-safe JSON-backed block store.
type Store struct {
	dir    string
	path   string
	mu     sync.RWMutex
	blocks map[string]Block
	// dirty means Extend changed the in-memory state without an immediate
	// fsync. A short flush interval coalesces bursts of duplicate detections.
	dirty bool
}

// New opens (or creates) the store inside dir. A corrupt store is a hard
// error: silently starting empty could leave accounts disabled forever.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("storage directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create storage directory %s: %w", dir, err)
	}

	s := &Store{
		dir:    dir,
		path:   filepath.Join(dir, fileName),
		blocks: make(map[string]Block),
	}

	if err := s.load(); err != nil {
		return nil, err
	}

	if _, err := os.Stat(filepath.Join(dir, "blocked_ips.json")); err == nil {
		if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
			log.Printf("storage: ignored legacy blocked_ips.json (IP blocks cannot be mapped to PasarGuard users; old firewall rules must be removed manually)")
		}
	}

	return s, nil
}

// Path returns the backing file path.
func (s *Store) Path() string { return s.path }

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read block store %s: %w", s.path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	var blocks map[string]Block
	if err := dec.Decode(&blocks); err != nil {
		return fmt.Errorf("block store %s is corrupt; restore it from backup or move it aside deliberately: %w", s.path, err)
	}
	// Reject trailing data so a truncated/merged file cannot silently lose
	// records.
	var tail any
	if err := dec.Decode(&tail); !errors.Is(err, io.EOF) {
		return fmt.Errorf("block store %s contains trailing data; refusing to start", s.path)
	}
	if blocks == nil {
		blocks = make(map[string]Block)
	}

	for key, block := range blocks {
		if key == "" || block.Key != key {
			return fmt.Errorf("block store %s has an entry with an inconsistent key (%q)", s.path, key)
		}
		if block.BlockedUntil.IsZero() {
			return fmt.Errorf("block store %s has entry %q without blocked_until", s.path, key)
		}
		if block.State == "" {
			block.State = StateActive
			blocks[key] = block
		}
		if block.State != StatePending && block.State != StateActive {
			return fmt.Errorf("block store %s has entry %q with unknown state %q", s.path, key, block.State)
		}
	}

	s.mu.Lock()
	s.blocks = blocks
	s.mu.Unlock()

	// The file contains usernames and IPs; keep it private.
	if err := os.Chmod(s.path, 0o600); err != nil {
		log.Printf("storage: could not restrict permissions on %s: %v", s.path, err)
	}

	return nil
}

// Get returns a copy of a block.
func (s *Store) Get(key string) (Block, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	block, ok := s.blocks[key]
	return block, ok
}

// Put inserts or replaces a block and persists the store atomically.
func (s *Store) Put(block Block) error {
	if block.Key == "" {
		return errors.New("block key must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	previous, existed := s.blocks[block.Key]
	s.blocks[block.Key] = block
	if err := s.saveLocked(); err != nil {
		if existed {
			s.blocks[block.Key] = previous
		} else {
			delete(s.blocks, block.Key)
		}
		s.dirty = true
		return err
	}
	s.dirty = false
	return nil
}

// Remove deletes a block and persists the store atomically.
func (s *Store) Remove(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	previous, existed := s.blocks[key]
	if !existed {
		return nil
	}
	delete(s.blocks, key)
	if err := s.saveLocked(); err != nil {
		s.blocks[key] = previous
		s.dirty = true
		return err
	}
	s.dirty = false
	return nil
}

// Extend updates an active block's expiry and last-seen metadata without an
// immediate fsync. The caller is responsible for periodic Flush calls. It
// returns false if the key is unknown or the record is not active.
func (s *Store) Extend(key string, until time.Time, ip, telegramID string) (Block, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	block, ok := s.blocks[key]
	if !ok || block.State != StateActive {
		return Block{}, false
	}

	changed := false
	if until.After(block.BlockedUntil) {
		block.BlockedUntil = until
		changed = true
	}
	if ip != "" && ip != block.IP {
		block.IP = ip
		changed = true
	}
	if telegramID != "" && telegramID != block.TelegramID {
		block.TelegramID = telegramID
		changed = true
	}
	if changed {
		s.blocks[key] = block
		s.dirty = true
	}
	return block, true
}

// Flush persists pending in-memory extensions. It is a no-op when clean.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// List returns a stable snapshot sorted by expiry and key.
func (s *Store) List() []Block {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Block, 0, len(s.blocks))
	for _, block := range s.blocks {
		out = append(out, block)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BlockedUntil.Equal(out[j].BlockedUntil) {
			return out[i].Key < out[j].Key
		}
		return out[i].BlockedUntil.Before(out[j].BlockedUntil)
	})
	return out
}

// Len returns the number of records.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.blocks)
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.blocks, "", "  ")
	if err != nil {
		return fmt.Errorf("encode block store: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.dir, ".blocks-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary block store: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temporary block store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temporary block store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync temporary block store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temporary block store: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace block store: %w", err)
	}

	// Best-effort directory fsync so the rename survives a crash.
	if dir, err := os.Open(s.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// Close flushes any coalesced extensions. Every Put/Remove is already durable.
func (s *Store) Close() error { return s.Flush() }
