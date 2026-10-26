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
	closed   []string
	logs     []string
	// usage is returned by the next pings as cache_read / cache_creation.
	cacheRead, cacheCreation int64
	execErr                  error
}

func (h *fakeHost) ModelExecuteStream(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, req)
	if h.execErr != nil {
		return pluginapi.HostModelStreamResponse{}, h.execErr
	}
	return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: fmt.Sprintf("s%d", len(h.requests))}, nil
}

func (h *fakeHost) ModelStreamRead(string) (pluginapi.HostModelStreamReadResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	payload := fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":%d,\"cache_creation_input_tokens\":%d,\"output_tokens\":1}}}\n\n", h.cacheRead, h.cacheCreation)
	return pluginapi.HostModelStreamReadResponse{Payload: []byte(payload)}, nil
}

func (h *fakeHost) ModelStreamClose(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = append(h.closed, id)
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

// fakeRecorder keeps the recorded events.
type fakeRecorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *fakeRecorder) Append(ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *fakeRecorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func newTestManager(cfg config.Config) (*Manager, *fakeClock, *fakeHost) {
	m, clock, h, _ := newRecordingManager(cfg)
	return m, clock, h
}

func newRecordingManager(cfg config.Config) (*Manager, *fakeClock, *fakeHost, *fakeRecorder) {
	clock := &fakeClock{now: start}
	h := &fakeHost{cacheRead: 40000}
	rec := &fakeRecorder{}
	m := New(Options{Host: h, Clock: clock, Rand: func() float64 { return 0 }, Recorder: rec})
	m.Configure(cfg)
	return m, clock, h, rec
}

// defaultPings is the number of pings in the default 30 minutes with the
// test's random delay of 0: at 260 s and every 260 s after, until the
// cache of one, at 1560 s, lasts past 1800 s.
const defaultPings = 6

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
			"Authorization":            {"Bearer client-key"},
		},
		Body:     b,
		Metadata: map[string]any{"selected_auth_id": "claude-a.json"},
	}
}

func TestParseRequestBuildsReplay(t *testing.T) {
	req, skip := parseRequest(intercept("r1", "sess", body("main", "1")), 0, false)
	if skip != "" {
		t.Fatalf("skip %q", skip)
	}
	if req.AuthID != "claude-a.json" || req.Session != "sess" || req.Model != "claude-opus-5-5" {
		t.Fatalf("request %+v", req)
	}
	if string(req.Body) != string(body("main", "1")) {
		t.Fatalf("body changed:\n%s", req.Body)
	}
	if req.Headers.Get("Content-Length") != "" || req.Headers.Get("Authorization") != "" || req.Headers.Get("X-App") != "cli" {
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
	cases := map[string]struct {
		req     pluginapi.RequestInterceptRequest
		minBody int
		want    string
	}{
		"not native": {notNative, 0, skipNotNative},
		"no auth":    {noAuth, 0, skipNoAuth},
		"not stream": {notStream, 0, skipNotStream},
		"small":      {intercept("r", "s", body("main", "1")), 64 * 1024, skipSmall},
		"1h":         {intercept("r", "s", []byte(strings.Replace(string(body("main", "1")), `"type":"ephemeral"`, `"type":"ephemeral","ttl":"1h"`, 1))), 0, skipTTL1h},
	}
	for name, tc := range cases {
		if _, skip := parseRequest(tc.req, tc.minBody, false); skip != tc.want {
			t.Errorf("%s: skip %q, want %q", name, skip, tc.want)
		}
	}
}

func TestConversationKeyIgnoresBillingAndMessages(t *testing.T) {
	a := conversationKey("", "s", "", "m", body("main", "1"))
	b := conversationKey("", "s", "", "m", body("main", "2"))
	c := conversationKey("", "s", "", "m", body("subagent", "1"))
	if a != b {
		t.Fatal("key changes between turns of one conversation")
	}
	if a == c {
		t.Fatal("main thread and subagent share a key")
	}
}

// Pings continue until the cache lasts the duration after the last
// request: with 15 minutes the third ping, at 780 s, keeps it until 1080 s.
func TestPingsUntilDuration(t *testing.T) {
	cfg := testConfig()
	cfg.DurationMinutes = 15
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
	if sent.AuthID != "claude-a.json" || !sent.Stream || gjson.GetBytes(sent.Body, "max_tokens").Int() != 32000 {
		t.Fatalf("ping request %+v", sent)
	}
	if len(h.closed) == 0 {
		t.Fatal("stream not closed")
	}
	clock.Advance(time.Hour)
	if h.pings() != 3 {
		t.Fatalf("pings %d, want 3", h.pings())
	}
	if len(m.Status().Conversations) != 0 {
		t.Fatal("conversation kept after its duration")
	}
	if last := h.logs[len(h.logs)-1]; !strings.Contains(last, "wick: ping session=sess ") || !strings.Contains(last, " ping=3 stopped=until cache_read=40000 cache_creation=0 input=3 output=1") {
		t.Fatalf("log message %q", last)
	}
}

// The cache of a request lasts 5 minutes, so a duration that short needs
// no pings.
func TestShortDurationSendsNoPings(t *testing.T) {
	cfg := testConfig()
	cfg.DurationMinutes = 5
	m, clock, h := newTestManager(cfg)
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(time.Hour)
	if h.pings() != 0 || len(m.Status().Conversations) != 0 {
		t.Fatalf("pings %d conversations %d", h.pings(), len(m.Status().Conversations))
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

func TestSubagentsUseTheirOwnDuration(t *testing.T) {
	cfg := testConfig()
	cfg.DurationMinutes, cfg.SubagentDurationMinutes = 30, 10
	m, clock, h := newTestManager(cfg)
	m.Observe(withAgent(intercept("r1", "sess", body("worker", "1")), "a1"))
	if c := m.Status().Conversations[0]; c.DefaultMinutes != 10 || !c.Until.Equal(start.Add(10*time.Minute)) {
		t.Fatalf("subagent duration %+v, want 10 minutes", c)
	}
	clock.Advance(time.Hour)
	if h.pings() != 2 || len(m.Status().Conversations) != 0 {
		t.Fatalf("pings %d, want 2", h.pings())
	}
	// A time set on the page replaces the subagent duration.
	m.Observe(withAgent(intercept("r2", "sess", body("worker", "2")), "a1"))
	m.SetDuration(m.Status().Conversations[0].Key, 60)
	clock.Advance(3 * 260 * time.Second)
	if h.pings() != 5 || len(m.Status().Conversations) != 1 {
		t.Fatalf("pings %d with a time set, want 3 more", h.pings())
	}
}

func TestShortenedDurationSnuffsOut(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	m.Observe(intercept("r2", "sess", body("other", "1")))
	clock.Advance(2 * 260 * time.Second)
	m.SetDuration(m.Status().Conversations[1].Key, 60)
	cfg := testConfig()
	cfg.DurationMinutes = 10
	m.Configure(cfg)
	convs := m.Status().Conversations
	if h.pings() != 4 || len(convs) != 1 || convs[0].Minutes != 60 {
		t.Fatalf("pings %d conversations %+v, want only the one with a time set", h.pings(), convs)
	}
}

func TestSubagentDurationZero(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	m.Observe(withAgent(intercept("r2", "sess", body("worker", "1")), "a1"))
	cfg := testConfig()
	cfg.SubagentDurationMinutes = 0
	m.Configure(cfg)
	convs := m.Status().Conversations
	if len(convs) != 1 || convs[0].Agent {
		t.Fatalf("conversations %+v, want the main one only", convs)
	}
	m.Observe(withAgent(intercept("r3", "sess", body("worker", "2")), "a2"))
	if n := len(m.Status().Conversations); n != 1 {
		t.Fatalf("conversations %d, a subagent was tracked", n)
	}
	clock.Advance(260 * time.Second)
	if h.pings() != 1 {
		t.Fatalf("pings %d, want the main conversation's", h.pings())
	}
}

func TestPingRequiresCacheReadWithoutWrites(t *testing.T) {
	for _, client := range []string{"native", "other"} {
		for _, usage := range []struct {
			name        string
			read, write int64
		}{
			{"no-cache", 0, 0},
			{"write-only", 0, 5000},
			{"read", 40000, 0},
			{"read-and-write", 40000, 5000},
		} {
			t.Run(client+"/"+usage.name, func(t *testing.T) {
				cfg := testConfig()
				cfg.OtherClients = client == "other"
				m, clock, h, rec := newRecordingManager(cfg)
				h.cacheRead, h.cacheCreation = usage.read, usage.write
				in := intercept("r1", "sess", body("main", "1"))
				if client == "other" {
					in = otherRequest(body("main", "1"))
				}
				m.Observe(in)
				var c *conversation
				for _, tracked := range m.convs {
					c = tracked
				}
				before := c.lastRead
				clock.Advance(260 * time.Second)
				events := rec.all()
				if h.pings() != 1 || len(events) != 1 {
					t.Fatalf("pings %d events %+v", h.pings(), events)
				}
				if usage.read <= 0 || usage.write > 0 {
					if events[0].Stopped != stopCacheMiss || !c.lastRead.Equal(before) || len(m.convs) != 0 || len(m.ended) != 0 {
						t.Fatalf("miss renewed cache or retained conversation: lastRead=%s before=%s event=%+v", c.lastRead, before, events[0])
					}
					if logs := strings.Join(h.logs, "\n"); !strings.Contains(logs, "ping missed the cache") || !strings.Contains(logs, stopCacheMiss) {
						t.Fatalf("missing cache-miss log: %s", logs)
					}
					clock.Advance(time.Hour)
					if h.pings() != 1 {
						t.Fatalf("pings %d after cache miss", h.pings())
					}
					m.Observe(in)
					if len(rec.all()) != 1 {
						t.Fatal("cache miss recorded a saved return")
					}
				} else {
					if events[0].Stopped != "" || !c.lastRead.Equal(clock.Now()) || len(m.convs) != 1 {
						t.Fatalf("cache read did not renew: lastRead=%s event=%+v", c.lastRead, events[0])
					}
					clock.Advance(260 * time.Second)
					if h.pings() != 2 {
						t.Fatalf("pings %d after cache read", h.pings())
					}
				}
			})
		}
	}
}

func TestErrorStops(t *testing.T) {
	m, clock, h, rec := newRecordingManager(testConfig())
	h.execErr = errors.New("boom")
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(time.Hour)
	if ev := rec.all(); h.pings() != 1 || len(ev) != 1 || ev[0].Error != "boom" || ev[0].Stopped != stopError {
		t.Fatalf("pings %d events %+v", h.pings(), ev)
	}
}

func TestEventDescribesConversation(t *testing.T) {
	b := []byte(`{"model":"claude-opus-5-5","system":[{"type":"text","text":"x-anthropic-billing-header: cch=1;"},{"type":"text","text":"# Environment\n - Primary working directory: /home/me/project\n - Is a git repository: true"}],"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>CLAUDE.md</system-reminder>"},{"type":"text","text":"Fix the   login\nbug"}]}],"stream":true}`)
	m, clock, _, rec := newRecordingManager(testConfig())
	m.Observe(withAgent(intercept("r1", "sess", b), "a1"))
	c := m.Status().Conversations[0]
	if c.Cwd != "/home/me/project" || c.Prompt != "Fix the login bug" || !c.Agent || c.Session != "sess" || c.Client != "cli" {
		t.Fatalf("status %+v", c)
	}
	clock.Advance(260 * time.Second)
	if ev := rec.all(); len(ev) != 1 || ev[0].Cwd != "/home/me/project" || ev[0].Prompt != "Fix the login bug" || !ev[0].Agent {
		t.Fatalf("events %+v", ev)
	}
}

func TestClientEntrypoint(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.1.299 (external, cli)":                             "cli",
		"claude-cli/2.1.299 (external, claude-desktop, agent-sdk/0.2.1)": "claude-desktop",
		"claude-cli/2.1.299":                                             "",
	} {
		if got := clientEntrypoint(ua); got != want {
			t.Errorf("%q: %q, want %q", ua, got, want)
		}
	}
}

func TestWorkingDirectoryAndFirstPrompt(t *testing.T) {
	dirs := map[string]string{
		`{"system":[{"type":"text","text":" - Primary working directory: D:\\Code\\app (was D:\\old)\n"}]}`: `D:\Code\app`,
		`{"system":"<env>\nWorking directory: /srv/x\n</env>"}`:                                             "/srv/x",
		`{"system":[{"type":"text","text":"no directory"}]}`:                                                "",
		`{"system":[{"type":"text","text":"x"}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":"# Environment
 - Primary working directory: /first
"},{"role":"system","content":"# Environment update
 - Primary working directory: /second (was /first)
"}]}`: "/first",
	}
	for raw, want := range dirs {
		if got := workingDirectory([]byte(raw)); got != want {
			t.Errorf("%s: dir %q, want %q", raw, got, want)
		}
	}
	prompts := map[string]string{
		`{"messages":[{"role":"user","content":"hello"}]}`: "hello",
		`{"messages":[{"role":"user","content":[{"type":"text","text":"<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>42</command-args>"}]}]}`:                                                                                                                                        "/review 42",
		`{"messages":[{"role":"user","content":[{"type":"text","text":"Caveat: The messages below were generated by the user while running local commands. DO NOT respond."},{"type":"text","text":"<local-command-stdout>ok</local-command-stdout>"}]},{"role":"assistant","content":"x"},{"role":"user","content":"real prompt"}]}`: "real prompt",
		`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"x"}]}]}`: "",
	}
	for raw, want := range prompts {
		if got := firstPrompt([]byte(raw)); got != want {
			t.Errorf("%s: prompt %q, want %q", raw, got, want)
		}
	}
	long := firstPrompt([]byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("字", 300) + `"}]}`))
	if n := len([]rune(long)); n != maxPromptRunes || !strings.HasSuffix(long, "…") {
		t.Fatalf("long prompt %d runes", n)
	}
}

func TestSetDurationReplacesDefault(t *testing.T) {
	m, clock, h, rec := newRecordingManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	key := short(conversationKey("", "sess", "", "claude-opus-5-5", body("main", "1")))
	if m.SetDuration(key, 25*60) == nil || !errors.Is(m.SetDuration("unknown", 60), ErrNotFound) {
		t.Fatal("accepted an invalid duration or key")
	}
	if err := m.SetDuration(key, 120); err != nil {
		t.Fatal(err)
	}
	if c := m.Status().Conversations[0]; !c.Until.Equal(start.Add(2*time.Hour)) || c.Minutes != 120 {
		t.Fatalf("status %+v", c)
	}
	clock.Advance(3 * time.Hour)
	// Pings every 260 s; the last one is the first whose cache lasts past
	// two hours: (7200 - 300) / 260 rounded up.
	if h.pings() != 27 {
		t.Fatalf("pings %d, want 27", h.pings())
	}
	if ev := rec.all(); ev[len(ev)-1].Stopped != stopUntil {
		t.Fatalf("last event %+v", ev[len(ev)-1])
	}
}

// A duration counts from the last request; one the cache already lasts
// past is refused and changes nothing.
func TestSetDurationRefusesPassedDuration(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	key := short(conversationKey("", "sess", "", "claude-opus-5-5", body("main", "1")))
	if !errors.Is(m.SetDuration(key, 5), ErrPassed) {
		t.Fatal("accepted a duration the cache already lasts")
	}
	// The ping at 260 s keeps the cache until 560 s.
	clock.Advance(270 * time.Second)
	if h.pings() != 1 {
		t.Fatalf("pings %d", h.pings())
	}
	if !errors.Is(m.SetDuration(key, 9), ErrPassed) {
		t.Fatal("accepted 9 minutes with the cache alive until 560 s")
	}
	if c := m.Status().Conversations[0]; c.Minutes != 0 {
		t.Fatalf("refused duration was set: %+v", c)
	}
	if err := m.SetDuration(key, 10); err != nil {
		t.Fatal(err)
	}
	if c := m.Status().Conversations[0]; !c.Until.Equal(start.Add(10 * time.Minute)) {
		t.Fatalf("status %+v", c)
	}
	clock.Advance(time.Hour)
	// The ping at 520 s keeps the cache past 600 s and is the last.
	if h.pings() != 2 {
		t.Fatalf("pings %d, want 2", h.pings())
	}
}

func TestNewRequestClearsDuration(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	key := short(conversationKey("", "sess", "", "claude-opus-5-5", body("main", "1")))
	m.SetDuration(key, 480)
	m.Observe(intercept("r2", "sess", body("main", "2")))
	if c := m.Status().Conversations[0]; c.Minutes != 0 {
		t.Fatalf("duration kept after a new request: %+v", c)
	}
	clock.Advance(8 * time.Hour)
	if h.pings() != defaultPings {
		t.Fatalf("pings %d, want %d", h.pings(), defaultPings)
	}
}

// Returning to the default duration after the cache already lasts it
// would snuff out the conversation, so it is refused; before that it is
// allowed.
func TestResetDurationAfterDefaultPassed(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	key := short(conversationKey("", "sess", "", "claude-opus-5-5", body("main", "1")))
	m.SetDuration(key, 60)
	clock.Advance(10 * time.Minute)
	if err := m.SetDuration(key, 0); err != nil {
		t.Fatal(err)
	}
	m.SetDuration(key, 60)
	clock.Advance(30 * time.Minute)
	if !errors.Is(m.SetDuration(key, 0), ErrPassed) {
		t.Fatal("reset a duration past the default")
	}
	if c := m.Status().Conversations; len(c) != 1 || c[0].Minutes != 60 {
		t.Fatalf("conversations %+v", c)
	}
	n := h.pings()
	clock.Advance(10 * time.Minute)
	if h.pings() == n {
		t.Fatal("stopped pinging after the refused reset")
	}
}

func TestEvictionSparesSetDurations(t *testing.T) {
	m, clock, _ := newTestManager(testConfig())
	m.Observe(intercept("r0", "kept", body("main", "1")))
	m.SetDuration(short(conversationKey("", "kept", "", "claude-opus-5-5", body("main", "1"))), 240)
	for i := 1; i <= maxConversations; i++ {
		clock.Advance(time.Second)
		m.Observe(intercept(fmt.Sprintf("r%d", i), fmt.Sprintf("s%d", i), body("main", "1")))
	}
	found := false
	for _, c := range m.Status().Conversations {
		if c.Session == "kept" {
			found = true
		}
		if c.Session == "s1" {
			t.Fatal("the oldest conversation without a duration was kept")
		}
	}
	if !found {
		t.Fatal("conversation with a duration was evicted")
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
	if len(convs) != 1 || convs[0].Key != short(conversationKey("", "sess", "", "claude-opus-5-5", body("main", "1"))) {
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

func TestSSEScanner(t *testing.T) {
	var s sseScanner
	full := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":0,\"cache_creation\":{\"ephemeral_5m_input_tokens\":0}}}}\n\n"
	usage, err := s.feed([]byte(full[:30]))
	if usage != nil || err != nil {
		t.Fatal("result before the event was complete")
	}
	usage, err = s.feed([]byte(full[30:]))
	if err != nil || usage == nil || usage.CacheReadTokens != 100 || usage.InputTokens != 3 {
		t.Fatalf("usage %+v err %v", usage, err)
	}

	var e sseScanner
	if _, err := e.feed([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n")); err == nil || !strings.Contains(err.Error(), "overloaded") {
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

func TestDurationZeroCancelsSchedules(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	cfg := testConfig()
	cfg.DurationMinutes = 0
	m.Configure(cfg)
	clock.Advance(time.Hour)
	if h.pings() != 0 {
		t.Fatal("pinged after duration_minutes was set to 0")
	}
}

// A ping that returns after its conversation was removed and recreated must
// not schedule a second chain of pings.
func TestStalePingAfterRecreate(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	m.mu.Lock()
	staleGen := m.convs[conversationKey("", "sess", "", "claude-opus-5-5", body("main", "1"))].gen
	m.mu.Unlock()
	m.Complete(pluginapi.RequestCompletion{RequestID: "r1", Outcome: pluginapi.RequestCompletionFailed})
	m.Observe(intercept("r2", "sess", body("main", "2")))
	m.fire(conversationKey("", "sess", "", "claude-opus-5-5", body("main", "1")), staleGen)
	if h.pings() != 0 {
		t.Fatal("stale timer sent a ping")
	}
	clock.Advance(time.Hour)
	if h.pings() != defaultPings {
		t.Fatalf("pings %d, want one chain of %d", h.pings(), defaultPings)
	}
}

func TestStopConversation(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	m.Observe(intercept("r2", "other", body("main", "1")))
	key := conversationKey("", "sess", "", "claude-opus-5-5", body("main", "1"))
	if m.StopConversation("unknown") {
		t.Fatal("stopped an unknown conversation")
	}
	if !m.StopConversation(short(key)) {
		t.Fatal("conversation not found by its short key")
	}
	convs := m.Status().Conversations
	if len(convs) != 1 || convs[0].Session != "other" {
		t.Fatalf("conversations %+v", convs)
	}
	clock.Advance(260 * time.Second)
	if h.pings() != 1 || h.requests[0].Headers.Get("X-Claude-Code-Session-Id") != "other" {
		t.Fatalf("pings %d, want only the other conversation", h.pings())
	}
}

// chat builds a request body with the given tools and messages, each
// message a user text.
func chat(system string, tools []string, messages ...string) []byte {
	toolJSON := make([]string, len(tools))
	for i, name := range tools {
		toolJSON[i] = `{"name":"` + name + `"}`
	}
	msgJSON := make([]string, len(messages))
	for i, text := range messages {
		msgJSON[i] = `{"role":"user","content":"` + text + `"}`
	}
	return []byte(`{"model":"claude-opus-5-5","system":[{"type":"text","text":"` + system + `"}],"tools":[` + strings.Join(toolJSON, ",") + `],"messages":[` + strings.Join(msgJSON, ",") + `],"stream":true}`)
}

func TestNewToolSnuffsOutTheOldKey(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", chat("main", []string{"Bash"}, "a", "b", "c")))
	clock.Advance(10 * time.Second)
	// A deferred tool loaded mid-conversation changes the key; the last
	// message carried the cache marker and differs.
	m.Observe(intercept("r2", "sess", chat("main", []string{"Bash", "WebFetch"}, "a", "b", "c2", "d", "e")))
	convs := m.Status().Conversations
	if len(convs) != 1 || convs[0].Key != short(conversationKey("", "sess", "", "claude-opus-5-5", chat("main", []string{"Bash", "WebFetch"}))) {
		t.Fatalf("conversations %+v, want only the new key", convs)
	}
	if last := h.logs[len(h.logs)-1]; !strings.Contains(last, "stopped=replaced") {
		t.Fatalf("log %q", last)
	}
	clock.Advance(time.Hour)
	if h.pings() != defaultPings {
		t.Fatalf("pings %d, want one chain", h.pings())
	}
}

func TestHelperRequestsKeepTheConversation(t *testing.T) {
	m, _, _ := newTestManager(testConfig())
	m.Observe(intercept("r1", "sess", chat("main", []string{"Bash"}, "a", "b", "c")))
	// Same session, another system prompt, messages that do not continue.
	m.Observe(intercept("r2", "sess", chat("helper", nil, "summarize", "b", "c", "d")))
	// A request of one message cannot be compared and is kept.
	m.Observe(intercept("r3", "other", chat("main", nil, "a")))
	m.Observe(intercept("r4", "other", chat("main", []string{"Bash"}, "a", "b")))
	// Another agent of the same session continues nothing of the main
	// conversation, even with the same messages.
	m.Observe(withAgent(intercept("r5", "sess", chat("main", []string{"Bash", "Read"}, "a", "b", "c", "d")), "a1"))
	if n := len(m.Status().Conversations); n != 5 {
		t.Fatalf("conversations %d, want all 5", n)
	}
}

func returns(rec *fakeRecorder) []Event {
	var out []Event
	for _, ev := range rec.all() {
		if ev.Kind == KindReturn {
			out = append(out, ev)
		}
	}
	return out
}

func TestReturnAfterDurationSaves(t *testing.T) {
	cfg := testConfig()
	cfg.DurationMinutes = 10
	m, clock, _, rec := newRecordingManager(cfg)
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(520 * time.Second) // pings at 260 s and 520 s, then snuffed out
	if n := len(m.Status().Conversations); n != 0 {
		t.Fatalf("conversations %d after the duration", n)
	}
	clock.Advance(200 * time.Second)
	m.Observe(intercept("r2", "sess", body("main", "2")))
	got := returns(rec)
	if len(got) != 1 || !got[0].Saved || got[0].Pings != 2 || got[0].Idle != 720 || got[0].Context != 40003 || got[0].Ping != 0 {
		t.Fatalf("returns %+v", got)
	}
	// The page reloads the history when a return is recorded.
	if last := m.Status().LastEvent; !last.Equal(got[0].Time) {
		t.Fatalf("last_event %v, want the return at %v", last, got[0].Time)
	}
	// The next round starts from the new request.
	clock.Advance(100 * time.Second)
	m.Observe(intercept("r3", "sess", body("main", "3")))
	if len(returns(rec)) != 1 {
		t.Fatal("a request without pings in its round was recorded as a return")
	}
}

func TestReturnTooEarlyOrTooLateSavesNothing(t *testing.T) {
	m, clock, _, rec := newRecordingManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(280 * time.Second) // one ping at 260 s
	m.Observe(intercept("r2", "sess", body("main", "2")))
	if got := returns(rec); len(got) != 1 || got[0].Saved || got[0].Pings != 1 {
		t.Fatalf("early return %+v, want not saved", got)
	}

	cfg := testConfig()
	cfg.DurationMinutes = 9
	m, clock, _, rec = newRecordingManager(cfg)
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(260*time.Second + cacheTTL) // the cache of the only ping expired
	m.Observe(intercept("r2", "sess", body("main", "2")))
	if got := returns(rec); len(got) != 0 {
		t.Fatalf("late return recorded: %+v", got)
	}
}

func TestReplacedConversationHasNoReturn(t *testing.T) {
	cfg := testConfig()
	cfg.DurationMinutes = 9
	m, clock, _, rec := newRecordingManager(cfg)
	m.Observe(intercept("r1", "sess", chat("main", []string{"Bash"}, "a", "b")))
	clock.Advance(400 * time.Second)
	m.Observe(intercept("r2", "sess", chat("main", []string{"Bash", "WebFetch"}, "a", "b2", "c")))
	if got := returns(rec); len(got) != 0 {
		t.Fatalf("returns %+v, want none for a conversation under a new key", got)
	}
}

func TestManualStopKeepsTheRoundForAReturn(t *testing.T) {
	m, clock, _, rec := newRecordingManager(testConfig())
	m.Observe(intercept("r1", "sess", body("main", "1")))
	clock.Advance(260 * time.Second)
	m.StopConversation(short(conversationKey("", "sess", "", "claude-opus-5-5", body("main", "1"))))
	clock.Advance(100 * time.Second)
	m.Observe(intercept("r2", "sess", body("main", "2")))
	if got := returns(rec); len(got) != 1 || !got[0].Saved {
		t.Fatalf("returns %+v, want one saved", got)
	}
}
