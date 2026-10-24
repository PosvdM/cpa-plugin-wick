package history

import (
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/keepalive"
	"github.com/PosvdM/cpa-plugin-wick/internal/pricing"
)

func TestCompareScopedCountsAndViews(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return now.Add(time.Duration(min) * time.Minute) }
	prices := &pricing.Table{Models: []pricing.Model{{Family: "opus", Version: "5.5", Tiers: []pricing.Tier{{Rates: pricing.Rates{Input: 4, Write5m: 5, Write1h: 8, Read: 0.2}}}}}}
	hit := &keepalive.Usage{CacheReadTokens: 100000}
	ping := func(key string, min, n int) keepalive.Event {
		return keepalive.Event{Key: key, Time: at(min), Model: "claude-opus-5-5", Ping: n, Usage: hit, Returns: true}
	}
	events := []keepalive.Event{
		ping("old", -50, 1),
		// This round starts before coverage but returns exactly at since.
		ping("cross", -40, 1),
		{Key: "cross", Time: at(-30), Kind: keepalive.KindReturn, Saved: true, Context: 100000, Idle: 600},
		{Key: "request", Time: at(-30), Kind: keepalive.KindRequest, Model: "claude-opus-5-5", Usage: hit},
		ping("failed", -20, 1),
		{Key: "failed", Time: at(-19), Model: "claude-opus-5-5", Ping: 2, Error: "boom", Returns: true},
		{Key: "unknown", Time: at(-18), Model: "claude-opus-5-5", Ping: 1, Usage: hit},
		{Key: "unpriced", Time: at(-10), Model: "missing", Ping: 1, Usage: hit, Returns: true},
		ping("pending", -1, 1),
	}
	got := Compare(events, Group(events, now, prices), prices)
	if got.PingCount != 6 || got.Rounds != 5 || got.Failed != 1 || got.Unpriced != 1 || got.Results[ResultSaved] != 1 || got.Results[ResultFailed] != 1 || got.Results[ResultUnknown] != 1 || got.Results[ResultWasted] != 1 || got.Results[ResultPending] != 1 {
		t.Fatalf("scope: %+v", got)
	}
	if !near(got.Wick, got.Cache+got.Pings) || !near(got.None, got.Cache+got.Saved) || !near(got.Difference, got.Saved-got.Pings) || !near(got.HourDifference, got.Hour-got.Wick) || got.Percent == nil || !near(*got.Percent, got.Difference/got.None*100) {
		t.Fatalf("views: %+v", got)
	}
	// A pure ping cost with a zero no-Wick baseline has no percentage.
	zero := []keepalive.Event{{Time: at(-2), Kind: keepalive.KindRequest, Model: "claude-opus-5-5", Usage: &keepalive.Usage{}}, ping("pending", -1, 1)}
	cost := Compare(zero, Group(zero, now, prices), prices)
	if cost.Percent != nil || cost.Difference >= 0 || cost.None != 0 {
		t.Fatalf("zero baseline: %+v", cost)
	}
	if empty := Compare(events[0:1], Group(events[0:1], now, prices), prices); empty.Since != nil || empty.Rounds != 0 || empty.PingCount != 0 {
		t.Fatalf("no usage: %+v", empty)
	}
}
