package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestInterceptOtherClientWithoutSession(t *testing.T) {
	p := New(nopHost{}, "1.0.0", "missing.yaml", t.TempDir())
	defer p.Shutdown()
	config, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("min_body_kb: 0\nother_clients: true\n")})
	decode(t, p.Handle("plugin.register", config))
	in := pluginapi.RequestInterceptRequest{RequestID: "pi-no-session", SourceFormat: "claude", Stream: true,
		Headers:  http.Header{"User-Agent": {"pi (win32 10.0.26100; x64)"}},
		Body:     []byte(`{ "model":"claude-opus-5-5", "system":[{"text":"system","cache_control":{"type":"ephemeral"}}], "messages":[{"role":"user","content":"hi"}] }`),
		Metadata: map[string]any{"selected_auth_id": "claude-a.json"}}
	before := bytes.Clone(in.Body)
	payload, _ := json.Marshal(in)
	if result := decode(t, p.Handle("request.intercept_after", payload)); len(result) != 0 {
		t.Fatalf("request without session returned Body: %v", result)
	}
	if len(p.manager.Status().Conversations) != 0 || !bytes.Equal(in.Body, before) {
		t.Fatal("request without session tracked or changed")
	}
}

func TestInterceptOtherClientBody(t *testing.T) {
	p := New(nopHost{}, "1.0.0", "missing.yaml", t.TempDir())
	defer p.Shutdown()
	p.Handle("plugin.register", []byte(`{"config_yaml":"bWluX2JvZHlfa2I6IDAK"}`))
	in := pluginapi.RequestInterceptRequest{RequestID: "pi-request", CallerScope: "caller-a", SourceFormat: "claude", Stream: true,
		Headers:  http.Header{"User-Agent": {"pi (win32 10.0.26100; x64)"}, "X-Session-Affinity": {"pi-session"}},
		Body:     []byte(`{ "model":"claude-opus-5-5", "system":[{"text":"system","cache_control":{"type":"ephemeral"}}], "messages":[{"role":"user","content":"hi"}] }`),
		Metadata: map[string]any{"selected_auth_id": "claude-a.json"}}
	payload, _ := json.Marshal(in)
	if result := decode(t, p.Handle("request.intercept_after", payload)); len(result) != 0 || len(p.manager.Status().Conversations) != 0 {
		t.Fatalf("other client taken over by default: %v", result)
	}
	config, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("min_body_kb: 0\nother_clients: true\n")})
	decode(t, p.Handle("plugin.reconfigure", config))
	result := decode(t, p.Handle("request.intercept_after", payload))
	var out []byte
	if err := json.Unmarshal(result["Body"], &out); err != nil || gjson.GetBytes(out, "system.0.cache_control.ttl").String() != "5m" || gjson.GetBytes(out, "prompt_cache_options.mode").String() != "explicit" {
		t.Fatalf("response %v error %v", result, err)
	}
	retry := decode(t, p.Handle("request.intercept_after", payload))
	if string(retry["Body"]) != string(result["Body"]) {
		t.Fatal("retry returned different Body")
	}
	if convs := p.manager.Status().Conversations; len(convs) != 1 || convs[0].Client != "pi" || convs[0].Session != "pi-session" {
		t.Fatalf("tracked %+v", convs)
	}
	// Distinct caller scopes decoded through RPC must retain separate requests.
	in.CallerScope = "caller-b"
	second, _ := json.Marshal(in)
	decode(t, p.Handle("request.intercept_after", second))
	if len(p.manager.Status().Conversations) != 2 {
		t.Fatal("RPC caller scope was lost")
	}
	in.CallerScope = ""
	in.Metadata[pluginapi.CallerScopeMetadataKey] = "caller-c"
	third, _ := json.Marshal(in)
	decode(t, p.Handle("request.intercept_after", third))
	if len(p.manager.Status().Conversations) != 3 {
		t.Fatal("RPC metadata caller scope was lost")
	}
	// The registered field is boolean; reconfiguration stops tracked clients.
	reg := p.registration()
	found := false
	for _, field := range reg.Metadata.ConfigFields {
		if field.Name == "other_clients" {
			found = field.Type == pluginapi.ConfigFieldTypeBoolean
		}
	}
	if !found {
		t.Fatal("missing boolean config field")
	}
	config, _ = json.Marshal(map[string][]byte{"config_yaml": []byte("min_body_kb: 0\nother_clients: false\n")})
	decode(t, p.Handle("plugin.reconfigure", config))
	if len(p.manager.Status().Conversations) != 0 {
		t.Fatal("other client still tracked")
	}
	if result := decode(t, p.Handle("request.intercept_after", payload)); len(result) != 0 {
		t.Fatalf("disabled client returned Body: %v", result)
	}
}
