package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/keepalive"
)

func newStore(t *testing.T, now time.Time) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "history")
	s := New()
	s.now = func() time.Time { return now }
	s.Configure(dir, 40)
	return s, dir
}

func TestAppendAndQuery(t *testing.T) {
	now := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	s, dir := newStore(t, now)
	events := []keepalive.Event{
		{Time: now.Add(-30 * time.Hour), Key: "old", Usage: &keepalive.Usage{CacheReadTokens: 1}},
		{Time: now.Add(-2 * time.Hour), Key: "a", Usage: &keepalive.Usage{CacheReadTokens: 100}},
		{Time: now.Add(-time.Hour), Key: "b", Error: "boom"},
	}
	for _, ev := range events {
		if err := s.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-10-23.jsonl")); err != nil {
		t.Fatalf("day file: %v", err)
	}
	got, err := s.Query(now.Add(-24 * time.Hour))
	if err != nil || len(got) != 2 || got[0].Key != "a" || got[1].Key != "b" {
		t.Fatalf("events %+v err %v", got, err)
	}
	if sum := Sum(got); sum.Pings != 2 || sum.Failed != 1 || sum.CacheReadTokens != 100 {
		t.Fatalf("totals %+v", sum)
	}
	if counts, err := s.CountByKey(now.Add(-48 * time.Hour)); err != nil || counts["old"] != 1 || counts["a"] != 1 || counts["b"] != 1 {
		t.Fatalf("counts %v err %v", counts, err)
	}
	// A cached day file is read again after it changes.
	s.Append(keepalive.Event{Time: now.Add(-time.Minute), Key: "c"})
	if got, _ := s.Query(now.Add(-24 * time.Hour)); len(got) != 3 {
		t.Fatalf("events %d after an append, want 3", len(got))
	}
}

func TestPruneDeletesOldDays(t *testing.T) {
	now := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	s, dir := newStore(t, now)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "2026-09-01.jsonl"), []byte("{}\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "2026-09-20.jsonl"), []byte("{}\n"), 0o600)
	if err := s.Append(keepalive.Event{Time: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-09-01.jsonl")); !os.IsNotExist(err) {
		t.Fatal("file older than 40 days kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-09-20.jsonl")); err != nil {
		t.Fatal("file within 40 days deleted")
	}
}

func TestGroup(t *testing.T) {
	now := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	events := []keepalive.Event{
		{Time: now, Key: "a", Session: "s", Model: "m1", Ping: 1, Usage: &keepalive.Usage{CacheReadTokens: 100, InputTokens: 2}},
		{Time: now.Add(time.Minute), Key: "b", Session: "s", Ping: 1, Error: "boom", Stopped: "error"},
		{Time: now.Add(5 * time.Minute), Key: "a", Session: "s", Model: "m2", Ping: 2, Usage: &keepalive.Usage{CacheReadTokens: 120, InputTokens: 3}},
		{Time: now.Add(9 * time.Minute), Key: "a", Session: "s", Model: "m2", Ping: 1, Usage: &keepalive.Usage{CacheReadTokens: 10, CacheCreationTokens: 130, InputTokens: 1}, Stopped: "cache_miss"},
		{Time: now.Add(2 * time.Minute), Session: "old"},
	}
	got := Group(events)
	if len(got) != 3 || got[0].Key != "a" || got[1].Session != "old" || got[2].Key != "b" {
		t.Fatalf("groups %+v", got)
	}
	a := got[0]
	if a.Pings != 3 || a.Hits != 2 || a.Misses != 1 || a.Failed != 0 || a.CacheReadTokens != 230 || a.CacheCreationTokens != 130 ||
		a.Context != 141 || a.Model != "m2" || !a.First.Equal(now) || !a.Last.Equal(now.Add(9*time.Minute)) || a.LastStopped != "cache_miss" {
		t.Fatalf("conversation a %+v", a)
	}
	if b := got[2]; b.Failed != 1 || b.LastError != "boom" || b.Context != 0 {
		t.Fatalf("conversation b %+v", b)
	}
}
