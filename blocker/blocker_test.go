package blocker

import (
	"context"
	"errors"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"tblocker/config"
	"tblocker/logwatch"
	"tblocker/notify"
	"tblocker/panel"
	"tblocker/storage"
)

// --- fakes -----------------------------------------------------------------

type setCall struct {
	ID       int
	Username string
	Disabled bool
}

type fakePanel struct {
	mu sync.Mutex

	users  map[int]*panel.User
	byName map[string]int

	getByIDCalls   int
	getByNameCalls int
	setCalls       []setCall

	getErr           error
	setErr           error
	applyEvenOnError bool

	nodes          []panel.Node
	nodesErr       error
	reconnectErr   error
	reconnectedIDs []int
	nodeListCalls  int
}

func newFakePanel(users ...*panel.User) *fakePanel {
	f := &fakePanel{
		users:  make(map[int]*panel.User),
		byName: make(map[string]int),
	}
	for _, user := range users {
		f.addUser(user)
	}
	return f
}

func (f *fakePanel) addUser(user *panel.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := *user
	f.users[user.ID] = &copied
	if user.Username != "" {
		f.byName[user.Username] = user.ID
	}
}

func (f *fakePanel) removeUser(id int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if user, ok := f.users[id]; ok {
		delete(f.byName, user.Username)
		delete(f.users, id)
	}
}

func (f *fakePanel) GetUserByID(_ context.Context, id int) (*panel.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getByIDCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	user, ok := f.users[id]
	if !ok {
		return nil, notFound()
	}
	copied := *user
	return &copied, nil
}

func (f *fakePanel) GetUserByUsername(_ context.Context, username string) (*panel.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getByNameCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	id, ok := f.byName[username]
	if !ok {
		return nil, notFound()
	}
	copied := *f.users[id]
	return &copied, nil
}

func (f *fakePanel) SetDisabledByID(_ context.Context, id int, disabled bool) (*panel.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, setCall{ID: id, Disabled: disabled})
	if f.setErr != nil && !f.applyEvenOnError {
		return nil, f.setErr
	}
	user, ok := f.users[id]
	if !ok {
		return nil, notFound()
	}
	user.Status = statusFor(disabled)
	copied := *user
	if f.setErr != nil {
		return nil, f.setErr
	}
	return &copied, nil
}

func (f *fakePanel) SetDisabledByUsername(_ context.Context, username string, disabled bool) (*panel.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byName[username]
	if !ok {
		return nil, notFound()
	}
	f.setCalls = append(f.setCalls, setCall{ID: id, Username: username, Disabled: disabled})
	if f.setErr != nil && !f.applyEvenOnError {
		return nil, f.setErr
	}
	user := f.users[id]
	user.Status = statusFor(disabled)
	copied := *user
	if f.setErr != nil {
		return nil, f.setErr
	}
	return &copied, nil
}

func statusFor(disabled bool) string {
	if disabled {
		return panel.StatusDisabled
	}
	return panel.StatusActive
}

func notFound() error {
	return &panel.APIError{StatusCode: 404, Method: "GET", Path: "/api/user", Detail: "User not found"}
}

func serverError() error {
	return &panel.APIError{StatusCode: 500, Method: "PUT", Path: "/api/user", Detail: "boom"}
}

func forbidden() error {
	return &panel.APIError{StatusCode: 403, Method: "PUT", Path: "/api/user", Detail: "forbidden"}
}

func (f *fakePanel) calls() []setCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]setCall, len(f.setCalls))
	copy(out, f.setCalls)
	return out
}

func (f *fakePanel) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getByIDCalls, f.getByNameCalls
}

func (f *fakePanel) ListEnabledNodes(_ context.Context) ([]panel.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodeListCalls++
	if f.nodesErr != nil {
		return nil, f.nodesErr
	}
	return append([]panel.Node(nil), f.nodes...), nil
}

func (f *fakePanel) ReconnectNode(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reconnectErr != nil {
		return f.reconnectErr
	}
	f.reconnectedIDs = append(f.reconnectedIDs, id)
	return nil
}

func (f *fakePanel) reconnected() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.reconnectedIDs...)
}

func (f *fakePanel) nodeListCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nodeListCalls
}

func (f *fakePanel) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setErr = err
}

func (f *fakePanel) setStatus(id int, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if user, ok := f.users[id]; ok {
		user.Status = status
	}
}

type fakeFailure struct {
	event   notify.Event
	action  string
	cause   error
	attempt int
	retry   time.Duration
}

type fakeNotifier struct {
	mu        sync.Mutex
	blocks    []notify.Event
	unblocks  []notify.Event
	failures  []fakeFailure
	failsafes []fakeFailsafe
}

type fakeFailsafe struct {
	event       notify.Event
	reconnected []string
	failed      []string
	cause       error
}

func (n *fakeNotifier) Block(event notify.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blocks = append(n.blocks, event)
}

func (n *fakeNotifier) Unblock(event notify.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.unblocks = append(n.unblocks, event)
}

func (n *fakeNotifier) blockEvents() []notify.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notify.Event(nil), n.blocks...)
}

func (n *fakeNotifier) unblockEvents() []notify.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notify.Event(nil), n.unblocks...)
}

func (n *fakeNotifier) Failure(event notify.Event, action string, cause error, attempt int, retry time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failures = append(n.failures, fakeFailure{event: event, action: action, cause: cause, attempt: attempt, retry: retry})
}

func (n *fakeNotifier) Failsafe(event notify.Event, reconnected, failed []string, cause error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failsafes = append(n.failsafes, fakeFailsafe{
		event:       event,
		reconnected: append([]string(nil), reconnected...),
		failed:      append([]string(nil), failed...),
		cause:       cause,
	})
}

func (n *fakeNotifier) failsafeEvents() []fakeFailsafe {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]fakeFailsafe(nil), n.failsafes...)
}

func (n *fakeNotifier) failureEvents() []fakeFailure {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]fakeFailure(nil), n.failures...)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// --- helpers ---------------------------------------------------------------

func testConfig() *config.Config {
	re := regexp.MustCompile(`email:\s*(\S+)`)
	return &config.Config{
		LogFile:         "/tmp/access.log",
		BlockDuration:   10,
		TorrentTag:      "TORRENT",
		Hostname:        "node-1",
		UsernameRegex:   `email:\s*(\S+)`,
		UsernameRE:      re,
		TidRE:           regexp.MustCompile(`tgid-(-?[0-9]+)`),
		WebhookTemplate: config.DefaultWebhookTemplate,
		PasarGuard: config.PasarGuardConfig{
			BaseURL:        "http://panel",
			APIKey:         "pg_key_test",
			Identifier:     "auto",
			TimeoutSeconds: 10,
		},
	}
}

func testDetection(identity, ip, tid string) logwatch.Detection {
	return logwatch.Detection{Identity: identity, IP: ip, TelegramID: tid}
}

func newTestManager(t *testing.T, cfg *config.Config, fp *fakePanel, clock *fakeClock) (*Manager, *fakeNotifier, *storage.Store) {
	t.Helper()
	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	notifier := &fakeNotifier{}
	manager := New(cfg, store, fp, notifier,
		WithClock(clock.Now),
		WithLogger(log.New(io.Discard, "", 0)),
	)
	return manager, notifier, store
}

func mustBlock(t *testing.T, store *storage.Store, key string) storage.Block {
	t.Helper()
	block, ok := store.Get(key)
	if !ok {
		t.Fatalf("block %q not found (store has %d records)", key, store.Len())
	}
	return block
}

// --- tests -----------------------------------------------------------------

func TestHandleBlocksActiveUser(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "203.0.113.9", "")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	calls := fp.calls()
	if len(calls) != 1 || calls[0].ID != 7 || !calls[0].Disabled {
		t.Fatalf("set calls = %+v", calls)
	}

	block := mustBlock(t, store, "id:7")
	if block.State != storage.StateActive || block.PreExistingDisabled {
		t.Errorf("block = %+v", block)
	}
	if block.PreviousStatus != panel.StatusActive {
		t.Errorf("PreviousStatus = %q", block.PreviousStatus)
	}
	if want := clock.Now().Add(10 * time.Minute); !block.BlockedUntil.Equal(want) {
		t.Errorf("BlockedUntil = %s, want %s", block.BlockedUntil, want)
	}

	events := notifier.blockEvents()
	if len(events) != 1 {
		t.Fatalf("block events = %d", len(events))
	}
	if events[0].Username != "alice" || events[0].UserID != 7 || events[0].IP != "203.0.113.9" || events[0].DurationMinutes != 10 {
		t.Errorf("event = %+v", events[0])
	}
}

func TestHandleUsernameMode(t *testing.T) {
	cfg := testConfig()
	cfg.PasarGuard.Identifier = "username"
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 9, Username: "bob", Status: panel.StatusActive})
	manager, _, store := newTestManager(t, cfg, fp, clock)

	if err := manager.Handle(context.Background(), testDetection("bob", "", "")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	calls := fp.calls()
	if len(calls) != 1 || calls[0].Username != "bob" || calls[0].ID != 9 || !calls[0].Disabled {
		t.Fatalf("set calls = %+v", calls)
	}
	mustBlock(t, store, "id:9")
}

func TestHandleAutoNumericFallsBackToUsername(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 10, Username: "12345", Status: panel.StatusActive})
	manager, _, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("12345", "", "")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	idCalls, nameCalls := fp.counts()
	if idCalls != 1 || nameCalls != 1 {
		t.Errorf("calls by id/name = %d/%d", idCalls, nameCalls)
	}
	mustBlock(t, store, "id:10")
}

func TestHandleAutoNumericPrefersID(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(
		&panel.User{ID: 5, Username: "alice", Status: panel.StatusActive},
		&panel.User{ID: 99, Username: "5", Status: panel.StatusActive},
	)
	manager, _, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("5", "", "")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	mustBlock(t, store, "id:5")
	if _, ok := store.Get("id:99"); ok {
		t.Error("numeric username was blocked instead of the numeric user ID")
	}
}

func TestHandleAlreadyDisabled(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusDisabled})
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if calls := fp.calls(); len(calls) != 0 {
		t.Fatalf("panel was modified: %+v", calls)
	}
	block := mustBlock(t, store, "id:7")
	if !block.PreExistingDisabled || block.State != storage.StateActive {
		t.Errorf("block = %+v", block)
	}
	if len(notifier.blockEvents()) != 1 {
		t.Error("admin was not notified about the detection")
	}
}

func TestHandleDuplicateDetection(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	for i := 0; i < 2; i++ {
		if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	if calls := fp.calls(); len(calls) != 1 {
		t.Errorf("set calls = %d, want 1", len(calls))
	}
	if idCalls, nameCalls := fp.counts(); idCalls != 1 || nameCalls != 0 {
		t.Errorf("resolve calls after duplicate = id:%d name:%d, want 1/0", idCalls, nameCalls)
	}
	if len(notifier.blockEvents()) != 1 {
		t.Errorf("block events = %d, want 1", len(notifier.blockEvents()))
	}
	if store.Len() != 1 {
		t.Errorf("store length = %d", store.Len())
	}
}

func TestHandleDefinitiveErrorRemovesRecord(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	fp.setErr = forbidden()
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	err := manager.Handle(context.Background(), testDetection("7", "", ""))
	if err == nil {
		t.Fatal("expected an error")
	}
	if store.Len() != 0 {
		t.Errorf("failed block record was kept: %+v", store.List())
	}
	if len(notifier.blockEvents()) != 0 {
		t.Error("notification was sent for a failed block")
	}
}

func TestHandleUnknownErrorKeepsPendingAndReconciles(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	fp.setErr = serverError()
	fp.applyEvenOnError = true
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	err := manager.Handle(context.Background(), testDetection("7", "", ""))
	if err == nil {
		t.Fatal("expected an error")
	}
	block := mustBlock(t, store, "id:7")
	if block.State != storage.StatePending {
		t.Errorf("state = %q, want pending", block.State)
	}
	if len(notifier.blockEvents()) != 0 {
		t.Error("notification sent before reconciliation")
	}

	fp.setError(nil)
	clock.Advance(16 * time.Second)
	manager.ProcessExpired(context.Background())

	block = mustBlock(t, store, "id:7")
	if block.State != storage.StateActive {
		t.Errorf("state after reconcile = %q", block.State)
	}
	if len(notifier.blockEvents()) != 1 {
		t.Errorf("block events = %d", len(notifier.blockEvents()))
	}
}

func TestPendingReconcileRetriesDisable(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	if err := store.Put(storage.Block{
		Key:            "id:7",
		UserID:         7,
		Username:       "alice",
		BlockedUntil:   clock.Now().Add(10 * time.Minute),
		PreviousStatus: panel.StatusActive,
		State:          storage.StatePending,
	}); err != nil {
		t.Fatal(err)
	}

	manager.ProcessExpired(context.Background())
	block := mustBlock(t, store, "id:7")
	if block.State != storage.StateActive {
		t.Errorf("state = %q, want active", block.State)
	}
	if calls := fp.calls(); len(calls) != 1 || !calls[0].Disabled {
		t.Errorf("retry disable calls = %+v", calls)
	}
	if len(notifier.blockEvents()) != 1 {
		t.Errorf("block events = %d, want 1", len(notifier.blockEvents()))
	}
}

func TestPendingReconcileExpiredActiveDropsWithoutLateDisable(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, _, store := newTestManager(t, testConfig(), fp, clock)

	if err := store.Put(storage.Block{
		Key:            "id:7",
		UserID:         7,
		Username:       "alice",
		BlockedUntil:   clock.Now().Add(-time.Minute),
		PreviousStatus: panel.StatusActive,
		State:          storage.StatePending,
	}); err != nil {
		t.Fatal(err)
	}

	manager.ProcessExpired(context.Background())
	if store.Len() != 0 {
		t.Errorf("expired pending record was not removed: %+v", store.List())
	}
	if calls := fp.calls(); len(calls) != 0 {
		t.Errorf("late disable was attempted: %+v", calls)
	}
}

func TestUnblockExpired(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "203.0.113.9", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Minute)
	manager.ProcessExpired(context.Background())

	calls := fp.calls()
	if len(calls) != 2 || calls[1].Disabled {
		t.Fatalf("set calls = %+v", calls)
	}
	if store.Len() != 0 {
		t.Errorf("record not removed: %+v", store.List())
	}
	unblocks := notifier.unblockEvents()
	if len(unblocks) != 1 || unblocks[0].Username != "alice" {
		t.Errorf("unblock events = %+v", unblocks)
	}
	if unblocks[0].DurationMinutes != 10 {
		t.Errorf("unblock duration = %d", unblocks[0].DurationMinutes)
	}
}

func TestUnblockRespectsExternalStatusChange(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	fp.setStatus(7, panel.StatusLimited)
	clock.Advance(11 * time.Minute)
	manager.ProcessExpired(context.Background())

	if calls := fp.calls(); len(calls) != 1 {
		t.Errorf("unexpected enable call: %+v", calls)
	}
	if store.Len() != 0 {
		t.Errorf("record not removed: %+v", store.List())
	}
	if len(notifier.unblockEvents()) != 0 {
		t.Error("unblock notification sent when nothing was enabled")
	}
}

func TestUnblockFailureBacksOff(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, _, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	fp.setError(serverError())
	clock.Advance(11 * time.Minute)
	manager.ProcessExpired(context.Background())

	block := mustBlock(t, store, "id:7")
	if block.Attempts != 1 {
		t.Errorf("Attempts = %d", block.Attempts)
	}
	if !block.NextAttempt.After(clock.Now()) {
		t.Errorf("NextAttempt = %s, now = %s", block.NextAttempt, clock.Now())
	}

	// A second tick inside the backoff window must not call the panel.
	callsBefore := len(fp.calls())
	manager.ProcessExpired(context.Background())
	if got := len(fp.calls()); got != callsBefore {
		t.Errorf("panel was called during backoff: %d -> %d", callsBefore, got)
	}

	fp.setError(nil)
	clock.Advance(16 * time.Second)
	manager.ProcessExpired(context.Background())
	if store.Len() != 0 {
		t.Errorf("record not removed after successful retry: %+v", store.List())
	}
}

func TestReenableDisabledByConfig(t *testing.T) {
	cfg := testConfig()
	disabled := false
	cfg.PasarGuard.ReenableOnUnblock = &disabled
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, notifier, store := newTestManager(t, cfg, fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Minute)
	manager.ProcessExpired(context.Background())

	if calls := fp.calls(); len(calls) != 1 {
		t.Errorf("enable was called with ReenableOnUnblock=false: %+v", calls)
	}
	if store.Len() != 0 {
		t.Errorf("record not removed: %+v", store.List())
	}
	if len(notifier.unblockEvents()) != 0 {
		t.Error("unblock notification sent without enabling")
	}
}

func TestRestartKeepsBlockUntilExpiry(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})

	store1, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	manager1 := New(testConfig(), store1, fp, &fakeNotifier{}, WithClock(clock.Now), WithLogger(log.New(io.Discard, "", 0)))
	if err := manager1.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}

	store2, err := storage.New(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	manager2 := New(testConfig(), store2, fp, &fakeNotifier{}, WithClock(clock.Now), WithLogger(log.New(io.Discard, "", 0)))

	clock.Advance(5 * time.Minute)
	manager2.ProcessExpired(context.Background())
	if calls := fp.calls(); len(calls) != 1 {
		t.Errorf("premature enable after restart: %+v", calls)
	}

	clock.Advance(6 * time.Minute)
	manager2.ProcessExpired(context.Background())
	if calls := fp.calls(); len(calls) != 2 {
		t.Errorf("enable was not called after expiry: %+v", calls)
	}
}

func TestReArmExpiredBlockWhenStillDisabled(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}

	if calls := fp.calls(); len(calls) != 1 {
		t.Errorf("re-arm called the panel again: %+v", calls)
	}
	block := mustBlock(t, store, "id:7")
	if want := clock.Now().Add(10 * time.Minute); !block.BlockedUntil.Equal(want) {
		t.Errorf("BlockedUntil = %s, want %s", block.BlockedUntil, want)
	}
	if len(notifier.blockEvents()) != 2 {
		t.Errorf("block events = %d", len(notifier.blockEvents()))
	}
}

func TestExpiredRecordWithActiveUserCreatesNewBlock(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, _, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Minute)
	fp.setStatus(7, panel.StatusActive)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	if calls := fp.calls(); len(calls) != 2 {
		t.Errorf("set calls = %+v", calls)
	}
	block := mustBlock(t, store, "id:7")
	if block.State != storage.StateActive || block.PreExistingDisabled {
		t.Errorf("block = %+v", block)
	}
}

func TestTelegramIDFallbacks(t *testing.T) {
	cases := []struct {
		name     string
		username string
		note     string
		detTid   string
		want     string
	}{
		{name: "log line wins", username: "alice_tgid-1", note: "tgid-2", detTid: "3", want: "3"},
		{name: "username", username: "alice_tgid-111", want: "111"},
		{name: "note", username: "alice", note: "contact tgid--100222", want: "-100222"},
		{name: "none", username: "alice", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			fp := newFakePanel(&panel.User{ID: 7, Username: tc.username, Note: tc.note, Status: panel.StatusActive})
			manager, notifier, _ := newTestManager(t, testConfig(), fp, clock)
			if err := manager.Handle(context.Background(), testDetection("7", "", tc.detTid)); err != nil {
				t.Fatal(err)
			}
			events := notifier.blockEvents()
			if len(events) != 1 || events[0].TelegramID != tc.want {
				t.Fatalf("events = %+v, want TelegramID %q", events, tc.want)
			}
		})
	}
}

func TestHandleMissingUser(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel()
	manager, _, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("404", "", "")); err == nil {
		t.Fatal("expected an error for a missing user")
	}
	if store.Len() != 0 {
		t.Errorf("record created for missing user: %+v", store.List())
	}
}

func TestHandleEmptyIdentity(t *testing.T) {
	manager, _, _ := newTestManager(t, testConfig(), newFakePanel(), newFakeClock())
	if err := manager.Handle(context.Background(), testDetection("   ", "", "")); err == nil {
		t.Fatal("expected an error for an empty identity")
	}
}

func TestParseUserID(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"1", 1, false},
		{"42", 42, false},
		{"0", 0, true},
		{"-1", 0, true},
		{"12a", 0, true},
		{"", 0, true},
		{"999999999999999999999999999", 0, true},
	}
	for _, tc := range cases {
		got, err := parseUserID(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseUserID(%q) = %d, want error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseUserID(%q) = %d, %v", tc.in, got, err)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 1; attempt <= 10; attempt++ {
		delay := retryDelay(attempt)
		if delay < 15*time.Second || delay > 10*time.Minute {
			t.Errorf("retryDelay(%d) = %s", attempt, delay)
		}
		if delay < prev {
			t.Errorf("retryDelay decreased: %s -> %s", prev, delay)
		}
		prev = delay
	}
}

func TestDetectionCacheKey(t *testing.T) {
	cases := []struct {
		mode     string
		identity string
		want     string
	}{
		{"auto", "42", "id:42"},
		{"auto", "alice", "username:alice"},
		{"auto", "0", "username:0"},
		{"id", "42", "id:42"},
		{"id", "alice", ""},
		{"username", "42", "username:42"},
		{"username", "alice", "username:alice"},
		{"auto", "  ", ""},
	}
	for _, tc := range cases {
		cfg := testConfig()
		cfg.PasarGuard.Identifier = tc.mode
		manager := New(cfg, &storage.Store{}, nil, &fakeNotifier{})
		if got := manager.detectionCacheKey(tc.identity); got != tc.want {
			t.Errorf("mode=%s identity=%q: got %q, want %q", tc.mode, tc.identity, got, tc.want)
		}
	}
}

func TestUnblockUsernameMode(t *testing.T) {
	cfg := testConfig()
	cfg.PasarGuard.Identifier = "username"
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 9, Username: "bob", Status: panel.StatusActive})
	manager, _, store := newTestManager(t, cfg, fp, clock)

	if err := manager.Handle(context.Background(), testDetection("bob", "", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Minute)
	manager.ProcessExpired(context.Background())

	calls := fp.calls()
	if len(calls) != 2 || calls[0].Username != "bob" || !calls[0].Disabled || calls[1].Username != "bob" || calls[1].Disabled {
		t.Fatalf("set calls = %+v", calls)
	}
	if store.Len() != 0 {
		t.Errorf("record not removed: %+v", store.List())
	}
}

func TestPendingReconcileDeletedUser(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel()
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	if err := store.Put(storage.Block{
		Key:          "id:99",
		UserID:       99,
		Username:     "gone",
		BlockedUntil: clock.Now().Add(10 * time.Minute),
		State:        storage.StatePending,
	}); err != nil {
		t.Fatal(err)
	}
	manager.ProcessExpired(context.Background())

	if store.Len() != 0 {
		t.Errorf("pending record for deleted user was kept: %+v", store.List())
	}
	if len(notifier.blockEvents()) != 0 {
		t.Error("unexpected notification")
	}
}

func TestUserKey(t *testing.T) {
	if key, err := userKey(&panel.User{ID: 3, Username: "a"}); err != nil || key != "id:3" {
		t.Errorf("key = %q, err = %v", key, err)
	}
	if key, err := userKey(&panel.User{Username: "a"}); err != nil || key != "username:a" {
		t.Errorf("key = %q, err = %v", key, err)
	}
	if _, err := userKey(&panel.User{}); err == nil {
		t.Error("expected error for empty user")
	}
	if _, err := userKey(nil); err == nil {
		t.Error("expected error for nil user")
	}
}

func TestUnblockWhenUserDeleted(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, _, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	fp.removeUser(7)
	clock.Advance(11 * time.Minute)
	manager.ProcessExpired(context.Background())

	if store.Len() != 0 {
		t.Errorf("record for deleted user was kept: %+v", store.List())
	}
}

func TestUnblockDoesNotTargetRecreatedUsername(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, _, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}

	// ID 7 is deleted and a new account reuses the username.
	fp.removeUser(7)
	fp.addUser(&panel.User{ID: 8, Username: "alice", Status: panel.StatusActive})

	clock.Advance(11 * time.Minute)
	manager.ProcessExpired(context.Background())

	if calls := fp.calls(); len(calls) != 1 {
		t.Fatalf("recreated username was modified: %+v", calls)
	}
	if got, _ := fp.GetUserByID(context.Background(), 8); got == nil || got.Status != panel.StatusActive {
		t.Fatalf("new user status = %+v", got)
	}
	if store.Len() != 0 {
		t.Errorf("record was not removed: %+v", store.List())
	}
}

func TestIdentityCacheClearedOnUnblock(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, _, _ := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Minute)
	manager.ProcessExpired(context.Background())

	// The cache must not suppress a legitimate new block after the first one
	// has been lifted.
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	if calls := fp.calls(); len(calls) != 3 {
		t.Fatalf("set calls = %+v, want disable, enable, disable", calls)
	}
}

func TestStartupCachePreventsExtraLookups(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})

	store1, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	manager1 := New(testConfig(), store1, fp, &fakeNotifier{}, WithClock(clock.Now), WithLogger(log.New(io.Discard, "", 0)))
	if err := manager1.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	if idCalls, _ := fp.counts(); idCalls != 1 {
		t.Fatalf("initial lookups = %d", idCalls)
	}

	store2, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	manager2 := New(testConfig(), store2, fp, &fakeNotifier{}, WithClock(clock.Now), WithLogger(log.New(io.Discard, "", 0)))
	if err := manager2.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	if idCalls, _ := fp.counts(); idCalls != 1 {
		t.Errorf("duplicate lookup after restart = %d, want 1", idCalls)
	}
}

func TestProcessExpiredStopsOnCancelledContext(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, _, _ := newTestManager(t, testConfig(), fp, clock)
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager.ProcessExpired(ctx)
	if calls := fp.calls(); len(calls) != 1 {
		t.Errorf("panel was called with a cancelled context: %+v", calls)
	}
}

func TestResolveErrorsAreWrapped(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel()
	fp.getErr = errors.New("network down")
	manager, _, _ := newTestManager(t, testConfig(), fp, clock)

	err := manager.Handle(context.Background(), testDetection("7", "", ""))
	if err == nil || !strings.Contains(err.Error(), "resolve user") {
		t.Fatalf("error = %v", err)
	}
}

func TestHandleDuplicateDetectionExtendsBlock(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "203.0.113.9", "")); err != nil {
		t.Fatal(err)
	}
	first := mustBlock(t, store, "id:7")
	if want := clock.Now().Add(10 * time.Minute); !first.BlockedUntil.Equal(want) {
		t.Fatalf("first expiry = %s, want %s", first.BlockedUntil, want)
	}

	clock.Advance(2 * time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "203.0.113.10", "")); err != nil {
		t.Fatal(err)
	}
	second := mustBlock(t, store, "id:7")
	if want := clock.Now().Add(10 * time.Minute); !second.BlockedUntil.Equal(want) {
		t.Fatalf("extended expiry = %s, want %s", second.BlockedUntil, want)
	}
	if second.BlockedAt != first.BlockedAt {
		t.Errorf("BlockedAt changed from %s to %s", first.BlockedAt, second.BlockedAt)
	}
	if second.IP != "203.0.113.10" {
		t.Errorf("IP = %q, want latest detection IP", second.IP)
	}
	if calls := fp.calls(); len(calls) != 1 {
		t.Errorf("panel calls = %d, want 1", len(calls))
	}
	if events := len(notifier.blockEvents()); events != 1 {
		t.Errorf("block events = %d, want 1", events)
	}
}

func TestHandleAlreadyDisabledExtendsBlock(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusDisabled})
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	first := mustBlock(t, store, "id:7")
	if !first.PreExistingDisabled {
		t.Fatal("record is not marked pre-existing")
	}

	clock.Advance(3 * time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	second := mustBlock(t, store, "id:7")
	if want := clock.Now().Add(10 * time.Minute); !second.BlockedUntil.Equal(want) {
		t.Fatalf("extended expiry = %s, want %s", second.BlockedUntil, want)
	}
	if !second.PreExistingDisabled {
		t.Error("pre-existing flag was lost")
	}
	if calls := fp.calls(); len(calls) != 0 {
		t.Errorf("panel was modified: %+v", calls)
	}
	if events := len(notifier.blockEvents()); events != 1 {
		t.Errorf("block events = %d, want 1", events)
	}
}

func TestRestartAfterExpiryUnblocks(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})

	store1, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	manager1 := New(testConfig(), store1, fp, &fakeNotifier{}, WithClock(clock.Now), WithLogger(log.New(io.Discard, "", 0)))
	if err := manager1.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	// The service was down across the expiry; startup must lift the block.
	clock.Advance(11 * time.Minute)
	store2, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	notifier := &fakeNotifier{}
	manager2 := New(testConfig(), store2, fp, notifier, WithClock(clock.Now), WithLogger(log.New(io.Discard, "", 0)))
	manager2.ProcessExpired(context.Background())

	calls := fp.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want disable + enable", calls)
	}
	if calls[1].Disabled {
		t.Errorf("second call was not an enable: %+v", calls[1])
	}
	if store2.Len() != 0 {
		t.Errorf("record was not removed: %+v", store2.List())
	}
	if len(notifier.unblockEvents()) != 1 {
		t.Errorf("unblock notifications = %d, want 1", len(notifier.unblockEvents()))
	}
}

func TestResolveFailurePersistsPendingAndRetries(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	fp.getErr = errors.New("network down")
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err == nil {
		t.Fatal("expected resolve error")
	}
	block := mustBlock(t, store, "id:7")
	if block.State != storage.StatePending || !block.Unresolved {
		t.Fatalf("block = %+v, want pending unresolved", block)
	}
	if failures := notifier.failureEvents(); len(failures) != 1 || failures[0].action != "block" {
		t.Fatalf("failure notifications = %+v", failures)
	}

	fp.getErr = nil
	clock.Advance(16 * time.Second)
	manager.ProcessExpired(context.Background())

	block = mustBlock(t, store, "id:7")
	if block.State != storage.StateActive || block.Unresolved || block.PreExistingDisabled {
		t.Fatalf("reconciled block = %+v", block)
	}
	if calls := fp.calls(); len(calls) != 1 || !calls[0].Disabled {
		t.Errorf("disable calls = %+v", calls)
	}
	if len(notifier.blockEvents()) != 1 {
		t.Errorf("block notifications = %d, want 1", len(notifier.blockEvents()))
	}
}

func TestBlockFailureNotifiesAdmin(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	fp.setErr = serverError()
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err == nil {
		t.Fatal("expected error")
	}
	if store.Len() != 1 {
		t.Fatalf("pending record was not kept: %+v", store.List())
	}
	failures := notifier.failureEvents()
	if len(failures) != 1 || failures[0].action != "block" || failures[0].retry <= 0 {
		t.Fatalf("failure notifications = %+v", failures)
	}
}

func TestUnblockFailureNotifiesAdmin(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}

	fp.setError(serverError())
	clock.Advance(11 * time.Minute)
	manager.ProcessExpired(context.Background())

	if store.Len() != 1 {
		t.Fatalf("record was not kept for retry: %+v", store.List())
	}
	failures := notifier.failureEvents()
	if len(failures) != 1 || failures[0].action != "unblock" || failures[0].retry <= 0 {
		t.Fatalf("failure notifications = %+v", failures)
	}
}

func TestResolveFailureThenDisabledUserIsPreExisting(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	fp.getErr = errors.New("network down")
	manager, _, store := newTestManager(t, testConfig(), fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err == nil {
		t.Fatal("expected resolve error")
	}
	fp.getErr = nil
	fp.setStatus(7, panel.StatusDisabled)
	clock.Advance(16 * time.Second)
	manager.ProcessExpired(context.Background())

	block := mustBlock(t, store, "id:7")
	if !block.PreExistingDisabled || block.Unresolved || block.State != storage.StateActive {
		t.Fatalf("block = %+v, want active pre-existing", block)
	}
	if calls := fp.calls(); len(calls) != 0 {
		t.Fatalf("panel was modified: %+v", calls)
	}
}

func TestUnresolvedPendingSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	fp.getErr = errors.New("network down")

	store1, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	manager1 := New(testConfig(), store1, fp, &fakeNotifier{}, WithClock(clock.Now), WithLogger(log.New(io.Discard, "", 0)))
	if err := manager1.Handle(context.Background(), testDetection("7", "", "")); err == nil {
		t.Fatal("expected resolve error")
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	fp.getErr = nil
	clock.Advance(16 * time.Second)
	store2, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	notifier := &fakeNotifier{}
	manager2 := New(testConfig(), store2, fp, notifier, WithClock(clock.Now), WithLogger(log.New(io.Discard, "", 0)))
	manager2.ProcessExpired(context.Background())

	block := mustBlock(t, store2, "id:7")
	if block.State != storage.StateActive {
		t.Fatalf("state = %q, want active", block.State)
	}
	if calls := fp.calls(); len(calls) != 1 || !calls[0].Disabled {
		t.Errorf("calls = %+v", calls)
	}
	if len(notifier.blockEvents()) != 1 {
		t.Errorf("block events = %d", len(notifier.blockEvents()))
	}
}

func TestDetectionThresholdRequiresBurst(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	cfg := testConfig()
	cfg.DetectionThreshold = 10
	cfg.DetectionWindowMinutes = 5
	manager, notifier, store := newTestManager(t, cfg, fp, clock)

	for i := 0; i < 9; i++ {
		if err := manager.Handle(context.Background(), testDetection("7", "203.0.113.9", "")); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	if calls := fp.calls(); len(calls) != 0 {
		t.Fatalf("panel was called before the threshold: %+v", calls)
	}
	if idCalls, nameCalls := fp.counts(); idCalls != 0 || nameCalls != 0 {
		t.Fatalf("panel was queried before the threshold: id=%d name=%d", idCalls, nameCalls)
	}
	if store.Len() != 0 {
		t.Fatalf("record created before the threshold: %+v", store.List())
	}

	if err := manager.Handle(context.Background(), testDetection("7", "203.0.113.9", "")); err != nil {
		t.Fatalf("threshold Handle: %v", err)
	}
	if calls := fp.calls(); len(calls) != 1 || !calls[0].Disabled {
		t.Fatalf("calls after threshold = %+v", calls)
	}
	if block := mustBlock(t, store, "id:7"); block.State != storage.StateActive {
		t.Errorf("block = %+v", block)
	}
	if len(notifier.blockEvents()) != 1 {
		t.Errorf("block events = %d", len(notifier.blockEvents()))
	}
}

func TestDetectionWindowExpiresOldLines(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	cfg := testConfig()
	cfg.DetectionThreshold = 10
	cfg.DetectionWindowMinutes = 5
	manager, _, store := newTestManager(t, cfg, fp, clock)

	for i := 0; i < 9; i++ {
		if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(6 * time.Minute)

	for i := 0; i < 9; i++ {
		if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
			t.Fatal(err)
		}
	}
	if calls := fp.calls(); len(calls) != 0 {
		t.Fatalf("panel called with 9 fresh lines: %+v", calls)
	}
	if store.Len() != 0 {
		t.Fatalf("record created with 9 fresh lines: %+v", store.List())
	}

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	if calls := fp.calls(); len(calls) != 1 {
		t.Fatalf("calls after 10 fresh lines = %+v", calls)
	}
}

func TestActiveBlockBypassesDetectionThreshold(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	cfg := testConfig()
	cfg.DetectionThreshold = 10
	cfg.DetectionWindowMinutes = 5
	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(storage.Block{
		Key:          "id:7",
		UserID:       7,
		Username:     "alice",
		BlockedAt:    clock.Now(),
		BlockedUntil: clock.Now().Add(10 * time.Minute),
		State:        storage.StateActive,
	}); err != nil {
		t.Fatal(err)
	}
	notifier := &fakeNotifier{}
	manager := New(cfg, store, fp, notifier, WithClock(clock.Now), WithLogger(log.New(io.Discard, "", 0)))
	clock.Advance(time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}

	block := mustBlock(t, store, "id:7")
	if want := clock.Now().Add(10 * time.Minute); !block.BlockedUntil.Equal(want) {
		t.Errorf("expiry = %s, want %s", block.BlockedUntil, want)
	}
	if calls := fp.calls(); len(calls) != 0 {
		t.Errorf("panel called for an active block: %+v", calls)
	}
}

func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestFailsafeReconnectsAllEnabledNodesAfterDelay(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	fp.nodes = []panel.Node{
		{ID: 1, Name: "node-a", Status: "connected"},
		{ID: 2, Name: "node-b", Status: panel.NodeStatusDisabled},
		{ID: 3, Name: "node-c", Status: panel.NodeStatusLimited},
		{ID: 4, Name: "node-d", Status: "connected"},
	}
	cfg := testConfig()
	cfg.BlockDuration = 30
	cfg.Failsafe = config.FailsafeConfig{Enabled: true, AfterMinutes: 20, CooldownMinutes: 60}
	manager, notifier, store := newTestManager(t, cfg, fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "203.0.113.9", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "203.0.113.9", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "203.0.113.9", "")); err != nil {
		t.Fatal(err)
	}

	waitForCondition(t, "failsafe reconnect", func() bool {
		return len(fp.reconnected()) == 2 && len(notifier.failsafeEvents()) == 1
	})
	if got := fp.reconnected(); len(got) != 2 || got[0] != 1 || got[1] != 4 {
		t.Fatalf("reconnected nodes = %v, want [1 4]", got)
	}
	record := mustBlock(t, store, "id:7")
	if !record.FailsafeTriggered {
		t.Errorf("FailsafeTriggered = false")
	}
	events := notifier.failsafeEvents()
	if len(events) != 1 || events[0].cause != nil {
		t.Fatalf("failsafe events = %+v", events)
	}
	if len(events[0].reconnected) != 2 || events[0].reconnected[0] != "node-a" || events[0].reconnected[1] != "node-d" {
		t.Errorf("reconnected names = %v", events[0].reconnected)
	}

	// Once the failsafe has run for this block, subsequent lines must not
	// restart every node again.
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	if calls := fp.nodeListCount(); calls != 1 {
		t.Errorf("node list calls = %d, want 1", calls)
	}
	if calls := fp.calls(); len(calls) != 1 {
		t.Errorf("user disable calls = %d, want 1", len(calls))
	}
}

func TestFailsafeSkipsPreExistingDisabledByDefault(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusDisabled})
	fp.nodes = []panel.Node{{ID: 1, Name: "node-a", Status: "connected"}}
	cfg := testConfig()
	cfg.BlockDuration = 30
	cfg.Failsafe = config.FailsafeConfig{Enabled: true, AfterMinutes: 20, CooldownMinutes: 60}
	manager, _, store := newTestManager(t, cfg, fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	record := mustBlock(t, store, "id:7")
	if !record.PreExistingDisabled {
		t.Fatalf("record = %+v", record)
	}
	if record.FailsafeTriggered {
		t.Error("pre-existing block triggered failsafe without IncludePreExisting")
	}
	if calls := fp.nodeListCount(); calls != 0 {
		t.Errorf("node list calls = %d, want 0", calls)
	}
	if got := fp.reconnected(); len(got) != 0 {
		t.Errorf("reconnected = %v", got)
	}
}

func TestFailsafeIncludesPreExistingWhenConfigured(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusDisabled})
	fp.nodes = []panel.Node{{ID: 1, Name: "node-a", Status: "connected"}}
	cfg := testConfig()
	cfg.BlockDuration = 30
	cfg.Failsafe = config.FailsafeConfig{
		Enabled:            true,
		AfterMinutes:       20,
		CooldownMinutes:    60,
		IncludePreExisting: true,
	}
	manager, notifier, _ := newTestManager(t, cfg, fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "pre-existing failsafe", func() bool {
		return len(fp.reconnected()) == 1 && len(notifier.failsafeEvents()) == 1
	})
}

func TestWhitelistSkipsIdentityWithoutPanelCall(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	cfg := testConfig()
	cfg.Whitelist = []string{"7", "  ", "alice"}
	manager, notifier, store := newTestManager(t, cfg, fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	if calls := fp.calls(); len(calls) != 0 {
		t.Errorf("disable calls = %+v", calls)
	}
	if idCalls, nameCalls := fp.counts(); idCalls != 0 || nameCalls != 0 {
		t.Errorf("panel queries = id:%d name:%d", idCalls, nameCalls)
	}
	if store.Len() != 0 {
		t.Errorf("store = %+v", store.List())
	}
	if len(notifier.blockEvents()) != 0 {
		t.Errorf("block events = %d", len(notifier.blockEvents()))
	}
}

func TestWhitelistMatchesUsernameAfterResolve(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	cfg := testConfig()
	cfg.Whitelist = []string{"alice"}
	manager, notifier, store := newTestManager(t, cfg, fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	if calls := fp.calls(); len(calls) != 0 {
		t.Errorf("disable calls = %+v", calls)
	}
	if idCalls, nameCalls := fp.counts(); idCalls != 1 || nameCalls != 0 {
		t.Errorf("panel queries = id:%d name:%d", idCalls, nameCalls)
	}
	if store.Len() != 0 {
		t.Errorf("store = %+v", store.List())
	}
	if len(notifier.blockEvents()) != 0 {
		t.Errorf("block events = %d", len(notifier.blockEvents()))
	}
}

func TestFailsafeFailureIsReportedAndRetryable(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	fp.nodes = []panel.Node{{ID: 1, Name: "node-a", Status: "connected"}}
	fp.reconnectErr = errors.New("node agent unavailable")
	cfg := testConfig()
	cfg.BlockDuration = 120
	cfg.Failsafe = config.FailsafeConfig{Enabled: true, AfterMinutes: 20, CooldownMinutes: 60}
	manager, notifier, store := newTestManager(t, cfg, fp, clock)

	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(20 * time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "failed failsafe event", func() bool {
		return len(notifier.failsafeEvents()) == 1
	})
	record := mustBlock(t, store, "id:7")
	if record.FailsafeTriggered {
		t.Error("failed failsafe must not mark the block as triggered")
	}
	events := notifier.failsafeEvents()
	if events[0].cause == nil || len(events[0].failed) != 1 {
		t.Errorf("failsafe event = %+v", events[0])
	}

	// After the cooldown, another detection retries and succeeds.
	fp.mu.Lock()
	fp.reconnectErr = nil
	fp.mu.Unlock()
	clock.Advance(60 * time.Minute)
	if err := manager.Handle(context.Background(), testDetection("7", "", "")); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "retried failsafe", func() bool {
		return len(fp.reconnected()) == 1 && len(notifier.failsafeEvents()) == 2
	})
	if record := mustBlock(t, store, "id:7"); !record.FailsafeTriggered {
		t.Error("successful retry did not mark the block")
	}
}

func TestInboundIsPersistedAndSentToAdmin(t *testing.T) {
	clock := newFakeClock()
	fp := newFakePanel(&panel.User{ID: 7, Username: "alice", Status: panel.StatusActive})
	manager, notifier, store := newTestManager(t, testConfig(), fp, clock)

	det := testDetection("7", "203.0.113.9", "")
	det.Inbound = "vless-in"
	if err := manager.Handle(context.Background(), det); err != nil {
		t.Fatal(err)
	}

	block := mustBlock(t, store, "id:7")
	if block.Inbound != "vless-in" {
		t.Errorf("stored inbound = %q", block.Inbound)
	}
	events := notifier.blockEvents()
	if len(events) != 1 || events[0].Inbound != "vless-in" {
		t.Errorf("block events = %+v", events)
	}
}
