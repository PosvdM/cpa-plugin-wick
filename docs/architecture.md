# 架构

[English](./architecture_EN.md)

## 模块

| 路径 | 职责 |
| --- | --- |
| `main.go` | cgo 入口，导出 `cliproxy_plugin_init`，在 C 边界和 `plugin.Handle` 之间转发调用 |
| `internal/plugin` | RPC 方法分发：注册、拦截、请求结束、管理接口 |
| `internal/keepalive` | 请求筛选、对话识别、续期调度和 SSE 用量解析 |
| `internal/host` | 宿主回调的封装：流式模型执行、读取、关闭和日志 |
| `internal/config` | 解析 `plugins.configs.wick`，读取 CPA 配置中的启用状态 |

## 与 CPA 的接口

插件注册三项能力：`request_interceptor`、`request_lifecycle_plugin` 和 `management_api`。`go.mod` 中的 CPA SDK 为 v8.0.4，这些方法和类型在该版本中已经存在；接口行为按 CPA v8.0.39 的源码核对。

| 方法 | 用途 |
| --- | --- |
| `request.intercept_after` | 在 CPA 选定账号之后、执行器改写请求之前调用，带客户端原始请求体、入站请求头和 `Metadata["selected_auth_id"]`。插件记录请求，返回空对象，请求原样通过 |
| `request.intercept_before` | 能力要求实现，直接返回空对象 |
| `request.complete` | 请求结束时异步调用。`Outcome` 为 `failed` 或 `rejected` 时停止对应对话的续期 |
| `host.model.execute_stream` | 发送续期请求，`auth_id` 锁定原账号 |
| `host.model.stream_read` / `stream_close` | 读取续期响应到 `message_start`，然后关闭 |
| `host.log` | 写 CPA 日志 |

注意事项：

- `RequestInterceptRequest` 和 `RequestCompletion` 没有 JSON 标签，宿主发送的字段名是 Go 字段名（如 `Body`、`Metadata`），`Body` 为 base64。解码必须使用 `pluginapi` 中的类型。
- `request.intercept_after` 在请求的 goroutine 中同步调用，没有超时，CPA 每次换账号重试时会再调用一次。处理时只做筛选、计算对话键和复制请求体。
- 插件自己通过 `host.model.execute_stream` 发出的请求不会再经过本插件的拦截器，其他插件（如用量统计）照常能看到它们。
- 调用 `execute_stream` 时不传 `host_callback_id`，否则回调作用域结束时 CPA 会关闭这个流。`execute_stream` 会阻塞到第一个数据块到达，期间无法取消。
- `stream_read` 没有超时。每次续期启动一个 60 秒的看门狗，到时关闭流，阻塞中的读取随之返回。
- CPA 卸载插件时先拒绝该插件的所有宿主回调，再调用 `plugin.shutdown`。此时进行中的续期流无法关闭，会一直占用。`plugin.quiesce`（热更新替换时调用）不受影响。
- 续期请求和普通请求一样计入账号状态，失败（如 429）会让账号进入冷却。

## 续期请求

续期请求要命中原请求写入的缓存，发往上游的内容必须与原请求一致：

- 请求体是客户端原始请求体，只用 `sjson` 把 `max_tokens` 改成 0。`sjson` 原位修改，其余字节和键的顺序不变。CPA 计算 CCH 签名时不包含 `max_tokens`，系统提示开头的计费块因此不变。
- 请求头是记录下来的入站请求头，去掉 `Content-Length`。CPA 依据 `X-App: cli`、`claude-cli/` 开头的 User-Agent、`anthropic-beta` 中的 `claude-code-20250219` 和请求体中的 `metadata.user_id` 判断原生 Claude Code，满足时不改写 `cache_control`。
- 请求以流式发送。改成非流式会改变请求体中的 `stream`，进而改变 CCH 签名。
- `max_tokens: 0` 时上游返回 `stop_reason: max_tokens`，不生成内容。CPA 的 thinking 处理在 budget 模式（`thinking.type` 为 `enabled`）下把 `max_tokens <= 0` 替换为模型默认值（`internal/thinking/provider/claude/apply.go`），续期请求会真正生成内容，因此这类请求不续期。adaptive 模式不经过这段代码。

读到 `message_start` 后，插件记录其中的 `usage`。`cache_creation_input_tokens` 大于 0 说明这次请求写入了缓存：缓存已经过期，或续期请求与原请求不一致。此时停止续期，避免反复支付写入费用。

## 请求筛选

满足以下全部条件的请求才会记录：

- `SourceFormat` 为 `claude`，并且是流式请求；
- 请求头 `X-App` 为 `cli`，User-Agent 以 `claude-cli/` 开头；
- 请求体不小于 `min_body_kb`；
- 能取得会话 ID（`X-Claude-Code-Session-Id`，缺失时用 `metadata.user_id`）和 `selected_auth_id`；
- `thinking.type` 不是 `enabled`；
- 请求体中没有 `"ttl": "1h"`。出现 1 小时缓存时，删除该对话的记录。

## 对话识别

对话键是会话 ID、agent ID、模型、`tools` 和 `system` 的 SHA-256。Claude Code 只在 subagent 的请求中发送 `X-Claude-Code-Agent-Id`，每个 subagent 的值不同，主对话没有这个请求头。`system` 中以 `x-anthropic-billing-header` 开头的计费块每次请求都不同，计算时跳过。同一对话前后的请求只有 `messages` 不同，键相同。

每个对话只保存最后一个请求。主对话（没有 agent ID 的请求）收到新请求时，同一会话中首次出现时间晚于主对话上一个请求的其他对话被删除：它们是主对话等待期间启动的 subagent，主对话继续说明它们已经返回。subagent 的请求不触发删除。如果 Claude Code 不发送 agent ID，所有请求都按主对话处理。

内存中最多保存 32 个对话，超出时删除最后请求最早的一个。

## 调度

- 对话收到请求时，在请求开始时间之后的 `interval_min` 到 `interval_max` 秒之间均匀随机选一个时刻续期。缓存从上游收到请求时计时，所以起点是请求开始而不是响应结束；回答输出时间较长时，第一次续期可能在回答结束前发出。
- 每次续期成功后，从这次续期的开始时间起按同样规则安排下一次。
- 每次安排都从管理器的全局计数器取一个代数（`gen`）。计时器触发和续期返回时都核对代数，旧的计时器和结果不影响新的安排；对话被删除后重新出现时，进行中的旧续期也不会与它匹配。
- `max_pings` 改为 0 时清空所有对话。
- 每次续期前读取 CPA 配置文件，插件被关闭时删除对话。CPA 关闭插件后仍保留已加载的实例，不会通知插件。
- `plugin.quiesce` 和 `plugin.shutdown` 停止所有计时器、清空对话，并关闭进行中的续期流。之后的 `plugin.reconfigure` 恢复运行。
- 计时器回调运行在独立的 goroutine 中，未恢复的 panic 会使 CPA 进程退出，因此回调带 `recover`。

## 开发

### 环境

- Go 1.26。
- 构建动态库需要 cgo 和目标平台的 C 编译器。`internal/` 下的包都是纯 Go，测试不需要 cgo。

### 测试

```bash
go vet ./internal/...
go test ./internal/...
```

测试使用假的宿主和时钟，不发送真实请求，也不等待真实时间。覆盖内容：请求筛选和续期请求体、对话键、续期间隔和次数、新请求重新计时、缓存未命中和错误时停止、subagent 的删除（包括带 agent ID 的并行 subagent）、失败和中断请求的处理、插件关闭和 `max_pings: 0`、过期的续期结果、SSE 解析、配置校验，以及插件 RPC 的注册和拦截请求解码。

### 构建

本地有 Docker 时，在 Docker 主机的 CPU 架构上构建：

```bash
sh scripts/build.sh 0.1.0
```

产物为 `dist/wick-v0.1.0.so`。文件名中的 `-v<版本>` 让 CPA 识别插件 ID（`wick`）和版本。

### 发布

推送 `v*` 标签后，GitHub Actions（`.github/workflows/build.yml`）运行测试，构建 Linux amd64/arm64、macOS amd64/arm64 和 Windows amd64 五个版本，打包为 `wick_<版本>_<系统>_<架构>.zip` 并附上 `checksums.txt`，创建草稿 Release。推送到 `main` 时只构建并上传 Actions 产物。

### 修改约定

- 修改宿主回调、RPC 字段或注册内容前，对照 CPA 源码中的 `sdk/pluginabi`、`sdk/pluginapi` 和 `internal/pluginhost`。
- 修改续期请求的构造时，对照 CPA 的 `internal/runtime/executor/claude_executor*.go`，确认发往上游的内容仍与原请求一致。
- 新增的 goroutine 必须有 `recover`。
- 用户可见的行为、配置和限制写进 README（中英文同步），实现细节写进本文档（中英文同步）。
