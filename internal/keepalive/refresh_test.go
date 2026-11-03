package keepalive

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type recorderFunc func(Event) error

func (f recorderFunc) Append(ev Event) error { return f(ev) }

func statusRefreshMarker(t *testing.T, m *Manager) string {
	t.Helper()
	raw, err := json.Marshal(m.Status())
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		LastEvent string `json:"last_event"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	return status.LastEvent
}

func TestLatePingPublishesRefreshAfterStop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage *Usage
		err   error
	}{
		{"hit", &Usage{CacheReadTokens: 40000}, nil},
		{"miss", &Usage{CacheCreationTokens: 40000}, nil},
		{"failed", nil, errors.New("upstream failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, clock, _, rec := newRecordingManager(testConfig())
			m.Observe(intercept("first", "session", body("main", "first")))
			key := conversationKey("", "session", "", "claude-opus-5-5", body("main", "first"))
			gen := m.convs[key].gen
			// Drive the in-flight ping explicitly, without firing its timer.
			clock.now = start.Add(260 * time.Second)
			req, n, at, ok := m.beginPing(key, gen, true)
			if !ok || n != 1 || !at.Equal(clock.Now()) {
				t.Fatal("ping did not begin")
			}
			clock.now = start.Add(261 * time.Second)
			if !m.StopConversation(short(key)) {
				t.Fatal("conversation did not stop")
			}
			// The page has read the stop while the ping is still absent.
			seen := rec.all()
			if len(seen) != 1 || seen[0].Kind != KindStop || !seen[0].Time.Equal(clock.Now()) {
				t.Fatalf("stop was not readable before ping completion: %+v", seen)
			}
			stopMarker, stopTime := statusRefreshMarker(t, m), m.Status().LastEvent
			clock.now = start.Add(262 * time.Second)
			ev := m.finishPing(key, gen, req, n, at, tc.usage, tc.err)
			if !ev.Time.Equal(at) || ev.Kind != "" || ev.Ping != 1 || ev.Usage != tc.usage || (ev.Error != "") != (tc.err != nil) {
				t.Fatalf("late ping lost business time or outcome: %+v", ev)
			}
			if statusRefreshMarker(t, m) != stopMarker {
				t.Fatal("finishPing published before the event was appended")
			}
			m.record(ev)
			if got := rec.all(); len(got) != 2 || !got[1].Time.Equal(at) {
				t.Fatalf("late ping was not appended with its original time: %+v", got)
			}
			if !m.Status().LastEvent.After(stopTime) || statusRefreshMarker(t, m) == stopMarker {
				t.Fatal("late ping did not change the refresh marker seen by the page")
			}
		})
	}
}

func TestRefreshMarkerPublishedOnlyAfterSuccessfulAppend(t *testing.T) {
	m, clock, _, _ := newRecordingManager(testConfig())
	before := m.Status().LastEvent
	var received []Event
	m.recorder = recorderFunc(func(ev Event) error {
		if !m.mu.TryLock() {
			t.Fatal("Append was called while the manager lock was held")
		}
		marker := m.lastEvent
		m.mu.Unlock()
		if !marker.Equal(before) {
			t.Fatal("refresh marker was published before Append returned")
		}
		received = append(received, ev)
		return nil
	})
	for i, kind := range []string{"", KindStop, KindReturn, KindRequest} {
		if i == 2 {
			// A wall-clock rollback must not move the marker backwards.
			clock.now = start.Add(-time.Hour)
		}
		previous := statusRefreshMarker(t, m)
		ev := Event{Time: start.Add(24 * time.Hour), Kind: kind}
		m.record(ev)
		marker := m.Status().LastEvent
		if !marker.After(before) || statusRefreshMarker(t, m) == previous {
			t.Fatalf("marker did not increase at a fixed or earlier clock: %v -> %v", before, marker)
		}
		if !received[i].Time.Equal(ev.Time) {
			t.Fatal("refresh publication changed the event's business time")
		}
		before = marker
	}
	m.recorder = recorderFunc(func(Event) error { return errors.New("disk unavailable") })
	m.record(Event{Time: start.Add(48 * time.Hour)})
	if !m.Status().LastEvent.Equal(before) {
		t.Fatal("failed append changed the refresh marker")
	}
	m.recorder = nil
	m.record(Event{Time: start.Add(72 * time.Hour)})
	if !m.Status().LastEvent.Equal(before) {
		t.Fatal("missing recorder changed the refresh marker")
	}
}

func TestFailedStopAndReturnAppendDoNotPublishRefresh(t *testing.T) {
	m, clock, _, _ := newRecordingManager(testConfig())
	m.Observe(intercept("first", "session", body("main", "first")))
	key := conversationKey("", "session", "", "claude-opus-5-5", body("main", "first"))
	clock.now = start.Add(260 * time.Second)
	if _, _, _, ok := m.beginPing(key, m.convs[key].gen, true); !ok {
		t.Fatal("ping did not begin")
	}
	var attempted []string
	m.recorder = recorderFunc(func(ev Event) error {
		attempted = append(attempted, ev.Kind)
		return errors.New("disk unavailable")
	})
	before := m.Status().LastEvent
	clock.now = start.Add(261 * time.Second)
	if !m.StopConversation(short(key)) || !m.Status().LastEvent.Equal(before) {
		t.Fatal("failed stop append published a refresh marker")
	}
	clock.now = start.Add(280 * time.Second)
	m.Observe(intercept("next", "session", body("main", "next")))
	if !m.Status().LastEvent.Equal(before) || len(attempted) != 2 || attempted[0] != KindStop || attempted[1] != KindReturn {
		t.Fatalf("failed return append published a refresh marker or was not attempted: %v", attempted)
	}
}

func TestUsagePublishesRefreshAfterAppend(t *testing.T) {
	m, clock, _, rec := newRecordingManager(testConfig())
	m.Observe(intercept("first", "session", body("main", "first")))
	before := m.Status().LastEvent
	clock.now = start.Add(time.Second)
	m.Usage(usageRecord("first", "", 40000, 0, 3))
	if len(requestEvents(rec)) != 1 || !m.Status().LastEvent.After(before) {
		t.Fatal("request usage did not publish a refresh marker after append")
	}
}
