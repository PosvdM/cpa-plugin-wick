package keepalive

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/config"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func otherClientsConfig() config.Config {
	cfg := testConfig()
	cfg.OtherClients = true
	return cfg
}

func TestOtherClientsDisabledByDefault(t *testing.T) {
	cfg := testConfig()
	if cfg.OtherClients {
		t.Fatal("other clients enabled by default")
	}
	m, clock, h := newTestManager(cfg)
	in := otherRequest(body("pi", "1"))
	before := bytes.Clone(in.Body)
	if out := m.Observe(in); out != nil || len(m.Status().Conversations) != 0 || !bytes.Equal(in.Body, before) {
		t.Fatal("other client tracked or changed by default")
	}
	native := intercept("native", "native-session", body("native", "1"))
	if out := m.Observe(native); out != nil || len(m.Status().Conversations) != 1 {
		t.Fatal("native client not tracked unchanged with other clients disabled")
	}
	clock.Advance(260 * time.Second)
	if h.pings() != 1 || !bytes.Equal(h.requests[0].Body, native.Body) {
		t.Fatal("default config did not ping only the native client")
	}
}

func otherRequest(b []byte) pluginapi.RequestInterceptRequest {
	in := intercept("other", "", b)
	in.Headers.Del("X-App")
	in.Headers.Del(sessionHeader)
	in.Headers.Set("User-Agent", "pi (win32 10.0.26100; x64)")
	in.Headers.Set("X-Session-Affinity", "pi-session")
	return in
}

func TestPinCacheTTLExactBytes(t *testing.T) {
	// Deliberately use a different JSON field order than CPA's traversal.
	original := " \n" + `{ "messages": [{"role":"user","content":[{"type":"text","text":"hi \\u1234","cache_control": { "type" : "ephemeral" }}]}], "system":[{"cache_control":{"type":"ephemeral","scope":"global"}}], "tools":[{"cache_control":{"type":"ephemeral"}},{"cache_control":{"type":"ephemeral"}}], "cache_control":{"type":"ephemeral"}, "extra":{"cache_control":{"type":"ephemeral"}}, "number": 1.00 }` + "\n\t"
	input := []byte(original)
	want := strings.ReplaceAll(original, `{ "type" : "ephemeral" }`, `{ "type" : "ephemeral" ,"ttl":"5m"}`)
	want = strings.ReplaceAll(want, `{"type":"ephemeral","scope":"global"}`, `{"type":"ephemeral","scope":"global","ttl":"5m"}`)
	want = strings.Replace(want, `"tools":[{"cache_control":{"type":"ephemeral"}},{"cache_control":{"type":"ephemeral"}}]`, `"tools":[{"cache_control":{"type":"ephemeral","ttl":"5m"}},{"cache_control":{"type":"ephemeral","ttl":"5m"}}]`, 1)
	want = strings.Replace(want, `"number": 1.00 }`, `"number": 1.00 ,"prompt_cache_options":{"mode":"explicit"}}`, 1)
	got, reason := pinCacheTTL(input)
	if reason != "" || string(got) != want || !gjson.ValidBytes(got) {
		t.Fatalf("reason %s\n got %s\nwant %s", reason, got, want)
	}
	again, reason := pinCacheTTL(got)
	if reason != skipCachePolicy || !bytes.Equal(again, got) {
		t.Fatal("already-pinned request must pass through unchanged")
	}
	if string(input) != original {
		t.Fatal("input was mutated")
	}
}

func TestPinCacheTTLOnlyLegalPositions(t *testing.T) {
	for _, text := range []string{
		`{"cache_control":{"type":"ephemeral"}}`,
		`{"tools":{"cache_control":{"type":"ephemeral"}}}`,
		`{"system":{"cache_control":{"type":"ephemeral"}}}`,
		`{"messages":[{"cache_control":{"type":"ephemeral"},"content":{"cache_control":{"type":"ephemeral"}}}]}`,
		`{"tools":[{"cache_control":null},{"cache_control":false},{"cache_control":{}},{"cache_control":{"type":"other"}}]}`,
		`{"messages":[{"content":[{"tool_result":{"cache_control":{"type":"ephemeral"}}}]}]}`,
	} {
		got, reason := pinCacheTTL([]byte(text))
		if reason != skipNoCache || string(got) != text {
			t.Fatalf("illegal position changed: %s", got)
		}
	}
	input := []byte(`{"system":[{"cache_control":{"type":"ephemeral","ttl":null}},{"cache_control":{"type":"ephemeral","ttl":"custom"}}]}`)
	if got, reason := pinCacheTTL(input); reason != skipCachePolicy || !bytes.Equal(got, input) {
		t.Fatalf("explicit TTL changed: %s", got)
	}
}

func TestPinCacheTTLOptions(t *testing.T) {
	for _, tc := range []struct{ name, options, want string }{
		{"absent", "", `,"prompt_cache_options":{"mode":"explicit"}`},
		{"empty", `,"prompt_cache_options":{ }`, `,"prompt_cache_options":{ "mode":"explicit"}`},
		{"other-fields", `,"prompt_cache_options":{ "custom" : [1.00, true] }`, `,"prompt_cache_options":{ "custom" : [1.00, true] ,"mode":"explicit"}`},
		{"explicit", `,"prompt_cache_options":{ "mode" : "explicit", "custom":1.00 }`, `,"prompt_cache_options":{ "mode" : "explicit", "custom":1.00 }`},
		{"case-insensitive", `,"prompt_cache_options":{"mode":" Explicit "}`, `,"prompt_cache_options":{"mode":" Explicit "}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := `{"system":[{"cache_control":{"type":"ephemeral"}}]`
			input := []byte(prefix + tc.options + `}`)
			before := bytes.Clone(input)
			want := strings.Replace(prefix, `"ephemeral"`, `"ephemeral","ttl":"5m"`, 1) + tc.want + `}`
			got, reason := pinCacheTTL(input)
			if reason != "" || string(got) != want || !gjson.ValidBytes(got) || !bytes.Equal(input, before) {
				t.Fatalf("reason %s\n got %s\nwant %s", reason, got, want)
			}
		})
	}
}

func TestPinCacheTTLOptionsBeforeMarkers(t *testing.T) {
	original := ` { "prompt_cache_options":{ "custom":{"value":1.00} }, "system":[{"cache_control":{"type":"ephemeral"}}]} ` + "\n"
	want := strings.Replace(original, `"value":1.00} }`, `"value":1.00} ,"mode":"explicit"}`, 1)
	want = strings.Replace(want, `"type":"ephemeral"`, `"type":"ephemeral","ttl":"5m"`, 1)
	got, reason := pinCacheTTL([]byte(original))
	if reason != "" || string(got) != want || !gjson.ValidBytes(got) {
		t.Fatalf("reason %s\n got %s\nwant %s", reason, got, want)
	}
}

func TestOtherClientsCachePolicy(t *testing.T) {
	marker := `{"type":"text","text":"system","cache_control":{"type":"ephemeral"}}`
	for _, tc := range []struct {
		name, markers, options string
		accept                 bool
	}{
		{"one", marker, "", true},
		{"two", marker + "," + marker, "", true},
		{"three", strings.Repeat(marker+",", 2) + marker, "", true},
		{"four", strings.Repeat(marker+",", 3) + marker, "", true},
		{"five", strings.Repeat(marker+",", 4) + marker, "", false},
		{"none", "", "", false},
		{"5m", strings.Replace(marker, `"ephemeral"`, `"ephemeral","ttl":"5m"`, 1), "", false},
		{"1h", strings.Replace(marker, `"ephemeral"`, `"ephemeral","ttl":"1h"`, 1), "", false},
		{"mixed", marker + "," + strings.Replace(marker, `"ephemeral"`, `"ephemeral","ttl":"5m"`, 1), "", false},
		{"null-ttl", strings.Replace(marker, `"ephemeral"`, `"ephemeral","ttl":null`, 1), "", false},
		{"empty-ttl", strings.Replace(marker, `"ephemeral"`, `"ephemeral","ttl":""`, 1), "", false},
		{"custom-ttl", strings.Replace(marker, `"ephemeral"`, `"ephemeral","ttl":"custom"`, 1), "", false},
		{"false-ttl", strings.Replace(marker, `"ephemeral"`, `"ephemeral","ttl":false`, 1), "", false},
		{"existing-explicit", marker, `,"prompt_cache_options":{"mode":"explicit","custom":true}`, true},
		{"existing-object", marker, `,"prompt_cache_options":{"custom":true}`, true},
		{"auto-mode", marker, `,"prompt_cache_options":{"mode":"auto"}`, false},
		{"empty-mode", marker, `,"prompt_cache_options":{"mode":""}`, false},
		{"null-mode", marker, `,"prompt_cache_options":{"mode":null}`, false},
		{"false-mode", marker, `,"prompt_cache_options":{"mode":false}`, false},
		{"null-options", marker, `,"prompt_cache_options":null`, false},
		{"array-options", marker, `,"prompt_cache_options":[]`, false},
		{"string-options", marker, `,"prompt_cache_options":"explicit"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := otherRequest([]byte(`{"system":[` + tc.markers + `],"messages":[{"role":"user","content":"hello"}]` + tc.options + `}`))
			before := bytes.Clone(in.Body)
			m, clock, h := newTestManager(otherClientsConfig())
			out := m.Observe(in)
			if !bytes.Equal(before, in.Body) {
				t.Fatal("input mutated")
			}
			if tc.accept {
				if len(m.Status().Conversations) != 1 || gjson.GetBytes(out, "prompt_cache_options.mode").String() != "explicit" {
					t.Fatalf("not taken over: %s", out)
				}
				forEachCacheBlock(out, func(block gjson.Result) {
					if block.Get("cache_control.ttl").String() != "5m" {
						t.Fatalf("marker not pinned: %s", block.Raw)
					}
				})
				clock.Advance(260 * time.Second)
				if h.pings() != 1 || !bytes.Equal(h.requests[0].Body, out) {
					t.Fatal("replay differs from intercept Body")
				}
			} else {
				if out != nil || len(m.Status().Conversations) != 0 {
					t.Fatalf("ineligible request taken over: %s", out)
				}
				clock.Advance(260 * time.Second)
				if h.pings() != 0 {
					t.Fatal("ineligible request pinged")
				}
			}
		})
	}
}

func TestNativeCachePolicyUnchanged(t *testing.T) {
	in := intercept("native", "session", []byte(`{"system":[{"cache_control":{"type":"ephemeral","ttl":"5m"}}],"prompt_cache_options":{"mode":"auto"},"messages":[{"role":"user","content":"hello"}]}`))
	before := bytes.Clone(in.Body)
	m, clock, h := newTestManager(testConfig())
	if out := m.Observe(in); out != nil || len(m.Status().Conversations) != 1 {
		t.Fatalf("native request not tracked unchanged: %s", out)
	}
	clock.Advance(260 * time.Second)
	if h.pings() != 1 || !bytes.Equal(h.requests[0].Body, before) || !bytes.Equal(in.Body, before) {
		t.Fatal("native cache policy changed")
	}
}

func TestOtherClientsEligibilityAndReplay(t *testing.T) {
	in := otherRequest(body("main", "1"))
	req, skip := parseRequest(in, 0, true)
	if skip != "" || !req.Other || req.Client != "pi" || gjson.GetBytes(req.Body, "system.1.cache_control.ttl").String() != "5m" || gjson.GetBytes(req.Body, "prompt_cache_options.mode").String() != "explicit" {
		t.Fatalf("parsed %+v skip %s", req, skip)
	}
	m, clock, h := newTestManager(otherClientsConfig())
	out := m.Observe(in)
	if len(out) == 0 || bytes.Equal(out, in.Body) || !bytes.Equal(out, req.Body) {
		t.Fatalf("outgoing body %s", out)
	}
	// Every retry of the original body returns identical bytes.
	if retry := m.Observe(in); !bytes.Equal(retry, out) {
		t.Fatal("retry differs")
	}
	clock.Advance(260 * time.Second)
	if h.pings() != 1 || !bytes.Equal(h.requests[0].Body, out) {
		t.Fatal("replay does not match outgoing request")
	}
	saved := encodeRequest(req)
	restored, err := decodeRequest(saved)
	if err != nil || !restored.Other || !bytes.Equal(restored.Body, out) {
		t.Fatalf("persisted request differs: %+v %v", restored, err)
	}
	native := intercept("native", "native-session", body("main", "1"))
	if out := m.Observe(native); out != nil {
		t.Fatal("native request returned a Body")
	}
}

func TestOtherClientsSkipWithoutRewrite(t *testing.T) {
	for _, name := range []string{"off", "five", "zero", "agent-five", "small", "no-auth", "no-session", "1h", "no-cache", "openai", "not-stream", "bad-body", "stopped"} {
		t.Run(name, func(t *testing.T) {
			cfg := otherClientsConfig()
			in := otherRequest(body("main", "1"))
			switch name {
			case "off":
				cfg.OtherClients = false
			case "five":
				cfg.DurationMinutes = 5
			case "zero":
				cfg.DurationMinutes = 0
			case "agent-five":
				cfg.SubagentDurationMinutes = 5
				in.Headers.Set(agentHeader, "agent")
			case "small":
				cfg.MinBodyKB = 64
			case "no-auth":
				in.Metadata = nil
			case "no-session":
				in.Headers.Del("X-Session-Affinity")
				in.Body = bytes.Replace(in.Body, []byte(`,"metadata":{"user_id":"u"}`), nil, 1)
			case "1h":
				in.Body = bytes.ReplaceAll(in.Body, []byte(`"type":"ephemeral"`), []byte(`"type":"ephemeral","ttl":"1h"`))
			case "no-cache":
				in.Body = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
			case "openai":
				in.SourceFormat = "openai"
			case "not-stream":
				in.Stream = false
			case "bad-body":
				in.Body = []byte(`{`)
			}
			m, _, _ := newTestManager(cfg)
			if name == "stopped" {
				m.Stop()
			}
			before := bytes.Clone(in.Body)
			if out := m.Observe(in); out != nil || len(m.Status().Conversations) != 0 || !bytes.Equal(before, in.Body) {
				t.Fatalf("skipped request tracked or changed: %s", out)
			}
			if name == "no-cache" {
				if _, reason := parseRequest(in, 0, true); reason != skipNoCache {
					t.Fatalf("reason %s", reason)
				}
			}
		})
	}
}

func TestRequestSessionPrecedence(t *testing.T) {
	in := otherRequest([]byte(`{"metadata":{"user_id":"{\"session_id\":\"meta-session\"}"},"messages":[{"role":"user","content":"first"}]}`))
	in.Headers.Set(sessionHeader, "cc")
	in.Headers.Set("X-Session-Id", "x-session")
	in.Headers.Set("Session-Id", "session")
	for _, tc := range []struct{ remove, want string }{
		{"", "cc"}, {sessionHeader, "meta-session"}, {"metadata", "session"}, {"Session-Id", "x-session"}, {"X-Session-Id", "pi-session"},
	} {
		if tc.remove == "metadata" {
			in.Body = bytes.Replace(in.Body, []byte(`"{\"session_id\":\"meta-session\"}"`), []byte(`"meta"`), 1)
		} else {
			in.Headers.Del(tc.remove)
		}
		if got := requestSession(in); got != tc.want {
			t.Fatalf("after %s: %s", tc.remove, got)
		}
	}
	in.Headers.Del("X-Session-Affinity")
	if got := requestSession(in); got != "meta" {
		t.Fatalf("plain metadata should follow generic headers: %s", got)
	}
	in.Body = []byte(`{"messages":[]}`)
	if got := requestSession(in); got != "" {
		t.Fatalf("request without explicit session: %s", got)
	}
}

func TestNormalizeExplicitSession(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"empty", "", ""},
		{"spaces", "   ", ""},
		{"trim", "  session \u00a0", "session"},
		{"256 bytes", strings.Repeat("x", 256), strings.Repeat("x", 256)},
		{"257 bytes", strings.Repeat("x", 257), ""},
		{"multibyte boundary", strings.Repeat("界", 85) + "x", strings.Repeat("界", 85) + "x"},
		{"multibyte oversized", strings.Repeat("界", 86), ""},
		{"leading control", "\tsession", ""},
		{"trailing control", "session\n", ""},
		{"embedded control", "ses\x00sion", ""},
		{"delete", "session\x7f", ""},
		{"unicode control", "session\u0085", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeExplicitSession(tc.raw); got != tc.want {
				t.Fatalf("normalizeExplicitSession(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestOtherClientsSessionValidation(t *testing.T) {
	long := strings.Repeat("x", 257)
	structured := func(session string) string {
		t.Helper()
		encoded, err := json.Marshal(map[string]string{"session_id": session, "account_uuid": long})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	type sessionTest struct {
		name, userID, want string
		headers            http.Header
		native             bool
	}
	tests := []sessionTest{
		{name: "structured valid", userID: structured(" stable-session "), want: "stable-session"},
		{name: "structured oversized", userID: structured(long)},
		{name: "structured control", userID: structured("\tbad-session")},
		{name: "structured missing", userID: `{"account_uuid":"` + long + `"}`},
		{name: "structured invalid then generic", userID: structured("bad\n"), headers: http.Header{"X-Session-Affinity": {"fallback"}}, want: "fallback"},
		{name: "structured before generic", userID: structured("metadata"), headers: http.Header{"Session-Id": {"generic"}}, want: "metadata"},
		// CPA tries the opaque container last if extraction fails and it is valid.
		{name: "structured opaque fallback", userID: `{"session_id":""}`, want: `{"session_id":""}`},
		{name: "structured invalid short then generic", userID: `{"session_id":""}`, headers: http.Header{"X-Session-Id": {"fallback"}}, want: "fallback"},
		{name: "legacy valid large container", userID: "user_" + long + "_session_a1-b2", want: "a1-b2"},
		{name: "legacy oversized session", userID: "user_account_session_" + strings.Repeat("a", 257)},
		{name: "invalid header then structured", headers: http.Header{sessionHeader: {long}}, userID: structured("metadata"), want: "metadata"},
		{name: "invalid header then later header", headers: http.Header{"Session-Id": {long}, "X-Session-Id": {"bad\n"}, "X-Session-Affinity": {"fallback"}}, want: "fallback"},
		{name: "later valid header value", headers: http.Header{"X-Session-Affinity": {"\tbad", long, " fallback "}}, want: "fallback"},
		{name: "case insensitive header", headers: http.Header{"x-session-id": {"bad\n", "fallback"}}, want: "fallback"},
		{name: "plain metadata last", userID: "metadata", headers: http.Header{"X-Session-Affinity": {"header"}}, want: "header"},
		{name: "native header wins", native: true, headers: http.Header{sessionHeader: {"native-session"}}, userID: structured(long), want: "native-session"},
	}
	for _, source := range []string{sessionHeader, "Session-Id", "X-Session-Id", "X-Session-Affinity", "metadata.user_id"} {
		for _, invalid := range []struct{ name, value string }{{"oversized", long}, {"control", "\tbad-session"}, {"unicode control", "session\u0085"}} {
			tc := sessionTest{name: source + " " + invalid.name, headers: make(http.Header)}
			if source == "metadata.user_id" {
				tc.userID = invalid.value
			} else {
				tc.headers.Set(source, invalid.value)
			}
			tests = append(tests, tc)
		}
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := otherRequest([]byte(`{"system":[{"text":"system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"}]}`))
			in.Headers = tc.headers.Clone()
			if in.Headers == nil {
				in.Headers = make(http.Header)
			}
			if tc.native {
				in.Headers.Set("X-App", "cli")
				in.Headers.Set("User-Agent", "claude-cli/1.0")
			} else {
				in.Headers.Set("User-Agent", "pi")
			}
			encoded, err := json.Marshal(tc.userID)
			if err != nil {
				t.Fatal(err)
			}
			in.Body = bytes.Replace(in.Body, []byte(`{"system"`), []byte(`{"metadata":{"user_id":`+string(encoded)+`},"system"`), 1)
			before := bytes.Clone(in.Body)
			if got := requestSession(in); got != tc.want {
				t.Fatalf("session = %q, want %q", got, tc.want)
			}
			req, reason := parseRequest(in, 0, true)
			m, clock, h := newTestManager(otherClientsConfig())
			out := m.Observe(in)
			if !bytes.Equal(in.Body, before) {
				t.Fatal("input body changed")
			}
			if tc.want == "" {
				if reason != skipNoSession || out != nil || len(m.Status().Conversations) != 0 {
					t.Fatalf("invalid session taken over: reason=%s body=%s", reason, out)
				}
				clock.Advance(time.Hour)
				if h.pings() != 0 {
					t.Fatal("invalid session pinged")
				}
				return
			}
			if reason != "" || req.Session != tc.want || len(m.Status().Conversations) != 1 {
				t.Fatalf("valid session not tracked: reason=%s session=%q", reason, req.Session)
			}
			expected := out
			if tc.native {
				if out != nil || !bytes.Equal(req.Body, before) {
					t.Fatal("native request body changed")
				}
				expected = before
			} else if len(out) == 0 {
				t.Fatal("valid other-client request not rewritten")
			}
			clock.Advance(260 * time.Second)
			if h.pings() != 1 || !bytes.Equal(h.requests[0].Body, expected) {
				t.Fatal("replay differs")
			}
		})
	}
}

func TestOtherClientsRequireExplicitSession(t *testing.T) {
	for _, source := range []string{"none", sessionHeader, "metadata.user_id", "X-Session-Affinity", "X-Session-Id", "Session-Id", "blank"} {
		t.Run(source, func(t *testing.T) {
			in := otherRequest([]byte(`{"system":[{"text":"system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"}]}`))
			in.Headers.Del("X-Session-Affinity")
			switch source {
			case "none":
			case "blank":
				for _, header := range []string{sessionHeader, "X-Session-Affinity", "X-Session-Id", "Session-Id"} {
					in.Headers.Set(header, " \t ")
				}
				in.Body = bytes.Replace(in.Body, []byte(`{"system"`), []byte(`{"metadata":{"user_id":" "},"system"`), 1)
			case "metadata.user_id":
				in.Body = bytes.Replace(in.Body, []byte(`{"system"`), []byte(`{"metadata":{"user_id":"stable-session"},"system"`), 1)
			default:
				in.Headers.Set(source, "stable-session")
			}
			before := bytes.Clone(in.Body)
			req, reason := parseRequest(in, 0, true)
			m, clock, h := newTestManager(otherClientsConfig())
			out := m.Observe(in)
			if !bytes.Equal(in.Body, before) {
				t.Fatal("input body changed")
			}
			if source == "none" || source == "blank" {
				if reason != skipNoSession || out != nil || len(m.Status().Conversations) != 0 {
					t.Fatalf("request without explicit session taken over: reason=%s body=%s", reason, out)
				}
				clock.Advance(time.Hour)
				if h.pings() != 0 {
					t.Fatal("request without explicit session pinged")
				}
				return
			}
			if reason != "" || req.Session != "stable-session" || len(out) == 0 || len(m.Status().Conversations) != 1 {
				t.Fatalf("explicit session not taken over: reason=%s request=%+v body=%s", reason, req, out)
			}
			clock.Advance(260 * time.Second)
			if h.pings() != 1 || !bytes.Equal(h.requests[0].Body, out) {
				t.Fatal("explicit session replay differs")
			}
			if source == "metadata.user_id" {
				if got := gjson.GetBytes(h.requests[0].Body, source).String(); got != "stable-session" {
					t.Fatalf("replay metadata session: %s", got)
				}
			} else if got := h.requests[0].Headers.Get(source); got != "stable-session" {
				t.Fatalf("replay session header: %s", got)
			}
		})
	}
}

func TestOtherClientsDisableAndRestore(t *testing.T) {
	cfg := otherClientsConfig()
	m, _, _ := newTestManager(cfg)
	m.Observe(otherRequest(body("pi", "1")))
	m.Observe(intercept("cc", "cc-session", body("cc", "1")))
	dir := t.TempDir()
	s := newConvStore(m, dir)
	if err := s.write(m.snapshotLocked()); err != nil {
		t.Fatal(err)
	}
	cfg.OtherClients = false
	m.Configure(cfg)
	if convs := m.Status().Conversations; len(convs) != 1 || convs[0].Client != "cli" {
		t.Fatalf("disable: %+v", convs)
	}
	restored, _, _ := newTestManager(cfg)
	restored.SetStore(dir)
	defer restored.Stop()
	if convs := restored.Status().Conversations; len(convs) != 1 || convs[0].Client != "cli" {
		t.Fatalf("restore: %+v", convs)
	}
	// Native requests omit the false other flag and remain eligible.
	for _, c := range restored.convs {
		file, err := os.ReadFile(filepath.Join(dir, c.req.Key+requestExt))
		if err != nil || bytes.Contains(file, []byte(`"other":true`)) {
			t.Fatal("native persistence flag")
		}
	}
}

func TestOtherClientExplicitHourStopsPinnedIdentity(t *testing.T) {
	for _, section := range []string{"tools", "system"} {
		for _, prior := range []string{`{"type":"ephemeral"}`, `{ "type" : "ephemeral" }`} {
			for _, next := range []string{`{"type":"ephemeral","ttl":"1h"}`, `{"ttl":"1h","type":"ephemeral"}`, `{ "ttl" : "1h", "type" : "ephemeral" }`} {
				t.Run(section+"/"+prior+"/"+next, func(t *testing.T) {
					m, clock, h := newTestManager(otherClientsConfig())
					tool := `{"name":"Bash","cache_control":{"type":"ephemeral"}}`
					system := `{"type":"text","text":"main","cache_control":{"type":"ephemeral"}}`
					block := system
					nextBlock := `{ "cache_control" : ` + next + `, "text" : "main", "type" : "text" }`
					if section == "tools" {
						block = tool
						nextBlock = `{ "cache_control" : ` + next + `, "name" : "Bash" }`
					}
					priorBlock := strings.Replace(block, `{"type":"ephemeral"}`, prior, 1)
					original := []byte(`{"model":"claude-opus-5-5","tools":[` + tool + `],"system":[` + system + `],"messages":[{"role":"user","content":"hello"}]}`)
					original = bytes.Replace(original, []byte(block), []byte(priorBlock), 1)
					in := otherRequest(original)
					m.Observe(in)
					key := m.Status().Conversations[0].Key
					// Different tools/system, session, agent and model must not be retired.
					for _, dimension := range []string{"prompt", "tools", "session", "agent", "model", "native", "marker-scope"} {
						other := otherRequest(bytes.Clone(original))
						other.RequestID = dimension
						switch dimension {
						case "prompt":
							other.Body = bytes.Replace(other.Body, []byte(`"text":"main"`), []byte(`"text":"unrelated"`), 1)
						case "tools":
							other.Body = bytes.Replace(other.Body, []byte(`"name":"Bash"`), []byte(`"name":"Read"`), 1)
						case "session":
							other.Headers.Set("X-Session-Affinity", "unrelated")
						case "agent":
							other.Headers.Set(agentHeader, "unrelated")
						case "model":
							other.RequestedModel = "claude-sonnet-5-5"
						case "native":
							other = intercept("native", "pi-session", bytes.Clone(original))
						case "marker-scope":
							other.Body = bytes.Replace(other.Body, []byte(`"cache_control":{`), []byte(`"cache_control":{"scope":"global",`), 1)
						}
						m.Observe(other)
					}
					if len(m.convs) != 8 {
						t.Fatalf("setup: got %d conversations, want 8", len(m.convs))
					}
					stored := make(map[string][]byte)
					for key, c := range m.convs {
						stored[key] = bytes.Clone(c.req.Body)
					}
					in.Body = bytes.Replace(original, []byte(priorBlock), []byte(nextBlock), 1)
					before := bytes.Clone(in.Body)
					if out := m.Observe(in); out != nil || !bytes.Equal(in.Body, before) {
						t.Fatal("1h request changed")
					}
					if len(m.convs) != 7 {
						t.Fatalf("got %d surviving conversations, want 7", len(m.convs))
					}
					for key, c := range m.convs {
						if !bytes.Equal(c.req.Body, stored[key]) {
							t.Fatal("identity lookup changed stored body")
						}
					}
					for _, c := range m.Status().Conversations {
						if c.Key == key {
							t.Fatal("pinned identity still tracked")
						}
					}
					clock.Advance(260 * time.Second)
					if h.pings() != 7 {
						t.Fatalf("pings %d want 7 unrelated conversations", h.pings())
					}
				})
			}
		}
	}
}

func TestCacheIdentityKeyNormalizationScope(t *testing.T) {
	for _, tc := range []struct {
		name, left, right string
		equal             bool
	}{
		{"legal-marker", `{"tools":[{"cache_control":{"type":"ephemeral","ttl":"5m"}}]}`, `{"tools":[{"cache_control":{ "ttl":"1h", "type":"ephemeral" }}]}`, true},
		{"missing-ttl", `{"system":[{"cache_control":{"type":"ephemeral"}}]}`, `{"system":[{"cache_control":{"ttl":"1h","type":"ephemeral"}}]}`, true},
		{"nested-marker", `{"tools":[{"input_schema":{"cache_control":{"type":"ephemeral","ttl":"5m"}}}]}`, `{"tools":[{"input_schema":{"cache_control":{"type":"ephemeral","ttl":"1h"}}}]}`, false},
		{"non-array", `{"system":{"cache_control":{"type":"ephemeral","ttl":"5m"}}}`, `{"system":{"cache_control":{"type":"ephemeral","ttl":"1h"}}}`, false},
		{"non-ephemeral", `{"tools":[{"cache_control":{"type":"other","ttl":"5m"}}]}`, `{"tools":[{"cache_control":{"type":"other","ttl":"1h"}}]}`, false},
		{"null-ttl", `{"system":[{"cache_control":{"type":"ephemeral","ttl":null}}]}`, `{"system":[{"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`, false},
		{"custom-ttl", `{"system":[{"cache_control":{"type":"ephemeral","ttl":"custom"}}]}`, `{"system":[{"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`, false},
		{"number-precision", `{"tools":[{"limit":9007199254740992}]}`, `{"tools":[{"limit":9007199254740993}]}`, false},
		{"number-form", `{"tools":[{"limit":1.00}]}`, `{"tools":[{"limit":1}]}`, false},
		{"array-order", `{"system":[{"text":"a"},{"text":"b"}]}`, `{"system":[{"text":"b"},{"text":"a"}]}`, false},
		{"billing", `{"system":[{"text":"x-anthropic-billing-header: a"},{"text":"main"}]}`, `{"system":[{"text":"x-anthropic-billing-header: b"},{"text":"main"}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := []byte(tc.left), []byte(tc.right)
			a, b := cacheIdentityKey(left), cacheIdentityKey(right)
			if a == "" || b == "" || (a == b) != tc.equal {
				t.Fatalf("identity equality = %t, want %t", a == b, tc.equal)
			}
			if string(left) != tc.left || string(right) != tc.right {
				t.Fatal("lookup changed source bytes")
			}
		})
	}
}

func TestNativeExplicitHourKeepsRawKeyBehavior(t *testing.T) {
	m, _, _ := newTestManager(testConfig())
	in := intercept("native", "native-session", body("main", "1"))
	m.Observe(in)
	key := m.Status().Conversations[0].Key
	in.Body = bytes.ReplaceAll(in.Body, []byte(`"type":"ephemeral"`), []byte(`"ttl":"1h","type":"ephemeral"`))
	if out := m.Observe(in); out != nil {
		t.Fatal("native 1h request returned a body")
	}
	if convs := m.Status().Conversations; len(convs) != 1 || convs[0].Key != key {
		t.Fatal("native key lookup was normalized")
	}
	// A marker change confined to messages leaves the native raw key intact.
	in.Body = bytes.Replace(body("main", "1"), []byte(`"content":"1"`), []byte(`"content":[{"type":"text","text":"1","cache_control":{"type":"ephemeral","ttl":"1h"}}]`), 1)
	if out := m.Observe(in); out != nil || len(m.Status().Conversations) != 0 {
		t.Fatal("native exact-key 1h lookup did not retire the conversation")
	}
}

func TestOtherClientNewRequestResetsCustomDuration(t *testing.T) {
	cfg := otherClientsConfig()
	m, _, _ := newTestManager(cfg)
	in := otherRequest(body("pi", "1"))
	m.Observe(in)
	key := m.Status().Conversations[0].Key
	if err := m.SetDuration(key, 120); err != nil {
		t.Fatal(err)
	}
	cfg.DurationMinutes = 5
	m.Configure(cfg)
	if len(m.Status().Conversations) != 1 {
		t.Fatal("custom duration was not preserved until the next request")
	}
	if out := m.Observe(in); out != nil || len(m.Status().Conversations) != 0 {
		t.Fatal("new request did not reset the custom duration without rewriting")
	}
}

func TestClientEntrypointOtherProducts(t *testing.T) {
	for ua, want := range map[string]string{"pi/0.60 node/24": "pi", "pi (win32 10.0.26100; x64)": "pi", "Other/1.0 (a,b)": "Other", "": "", "claude-cli/2 (external, claude-vscode)": "claude-vscode"} {
		if got := clientEntrypoint(ua); got != want {
			t.Errorf("%s: %s want %s", ua, got, want)
		}
	}
}
