package keepalive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// Skip reasons returned by parseRequest.
const (
	skipFormat      = "format"
	skipNotStream   = "not_stream"
	skipNotNative   = "not_native"
	skipSmall       = "small"
	skipNoSession   = "no_session"
	skipNoAuth      = "no_auth"
	skipTTL1h       = "ttl_1h"
	skipBadBody     = "bad_body"
	skipNoCache     = "no_cache_control"
	skipCachePolicy = "cache_policy"
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
	// Session is an explicit client session identifier.
	Session string
	// CallerScope is CPA's opaque caller identity, retained only for replay.
	CallerScope string
	// Other marks a non-native client; false also covers saved native requests.
	Other bool
	// Agent is the subagent ID, empty for the main conversation.
	Agent string
	// Key identifies one conversation: the caller scope, session, agent ID, and the
	// system prompt and tools, which also differ between kinds of subagent.
	Key   string
	Model string
	// Client, Cwd and Prompt describe the conversation on the management
	// page: the client entrypoint or product, the working directory and the first
	// prompt.
	Client    string
	Cwd       string
	Prompt    string
	AuthID    string
	RequestID string
	TraceID   string
	Headers   http.Header
	// Body is the outgoing client body, with 5m TTLs and CPA explicit cache
	// mode for other clients. Interception, persistence and replay use these
	// exact bytes.
	Body []byte
}

// parseRequest checks whether req is a Claude Messages streaming request
// worth keeping alive and builds the replay. It returns a skip reason when
// it is not. For skipTTL1h the returned request still carries the conversation
// scope and raw Key, so the caller can find and drop matching conversations.
func parseRequest(req pluginapi.RequestInterceptRequest, minBodyBytes int, otherClients bool) (request, string) {
	if req.SourceFormat != "claude" {
		return request{}, skipFormat
	}
	if !req.Stream {
		return request{}, skipNotStream
	}
	native := strings.TrimSpace(req.Headers.Get("X-App")) == "cli" && strings.HasPrefix(req.Headers.Get("User-Agent"), "claude-cli/")
	if !native && !otherClients {
		return request{}, skipNotNative
	}
	if len(req.Body) < minBodyBytes {
		return request{}, skipSmall
	}
	if !gjson.ValidBytes(req.Body) {
		return request{}, skipBadBody
	}
	session := requestSession(req)
	if session == "" {
		return request{}, skipNoSession
	}
	model := strings.TrimSpace(req.RequestedModel)
	if model == "" {
		model = gjson.GetBytes(req.Body, "model").String()
	}
	callerScope := req.CallerScope
	if callerScope == "" {
		callerScope, _ = req.Metadata[pluginapi.CallerScopeMetadataKey].(string)
	}
	agent := strings.TrimSpace(req.Headers.Get(agentHeader))
	out := request{
		Session:     session,
		CallerScope: callerScope,
		Other:       !native,
		Agent:       agent,
		Key:         conversationKey(callerScope, session, agent, model, req.Body),
		Model:       model,
		RequestID:   req.RequestID,
		TraceID:     req.TraceID,
		Client:      clientEntrypoint(req.Headers.Get("User-Agent")),
		Cwd:         workingDirectory(req.Body),
		Prompt:      firstPrompt(req.Body),
	}
	if ttl1hPattern.Match(req.Body) {
		return out, skipTTL1h
	}
	authID, _ := req.Metadata["selected_auth_id"].(string)
	if out.AuthID = strings.TrimSpace(authID); out.AuthID == "" {
		return request{}, skipNoAuth
	}
	out.Body = bytes.Clone(req.Body)
	if out.Other {
		var reason string
		out.Body, reason = pinCacheTTL(out.Body)
		if reason != "" {
			return request{}, reason
		}
		// Hash the pinned tools and system so retries of the original body
		// identify the same conversation.
		out.Key = conversationKey(callerScope, session, agent, model, out.Body)
	}
	out.Headers = req.Headers.Clone()
	// The executor sets the account's credentials itself, so the client's
	// credentials for CPA are not needed and are not kept or saved.
	for _, name := range []string{"Content-Length", "Authorization", "X-Api-Key", "Proxy-Authorization", "Cookie"} {
		out.Headers.Del(name)
	}
	return out, ""
}

// conversationKey hashes the caller scope, session, agent ID, model, tools and system prompt.
func conversationKey(callerScope, session, agent, model string, body []byte) string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(callerScope)
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
