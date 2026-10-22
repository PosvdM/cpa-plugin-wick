package keepalive

import (
	"bytes"
	"errors"

	"github.com/tidwall/gjson"
)

// maxSSEBytes bounds the bytes read while looking for message_start.
const maxSSEBytes = 1 << 20

// Usage is the token usage reported in message_start.
type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	Ephemeral5mTokens   int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hTokens   int64 `json:"ephemeral_1h_input_tokens"`
}

// sseScanner reads a Claude SSE stream until message_start or an error event.
type sseScanner struct {
	buf   []byte
	total int
}

// feed adds a chunk. It returns the usage once message_start arrives, or an
// error for an error event or an oversized stream.
func (s *sseScanner) feed(chunk []byte) (*Usage, error) {
	s.buf = append(s.buf, chunk...)
	s.total += len(chunk)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(s.buf[:i])
		s.buf = s.buf[i+1:]
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		event := gjson.ParseBytes(bytes.TrimSpace(data))
		switch event.Get("type").String() {
		case "message_start":
			usage := event.Get("message.usage")
			return &Usage{
				InputTokens:         usage.Get("input_tokens").Int(),
				CacheReadTokens:     usage.Get("cache_read_input_tokens").Int(),
				CacheCreationTokens: usage.Get("cache_creation_input_tokens").Int(),
				Ephemeral5mTokens:   usage.Get("cache_creation.ephemeral_5m_input_tokens").Int(),
				Ephemeral1hTokens:   usage.Get("cache_creation.ephemeral_1h_input_tokens").Int(),
			}, nil
		case "error":
			msg := event.Get("error.message").String()
			if msg == "" {
				msg = event.Raw
			}
			return nil, errors.New("upstream error: " + msg)
		}
	}
	if s.total > maxSSEBytes {
		return nil, errors.New("no message_start in the first 1 MiB of the stream")
	}
	return nil, nil
}
