// Package host wraps the CPA host callbacks used by Wick.
//
// The native ABI exchanges JSON envelopes. Package main supplies a Caller that
// crosses the C boundary; everything above it works with Go values so that the
// rest of the plugin can be tested with a fake host.
package host

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Caller sends one host callback and returns the raw envelope bytes.
type Caller func(method string, payload []byte) ([]byte, error)

// Error is a failed host callback. Status carries the HTTP status that CPA
// attached to the error, or 0 when it did not set one.
type Error struct {
	Method  string
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.Status)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// StatusOf returns the HTTP status carried by err, or 0.
func StatusOf(err error) int {
	var hostErr *Error
	if errors.As(err, &hostErr) {
		return hostErr.Status
	}
	return 0
}

// Host is the set of host callbacks Wick uses.
type Host interface {
	ModelExecuteStream(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error)
	ModelStreamRead(streamID string) (pluginapi.HostModelStreamReadResponse, error)
	ModelStreamClose(streamID string)
	Log(level, message string, fields map[string]any)
}

// RPC implements Host on top of a Caller.
type RPC struct {
	Call Caller
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"http_status,omitempty"`
	} `json:"error,omitempty"`
}

func (r RPC) call(method string, payload any, out any) error {
	if r.Call == nil {
		return &Error{Method: method, Code: "host_unavailable", Message: "host API is not initialized"}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := r.Call(method, raw)
	if err != nil {
		return &Error{Method: method, Code: "host_call_failed", Message: err.Error()}
	}
	if len(resp) == 0 {
		return &Error{Method: method, Code: "host_call_failed", Message: "empty response"}
	}
	var env envelope
	if err := json.Unmarshal(resp, &env); err != nil {
		return &Error{Method: method, Code: "host_call_failed", Message: "invalid envelope: " + err.Error()}
	}
	if !env.OK {
		hostErr := &Error{Method: method, Code: "host_call_failed", Message: "host callback failed"}
		if env.Error != nil {
			hostErr.Code = env.Error.Code
			hostErr.Message = env.Error.Message
			hostErr.Status = env.Error.HTTPStatus
		}
		return hostErr
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return &Error{Method: method, Code: "host_call_failed", Message: "invalid result: " + err.Error()}
	}
	return nil
}

// ModelExecuteStream starts one streaming request through CPA's model
// executor. It returns once the first chunk or an error arrives. The caller
// must close the stream with ModelStreamClose.
//
// The request carries no host_callback_id: with one, CPA would close the
// stream as soon as the callback scope that started it ends.
func (r RPC) ModelExecuteStream(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
	req.Stream = true
	var resp pluginapi.HostModelStreamResponse
	err := r.call(pluginabi.MethodHostModelExecuteStream, req, &resp)
	return resp, err
}

// ModelStreamRead returns the next chunk of a stream. It blocks until a chunk
// arrives, the stream ends or the stream is closed.
func (r RPC) ModelStreamRead(streamID string) (pluginapi.HostModelStreamReadResponse, error) {
	var resp pluginapi.HostModelStreamReadResponse
	err := r.call(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: streamID}, &resp)
	return resp, err
}

// ModelStreamClose cancels a stream. A pending ModelStreamRead returns.
func (r RPC) ModelStreamClose(streamID string) {
	_ = r.call(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID}, nil)
}

// Log writes one entry to the CPA log.
func (r RPC) Log(level, message string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["plugin"] = "wick"
	_ = r.call(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": message,
		"fields":  fields,
	}, nil)
}
