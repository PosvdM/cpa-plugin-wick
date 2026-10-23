# Architecture

[中文](./architecture.md)

A ping in this document is one keep-alive request: the last request of a conversation sent again unchanged and closed once it has read the cache.

## Modules

| Path | Responsibility |
| --- | --- |
| `main.go` | cgo entry point; exports `cliproxy_plugin_init` and forwards calls between the C boundary and `plugin.Handle` |
| `internal/plugin` | RPC dispatch: registration, interception, request completion, management API; hands the data directory to the two stores below |
| `internal/keepalive` | Request filtering, conversation keys and descriptions, ping scheduling, SSE usage parsing, and saving and restoring the conversations kept lit |
| `internal/history` | Ping history: daily JSON Lines files, range queries, deletion of old files |
| `internal/host` | Host callback wrappers: streaming model execution, read, close and log |
| `internal/config` | Parses `plugins.configs.wick` and reads the enabled flags and the plugin directory from the CPA config |
| `internal/web` | The management page. HTML, CSS and JavaScript are edited as separate files, embedded in the plugin and served as one document |

## CPA interface

The plugin registers three capabilities: `request_interceptor`, `request_lifecycle_plugin` and `management_api`. The CPA SDK in `go.mod` is v8.0.4, which already has these methods and types; their behavior was checked against the CPA v8.0.39 source.

| Method | Use |
| --- | --- |
| `request.intercept_after` | Called after CPA selects the account and before the executor rewrites the request, with the raw client body, the inbound headers and `Metadata["selected_auth_id"]`. The plugin records the request and returns an empty object, so the request passes through unchanged |
| `request.intercept_before` | Required by the capability; returns an empty object |
| `request.complete` | Called asynchronously when a request ends. An `Outcome` of `failed` or `rejected` snuffs out that conversation |
| `host.model.execute_stream` | Sends a ping, with `auth_id` pinned to the original account |
| `host.model.stream_read` / `stream_close` | Reads the ping response up to `message_start`, then closes it |
| `host.log` | Writes to the CPA log |

Notes:

- `RequestInterceptRequest` and `RequestCompletion` have no JSON tags, so the host sends Go field names (`Body`, `Metadata` and so on), with `Body` in base64. Decode them with the `pluginapi` types.
- `request.intercept_after` runs synchronously on the request goroutine with no timeout, and CPA calls it again for every account retry. The handler only filters, computes the conversation key and description, and copies the body; a background goroutine writes the files.
- Requests the plugin sends through `host.model.execute_stream` skip this plugin's own interceptors. Other plugins, such as usage trackers, still see them.
- `execute_stream` is called without `host_callback_id`; with one, CPA would close the stream when the callback scope ends. `execute_stream` blocks until the first chunk arrives and cannot be cancelled meanwhile.
- `stream_read` has no timeout. Each ping starts a 60-second watchdog that closes the stream, which makes a blocked read return.
- When CPA unloads the plugin, it rejects all of the plugin's host callbacks before calling `plugin.shutdown`, so a ping stream in progress cannot be closed and stays open. `plugin.quiesce`, called when a hot update replaces the plugin, is not affected.
- On a hot update CPA calls `plugin.quiesce` on the old instance before loading the new one, so by the time the new instance restores conversations from disk, the old one has stopped pinging.
- Pings count toward the account state like any request; a failure such as a 429 puts the account into cooldown.
- CPA's log format prints only the fields it knows, such as `model`, and drops the other fields passed to `host.log`, so the plugin also writes them into the message.
- The plugin registers no `response_interceptor`. The session title Claude Code generates appears only in the response of a small streaming request, and reading it needs a stream chunk interceptor; with one registered, CPA hands the plugin the full body of every streaming request and every chunk, adding that step to all clients' traffic. The page describes a conversation by its first prompt instead.

## Management API and page

`management_api` registers the following routes and one page. The routes under `/v0/management/wick/` require the CPA management key; the page is a resource route that CPA serves without authentication, and it holds no data of its own, reading everything through the routes that need the key:

| Route | Use |
| --- | --- |
| `GET /v0/management/wick/status` | Returns `Manager.Status()` (the config, the conversations kept lit, and the time of the latest ping, `last_ping`) plus the plugin version `version`, the enabled flag in the CPA config `enabled`, the config error `config_error`, the data directory `data_dir` and the plugin clock `now`. A conversation's `session` is the full session ID and `key` the first 8 characters of the conversation key |
| `GET /v0/management/wick/history` | `range` is `24h` (default), `7d` or `all` (everything within the retention), and `limit` is the number of entries returned (default 20, at most 5000). Returns the totals for the range `totals`, the number of entries `count`, and the newest `limit` entries `events`, newest first |
| `POST /v0/management/wick/duration` | Takes `{"key": "<first 8 characters of the conversation key>", "hours": <hours>}` with `hours` from 0 to 24, where 0 returns to the default time. Returns the new status; returns 404 when no conversation matches |
| `POST /v0/management/wick/stop` | Takes `{"key": "<first 8 characters of the conversation key>"}`, snuffs out that conversation and returns the new status; returns 404 when no conversation matches |
| `GET /v0/resource/plugins/wick/page` | The management page, listed as Wick in the Management Center menu |

How the page works:

- The page runs in a same-origin iframe of the Management Center. It takes the management key from the Management Center's `localStorage` (`cli-proxy-auth`, XOR-obfuscated with a key derived from the host and User-Agent); without one it asks the user and keeps the key in `sessionStorage` only. The theme and language come from the `data-theme` and `lang` attributes of the parent page's root element, or from the Management Center settings in `localStorage` when the page is opened on its own. This part matches Lamplighter's management page.
- Settings are read and written through CPA's `GET` / `PATCH /v0/management/plugins/wick/config`. CPA shallow-merges the top-level keys, writes `config.yaml`, then hot-reloads and calls `plugin.reconfigure`. CPA does not validate plugin config, so the page checks the interval range, the retention and whole numbers before saving; with an invalid config the plugin uses the defaults and reports `config_error` in the status.
- The default duration presets and their `max_pings` values are in `PRESETS` in `page.js`, and the per-conversation presets in `DURATION_PRESETS`. The estimated cache lifetime is `max_pings × (interval_min + interval_max) / 2 + 300` seconds, shown in whole minutes rounded down.
- The page refreshes the status every 5 seconds and pauses while the tab is hidden. Idle times and countdowns update locally every second, corrected for the offset from the plugin clock using `now` in the status. The history is reloaded only when `last_ping` in the status changes.
- While the user is typing in the custom duration field, or has touched a conversation's duration menu in the last 15 seconds, a refresh does not rebuild the conversation table, which would close the menu. Arrow keys on a closed menu fire `change` at every step, so a change made with the keyboard is committed on Enter or when the menu loses focus.
- A conversation is shown by the last element of its project directory and its first prompt; the full first prompt, directory, session ID and conversation key are in the tooltip.

## Ping request

To hit the cache written by the original request, what goes upstream must match the original request:

- The body is an unchanged copy of the raw client body. For Claude OAuth accounts CPA computes the CCH of the final body every time (`signAnthropicMessagesBody` in `claude_signing.go`). The CCH in the billing block differs every turn and does not affect cache hits.
- The headers are the recorded inbound headers without `Content-Length` and the client's credentials for CPA (`Authorization`, `X-Api-Key`, `Proxy-Authorization`, `Cookie`). The Claude executor always replaces these headers with the account's own credentials, so removing them does not change the request, and it keeps the client key out of the data directory. CPA treats a request as native Claude Code when it has `X-App: cli`, a User-Agent starting with `claude-cli/`, `claude-code-20250219` in `anthropic-beta`, and `metadata.user_id` in the body; it then leaves `cache_control` alone.
- The request streams like the original. After `message_start` the plugin closes the stream and CPA cancels the upstream connection, which upstream sees the same as a client pressing Esc. `message_start` comes before any content, and by then the cache has been read; the upstream may generate a few tokens before the close, billed as output.
- It cannot disconnect earlier: `execute_stream` returns the stream ID only once the first chunk, `message_start`, arrives, and without `message_start` there is no confirmation that the cache was read or hit.
- `max_tokens` is left unchanged. Anthropic rejects `max_tokens: 0` on streaming requests (400, `stream cannot be true when max_tokens is 0`), and neither 0 nor 1 is a value Claude Code sends itself.

After `message_start`, the plugin records its `usage`. A `cache_creation_input_tokens` above 0 means the ping wrote to the cache: the cache had expired, or the ping differs from the original request. The conversation is then snuffed out so that writes are not paid for again and again.

## Request filter

A request is recorded only when all of the following hold:

- `SourceFormat` is `claude` and the request is streaming;
- the `X-App` header is `cli` and the User-Agent starts with `claude-cli/`;
- the body is at least `min_body_kb`;
- a session ID (`X-Claude-Code-Session-Id`, falling back to `metadata.user_id`) and `selected_auth_id` are present;
- the body contains no `"ttl": "1h"`. When a 1-hour cache shows up, the conversation's record is deleted.

## Conversation key and description

The conversation key is the SHA-256 of the session ID, the agent ID, the model, `tools` and `system`. Claude Code sends `X-Claude-Code-Agent-Id` only on subagent requests, with a different value per subagent; the main conversation has no such header. The billing block in `system` that starts with `x-anthropic-billing-header` changes with every request and is skipped. Consecutive requests of one conversation differ only in `messages` and share a key.

Only the last request of each conversation is kept. When the main conversation (a request without an agent ID) receives a new request, other conversations in the same session first seen after the main conversation's previous request are deleted: they are subagents started while the main conversation waited, and the main conversation continuing means they have returned. Subagent requests do not trigger deletion. If Claude Code sends no agent ID, every request is treated as the main conversation.

At most 32 conversations are kept in memory; beyond that, the one with the oldest last request is deleted, with conversations that have a time set deleted last.

The description on the page is taken from the body when the request is recorded (`describe.go`):

- Project directory: the path on the first `Primary working directory:` or `Working directory:` line, that is, the directory the session started in. Recent Claude Code versions send the environment as a message with role `system` in `messages`, older ones in `system`; both are searched, `messages` only in its first 20 entries.
- First prompt: the first user text in the first 20 messages. `<system-reminder>` blocks are removed first; local command output (`<local-command-…>`, `<bash-…>`) and the `Caveat: …` note Claude Code adds are skipped; a slash command is shown as `<command-name>` plus `<command-args>`. Whitespace collapses to single spaces; the status keeps up to 200 characters and the history up to 80.

## Scheduling

- When a conversation receives a request, the ping is scheduled at a uniformly random moment between `interval_min` and `interval_max` seconds after the request started. The cache timer starts when the upstream receives the request, so the starting point is the request start, not the end of the response; with a long response, the first ping may go out before the response finishes.
- After each successful ping, the next one is scheduled the same way from the start of that ping.
- By default a conversation is snuffed out after its `max_pings`th ping. A conversation with a time set uses a deadline `until` (the moment it was set plus the hours) instead: it is snuffed out when 300 seconds (the cache lifetime) after the start of a ping is not earlier than `until`, so the cache lasts at least until `until`. A new request clears `until`; returning to the default time on the page snuffs the conversation out at once if it has already reached `max_pings`.
- Every schedule takes a generation (`gen`) from a counter shared by the whole manager. The timer callback and the ping result both check it, so stale timers and results do not affect the new schedule, and a ping still in flight cannot match a conversation that was deleted and then seen again.
- Setting `max_pings` to 0 clears all conversations.
- Before each ping the plugin reads the CPA config file and deletes the conversation if the plugin is disabled. CPA v8.0.39 unloads a disabled plugin (the same as `plugin.shutdown`, below); the check covers the gap before unloading and CPA versions that keep the instance.
- `plugin.quiesce` and `plugin.shutdown` save the conversations, then stop all timers, clear the conversations in memory and close ping streams in progress; the next instance restores what was saved. A later `plugin.reconfigure` resumes and restores the conversations from disk again. Re-enabling a disabled plugin before the cache expires also restores the saved conversations.
- Snuffing out a conversation from the management page deletes it. A ping in flight still finishes and, finding no conversation when it returns, schedules nothing.
- Timer callbacks run on their own goroutines, where an unrecovered panic would exit the CPA process, so they use `recover`.

## Data directory

The data directory is the `data_dir` setting, or `<plugins.dir>/data/wick` when it is empty (`config.DataDir`). `plugins.dir` is read from the CPA config file, and relative paths are relative to the CPA working directory; CPA loads plugins from copies in the temp directory, so the path of the loaded library cannot be used. Directories have mode `0700` and files `0600`.

### Conversations kept lit

`internal/keepalive/persist.go` saves the conversations in memory under `conversations/`:

- `index.json`: for each conversation, the key, first-seen time, last request time, the last time the cache was read `last_read`, next ping time, ping count, `until` and the hours, and the last usage.
- `<conversation key>.req`: the first line is the request metadata (session ID, agent ID, model, account, request ID, directory, first prompt, headers) as JSON, followed by the request body unchanged. The body is not JSON-encoded, so the replay matches the original request byte for byte.

When a conversation changes (new request, ping, duration set, removal), the manager signals a background goroutine, which waits 5 seconds to batch the changes, takes a snapshot under the lock and writes the files outside it. A request file is rewritten only when its conversation's generation changes; other changes rewrite only `index.json`. Files are written to `.tmp` and renamed; files not in the snapshot are deleted; a conversation key must be 64 hexadecimal characters or it is not read.

`plugin.quiesce` and `plugin.shutdown` take a last snapshot before clearing the conversations, release the lock, make the goroutine exit and wait for it, then write that snapshot synchronously. On a hot update CPA quiesces the old instance before loading the new one, so the new instance reads the state the old one stopped with. Requests CPA receives after quiescing the old instance and before switching to the new one are recorded by neither; such a conversation replays its previous turn after the restore until it receives its next request.

After `plugin.register` or `plugin.reconfigure` sets the directory, the manager reads the saved conversations the first time the directory is set after the instance starts or resumes:

- The next ping is no earlier than 3 seconds after loading, which leaves CPA time to finish loading the plugin, and no later than 290 seconds after `last_read` (the cache lifetime less 10 seconds for the request to reach upstream); with `interval_max` above 290 the saved time can be later.
- If 3 seconds after loading is already later than 290 seconds after `last_read`, the cache counts as expired and the conversation is not restored; its files are deleted on the next write.
- A restored conversation takes a new generation and keeps its count, `until` and last request time.

### Ping history

`internal/history` appends the `keepalive.Event` of every ping to `history/<UTC date>.jsonl`, one JSON object per line, each written whole with one `O_APPEND` write, so two instances overlapping briefly during a hot update cannot corrupt a file. The first write of each day deletes files older than `history_retention_days` days. Queries read the files day by day and cache the parsed result by file size and modification time, so unchanged files are not parsed again.

## Development

### Environment

- Go 1.26.
- Building the library needs cgo and a C compiler for the target platform. The packages under `internal/` are pure Go, so tests do not need cgo.

### Tests

```bash
go vet ./internal/...
go test ./internal/...
```

The tests use a fake host and clock; they send no real requests and do not wait in real time. The persistence tests call the write function directly in a temp directory instead of waiting for the background goroutine. They cover the request filter and ping body (including removal of the client credentials), conversation keys, extraction of the project directory and first prompt, ping intervals and counts, rescheduling on a new request, snuffing out on a cache miss or error, removal of subagents (including parallel subagents with agent IDs), failed and interrupted requests, disabling the plugin and `max_pings: 0`, stale ping results, the deadline and clearing of a time set for a conversation, sparing such conversations on eviction, saving and restoring conversations (schedule kept, overdue ping sent soon, late ping brought forward, expired cache not restored, the last state before stopping, files deleted), writing, querying and pruning the ping history, SSE parsing, log messages, config validation and the data directory, snuffing out a conversation by hand, plugin RPC registration and decoding of intercepted requests, and the management API and page.

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
- When changing how the ping request is built, check CPA's `internal/runtime/executor/claude_executor*.go` to confirm that what goes upstream still matches the original request.
- When changing the fields of `conversation` or `request`, update the fields saved and restored in `persist.go`.
- New goroutines must use `recover`.
- User-visible behavior, configuration and limits go in the README (Chinese and English); implementation details go in this document (Chinese and English).
