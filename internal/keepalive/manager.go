// Package keepalive keeps the 5-minute prompt cache of idle Claude Code
// conversations alive. It remembers the last request of each conversation
// and, while the conversation is idle, replays it unchanged before the cache
// entry expires and closes the stream at message_start, like a request
// canceled right after sending. By then the replay has read the cache, which
// resets the entry's TTL.
package keepalive

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/config"
	"github.com/PosvdM/cpa-plugin-wick/internal/host"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// maxConversations bounds memory: each entry holds a full request body.
	maxConversations = 32
	// pingTimeout closes a ping stream that has not delivered message_start.
	pingTimeout = 60 * time.Second
	// cacheTTL is how long a prompt cache entry lives after it was last read.
	cacheTTL = 300 * time.Second
	// MaxDurationHours bounds the duration set for one conversation.
	MaxDurationHours = 24
	// maxEventPromptRunes bounds the first prompt stored with each ping.
	maxEventPromptRunes = 80
)

// Stop reasons, also reported in the status API.
const (
	stopMaxPings   = "max_pings"
	stopUntil      = "until"
	stopError      = "error"
	stopCacheMiss  = "cache_miss"
	stopDisabled   = "disabled"
	stopFailed     = "request_failed"
	stopSuperseded = "superseded"
	stopEvicted    = "evicted"
	stopTTL1h      = "ttl_1h"
	stopManual     = "manual"
	stopExpired    = "expired"
)

// Clock abstracts time so that tests can drive the scheduler.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a scheduled call that can be cancelled.
type Timer interface {
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Options configures a Manager.
type Options struct {
	Host  host.Host
	Clock Clock
	// Rand returns a number in [0, 1). It picks the delay within the interval.
	Rand func() float64
	// Enabled reports whether the plugin is enabled in the CPA config.
	Enabled func() bool
	// Recorder stores the result of each ping. It may be nil.
	Recorder Recorder
}

// Recorder stores the result of each ping.
type Recorder interface {
	Append(Event) error
}

// Manager tracks conversations and sends the pings.
type Manager struct {
	host     host.Host
	clock    Clock
	rand     func() float64
	enabled  func() bool
	recorder Recorder

	mu      sync.Mutex
	cfg     config.Config
	stopped bool
	convs   map[string]*conversation
	streams map[string]struct{}
	// gen numbers schedules across all conversations, so that a ping still
	// in flight for a removed conversation cannot match a new one.
	gen uint64
	// lastPing is the start of the latest ping, so that the page knows when
	// to reload the history.
	lastPing time.Time
	// store saves the conversations so that they survive a restart of CPA.
	// restored is set once the saved conversations have been loaded.
	store    *convStore
	restored bool
}

type conversation struct {
	req         request
	firstSeen   time.Time
	lastRequest time.Time
	// lastRead is when the cache was last read: the start of the last
	// request or of the last successful ping.
	lastRead  time.Time
	nextPing  time.Time
	pings     int
	gen       uint64
	timer     Timer
	lastUsage *Usage
	// until, when set, replaces max_pings: pings continue until the cache
	// lasts past this time. hours is the duration that was set. Both are
	// cleared by the next request.
	until time.Time
	hours float64
}

// Event is the result of one ping.
type Event struct {
	Time    time.Time `json:"time"`
	Session string    `json:"session"`
	Key     string    `json:"key"`
	Model   string    `json:"model"`
	Agent   bool      `json:"agent,omitempty"`
	Client  string    `json:"client,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	Prompt  string    `json:"prompt,omitempty"`
	Ping    int       `json:"ping"`
	Usage   *Usage    `json:"usage,omitempty"`
	Error   string    `json:"error,omitempty"`
	Stopped string    `json:"stopped,omitempty"`
}

// New creates a Manager with the default config.
func New(opts Options) *Manager {
	m := &Manager{
		host:     opts.Host,
		clock:    opts.Clock,
		rand:     opts.Rand,
		enabled:  opts.Enabled,
		recorder: opts.Recorder,
		cfg:      config.Default(),
		convs:    make(map[string]*conversation),
		streams:  make(map[string]struct{}),
	}
	if m.clock == nil {
		m.clock = realClock{}
	}
	if m.rand == nil {
		m.rand = rand.Float64
	}
	if m.enabled == nil {
		m.enabled = func() bool { return true }
	}
	return m
}

// Configure applies cfg to new schedules and resumes a stopped manager.
// Conversations that have reached their new limit are snuffed out.
func (m *Manager) Configure(cfg config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
	m.stopped = false
	for key, c := range m.convs {
		if m.doneLocked(c) {
			m.removeLocked(key)
			m.logLocked("info", "limit lowered, keep-alive stopped", c.req, c.pings, stopMaxPings)
		}
	}
}

// doneLocked reports whether c has used up its pings: its limit is 0, or it
// has no time set and has reached its limit.
func (m *Manager) doneLocked(c *conversation) bool {
	limit := m.limitLocked(c.req)
	return limit == 0 || (c.until.IsZero() && c.pings >= limit)
}

// limitLocked returns the pings per idle period of req's conversation:
// subagent_max_pings for subagents, max_pings otherwise. max_pings 0 turns
// Wick off, subagents included.
func (m *Manager) limitLocked(req request) int {
	if m.cfg.MaxPings == 0 {
		return 0
	}
	if req.Agent != "" {
		return m.cfg.SubagentMaxPings
	}
	return m.cfg.MaxPings
}

// SetStore saves the conversations under dir from now on. The first call
// after the manager starts or resumes also restores the conversations saved
// there, keeping their ping counts and schedules.
func (m *Manager) SetStore(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || (m.store != nil && m.store.dir == dir) {
		return
	}
	if m.store != nil {
		// The old goroutine may be waiting for m.mu, so it is not waited for.
		m.store.stop()
	}
	m.store = newConvStore(m, dir)
	if !m.restored {
		m.restored = true
		m.restoreLocked()
	}
	m.store.start()
	m.persistLocked()
}

// Stop cancels every schedule, forgets every conversation and closes the
// streams of pings in flight. It saves the conversations first, so that the
// next instance restores the latest state; CPA quiesces the old instance
// before it loads the new one.
func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopped = true
	store := m.store
	var final []savedConversation
	if store != nil {
		final = m.snapshotLocked()
		m.store = nil
	}
	m.restored = false
	for key := range m.convs {
		m.removeLocked(key)
	}
	streams := make([]string, 0, len(m.streams))
	for id := range m.streams {
		streams = append(streams, id)
	}
	m.mu.Unlock()
	if store != nil {
		store.stop()
		store.wait()
		if err := store.write(final); err != nil {
			m.host.Log("warn", "wick: save conversations: "+err.Error(), nil)
		}
	}
	for _, id := range streams {
		m.host.ModelStreamClose(id)
	}
}

// Observe records an intercepted request. It runs on the request path, so it
// only copies the request and schedules a timer.
func (m *Manager) Observe(in pluginapi.RequestInterceptRequest) {
	m.mu.Lock()
	minBody := m.cfg.MinBodyKB * 1024
	m.mu.Unlock()
	req, skip := parseRequest(in, minBody)
	if skip == skipTTL1h {
		m.mu.Lock()
		if _, ok := m.convs[req.Key]; ok {
			m.removeLocked(req.Key)
			m.logLocked("info", "1h cache in use, keep-alive not needed", req, 0, stopTTL1h)
		}
		m.mu.Unlock()
		return
	}
	if skip != "" {
		return
	}

	now := m.clock.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.limitLocked(req) == 0 {
		return
	}
	c, ok := m.convs[req.Key]
	if ok {
		// Conversations that started while the main conversation waited for
		// its last response are its subagents. A new request from the main
		// conversation means they have returned and no longer need the cache.
		// Subagent requests carry an agent ID; without one every request is
		// treated as the main conversation.
		for key, other := range m.convs {
			if req.Agent == "" && key != req.Key && other.req.Session == req.Session && other.firstSeen.After(c.lastRequest) {
				m.removeLocked(key)
				m.logLocked("debug", "subagent finished, keep-alive stopped", other.req, other.pings, stopSuperseded)
			}
		}
		if c.timer != nil {
			c.timer.Stop()
		}
		c.until, c.hours = time.Time{}, 0
	} else {
		if len(m.convs) >= maxConversations {
			m.evictOldestLocked()
		}
		c = &conversation{firstSeen: now}
		m.convs[req.Key] = c
	}
	c.req = req
	c.lastRequest = now
	c.lastRead = now
	c.pings = 0
	m.gen++
	c.gen = m.gen
	m.scheduleLocked(req.Key, c, now)
}

// Complete handles the end of an intercepted request. A failed request
// stops the keep-alive of its conversation. A canceled request keeps it:
// after pressing Esc, the next message usually shares the same prefix.
func (m *Manager) Complete(done pluginapi.RequestCompletion) {
	if done.Outcome != pluginapi.RequestCompletionFailed && done.Outcome != pluginapi.RequestCompletionRejected {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, c := range m.convs {
		if c.req.RequestID == done.RequestID {
			m.removeLocked(key)
			m.logLocked("info", "request failed, keep-alive stopped", c.req, c.pings, stopFailed)
		}
	}
}

// SetDuration keeps the conversation whose short key is key alive for hours
// from now, in place of its limit of pings, until the conversation's next
// request. hours 0 returns it to the limit. It reports whether a
// conversation was found.
func (m *Manager) SetDuration(key string, hours float64) bool {
	if !(hours >= 0 && hours <= MaxDurationHours) {
		return false
	}
	now := m.clock.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	for full, c := range m.convs {
		if short(full) != key {
			continue
		}
		found = true
		if hours > 0 {
			c.until, c.hours = now.Add(time.Duration(hours*float64(time.Hour))), hours
			m.logLocked("info", fmt.Sprintf("kept lit for %g h from the management page", hours), c.req, c.pings, "")
		} else {
			c.until, c.hours = time.Time{}, 0
			if c.pings >= m.limitLocked(c.req) {
				m.removeLocked(full)
				m.logLocked("info", "duration reset from the management page", c.req, c.pings, stopMaxPings)
			}
		}
		m.persistLocked()
	}
	return found
}

// StopConversation stops the keep-alive of the conversation whose short key,
// as shown in the status, is key. A ping in flight still finishes but does not
// schedule the next one. It reports whether a conversation was found.
func (m *Manager) StopConversation(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	for full, c := range m.convs {
		if short(full) == key {
			m.removeLocked(full)
			m.logLocked("info", "keep-alive stopped from the management page", c.req, c.pings, stopManual)
			found = true
		}
	}
	return found
}

// scheduleLocked sets the next ping at a random delay within the interval
// after from.
func (m *Manager) scheduleLocked(key string, c *conversation, from time.Time) {
	span := m.cfg.IntervalMax - m.cfg.IntervalMin
	delay := time.Duration(m.cfg.IntervalMin)*time.Second + time.Duration(m.rand()*float64(span)*float64(time.Second))
	m.scheduleAtLocked(key, c, from.Add(delay))
}

func (m *Manager) scheduleAtLocked(key string, c *conversation, at time.Time) {
	c.nextPing = at
	gen := c.gen
	wait := c.nextPing.Sub(m.clock.Now())
	c.timer = m.clock.AfterFunc(wait, func() {
		// Timer callbacks run on their own goroutine; a panic there would
		// take down CPA.
		defer func() {
			if r := recover(); r != nil {
				m.host.Log("error", fmt.Sprintf("wick: ping panicked: %v", r), nil)
			}
		}()
		m.fire(key, gen)
	})
	m.persistLocked()
}

func (m *Manager) removeLocked(key string) {
	c, ok := m.convs[key]
	if !ok {
		return
	}
	if c.timer != nil {
		c.timer.Stop()
	}
	delete(m.convs, key)
	m.persistLocked()
}

// persistLocked asks the store to save the conversations.
func (m *Manager) persistLocked() {
	if m.store != nil {
		m.store.changed()
	}
}

// evictOldestLocked removes the conversation whose last request is the
// oldest, sparing those with a duration set on the management page unless
// every conversation has one.
func (m *Manager) evictOldestLocked() {
	oldest := ""
	var oldestAt time.Time
	oldestSet := true
	for key, c := range m.convs {
		set := !c.until.IsZero()
		if oldest == "" || (oldestSet && !set) || (oldestSet == set && c.lastRequest.Before(oldestAt)) {
			oldest, oldestAt, oldestSet = key, c.lastRequest, set
		}
	}
	if c, ok := m.convs[oldest]; ok {
		m.removeLocked(oldest)
		m.logLocked("info", "too many conversations, keep-alive stopped", c.req, c.pings, stopEvicted)
	}
}

// fire sends one ping. gen guards against a request that arrived after the
// timer was set.
func (m *Manager) fire(key string, gen uint64) {
	req, n, ok := m.beginPing(key, gen, m.enabled())
	if !ok {
		return
	}
	start := m.clock.Now()
	usage, err := m.ping(req)
	event := m.finishPing(key, gen, req, n, start, usage, err)
	if m.recorder != nil {
		if err := m.recorder.Append(event); err != nil {
			m.host.Log("warn", "wick: write ping history: "+err.Error(), nil)
		}
	}
}

// beginPing counts the ping and returns the request to replay, or ok=false
// when the conversation changed or the plugin is disabled.
func (m *Manager) beginPing(key string, gen uint64, enabled bool) (request, int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[key]
	if !ok || c.gen != gen || m.stopped {
		return request{}, 0, false
	}
	if !enabled {
		m.removeLocked(key)
		m.logLocked("info", "plugin disabled, keep-alive stopped", c.req, c.pings, stopDisabled)
		return request{}, 0, false
	}
	c.pings++
	return c.req, c.pings, true
}

// finishPing applies the result of a ping to its conversation and returns
// the event to record.
func (m *Manager) finishPing(key string, gen uint64, req request, n int, start time.Time, usage *Usage, err error) Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	event := Event{
		Time:    start,
		Session: short(req.Session),
		Key:     short(req.Key),
		Model:   req.Model,
		Agent:   req.Agent != "",
		Client:  req.Client,
		Cwd:     req.Cwd,
		Prompt:  truncate(req.Prompt, maxEventPromptRunes),
		Ping:    n,
		Usage:   usage,
	}
	if start.After(m.lastPing) {
		m.lastPing = start
	}
	current, ok := m.convs[key]
	stillCurrent := ok && current.gen == gen && !m.stopped
	switch {
	case err != nil:
		event.Error = err.Error()
		if stillCurrent {
			event.Stopped = stopError
			m.removeLocked(key)
		}
		m.logLocked("warn", "ping failed: "+err.Error(), req, n, event.Stopped)
	case usage.CacheCreationTokens > 0:
		// The replay wrote to the cache, so the entry had expired or the
		// replay differs from the original request. Further pings would pay
		// for writes again.
		if stillCurrent {
			event.Stopped = stopCacheMiss
			m.removeLocked(key)
		}
		m.logLocked("warn", "ping missed the cache", req, n, event.Stopped, usage)
	default:
		if stillCurrent {
			current.lastUsage = usage
			current.lastRead = start
			switch {
			case !current.until.IsZero() && !start.Add(cacheTTL).Before(current.until):
				event.Stopped = stopUntil
				m.removeLocked(key)
			case current.until.IsZero() && n >= m.limitLocked(req):
				event.Stopped = stopMaxPings
				m.removeLocked(key)
			default:
				m.scheduleLocked(key, current, start)
			}
		}
		m.logLocked("info", "ping", req, n, event.Stopped, usage)
	}
	return event
}

// ping replays req and reads the stream until message_start.
func (m *Manager) ping(req request) (*Usage, error) {
	resp, err := m.host.ModelExecuteStream(pluginapi.HostModelExecutionRequest{
		EntryProtocol: "claude",
		ExitProtocol:  "claude",
		Model:         req.Model,
		Stream:        true,
		Body:          req.Body,
		Headers:       req.Headers,
		AuthID:        req.AuthID,
	})
	if err != nil {
		return nil, err
	}
	if resp.StreamID == "" {
		return nil, errors.New("host returned no stream")
	}
	id := resp.StreamID
	m.mu.Lock()
	m.streams[id] = struct{}{}
	m.mu.Unlock()
	watchdog := m.clock.AfterFunc(pingTimeout, func() {
		defer func() { _ = recover() }()
		m.host.ModelStreamClose(id)
	})
	defer func() {
		watchdog.Stop()
		m.host.ModelStreamClose(id)
		m.mu.Lock()
		delete(m.streams, id)
		m.mu.Unlock()
	}()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	var scanner sseScanner
	for {
		chunk, err := m.host.ModelStreamRead(id)
		if err != nil {
			return nil, err
		}
		usage, err := scanner.feed(chunk.Payload)
		if usage != nil || err != nil {
			return usage, err
		}
		if chunk.Error != "" {
			return nil, errors.New(chunk.Error)
		}
		if chunk.Done {
			return nil, errors.New("stream ended before message_start")
		}
	}
}

func (m *Manager) logLocked(level, message string, req request, ping int, stopped string, usage ...*Usage) {
	fields := map[string]any{
		"session": short(req.Session),
		"key":     short(req.Key),
		"model":   req.Model,
	}
	if ping > 0 {
		fields["ping"] = ping
	}
	if stopped != "" {
		fields["stopped"] = stopped
	}
	if len(usage) > 0 && usage[0] != nil {
		fields["cache_read"] = usage[0].CacheReadTokens
		fields["cache_creation"] = usage[0].CacheCreationTokens
		fields["input"] = usage[0].InputTokens
		fields["output"] = usage[0].OutputTokens
	}
	// CPA's log formatter prints only the fields it knows, such as model, so
	// the others also go into the message.
	var text strings.Builder
	text.WriteString("wick: " + message)
	for _, k := range logFieldOrder {
		if v, ok := fields[k]; ok {
			fmt.Fprintf(&text, " %s=%v", k, v)
		}
	}
	// host.log is a quick callback into CPA's logger, so calling it while
	// holding the lock keeps the log in the same order as the state changes.
	m.host.Log(level, text.String(), fields)
}

// logFieldOrder is the order of the fields written into log messages.
var logFieldOrder = []string{"session", "key", "ping", "stopped", "cache_read", "cache_creation", "input", "output"}

// ConversationStatus describes one tracked conversation.
type ConversationStatus struct {
	// Session is the full session ID, which names the Claude Code transcript.
	Session     string    `json:"session"`
	Key         string    `json:"key"`
	Model       string    `json:"model"`
	Agent       bool      `json:"agent,omitempty"`
	Client      string    `json:"client,omitempty"`
	Cwd         string    `json:"cwd,omitempty"`
	Prompt      string    `json:"prompt,omitempty"`
	AuthID      string    `json:"auth_id"`
	FirstSeen   time.Time `json:"first_seen"`
	LastRequest time.Time `json:"last_request"`
	NextPing    time.Time `json:"next_ping"`
	// Pings counts the pings since the last request, up to MaxPings, the
	// limit of the conversation. TotalPings is filled in by the plugin from
	// the history.
	Pings      int    `json:"pings"`
	MaxPings   int    `json:"max_pings"`
	TotalPings int    `json:"total_pings"`
	BodyBytes  int    `json:"body_bytes"`
	LastUsage  *Usage `json:"last_usage,omitempty"`
	// Until and Hours are set when the management page set a duration for
	// the conversation.
	Until *time.Time `json:"until,omitempty"`
	Hours float64    `json:"hours,omitempty"`
}

// Status is the response of the status API.
type Status struct {
	Config        config.Config        `json:"config"`
	Conversations []ConversationStatus `json:"conversations"`
	// LastPing is the start of the latest ping since the plugin was loaded.
	LastPing time.Time `json:"last_ping"`
}

// Status returns the tracked conversations.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{
		Config:        m.cfg,
		Conversations: make([]ConversationStatus, 0, len(m.convs)),
		LastPing:      m.lastPing,
	}
	for _, c := range m.convs {
		cs := ConversationStatus{
			Session:     c.req.Session,
			Key:         short(c.req.Key),
			Model:       c.req.Model,
			Agent:       c.req.Agent != "",
			Client:      c.req.Client,
			Cwd:         c.req.Cwd,
			Prompt:      c.req.Prompt,
			AuthID:      c.req.AuthID,
			FirstSeen:   c.firstSeen,
			LastRequest: c.lastRequest,
			NextPing:    c.nextPing,
			Pings:       c.pings,
			MaxPings:    m.limitLocked(c.req),
			BodyBytes:   len(c.req.Body),
			LastUsage:   c.lastUsage,
		}
		if !c.until.IsZero() {
			until := c.until
			cs.Until, cs.Hours = &until, c.hours
		}
		status.Conversations = append(status.Conversations, cs)
	}
	sort.Slice(status.Conversations, func(i, j int) bool {
		return status.Conversations[i].LastRequest.After(status.Conversations[j].LastRequest)
	})
	return status
}

// short returns the first 8 characters of an ID for logs and status.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
