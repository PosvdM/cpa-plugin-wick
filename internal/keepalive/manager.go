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
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// maxConversations bounds memory: each entry holds a full request body.
	maxConversations = 32
	// pingTimeout closes a ping stream that has not delivered message_start.
	pingTimeout = 60 * time.Second
	// cacheTTL is how long a prompt cache entry lives after it was last read.
	cacheTTL = 300 * time.Second
	// maxEventPromptRunes bounds the first prompt stored with each ping.
	maxEventPromptRunes = 80
)

// Stop reasons, also reported in the status API.
const (
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
	stopReplaced   = "replaced"
)

// KindReturn marks the event written when a request ends a round of pings.
const KindReturn = "return"

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
	// lastEvent is the time of the latest event recorded, a ping or a
	// return, so that the page knows when to reload the history.
	lastEvent time.Time
	// store saves the conversations so that they survive a restart of CPA.
	// restored is set once the saved conversations have been loaded.
	store    *convStore
	restored bool
	// ended keeps the rounds of conversations snuffed out while their cache
	// may still be warm, until it expires, so that a request coming back in
	// that time is recorded as a return.
	ended map[string]round
	// seen and awaiting record the usage of requests; see usage.go.
	seen     map[string]lastSeen
	awaiting map[string]*awaitingUsage
}

// round is the state of a conversation's round of pings that a return is
// judged by.
type round struct {
	lastRequest time.Time
	lastRead    time.Time
	pings       int
	// context is the input of the round's last successful ping.
	context int64
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
	// minutes, when set on the management page, replaces the configured
	// duration until the next request.
	minutes int
}

// Event is the result of one ping; with Kind KindReturn, the request that
// ended a round of pings; with Kind KindRequest, the usage of a request.
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
	Kind    string    `json:"kind,omitempty"`
	// Saved, Idle, Context and Pings describe a return: whether it came
	// back within the cache's lifetime after the last successful ping and
	// at least that long after the previous request, the seconds since
	// the previous request, the input of the round's last successful ping
	// and the pings of the round.
	Saved   bool  `json:"saved,omitempty"`
	Idle    int64 `json:"idle,omitempty"`
	Context int64 `json:"context,omitempty"`
	Pings   int   `json:"pings,omitempty"`
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
		ended:    make(map[string]round),
		seen:     make(map[string]lastSeen),
		awaiting: make(map[string]*awaitingUsage),
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
// Conversations whose cache already lasts their new duration are snuffed
// out.
func (m *Manager) Configure(cfg config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
	m.stopped = false
	for key, c := range m.convs {
		if c.req.Other && !cfg.OtherClients {
			m.removeLocked(key)
			m.logLocked("info", "other clients disabled, keep-alive stopped", c.req, c.pings, stopDisabled)
			continue
		}
		if m.doneLocked(c) {
			m.retireLocked(key, c)
			m.logLocked("info", "duration shortened, keep-alive stopped", c.req, c.pings, stopUntil)
		}
	}
}

// doneLocked reports whether c needs no more pings: Wick is off for it, or
// its cache already lasts until the end of its duration.
func (m *Manager) doneLocked(c *conversation) bool {
	return (c.req.Other && !m.cfg.OtherClients) || m.defaultMinutesLocked(c.req) == 0 || !m.untilLocked(c).After(c.lastRead.Add(cacheTTL))
}

// defaultMinutesLocked returns the configured duration of req's
// conversation: subagent_duration_minutes for subagents, duration_minutes
// otherwise. duration_minutes 0 turns Wick off, subagents included.
func (m *Manager) defaultMinutesLocked(req request) int {
	if m.cfg.DurationMinutes == 0 {
		return 0
	}
	if req.Agent != "" {
		return m.cfg.SubagentDurationMinutes
	}
	return m.cfg.DurationMinutes
}

// untilLocked returns the end of c's duration: the duration set on the
// management page, or the configured one, after its last request.
func (m *Manager) untilLocked(c *conversation) time.Time {
	minutes := c.minutes
	if minutes == 0 {
		minutes = m.defaultMinutesLocked(c.req)
	}
	return c.lastRequest.Add(time.Duration(minutes) * time.Minute)
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
	clear(m.ended)
	clear(m.seen)
	clear(m.awaiting)
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

// Observe records an intercepted request and schedules it. For eligible
// non-native clients it returns the pinned body CPA must send; otherwise nil.
func (m *Manager) Observe(in pluginapi.RequestInterceptRequest) []byte {
	m.mu.Lock()
	minBody, otherClients := m.cfg.MinBodyKB*1024, m.cfg.OtherClients
	m.mu.Unlock()
	req, skip := parseRequest(in, minBody, otherClients)
	if skip == skipTTL1h {
		identity := ""
		if req.Other {
			identity = cacheIdentityKey(in.Body)
		}
		m.mu.Lock()
		for key, c := range m.convs {
			if req.Other {
				if !c.req.Other || c.req.CallerScope != req.CallerScope || c.req.Session != req.Session || c.req.Agent != req.Agent || c.req.Model != req.Model {
					continue
				}
				if identity == "" || cacheIdentityKey(c.req.Body) != identity {
					continue
				}
			} else if key != req.Key {
				continue
			}
			m.removeLocked(key)
			m.logLocked("info", "1h cache in use, keep-alive not needed", c.req, 0, stopTTL1h)
		}
		m.mu.Unlock()
		return nil
	}
	if skip != "" {
		return nil
	}

	m.mu.Lock()
	// Pin the outgoing body only when Wick can actually keep this client lit.
	if req.Other && (m.stopped || !m.cfg.OtherClients || m.defaultMinutesLocked(req) <= 5) {
		// A new request resets a per-conversation duration. Do not keep
		// replaying the previous request when its default needs no pings.
		m.removeLocked(req.Key)
		m.mu.Unlock()
		return nil
	}
	// Taken under the lock, so that a return is never earlier than the
	// start of a ping counted before it.
	ret, returned := m.observeLocked(req, m.clock.Now())
	m.mu.Unlock()
	// A short line appended to a file, only when a request ends a round of
	// pings, that is after at least one idle interval.
	if returned && m.recorder != nil {
		if err := m.recorder.Append(ret); err != nil {
			m.host.Log("warn", "wick: write ping history: "+err.Error(), nil)
		}
	}
	if req.Other {
		return req.Body
	}
	return nil
}

// observeLocked tracks req and returns the event to record when req ends a
// round of pings.
func (m *Manager) observeLocked(req request, now time.Time) (Event, bool) {
	if m.stopped || m.defaultMinutesLocked(req) == 0 {
		return Event{}, false
	}
	m.pruneEndedLocked(now)
	var ret Event
	returned := false
	c, ok := m.convs[req.Key]
	m.awaitLocked(req, now, c)
	if r, found := m.ended[req.Key]; found && !ok {
		ret, returned = m.returnLocked(req, now, r), true
	} else if ok && c.pings > 0 {
		ret, returned = m.returnLocked(req, now, roundOf(c)), true
	}
	delete(m.ended, req.Key)
	if ok {
		// Conversations that started while the main conversation waited for
		// its last response are its subagents. A new request from the main
		// conversation means they have returned and no longer need the cache.
		// Subagent requests carry an agent ID; without one every request is
		// treated as the main conversation.
		for key, other := range m.convs {
			if req.Agent == "" && key != req.Key && other.req.Session == req.Session && other.req.CallerScope == req.CallerScope && other.firstSeen.After(c.lastRequest) {
				m.removeLocked(key)
				m.logLocked("debug", "subagent finished, keep-alive stopped", other.req, other.pings, stopSuperseded)
			}
		}
		if c.timer != nil {
			c.timer.Stop()
		}
		c.minutes = 0
	} else {
		m.replaceLocked(req)
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
	// A duration of up to 5 minutes needs no pings.
	if m.doneLocked(c) {
		m.removeLocked(req.Key)
		return ret, returned
	}
	m.scheduleLocked(req.Key, c, now)
	return ret, returned
}

// roundOf returns the round of pings of c since its last request.
func roundOf(c *conversation) round {
	r := round{lastRequest: c.lastRequest, lastRead: c.lastRead, pings: c.pings}
	if c.lastUsage != nil && c.lastRead.After(c.lastRequest) {
		r.context = c.lastUsage.CacheReadTokens + c.lastUsage.CacheCreationTokens + c.lastUsage.InputTokens
	}
	return r
}

// returnLocked judges a request that ends round r. The pings saved a cache
// write when the request came back while the cache of the last successful
// ping was alive, and at least the cache's lifetime after the previous
// request, so that the cache would have expired without them.
func (m *Manager) returnLocked(req request, now time.Time, r round) Event {
	idle := now.Sub(r.lastRequest)
	saved := now.Sub(r.lastRead) < cacheTTL && idle >= cacheTTL
	if now.After(m.lastEvent) {
		m.lastEvent = now
	}
	ev := Event{
		Time:    now,
		Session: short(req.Session),
		Key:     short(req.Key),
		Model:   req.Model,
		Agent:   req.Agent != "",
		Client:  req.Client,
		Cwd:     req.Cwd,
		Prompt:  truncate(req.Prompt, maxEventPromptRunes),
		Kind:    KindReturn,
		Saved:   saved,
		Idle:    int64(idle / time.Second),
		Context: r.context,
		Pings:   r.pings,
	}
	message := "returned in time, the pings saved a cache write"
	if !saved {
		message = "returned, the pings saved nothing"
	}
	m.logLocked("info", fmt.Sprintf("%s idle=%ds", message, ev.Idle), req, r.pings, "")
	return ev
}

// retireLocked removes a conversation whose cache may stay warm for a
// while and keeps its round until the cache expires, so that a request
// coming back in that time is judged as a return.
func (m *Manager) retireLocked(key string, c *conversation) {
	if c.pings > 0 {
		m.ended[key] = roundOf(c)
	}
	m.removeLocked(key)
}

// pruneEndedLocked forgets the ended rounds whose cache has expired.
func (m *Manager) pruneEndedLocked(now time.Time) {
	for key, r := range m.ended {
		if !now.Before(r.lastRead.Add(cacheTTL)) {
			delete(m.ended, key)
		}
	}
}

// replaceLocked snuffs out the conversations that req continues under a
// new key: same session and agent, and req's messages open with the old
// request's messages, all but the last one, which clients may extend with
// the next turn, unchanged apart from cache_control. The key changes when
// tools are added, such as MCP servers or deferred tools loaded
// mid-conversation, or when the system prompt or the model changes. The
// cache is matched from the start, tools first, so the old conversation's
// cache can no longer be read. Comparing the messages spares Claude Code's
// helper requests, which share the session ID but do not continue the
// conversation.
func (m *Manager) replaceLocked(req request) {
	var messages []string
	parsed := false
	for key, other := range m.convs {
		if other.req.CallerScope != req.CallerScope || other.req.Session != req.Session || other.req.Agent != req.Agent {
			continue
		}
		if !parsed {
			for _, msg := range gjson.GetBytes(req.Body, "messages").Array() {
				messages = append(messages, messageWithoutCacheControl(msg))
			}
			parsed = true
		}
		if continues(other.req.Body, messages) {
			m.removeLocked(key)
			m.logLocked("info", "conversation continued with other tools, system prompt or model, keep-alive stopped", other.req, other.pings, stopReplaced)
		}
	}
}

// continues reports whether messages open with the messages of the request
// body old, all but the last one, ignoring cache_control on messages and
// their direct content blocks. A conversation of one message is never
// matched, since none of it can be compared.
func continues(old []byte, messages []string) bool {
	before := gjson.GetBytes(old, "messages").Array()
	if len(before) < 2 || len(messages) < len(before) {
		return false
	}
	for i := 0; i < len(before)-1; i++ {
		if messageWithoutCacheControl(before[i]) != messages[i] {
			return false
		}
	}
	return true
}

// messageWithoutCacheControl ignores cache markers on the message and its
// direct content blocks, leaving nested tool inputs and results untouched.
func messageWithoutCacheControl(msg gjson.Result) string {
	raw := msg.Raw
	if msg.Get("cache_control").Exists() {
		raw, _ = sjson.Delete(raw, "cache_control")
	}
	if content := msg.Get("content"); content.IsArray() {
		for i, block := range content.Array() {
			if block.Get("cache_control").Exists() {
				raw, _ = sjson.Delete(raw, fmt.Sprintf("content.%d.cache_control", i))
			}
		}
	}
	return raw
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

// Errors of SetDuration.
var (
	ErrNotFound = errors.New("conversation not found")
	// ErrPassed: the cache already lasts past the duration, so it would
	// not change anything.
	ErrPassed = errors.New("the duration has already passed")
)

// SetDuration keeps the conversation whose short key is key lit until
// minutes after its last request, in place of the configured duration,
// until the conversation's next request. minutes 0 returns it to the
// configured duration. A duration the cache already lasts would stop the
// pings at once and returns ErrPassed, so that the menu cannot snuff out a
// conversation.
func (m *Manager) SetDuration(key string, minutes int) error {
	if minutes < 0 || minutes > config.MaxDurationMinutes {
		return fmt.Errorf("minutes must be between 0 and %d", config.MaxDurationMinutes)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var matched []*conversation
	for full, c := range m.convs {
		if short(full) != key {
			continue
		}
		set := *c
		set.minutes = minutes
		if m.doneLocked(&set) {
			return ErrPassed
		}
		matched = append(matched, c)
	}
	if len(matched) == 0 {
		return ErrNotFound
	}
	for _, c := range matched {
		c.minutes = minutes
		if minutes > 0 {
			m.logLocked("info", fmt.Sprintf("kept lit for %d min from the management page", minutes), c.req, c.pings, "")
		} else {
			m.logLocked("info", "duration reset from the management page", c.req, c.pings, "")
		}
	}
	m.persistLocked()
	return nil
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
			m.retireLocked(full, c)
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
		set := c.minutes > 0
		if oldest == "" || (oldestSet && !set) || (oldestSet == set && c.lastRequest.Before(oldestAt)) {
			oldest, oldestAt, oldestSet = key, c.lastRequest, set
		}
	}
	if c, ok := m.convs[oldest]; ok {
		m.retireLocked(oldest, c)
		m.logLocked("info", "too many conversations, keep-alive stopped", c.req, c.pings, stopEvicted)
	}
}

// fire sends one ping. gen guards against a request that arrived after the
// timer was set.
func (m *Manager) fire(key string, gen uint64) {
	req, n, start, ok := m.beginPing(key, gen, m.enabled())
	if !ok {
		return
	}
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
func (m *Manager) beginPing(key string, gen uint64, enabled bool) (request, int, time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[key]
	if !ok || c.gen != gen || m.stopped {
		return request{}, 0, time.Time{}, false
	}
	if !enabled {
		m.removeLocked(key)
		m.logLocked("info", "plugin disabled, keep-alive stopped", c.req, c.pings, stopDisabled)
		return request{}, 0, time.Time{}, false
	}
	c.pings++
	// The start is taken under the lock, so that a request arriving after
	// the ping was counted is recorded after it.
	return c.req, c.pings, m.clock.Now(), true
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
	if start.After(m.lastEvent) {
		m.lastEvent = start
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
	case usage.CacheReadTokens <= 0 || usage.CacheCreationTokens > 0:
		// Renewal requires a cache read with no writes. Even a partial
		// write can indicate expiration or a changed prefix; stop to avoid
		// paying for it repeatedly. No read also stops uncached replays.
		if stillCurrent {
			event.Stopped = stopCacheMiss
			m.removeLocked(key)
		}
		m.logLocked("warn", "ping missed the cache", req, n, event.Stopped, usage)
	default:
		if stillCurrent {
			current.lastUsage = usage
			current.lastRead = start
			if m.doneLocked(current) {
				event.Stopped = stopUntil
				m.retireLocked(key, current)
			} else {
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
		CallerScope:   req.CallerScope,
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
	// LastRead is when the cache was last read; it lasts 300 seconds more.
	LastRead time.Time `json:"last_read"`
	NextPing time.Time `json:"next_ping"`
	// Pings counts the pings since the last request. TotalPings is filled
	// in by the plugin from the history.
	Pings      int    `json:"pings"`
	TotalPings int    `json:"total_pings"`
	BodyBytes  int    `json:"body_bytes"`
	LastUsage  *Usage `json:"last_usage,omitempty"`
	// Until is the end of the conversation's duration: Minutes, set on the
	// management page, or DefaultMinutes after the last request.
	Until          time.Time `json:"until"`
	Minutes        int       `json:"minutes,omitempty"`
	DefaultMinutes int       `json:"default_minutes"`
}

// Status is the response of the status API.
type Status struct {
	Config        config.Config        `json:"config"`
	Conversations []ConversationStatus `json:"conversations"`
	// LastEvent is the time of the latest ping or return recorded since the
	// plugin was loaded.
	LastEvent time.Time `json:"last_event"`
}

// Status returns the tracked conversations.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{
		Config:        m.cfg,
		Conversations: make([]ConversationStatus, 0, len(m.convs)),
		LastEvent:     m.lastEvent,
	}
	for _, c := range m.convs {
		cs := ConversationStatus{
			Session:        c.req.Session,
			Key:            short(c.req.Key),
			Model:          c.req.Model,
			Agent:          c.req.Agent != "",
			Client:         c.req.Client,
			Cwd:            c.req.Cwd,
			Prompt:         c.req.Prompt,
			AuthID:         c.req.AuthID,
			FirstSeen:      c.firstSeen,
			LastRequest:    c.lastRequest,
			LastRead:       c.lastRead,
			NextPing:       c.nextPing,
			Pings:          c.pings,
			Until:          m.untilLocked(c),
			Minutes:        c.minutes,
			DefaultMinutes: m.defaultMinutesLocked(c.req),
			BodyBytes:      len(c.req.Body),
			LastUsage:      c.lastUsage,
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
