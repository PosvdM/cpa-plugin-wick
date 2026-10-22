package keepalive

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Skip reasons returned by parseRequest.
const (
	skipFormat    = "format"
	skipNotStream = "not_stream"
	skipNotNative = "not_native"
	skipSmall     = "small"
	skipNoSession = "no_session"
	skipNoAuth    = "no_auth"
	skipTTL1h     = "ttl_1h"
	skipBudget    = "budget_thinking"
	skipBadBody   = "bad_body"
)

const (
	sessionHeader = "X-Claude-Code-Session-Id"
	// Claude Code sends this header on subagent requests only.
	agentHeader = "X-Claude-Code-Agent-Id"
)

// Claude Code puts a billing block at the start of the system prompt. Its
// cch value changes with every request, so it is left out of the
// conversation key.
const billingPrefix = "x-anthropic-billing-header"

var ttl1hPattern = regexp.MustCompile(`"ttl"\s*:\s*"1h"`)

// request is an intercepted request that Wick can replay.
type request struct {
	// Session is the Claude Code session ID.
	Session string
	// Agent is the subagent ID, empty for the main conversation.
	Agent string
	// Key identifies one conversation: the session, the agent ID, and the
	// system prompt and tools, which also differ between kinds of subagent.
	Key       string
	Model     string
	AuthID    string
	RequestID string
	Headers   http.Header
	// Body is the client body with max_tokens set to 0.
	Body []byte
}

// parseRequest checks whether req is a native Claude Code streaming request
// worth keeping alive and builds the replay. It returns a skip reason when
// it is not. For skipTTL1h the returned request still carries Session and
// Key, so the caller can drop that conversation.
func parseRequest(req pluginapi.RequestInterceptRequest, minBodyBytes int) (request, string) {
	if req.SourceFormat != "claude" {
		return request{}, skipFormat
	}
	if !req.Stream {
		return request{}, skipNotStream
	}
	if strings.TrimSpace(req.Headers.Get("X-App")) != "cli" || !strings.HasPrefix(req.Headers.Get("User-Agent"), "claude-cli/") {
		return request{}, skipNotNative
	}
	if len(req.Body) < minBodyBytes {
		return request{}, skipSmall
	}
	if !gjson.ValidBytes(req.Body) {
		return request{}, skipBadBody
	}
	session := strings.TrimSpace(req.Headers.Get(sessionHeader))
	if session == "" {
		session = gjson.GetBytes(req.Body, "metadata.user_id").String()
	}
	if session == "" {
		return request{}, skipNoSession
	}
	model := strings.TrimSpace(req.RequestedModel)
	if model == "" {
		model = gjson.GetBytes(req.Body, "model").String()
	}
	agent := strings.TrimSpace(req.Headers.Get(agentHeader))
	out := request{
		Session:   session,
		Agent:     agent,
		Key:       conversationKey(session, agent, model, req.Body),
		Model:     model,
		RequestID: req.RequestID,
	}
	if ttl1hPattern.Match(req.Body) {
		return out, skipTTL1h
	}
	// In budget thinking mode CPA replaces max_tokens <= 0 with the model
	// default, so the replay would generate output. Adaptive thinking is not
	// affected.
	if gjson.GetBytes(req.Body, "thinking.type").String() == "enabled" {
		return request{}, skipBudget
	}
	authID, _ := req.Metadata["selected_auth_id"].(string)
	if out.AuthID = strings.TrimSpace(authID); out.AuthID == "" {
		return request{}, skipNoAuth
	}
	// sjson edits the bytes in place and keeps the key order, so the rest of
	// the body stays byte for byte the same as the request that wrote the cache.
	body, err := sjson.SetBytes(req.Body, "max_tokens", 0)
	if err != nil {
		return request{}, skipBadBody
	}
	out.Body = body
	out.Headers = req.Headers.Clone()
	out.Headers.Del("Content-Length")
	return out, ""
}

// conversationKey hashes the session, agent ID, model, tools and system prompt.
func conversationKey(session, agent, model string, body []byte) string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(session)
	write(agent)
	write(model)
	write(gjson.GetBytes(body, "tools").Raw)
	system := gjson.GetBytes(body, "system")
	if system.IsArray() {
		system.ForEach(func(_, block gjson.Result) bool {
			if !strings.HasPrefix(block.Get("text").String(), billingPrefix) {
				write(block.Raw)
			}
			return true
		})
	} else {
		write(system.Raw)
	}
	return hex.EncodeToString(h.Sum(nil))
}
