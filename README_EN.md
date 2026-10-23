# Wick

[中文](./README.md)

Wick is a native plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA). When Claude Code uses a Claude subscription account through CPA, Wick refreshes the 5-minute prompt cache while a conversation is idle, so the next message still hits the existing cache.

## Purpose

Claude's prompt cache has two lifetimes: a 5-minute write costs 1.25× the input price and a 1-hour write costs 2×; a cache read costs 0.1×. Every read restarts the cache's timer. Native Claude Code uses the 5-minute cache by default, so after more than 5 minutes away, the next message writes the whole context again at 1.25×. With a context of a few hundred thousand tokens, that one rewrite uses a noticeable share of the quota.

Wick keeps normal writes at the 5-minute price and, while a conversation is idle, reads the cache every 4 min 20 s to 4 min 50 s, up to 12 times in a row, which keeps the cache for about an hour.

## How it works

- Wick records the last request of each conversation. Counting from the start of that request, it picks a random moment between `interval_min` and `interval_max` seconds and sends the request again, non-streaming and with only `max_tokens` set to 0. This request only reads the cache and generates nothing. Its result never reaches Claude Code, so the conversation context stays the same. Anthropic accepts `max_tokens: 0` only on non-streaming requests.
- The refresh goes through CPA's model executor with the original account and request headers, so it hits the same cache as the original request.
- A new request in the conversation restarts the timer from that request and resets the refresh count.
- Refreshing stops when it reaches `max_pings`, when a refresh fails, when a refresh writes to the cache (the cache had expired or the request differs, so further refreshes cannot hit it), when the original request fails, or when the plugin is disabled. Requests interrupted with Esc keep being refreshed.
- Only native Claude Code streaming requests are handled. These requests are not refreshed: requests that use the 1-hour cache; requests with budget-mode thinking (`budget_tokens`), because CPA may replace their `max_tokens: 0` with the default; and requests with a body smaller than `min_body_kb`, such as title generation.
- Within one session, the main conversation and each subagent are refreshed separately. When the main conversation sends its next request, subagents that started while it was waiting are treated as finished and no longer refreshed.

Refresh reads count against the subscription quota. Each refresh reads the whole context at 0.1×; if you come back within one or two refresh intervals, the savings are small.

## Requirements

- CPA v8.0.39 or later with plugins enabled (`plugins.enabled: true`).
- Native Claude Code using a Claude subscription account through CPA. Requests from other clients (pi, Cherry Studio and so on) are ignored.

## Installation

1. Download the zip for the system CPA runs on from [Releases](https://github.com/PosvdM/cpa-plugin-wick/releases), or build it as described in the [architecture document](./docs/architecture_EN.md#build).
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

Settings live under `plugins.configs.wick` in `config.yaml` and take effect when saved.

| Key | Default | Description |
| --- | --- | --- |
| `interval_min` | `260` | Shortest refresh interval in seconds |
| `interval_max` | `290` | Longest refresh interval in seconds, at most `299` |
| `max_pings` | `12` | Refreshes per idle period; `0` turns refreshing off |
| `min_body_kb` | `64` | Skip requests whose body is smaller than this many KiB |

The cache lifetime counts from when Anthropic receives the request, network latency included, so leave a margin between `interval_max` and 300 seconds. With an invalid config the plugin uses the defaults and writes a warning to the CPA log.

## Status

Each refresh writes one line to the CPA log with the first 8 characters of the session ID, the model, the refresh number, the cache tokens read and written, and the output tokens, for example:

```text
wick: ping session=1a2b3c4d key=5e6f7a8b ping=3 cache_read=322200 cache_creation=0 input=3 output=0 model=claude-opus-5-5
```

`GET /v0/management/wick/status` returns the conversations being refreshed, the next refresh times, the results of the last 100 refreshes and running totals. It requires the CPA management key:

```bash
curl -H "Authorization: Bearer <management key>" http://127.0.0.1:8317/v0/management/wick/status
```

## Security and risks

- Conversation content is kept only in memory, never written to disk or logs, and is cleared when CPA restarts or the plugin is unloaded.
- A refresh differs from the original request only in `max_tokens: 0` and not streaming, and the intervals are random, but regularly resending the same request without generating anything may still be noticed by the provider. Using a subscription account through CPA carries account risk in itself; judge for yourself.
- When a refresh fails (for example, it is rate limited), CPA puts the account into cooldown as it does for any request, and your next request is rejected until the cooldown ends.

## Development

Plugin structure, the CPA interface, building and testing are described in the [architecture document](./docs/architecture_EN.md).

## License

[AGPL-3.0-or-later](./LICENSE). If you distribute a modified version, or offer a modified version to others over a network, you must publish its source code under the same license.
