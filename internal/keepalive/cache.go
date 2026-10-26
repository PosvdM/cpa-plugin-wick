package keepalive

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

var legacyClaudeSessionPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// requestSession follows CPA v8.0.43's ExtractSessionInfo priority for the
// explicit session sources supported by Wick.
func requestSession(req pluginapi.RequestInterceptRequest) string {
	if session := sessionHeaderValue(req.Headers, sessionHeader); session != "" {
		return session
	}
	rawUserID := gjson.GetBytes(req.Body, "metadata.user_id").String()
	userID := strings.TrimSpace(rawUserID)
	// ClaudeMetadataIdentities validates the extracted session, not the
	// surrounding JSON/legacy container, which may exceed 256 bytes.
	var metadataSession string
	if strings.HasPrefix(userID, "{") {
		metadataSession = gjson.Get(userID, "session_id").String()
	} else if matches := legacyClaudeSessionPattern.FindStringSubmatch(userID); len(matches) >= 2 {
		metadataSession = matches[1]
	}
	if session := normalizeExplicitSession(metadataSession); session != "" {
		return session
	}
	for _, header := range []string{"Session-Id", "X-Session-Id", "X-Session-Affinity"} {
		if session := sessionHeaderValue(req.Headers, header); session != "" {
			return session
		}
	}
	// CPA also accepts the entire user_id as a final opaque candidate when
	// Claude metadata extraction fails, provided that container is valid.
	// Wick tracks explicit sessions only; content-derived identities are
	// outside its conversation lifecycle.
	return normalizeExplicitSession(rawUserID)
}

// normalizeExplicitSession mirrors CPA's NormalizeExplicitID. Controls are
// rejected before trimming, and the size limit counts bytes rather than runes.
func normalizeExplicitSession(raw string) string {
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ""
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 {
		return ""
	}
	return raw
}

func sessionHeaderValue(headers http.Header, name string) string {
	if value := normalizeExplicitSession(headers.Get(name)); value != "" {
		return value
	}
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if session := normalizeExplicitSession(value); session != "" {
				return session
			}
		}
	}
	return ""
}

// pinCacheTTL takes over 1–4 valid ephemeral markers at CPA's cache positions
// only when all omit ttl and the caller has not chosen another cache mode.
// Explicit mode preserves surviving markers and TTLs, but cannot prevent
// strict cloaking from discarding caller system content.
// Edits run backwards so bytes outside the inserted TTLs and mode stay intact.
func pinCacheTTL(body []byte) ([]byte, string) {
	type insertion struct {
		at   int
		text string
	}
	var inserts []insertion
	count, hasTTL := 0, false
	forEachCacheBlock(body, func(block gjson.Result) {
		cc := block.Get("cache_control")
		if !cc.IsObject() || cc.Get("type").String() != "ephemeral" {
			return
		}
		count++
		if cc.Get("ttl").Exists() {
			hasTTL = true
		}
		inserts = append(inserts, insertion{cc.Index + len(cc.Raw) - 1, `,"ttl":"5m"`})
	})
	if count == 0 {
		return body, skipNoCache
	}
	if count > 4 || hasTTL {
		return body, skipCachePolicy
	}
	options := gjson.GetBytes(body, "prompt_cache_options")
	if options.Exists() {
		if !options.IsObject() {
			return body, skipCachePolicy
		}
		mode := options.Get("mode")
		if mode.Exists() {
			if mode.Type != gjson.String || !strings.EqualFold(strings.TrimSpace(mode.String()), "explicit") {
				return body, skipCachePolicy
			}
		} else {
			text := `"mode":"explicit"`
			if len(options.Map()) > 0 {
				text = "," + text
			}
			inserts = append(inserts, insertion{options.Index + len(options.Raw) - 1, text})
		}
	} else {
		inserts = append(inserts, insertion{bytes.LastIndexByte(body, '}'), `,"prompt_cache_options":{"mode":"explicit"}`})
	}
	sort.Slice(inserts, func(i, j int) bool { return inserts[i].at < inserts[j].at })
	for i := len(inserts) - 1; i >= 0; i-- {
		edit := inserts[i]
		next := make([]byte, 0, len(body)+len(edit.text))
		next = append(next, body[:edit.at]...)
		next = append(next, edit.text...)
		body = append(next, body[edit.at:]...)
	}
	return body, ""
}

// cacheIdentityKey compares tools/system structurally for non-native 1h
// retirement only. Missing, 5m and 1h TTLs match at legal cache positions;
// nested markers, other values and array order remain significant. Neither
// stored conversation keys nor outgoing/replayed bytes use this identity.
func cacheIdentityKey(body []byte) string {
	identity := make(map[string]any, 2)
	for _, name := range []string{"tools", "system"} {
		raw := gjson.GetBytes(body, name)
		if !raw.Exists() {
			continue
		}
		var value any
		decoder := json.NewDecoder(strings.NewReader(raw.Raw))
		// Keep number representations and precision in tool schemas intact.
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return ""
		}
		if blocks, ok := value.([]any); ok {
			for _, block := range blocks {
				obj, _ := block.(map[string]any)
				cc, _ := obj["cache_control"].(map[string]any)
				if cc["type"] != "ephemeral" {
					continue
				}
				if ttl, ok := cc["ttl"].(string); ok && (ttl == "5m" || ttl == "1h") {
					delete(cc, "ttl")
				}
			}
		}
		identity[name] = value
	}
	canonical, err := json.Marshal(identity)
	if err != nil {
		return ""
	}
	// Reuse billing-block exclusion without changing conversationKey itself.
	return conversationKey("", "", "", "", canonical)
}

func forEachCacheBlock(body []byte, visit func(gjson.Result)) {
	for _, name := range []string{"tools", "system"} {
		if blocks := gjson.GetBytes(body, name); blocks.IsArray() {
			blocks.ForEach(func(_, block gjson.Result) bool { visit(block); return true })
		}
	}
	if messages := gjson.GetBytes(body, "messages"); messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			if content := msg.Get("content"); content.IsArray() {
				content.ForEach(func(_, block gjson.Result) bool { visit(block); return true })
			}
			return true
		})
	}
}
