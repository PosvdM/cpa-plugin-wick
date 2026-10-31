package keepalive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
		before := gjson.GetBytes(body, "messages").Array()
		if !continues(before, messages, commonMessagePrefix(before, messages)) {
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

// chatMessage accepts text or JSON content blocks and an optional
// message-level cache marker. Block markers can be set in content.
func chatMessage(role string, content any, cacheControl bool) string {
	message := map[string]any{"role": role, "content": content}
	if cacheControl {
		message["cache_control"] = map[string]string{"type": "ephemeral"}
	}
	raw, err := json.Marshal(message)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func chatMessages(system string, tools []string, messages ...string) []byte {
	body, err := sjson.SetRawBytes(chat(system, tools), "messages", []byte("["+strings.Join(messages, ",")+"]"))
	if err != nil {
		panic(err)
	}
	return body
}

func TestRewoundConversationReplacement(t *testing.T) {
	user := func(text string) string { return chatMessage("user", text, false) }
	assistant := func(text string) string { return chatMessage("assistant", text, false) }
	tool := chatMessage("user", json.RawMessage(`[{"type":"tool_result","tool_use_id":"call","content":"done"}]`), false)
	mixedTool := chatMessage("user", json.RawMessage(`[{"type":"text","text":"note"},{"type":"tool_result","tool_use_id":"call","content":"done"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]`), false)
	imageInput := chatMessage("user", json.RawMessage(`[{"type":"text","text":"new input"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]`), false)
	prefix := []string{user("first"), assistant("answer")}
	history := func(tail ...string) []string { return append(append([]string(nil), prefix...), tail...) }
	for _, tt := range []struct {
		name       string
		before     []string
		after      []string
		wantRewind bool
	}{
		{"one shared message", []string{user("first"), user("old"), assistant("reply")}, []string{user("first"), user("new")}, true},
		{"last user turn", history(user("old"), assistant("reply")), history(user("new")), true},
		{"last user turn with tool results", history(user("old"), assistant("tool call"), tool), history(user("new")), true},
		{"earlier user turn", history(user("old"), assistant("reply"), user("later"), assistant("another reply"), user("latest")), history(user("new")), true},
		{"longer resent history", history(user("old"), assistant("reply")), history(user("new"), assistant("new reply"), user("next")), true},
		{"text and image input", history(user("old"), assistant("reply")), history(imageInput), true},
		{"old assistant", history(assistant("old"), user("later")), history(user("new")), false},
		{"new assistant", history(user("old"), assistant("reply")), history(assistant("new")), false},
		{"old tool result", history(tool, assistant("reply")), history(user("new")), false},
		{"new tool result", history(user("old"), assistant("reply")), history(tool), false},
		{"old mixed tool result", history(mixedTool, assistant("reply")), history(user("new")), false},
		{"new mixed tool result", history(user("old"), assistant("reply")), history(mixedTool), false},
		{"first message differs", history(user("old"), assistant("reply")), []string{user("new first"), assistant("answer"), user("new")}, false},
		{"no divergent new message", history(user("old"), assistant("reply")), prefix, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, clock, h, rec := newRecordingManager(testConfig())
			oldBody := chatMessages("old system", nil, tt.before...)
			newBody := chatMessages("new system", nil, tt.after...)
			m.Observe(intercept("old", "sess", oldBody))
			clock.Advance(400 * time.Second) // one old-key ping
			m.Observe(intercept("new", "sess", newBody))
			wantConvs := 2
			if tt.wantRewind {
				wantConvs = 1
			}
			convs := m.Status().Conversations
			newKey := short(conversationKey("", "sess", "", "claude-opus-5-5", newBody))
			if len(convs) != wantConvs || (tt.wantRewind && convs[0].Key != newKey) {
				t.Fatalf("conversations %+v, want %d with new key %s", convs, wantConvs, newKey)
			}
			logs := strings.Join(h.logs, "\n")
			if got := strings.Contains(logs, "conversation rewound and resent, keep-alive stopped"); got != tt.wantRewind {
				t.Fatalf("rewind log = %v, want %v: %s", got, tt.wantRewind, logs)
			}
			if strings.Contains(logs, "stopped=replaced") != tt.wantRewind {
				t.Fatalf("replacement log: %s", logs)
			}
			if got := returns(rec); len(got) != 0 {
				t.Fatalf("returns %+v, want none for a new key", got)
			}
			clock.Advance(260 * time.Second)
			if h.pings() != 1+wantConvs {
				t.Fatalf("pings %d, want %d with only active keys scheduled", h.pings(), 1+wantConvs)
			}
		})
	}
}

func TestCacheMarkersDoNotShortenMessagePrefix(t *testing.T) {
	user := func(text string) string { return chatMessage("user", text, false) }
	assistant := chatMessage("assistant", "reply", false)
	plainBlocks := chatMessage("user", json.RawMessage(`[{"type":"text","text":"first"},{"type":"text","text":"second"}]`), false)
	markedFirstBlock := chatMessage("user", json.RawMessage(`[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}},{"type":"text","text":"second"}]`), false)
	markedLastBlock := chatMessage("user", json.RawMessage(`[{"type":"text","text":"first"},{"type":"text","text":"second","cache_control":{"type":"ephemeral"}}]`), false)
	for _, tt := range []struct {
		name   string
		before []string
		after  []string
		rewind bool
	}{
		{"message marker removed", []string{chatMessage("user", "first", true), assistant, user("old"), assistant}, []string{user("first"), assistant, user("new")}, true},
		{"message marker added", []string{user("first"), assistant, user("old"), assistant}, []string{chatMessage("user", "first", true), assistant, user("new")}, true},
		{"block marker removed", []string{markedFirstBlock, assistant, user("old"), assistant}, []string{plainBlocks, assistant, user("new")}, true},
		{"block marker moved", []string{markedFirstBlock, assistant, user("old"), assistant}, []string{markedLastBlock, assistant, user("new")}, true},
		{"assistant marker moved", []string{user("first"), chatMessage("assistant", "reply", true), user("old"), assistant}, []string{chatMessage("user", "first", true), assistant, user("new")}, true},
		{"continuation ignores markers", []string{markedFirstBlock, assistant, user("old")}, []string{markedLastBlock, assistant, user("new"), assistant}, false},
		{"complete old prefix", []string{markedFirstBlock, assistant}, []string{markedLastBlock, assistant, user("next")}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, _, h := newTestManager(testConfig())
			m.Observe(intercept("old", "sess", chatMessages("old", nil, tt.before...)))
			m.Observe(intercept("new", "sess", chatMessages("new", nil, tt.after...)))
			if n := len(m.Status().Conversations); n != 1 {
				t.Fatalf("conversations %d, want only new key", n)
			}
			last := h.logs[len(h.logs)-1]
			wantLog := "conversation continued with other tools, system prompt or model, keep-alive stopped"
			if tt.rewind {
				wantLog = "conversation rewound and resent, keep-alive stopped"
			}
			if !strings.Contains(last, wantLog) || !strings.Contains(last, "stopped=replaced") {
				t.Fatalf("log %q, want %q and stopped=replaced", last, wantLog)
			}
		})
	}
}

func TestReplacementComparisonPreservesNestedCacheFields(t *testing.T) {
	before := gjson.Parse(`{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"run","input":{"cache_control":"old","number":9007199254740993}}]}`)
	for _, raw := range []string{
		`{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"run","input":{"cache_control":"new","number":9007199254740993}}]}`,
		`{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"run","input":{"cache_control":"old","number":9007199254740992}}]}`,
	} {
		if k := commonMessagePrefix([]gjson.Result{before}, []string{messageWithoutCacheControl(gjson.Parse(raw))}); k != 0 {
			t.Fatalf("different tool input matched: %s", raw)
		}
	}
}

func TestReplacementStopsOnlyItsSubagents(t *testing.T) {
	for _, change := range []string{"continued key", "rewound key"} {
		t.Run(change, func(t *testing.T) {
			m, clock, h := newTestManager(testConfig())
			observe := func(id, session, agent string, b []byte) string {
				m.Observe(withAgent(intercept(id, session, b), agent))
				return conversationKey("", session, agent, "claude-opus-5-5", b)
			}
			oldKey := observe("old", "sess", "", chat("main", nil, "a", "b", "c", "d"))
			// Equal firstSeen and lastRequest is outside the subagent window.
			equalKey := observe("equal", "sess", "equal", chat("worker", nil, "a"))
			clock.Advance(time.Second)
			childKey := observe("child", "sess", "child", chat("worker", nil, "a"))
			otherSessionKey := observe("other session", "other", "child", chat("worker", nil, "a"))
			helperKey := observe("helper", "sess", "", chat("helper", nil, "unrelated"))
			clock.Advance(time.Second)
			newBody := chat("new", nil, "a", "b", "c", "d", "e")
			if change == "rewound key" {
				newBody = chat("new", nil, "a", "edited")
			}
			newKey := observe("new", "sess", "", newBody)
			for _, key := range []string{equalKey, otherSessionKey, helperKey, newKey} {
				if _, ok := m.convs[key]; !ok {
					t.Fatalf("conversation %s was incorrectly removed", key)
				}
			}
			if _, ok := m.convs[childKey]; ok {
				t.Fatal("subagent of replaced conversation remains lit")
			}
			if _, ok := m.convs[oldKey]; ok {
				t.Fatal("old key remains lit")
			}
			if len(m.convs) != 4 {
				t.Fatalf("conversations %d, want 4", len(m.convs))
			}
			if logs := strings.Join(h.logs, "\n"); !strings.Contains(logs, "subagent finished, keep-alive stopped") || !strings.Contains(logs, "stopped=superseded") {
				t.Fatalf("subagent stop log missing: %s", logs)
			}
			clock.Advance(260 * time.Second)
			if h.pings() != 4 {
				t.Fatalf("pings %d, want only the 4 active keys", h.pings())
			}
		})
	}
}

func TestMultipleReplacementsStopSubagentsAcrossAllWindows(t *testing.T) {
	for _, earlier := range []string{"continued", "rewound"} {
		t.Run(earlier+" earlier", func(t *testing.T) {
			// Fresh maps exercise different traversal orders; the earlier
			// request's window must apply even if its key is visited last.
			for attempt := 0; attempt < 32; attempt++ {
				m, clock, h, rec := newRecordingManager(testConfig())
				clock.Advance(time.Minute)
				continuedAt, rewoundAt := 20, 40
				if earlier == "rewound" {
					continuedAt, rewoundAt = rewoundAt, continuedAt
				}
				fixtures := []struct {
					name, session, agent, scope string
					body                        []byte
					firstSeen, lastRequest      int
					stopped                     string
				}{
					{"continued", "sess", "", "caller-a", chat("continued", nil, "a", "b", "c"), continuedAt, continuedAt, stopReplaced},
					{"rewound", "sess", "", "caller-a", chat("rewound", nil, "a", "old", "c", "d", "e"), rewoundAt, rewoundAt, stopReplaced},
					{"before both", "sess", "before", "caller-a", chat("worker", nil, "a"), 10, 10, ""},
					{"equal earlier", "sess", "equal-earlier", "caller-a", chat("worker", nil, "a"), 20, 20, ""},
					{"between", "sess", "between", "caller-a", chat("worker", nil, "a"), 30, 30, stopSuperseded},
					{"equal later", "sess", "equal-later", "caller-a", chat("worker", nil, "a"), 40, 40, stopSuperseded},
					{"after both", "sess", "after", "caller-a", chat("worker", nil, "a"), 50, 50, stopSuperseded},
					{"other session child", "other", "after", "caller-a", chat("worker", nil, "a"), 50, 50, ""},
					{"unrelated main", "sess", "", "caller-a", chat("helper", nil, "unrelated"), 30, 30, ""},
					{"other session main", "other", "", "caller-a", chat("continued", nil, "a", "b", "c"), 50, 50, ""},
					{"other caller child", "sess", "after", "caller-b", chat("worker", nil, "a"), 50, 50, ""},
					{"other caller main", "sess", "", "caller-b", chat("continued", nil, "a", "b", "c"), 50, 50, ""},
				}
				wantActive := make(map[string]bool)
				wantStopped := make(map[string]string)
				keys := make(map[string]string)
				// Seed coexisting old keys, as a restored manager may have
				// them, without replacing one while constructing the fixture.
				for _, f := range fixtures {
					req, skip := parseRequest(withScope(withAgent(intercept(f.name, f.session, f.body), f.agent), f.scope), 0, false)
					if skip != "" {
						t.Fatalf("fixture %s skipped: %s", f.name, skip)
					}
					keys[f.name] = req.Key
					if f.stopped == "" {
						wantActive[req.Key] = true
					} else {
						wantStopped[req.Key] = f.stopped
					}
					m.mu.Lock()
					m.gen++
					lastRequest := start.Add(time.Duration(f.lastRequest) * time.Second)
					c := &conversation{req: req, firstSeen: start.Add(time.Duration(f.firstSeen) * time.Second), lastRequest: lastRequest, lastRead: lastRequest, gen: m.gen}
					m.convs[req.Key] = c
					m.scheduleLocked(req.Key, c, lastRequest)
					m.mu.Unlock()
				}
				newBody := chat("new", nil, "a", "b", "c", "d")
				newKey := conversationKey("caller-a", "sess", "", "claude-opus-5-5", newBody)
				wantActive[newKey] = true
				m.Observe(withScope(intercept("new", "sess", newBody), "caller-a"))
				if len(m.convs) != len(wantActive) {
					t.Fatalf("attempt %d: conversations %d, want %d", attempt, len(m.convs), len(wantActive))
				}
				for key := range m.convs {
					if !wantActive[key] {
						t.Fatalf("attempt %d: unexpected active key %s", attempt, key)
					}
				}
				for _, f := range fixtures {
					key := keys[f.name]
					stops := 0
					for _, log := range h.logs {
						if !strings.Contains(log, "key="+short(key)+" ") {
							continue
						}
						stops++
						if f.stopped == "" || !strings.Contains(log, "stopped="+f.stopped) {
							t.Fatalf("attempt %d: %s stop log %q, want %q", attempt, f.name, log, f.stopped)
						}
						message := "subagent finished, keep-alive stopped"
						if f.name == "continued" {
							message = "conversation continued with other tools, system prompt or model, keep-alive stopped"
						} else if f.name == "rewound" {
							message = "conversation rewound and resent, keep-alive stopped"
						}
						if !strings.Contains(log, message) {
							t.Fatalf("attempt %d: %s log %q, want %q", attempt, f.name, log, message)
						}
					}
					want := 0
					if _, ok := wantStopped[key]; ok {
						want = 1
					}
					if stops != want {
						t.Fatalf("attempt %d: %s stop logs %d, want %d", attempt, f.name, stops, want)
					}
				}
				if got := returns(rec); len(got) != 0 {
					t.Fatalf("attempt %d: replaced conversations recorded returns: %+v", attempt, got)
				}
				clock.Advance(260 * time.Second)
				if h.pings() != len(wantActive) {
					t.Fatalf("attempt %d: pings %d, want %d active keys only", attempt, h.pings(), len(wantActive))
				}
			}
		})
	}
}

func TestRewoundSubagentKeepsOtherAgents(t *testing.T) {
	m, clock, _ := newTestManager(testConfig())
	oldBody := chat("old", nil, "a", "b", "c", "d")
	newBody := chat("new", nil, "a", "edited")
	m.Observe(withAgent(intercept("old", "sess", oldBody), "a1"))
	clock.Advance(time.Second)
	m.Observe(withAgent(intercept("other", "sess", oldBody), "a2"))
	m.Observe(withAgent(intercept("other session", "other", oldBody), "a1"))
	m.Observe(intercept("main", "sess", oldBody))
	m.Observe(withAgent(intercept("new", "sess", newBody), "a1"))
	for _, key := range []string{
		conversationKey("", "sess", "a1", "claude-opus-5-5", newBody),
		conversationKey("", "sess", "a2", "claude-opus-5-5", oldBody),
		conversationKey("", "other", "a1", "claude-opus-5-5", oldBody),
		conversationKey("", "sess", "", "claude-opus-5-5", oldBody),
	} {
		if _, ok := m.convs[key]; !ok {
			t.Fatalf("expected conversation %s not found", key)
		}
	}
	if n := len(m.Status().Conversations); n != 4 {
		t.Fatalf("conversations %d, want main, both agents and the other session", n)
	}
}
