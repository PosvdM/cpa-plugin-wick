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
