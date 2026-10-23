package keepalive

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	was := !t.stopped
	t.stopped = true
	return was
}

// fakeClock runs due timers synchronously in Advance.
type fakeClock struct {
	now    time.Time
	timers []*fakeTimer
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeClock) Advance(d time.Duration) {
	end := c.now.Add(d)
	for {
		sort.SliceStable(c.timers, func(i, j int) bool { return c.timers[i].at.Before(c.timers[j].at) })
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.at.After(end) {
				next = t
				break
			}
		}
		if next == nil {
			break
		}
		next.stopped = true
		c.now = next.at
		next.f()
	}
	c.now = end
}

type fakeHost struct {
	mu       sync.Mutex
	requests []pluginapi.HostModelExecutionRequest
	logs     []string
	// usage is returned by the next pings as cache_read / cache_creation.
	cacheRead, cacheCreation int64
	execErr                  error
}

func (h *fakeHost) ModelExecute(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, req)
	if h.execErr != nil {
		return pluginapi.HostModelExecutionResponse{}, h.execErr
	}
	body := fmt.Sprintf(`{"type":"message","content":[],"stop_reason":"max_tokens","usage":{"input_tokens":3,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":0}}`, h.cacheRead, h.cacheCreation)
	return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(body)}, nil
}

func (h *fakeHost) Log(level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, level+" "+message+" "+fmt.Sprint(fields["stopped"]))
}

func (h *fakeHost) pings() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

var start = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func newTestManager(cfg config.Config) (*Manager, *fakeClock, *fakeHost) {
	clock := &fakeClock{now: start}
	h := &fakeHost{cacheRead: 40000}
	m := New(Options{Host: h, Clock: clock, Rand: func() float64 { return 0 }})
	m.Configure(cfg)
	return m, clock, h
}

func testConfig() config.Config {
	cfg := config.Default()
	cfg.MinBodyKB = 0
	return cfg
}

// body builds a Claude Code style request body. system names the
// conversation; turn changes the messages.
func body(system, turn string) []byte {
	return []byte(`{"model":"claude-opus-5-5","max_tokens":32000,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2; cch=` + turn + `;"},{"type":"text","text":"` + system + `","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"` + turn + `"}],"metadata":{"user_id":"u"},"stream":true}`)
}

func intercept(id, session string, b []byte) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		RequestID:      id,
		SourceFormat:   "claude",
		RequestedModel: "claude-opus-5-5",
		Stream:         true,
		Headers: http.Header{
			"X-App":                    {"cli"},
			"User-Agent":               {"claude-cli/2.1.300 (external, cli)"},
			"Anthropic-Beta":           {"claude-code-20250219"},
			"X-Claude-Code-Session-Id": {session},
			"Content-Length":           {"123"},
		},
		Body:     b,
		Metadata: map[string]any{"selected_auth_id": "claude-a.json"},
	}
}

func TestParseRequestBuildsReplay(t *testing.T) {
	req, skip := parseRequest(intercept("r1", "sess", body("main", "1")), 0)
	if skip != "" {
		t.Fatalf("skip %q", skip)
	}
	if req.AuthID != "claude-a.json" || req.Session != "sess" || req.Model != "claude-opus-5-5" {
		t.Fatalf("request %+v", req)
	}
	if got := gjson.GetBytes(req.Body, "max_tokens"); got.Int() != 0 || !got.Exists() {
		t.Fatalf("max_tokens %v", got)
	}
	want := strings.Replace(strings.Replace(string(body("main", "1")), `"max_tokens":32000`, `"max_tokens":0`, 1), `"stream":true`, `"stream":false`, 1)
	if string(req.Body) != want {
		t.Fatalf("body changed beyond max_tokens:\n%s", req.Body)
	}
	if req.Headers.Get("Content-Length") != "" || req.Headers.Get("X-App") != "cli" {
		t.Fatalf("headers %v", req.Headers)
	}
}

func TestParseRequestSkips(t *testing.T) {
	notNative := intercept("r", "s", body("main", "1"))
	notNative.Headers.Set("User-Agent", "pi/1.0")
	noAuth := intercept("r", "s", body("main", "1"))
	noAuth.Metadata = nil
	notStream := intercept("r", "s", body("main", "1"))
	notStream.Stream = false
	budget := intercept("r", "s", []byte(strings.Replace(string(body("main", "1")), `"stream":true`, `"stream":true,"thinking":{"type":"enabled","budget_tokens":2048}`, 1)))
	cases := map[string]struct {
		req     pluginapi.RequestInterceptRequest
		minBody int
		want    string
	}{
		"not native": {notNative, 0, skipNotNative},
		"no auth":    {noAuth, 0, skipNoAuth},
		"not stream": {notStream, 0, skipNotStream},
		"budget":     {budget, 0, skipBudget},
		"small":      {intercept("r", "s", body("main", "1")), 64 * 1024, skipSmall},
		"1h":         {intercept("r", "s", []byte(strings.Replace(string(body("main", "1")), `"type":"ephemeral"`, `"type":"ephemeral","ttl":"1h"`, 1))), 0, skipTTL1h},
	}
	for name, tc := range cases {
		if _, skip := parseRequest(tc.req, tc.minBody); skip != tc.want {
			t.Errorf("%s: skip %q, want %q", name, skip, tc.want)
		}
	}
}

func TestConversationKeyIgnoresBillingAndMessages(t *testing.T) {
	a := conversationKey("s", "", "m", body("main", "1"))
	b := conversationKey("s", "", "m", body("main", "2"))
	c := conversationKey("s", "", "m", body("subagent", "1"))
	if a != b {
		t.Fatal("key changes between turns of one conversation")
	}
	if a == c {
		t.Fatal("main thread and subagent share a key")
	}
}

func TestPingsUntilMaxPings(t *testing.T) {
	cfg := testConfig()
	cfg.MaxPings = 3
	m, clock, h := newTestManager(cfg)
	m.Observe(intercept("r1", "sess", body("main", "1")))

	clock.Advance(259 * time.Second)
	if h.pings() != 0 {
		t.Fatal("pinged before interval_min")
	}
	clock.Advance(time.Second)
	if h.pings() != 1 {
		t.Fatalf("pings %d after 260s", h.pings())
	}
	sent := h.requests[0]
	if sent.AuthID != "claude-a.json" || sent.Stream || gjson.GetBytes(sent.Body, "max_tokens").Int() != 0 || gjson.GetBytes(sent.Body, "stream").Bool() {
		t.Fatalf("ping request %+v", sent)
	}
	clock.Advance(time.Hour)
	if h.pings() != 3 {
		t.Fatalf("pings %d, want max_pings 3", h.pings())
	}
	if len(m.Status().Conversations) != 0 {
		t.Fatal("conversation kept after max_pings")
	}
	if last := h.logs[len(h.logs)-1]; !strings.Contains(last, "wick: ping session=sess ") || !strings.Contains(last, " ping=3 stopped=max_pings cache_read=40000 cache_creation=0 input=3 output=0") {
		t.Fatalf("log message %q", last)
	}
}

func TestNewRequestResetsSchedule(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(200 * time.Second)
	m.Observe(intercept("r2", "sess", body("main", "2")))
	clock.Advance(100 * time.Second)
	if h.pings() != 0 {
		t.Fatal("old schedule fired after a new request")
	}
	clock.Advance(160 * time.Second)
	if h.pings() != 1 || !strings.Contains(string(h.requests[0].Body), `"content":"2"`) {
		t.Fatalf("pings %d, want one replay of the latest request", h.pings())
	}
}

func TestCacheMissStops(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	h.cacheCreation = 5000
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(time.Hour)
	if h.pings() != 1 {
		t.Fatalf("pings %d after a cache miss", h.pings())
	}
	if ev := m.Status().Events; len(ev) != 1 || ev[0].Stopped != stopCacheMiss {
		t.Fatalf("events %+v", ev)
	}
}

func TestErrorStops(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	h.execErr = errors.New("boom")
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(time.Hour)
	if h.pings() != 1 || m.Status().Totals.Failed != 1 {
		t.Fatalf("pings %d status %+v", h.pings(), m.Status().Totals)
	}
}

func TestSubagentDroppedWhenMainThreadResumes(t *testing.T) {
	m, clock, _ := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(10 * time.Second)
	m.Observe(intercept("r2", "sess", body("subagent", "1")))
	clock.Advance(10 * time.Second)
	m.Observe(intercept("r3", "sess", body("subagent", "2")))
	if n := len(m.Status().Conversations); n != 2 {
		t.Fatalf("conversations %d while the subagent runs, want 2", n)
	}
	clock.Advance(10 * time.Second)
	m.Observe(intercept("r4", "sess", body("main", "2")))
	convs := m.Status().Conversations
	if len(convs) != 1 || convs[0].Key != short(conversationKey("sess", "", "claude-opus-5-5", body("main", "1"))) {
		t.Fatalf("conversations %+v, want only the main thread", convs)
	}
}

func TestOtherSessionsAreKept(t *testing.T) {
	m, clock, _ := newTestManager(testConfig())
	m.Observe(intercept("r1", "a", body("main", "1")))
	clock.Advance(10 * time.Second)
	m.Observe(intercept("r2", "b", body("main", "1")))
	clock.Advance(10 * time.Second)
	m.Observe(intercept("r3", "a", body("main", "2")))
	if n := len(m.Status().Conversations); n != 2 {
		t.Fatalf("conversations %d, want 2", n)
	}
}

func TestFailedRequestStops(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	m.Complete(pluginapi.RequestCompletion{RequestID: "r1", Outcome: pluginapi.RequestCompletionCanceled})
	if len(m.Status().Conversations) != 1 {
		t.Fatal("canceled request stopped the keep-alive")
	}
	m.Complete(pluginapi.RequestCompletion{RequestID: "r1", Outcome: pluginapi.RequestCompletionFailed})
	clock.Advance(time.Hour)
	if h.pings() != 0 {
		t.Fatal("pinged after the request failed")
	}
}

func TestDisabledStops(t *testing.T) {
	clock := &fakeClock{now: start}
	h := &fakeHost{}
	m := New(Options{Host: h, Clock: clock, Rand: func() float64 { return 0 }, Enabled: func() bool { return false }})
	m.Configure(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(time.Hour)
	if h.pings() != 0 || len(m.Status().Conversations) != 0 {
		t.Fatal("pinged while disabled")
	}
}

func TestStopForgetsConversations(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	m.Stop()
	m.Observe(intercept("r2", "sess", body("main", "2")))
	clock.Advance(time.Hour)
	if h.pings() != 0 {
		t.Fatal("pinged after Stop")
	}
}

func TestIntervalUsesRand(t *testing.T) {
	clock := &fakeClock{now: start}
	h := &fakeHost{}
	m := New(Options{Host: h, Clock: clock, Rand: func() float64 { return 0.5 }})
	m.Configure(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	if next := m.Status().Conversations[0].NextPing; !next.Equal(start.Add(275 * time.Second)) {
		t.Fatalf("next ping %v, want 275s after the request", next.Sub(start))
	}
}

func TestParseUsage(t *testing.T) {
	usage, err := parseUsage([]byte(`{"type":"message","content":[],"usage":{"input_tokens":3,"cache_read_input_tokens":100,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0},"output_tokens":0}}`))
	if err != nil || usage.CacheReadTokens != 100 || usage.InputTokens != 3 {
		t.Fatalf("usage %+v err %v", usage, err)
	}
	if _, err := parseUsage([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)); err == nil || !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("err %v", err)
	}
}

func withAgent(req pluginapi.RequestInterceptRequest, agent string) pluginapi.RequestInterceptRequest {
	req.Headers.Set("X-Claude-Code-Agent-Id", agent)
	return req
}

func TestParallelSubagentsWithAgentIDs(t *testing.T) {
	m, clock, _ := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(10 * time.Second)
	m.Observe(withAgent(intercept("r2", "sess", body("worker", "1")), "a1"))
	m.Observe(withAgent(intercept("r3", "sess", body("worker", "1")), "a2"))
	clock.Advance(10 * time.Second)
	m.Observe(withAgent(intercept("r4", "sess", body("worker", "2")), "a1"))
	if n := len(m.Status().Conversations); n != 3 {
		t.Fatalf("conversations %d, want main and two subagents", n)
	}
	m.Observe(intercept("r5", "sess", body("main", "2")))
	if n := len(m.Status().Conversations); n != 1 {
		t.Fatalf("conversations %d after the main conversation resumed, want 1", n)
	}
}

func TestMaxPingsZeroCancelsSchedules(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	cfg := testConfig()
	cfg.MaxPings = 0
	m.Configure(cfg)
	clock.Advance(time.Hour)
	if h.pings() != 0 {
		t.Fatal("pinged after max_pings was set to 0")
	}
}

// A ping that returns after its conversation was removed and recreated must
// not schedule a second chain of pings.
func TestStalePingAfterRecreate(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	m.mu.Lock()
	staleGen := m.convs[conversationKey("sess", "", "claude-opus-5-5", body("main", "1"))].gen
	m.mu.Unlock()
	m.Complete(pluginapi.RequestCompletion{RequestID: "r1", Outcome: pluginapi.RequestCompletionFailed})
	m.Observe(intercept("r2", "sess", body("main", "2")))
	m.fire(conversationKey("sess", "", "claude-opus-5-5", body("main", "1")), staleGen)
	if h.pings() != 0 {
		t.Fatal("stale timer sent a ping")
	}
	clock.Advance(time.Hour)
	if h.pings() != testConfig().MaxPings {
		t.Fatalf("pings %d, want one chain of %d", h.pings(), testConfig().MaxPings)
	}
}
