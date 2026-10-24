# Architecture

[中文](./architecture.md)

A ping in this document is one keep-alive request: the last request of a conversation sent again unchanged and closed once it has read the cache.

## Modules

| Path | Responsibility |
| --- | --- |
| `main.go` | cgo entry point; exports `cliproxy_plugin_init` and forwards calls between the C boundary and `plugin.Handle` |
| `internal/plugin` | RPC dispatch: registration, interception, request completion, management API; hands the data directory to the two stores below |
| `internal/keepalive` | Request filtering, conversation keys and descriptions, ping scheduling, SSE usage parsing, recording the usage of requests, and saving and restoring the conversations kept lit |
| `internal/history` | Ping history: writes daily JSON Lines files, queries ranges, sums by conversation and round, works out cost and savings, prices the cache three ways, and deletes old files |
| `internal/pricing` | Fetches and parses Anthropic's pricing page, keeps the prices last fetched, and looks up prices by model name |
| `internal/host` | Host callback wrappers: streaming model execution, read, close and log |
| `internal/config` | Parses `plugins.configs.wick` and reads the enabled flags and the plugin directory from the CPA config |
| `internal/web` | The management page. HTML, CSS, JavaScript and the logo are edited as separate files, embedded in the plugin and served as one document |

## CPA interface

The plugin registers four capabilities: `request_interceptor`, `request_lifecycle_plugin`, `usage_plugin` and `management_api`. The CPA SDK in `go.mod` is v8.0.4, which already has these methods and types; their behavior was checked against the CPA v8.0.39 source.

| Method | Use |
| --- | --- |
| `request.intercept_after` | Called after CPA selects the account and before the executor rewrites the request, with the raw client body, the inbound headers and `Metadata["selected_auth_id"]`. The plugin records the request and returns an empty object, so the request passes through unchanged |
| `request.intercept_before` | Required by the capability; returns an empty object |
| `request.complete` | Called asynchronously when a request ends. An `Outcome` of `failed` or `rejected` snuffs out that conversation |
| `usage.handle` | Called asynchronously after a request completes, with a `UsageRecord`. The plugin writes a `request` event for a request it recorded and ignores the usage of others; see [Request usage](#request-usage) |
| `host.model.execute_stream` | Sends a ping, with `auth_id` pinned to the original account |
| `host.model.stream_read` / `stream_close` | Reads the ping response up to `message_start`, then closes it |
| `host.log` | Writes to the CPA log |

Notes:

- `RequestInterceptRequest`, `RequestCompletion` and `UsageRecord` have no JSON tags, so the host sends Go field names (`Body`, `Metadata` and so on), with `Body` in base64. Decode them with the `pluginapi` types.
- `request.intercept_after` runs synchronously on the request goroutine with no timeout, and CPA calls it again for every account retry. The handler only filters, computes the conversation key and description, and copies the body; a background goroutine saves the conversations. When the request ends a round of pings, it appends one line to the history after releasing the lock; when a new conversation key appears and the same session and agent already have another conversation, it also compares their messages one by one (see [Conversation key and description](#conversation-key-and-description)).
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
| `GET /v0/management/wick/status` | Returns `Manager.Status()` (the config, the conversations kept lit, and the time of the latest event recorded, a ping or a return, `last_event`) plus the plugin version `version`, the enabled flag in the CPA config `enabled`, the config error `config_error`, the data directory `data_dir` and the plugin clock `now`. A conversation's `session` is the full session ID, `key` the first 8 characters of the conversation key, `pings` the pings of this round (since the last request), `last_read` when the cache was last read, `until` when the pings end (the last request plus the duration), `minutes` the minutes set on the page (left out when none), `default_minutes` the configured duration of the conversation, and `total_pings` the pings of the conversation key in the history within the retention, counted from the history by `plugin.status` (`Store.CountByKey`, without sorting); a ping in flight is not in the history yet, so the total is never below the count of the round |
| `GET /v0/management/wick/history` | `range` is `24h` (default), `7d`, `1m` (from the same time a month ago) or `all` (everything within the retention), and `limit` is the number of conversations returned (default 20, at most 5000). Returns the price state `prices`, the number of conversations `count`, and the `limit` conversations pinged last, `conversations`, newest first. Each conversation is a `history.Conversation`, which holds a `history.Totals` (see the next row) and also has the times of the first and last ping, the context size of the last ping `context`, the error and stop reason of the last ping, the description taken from the last ping, and the rounds `detail`, newest first: start and end, model, pings, context, cache read, result, the time of the return and the idle seconds, cost and amount saved. `prices` is a `pricing.State`: when the prices in use were fetched `fetched`, the source, the number of models, and the error of the last failed fetch `error` |
| `GET /v0/management/wick/savings` | `range` as above. Returns the totals `totals`, the comparison `compare` and the price state `prices`. `totals` is a `history.Totals`: pings, failures, cache read and written in total, rounds `rounds`, rounds by result `results`, cost `cost`, amount saved `saved`, net `net` and rounds without a price `unpriced`, worked out as described in [Rounds, cost and savings](#rounds-cost-and-savings). `compare` is a `history.Comparison`: the time of the first request with recorded usage `since`, the requests `requests`, what they paid for cache reads and writes `cache`, the cost of the pings `pings`, the amount saved `saved`, the cost with a 1-hour cache `hour`, and the requests and rounds without a price `unpriced`, worked out as described in [Cost comparison](#cost-comparison) |
| `POST /v0/management/wick/duration` | Takes `{"key": "<first 8 characters of the conversation key>", "minutes": <minutes>}` with `minutes` from 0 to 1440, where 0 returns to the default time; the time counts from the conversation's last request. Returns the new status; returns 400 with `the duration has already passed`, leaving the conversation unchanged, when the cache already lasts the chosen time (returning to the default included); returns 404 when no conversation matches |
| `POST /v0/management/wick/stop` | Takes `{"key": "<first 8 characters of the conversation key>"}`, snuffs out that conversation and returns the new status; returns 404 when no conversation matches |
| `GET /v0/resource/plugins/wick/page` | The management page, listed as Wick in the Management Center menu |

How the page works:

- The page runs in a same-origin iframe of the Management Center. It takes the management key from the Management Center's `localStorage` (`cli-proxy-auth`, XOR-obfuscated with a key derived from the host and User-Agent); without one it asks the user and keeps the key in `sessionStorage` only. The theme and language come from the `data-theme` and `lang` attributes of the parent page's root element, or from the Management Center settings in `localStorage` when the page is opened on its own. This part matches Lamplighter's management page.
- Settings are read and written through CPA's `GET` / `PATCH /v0/management/plugins/wick/config`. CPA shallow-merges the top-level keys, writes `config.yaml`, then hot-reloads and calls `plugin.reconfigure`. CPA does not validate plugin config, so the page checks the interval range, the retention and whole numbers before saving; with an invalid config the plugin uses the defaults and reports `config_error` in the status.
- The default time and the subagent time share the presets in minutes `PRESETS` in `page.js`, for `duration_minutes` and `subagent_duration_minutes`; the elements of both rows are in `DURATIONS`.
- The per-conversation presets are in `DURATION_PRESETS`. The menu lists them in order of length with the default time (`default_minutes` in the status) among them, merged into one entry when it equals a preset. Times the cache already lasts are disabled, except the one selected; the limit `minutesCovered` is `last_read` minus `last_request` plus 5 minutes. Custom minutes are checked the same way.
- The page refreshes the status every 5 seconds and pauses while the tab is hidden. Idle times and countdowns update locally every second, corrected for the offset from the plugin clock using `now` in the status. The history and the cost comparison are reloaded only when `last_event` in the status changes, or when the earliest pending round gets its result (300 seconds after its last ping; the server settles it by time, with no new event). Request usage does not change `last_event`, so the cost comparison is also reloaded every 60 seconds. The cost comparison and the history each have their own range buttons. A status refresh does not rebuild the history table, so its focus, text selection and scroll position stay.
- While the user is typing in the custom duration field, or has touched a conversation's duration menu in the last 15 seconds, a refresh does not rebuild the conversation table, which would close the menu. Arrow keys on a closed menu fire `change` at every step, so a change made with the keyboard is committed on Enter or when the menu loses focus.
- A conversation is shown by its client and first prompt; the full first prompt, project directory, session ID and conversation key are in the tooltip. The entrypoint-to-client names are in `CLIENTS` in `page.js`; entrypoints not listed are shown as sent.
- The idle time and the countdown are both shown as `m:ss`, or `h:mm:ss` from an hour on.
- The context size is the whole input of a ping: the sum of `cache_read_input_tokens`, `cache_creation_input_tokens` and `input_tokens`. The conversations kept lit use `last_usage` from the status, the history uses `context`.
- A history row opens its rounds when clicked, or with Enter or Space. The open rows are kept by conversation key in the page and stay open across refreshes. The page only renders the rounds and amounts the route returns and calculates nothing itself; without prices (`prices.fetched` empty) it shows no amounts, and a conversation shows a cost only when it has a round with a result and a price. Rounds with the result `unknown` get no badge. The three rows of the cost comparison are sums of the fields of `compare`; without `since` the table is hidden.
- The thumb of the segmented buttons (default time, subagent time, both ranges) is placed by `placeThumb`, which measures the pressed button's `offsetLeft`, `offsetTop`, width and height and writes them to CSS variables. The buttons differ in width and may wrap, so the position is not computed from an index. On a click the thumb slides there with an overshooting transition and plays the squash animation `seg-squash`; on the first layout, a language switch, a resize or when the page turns visible, a ResizeObserver places it at once without animation. A placement that changes nothing is skipped, so that it cannot cut a slide short. With `prefers-reduced-motion` the transitions and animation are off. The look and curves match the mode switches of Lamplighter; each project has its own copy.
- The icon next to the title is `internal/web/logo.png` (64 px), inlined in the page as a data URI. The repository is private, so the page cannot load the repository's files from GitHub.

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

The key covers the tools, the system prompt and the model. When a conversation loads MCP servers or deferred tools midway (such as WebFetch through ToolSearch), changes its system prompt, or switches model with `/model`, its later requests get a new key. The cache matches prefixes in the order tools → system → messages, so the old key's cache can never be read again and further pings only waste money. When a new key appears, `replaceLocked` therefore checks the other conversations of the same session and agent ID (both empty counts as the same): if the old request's messages, all but the last, open the new request's messages unchanged (compared as raw JSON), the new request continues the old conversation, which is deleted with the stop reason `replaced` in the log. The last message is not compared because Claude Code moves the cache marker to the new last message. An old conversation of one message cannot be compared and is kept. The session ID alone is not enough: Claude Code's background helper requests carry the same session ID but do not continue the conversation. Messages are parsed only when the same session and agent already have another conversation, so ordinary requests are not slowed down.

At most 32 conversations are kept in memory; beyond that, the one with the oldest last request is deleted, with conversations that have a time set deleted last.

The description on the page is taken when the request is recorded (`describe.go`):

- Client: the entrypoint in the User-Agent `claude-cli/<version> (<user type>, <entrypoint>, ...)`, such as `cli`, `claude-desktop`, `claude-vscode` or `sdk-ts`.

- Project directory: the path on the first `Primary working directory:` or `Working directory:` line, that is, the directory the session started in. Recent Claude Code versions send the environment as a message with role `system` in `messages`, older ones in `system`; both are searched, `messages` only in its first 20 entries.
- First prompt: the first user text in the first 20 messages. `<system-reminder>` blocks are removed first; local command output (`<local-command-…>`, `<bash-…>`) and the `Caveat: …` note Claude Code adds are skipped; a slash command is shown as `<command-name>` plus `<command-args>`. Whitespace collapses to single spaces; the status keeps up to 200 characters and the history up to 80.

## Scheduling

- When a conversation receives a request, the ping is scheduled at a uniformly random moment between `interval_min` and `interval_max` seconds after the request started. The cache timer starts when the upstream receives the request, so the starting point is the request start, not the end of the response; with a long response, the first ping may go out before the response finishes.
- After each successful ping, the next one is scheduled the same way from the start of that ping.
- Each conversation is pinged until `untilLocked`: the start of its last request plus its duration. The duration is the `minutes` set on the page or, without one, the configured one from `defaultMinutesLocked`: `subagent_duration_minutes` for subagents, which carry an agent ID, and `duration_minutes` for the rest; with `duration_minutes` at 0 subagents are not pinged either. A conversation is snuffed out when 300 seconds (the cache lifetime) after the start of a ping is not earlier than that time (`doneLocked`), so the cache lasts at least until then. The time is worked out from the current config when a ping returns; after a config change, conversations whose cache already lasts their new time are snuffed out at once, and restoring saved conversations skips them too.
- With a duration of 5 minutes or less, the cache of the request itself is enough: the conversation is deleted right after the request is recorded, without pings.
- The manager keeps no total: a conversation is deleted when it is snuffed out and is new when seen again, so the status route counts the total from the history, which neither snuffing out nor a restart loses.
- A new request restarts the count of the round and clears a time set for the conversation. `SetDuration` refuses a time that would snuff out the conversation at once (one the cache already lasts, returning to the default included) with `ErrPassed`, so only Snuff out snuffs out a conversation.
- Every schedule takes a generation (`gen`) from a counter shared by the whole manager. The timer callback and the ping result both check it, so stale timers and results do not affect the new schedule, and a ping still in flight cannot match a conversation that was deleted and then seen again.
- A duration of 0 works the same way: setting `duration_minutes` to 0 clears all conversations, and setting `subagent_duration_minutes` to 0 deletes all subagents. Requests whose duration is 0 are not recorded.
- Before each ping the plugin reads the CPA config file and deletes the conversation if the plugin is disabled. CPA v8.0.39 unloads a disabled plugin (the same as `plugin.shutdown`, below); the check covers the gap before unloading and CPA versions that keep the instance.
- `plugin.quiesce` and `plugin.shutdown` save the conversations, then stop all timers, clear the conversations in memory and close ping streams in progress; the next instance restores what was saved. A later `plugin.reconfigure` resumes and restores the conversations from disk again. Re-enabling a disabled plugin before the cache expires also restores the saved conversations.
- Snuffing out a conversation from the management page deletes it. A ping in flight still finishes and, finding no conversation when it returns, schedules nothing.
- When a conversation with pings in its round is deleted for reaching the end of its time, a config change, snuffing out by hand or eviction, `retireLocked` keeps the round in `ended` until 300 seconds after the cache was last read, so that a return in that time is judged. Conversations deleted after a missed or failed ping, a disabled plugin, a finished subagent or a new key are not kept. `ended` is not saved to disk, so a restart or hot update loses it, and returns in that time are not recorded.
- Timer callbacks run on their own goroutines, where an unrecovered panic would exit the CPA process, so they use `recover`.

## Rounds, cost and savings

### Returns

When a conversation with pings in its round (kept lit or in `ended`) receives a request, `Manager.Observe` writes an event with `kind` `return` to the history, recording `saved`, the seconds since the previous request `idle`, the input of the round's last successful ping `context`, and the pings of the round `pings`, with `ping` 0. `saved` holds when the last successful cache read (`lastRead`) was less than 300 seconds ago and the previous request (`lastRequest`) at least 300 seconds ago: the first means the pings kept the cache alive, the second that without them it would have expired.

### Rebuilding rounds

`history.Group` rebuilds the rounds from the history; rounds are not written to the files, so older history can be split too. The events of one conversation key are ordered by time, and a new round starts with a `ping` number no higher than the previous one, or after a return; a return ends the current round. A return without a ping of its round (for example, with the pings outside the range) is left out. `request` events take no part in the rounds.

A round's result, `Round.Result`, is decided in this order:

1. The round's last ping failed or wrote to the cache: `failed` or `miss`.
2. The round has a return: `saved` if its `saved` is true, otherwise `wasted`.
3. The last ping has no `returns` flag, so a version before v0.5.0 wrote it: `unknown`, shown without amounts.
4. The last successful ping was less than 300 seconds ago: `pending`.
5. Otherwise `wasted`: with no return within 300 seconds the result is settled without waiting. A conversation deleted for a new key never gets a return, so it counts as `wasted` here.

From v0.5.0 every event carries `returns: true`, which tells "did not come back" apart from "the version did not record returns".

### Amounts

Amounts are calculated at the current prices when the history is queried and are not written to the events:

- A ping costs `cache_read × read price + cache_creation × 5-minute write price + input × input price`. The ping disconnects at `message_start`, so output counts as 0. Prices with tiers use the tier of the ping's total input.
- A `saved` round saves the return's `context` × (5-minute write price − read price).
- `cost` and `saved` in the totals include `pending` rounds; `net` is `saved` less the cost of rounds with a result, leaving out `pending` rounds. `unknown` rounds and rounds whose model has no price are left out of all three, and the latter are counted in `unpriced`.

### Request usage

The cost comparison needs the cache tokens each request read and wrote, but the plugin only intercepts requests and does not see responses. So the plugin registers `usage_plugin`, and CPA sends a usage record through `usage.handle` after each request completes (`internal/keepalive/usage.go`):

- When `Observe` records a request (only for conversations Wick keeps lit), `awaitLocked` keeps a `request` event waiting for its usage under the trace ID of the inbound HTTP request, or the request ID without one: CPA gives each upstream execution its own request ID in usage records, different from the one at interception. CPA calls the interceptor for every attempt of a request, such as a retry with another account, under the same trace ID; only the first attempt's event is kept, so that a retry cannot reset the idle time. When the previous request came less than an hour before, the event holds the seconds since it, `idle`, and its input, `context`. The previous request is kept in `seen`; without an entry there, such as after a hot update, the last request time of the conversation kept lit is used, with the usage of its last ping when it was pinged in this round.
- `Usage` looks up the request of a usage record by the same key, adds `cache_read_input_tokens`, `cache_creation_input_tokens` and `input_tokens`, writes the event to the history, and keeps the total input as the input of the conversation's previous request. A `Failed` record is ignored and the request keeps waiting, so a retry under the same trace still matches. Records without a match, including pings and other clients' requests, are ignored.
- Requests waiting for more than an hour and conversations in `seen` idle for more than an hour are dropped; each keeps at most 1000 entries, and neither is saved to disk.

### Cost comparison

`history.Compare` works out a `Comparison` from the events of a range and the result of `Group`. Amounts use the prices current at query time, with the tier picked by the request's total input:

- `cache`: `cache_read × read price + cache_creation × 5-minute write price` of each `request` event.
- `hour`: what the same request would pay with a 1-hour cache. It reads what it read, and the rest of its cached part is priced at the 1-hour write rate. With `idle` of at least 300 seconds and less read than `context`, the 5-minute cache had expired while a 1-hour one would hold on, so the read becomes the smaller of `context` and the whole cached part. For a `saved` round with `idle` of at least 3600 seconds, the 1-hour cache would have expired, adding `context` × (1-hour write price − read price).
- `pings` and `saved`: the cost and amount saved of the rounds that end (at their return, or their last ping without one) no earlier than `since`, with the cost of `pending` rounds and without `unknown` rounds.
- `since` is the time of the first `request` event with usage in the range; rounds that ended before it are left out, so the three approaches cover the same time. Requests and rounds whose model has no price count in `unpriced`.
- The three rows on the page: no pings is `cache + saved`, Wick is `cache + pings`, the 1-hour cache is `hour`. Uncached input and output cost the same in all three and are left out.

### API prices

`internal/pricing` reads the Markdown version of the official pricing page, `https://platform.claude.com/docs/en/about-claude/pricing.md`:

- It parses the first table under `## Model pricing`, finding the Base input, 5m cache writes, 1h cache writes, Cache hits and refreshes, and Output columns by their headers; prices are written as `$x / MTok`.
- Notes in parentheses after a model name, such as "([limited availability](…))", are removed. A note such as "(for prompts up to / over 100,000 tokens)" makes a price tier, chosen by the total input (cache reads, cache writes and uncached input). The display name is split into a family and version: "Claude Opus 5.5" has family `opus`, version `5.5` and ID `claude-opus-5-5`.
- To look up a price, CPA's model name is lowercased, stripped of a bracketed suffix (such as `[1m]`), and split into words and a version. The version is the first run of one- or two-digit numbers; dates and numbers after it (such as `-v1:0` at the end of a Bedrock ID) are not part of it. Trailing `.0` parts are dropped from versions in the table and in model names, so `claude-opus-4-0` is Claude Opus 4. A model matches when the version is the same and the words include every word of the family, so suffixes such as `-thinking` and the old word order (such as `claude-3-5-haiku`) match. Models that match nothing get no amounts.
- Only the model pricing table is parsed. Claude Sonnet 4 and 4.5 prompts over 200K tokens are billed at long context prices; that premium is not counted, so their amounts come out low.
- The prices are fetched when the plugin loads, every 24 hours after a success, and an hour after a failure, with standard `net/http` and a 15-second timeout. A good fetch is written to `prices.json` in the data directory with its time, and the plugin reads that file when it loads. A failed fetch or parse keeps the prices in use and is reported in the log and on the page. A parse that finds fewer than 5 models counts as a failure, so that a redesigned page cannot replace good prices.
- `plugin.quiesce` and `plugin.shutdown` stop the fetching; a later `plugin.reconfigure` starts it again with an immediate fetch.

## Data directory

The data directory is the `data_dir` setting, or `<plugins.dir>/data/wick` when it is empty (`config.DataDir`). `plugins.dir` is read from the CPA config file, and relative paths are relative to the CPA working directory; CPA loads plugins from copies in the temp directory, so the path of the loaded library cannot be used. Directories have mode `0700` and files `0600`.

### Conversations kept lit

`internal/keepalive/persist.go` saves the conversations in memory under `conversations/`:

- `index.json`: for each conversation, the key, first-seen time, last request time, the last time the cache was read `last_read`, next ping time, the ping count of this round, the minutes set for it, and the last usage.
- `<conversation key>.req`: the first line is the request metadata (session ID, agent ID, model, account, request ID, directory, first prompt, headers) as JSON, followed by the request body unchanged. The body is not JSON-encoded, so the replay matches the original request byte for byte.

When a conversation changes (new request, ping, duration set, removal), the manager signals a background goroutine, which waits 5 seconds to batch the changes, takes a snapshot under the lock and writes the files outside it. A request file is rewritten only when its conversation's generation changes; other changes rewrite only `index.json`. Files are written to `.tmp` and renamed; files not in the snapshot are deleted; a conversation key must be 64 hexadecimal characters or it is not read.

`plugin.quiesce` and `plugin.shutdown` take a last snapshot before clearing the conversations, release the lock, make the goroutine exit and wait for it, then write that snapshot synchronously. On a hot update CPA quiesces the old instance before loading the new one, so the new instance reads the state the old one stopped with. Requests CPA receives after quiescing the old instance and before switching to the new one are recorded by neither; such a conversation replays its previous turn after the restore until it receives its next request.

After `plugin.register` or `plugin.reconfigure` sets the directory, the manager reads the saved conversations the first time the directory is set after the instance starts or resumes:

- The next ping is no earlier than 3 seconds after loading, which leaves CPA time to finish loading the plugin, and no later than 290 seconds after `last_read` (the cache lifetime less 10 seconds for the request to reach upstream); with `interval_max` above 290 the saved time can be later.
- If 3 seconds after loading is already later than 290 seconds after `last_read`, the cache counts as expired and the conversation is not restored; its files are deleted on the next write.
- A restored conversation takes a new generation and keeps its count, the minutes set for it and the last request time.

### Ping history

`internal/history` appends the `keepalive.Event` of every ping, every return and the usage of every request to `history/<UTC date>.jsonl`, one JSON object per line, each written whole with one `O_APPEND` write, so two instances overlapping briefly during a hot update cannot corrupt a file. The first write of each day deletes files older than `history_retention_days` days. Queries read the files day by day and cache the parsed result by file size and modification time, so unchanged files are not parsed again.

The history route passes the query result to `history.Group`, which sums it by conversation key (the first 8 characters) and splits it into rounds, so a conversation appears once per range; entries without a conversation key are summed by session ID. The total in the status (`Store.CountByKey`) and the sums count pings only, not returns or requests.

### API prices

`prices.json` is the price table last fetched (`pricing.Table`): the time of the fetch, the source, and the name, ID, family, version and price tiers of each model.

## Development

### Environment

- Go 1.26.
- Building the library needs cgo and a C compiler for the target platform. The packages under `internal/` are pure Go, so tests do not need cgo.

### Tests

```bash
go vet ./internal/...
go test ./internal/...
```

The tests use a fake host and clock; they send no real requests and do not wait in real time. The persistence tests call the write function directly in a temp directory instead of waiting for the background goroutine. They cover the request filter and ping body (including removal of the client credentials), conversation keys, extraction of the project directory and first prompt, ping intervals and stopping at the end of the time, no pings for 5 minutes or less, the subagent time (including a time set and `subagent_duration_minutes: 0`), the total counted from the history, rescheduling on a new request, snuffing out on a cache miss or error, removal of subagents (including parallel subagents with agent IDs), failed and interrupted requests, disabling the plugin and `duration_minutes: 0`, stale ping results, the deadline and clearing of a time set for a conversation, refusing a time already passed, sparing such conversations on eviction, saving and restoring conversations (schedule kept, overdue ping sent soon, late ping brought forward, expired cache not restored, the last state before stopping, files deleted), writing, querying, summing by conversation and pruning the ping history, SSE parsing, log messages, config validation and the data directory, snuffing out a conversation by hand, plugin RPC registration and decoding of intercepted requests, the management API and page, snuffing out the old conversation after a tool change (with helper requests and other agents left alone), judging returns (in time, too early, too late, after snuffing out by hand, none after a new key), rebuilding rounds with their results and amounts, recording request usage (matching by trace ID and request ID, retries after a failure, more than an hour) and decoding `usage.handle`, the cost comparison, and parsing the pricing page (with the official page of 2026-10-24 in `internal/pricing/testdata/pricing.md`), matching model names and caching prices.

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
- When changing the fields of `request` events or how the 1-hour cache is priced, update `history.Compare` and the Cost and savings section of the README.
- New goroutines must use `recover`.
- When the pricing page changes its table layout, replace `internal/pricing/testdata/pricing.md` with the new page before changing the parser.
- When replacing `assets/logo.png`, also generate the 64 px `internal/web/logo.png`; the page shows the new icon only after a release.
- User-visible behavior, configuration and limits go in the README (Chinese and English); implementation details go in this document (Chinese and English).
