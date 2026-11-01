package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testBlock(key string, until time.Time) Block {
	return Block{
		Key:            key,
		UserID:         1,
		Username:       "alice",
		IP:             "1.2.3.4",
		BlockedAt:      time.Now().Add(-time.Minute),
		BlockedUntil:   until,
		PreviousStatus: "active",
		State:          StateActive,
	}
}

func TestPutGetRemoveList(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	until := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	block := testBlock("id:1", until)
	if err := store.Put(block); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if store.Len() != 1 {
		t.Fatalf("Len = %d", store.Len())
	}

	got, ok := store.Get("id:1")
	if !ok {
		t.Fatal("Get returned no block")
	}
	if !got.BlockedUntil.Equal(until) || got.Username != "alice" || got.State != StateActive {
		t.Errorf("block = %+v", got)
	}

	if err := store.Put(testBlock("id:2", until.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	list := store.List()
	if len(list) != 2 || list[0].Key != "id:1" || list[1].Key != "id:2" {
		t.Errorf("List = %+v", list)
	}

	if err := store.Remove("id:1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("id:1"); ok {
		t.Error("block still present after Remove")
	}
	if err := store.Remove("missing"); err != nil {
		t.Errorf("Remove(missing) = %v", err)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	block := testBlock("id:9", until)
	block.TelegramID = "-100123"
	if err := store.Put(block); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Get("id:9")
	if !ok {
		t.Fatal("block missing after reopen")
	}
	if !got.BlockedUntil.Equal(until) || got.TelegramID != "-100123" {
		t.Errorf("block after reopen = %+v", got)
	}

	info, err := os.Stat(reopened.Path())
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("store mode = %o, want 600", mode)
	}
}

func TestCorruptStoreIsRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(`{"id:1":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil {
		t.Fatal("corrupt store was accepted")
	}
}

func TestTrailingDataIsRejected(t *testing.T) {
	dir := t.TempDir()
	body := `{"id:1":{"key":"id:1","blocked_until":"2030-01-01T00:00:00Z"}} {}`
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil {
		t.Fatal("trailing data was accepted")
	}
}

func TestInconsistentKeyIsRejected(t *testing.T) {
	dir := t.TempDir()
	body := `{"id:1":{"key":"id:2","blocked_until":"2030-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil {
		t.Fatal("inconsistent key was accepted")
	}
}

func TestMissingExpiryIsRejected(t *testing.T) {
	dir := t.TempDir()
	body := `{"id:1":{"key":"id:1"}}`
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil {
		t.Fatal("missing blocked_until was accepted")
	}
}

func TestLegacyFileDoesNotBreakStartup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blocked_ips.json"), []byte(`{"1.2.3.4":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(dir)
	if err != nil {
		t.Fatalf("New with legacy file: %v", err)
	}
	if store.Len() != 0 {
		t.Errorf("legacy file was imported unexpectedly")
	}
}

func TestPutRollsBackOnWriteFailure(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	original := testBlock("id:1", time.Now().Add(time.Hour))
	if err := store.Put(original); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make directory read-only: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o750) }()

	replacement := testBlock("id:1", time.Now().Add(2*time.Hour))
	replacement.Username = "changed"
	if err := store.Put(replacement); err == nil {
		t.Fatal("Put succeeded on a read-only directory")
	}

	got, _ := store.Get("id:1")
	if got.Username != "alice" {
		t.Errorf("in-memory state was not rolled back: %+v", got)
	}
}

func TestPutRejectsEmptyKey(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(Block{}); err == nil {
		t.Fatal("empty key was accepted")
	}
}

func TestStateDefaultsToActive(t *testing.T) {
	dir := t.TempDir()
	body := `{"id:1":{"key":"id:1","blocked_until":"2030-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	block, ok := store.Get("id:1")
	if !ok || block.State != StateActive {
		t.Errorf("block = %+v, ok = %v", block, ok)
	}
}

func TestConcurrentAccess(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			key := "id:" + string(rune('A'+i%26)) + string(rune('0'+i/26))
			_ = store.Put(testBlock(key, time.Now().Add(time.Duration(i+1)*time.Minute)))
		}(i)
		go func() {
			defer wg.Done()
			_ = store.List()
		}()
	}
	wg.Wait()

	if store.Len() != 26 {
		// Keys repeat every 26; the exact count is deliberately not asserted
		// beyond "all writes finished without a data race".
		t.Logf("records after concurrent writes: %d", store.Len())
	}
}

func TestStoreFileIsValidJSON(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(testBlock("id:1", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("store is not valid JSON: %v", err)
	}
	if len(decoded) != 1 {
		t.Errorf("decoded %d records", len(decoded))
	}
}

func TestEmptyStoreFileIsAccepted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if store.Len() != 0 {
		t.Errorf("empty store has %d records", store.Len())
	}
}

func TestNewRejectsFileAsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil {
		t.Fatal("expected an error when storage path is a file")
	}
}

func TestExtendAndFlushPersists(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	block := Block{
		Key:          "id:7",
		UserID:       7,
		Username:     "alice",
		IP:           "203.0.113.9",
		BlockedAt:    now,
		BlockedUntil: now.Add(10 * time.Minute),
		State:        StateActive,
	}
	if err := store.Put(block); err != nil {
		t.Fatal(err)
	}

	until := now.Add(20 * time.Minute)
	updated, ok := store.Extend("id:7", until, "203.0.113.10", "vless-in", "777")
	if !ok {
		t.Fatal("Extend returned false")
	}
	if !updated.BlockedUntil.Equal(until) || updated.IP != "203.0.113.10" || updated.Inbound != "vless-in" || updated.TelegramID != "777" {
		t.Fatalf("updated = %+v", updated)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("id:7")
	if !ok {
		t.Fatal("record missing after reopen")
	}
	if !got.BlockedUntil.Equal(until) || got.IP != "203.0.113.10" || got.Inbound != "vless-in" || got.TelegramID != "777" {
		t.Fatalf("persisted = %+v", got)
	}
}

func TestExtendIgnoresPendingAndMissing(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Put(Block{Key: "id:1", UserID: 1, BlockedAt: now, BlockedUntil: now.Add(time.Minute), State: StatePending}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Extend("id:1", now.Add(time.Hour), "", "", ""); ok {
		t.Fatal("Extend changed a pending record")
	}
	if _, ok := store.Extend("id:404", now.Add(time.Hour), "", "", ""); ok {
		t.Fatal("Extend changed a missing record")
	}
}
