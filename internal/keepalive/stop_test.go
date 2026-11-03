package keepalive

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func stopEvents(rec *fakeRecorder) []Event {
	var out []Event
	for _, ev := range rec.all() {
		if ev.Kind == KindStop {
			out = append(out, ev)
		}
	}
	return out
}

func TestStopEvents(t *testing.T) {
	for _, reason := range []string{stopManual, stopReplaced, stopSuperseded, "replaced-main-subagent", stopEvicted} {
		for _, lit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/lit=%t", reason, lit), func(t *testing.T) {
				m, clock, _, rec := newRecordingManager(testConfig())
				main := intercept("main", "session-with-long-id", chat("Working directory: /project", []string{"Bash"}, "first", "second", "third"))
				m.Observe(main)
				target := main
				if reason == stopSuperseded || reason == "replaced-main-subagent" {
					clock.Advance(10 * time.Second)
					target = withAgent(intercept("child", "session-with-long-id", chat("Working directory: /project", []string{"Bash"}, "child first", "second")), "agent-1")
					m.Observe(target)
				}
				if lit {
					clock.Advance(280*time.Second - clock.Now().Sub(start))
				} else {
					clock.Advance(time.Second)
				}
				req, skip := parseRequest(target, 0, false)
				if skip != "" {
					t.Fatal(skip)
				}
				switch reason {
				case stopManual:
					if !m.StopConversation(short(req.Key)) || m.StopConversation(short(req.Key)) {
						t.Fatal("manual stop did not remove the conversation exactly once")
					}
				case stopReplaced, "replaced-main-subagent":
					m.Observe(intercept("new-main", "session-with-long-id", chat("Working directory: /project", []string{"Bash", "Read"}, "first", "second", "extended third", "fourth")))
				case stopSuperseded:
					m.Observe(intercept("next-main", "session-with-long-id", main.Body))
				case stopEvicted:
					for i := 0; i < maxConversations; i++ {
						m.Observe(intercept(fmt.Sprint(i), fmt.Sprintf("other-%d", i), body("other", "first")))
					}
				}
				var got []Event
				for _, ev := range stopEvents(rec) {
					if ev.Key == short(req.Key) {
						got = append(got, ev)
					}
				}
				if !lit {
					if len(stopEvents(rec)) != 0 {
						t.Fatalf("unlit rounds wrote stops: %+v", stopEvents(rec))
					}
					return
				}
				wantReason := reason
				if reason == "replaced-main-subagent" {
					wantReason = stopSuperseded
				}
				if len(got) != 1 {
					t.Fatalf("stop events %+v, want exactly one for %s", got, short(req.Key))
				}
				ev := got[0]
				if ev.Stopped != wantReason || ev.Pings != 1 || ev.Ping != 0 || ev.Usage != nil || ev.Saved ||
					ev.Session != short(req.Session) || ev.Model != req.Model || ev.Agent != (req.Agent != "") ||
					ev.Client != req.Client || ev.Cwd != req.Cwd || ev.Prompt != req.Prompt || !ev.Time.Equal(clock.Now()) {
					t.Fatalf("stop %+v does not describe %+v", ev, req)
				}
				if !m.Status().LastEvent.Equal(ev.Time) {
					t.Fatal("stop did not update last_event")
				}
				if m.convs[req.Key] != nil {
					t.Fatal("stopped conversation remains tracked")
				}
			})
		}
	}
}

func TestStopAfterReturnDoesNotRecordPreviousRound(t *testing.T) {
	m, clock, _, rec := newRecordingManager(testConfig())
	in := intercept("first", "session", body("main", "first"))
	m.Observe(in)
	clock.Advance(280 * time.Second)
	m.Observe(intercept("second", "session", body("main", "second")))
	if len(returns(rec)) != 1 {
		t.Fatal("missing return")
	}
	m.StopConversation(m.Status().Conversations[0].Key)
	if got := stopEvents(rec); len(got) != 0 {
		t.Fatalf("stop after return reused previous pings: %+v", got)
	}
}

func TestOtherStopReasonsHaveNoStopEvent(t *testing.T) {
	for _, reason := range []string{stopUntil, stopError, stopCacheMiss, stopDisabled, stopFailed, stopExpired} {
		t.Run(reason, func(t *testing.T) {
			cfg := testConfig()
			if reason == stopUntil || reason == stopExpired {
				cfg.DurationMinutes = 9
			}
			m, clock, h, rec := newRecordingManager(cfg)
			in := intercept("first", "session", body("main", "first"))
			m.Observe(in)
			clock.Advance(280 * time.Second)
			switch reason {
			case stopError:
				h.execErr = fmt.Errorf("upstream failed")
				clock.Advance(260 * time.Second)
			case stopCacheMiss:
				h.cacheCreation = 1
				clock.Advance(260 * time.Second)
			case stopDisabled:
				m.enabled = func() bool { return false }
				clock.Advance(260 * time.Second)
			case stopFailed:
				m.Complete(pluginapi.RequestCompletion{RequestID: in.RequestID, Outcome: pluginapi.RequestCompletionFailed})
			case stopExpired:
				clock.Advance(cacheTTL)
				m.Observe(intercept("next", "session", in.Body))
			}
			if got := stopEvents(rec); len(got) != 0 {
				t.Fatalf("%s wrote stop events: %+v", reason, got)
			}
			if reason == stopUntil && (len(rec.all()) != 1 || rec.all()[0].Stopped != stopUntil) {
				t.Fatalf("until must stay on the last ping: %+v", rec.all())
			}
		})
	}
}

func TestStopEventPromptIsBounded(t *testing.T) {
	m, clock, _, rec := newRecordingManager(testConfig())
	m.Observe(intercept("first", "session", chat("main", nil, strings.Repeat("灯", 200))))
	clock.Advance(280 * time.Second)
	m.StopConversation(m.Status().Conversations[0].Key)
	got := stopEvents(rec)
	if len(got) != 1 || got[0].Prompt != truncate(strings.Repeat("灯", 200), maxEventPromptRunes) {
		t.Fatalf("stop prompt is not bounded: %+v", got)
	}
}
