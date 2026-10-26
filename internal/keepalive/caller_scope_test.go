package keepalive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func withScope(in pluginapi.RequestInterceptRequest, scope string) pluginapi.RequestInterceptRequest {
	in.CallerScope = scope
	return in
}

func TestCallerScopeReplay(t *testing.T) {
	for _, other := range []bool{false, true} {
		for _, tc := range []struct {
			name, direct string
			metadata     any
			want         string
		}{
			{"direct", "caller-a", "caller-b", "caller-a"},
			{"fallback", "", "caller-b", "caller-b"},
			{"empty", "", nil, ""},
			{"invalid metadata", "", 123, ""},
		} {
			t.Run(fmt.Sprintf("other=%t/%s", other, tc.name), func(t *testing.T) {
				m, clock, h, rec := newRecordingManager(otherClientsConfig())
				defer m.Stop()
				in := intercept("r", "sess", body("main", "1"))
				if other {
					in = otherRequest(in.Body)
				}
				in.CallerScope = tc.direct
				in.Metadata[pluginapi.CallerScopeMetadataKey] = tc.metadata
				parsed, skip := parseRequest(in, 0, true)
				if skip != "" || parsed.CallerScope != tc.want {
					t.Fatalf("scope %q skip %q", parsed.CallerScope, skip)
				}
				m.Observe(in)
				clock.Advance(260 * time.Second)
				if h.pings() != 1 || h.requests[0].CallerScope != tc.want {
					t.Fatalf("replays %+v", h.requests)
				}
				status, _ := json.Marshal(m.Status())
				events, _ := json.Marshal(rec.all())
				for _, out := range []string{string(status), string(events), strings.Join(h.logs, "\n")} {
					if strings.Contains(out, "caller_scope") || (tc.want != "" && strings.Contains(out, tc.want)) {
						t.Fatal("caller scope exposed")
					}
				}
				in.Body = bytes.ReplaceAll(in.Body, []byte(`"type":"ephemeral"`), []byte(`"type":"ephemeral","ttl":"1h"`))
				parsed, skip = parseRequest(in, 0, true)
				if skip != skipTTL1h || parsed.CallerScope != tc.want {
					t.Fatal("scope missing from 1h request")
				}
			})
		}
	}
}

func TestCallerScopeRestore(t *testing.T) {
	for _, other := range []bool{false, true} {
		for _, scope := range []string{"", "caller-a"} {
			t.Run(fmt.Sprintf("other=%t/scope=%s", other, scope), func(t *testing.T) {
				dir := t.TempDir()
				m, _, _ := newTestManager(otherClientsConfig())
				m.SetStore(dir)
				in := intercept("r", "sess", body("main", "1"))
				if other {
					in = otherRequest(in.Body)
				}
				m.Observe(withScope(in, scope))
				saveNow(t, m)
				m.Stop()
				files, _ := filepath.Glob(filepath.Join(dir, "*.req"))
				if len(files) != 1 {
					t.Fatalf("files %v", files)
				}
				raw, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				head, _, _ := bytes.Cut(raw, []byte{'\n'})
				if !bytes.Contains(head, []byte(`"caller_scope":"`+scope+`"`)) {
					t.Fatalf("scope missing from header %s", head)
				}
				clock := &fakeClock{now: start.Add(time.Minute)}
				h := &fakeHost{cacheRead: 40000}
				m2 := New(Options{Host: h, Clock: clock, Rand: func() float64 { return 0 }})
				m2.Configure(otherClientsConfig())
				m2.SetStore(dir)
				defer m2.Stop()
				clock.Advance(200 * time.Second)
				if h.pings() != 1 || h.requests[0].CallerScope != scope {
					t.Fatalf("restored replay %+v", h.requests)
				}
			})
		}
	}
}

func TestRestoreDiscardsLegacyCallerScope(t *testing.T) {
	dir := t.TempDir()
	m, _, _ := newTestManager(otherClientsConfig())
	m.SetStore(dir)
	m.Observe(intercept("native", "native", body("main", "1")))
	m.Observe(otherRequest(body("other", "1")))
	m.Stop()
	files, _ := filepath.Glob(filepath.Join(dir, "*.req"))
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.Replace(raw, []byte(`"caller_scope":"",`), nil, 1)
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	m2, clock, h := restoredManager(dir, start.Add(time.Minute))
	defer m2.Stop()
	if len(m2.Status().Conversations) != 0 {
		t.Fatal("legacy requests restored")
	}
	clock.Advance(260 * time.Second)
	if h.pings() != 0 {
		t.Fatal("legacy requests replayed")
	}
	if !strings.Contains(strings.Join(h.logs, "\n"), "info wick: discarded 2 saved conversations without caller_scope; replay identity is unknown") {
		t.Fatalf("logs %v", h.logs)
	}
	saveNow(t, m2)
	files, _ = filepath.Glob(filepath.Join(dir, "*.req"))
	if len(files) != 0 {
		t.Fatalf("legacy files retained %v", files)
	}
}

func TestCallerScopeConversationsDoNotOverwrite(t *testing.T) {
	m, clock, h := newTestManager(testConfig())
	defer m.Stop()
	for _, scope := range []string{"caller-a", "caller-b"} {
		m.Observe(withScope(intercept(scope, "sess", body("main", "1")), scope))
	}
	if len(m.Status().Conversations) != 2 {
		t.Fatal("different callers share a conversation")
	}
	clock.Advance(260 * time.Second)
	if h.pings() != 2 || h.requests[0].CallerScope == h.requests[1].CallerScope {
		t.Fatalf("replays %+v", h.requests)
	}
}

func TestCallerScopeCleanupIsolation(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(fmt.Sprintf("same=%t", same), func(t *testing.T) {
			scope := "caller-b"
			if same {
				scope = "caller-a"
			}
			t.Run("other 1h", func(t *testing.T) {
				m, _, _ := newTestManager(otherClientsConfig())
				defer m.Stop()
				in := otherRequest(body("main", "1"))
				m.Observe(withScope(in, "caller-a"))
				in.Body = bytes.ReplaceAll(in.Body, []byte(`"type":"ephemeral"`), []byte(`"type":"ephemeral","ttl":"1h"`))
				m.Observe(withScope(in, scope))
				want := 1
				if same {
					want = 0
				}
				if len(m.convs) != want {
					t.Fatalf("conversations %d want %d", len(m.convs), want)
				}
			})
			t.Run("subagent return", func(t *testing.T) {
				m, clock, _ := newTestManager(testConfig())
				defer m.Stop()
				main := withScope(intercept("main", "sess", body("main", "1")), "caller-a")
				m.Observe(main)
				clock.Advance(time.Second)
				m.Observe(withScope(withAgent(intercept("agent", "sess", body("worker", "1")), "a1"), scope))
				clock.Advance(time.Second)
				m.Observe(main)
				want := 2
				if same {
					want = 1
				}
				if len(m.convs) != want {
					t.Fatalf("conversations %d want %d", len(m.convs), want)
				}
			})
			t.Run("replacement", func(t *testing.T) {
				m, _, _ := newTestManager(testConfig())
				defer m.Stop()
				old := []byte(`{"model":"claude-opus-5-5","system":"old","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"}]}`)
				m.Observe(withScope(intercept("old", "sess", old), "caller-a"))
				m.Observe(withScope(intercept("new", "sess", bytes.Replace(old, []byte(`"old"`), []byte(`"new"`), 1)), scope))
				want := 2
				if same {
					want = 1
				}
				if len(m.convs) != want {
					t.Fatalf("conversations %d want %d", len(m.convs), want)
				}
			})
		})
	}
}
