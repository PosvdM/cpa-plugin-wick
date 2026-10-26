package keepalive

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestCacheControlInsideToolsIsPreserved(t *testing.T) {
	for _, tt := range []struct {
		name, block, nestedPath string
	}{
		{"tool input", `{"type":"tool_use","input":{"cache_control":{"type":"custom"}},"cache_control":{"type":"ephemeral"}}`, "content.0.input.cache_control"},
		{"tool result", `{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"result","cache_control":{"type":"custom"}}],"cache_control":{"type":"ephemeral"}}`, "content.0.content.0.cache_control"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			msg := gjson.Parse(`{"role":"user","content":[` + tt.block + `],"cache_control":{"type":"ephemeral"}}`)
			got := gjson.Parse(messageWithoutCacheControl(msg))
			if got.Get("cache_control").Exists() || got.Get("content.0.cache_control").Exists() {
				t.Fatal("message or direct content block cache marker was retained")
			}
			if nested := got.Get(tt.nestedPath); !nested.Exists() || nested.Raw != msg.Get(tt.nestedPath).Raw {
				t.Fatal("nested cache_control field was changed")
			}
		})
	}
}

func BenchmarkContinuationWithoutCacheControl(b *testing.B) {
	block := `{"type":"text","text":"` + strings.Repeat("a", 256) + `"}`
	message := `{"role":"user","content":[` + strings.TrimSuffix(strings.Repeat(block+",", 10), ",") + `]}`
	body := []byte(`{"messages":[` + strings.TrimSuffix(strings.Repeat(message+",", 500), ",") + `]}`)
	b.ReportAllocs()
	for b.Loop() {
		var messages []string
		for _, msg := range gjson.GetBytes(body, "messages").Array() {
			messages = append(messages, messageWithoutCacheControl(msg))
		}
		if !continues(body, messages) {
			b.Fatal("identical messages did not continue")
		}
	}
}

func TestMovingCacheControlReplacesOldKey(t *testing.T) {
	for _, position := range []string{"message", "content block"} {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/changed=%t", position, changed), func(t *testing.T) {
				cfg := testConfig()
				cfg.OtherClients = true
				m, clock, h := newTestManager(cfg)
				marker := `,"cache_control":{"type":"ephemeral"}`
				pinnedMarker := `,"cache_control":{"type":"ephemeral","ttl":"5m"}`
				toolResult := `{"type":"tool_result","tool_use_id":"tool-1","content":"output"}`
				marked := `{"role":"user","content":[` + toolResult + `]}`
				if position == "message" {
					marked = strings.TrimSuffix(marked, "}") + marker + "}"
				} else {
					marked = `{"role":"user","content":[` + strings.TrimSuffix(toolResult, "}") + marker + `}]}`
				}
				first := `{"role":"user","content":"question"}`
				last := `{"role":"assistant","content":[{"type":"text","text":"answer"}]}`
				makeRequest := func(id, snapshot, messages, expectedMessages string) []byte {
					body := []byte(`{"model":"claude-opus-5-5","system":[{"type":"text","text":"Snapshot session_start at ` + snapshot + `","cache_control":{"type":"ephemeral"}}],"messages":[` + messages + `],"stream":true}`)
					expected := []byte(`{"model":"claude-opus-5-5","system":[{"type":"text","text":"Snapshot session_start at ` + snapshot + `","cache_control":{"type":"ephemeral","ttl":"5m"}}],"messages":[` + expectedMessages + `],"stream":true,"prompt_cache_options":{"mode":"explicit"}}`)
					req := intercept(id, "pi-session", body)
					req.Headers.Set("User-Agent", "pi/0.60 node/24")
					req.Headers.Del("X-App")
					if got := m.Observe(req); !bytes.Equal(got, expected) {
						t.Fatal("Observe returned a body different from the expected pinned request")
					}
					key := conversationKey("", "pi-session", "", "claude-opus-5-5", expected)
					if saved := m.convs[key]; saved == nil || !bytes.Equal(saved.req.Body, expected) {
						t.Fatal("saved body differs from the expected pinned request")
					}
					return expected
				}
				expectedMarked := marked
				if position == "content block" {
					expectedMarked = strings.Replace(marked, marker, pinnedMarker, 1)
				}
				oldBody := makeRequest("r1", "14:45:12", first+","+marked+","+last, first+","+expectedMarked+","+last)
				oldKey := conversationKey("", "pi-session", "", "claude-opus-5-5", oldBody)
				clock.Advance(10 * time.Second)
				if changed {
					toolResult = strings.Replace(toolResult, "output", "different output", 1)
				}
				unmarked := `{"role":"user","content":[` + toolResult + `]}`
				latest := `{"role":"user","content":[{"type":"text","text":"next","cache_control":{"type":"ephemeral"}}]}`
				expectedLatest := strings.Replace(latest, marker, pinnedMarker, 1)
				newBody := makeRequest("r2", "14:54:30", first+","+unmarked+","+last+","+latest, first+","+unmarked+","+last+","+expectedLatest)
				newKey := conversationKey("", "pi-session", "", "claude-opus-5-5", newBody)
				if oldKey == newKey {
					t.Fatal("snapshot change did not change the conversation key")
				}
				wantConversations := 1
				if changed {
					wantConversations = 2
				}
				if got := len(m.Status().Conversations); got != wantConversations {
					t.Fatalf("conversations %d, want %d", got, wantConversations)
				}
				_, oldPresent := m.convs[oldKey]
				if oldPresent != changed {
					t.Fatalf("old key present=%t, content changed=%t", oldPresent, changed)
				}
				if got := strings.Contains(strings.Join(h.logs, "\n"), "stopped=replaced"); got == changed {
					t.Fatalf("replacement logged=%t, content changed=%t", got, changed)
				}
				clock.Advance(time.Hour)
				if got, want := h.pings(), wantConversations*defaultPings; got != want {
					t.Fatalf("pings %d, want %d", got, want)
				}
				for _, replay := range h.requests {
					if !bytes.Equal(replay.Body, newBody) && (!changed || !bytes.Equal(replay.Body, oldBody)) {
						t.Fatal("comparison changed the replay body")
					}
				}
			})
		}
	}
}
