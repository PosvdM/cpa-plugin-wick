package keepalive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// saveNow writes the conversations without waiting for the store's
// goroutine.
func saveNow(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	s := m.store
	m.mu.Unlock()
	convs, ok := m.snapshot(s)
	if !ok {
		t.Fatal("no store")
	}
	if err := s.write(convs); err != nil {
		t.Fatal(err)
	}
}

func restoredManager(dir string, at time.Time) (*Manager, *fakeClock, *fakeHost) {
	clock := &fakeClock{now: at}
	h := &fakeHost{cacheRead: 40000}
	m := New(Options{Host: h, Clock: clock, Rand: func() float64 { return 0 }})
	m.Configure(testConfig())
	m.SetStore(dir)
	return m, clock, h
}

func TestRestoreKeepsSchedule(t *testing.T) {
	dir := t.TempDir()
	m, clock, h := newTestManager(testConfig())
	m.SetStore(dir)
	m.Observe(intercept("r1", "sess", body("main", "1")))
	key := conversationKey("sess", "", "claude-opus-5-5", body("main", "1"))
	clock.Advance(260 * time.Second)
	m.SetDuration(short(key), 2)
	saveNow(t, m)
	m.Stop()
	if _, err := os.Stat(filepath.Join(dir, key+requestExt)); err != nil {
		t.Fatalf("request file removed by Stop: %v", err)
	}

	// The new instance starts 100 s after the first ping, before the next
	// one is due at 520 s.
	m2, clock2, h2 := restoredManager(dir, start.Add(360*time.Second))
	defer m2.Stop()
	convs := m2.Status().Conversations
	if len(convs) != 1 {
		t.Fatalf("conversations %+v", convs)
	}
	c := convs[0]
	if c.Pings != 1 || !c.NextPing.Equal(start.Add(520*time.Second)) || !c.LastRequest.Equal(start) || c.Until == nil || c.Hours != 2 {
		t.Fatalf("restored %+v", c)
	}
	clock2.Advance(160 * time.Second)
	if h2.pings() != 1 {
		t.Fatalf("pings %d after the saved next ping", h2.pings())
	}
	sent := h2.requests[0]
	if string(sent.Body) != string(h.requests[0].Body) || sent.AuthID != "claude-a.json" || sent.Headers.Get("X-Claude-Code-Session-Id") != "sess" || sent.Headers.Get("Authorization") != "" {
		t.Fatalf("replay after restore %+v", sent)
	}
	if got := m2.Status().Conversations[0].Pings; got != 2 {
		t.Fatalf("pings counted %d, want 2", got)
	}
}

func TestRestorePingsOverdueConversationSoon(t *testing.T) {
	dir := t.TempDir()
	m, _, _ := newTestManager(testConfig())
	m.SetStore(dir)
	m.Observe(intercept("r1", "sess", body("main", "1")))
	saveNow(t, m)
	m.Stop()

	// The ping was due at 260 s; the cache lasts until 300 s.
	m2, clock2, h2 := restoredManager(dir, start.Add(270*time.Second))
	defer m2.Stop()
	if c := m2.Status().Conversations; len(c) != 1 || !c[0].NextPing.Equal(start.Add(270*time.Second+restoreDelay)) {
		t.Fatalf("restored %+v", c)
	}
	clock2.Advance(restoreDelay)
	if h2.pings() != 1 {
		t.Fatalf("pings %d", h2.pings())
	}
}

func TestRestoreSkipsExpiredCache(t *testing.T) {
	dir := t.TempDir()
	m, _, _ := newTestManager(testConfig())
	m.SetStore(dir)
	m.Observe(intercept("r1", "sess", body("main", "1")))
	saveNow(t, m)
	m.Stop()

	m2, _, _ := restoredManager(dir, start.Add(290*time.Second))
	defer m2.Stop()
	if c := m2.Status().Conversations; len(c) != 0 {
		t.Fatalf("restored a conversation whose cache expires before the ping: %+v", c)
	}
}

func TestStopSavesLatestState(t *testing.T) {
	dir := t.TempDir()
	m, clock, _ := newTestManager(testConfig())
	m.SetStore(dir)
	m.Observe(intercept("r1", "sess", body("main", "1")))
	saveNow(t, m)
	// Changed less than saveDelay before Stop: a new turn and a second
	// conversation that is snuffed out.
	clock.Advance(100 * time.Second)
	m.Observe(intercept("r2", "sess", body("main", "2")))
	m.Observe(intercept("r3", "other", body("main", "1")))
	m.StopConversation(short(conversationKey("other", "", "claude-opus-5-5", body("main", "1"))))
	m.Stop()

	m2, clock2, h2 := restoredManager(dir, start.Add(150*time.Second))
	defer m2.Stop()
	convs := m2.Status().Conversations
	if len(convs) != 1 || !convs[0].LastRequest.Equal(start.Add(100*time.Second)) {
		t.Fatalf("restored %+v", convs)
	}
	clock2.Advance(time.Hour)
	if h2.pings() == 0 || string(h2.requests[0].Body) != string(body("main", "2")) {
		t.Fatal("restored the turn before the last request")
	}
}

func TestRestoreBringsLatePingForward(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.IntervalMin, cfg.IntervalMax = 299, 299
	m, _, _ := newTestManager(cfg)
	m.SetStore(dir)
	m.Observe(intercept("r1", "sess", body("main", "1")))
	m.Stop()

	m2, _, _ := restoredManager(dir, start.Add(100*time.Second))
	defer m2.Stop()
	if c := m2.Status().Conversations; len(c) != 1 || !c[0].NextPing.Equal(start.Add(cacheTTL-restoreMargin)) {
		t.Fatalf("restored %+v", c)
	}
}

func TestEndedConversationFilesAreDeleted(t *testing.T) {
	dir := t.TempDir()
	m, _, _ := newTestManager(testConfig())
	m.SetStore(dir)
	defer m.Stop()
	m.Observe(intercept("r1", "sess", body("main", "1")))
	key := conversationKey("sess", "", "claude-opus-5-5", body("main", "1"))
	saveNow(t, m)
	if info, err := os.Stat(filepath.Join(dir, key+requestExt)); err != nil || (info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Fatalf("request file %v err %v", info, err)
	}
	m.StopConversation(short(key))
	saveNow(t, m)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != indexFile {
		t.Fatalf("files %v, want only the index", entries)
	}
}
