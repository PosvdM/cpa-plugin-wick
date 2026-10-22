// Package plugin implements the CPA plugin RPC methods on top of the
// keep-alive manager. Package main forwards every native call to Handle.
package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/PosvdM/cpa-plugin-wick/internal/config"
	"github.com/PosvdM/cpa-plugin-wick/internal/host"
	"github.com/PosvdM/cpa-plugin-wick/internal/keepalive"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	repository = "https://github.com/PosvdM/cpa-plugin-wick"
	apiBase    = "/v0/management/" + config.PluginID
)

// Plugin handles the RPC methods of one loaded plugin instance.
type Plugin struct {
	version string
	host    host.Host
	manager *keepalive.Manager
}

// New creates the plugin. hostConfigPath is the CPA config file.
func New(h host.Host, version, hostConfigPath string) *Plugin {
	return &Plugin{
		version: version,
		host:    h,
		manager: keepalive.New(keepalive.Options{
			Host:    h,
			Enabled: func() bool { return enabledInHostConfig(hostConfigPath) },
		}),
	}
}

// enabledInHostConfig reads the enabled flags from the CPA config. When the
// file cannot be read it assumes the plugin is enabled, because CPA would not
// call a disabled plugin's interceptor in the first place.
func enabledInHostConfig(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	enabled, err := config.PluginEnabled(raw)
	return err != nil || enabled
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func ok(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// ErrorEnvelope encodes a failed RPC result.
func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// Handle runs one RPC method and returns the response envelope. A panic is
// turned into an error envelope so that it cannot crash CPA.
func (p *Plugin) Handle(method string, request []byte) (out []byte) {
	defer func() {
		if r := recover(); r != nil {
			out = ErrorEnvelope("plugin_panic", fmt.Sprint(r))
		}
	}()
	result, err := p.handle(method, request)
	if err != nil {
		return ErrorEnvelope("plugin_error", err.Error())
	}
	return result
}

// Shutdown stops all keep-alives.
func (p *Plugin) Shutdown() {
	p.manager.Stop()
}

func (p *Plugin) handle(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		cfg, err := config.Parse(req.ConfigYAML)
		if err != nil {
			p.host.Log("warn", "wick: invalid config, using defaults: "+err.Error(), nil)
		}
		p.manager.Configure(cfg)
		return ok(p.registration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		p.manager.Stop()
		return ok(struct{}{})
	case pluginabi.MethodRequestInterceptAfter:
		// The struct has no JSON tags, so the host sends Go field names.
		var req pluginapi.RequestInterceptRequest
		if err := json.Unmarshal(request, &req); err == nil {
			p.manager.Observe(req)
		}
		// An empty response passes the request through unchanged.
		return ok(struct{}{})
	case pluginabi.MethodRequestInterceptBefore:
		return ok(struct{}{})
	case pluginabi.MethodRequestComplete:
		var done pluginapi.RequestCompletion
		if err := json.Unmarshal(request, &done); err == nil {
			p.manager.Complete(done)
		}
		return ok(struct{}{})
	case pluginabi.MethodManagementRegister:
		return ok(managementRegistration())
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
		return ok(p.handleManagement(req))
	}
	return ErrorEnvelope("unknown_method", "unknown method: "+method), nil
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]bool    `json:"capabilities"`
}

func field(name string, kind pluginapi.ConfigFieldType, description string) pluginapi.ConfigField {
	return pluginapi.ConfigField{Name: name, Type: kind, Description: description}
}

func (p *Plugin) registration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Wick",
			Version:          p.version,
			Author:           "PosvdM",
			GitHubRepository: repository,
			ConfigFields: []pluginapi.ConfigField{
				field("interval_min", pluginapi.ConfigFieldTypeInteger, "最短保活间隔（秒） / Shortest keep-alive interval in seconds"),
				field("interval_max", pluginapi.ConfigFieldTypeInteger, "最长保活间隔（秒），不超过 299 / Longest keep-alive interval in seconds, at most 299"),
				field("max_pings", pluginapi.ConfigFieldTypeInteger, "一次空闲最多保活几次，0 为关闭 / Pings per idle period; 0 turns keep-alive off"),
				field("min_body_kb", pluginapi.ConfigFieldTypeInteger, "请求体小于此大小（KiB）时不保活 / Skip requests whose body is smaller than this many KiB"),
			},
		},
		Capabilities: map[string]bool{
			"request_interceptor":      true,
			"request_lifecycle_plugin": true,
			"management_api":           true,
		},
	}
}

type route struct {
	Method      string
	Path        string
	Description string
}

func managementRegistration() map[string]any {
	return map[string]any{
		"routes": []route{
			{Method: http.MethodGet, Path: apiBase + "/status", Description: "Wick keep-alive status"},
		},
	}
}

func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		raw = []byte(`{"error":"encode response failed"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
		Body:       raw,
	}
}

func (p *Plugin) handleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimRight(req.Path, "/")
	if strings.ToUpper(req.Method) == http.MethodGet && path == apiBase+"/status" {
		return jsonResponse(http.StatusOK, p.manager.Status())
	}
	return jsonResponse(http.StatusNotFound, map[string]string{"error": fmt.Sprintf("unknown route %s %s", req.Method, req.Path)})
}
