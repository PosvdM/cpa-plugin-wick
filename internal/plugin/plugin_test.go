package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/keepalive"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestMain(m *testing.M) {
	// The plugin fetches prices when registered; tests do not reach out.
	priceSource = "http://127.0.0.1:1/pricing.md"
	os.Exit(m.Run())
}

type nopHost struct{}

func (nopHost) ModelExecuteStream(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
	return pluginapi.HostModelStreamResponse{}, nil
}

func (nopHost) ModelStreamRead(string) (pluginapi.HostModelStreamReadResponse, error) {
	return pluginapi.HostModelStreamReadResponse{Done: true}, nil
}

func (nopHost) ModelStreamClose(string) {}

func (nopHost) Log(string, string, map[string]any) {}

func decode(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var env struct {
		OK     bool
		Result map[string]json.RawMessage
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope %s err %v", raw, err)
	}
	return env.Result
}

func TestRegisterDeclaresCapabilities(t *testing.T) {
	p := New(nopHost{}, "1.0.0", "missing.yaml", t.TempDir())
	result := decode(t, p.Handle("plugin.register", []byte(`{"config_yaml":"ZHVyYXRpb25fbWludXRlczogNDUK"}`)))
	var caps map[string]bool
	json.Unmarshal(result["capabilities"], &caps)
	if !caps["request_interceptor"] || !caps["request_lifecycle_plugin"] || !caps["usage_plugin"] || !caps["management_api"] {
		t.Fatalf("capabilities %v", caps)
	}
	if got := p.manager.Status().Config.DurationMinutes; got != 45 {
		t.Fatalf("duration_minutes %d, want 45 from config_yaml", got)
	}
}

// The host embeds RequestInterceptRequest, which has no JSON tags, next to
// host_callback_id.
func TestInterceptDecodesHostPayload(t *testing.T) {
	p := New(nopHost{}, "1.0.0", "missing.yaml", t.TempDir())
	p.Handle("plugin.register", []byte(`{"config_yaml":"bWluX2JvZHlfa2I6IDAK"}`))
	payload, _ := json.Marshal(struct {
		pluginapi.RequestInterceptRequest
		HostCallbackID string `json:"host_callback_id"`
	}{
		RequestInterceptRequest: pluginapi.RequestInterceptRequest{
			RequestID:    "r1",
			TraceID:      "t1",
			SourceFormat: "claude",
			Stream:       true,
			Headers: http.Header{
				"X-App":                    {"cli"},
				"User-Agent":               {"claude-cli/2.1.300 (external, cli)"},
				"X-Claude-Code-Session-Id": {"session-1"},
			},
			Body:     []byte(`{"model":"claude-opus-5-5","max_tokens":100,"messages":[]}`),
			Metadata: map[string]any{"selected_auth_id": "claude-a.json"},
		},
		HostCallbackID: "cb",
	})
	result := decode(t, p.Handle("request.intercept_after", payload))
	if len(result) != 0 {
		t.Fatalf("intercept response %v, want pass-through", result)
	}
	convs := p.manager.Status().Conversations
	if len(convs) != 1 || convs[0].Session != "session-1" || convs[0].AuthID != "claude-a.json" {
		t.Fatalf("conversations %+v", convs)
	}

	// The total counts the conversation's pings in the history, including
	// those of earlier rounds.
	for range 3 {
		p.history.Append(keepalive.Event{Time: time.Now().Add(-time.Hour), Key: convs[0].Key, Ping: 1})
	}
	p.history.Append(keepalive.Event{Time: time.Now(), Key: "other", Ping: 1})
	if got := p.status().Conversations[0]; got.TotalPings != 3 || got.DefaultMinutes != 30 {
		t.Fatalf("total %d duration %d, want 3 and 30", got.TotalPings, got.DefaultMinutes)
	}

	// The usage record, also without JSON tags, is written as a request.
	usage, _ := json.Marshal(pluginapi.UsageRecord{RequestID: "u1", TraceID: "t1", Detail: pluginapi.UsageDetail{CacheCreationTokens: 1234, InputTokens: 5}})
	decode(t, p.Handle("usage.handle", usage))
	events, _ := p.history.Query(time.Now().Add(-time.Minute))
	var got []keepalive.Event
	for _, ev := range events {
		if ev.Kind == keepalive.KindRequest {
			got = append(got, ev)
		}
	}
	if len(got) != 1 || got[0].Key != convs[0].Key || got[0].Usage == nil || got[0].Usage.CacheCreationTokens != 1234 {
		t.Fatalf("request events %+v", got)
	}
}

func manage(t *testing.T, p *Plugin, method, path, body string) pluginapi.ManagementResponse {
	t.Helper()
	payload, _ := json.Marshal(pluginapi.ManagementRequest{Method: method, Path: path, Body: []byte(body)})
	var env struct {
		OK     bool
		Result pluginapi.ManagementResponse
	}
	if err := json.Unmarshal(p.Handle("management.handle", payload), &env); err != nil || !env.OK {
		t.Fatalf("management %s %s: %+v err %v", method, path, env, err)
	}
	return env.Result
}

func TestManagementPageAndRoutes(t *testing.T) {
	p := New(nopHost{}, "1.0.0", "missing.yaml", t.TempDir())
	p.Handle("plugin.register", []byte(`{"config_yaml":"ZHVyYXRpb25fbWludXRlczogLTEK"}`))

	var reg struct {
		Routes    []route
		Resources []resource
	}
	result := decode(t, p.Handle("management.register", nil))
	json.Unmarshal(result["routes"], &reg.Routes)
	json.Unmarshal(result["resources"], &reg.Resources)
	if len(reg.Routes) != 5 || len(reg.Resources) != 1 || reg.Resources[0].Path != pagePath {
		t.Fatalf("registration %+v", reg)
	}

	page := manage(t, p, "GET", "/v0/resource/plugins/wick/page", "")
	if page.StatusCode != http.StatusOK || !strings.Contains(string(page.Body), "<title>Wick</title>") || strings.Contains(string(page.Body), "/*JS*/") {
		t.Fatalf("page %d %.200s", page.StatusCode, page.Body)
	}

	resp := manage(t, p, "GET", apiBase+"/status", "")
	var got struct {
		Version     string `json:"version"`
		ConfigError string `json:"config_error"`
		Config      struct {
			DurationMinutes int `json:"duration_minutes"`
		} `json:"config"`
	}
	json.Unmarshal(resp.Body, &got)
	if resp.StatusCode != http.StatusOK || got.Version != "1.0.0" || got.ConfigError == "" || got.Config.DurationMinutes != 30 {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Body)
	}

	if resp := manage(t, p, "POST", apiBase+"/stop", `{"key":"missing"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stop unknown %d", resp.StatusCode)
	}
	if resp := manage(t, p, "POST", apiBase+"/stop", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("stop without key %d", resp.StatusCode)
	}
	for body, want := range map[string]int{
		`{"key":"missing","minutes":120}`:  http.StatusNotFound,
		`{"key":"missing","minutes":1500}`: http.StatusBadRequest,
		`{"key":"missing","hours":2}`:      http.StatusBadRequest,
	} {
		if resp := manage(t, p, "POST", apiBase+"/duration", body); resp.StatusCode != want {
			t.Errorf("duration %s: %d, want %d", body, resp.StatusCode, want)
		}
	}
}

func TestHistoryRoute(t *testing.T) {
	p := New(nopHost{}, "1.0.0", "missing.yaml", t.TempDir())
	p.Handle("plugin.register", nil)
	now := time.Now()
	for i, key := range []string{"a", "b", "a", "c"} {
		p.history.Append(keepalive.Event{Time: now.Add(-time.Duration(i) * time.Hour), Key: key, Ping: 1, Usage: &keepalive.Usage{CacheReadTokens: 10}})
	}
	p.history.Append(keepalive.Event{Time: now.AddDate(0, 0, -3), Key: "d", Ping: 9})
	// A return is not a ping.
	p.history.Append(keepalive.Event{Time: now.Add(-time.Minute), Key: "a", Kind: keepalive.KindReturn, Returns: true, Pings: 1})
	// A request with usage is not a ping either.
	p.history.Append(keepalive.Event{Time: now.Add(-2 * time.Minute), Key: "a", Kind: keepalive.KindRequest, Usage: &keepalive.Usage{CacheReadTokens: 10}})
	get := func(path string, q url.Values, out any) int {
		payload, _ := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: apiBase + path, Query: q})
		var env struct{ Result pluginapi.ManagementResponse }
		json.Unmarshal(p.Handle("management.handle", payload), &env)
		json.Unmarshal(env.Result.Body, out)
		return env.Result.StatusCode
	}
	query := func(q url.Values) (int, historyResponse) {
		var got historyResponse
		return get("/history", q, &got), got
	}
	code, got := query(url.Values{"limit": {"2"}})
	if code != http.StatusOK || got.Range != "24h" || got.Count != 3 ||
		len(got.Conversations) != 2 || got.Conversations[0].Key != "a" || got.Conversations[0].Pings != 2 || got.Conversations[1].Key != "b" {
		t.Fatalf("24h %d %+v", code, got)
	}
	if _, got := query(url.Values{"range": {"7d"}}); got.Count != 4 {
		t.Fatalf("7d count %d", got.Count)
	}
	if _, got := query(url.Values{"range": {"1m"}}); got.Count != 4 {
		t.Fatalf("1m count %d", got.Count)
	}
	if code, _ := query(url.Values{"range": {"year"}}); code != http.StatusBadRequest {
		t.Fatalf("bad range %d", code)
	}
	var savings savingsResponse
	if code := get("/savings", url.Values{"range": {"7d"}}, &savings); code != http.StatusOK || savings.Range != "7d" ||
		savings.Totals.Pings != 5 || savings.Totals.CacheReadTokens != 40 || savings.Totals.Rounds != 5 {
		t.Fatalf("savings %d %+v", code, savings)
	}
	if code := get("/savings", url.Values{"range": {"year"}}, &savings); code != http.StatusBadRequest {
		t.Fatalf("savings bad range %d", code)
	}
}
