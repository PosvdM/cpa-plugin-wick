package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
	p := New(nopHost{}, "1.0.0", "missing.yaml")
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
	p := New(nopHost{}, "1.0.0", "missing.yaml")
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
	if len(convs) != 1 || convs[0].Session != "session-" || convs[0].AuthID != "claude-a.json" {
		t.Fatalf("conversations %+v", convs)
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
	p := New(nopHost{}, "1.0.0", "missing.yaml")
	p.Handle("plugin.register", []byte(`{"config_yaml":"bWF4X3BpbmdzOiAtMQo="}`))

	var reg struct {
		Routes    []route
		Resources []resource
	}
	result := decode(t, p.Handle("management.register", nil))
	json.Unmarshal(result["routes"], &reg.Routes)
	json.Unmarshal(result["resources"], &reg.Resources)
	if len(reg.Routes) != 2 || len(reg.Resources) != 1 || reg.Resources[0].Path != pagePath {
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
}
