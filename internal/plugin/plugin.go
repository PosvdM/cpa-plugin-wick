// Package plugin implements the CPA plugin RPC methods on top of the
// keep-alive manager. Package main forwards every native call to Handle.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/config"
	"github.com/PosvdM/cpa-plugin-wick/internal/history"
	"github.com/PosvdM/cpa-plugin-wick/internal/host"
	"github.com/PosvdM/cpa-plugin-wick/internal/keepalive"
	"github.com/PosvdM/cpa-plugin-wick/internal/pricing"
	"github.com/PosvdM/cpa-plugin-wick/internal/web"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	repository = "https://github.com/PosvdM/cpa-plugin-wick"
	apiBase    = "/v0/management/" + config.PluginID
	// pagePath is the management page, served by CPA under
	// /v0/resource/plugins/wick/.
	pagePath = "/page"
)

// priceSource is the pricing page; tests point it elsewhere.
var priceSource = pricing.Source

// Plugin handles the RPC methods of one loaded plugin instance.
type Plugin struct {
	version        string
	host           host.Host
	manager        *keepalive.Manager
	history        *history.Store
	prices         *pricing.Store
	hostConfigPath string
	workDir        string

	mu          sync.Mutex
	configError string
}

// New creates the plugin. hostConfigPath is the CPA config file and
// workDir the CPA working directory, against which relative paths resolve.
func New(h host.Host, version, hostConfigPath, workDir string) *Plugin {
	store := history.New()
	prices := pricing.NewStore(func(level, message string) { h.Log(level, message, nil) })
	prices.URL = priceSource
	return &Plugin{
		version:        version,
		host:           h,
		hostConfigPath: hostConfigPath,
		workDir:        workDir,
		history:        store,
		prices:         prices,
		manager: keepalive.New(keepalive.Options{
			Host:     h,
			Enabled:  func() bool { return enabledInHostConfig(hostConfigPath) },
			Recorder: store,
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

// Shutdown stops all keep-alives and the price updates.
func (p *Plugin) Shutdown() {
	p.manager.Stop()
	p.prices.Stop()
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
		configError := ""
		if err != nil {
			configError = err.Error()
			p.host.Log("warn", "wick: invalid config, using defaults: "+configError, nil)
		}
		p.mu.Lock()
		p.configError = configError
		p.mu.Unlock()
		p.manager.Configure(cfg)
		dir := config.DataDir(cfg.DataDir, p.hostConfigPath, p.workDir)
		p.history.Configure(filepath.Join(dir, "history"), cfg.HistoryRetentionDays)
		p.manager.SetStore(filepath.Join(dir, "conversations"))
		p.prices.Start(dir)
		return ok(p.registration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		p.Shutdown()
		return ok(struct{}{})
	case pluginabi.MethodRequestInterceptAfter:
		// The struct has no JSON tags, so the host sends Go field names.
		var req pluginapi.RequestInterceptRequest
		if err := json.Unmarshal(request, &req); err == nil {
			if body := p.manager.Observe(req); len(body) > 0 {
				return ok(struct{ Body []byte }{Body: body})
			}
		}
		// Native and skipped requests pass through without a Body field.
		return ok(struct{}{})
	case pluginabi.MethodRequestInterceptBefore:
		return ok(struct{}{})
	case pluginabi.MethodRequestComplete:
		var done pluginapi.RequestCompletion
		if err := json.Unmarshal(request, &done); err == nil {
			p.manager.Complete(done)
		}
		return ok(struct{}{})
	case pluginabi.MethodUsageHandle:
		var rec pluginapi.UsageRecord
		if err := json.Unmarshal(request, &rec); err == nil {
			p.manager.Usage(rec)
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
				field("interval_min", pluginapi.ConfigFieldTypeInteger, "最短挑灯间隔（秒） / Shortest interval between pings in seconds"),
				field("interval_max", pluginapi.ConfigFieldTypeInteger, "最长挑灯间隔（秒），不超过 299 / Longest interval between pings in seconds, at most 299"),
				field("duration_minutes", pluginapi.ConfigFieldTypeInteger, "对话空闲后挑灯多少分钟，0 为关闭 / Minutes an idle conversation is kept lit; 0 turns Wick off"),
				field("subagent_duration_minutes", pluginapi.ConfigFieldTypeInteger, "子代理空闲后挑灯多少分钟，0 为不挑灯 / Minutes an idle subagent is kept lit; 0 leaves them out"),
				field("other_clients", pluginapi.ConfigFieldTypeBoolean, "为其他 Claude Messages 客户端挑灯，默认关闭；CPA 工具别名可能使重放无法读取缓存，仅在确认缓存可命中时开启 / Keep other Claude Messages clients lit, off by default; CPA tool aliases can prevent cache hits on replay, so enable only after verifying cache hits"),
				field("min_body_kb", pluginapi.ConfigFieldTypeInteger, "请求体小于此大小（KiB）时不挑灯 / Skip requests whose body is smaller than this many KiB"),
				field("history_retention_days", pluginapi.ConfigFieldTypeInteger, "挑灯记录保留天数 / Days of ping history to keep"),
				field("data_dir", pluginapi.ConfigFieldTypeString, "数据目录，默认 <插件目录>/data/wick / Data directory, <plugins dir>/data/wick by default"),
			},
		},
		Capabilities: map[string]bool{
			"request_interceptor":      true,
			"request_lifecycle_plugin": true,
			"usage_plugin":             true,
			"management_api":           true,
		},
	}
}

type route struct {
	Method      string
	Path        string
	Description string
}

type resource struct {
	Path        string
	Menu        string
	Description string
}

func managementRegistration() map[string]any {
	return map[string]any{
		"routes": []route{
			{Method: http.MethodGet, Path: apiBase + "/status", Description: "Conversations kept lit"},
			{Method: http.MethodGet, Path: apiBase + "/history", Description: "Ping history"},
			{Method: http.MethodGet, Path: apiBase + "/savings", Description: "Cost of the pings, the writes they saved, and a comparison with a 1-hour cache"},
			{Method: http.MethodPost, Path: apiBase + "/duration", Description: "Set how long one conversation is kept lit"},
			{Method: http.MethodPost, Path: apiBase + "/stop", Description: "Snuff out one conversation"},
		},
		"resources": []resource{
			{Path: pagePath, Menu: "Wick", Description: "提示缓存挑灯的状态和设置 / Prompt cache keep-alive status and settings"},
		},
	}
}

// status is the response of the status API: the keep-alive state plus what
// the management page needs around it.
type status struct {
	Version string `json:"version"`
	// Enabled is the enabled flag in the CPA config.
	Enabled     bool   `json:"enabled"`
	ConfigError string `json:"config_error,omitempty"`
	// DataDir is the directory of the ping history and saved conversations.
	DataDir string `json:"data_dir"`
	// Now lets the page count down in the plugin's clock.
	Now time.Time `json:"now"`
	keepalive.Status
}

func (p *Plugin) status() status {
	p.mu.Lock()
	configError := p.configError
	p.mu.Unlock()
	s := status{
		Version:     p.version,
		Enabled:     enabledInHostConfig(p.hostConfigPath),
		ConfigError: configError,
		DataDir:     filepath.Dir(p.history.Dir()),
		Now:         time.Now(),
		Status:      p.manager.Status(),
	}
	// The total of a conversation counts its pings in the whole history, so
	// that it survives the conversation being snuffed out and seen again.
	if len(s.Conversations) > 0 {
		if totals, err := p.history.CountByKey(s.Now.AddDate(0, 0, -p.history.Retention()-1)); err == nil {
			for i := range s.Conversations {
				c := &s.Conversations[i]
				// A ping in flight is counted in the round before it is
				// written to the history.
				c.TotalPings = max(totals[c.Key], c.Pings)
			}
		}
	}
	return s
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
	if strings.HasSuffix(path, "/"+config.PluginID+pagePath) && strings.Contains(path, "/v0/resource/plugins/") {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":  []string{"text/html; charset=utf-8"},
				"Cache-Control": []string{"no-store"},
			},
			Body: web.Page(),
		}
	}
	switch strings.ToUpper(req.Method) + " " + path {
	case "GET " + apiBase + "/status":
		return jsonResponse(http.StatusOK, p.status())
	case "GET " + apiBase + "/history":
		return p.historyResponse(req.Query)
	case "GET " + apiBase + "/savings":
		return p.savingsResponse(req.Query)
	case "POST " + apiBase + "/duration":
		var body struct {
			Key     string `json:"key"`
			Minutes *int   `json:"minutes"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil || strings.TrimSpace(body.Key) == "" || body.Minutes == nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "key and minutes are required"})
		}
		switch err := p.manager.SetDuration(strings.TrimSpace(body.Key), *body.Minutes); {
		case errors.Is(err, keepalive.ErrNotFound):
			return jsonResponse(http.StatusNotFound, map[string]string{"error": err.Error()})
		case err != nil:
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return jsonResponse(http.StatusOK, p.status())
	case "POST " + apiBase + "/stop":
		var body struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil || strings.TrimSpace(body.Key) == "" {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "key is required"})
		}
		if !p.manager.StopConversation(strings.TrimSpace(body.Key)) {
			return jsonResponse(http.StatusNotFound, map[string]string{"error": "conversation not found"})
		}
		return jsonResponse(http.StatusOK, p.status())
	}
	return jsonResponse(http.StatusNotFound, map[string]string{"error": fmt.Sprintf("unknown route %s %s", req.Method, req.Path)})
}

// Limits on the number of conversations the history API returns.
const (
	defaultHistoryLimit = 20
	maxHistoryLimit     = 5000
)

type historyResponse struct {
	Range string    `json:"range"`
	From  time.Time `json:"from"`
	// Prices describes the API prices the amounts are calculated with.
	Prices pricing.State `json:"prices"`
	// Count is the number of conversations pinged in the range;
	// Conversations holds the limit that were pinged last, newest first.
	Count         int                    `json:"count"`
	Conversations []history.Conversation `json:"conversations"`
}

// parseRange returns the start of a range: 24h, 7d, 1m from the same time
// a month ago, or all, which covers the retention. An empty range is 24h.
func (p *Plugin) parseRange(rng string, now time.Time) (string, time.Time, bool) {
	switch rng {
	case "", "24h":
		return "24h", now.Add(-24 * time.Hour), true
	case "7d":
		return rng, now.AddDate(0, 0, -7), true
	case "1m":
		return rng, now.AddDate(0, -1, 0), true
	case "all":
		return rng, now.AddDate(0, 0, -p.history.Retention()-1), true
	}
	return "", time.Time{}, false
}

// grouped returns the events of the range in query and the conversations
// grouped from them.
func (p *Plugin) grouped(query url.Values, now time.Time) (string, time.Time, []keepalive.Event, []history.Conversation, *pluginapi.ManagementResponse) {
	rng, from, valid := p.parseRange(query.Get("range"), now)
	if !valid {
		resp := jsonResponse(http.StatusBadRequest, map[string]string{"error": "range must be 24h, 7d, 1m or all"})
		return "", time.Time{}, nil, nil, &resp
	}
	events, err := p.history.Query(from)
	if err != nil {
		resp := jsonResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return "", time.Time{}, nil, nil, &resp
	}
	return rng, from, events, history.Group(events, now, p.prices.Table()), nil
}

// historyResponse answers GET /history?range=24h|7d|1m|all&limit=N with the
// pings summed by conversation and round. Amounts are calculated at the
// current prices on every query.
func (p *Plugin) historyResponse(query url.Values) pluginapi.ManagementResponse {
	limit := defaultHistoryLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "limit must be a positive integer"})
		}
		limit = min(n, maxHistoryLimit)
	}
	rng, from, _, convs, errResp := p.grouped(query, time.Now())
	if errResp != nil {
		return *errResp
	}
	resp := historyResponse{Range: rng, From: from, Prices: p.prices.State(), Count: len(convs), Conversations: convs[:min(limit, len(convs))]}
	if resp.Conversations == nil {
		resp.Conversations = []history.Conversation{}
	}
	return jsonResponse(http.StatusOK, resp)
}

type savingsResponse struct {
	Range   string             `json:"range"`
	From    time.Time          `json:"from"`
	Totals  history.Totals     `json:"totals"`
	Compare history.Comparison `json:"compare"`
	Prices  pricing.State      `json:"prices"`
}

// savingsResponse answers GET /savings?range=24h|7d|1m|all with the totals
// of the pings and rounds and the comparison of cache prices.
func (p *Plugin) savingsResponse(query url.Values) pluginapi.ManagementResponse {
	rng, from, events, convs, errResp := p.grouped(query, time.Now())
	if errResp != nil {
		return *errResp
	}
	return jsonResponse(http.StatusOK, savingsResponse{
		Range:   rng,
		From:    from,
		Totals:  history.Total(convs),
		Compare: history.Compare(events, convs, p.prices.Table()),
		Prices:  p.prices.State(),
	})
}
