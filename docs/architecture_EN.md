# Architecture

[中文](./architecture.md)

## Modules

| Path | Responsibility |
| --- | --- |
| `main.go` | cgo entry point; exports `cliproxy_plugin_init` and forwards calls between the C boundary and `plugin.Handle` |
| `internal/plugin` | RPC dispatch: registration, interception, request completion, management API |
| `internal/keepalive` | Request filtering, conversation keys, refresh scheduling and usage parsing |
| `internal/host` | Host callback wrappers: model execution and log |
| `internal/config` | Parses `plugins.configs.wick` and reads the enabled flags from the CPA config |

## CPA interface

The plugin registers three capabilities: `request_interceptor`, `request_lifecycle_plugin` and `management_api`. The CPA SDK in `go.mod` is v8.0.4, which already has these methods and types; their behavior was checked against the CPA v8.0.39 source.

| Method | Use |
| --- | --- |
| `request.intercept_after` | Called after CPA selects the account and before the executor rewrites the request, with the raw client body, the inbound headers and `Metadata["selected_auth_id"]`. The plugin records the request and returns an empty object, so the request passes through unchanged |
| `request.intercept_before` | Required by the capability; returns an empty object |
| `request.complete` | Called asynchronously when a request ends. An `Outcome` of `failed` or `rejected` stops refreshing that conversation |
| `host.model.execute` | Sends a non-streaming refresh, with `auth_id` pinned to the original account |
| `host.log` | Writes to the CPA log |

Notes:

- `RequestInterceptRequest` and `RequestCompletion` have no JSON tags, so the host sends Go field names (`Body`, `Metadata` and so on), with `Body` in base64. Decode them with the `pluginapi` types.
- `request.intercept_after` runs synchronously on the request goroutine with no timeout, and CPA calls it again for every account retry. The handler only filters, computes the conversation key and copies the body.
- Requests the plugin sends through `host.model.execute` skip this plugin's own interceptors. Other plugins, such as usage trackers, still see them.
- `host.model.execute` returns once the upstream has sent the whole response; it has no timeout and cannot be cancelled. A `max_tokens: 0` request generates nothing and usually returns quickly.
- Refreshes count toward the account state like any request; a failure such as a 429 puts the account into cooldown.
- CPA's log format prints only the fields it knows, such as `model`, and drops the other fields passed to `host.log`, so the plugin also writes them into the message.

## Refresh request

To hit the cache written by the original request, what goes upstream must match the original request:

- The body is the raw client body with only `max_tokens` set to 0 and `stream` to `false` by `sjson`. `sjson` edits in place, so the other bytes and the key order stay the same. CPA would set `stream` to `false` for a non-streaming execution anyway.
- The headers are the recorded inbound headers without `Content-Length`. CPA treats a request as native Claude Code when it has `X-App: cli`, a User-Agent starting with `claude-cli/`, `claude-code-20250219` in `anthropic-beta`, and `metadata.user_id` in the body; it then leaves `cache_control` alone.
- The request is not streamed. Anthropic rejects `max_tokens: 0` on streaming requests (400, `stream cannot be true when max_tokens is 0`).
- CCH signature: for Claude OAuth accounts, CPA recomputes the CCH of the final body on every request (`signAnthropicMessagesBody` in `claude_signing.go`), because Claude Code behind CPA does not sign. After `stream` and `max_tokens` change, the signature still matches the body. The CCH in the billing block differs on every turn and does not affect cache hits.
- With `max_tokens: 0` the upstream returns `stop_reason: max_tokens` with no content and `output_tokens` 0.
- In budget mode (`thinking.type` is `enabled`), CPA's `normalizeClaudeBudget` (`internal/thinking/provider/claude/apply.go`) replaces `max_tokens <= 0` with the model's default output limit when the model registers one, and the refresh would generate output. In tests 0 still took effect, but such requests are not refreshed to be safe. Adaptive mode does not reach that code.

The plugin records the `usage` of the response. A `cache_creation_input_tokens` above 0 means the refresh wrote to the cache: the cache had expired, or the refresh differs from the original request. Refreshing then stops so that writes are not paid for again and again.

## Request filter

A request is recorded only when all of the following hold:

- `SourceFormat` is `claude` and the request is streaming (Claude Code's main conversation and subagents stream);
- the `X-App` header is `cli` and the User-Agent starts with `claude-cli/`;
- the body is at least `min_body_kb`;
- a session ID (`X-Claude-Code-Session-Id`, falling back to `metadata.user_id`) and `selected_auth_id` are present;
- `thinking.type` is not `enabled`;
- the body contains no `"ttl": "1h"`. When a 1-hour cache shows up, the conversation's record is deleted.

## Conversation key

The conversation key is the SHA-256 of the session ID, the agent ID, the model, `tools` and `system`. Claude Code sends `X-Claude-Code-Agent-Id` only on subagent requests, with a different value per subagent; the main conversation has no such header. The billing block in `system` that starts with `x-anthropic-billing-header` changes with every request and is skipped. Consecutive requests of one conversation differ only in `messages` and share a key.

Only the last request of each conversation is kept. When the main conversation (a request without an agent ID) receives a new request, other conversations in the same session first seen after the main conversation's previous request are deleted: they are subagents started while the main conversation waited, and the main conversation continuing means they have returned. Subagent requests do not trigger deletion. If Claude Code sends no agent ID, every request is treated as the main conversation.

At most 32 conversations are kept in memory; beyond that, the one with the oldest last request is deleted.

## Scheduling

- When a conversation receives a request, the refresh is scheduled at a uniformly random moment between `interval_min` and `interval_max` seconds after the request started. The cache timer starts when the upstream receives the request, so the starting point is the request start, not the end of the response; with a long response, the first refresh may go out before the response finishes.
- After each successful refresh, the next one is scheduled the same way from the start of that refresh.
- Every schedule takes a generation (`gen`) from a counter shared by the whole manager. The timer callback and the refresh result both check it, so stale timers and results do not affect the new schedule, and a refresh still in flight cannot match a conversation that was deleted and then seen again.
- Setting `max_pings` to 0 clears all conversations.
- Before each refresh the plugin reads the CPA config file and deletes the conversation if the plugin is disabled. CPA keeps a disabled plugin loaded without notifying it.
- `plugin.quiesce` and `plugin.shutdown` stop all timers and clear the conversations. A refresh in progress completes, but its result is not used. A later `plugin.reconfigure` resumes.
- Timer callbacks run on their own goroutines, where an unrecovered panic would exit the CPA process, so they use `recover`.

## Development

### Environment

- Go 1.26.
- Building the library needs cgo and a C compiler for the target platform. The packages under `internal/` are pure Go, so tests do not need cgo.

### Tests

```bash
go vet ./internal/...
go test ./internal/...
```

The tests use a fake host and clock; they send no real requests and do not wait in real time. They cover the request filter and refresh body, conversation keys, refresh intervals and counts, rescheduling on a new request, stopping on a cache miss or error, removal of subagents (including parallel subagents with agent IDs), failed and interrupted requests, disabling the plugin and `max_pings: 0`, stale refresh results, usage parsing, config validation, and plugin RPC registration and decoding of intercepted requests.

### Build

With Docker available, build for the CPU architecture of the Docker host:

```bash
sh scripts/build.sh 0.1.0
```

The output is `dist/wick-v0.1.0.so`. The `-v<version>` part of the file name tells CPA the plugin ID (`wick`) and version.

### Release

Pushing a `v*` tag makes GitHub Actions (`.github/workflows/build.yml`) run the tests, build Linux amd64/arm64, macOS amd64/arm64 and Windows amd64, package each as `wick_<version>_<os>_<arch>.zip` with `checksums.txt`, and create a draft release. Pushes to `main` only build and upload Actions artifacts.

### Conventions

- Before changing host callbacks, RPC fields or registration, check CPA's `sdk/pluginabi`, `sdk/pluginapi` and `internal/pluginhost`.
- When changing how the refresh request is built, check CPA's `internal/runtime/executor/claude_executor*.go` to confirm that what goes upstream still matches the original request.
- New goroutines must use `recover`.
- User-visible behavior, configuration and limits go in the README (Chinese and English); implementation details go in this document (Chinese and English).
