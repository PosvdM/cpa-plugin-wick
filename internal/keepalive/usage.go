package keepalive

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// KindRequest marks the event written with the usage of a request of a
// conversation Wick keeps lit, in Usage. When the conversation's previous
// request came less than an hour before, Idle holds the seconds since it
// and Context its input, if its usage was known. The management page
// compares the cache prices of these requests with those of a 1-hour
// cache.
const KindRequest = "request"

const (
	// hourTTL is the lifetime of a 1-hour cache entry after it was last
	// read.
	hourTTL = time.Hour
	// maxAwaiting bounds the requests waiting for their usage and the
	// conversations remembered by seen.
	maxAwaiting = 1000
	// awaitTimeout forgets a request whose usage has not arrived, such as
	// one that failed.
	awaitTimeout = time.Hour
)

// lastSeen is the latest request of a conversation: when it came and its
// input, known once its usage arrives.
type lastSeen struct {
	at      time.Time
	context int64
}

// awaitingUsage is a request whose usage has not arrived yet. ev is the
// event to record, without the usage.
type awaitingUsage struct {
	ev  Event
	key string
}

// awaitingKey returns the key a request and its usage record share. CPA
// gives every upstream execution its own request ID in usage records, so
// they are matched by the trace ID of the inbound HTTP request; the request
// ID is used only when there is no trace ID.
func awaitingKey(requestID, traceID string) string {
	if traceID = strings.TrimSpace(traceID); traceID != "" {
		return "trace:" + traceID
	}
	if requestID = strings.TrimSpace(requestID); requestID != "" {
		return "id:" + requestID
	}
	return ""
}

// awaitLocked remembers req until its usage arrives. c is req's tracked
// conversation, or nil.
func (m *Manager) awaitLocked(req request, now time.Time, c *conversation) {
	m.pruneAwaitingLocked(now)
	id := awaitingKey(req.RequestID, req.TraceID)
	// CPA intercepts every attempt of a request, such as a retry with
	// another account, under the same trace ID. The first attempt holds
	// the time since the previous request.
	if _, ok := m.awaiting[id]; ok {
		return
	}
	ev := Event{
		Time:    now,
		Session: short(req.Session),
		Key:     short(req.Key),
		Model:   req.Model,
		Agent:   req.Agent != "",
		Kind:    KindRequest,
	}
	if s, ok := m.seen[req.Key]; ok && now.Sub(s.at) < hourTTL {
		ev.Idle, ev.Context = int64(now.Sub(s.at)/time.Second), s.context
	} else if !ok && c != nil && now.Sub(c.lastRequest) < hourTTL {
		// After a reload seen is empty, but the restored conversation
		// still knows its last request.
		ev.Idle = int64(now.Sub(c.lastRequest) / time.Second)
		// lastUsage belongs to this round only after a ping since the
		// last request.
		if c.lastUsage != nil && c.lastRead.After(c.lastRequest) {
			u := c.lastUsage
			ev.Context = u.CacheReadTokens + u.CacheCreationTokens + u.InputTokens
		}
	}
	s := m.seen[req.Key]
	s.at = now
	m.seen[req.Key] = s
	if id != "" {
		m.awaiting[id] = &awaitingUsage{ev: ev, key: req.Key}
	}
}

// pruneAwaitingLocked forgets requests whose usage has not arrived within
// awaitTimeout and conversations idle for longer than a 1-hour cache
// lives. Beyond maxAwaiting entries it drops arbitrary ones.
func (m *Manager) pruneAwaitingLocked(now time.Time) {
	for id, a := range m.awaiting {
		if now.Sub(a.ev.Time) >= awaitTimeout || len(m.awaiting) > maxAwaiting {
			delete(m.awaiting, id)
		}
	}
	for key, s := range m.seen {
		if now.Sub(s.at) >= hourTTL || len(m.seen) > maxAwaiting {
			delete(m.seen, key)
		}
	}
}

// Usage records the usage of a request remembered by awaitLocked. Usage of
// other requests, including Wick's own pings, is ignored.
func (m *Manager) Usage(rec pluginapi.UsageRecord) {
	m.mu.Lock()
	id := awaitingKey(rec.RequestID, rec.TraceID)
	a := m.awaiting[id]
	// A failed attempt may be retried under the same trace ID, so the
	// request keeps waiting.
	if a == nil || rec.Failed {
		m.mu.Unlock()
		return
	}
	delete(m.awaiting, id)
	d := rec.Detail
	ev := a.ev
	ev.Usage = &Usage{InputTokens: d.InputTokens, CacheReadTokens: d.CacheReadTokens, CacheCreationTokens: d.CacheCreationTokens}
	if s, ok := m.seen[a.key]; ok {
		s.context = d.InputTokens + d.CacheReadTokens + d.CacheCreationTokens
		m.seen[a.key] = s
	}
	m.mu.Unlock()
	if m.recorder != nil {
		if err := m.recorder.Append(ev); err != nil {
			m.host.Log("warn", "wick: write request usage: "+err.Error(), nil)
		}
	}
}
