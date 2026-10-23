# Wick

[English](./README_EN.md)

Wick（灯芯）是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）的原生插件。Claude Code 经 CPA 使用 Claude 订阅账号时，Wick 在对话空闲期间定时续期 5 分钟提示缓存，回到对话时仍能命中原来的缓存。

## 用途

Claude 的提示缓存有两种有效期：5 分钟的写入价是输入价的 1.25 倍，1 小时的是 2 倍；读取缓存只需 0.1 倍。每次读取都会让缓存重新计时。原生 Claude Code 默认使用 5 分钟缓存，离开超过 5 分钟后，下一条消息要按 1.25 倍重新写入整个上下文。上下文有几十万 token 时，这一次重写会消耗明显的额度。

Wick 让平时的写入继续按 5 分钟的价格计算，同时在对话空闲时每隔 4 分 20 秒到 4 分 50 秒读一次缓存，最多连续读取 12 次，缓存可保持约 1 小时。

## 工作方式

- Wick 记录每个对话的最后一个请求。从这个请求开始计时，在 `interval_min` 到 `interval_max` 秒之间随机选一个时刻，把这个请求再发一次，只把 `max_tokens` 改成 0、改为非流式。这次请求只读取缓存，不生成内容，结果不会回到 Claude Code，对话的上下文保持不变。Anthropic 只在非流式请求中接受 `max_tokens: 0`。
- 续期请求经过 CPA 的模型执行器，使用原请求的账号和请求头，因此与原请求命中同一份缓存。
- 对话中出现新请求时，计时从新请求开始，续期次数清零。
- 以下情况停止续期：达到 `max_pings` 次；续期请求失败；续期请求写入了缓存（说明缓存已过期或请求不一致，再续期也无法命中）；原请求失败；插件被关闭。按 Esc 中断的请求继续续期。
- 只处理原生 Claude Code 的流式请求。以下请求不续期：使用 1 小时缓存的请求；开启 budget 模式 thinking（`budget_tokens`）的请求，因为 CPA 可能把这类请求的 `max_tokens: 0` 换成默认值；请求体小于 `min_body_kb` 的请求，如生成标题等辅助请求。
- 同一会话中，主对话和每个 subagent 分别续期。主对话收到下一个请求时，在它等待期间开始的 subagent 视为已结束，不再续期。

续期请求的读取量会计入订阅额度。每次续期读取整个上下文，按 0.1 倍计价；如果离开的时间短于续期一两次的时长，节省的额度较少。

## 要求

- CPA v8.0.39 或更新版本，并开启插件（`plugins.enabled: true`）。
- 原生 Claude Code 经 CPA 使用 Claude 订阅账号。其他客户端（pi、Cherry Studio 等）的请求不处理。

## 安装

1. 在 [Releases](https://github.com/PosvdM/cpa-plugin-wick/releases) 下载与 CPA 所在系统对应的 zip，或按 [架构文档](./docs/architecture.md#构建) 自行构建。
2. 把动态库（Linux 为 `wick-v<版本>.so`）放进 CPA 插件目录下的 `<系统>/<架构>/` 子目录，例如 `plugins/linux/arm64/`。Docker 部署时，插件目录是挂载到容器 `/CLIProxyAPI/plugins` 的宿主机目录。
3. 在 CPA 的 `config.yaml` 中启用插件：

   ```yaml
   plugins:
     enabled: true
     configs:
       wick:
         enabled: true
   ```

CPA 检测到配置变化后加载插件，不需要重启。

## 配置

配置写在 `config.yaml` 的 `plugins.configs.wick` 下，修改后自动生效。

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `interval_min` | `260` | 最短续期间隔（秒） |
| `interval_max` | `290` | 最长续期间隔（秒），不超过 `299` |
| `max_pings` | `12` | 一次空闲最多续期几次；`0` 关闭续期 |
| `min_body_kb` | `64` | 请求体小于此大小（KiB）时不续期 |

缓存的有效期从 Anthropic 收到请求时开始计算，网络延迟也计入其中，因此 `interval_max` 应与 300 秒留出余量。配置无效时插件使用默认值，并在 CPA 日志中写一条警告。

## 查看状态

每次续期在 CPA 日志中写一条记录，包括会话 ID 的前 8 位、模型、第几次续期，以及读取和写入的缓存 token 数和输出 token 数，例如：

```text
wick: ping session=1a2b3c4d key=5e6f7a8b ping=3 cache_read=322200 cache_creation=0 input=3 output=0 model=claude-opus-5-5
```

`GET /v0/management/wick/status` 返回正在续期的对话、下次续期时间、最近 100 次续期的结果和累计数据，需要 CPA 管理密钥：

```bash
curl -H "Authorization: Bearer <管理密钥>" http://127.0.0.1:8317/v0/management/wick/status
```

## 安全与风险

- 对话内容只保存在内存中，不写入磁盘或日志。CPA 重启或插件卸载后清空。
- 续期请求与原请求只差 `max_tokens: 0` 和非流式，并且间隔随机，但长时间按规律重发同一请求、不生成内容，仍可能被服务方识别。通过 CPA 使用订阅账号本身也有账号风险，请自行判断。
- 续期请求失败（如限流）时，CPA 会像处理普通请求一样让账号进入冷却，冷却期间你的下一个请求也会被拒绝。

## 开发

插件结构、与 CPA 的接口、构建和测试见 [架构文档](./docs/architecture.md)。

## 许可证

[AGPL-3.0-or-later](./LICENSE)。分发修改后的版本，或通过网络向他人提供修改后的版本时，需要按同一许可证公开源代码。
