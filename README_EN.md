<p align="center"><img src="./assets/logo.png" alt="" width="128"></p>

# Wick

[中文](./README.md)

> Drunk, I trim the lamp to look at my sword; in dreams, I return to the camps where bugles sound. — Xin Qiji
>
> The Chinese name, 挑灯 (tiǎo dēng), means trimming the wick of an oil lamp: when a lamp has burned for a while and the flame grows dim, a light touch to the wick with a hairpin makes it bright again. Wick makes the same small move: when a conversation is idle and its cache is about to expire, it sends the last request once more and the cache stays alive.
>
> - [Lamplighter](https://github.com/PosvdM/cpa-plugin-lamplighter): lights the next quota window on time
> - Wick: watches over the conversation in progress so that its lamp does not go out

Wick is a native plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA). When native Claude Code uses a Claude subscription account through CPA's Claude Messages endpoint, Wick reads the 5-minute prompt cache on a schedule while a conversation is idle, so the next message still hits the existing cache. Pings for pi and other clients are off by default; see [Client limitations](#client-limitations) for compatibility requirements. Each read is called a ping; stopping the pings is called snuffing out.

![Management page: the conversations kept lit, the ping history by conversation and round, and the settings](./docs/images/overview_EN.png)

## Features

- **Keeps the cache**: once a conversation is idle, Wick pings every 4 min 20 s to 4 min 50 s, keeping the cache until it has been idle for 30 minutes by default. The default can be 15 min, 30 min, 1 hour or a custom number of minutes; subagents have their own setting, 15 min by default, which can also be off; one conversation can be given its own time, up to 24 hours.
- **Leaves the conversation alone**: a ping resends the conversation's last request unchanged and disconnects at the first event. The result never reaches the client, and the conversation context stays the same.
- **Snuffs out by itself**: pings stop when the time is up, when a ping fails, when no cache is read, or when any cache is written, so uncached input or cache writes are not paid for repeatedly. When a conversation changes its tools, system prompt or model midway, its old cache can no longer be used, and the old pings stop.
- **Counts the cost**: at the API prices on Anthropic's pricing page, Wick works out what each round of pings cost and the cache write saved when you came back in time, and compares the cache cost with no pings and with a 1-hour cache. Prices are updated daily, so new models are priced without a plugin update.
- **Management page**: in the CPA Management Center, see the conversations kept lit with their context size, pings this round and countdowns, compare the cache cost of each approach, browse the ping history by conversation and round, and change the settings.
- **Survives restarts**: the conversations kept lit are saved to disk and continue on schedule after CPA restarts or reloads the plugin.

## Purpose

Claude's prompt cache has two lifetimes: a 5-minute write costs 1.25× the input price and a 1-hour write costs 2×; a cache read costs 0.1×. Every read restarts the cache's timer. Native Claude Code uses the 5-minute cache by default, so after more than 5 minutes away, the next message writes the whole context again at 1.25×. With a context of a few hundred thousand tokens, that one rewrite uses a noticeable share of the quota.

Wick keeps normal writes at the 5-minute price and reads the cache only while a conversation is idle. Ping reads also count against the subscription quota: each ping reads the whole context at 0.1×, so if you come back within one or two ping intervals, the savings are small.

## How it works

- Wick records the last request of each conversation. Counting from the start of that request, it picks a random moment between `interval_min` and `interval_max` seconds and sends the same request again unchanged, disconnecting as soon as the first event, `message_start`, arrives, as if Esc were pressed right after sending. By `message_start` the cache has been read and its lifetime restarted; the upstream may generate a few tokens before the disconnect.
- A ping goes through CPA's model executor with the original request's account, headers and caller identity, so CPA gives the tools the same names as in the original request. Whether the ping hits the original cache also depends on the conditions in [Client limitations](#client-limitations).
- Pings continue until the cache lasts `duration_minutes` minutes past the last request, or `subagent_duration_minutes` for subagents; the management page can give one conversation its own time. A new request in the conversation restarts the timer from that request and starts a new round: the count of the round restarts and a time set for the conversation returns to the default.
- Wick snuffs out a conversation when the cache lasts until the end of its time, when a ping fails, when a ping reads no cache or writes any cache (including partial hits with writes), when the original request fails, or when the plugin is disabled. Requests interrupted with Esc keep being pinged.
- When a conversation loads new tools midway (such as MCP servers or deferred tools), or changes its system prompt or model, Wick sees its requests as a new conversation. The cache is matched piece by piece from the tools on, so the old conversation's cache can no longer be read. When the new request continues or rewinds and resends the old conversation (diverging at user input), Wick snuffs out the old one; when a main conversation is replaced this way, subagents started while it was waiting are snuffed out as well. Moving cache markers between messages does not affect this check; other message content must still match. Requests diverging at the first message leave the old conversation active. See [Conversation key and description](./docs/architecture.md#对话识别和描述) (in Chinese) for details.
- Wick handles streaming Claude Messages requests and takes over only native Claude Code by default; other clients such as pi require manually enabling `other_clients`. Native Claude Code bodies stay unchanged; see [Client limitations](#client-limitations) for other clients' eligibility. OpenAI-format requests, explicit 1-hour cache requests and bodies below `min_body_kb` (such as title generation) are skipped. Turning off `other_clients` stops their existing pings and prevents restoring them from disk. Subsequent requests pass unchanged to CPA, whose default OAuth cache policy uses a 1-hour cache; CPA still handles caller-supplied explicit cache settings.
- Within one session, the main conversation and each subagent are pinged separately; a request with an agent ID counts as a subagent. When the main conversation sends its next request, subagents that started while it was waiting are treated as finished and snuffed out, so a subagent is pinged only when it has been idle longer than the shortest interval and the main conversation has not continued. Clients that use different keys for CPA are kept apart even when they send the same session ID.
- After CPA restarts or reloads the plugin, conversations whose cache has not expired continue on their original schedule, keeping the pings of the round and the times set for them; conversations whose cache has expired are not restored.

## Cost and savings

The pings after a request, up to the conversation's next request, make up a round. Each round ends with one of these results:

- **Saved**: you came back more than 5 minutes after the previous request, when the cache would have expired without pings, and less than 5 minutes after the last successful ping, while the cache was alive. The amount saved is the context priced at the difference between the 5-minute write price and the read price.
- **Wasted**: you did not come back within 5 minutes of the last successful ping; or you came back less than 5 minutes after the previous request, when the cache was alive without pings. A conversation that changed its tools, system prompt or model midway is also wasted.
- **Lit**: the last successful ping was less than 5 minutes ago, and there is no result yet.
- **Miss** and **Failed**: the round has no return event and its last ping read no cache, wrote any cache or failed. An in-flight ping finishing after a return does not override a settled Saved or Wasted result.

A ping costs its cache reads, cache writes and uncached input tokens at API prices; output tokens are not counted. At Claude Opus 5.5 prices, the write saved in one round equals about 24 pings, and a round has about 6 pings with the default time, so whether Wick saves or costs mostly depends on whether you come back while the cache is kept.

Wick also takes the cache reads and writes of every request of these conversations from CPA's usage records, and prices the cache three ways:

- **No pings**: what the requests paid for cache reads and writes, plus the writes the pings saved.
- **Wick**: what the requests paid for cache reads and writes, plus the cost of the pings.
- **1-hour cache**: what the same requests would pay with a 1-hour cache and no pings. Writes are priced at the 1-hour rate; a request 5 minutes to an hour after the previous one, when the 5-minute cache had expired, reads the previous request's input instead; a request that came back more than an hour later to a cache kept lit by Wick writes the context again.

Uncached input and output cost the same in all three and are left out. The comparison covers the conversations Wick keeps lit and starts from the first request with recorded usage in the range; counts, rounds and results use that same coverage. When coverage starts later than the selected range, the page shows the actual starting date and time.

Prices come from [Anthropic's pricing page](https://platform.claude.com/docs/en/about-claude/pricing). The plugin fetches them when it loads and daily after that, and keeps the last prices when a fetch fails; models missing from the page get no amounts. Amounts are calculated at the prices current when the history is viewed, so past rounds follow price changes. Subscription usage, such as Pro, does not map exactly to API prices, so take the amounts as proportions.

## Requirements

- CPA v8.0.43 or later with plugins enabled (`plugins.enabled: true`).
- A client using a Claude Messages model with prompt caching through CPA and sending streaming requests. Native Claude Code can use a Claude subscription account; other clients must also meet the conditions below, and `other_clients` defaults to `false`.

### Client limitations

- Only Claude Messages is supported, not OpenAI formats. Eligibility depends on the request format and cache markers, not just the client name.
- `other_clients` is off by default; turn it on when the conditions below hold. When CPA forwards another client's request with a Claude subscription account, it may rename the tools based on the key the client uses for CPA. A ping carries the original request's caller identity (a hash CPA computes from that key, without the key itself), so CPA gives the tools the same names and the ping reads the original cache. Native Claude Code requests are not renamed. After turning it on, check that pings read cache tokens and write none. Implementation details are in [Tool aliases and replay limits](./docs/architecture.md#工具别名与重放限制) (in Chinese).
- Other clients must supply 1–4 legal `cache_control` markers, all without `ttl`. `prompt_cache_options` must be absent or an object, with `mode` absent or `explicit`. When the request passes the general filters, the configured duration exceeds 5 minutes and Wick is running, Wick adds `ttl: "5m"` to every marker and sets `prompt_cache_options.mode: "explicit"`, preserving the object's other fields. Explicit mode constrains CPA's cache policy, preserving markers and TTLs that remain in the request without adding markers or upgrading to 1 hour. CPA removes this option before sending upstream. Explicit mode cannot prevent strict cloaking (`cloak_strict_mode`) or similar processing from deleting caller system content and its markers. Prefixes below the cache's minimum length may also remain uncached. A ping that reads no cache or writes any cache automatically stops further pings, even when reads and writes coexist. This also applies to native Claude Code. The normal request, persistence and pings use the same rewritten body; see [Request filter](./docs/architecture.md#请求筛选).
- Other-client requests pass through without body changes or pings when any marker already has `ttl` (including `5m`, `1h`, `null` or other values), there are no legal markers or more than 4, another mode is selected, or `prompt_cache_options` is not an object.
- pi-subagents does not send `X-Claude-Code-Agent-Id`. Wick treats those requests as main conversations and uses the default time (`duration_minutes`), not the subagent time.
- Other clients must send a stable explicit session identifier: one of `X-Claude-Code-Session-Id`, `metadata.user_id`, `X-Session-Affinity`, `X-Session-Id` or `Session-Id`. Wick selects valid identifiers under CPA's rules, rejecting controls, empty trimmed values and values longer than 256 bytes after trimming; Claude-format `metadata.user_id` first supplies its extracted session ID. With no valid identifier, Wick passes the request through without body changes or pings; see [Conversation key and description](./docs/architecture.md#对话识别和描述) for source precedence. For pi, enable `compat.sendSessionAffinityHeaders`; pi-endpoints 0.2.13 and later send `x-session-affinity` by default for Anthropic models.

## Installation

1. Download the zip for the system CPA runs on from [Releases](https://github.com/PosvdM/cpa-plugin-wick/releases), or build it as described in the [architecture document](./docs/architecture.md#构建).
2. Put the library (`wick-v<version>.so` on Linux) in the `<os>/<arch>/` subdirectory of the CPA plugin directory, for example `plugins/linux/arm64/`. With Docker, the plugin directory is the host directory mounted at `/CLIProxyAPI/plugins` in the container.
3. Enable the plugin in CPA's `config.yaml`:

   ```yaml
   plugins:
     enabled: true
     configs:
       wick:
         enabled: true
   ```

CPA loads the plugin when it detects the config change; no restart is needed.

## Configuration

Settings live under `plugins.configs.wick` in `config.yaml` and take effect when saved. They can also be changed on the [management page](#management-page).

| Key | Default | Description |
| --- | --- | --- |
| `interval_min` | `260` | Shortest interval between pings in seconds |
| `interval_max` | `290` | Longest interval between pings in seconds, at most `299` |
| `duration_minutes` | `30` | Minutes an idle conversation is kept lit, `0` to `1440`: pings continue until the cache lasts that long past the last request; `0` turns Wick off, subagents included |
| `subagent_duration_minutes` | `15` | Minutes an idle subagent is kept lit, `0` to `1440`; `0` leaves subagents out |
| `other_clients` | `false` | Apply a 5-minute cache and explicit mode to [eligible](#client-limitations) other-client Claude Messages requests and keep them lit; turning this off stops their pings and passes subsequent requests unchanged to CPA |
| `min_body_kb` | `64` | Skip requests whose body is smaller than this many KiB |
| `history_retention_days` | `40` | Days of ping history to keep, `1` to `3650` |
| `data_dir` | empty | Data directory. Empty means `<plugins.dir>/data/wick`; a relative path is relative to the CPA working directory |

The cache Wick manages lasts 5 minutes, so a time of 5 minutes or less sends no pings; those other-client requests also keep their original bodies. The cache lifetime counts from when Anthropic receives the request, network latency included, so leave a margin between `interval_max` and 300 seconds. With an invalid config the plugin uses the defaults and writes a warning to the CPA log.

## Management page

Open **Wick** from the plugin menu of the CPA Management Center. The page uses the management key the Management Center remembered at sign-in, or asks for one; the theme and language follow the Management Center.

- **Conversations kept lit**: each conversation shows its client (Claude Code, Claude Desktop, VS Code and so on, taken from the request's User-Agent) and its first prompt, with subagents tagged Subagent; hovering shows the full first prompt, the project directory and the session ID. **Context** is the input tokens of the last ping; **This round** gives the pings since the last request, with the conversation's total in the ping history below it, which carries on when a conversation is snuffed out and seen again. The other columns show the model and account, the idle time and a countdown to the next ping. **Keep lit** lists 15 min, 30 min, 1, 2, 4 and 8 hours and the default time, marked default, in order of length, or a custom number of minutes, counted from the conversation's last request; below it the page shows until when. Times the cache already lasts cannot be chosen, so that choosing one cannot snuff out the conversation. **Snuff out** stops pinging that conversation until it receives its next request.
- **Cost comparison**: switches between 24 hours, 7 days, 1 month (from the same time a month ago) and All. The headline shows what Wick saves or costs versus no Wick. The subtitle states the baseline once with “vs no Wick:”, followed by the percentage by which Wick and the 1-hour cache are cheaper or more expensive. Both use the no-Wick cost as the baseline and show one decimal place. Differences that round to zero at this precision are labeled “costs the same”; a zero baseline makes percentages unavailable, and missing data is labeled as unavailable. Three proportional horizontal bars show Wick's current cost, the 1-hour cache and no Wick, with cache, ping and expired-cache rewrite breakdowns. Counts below use the same coverage: pings, rounds, rounds you returned to, wasted rounds and rounds still lit; failures are listed separately. Pending rounds include ping costs but have no settled result. See [Cost and savings](#cost-and-savings) for the calculation. Without recorded request usage or prices, the card shows the ping summary instead; with prices it includes the net saving or cost, excluding pending rounds from the net.
- **Ping history**: switches between 24 hours, 7 days and All. Each conversation takes one row: the time of its last ping, its rounds and pings, the context of its last ping, the cost, the amount saved, and the rounds by result. Clicking a row shows each round's time, context, pings, cache read, cost and result, with the idle time for rounds you came back to. The list starts with 20 conversations and can be expanded.
- **Settings**: **Default time kept lit** offers Off, 15 min, 30 min, 1 hour or Custom, which set `duration_minutes` to `0`, `15`, `30`, `60` or the minutes entered. **Time kept lit for subagents** has the same choices for `subagent_duration_minutes`. **Other clients** controls `other_clients`. **Advanced** applies to every conversation and changes `interval_min`, `interval_max`, `min_body_kb` and `history_retention_days`. Changes are saved to `config.yaml` and applied by the plugin within a few seconds.

The first prompt is the text the user typed in the first user message of the conversation, leaving out the system reminders and local command output that Claude Code adds; a conversation that starts with a slash command shows the command and its arguments.

## Data directory

The data directory is `<plugins.dir>/data/wick` by default, inside the mounted plugin directory with Docker. Files have mode `0600`, readable only by the user running CPA.

| Path | Content |
| --- | --- |
| `history/<UTC date>.jsonl` | Ping history, one line per ping: time, the first 8 characters of the session ID and conversation key, model, client, project directory, the first 80 characters of the first prompt, cache tokens and result. Coming back to a conversation after pings adds a line with whether it saved a write, the idle time and the pings of the round. Each completed request of a conversation kept lit also adds a line with its cache tokens, the time since the previous request and that request's input, for the cost comparison. Files older than `history_retention_days` days are deleted |
| `conversations/` | The conversations kept lit: the last request, the schedule, the pings of the round and the time set for each. A conversation's files are deleted when it is snuffed out |
| `prices.json` | The API prices last fetched and when |

## Logs and API

Each ping writes one line to the CPA log with the first 8 characters of the session ID and conversation key, the model, the ping number within the round, and the cache tokens read and written, for example:

```text
wick: ping session=1a2b3c4d key=5e6f7a8b ping=3 cache_read=322200 cache_creation=0 input=3 output=1 model=claude-opus-5-5
```

`output` is the count in `message_start` and leaves out tokens generated before the disconnect. Coming back to a conversation after pings, snuffing out a conversation continued or rewound under a new key, and a failed price update each write a line too.

The routes the management page uses can also be called directly; all of them require the CPA management key. `GET /v0/management/wick/status` returns the conversations kept lit, `GET /v0/management/wick/history` the ping history summed by conversation, and `GET /v0/management/wick/savings` the summary and the cost comparison; parameters are in the [architecture document](./docs/architecture.md#管理接口和页面):

```bash
curl -H "Authorization: Bearer <management key>" http://127.0.0.1:8317/v0/management/wick/status
```

## Security and risks

- The last request of each conversation kept lit contains the whole conversation. It is kept in memory and in `conversations/` in the data directory, and deleted when the conversation is snuffed out; the ping history keeps the first 80 characters of the first prompt. The key the client uses for CPA (`Authorization`, `X-Api-Key`) is not saved. Conversation content is never written to the logs. The caller identity used for pings is a hash of the client's key, without the key itself. It is saved with the request and never shown on the management page, in the status API, the logs or the ping history.
- A ping is identical to the original request and the intervals are random, but regularly resending the same request and cancelling it may still be noticed by the provider. Using a subscription account through CPA carries account risk in itself; judge for yourself.
- When a ping fails (for example, it is rate limited), CPA puts the account into cooldown as it does for any request, and your next request is rejected until the cooldown ends.
- The plugin fetches the pricing page from `platform.claude.com` once a day; the request carries no account or conversation data.

## Development

Plugin structure, the CPA interface, building and testing are described in the [architecture document](./docs/architecture.md).

## License

[AGPL-3.0-or-later](./LICENSE). If you distribute a modified version, or offer a modified version to others over a network, you must publish its source code under the same license.
