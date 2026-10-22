package plugin

import (
	"encoding/json"
	"net/http"
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
