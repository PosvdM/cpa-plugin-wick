package keepalive

import (
	"errors"

	"github.com/tidwall/gjson"
)

// Usage is the token usage of a replay.
type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	Ephemeral5mTokens   int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hTokens   int64 `json:"ephemeral_1h_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
}

// parseUsage reads the usage from a non-streaming Claude Messages response,
// or the message of an error response.
func parseUsage(body []byte) (*Usage, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("invalid response body")
	}
	resp := gjson.ParseBytes(body)
	if resp.Get("type").String() == "error" {
		msg := resp.Get("error.message").String()
		if msg == "" {
			msg = resp.Raw
		}
		return nil, errors.New("upstream error: " + msg)
	}
	usage := resp.Get("usage")
	if !usage.Exists() {
		return nil, errors.New("response has no usage")
	}
	return &Usage{
		InputTokens:         usage.Get("input_tokens").Int(),
		CacheReadTokens:     usage.Get("cache_read_input_tokens").Int(),
		CacheCreationTokens: usage.Get("cache_creation_input_tokens").Int(),
		Ephemeral5mTokens:   usage.Get("cache_creation.ephemeral_5m_input_tokens").Int(),
		Ephemeral1hTokens:   usage.Get("cache_creation.ephemeral_1h_input_tokens").Int(),
		OutputTokens:        usage.Get("output_tokens").Int(),
	}, nil
}
