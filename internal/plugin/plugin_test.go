package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/keepalive"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

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
	result := decode(t, p.Handle("plugin.register", []byte(`{"config_yaml":"bWF4X3BpbmdzOiA0Cg=="}`)))
	var caps map[string]bool
	json.Unmarshal(result["capabilities"], &caps)
	if !caps["request_interceptor"] || !caps["request_lifecycle_plugin"] || !caps["management_api"] {
		t.Fatalf("capabilities %v", caps)
	}
	if got := p.manager.Status().Config.MaxPings; got != 4 {
		t.Fatalf("max_pings %d, want 4 from config_yaml", got)
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
	if got := p.status().Conversations[0]; got.TotalPings != 3 || got.MaxPings != 6 {
		t.Fatalf("total %d limit %d, want 3 and 6", got.TotalPings, got.MaxPings)
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
	p.Handle("plugin.register", []byte(`{"config_yaml":"bWF4X3BpbmdzOiAtMQo="}`))

	var reg struct {
		Routes    []route
		Resources []resource
	}
	result := decode(t, p.Handle("management.register", nil))
	json.Unmarshal(result["routes"], &reg.Routes)
	json.Unmarshal(result["resources"], &reg.Resources)
	if len(reg.Routes) != 4 || len(reg.Resources) != 1 || reg.Resources[0].Path != pagePath {
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
			MaxPings int `json:"max_pings"`
		} `json:"config"`
	}
	json.Unmarshal(resp.Body, &got)
	if resp.StatusCode != http.StatusOK || got.Version != "1.0.0" || got.ConfigError == "" || got.Config.MaxPings != 6 {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Body)
	}

	if resp := manage(t, p, "POST", apiBase+"/stop", `{"key":"missing"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stop unknown %d", resp.StatusCode)
	}
	if resp := manage(t, p, "POST", apiBase+"/stop", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("stop without key %d", resp.StatusCode)
	}
	for body, want := range map[string]int{
		`{"key":"missing","hours":2}`:  http.StatusNotFound,
		`{"key":"missing","hours":30}`: http.StatusBadRequest,
		`{"key":"missing"}`:            http.StatusBadRequest,
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
	query := func(q url.Values) (int, historyResponse) {
		payload, _ := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: apiBase + "/history", Query: q})
		var env struct{ Result pluginapi.ManagementResponse }
		json.Unmarshal(p.Handle("management.handle", payload), &env)
		var got historyResponse
		json.Unmarshal(env.Result.Body, &got)
		return env.Result.StatusCode, got
	}
	code, got := query(url.Values{"limit": {"2"}})
	if code != http.StatusOK || got.Range != "24h" || got.Count != 3 || got.Totals.Pings != 4 || got.Totals.CacheReadTokens != 40 ||
		len(got.Conversations) != 2 || got.Conversations[0].Key != "a" || got.Conversations[0].Pings != 2 || got.Conversations[1].Key != "b" {
		t.Fatalf("24h %d %+v", code, got)
	}
	if _, got := query(url.Values{"range": {"7d"}}); got.Count != 4 {
		t.Fatalf("7d count %d", got.Count)
	}
	if code, _ := query(url.Values{"range": {"year"}}); code != http.StatusBadRequest {
		t.Fatalf("bad range %d", code)
	}
}
