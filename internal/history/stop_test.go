package history

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/keepalive"
	"github.com/PosvdM/cpa-plugin-wick/internal/pricing"
)

func TestStoreStopEvents(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	store, _ := newStore(t, now)
	events := []keepalive.Event{
		{Time: now.Add(-time.Minute), Key: "a", Ping: 1, Usage: &keepalive.Usage{CacheReadTokens: 100}},
		{Time: now.Add(-50 * time.Second), Key: "a", Kind: keepalive.KindStop, Stopped: "manual", Pings: 1},
		{Time: now.Add(-40 * time.Second), Key: "a", Kind: keepalive.KindReturn, Saved: true, Context: 100, Idle: 600},
		{Time: now.Add(-40 * time.Second), Key: "a", Kind: keepalive.KindRequest},
		{Time: now.Add(-30 * time.Second), Key: "orphan", Kind: keepalive.KindStop, Stopped: "evicted", Pings: 1},
	}
	for _, event := range events {
		if err := store.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	from := now.Add(-time.Hour)
	got, err := store.Query(from)
	if err != nil || len(got) != len(events) || got[1].Kind != keepalive.KindStop || got[1].Stopped != "manual" || got[1].Pings != 1 {
		t.Fatalf("persisted stop: %+v err=%v", got, err)
	}
	counts, err := store.CountByKey(from)
	if err != nil || len(counts) != 1 || counts["a"] != 1 {
		t.Fatalf("stop changed ping counts: %v err=%v", counts, err)
	}
	convs := Group(got, now, nil)
	if len(convs) != 1 || convs[0].Detail[0].End != "manual" || convs[0].Detail[0].Result != ResultSaved {
		t.Fatalf("persisted return did not override stop: %+v", convs)
	}
}

func TestRoundEndJudgment(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, end, last, want string
		age                   time.Duration
		returned, saved       bool
	}{
		{"saved", "manual", ResultFailed, ResultSaved, time.Minute, true, true},
		{"early-return", "replaced", ResultMiss, ResultWasted, time.Minute, true, false},
		{"failed", "manual", ResultFailed, ResultFailed, time.Minute, false, false},
		{"miss", "replaced", ResultMiss, ResultMiss, time.Minute, false, false},
		{"key-changed", "replaced", "", ResultReplaced, time.Minute, false, false},
		{"manual", "manual", "", ResultEnded, time.Minute, false, false},
		{"subagent", "superseded", "", ResultEnded, time.Minute, false, false},
		{"evicted", "evicted", "", ResultEnded, time.Minute, false, false},
		{"until-warm", "until", "", ResultPending, time.Minute, false, false},
		{"until-expired", "until", "", ResultEnded, cacheTTL, false, false},
		{"legacy-warm", "", "", ResultPending, time.Minute, false, false},
		{"legacy-expired", "", "", ResultEnded, cacheTTL, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := now.Add(-tc.age)
			ping := keepalive.Event{Time: at, Key: "key", Model: "model", Ping: 1, Usage: &keepalive.Usage{CacheReadTokens: 100}}
			if tc.last == ResultFailed {
				ping.Error = "failure"
			} else if tc.last == ResultMiss {
				ping.Stopped = "cache_miss"
			} else if tc.end == "until" {
				ping.Stopped = "until"
			}
			events := []keepalive.Event{ping}
			if tc.end != "" && tc.end != "until" {
				events = append(events, keepalive.Event{Time: at.Add(time.Second), Key: "key", Kind: keepalive.KindStop, Stopped: tc.end, Pings: 1})
			}
			if tc.returned {
				events = append(events, keepalive.Event{Time: at.Add(2 * time.Second), Key: "key", Kind: keepalive.KindReturn, Saved: tc.saved, Idle: 600, Context: 100})
			}
			got := Group(events, now, nil)
			if len(got) != 1 || got[0].Rounds != 1 || got[0].Pings != 1 || len(got[0].Detail) != 1 {
				t.Fatalf("stop changed round or ping counts: %+v", got)
			}
			r := got[0].Detail[0]
			if r.Result != tc.want || r.End != tc.end || !r.Start.Equal(at) || !r.Last.Equal(at) || !got[0].Last.Equal(at) {
				t.Fatalf("round %+v, want result=%s end=%s", r, tc.want, tc.end)
			}
			if tc.returned != (r.Return != nil) {
				t.Fatalf("return lost after stop: %+v", r)
			}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if _, exists := fields["end"]; exists != (tc.end != "") {
				t.Fatalf("end omitempty: %s", raw)
			}
		})
	}
}

func TestStopDoesNotOpenOrCloseRound(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	at := func(seconds int) time.Time { return now.Add(time.Duration(seconds) * time.Second) }
	hit := &keepalive.Usage{CacheReadTokens: 100}
	events := []keepalive.Event{
		{Time: at(-500), Key: "orphan", Kind: keepalive.KindStop, Stopped: "manual"},
		{Time: at(-400), Key: "a", Ping: 1, Stopped: "until", Usage: hit},
		{Time: at(-300), Key: "a", Kind: keepalive.KindStop, Stopped: "manual", Pings: 1},
		{Time: at(-290), Key: "a", Kind: keepalive.KindReturn, Saved: true, Context: 100},
		// A stop cannot attach to a round that already returned.
		{Time: at(-280), Key: "a", Kind: keepalive.KindStop, Stopped: "replaced", Pings: 1},
		{Time: at(-100), Key: "a", Ping: 1, Usage: hit},
	}
	got := Group(events, now, nil)
	if len(got) != 1 || got[0].Rounds != 2 || got[0].Pings != 2 {
		t.Fatalf("groups %+v", got)
	}
	first, latest := got[0].Detail[1], got[0].Detail[0]
	if first.Result != ResultSaved || first.End != "manual" || latest.Result != ResultPending || latest.End != "" {
		t.Fatalf("stop attached to the wrong round: %+v", got[0].Detail)
	}
	// Only the last ping's until marker describes the end of its round.
	pings := []keepalive.Event{
		{Time: at(-100), Key: "b", Ping: 1, Usage: hit, Stopped: "until"},
		{Time: at(-50), Key: "b", Ping: 2, Usage: hit},
	}
	if r := Group(pings, now, nil)[0].Detail[0]; r.End != "" || r.Pings != 2 {
		t.Fatalf("until came from a non-final ping: %+v", r)
	}
}

func TestTotalsAndComparisonCosts(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	prices := &pricing.Table{Models: []pricing.Model{{Family: "opus", Version: "5.5", Tiers: []pricing.Tier{{Rates: pricing.Rates{Input: 4, Write5m: 5, Write1h: 8, Read: 0.2}}}}}}
	model := "claude-opus-5-5"
	hit := &keepalive.Usage{CacheReadTokens: 100000}
	events := []keepalive.Event{{Time: now.Add(-time.Hour), Kind: keepalive.KindRequest, Model: model, Usage: hit}}
	for _, result := range []string{ResultSaved, ResultWasted, ResultEnded, ResultReplaced, ResultPending, ResultMiss, ResultFailed, "unpriced"} {
		at := now.Add(-time.Minute)
		ping := keepalive.Event{Time: at, Key: result, Model: model, Ping: 1, Usage: hit}
		switch result {
		case ResultMiss:
			ping.Usage = &keepalive.Usage{CacheCreationTokens: 100000}
		case ResultFailed:
			ping.Error = "failed after usage"
		case "unpriced":
			ping.Model = "unknown"
		}
		events = append(events, ping)
		switch result {
		case ResultSaved, ResultWasted:
			events = append(events, keepalive.Event{Time: at.Add(time.Second), Key: result, Kind: keepalive.KindReturn, Saved: result == ResultSaved, Context: 100000, Idle: 600})
		case ResultEnded, ResultReplaced:
			reason := "manual"
			if result == ResultReplaced {
				reason = "replaced"
			}
			events = append(events, keepalive.Event{Time: at.Add(time.Second), Key: result, Kind: keepalive.KindStop, Stopped: reason, Pings: 1})
		}
	}
	convs := Group(events, now, prices)
	total := Total(convs)
	compare := Compare(events, convs, prices)
	wantCosts := map[string]float64{ResultSaved: 0.02, ResultWasted: 0.02, ResultEnded: 0.02, ResultReplaced: 0.02, ResultPending: 0.02, ResultMiss: 0.5, ResultFailed: 0.02}
	if !near(total.Cost, 0.62) || !near(total.Saved, 0.48) || !near(total.Net, -0.14) || total.Unpriced != 1 || total.Rounds != 8 {
		t.Fatalf("totals %+v", total)
	}
	if !near(total.Net, compare.Difference) || !near(compare.Difference, compare.Saved-compare.Pings) || compare.Unpriced != 1 || compare.Rounds != total.Rounds {
		t.Fatalf("net and difference disagree: total=%+v comparison=%+v", total, compare)
	}
	if len(total.Costs) != len(wantCosts) || len(compare.Costs) != len(wantCosts) {
		t.Fatalf("unpriced round entered costs: total=%v comparison=%v", total.Costs, compare.Costs)
	}
	for result, want := range wantCosts {
		if !near(total.Costs[result], want) || !near(compare.Costs[result], want) {
			t.Errorf("%s costs: total=%g comparison=%g want=%g", result, total.Costs[result], compare.Costs[result], want)
		}
	}
	for _, c := range convs {
		if !near(c.Net, c.Saved-c.Cost) {
			t.Errorf("conversation net excludes some costs: %+v", c)
		}
		if c.Key == "unpriced" && len(c.Costs) != 0 {
			t.Errorf("unpriced conversation costs: %v", c.Costs)
		}
	}
	if total.Results[ResultPending] != 2 || total.Results[ResultEnded] != 1 || total.Results[ResultReplaced] != 1 {
		t.Fatalf("result counts %+v", total.Results)
	}
}
