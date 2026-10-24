package keepalive

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func usageRecord(requestID, traceID string, read, write, input int64) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		RequestID: requestID,
		TraceID:   traceID,
		Detail:    pluginapi.UsageDetail{CacheReadTokens: read, CacheCreationTokens: write, InputTokens: input, OutputTokens: 500},
	}
}

func requestEvents(rec *fakeRecorder) []Event {
	var out []Event
	for _, ev := range rec.all() {
		if ev.Kind == KindRequest {
			out = append(out, ev)
		}
	}
	return out
}

func TestUsageRecordsRequests(t *testing.T) {
	m, clock, _, rec := newRecordingManager(testConfig())
	in := intercept("r1", "sess", body("main", "1"))
	in.TraceID = "t1"
	m.Observe(in)
	// The usage of other requests, such as pings, is ignored. CPA gives the
	// usage record its own request ID; the trace ID matches.
	m.Usage(usageRecord("ping", "", 40000, 0, 1))
	m.Usage(usageRecord("x1", "t1", 0, 30000, 10))
	m.Usage(usageRecord("x1", "t1", 0, 30000, 10))
	evs := requestEvents(rec)
	if len(evs) != 1 || evs[0].Idle != 0 || evs[0].Context != 0 || evs[0].Usage == nil ||
		evs[0].Usage.CacheCreationTokens != 30000 || evs[0].Usage.InputTokens != 10 || evs[0].Usage.OutputTokens != 0 || !evs[0].Time.Equal(start) {
		t.Fatalf("first request %+v", evs)
	}

	// A failed attempt leaves the request waiting for its retry, which is
	// intercepted again and keeps the idle time of the first attempt.
	clock.Advance(20 * time.Minute)
	in = intercept("r2", "sess", body("main", "2"))
	in.TraceID = "t2"
	m.Observe(in)
	m.Usage(pluginapi.UsageRecord{RequestID: "x", TraceID: "t2", Failed: true})
	clock.Advance(time.Second)
	m.Observe(in)
	m.Usage(usageRecord("y", "t2", 0, 31000, 5))
	evs = requestEvents(rec)
	if len(evs) != 2 || evs[1].Idle != 1200 || evs[1].Context != 30010 || evs[1].Usage.CacheCreationTokens != 31000 {
		t.Fatalf("second request %+v", evs)
	}

	// After an hour the previous request is forgotten. Without a trace ID
	// the request ID matches.
	clock.Advance(61 * time.Minute)
	m.Observe(intercept("r3", "sess", body("main", "3")))
	m.Usage(usageRecord("r3", "", 0, 32000, 5))
	evs = requestEvents(rec)
	if len(evs) != 3 || evs[2].Idle != 0 || evs[2].Context != 0 {
		t.Fatalf("third request %+v", evs)
	}
}

func TestUsageSkipsUntrackedRequests(t *testing.T) {
	cfg := testConfig()
	cfg.SubagentDurationMinutes = 0
	m, _, _, rec := newRecordingManager(cfg)
	in := intercept("r1", "sess", body("agent", "1"))
	in.Headers.Set(agentHeader, "a1")
	m.Observe(in)
	m.Usage(usageRecord("r1", "", 0, 30000, 10))
	if evs := requestEvents(rec); len(evs) != 0 {
		t.Fatalf("recorded a subagent that is not kept lit: %+v", evs)
	}
}
