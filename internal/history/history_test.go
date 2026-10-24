package history

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/keepalive"
	"github.com/PosvdM/cpa-plugin-wick/internal/pricing"
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
	if sum := Total(Group(got, now, nil)); sum.Pings != 2 || sum.Failed != 1 || sum.CacheReadTokens != 100 {
		t.Fatalf("totals %+v", sum)
	}
	s.Append(keepalive.Event{Time: now.Add(-90 * time.Minute), Key: "a", Kind: keepalive.KindReturn})
	if counts, err := s.CountByKey(now.Add(-48 * time.Hour)); err != nil || counts["old"] != 1 || counts["a"] != 1 || counts["b"] != 1 {
		t.Fatalf("counts %v err %v", counts, err)
	}
	// A cached day file is read again after it changes.
	s.Append(keepalive.Event{Time: now.Add(-time.Minute), Key: "c"})
	if got, _ := s.Query(now.Add(-24 * time.Hour)); len(got) != 4 {
		t.Fatalf("events %d after an append, want 4", len(got))
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
	got := Group(events, now.Add(time.Hour), nil)
	if len(got) != 3 || got[0].Key != "a" || got[1].Session != "old" || got[2].Key != "b" {
		t.Fatalf("groups %+v", got)
	}
	a := got[0]
	if a.Pings != 3 || a.Rounds != 2 || a.Failed != 0 || a.CacheReadTokens != 230 || a.CacheCreationTokens != 130 ||
		a.Context != 141 || a.Model != "m2" || !a.First.Equal(now) || !a.Last.Equal(now.Add(9*time.Minute)) || a.LastStopped != "cache_miss" {
		t.Fatalf("conversation a %+v", a)
	}
	// Written before returns were recorded, so the first round is unknown.
	if r := a.Totals.Results; r[ResultMiss] != 1 || r[ResultUnknown] != 1 {
		t.Fatalf("results %v", r)
	}
	if b := got[2]; b.Failed != 1 || b.LastError != "boom" || b.Context != 0 || b.Totals.Results[ResultFailed] != 1 {
		t.Fatalf("conversation b %+v", b)
	}
}

func TestRoundsAndAmounts(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return now.Add(time.Duration(minutes) * time.Minute) }
	hit := &keepalive.Usage{CacheReadTokens: 100000}
	ping := func(minutes, n int, usage *keepalive.Usage) keepalive.Event {
		return keepalive.Event{Time: at(minutes), Key: "a", Model: "claude-opus-5-5", Ping: n, Usage: usage, Returns: true}
	}
	back := func(minutes int, saved bool) keepalive.Event {
		return keepalive.Event{Time: at(minutes), Key: "a", Kind: keepalive.KindReturn, Returns: true, Saved: saved, Context: 100000, Idle: 600}
	}
	events := []keepalive.Event{
		// Saved: back within the cache's lifetime after the last ping.
		ping(-60, 1, hit), ping(-55, 2, hit), back(-52, true),
		// Wasted: no return within five minutes of the last ping.
		ping(-40, 1, hit),
		// Wasted: back too early.
		ping(-30, 1, hit), back(-29, false),
		// Miss: the ping wrote the cache.
		ping(-10, 1, &keepalive.Usage{CacheCreationTokens: 100000}),
		// Pending: the cache of the last ping is alive.
		ping(-2, 1, hit),
		// A return without a ping of its round in the range is left out.
		{Time: at(-1), Key: "z", Kind: keepalive.KindReturn, Saved: true},
		// No price for the model.
		{Time: at(-20), Key: "b", Model: "gpt-5", Ping: 1, Usage: hit, Returns: true},
	}
	prices := &pricing.Table{Models: []pricing.Model{{Family: "opus", Version: "5.5", Tiers: []pricing.Tier{{Rates: pricing.Rates{Input: 4, Write5m: 5, Write1h: 8, Read: 0.2, Output: 20}}}}}}
	got := Group(events, now, prices)
	if len(got) != 2 || got[0].Key != "a" {
		t.Fatalf("groups %+v", got)
	}
	a := got[0]
	want := []string{ResultPending, ResultMiss, ResultWasted, ResultWasted, ResultSaved}
	if len(a.Detail) != len(want) {
		t.Fatalf("rounds %+v", a.Detail)
	}
	for i, r := range a.Detail {
		if r.Result != want[i] {
			t.Errorf("round %d result %s, want %s", i, r.Result, want[i])
		}
	}
	saved := a.Detail[4]
	if saved.Pings != 2 || saved.Return == nil || saved.Idle != 600 || saved.Saved == nil || !near(*saved.Saved, 0.48) || !near(*saved.Cost, 0.04) {
		t.Fatalf("saved round %+v", saved)
	}
	if a.Saved != *saved.Saved || !near(a.Cost, 0.6) || !near(a.Net, 0.48-0.58) || a.Unpriced != 0 {
		t.Fatalf("totals %+v", a.Totals)
	}
	if b := got[1]; b.Detail[0].Cost != nil || b.Unpriced != 1 || b.Cost != 0 {
		t.Fatalf("unpriced conversation %+v", b)
	}
	total := Total(got)
	if total.Rounds != 6 || total.Results[ResultWasted] != 3 || total.Unpriced != 1 || !near(total.Net, a.Net) {
		t.Fatalf("total %+v", total)
	}
	// Without prices there are no amounts, and no round counts as unpriced.
	noPrices := Group(events, now, nil)
	if r := noPrices[0].Detail[4]; r.Cost != nil || r.Saved != nil || r.Result != ResultSaved {
		t.Fatalf("round without prices %+v", r)
	}
	if u := Total(noPrices).Unpriced; u != 0 {
		t.Fatalf("unpriced %d without prices", u)
	}
	// A ping in flight when the request came back may fail after the
	// return; the return decides.
	late := []keepalive.Event{ping(-20, 1, hit), {Time: at(-18), Key: "a", Model: "claude-opus-5-5", Ping: 2, Error: "boom", Returns: true}, back(-17, true)}
	if r := Group(late, now, prices)[0].Detail[0]; r.Result != ResultSaved || r.Pings != 2 {
		t.Fatalf("round with a failed ping in flight %+v", r)
	}
}

func TestRoundCacheReadAndWriteResult(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, stopped, want string
		read, write         int64
	}{
		{"no-cache", "cache_miss", ResultMiss, 0, 0},
		{"write-only", "cache_miss", ResultMiss, 0, 5000},
		{"zero-usage-without-stop", "", ResultMiss, 0, 0},
		{"read", "", ResultPending, 40000, 0},
		{"read-and-write", "", ResultMiss, 40000, 5000},
		{"write-only-without-stop", "", ResultMiss, 0, 5000},
		{"recorded-miss", "cache_miss", ResultMiss, 40000, 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []keepalive.Event{{Time: now.Add(-time.Minute), Key: "a", Ping: 1, Returns: true, Stopped: tc.stopped,
				Usage: &keepalive.Usage{CacheReadTokens: tc.read, CacheCreationTokens: tc.write, InputTokens: 3}}}
			got := Group(events, now, nil)
			if len(got) != 1 || len(got[0].Detail) != 1 || got[0].Detail[0].Result != tc.want {
				t.Fatalf("groups %+v, want %s", got, tc.want)
			}
		})
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCompare(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return now.Add(time.Duration(minutes) * time.Minute) }
	request := func(minutes int, idle, context, read, write int64) keepalive.Event {
		return keepalive.Event{Time: at(minutes), Key: "a", Model: "claude-opus-5-5", Kind: keepalive.KindRequest, Idle: idle, Context: context,
			Usage: &keepalive.Usage{CacheReadTokens: read, CacheCreationTokens: write, InputTokens: 1}}
	}
	hit := &keepalive.Usage{CacheReadTokens: 100000}
	events := []keepalive.Event{
		// A round that ended before the first request with usage is left
		// out; one that ended after it counts, though it started before.
		{Time: at(-120), Key: "a", Model: "claude-opus-5-5", Ping: 1, Usage: hit, Returns: true},
		{Time: at(-104), Key: "c", Model: "claude-opus-5-5", Ping: 1, Usage: hit, Returns: true},
		{Time: at(-99), Key: "c", Kind: keepalive.KindReturn, Returns: true, Saved: true, Context: 100000, Idle: 600},
		// A new conversation writes everything either way.
		request(-100, 0, 0, 0, 100000),
		// Back after 10 minutes: the 5-minute cache expired, the 1-hour
		// one would hold the previous input.
		request(-90, 600, 100000, 0, 110000),
		// Back after 2 minutes: a hit either way.
		request(-80, 120, 110000, 110000, 5000),
		// Kept lit for more than an hour: saved here, a write with a
		// 1-hour cache.
		{Time: at(-20), Key: "a", Model: "claude-opus-5-5", Ping: 1, Usage: hit, Returns: true},
		{Time: at(-18), Key: "a", Kind: keepalive.KindReturn, Returns: true, Saved: true, Context: 100000, Idle: 4000},
		request(-18, 0, 0, 100000, 0),
		{Time: at(-10), Key: "b", Model: "gpt-5", Kind: keepalive.KindRequest, Usage: hit},
	}
	prices := &pricing.Table{Models: []pricing.Model{{Family: "opus", Version: "5.5", Tiers: []pricing.Tier{{Rates: pricing.Rates{Input: 4, Write5m: 5, Write1h: 8, Read: 0.2, Output: 20}}}}}}
	convs := Group(events, now, prices)
	if len(convs) != 2 || len(convs[0].Detail) != 2 {
		t.Fatalf("requests changed the grouping: %+v", convs)
	}
	got := Compare(events, convs, prices)
	if got.Since == nil || !got.Since.Equal(at(-100)) || got.Requests != 4 || got.Unpriced != 1 {
		t.Fatalf("comparison %+v", got)
	}
	// Cache: 0.5 + 0.55 + (0.022 + 0.025) + 0.02.
	// Hour: 0.8 + (0.02 + 0.08) + (0.022 + 0.04) + 0.02 + 0.78 for the
	// return kept lit past an hour.
	if !near(got.Cache, 1.117) || !near(got.Hour, 1.762) || !near(got.Pings, 0.04) || !near(got.Saved, 0.96) {
		t.Fatalf("amounts %+v", got)
	}
	if empty := Compare(events, convs, nil); empty.Since != nil || empty.Requests != 0 {
		t.Fatalf("comparison without prices %+v", empty)
	}
}
