package history

import (
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/keepalive"
	"github.com/PosvdM/cpa-plugin-wick/internal/pricing"
)

func oldSavedPrices() *pricing.Table {
	return &pricing.Table{Models: []pricing.Model{{Family: "opus", Version: "5.5", Tiers: []pricing.Tier{{Rates: pricing.Rates{Input: 4, Write5m: 5, Write1h: 8, Read: 0.2}}}}}}
}

func TestReturnedRoundCacheMiss(t *testing.T) {
	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	hit := &keepalive.Usage{CacheReadTokens: 100000}
	zero := &keepalive.Usage{}
	write := &keepalive.Usage{CacheCreationTokens: 100000}
	mixed := &keepalive.Usage{CacheReadTokens: 100000, CacheCreationTokens: 5000}
	type ping struct {
		usage   *keepalive.Usage
		stopped string
		err     string
	}
	for _, tc := range []struct {
		name        string
		pings       []ping
		returnAfter time.Duration
		noReturn    bool
		saved       bool
		legacy      bool
		nowAfter    time.Duration
		want        string
	}{
		{name: "legacy-zero-without-stop", pings: []ping{{usage: zero}}, saved: true, legacy: true, want: ResultWasted},
		{name: "legacy-input-without-read", pings: []ping{{usage: &keepalive.Usage{InputTokens: 100000}}}, saved: true, legacy: true, want: ResultWasted},
		{name: "all-misses-with-context", pings: []ping{{usage: write}, {usage: &keepalive.Usage{InputTokens: 100000}}}, saved: true, want: ResultWasted},
		{name: "hit-then-miss-before-expiry", pings: []ping{{usage: hit}, {usage: zero}}, returnAfter: cacheTTL - time.Nanosecond, saved: true, want: ResultSaved},
		{name: "hit-then-miss-at-expiry", pings: []ping{{usage: hit}, {usage: zero}}, returnAfter: cacheTTL, saved: true, want: ResultWasted},
		{name: "hit-then-miss-after-expiry", pings: []ping{{usage: hit}, {usage: zero}}, returnAfter: cacheTTL + time.Nanosecond, saved: true, want: ResultWasted},
		{name: "miss-then-hit", pings: []ping{{usage: zero}, {usage: hit}}, saved: true, want: ResultSaved},
		{name: "write-only", pings: []ping{{usage: write}}, saved: true, want: ResultWasted},
		{name: "read-and-write", pings: []ping{{usage: mixed}}, saved: true, want: ResultWasted},
		{name: "stopped-with-zero", pings: []ping{{usage: zero, stopped: "cache_miss"}}, saved: true, want: ResultWasted},
		{name: "stopped-with-write", pings: []ping{{usage: write, stopped: "cache_miss"}}, saved: true, want: ResultWasted},
		{name: "stopped-with-read", pings: []ping{{usage: hit, stopped: "cache_miss"}}, saved: true, want: ResultWasted},
		{name: "stopped-without-usage", pings: []ping{{stopped: "cache_miss"}}, saved: true, want: ResultWasted},
		{name: "hit-then-write-within-ttl", pings: []ping{{usage: hit}, {usage: write}}, saved: true, want: ResultSaved},
		{name: "hit-then-mixed-within-ttl", pings: []ping{{usage: hit}, {usage: mixed}}, saved: true, want: ResultSaved},
		{name: "hit-then-stopped-within-ttl", pings: []ping{{usage: hit}, {usage: zero, stopped: "cache_miss"}}, saved: true, want: ResultSaved},
		{name: "normal-saved", pings: []ping{{usage: hit}}, saved: true, want: ResultSaved},
		{name: "return-not-saved", pings: []ping{{usage: hit}}, want: ResultWasted},
		{name: "miss-return-not-saved", pings: []ping{{usage: zero}}, want: ResultWasted},
		{name: "hit-miss-return-not-saved", pings: []ping{{usage: hit}, {usage: zero}}, want: ResultWasted},
		{name: "miss-without-return", pings: []ping{{usage: zero}}, noReturn: true, want: ResultMiss},
		{name: "failed-with-saved", pings: []ping{{usage: hit}, {err: "boom"}}, saved: true, want: ResultSaved},
		{name: "failed-with-saved-after-expiry", pings: []ping{{usage: hit}, {err: "boom"}}, returnAfter: cacheTTL + time.Second, saved: true, want: ResultSaved},
		{name: "failed-without-hit-saved", pings: []ping{{err: "boom"}}, saved: true, want: ResultSaved},
		{name: "failed-without-return", pings: []ping{{err: "boom"}}, noReturn: true, want: ResultFailed},
		{name: "pending", pings: []ping{{usage: hit}}, noReturn: true, nowAfter: time.Minute, want: ResultPending},
		{name: "unknown", pings: []ping{{usage: hit}}, noReturn: true, legacy: true, want: ResultUnknown},
		{name: "nil-usage-without-stop", pings: []ping{{}}, saved: true, want: ResultSaved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []keepalive.Event
			for i, p := range tc.pings {
				events = append(events, keepalive.Event{Time: start.Add(time.Duration(i) * time.Minute), Key: "a", Model: "claude-opus-5-5", Ping: i + 1, Usage: p.usage, Stopped: p.stopped, Error: p.err, Returns: !tc.legacy})
			}
			if !tc.noReturn {
				after := tc.returnAfter
				if after == 0 {
					after = time.Duration(len(tc.pings)) * time.Minute
				}
				events = append(events, keepalive.Event{Time: start.Add(after), Key: "a", Kind: keepalive.KindReturn, Saved: tc.saved, Context: 100000, Idle: 600})
			}
			nowAfter := tc.nowAfter
			if nowAfter == 0 {
				nowAfter = time.Hour
			}
			for _, prices := range []*pricing.Table{oldSavedPrices(), nil} {
				convs := Group(events, start.Add(nowAfter), prices)
				if len(convs) != 1 || len(convs[0].Detail) != 1 {
					t.Fatalf("groups %+v", convs)
				}
				r := convs[0].Detail[0]
				if r.Result != tc.want {
					t.Fatalf("result %q, want %q: %+v", r.Result, tc.want, r)
				}
				if tc.want == ResultSaved && prices != nil {
					if r.Saved == nil || !near(*r.Saved, 0.48) {
						t.Fatalf("saved amount %+v", r)
					}
				} else if r.Saved != nil {
					t.Fatalf("unexpected saved amount: %+v", r)
				}
			}
		})
	}
}

func TestStoreQueryInFlightMissReturn(t *testing.T) {
	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		returnAfter time.Duration
		want        string
	}{
		{"before-expiry", cacheTTL - time.Second, ResultSaved},
		{"at-expiry", cacheTTL, ResultWasted},
		{"after-expiry", cacheTTL + time.Second, ResultWasted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newStore(t, start.Add(time.Hour))
			events := []keepalive.Event{
				{Time: start, Key: "a", Model: "claude-opus-5-5", Ping: 1, Returns: true, Usage: &keepalive.Usage{CacheReadTokens: 100000}},
				{Time: start.Add(tc.returnAfter), Key: "a", Kind: keepalive.KindReturn, Saved: true, Context: 100000, Idle: 600},
				// 在途挑灯后完成，Time 仍是回来前的开始时间。
				{Time: start.Add(4 * time.Minute), Key: "a", Model: "claude-opus-5-5", Ping: 2, Returns: true, Usage: &keepalive.Usage{}},
			}
			for _, ev := range events {
				if err := s.Append(ev); err != nil {
					t.Fatal(err)
				}
			}
			ordered, err := s.Query(start)
			if err != nil {
				t.Fatal(err)
			}
			if len(ordered) != 3 || ordered[1].Ping != 2 || ordered[2].Kind != keepalive.KindReturn {
				t.Fatalf("query order %+v", ordered)
			}
			convs := Group(ordered, start.Add(time.Hour), oldSavedPrices())
			if len(convs) != 1 || len(convs[0].Detail) != 1 {
				t.Fatalf("groups %+v", convs)
			}
			r := convs[0].Detail[0]
			if r.Result != tc.want || r.Pings != 2 || r.last != ResultMiss || r.Return == nil || !r.Return.Equal(events[1].Time) {
				t.Fatalf("round %+v, want %s", r, tc.want)
			}
			if (r.Saved != nil) != (tc.want == ResultSaved) {
				t.Fatalf("saved amount %+v", r)
			}
		})
	}
}

func TestOldSavedRoundTotalsAndComparison(t *testing.T) {
	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name              string
		hit               bool
		input             int64
		returnAfter       time.Duration
		idle              int64
		want              string
		cost, saved, hour float64
	}{
		{"legacy-zero", false, 0, 2 * time.Minute, 600, ResultWasted, 0, 0, 0.8},
		{"legacy-input", false, 100000, 2 * time.Minute, 600, ResultWasted, 0.4, 0, 0.8},
		{"in-flight-miss", true, 100000, cacheTTL - time.Second, 600, ResultSaved, 0.42, 0.48, 0.8},
		{"expired-hit", true, 100000, cacheTTL, 600, ResultWasted, 0.42, 0, 0.8},
		{"hour-boundary-saved", true, 100000, cacheTTL - time.Second, 3600, ResultSaved, 0.42, 0.48, 1.58},
		{"hour-boundary-wasted", true, 100000, cacheTTL, 3600, ResultWasted, 0.42, 0, 0.8},
		{"over-hour-saved", true, 100000, cacheTTL - time.Second, 4000, ResultSaved, 0.42, 0.48, 1.58},
		{"over-hour-wasted", true, 100000, cacheTTL, 4000, ResultWasted, 0.42, 0, 0.8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []keepalive.Event{{Time: start.Add(-time.Minute), Key: "a", Kind: keepalive.KindRequest, Model: "claude-opus-5-5", Usage: &keepalive.Usage{CacheCreationTokens: 100000}}}
			if tc.hit {
				events = append(events, keepalive.Event{Time: start, Key: "a", Model: "claude-opus-5-5", Ping: 1, Usage: &keepalive.Usage{CacheReadTokens: 100000}})
			}
			events = append(events,
				keepalive.Event{Time: start.Add(time.Minute), Key: "a", Model: "claude-opus-5-5", Ping: 2, Usage: &keepalive.Usage{InputTokens: tc.input}},
				keepalive.Event{Time: start.Add(tc.returnAfter), Key: "a", Kind: keepalive.KindReturn, Saved: true, Context: 100000, Idle: tc.idle})
			prices := oldSavedPrices()
			convs := Group(events, start.Add(time.Hour), prices)
			total := Total(convs)
			wantSaved, wantWasted := 0, 1
			if tc.want == ResultSaved {
				wantSaved, wantWasted = 1, 0
			}
			if total.Rounds != 1 || total.Results[ResultSaved] != wantSaved || total.Results[ResultWasted] != wantWasted || !near(total.Cost, tc.cost) || !near(total.Saved, tc.saved) || !near(total.Net, tc.saved-tc.cost) {
				t.Fatalf("totals %+v", total)
			}
			got := Compare(events, convs, prices)
			if got.Rounds != 1 || got.PingCount != total.Pings || got.Results[ResultSaved] != wantSaved || got.Results[ResultWasted] != wantWasted || !near(got.Cache, 0.5) || !near(got.Pings, tc.cost) || !near(got.Saved, tc.saved) || !near(got.Wick, 0.5+tc.cost) || !near(got.None, 0.5+tc.saved) || !near(got.Difference, tc.saved-tc.cost) || !near(got.Difference, total.Net) {
				t.Fatalf("comparison %+v", got)
			}
			if !near(got.Hour, tc.hour) || !near(got.HourDifference, tc.hour-(0.5+tc.cost)) {
				t.Fatalf("hour comparison %+v, want hour %v and difference %v", got, tc.hour, tc.hour-(0.5+tc.cost))
			}
		})
	}
}

func TestReturnedRoundLastReadIsolation(t *testing.T) {
	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	hit := &keepalive.Usage{CacheReadTokens: 100000}
	miss := &keepalive.Usage{InputTokens: 1000}
	ping := func(minutes, n int, usage *keepalive.Usage) keepalive.Event {
		return keepalive.Event{Time: start.Add(time.Duration(minutes) * time.Minute), Key: "a", Model: "claude-opus-5-5", Ping: n, Usage: usage, Returns: true}
	}
	back := func(minutes int) keepalive.Event {
		return keepalive.Event{Time: start.Add(time.Duration(minutes) * time.Minute), Key: "a", Kind: keepalive.KindReturn, Saved: true, Context: 100000, Idle: 600}
	}
	for _, tc := range []struct {
		name     string
		events   []keepalive.Event
		want     []string
		lastRead time.Time
	}{
		{
			name:   "return-starts-isolated-round",
			events: []keepalive.Event{ping(0, 1, hit), back(1), ping(2, 2, miss), ping(3, 3, miss), back(4)},
			want:   []string{ResultWasted, ResultSaved},
		},
		{
			name:   "ping-reset-starts-isolated-round",
			events: []keepalive.Event{ping(0, 1, hit), ping(1, 2, hit), ping(2, 1, miss), ping(3, 2, miss), back(4)},
			want:   []string{ResultWasted, ResultWasted},
		},
		{
			name:     "latest-hit-before-expiry",
			events:   []keepalive.Event{ping(0, 1, hit), ping(4, 2, hit), ping(5, 3, miss), back(8)},
			want:     []string{ResultSaved},
			lastRead: start.Add(4 * time.Minute),
		},
		{
			name:     "latest-hit-at-expiry",
			events:   []keepalive.Event{ping(0, 1, hit), ping(4, 2, hit), ping(5, 3, miss), back(9)},
			want:     []string{ResultWasted},
			lastRead: start.Add(4 * time.Minute),
		},
		{
			name:     "latest-hit-after-expiry",
			events:   []keepalive.Event{ping(0, 1, hit), ping(4, 2, hit), ping(5, 3, miss), back(10)},
			want:     []string{ResultWasted},
			lastRead: start.Add(4 * time.Minute),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			convs := Group(tc.events, start.Add(time.Hour), oldSavedPrices())
			if len(convs) != 1 || len(convs[0].Detail) != len(tc.want) {
				t.Fatalf("groups %+v", convs)
			}
			for i, r := range convs[0].Detail {
				if r.Result != tc.want[i] {
					t.Errorf("round %d result %q, want %q", i, r.Result, tc.want[i])
				}
				if tc.want[i] == ResultSaved {
					if r.Saved == nil || !near(*r.Saved, 0.48) {
						t.Errorf("round %d saved amount %+v", i, r)
					}
				} else if r.Saved != nil {
					t.Errorf("round %d unexpected saved amount %+v", i, r)
				}
			}
			if r := convs[0].Detail[0]; !r.lastRead.Equal(tc.lastRead) || r.last != ResultMiss {
				t.Fatalf("latest round %+v, want lastRead %v and a final miss", r, tc.lastRead)
			}
		})
	}
}
