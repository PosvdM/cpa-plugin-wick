# Wick

[中文](./README.md)

Wick is a native plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA). When Claude Code uses a Claude subscription account through CPA, Wick refreshes the 5-minute prompt cache while a conversation is idle, so the next message still hits the existing cache.

## Purpose

Claude's prompt cache has two lifetimes: a 5-minute write costs 1.25× the input price and a 1-hour write costs 2×; a cache read costs 0.1×. Every read restarts the cache's timer. Native Claude Code uses the 5-minute cache by default, so after more than 5 minutes away, the next message writes the whole context again at 1.25×. With a context of a few hundred thousand tokens, that one rewrite uses a noticeable share of the quota.

Wick keeps normal writes at the 5-minute price and, while a conversation is idle, reads the cache every 4 min 20 s to 4 min 50 s. By default it reads up to 6 times in a row, which keeps the cache for about 30 minutes; the duration can be chosen on the [management page](#management-page).

## How it works

- Wick records the last request of each conversation. Counting from the start of that request, it picks a random moment between `interval_min` and `interval_max` seconds and sends the same request again unchanged, disconnecting as soon as the first event, `message_start`, arrives, as if Esc were pressed right after sending. By `message_start` the cache has been read and its lifetime restarted. The upstream may generate a few tokens before the disconnect. The result never reaches Claude Code, so the conversation context stays the same.
- The refresh goes through CPA's model executor with the original account and request headers, so it hits the same cache as the original request.
- A new request in the conversation restarts the timer from that request and resets the refresh count.
- Refreshing stops when it reaches `max_pings`, when a refresh fails, when a refresh writes to the cache (the cache had expired or the request differs, so further refreshes cannot hit it), when the original request fails, or when the plugin is disabled. Requests interrupted with Esc keep being refreshed.
- Only native Claude Code streaming requests are handled. These requests are not refreshed: requests that use the 1-hour cache; and requests with a body smaller than `min_body_kb`, such as title generation.
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

Settings live under `plugins.configs.wick` in `config.yaml` and take effect when saved. They can also be changed on the [management page](#management-page).

| Key | Default | Description |
| --- | --- | --- |
| `interval_min` | `260` | Shortest refresh interval in seconds |
| `interval_max` | `290` | Longest refresh interval in seconds, at most `299` |
| `max_pings` | `6` | Refreshes per idle period; `0` turns refreshing off |
| `min_body_kb` | `64` | Skip requests whose body is smaller than this many KiB |

Once a conversation is idle, the cache lasts about `max_pings` × the average interval + 5 minutes, about 32 minutes with the defaults. The cache lifetime counts from when Anthropic receives the request, network latency included, so leave a margin between `interval_max` and 300 seconds. With an invalid config the plugin uses the defaults and writes a warning to the CPA log.

## Management page

Open **Wick** from the plugin menu of the CPA Management Center. The page uses the management key the Management Center remembered at sign-in, or asks for one; the theme and language follow the Management Center.

- **Settings**: **Keep-alive duration** offers Off, 15 min, 30 min, 1 hour or Custom, which set `max_pings` to `0`, `2`, `6`, `12` or the number entered; below it the page shows the estimated cache lifetime for the current intervals. **Advanced** changes `interval_min`, `interval_max` and `min_body_kb`. Changes are saved to `config.yaml` and applied by the plugin within a few seconds.
- **Conversations kept alive**: for each conversation, the first 8 characters of the session ID, the model, the account, the cache tokens read by the last refresh, the refresh count, the idle time and a countdown to the next refresh. **Stop** stops refreshing that conversation until it receives its next request.
- **Ping history**: the time, cache tokens read and written, and result of the last 100 refreshes, plus totals since the plugin was loaded. The history is kept in memory only and is cleared when CPA restarts or reloads the plugin.

## Logs and status API

Each refresh writes one line to the CPA log with the first 8 characters of the session ID, the model, the refresh number, and the cache tokens read and written, for example:

```text
wick: ping session=1a2b3c4d key=5e6f7a8b ping=3 cache_read=322200 cache_creation=0 input=3 output=1 model=claude-opus-5-5
```

`output` is the count in `message_start` and leaves out tokens generated before the disconnect.

`GET /v0/management/wick/status` returns the conversations being refreshed, the next refresh times, the results of the last 100 refreshes and running totals. It requires the CPA management key:

```bash
curl -H "Authorization: Bearer <management key>" http://127.0.0.1:8317/v0/management/wick/status
```

## Security and risks

- Conversation content is kept only in memory, never written to disk or logs, and is cleared when CPA restarts or the plugin is unloaded.
- A refresh is identical to the original request and the intervals are random, but regularly resending the same request and cancelling it may still be noticed by the provider. Using a subscription account through CPA carries account risk in itself; judge for yourself.
- When a refresh fails (for example, it is rate limited), CPA puts the account into cooldown as it does for any request, and your next request is rejected until the cooldown ends.

## Development

Plugin structure, the CPA interface, building and testing are described in the [architecture document](./docs/architecture_EN.md).

## License

[AGPL-3.0-or-later](./LICENSE). If you distribute a modified version, or offer a modified version to others over a network, you must publish its source code under the same license.
