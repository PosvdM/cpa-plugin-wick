# 架构

[English](./architecture_EN.md)

本文中的“挑灯”指一次保活请求：原样重发对话的最后一个请求，读取缓存后断开。代码和日志中称为 ping。

## 模块

| 路径 | 职责 |
| --- | --- |
| `main.go` | cgo 入口，导出 `cliproxy_plugin_init`，在 C 边界和 `plugin.Handle` 之间转发调用 |
| `internal/plugin` | RPC 方法分发：注册、拦截、请求结束、管理接口；把数据目录交给下面两个存储 |
| `internal/keepalive` | 请求筛选、对话识别和描述、挑灯调度、SSE 用量解析，以及正在挑灯的对话的保存和恢复 |
| `internal/history` | 挑灯记录：按日写入 JSON Lines 文件、按范围查询、按对话和轮次汇总、计算花费和省下的金额、清理过期文件 |
| `internal/pricing` | 获取并解析 Anthropic 官方定价页，保存最近一次成功获取的价格，按模型名查价格 |
| `internal/host` | 宿主回调的封装：流式模型执行、读取、关闭和日志 |
| `internal/config` | 解析 `plugins.configs.wick`，读取 CPA 配置中的启用状态和插件目录 |
| `internal/web` | 管理页面。HTML、CSS、JavaScript 和图标分文件编辑，嵌入插件后合成一个文档返回 |

## 与 CPA 的接口

插件注册三项能力：`request_interceptor`、`request_lifecycle_plugin` 和 `management_api`。`go.mod` 中的 CPA SDK 为 v8.0.4，这些方法和类型在该版本中已经存在；接口行为按 CPA v8.0.39 的源码核对。

| 方法 | 用途 |
| --- | --- |
| `request.intercept_after` | 在 CPA 选定账号之后、执行器改写请求之前调用，带客户端原始请求体、入站请求头和 `Metadata["selected_auth_id"]`。插件记录请求，返回空对象，请求原样通过 |
| `request.intercept_before` | 能力要求实现，直接返回空对象 |
| `request.complete` | 请求结束时异步调用。`Outcome` 为 `failed` 或 `rejected` 时熄灭对应对话 |
| `host.model.execute_stream` | 发送挑灯请求，`auth_id` 锁定原账号 |
| `host.model.stream_read` / `stream_close` | 读取挑灯响应到 `message_start`，然后关闭 |
| `host.log` | 写 CPA 日志 |

注意事项：

- `RequestInterceptRequest` 和 `RequestCompletion` 没有 JSON 标签，宿主发送的字段名是 Go 字段名（如 `Body`、`Metadata`），`Body` 为 base64。解码必须使用 `pluginapi` 中的类型。
- `request.intercept_after` 在请求的 goroutine 中同步调用，没有超时，CPA 每次换账号重试时会再调用一次。处理时只做筛选、计算对话键和描述、复制请求体，保存对话由后台 goroutine 完成。请求结束了一轮挑灯时，释放锁之后向挑灯记录追加一行；出现新的对话键、且同一会话和 agent 下已有其他对话时，还要逐条比对消息（见[对话识别和描述](#对话识别和描述)）。
- 插件自己通过 `host.model.execute_stream` 发出的请求不会再经过本插件的拦截器，其他插件（如用量统计）照常能看到它们。
- 调用 `execute_stream` 时不传 `host_callback_id`，否则回调作用域结束时 CPA 会关闭这个流。`execute_stream` 会阻塞到第一个数据块到达，期间无法取消。
- `stream_read` 没有超时。每次挑灯启动一个 60 秒的看门狗，到时关闭流，阻塞中的读取随之返回。
- CPA 卸载插件时先拒绝该插件的所有宿主回调，再调用 `plugin.shutdown`。此时进行中的挑灯流无法关闭，会一直占用。`plugin.quiesce`（热更新替换时调用）不受影响。
- 热更新时 CPA 先对旧实例调用 `plugin.quiesce`，再加载新实例，所以新实例从磁盘恢复对话时旧实例已经停止挑灯。
- 挑灯请求和普通请求一样计入账号状态，失败（如 429）会让账号进入冷却。
- CPA 的日志格式只输出它认识的字段（如 `model`），插件传给 `host.log` 的其他字段会被丢掉，因此插件把这些字段也写进日志正文。
- 插件不注册 `response_interceptor`。Claude Code 自动生成的会话标题只出现在一个流式小请求的响应里，读取它需要流式分块拦截器；注册后 CPA 会把每个流式请求的完整请求体和每个分块都交给插件，所有客户端的流量都要多经过这一步，因此页面用第一句话描述对话。

## 管理接口和页面

`management_api` 注册以下路由和一个页面。`/v0/management/wick/` 下的路由需要 CPA 管理密钥；页面属于资源路由，CPA 不做鉴权，页面本身不含任何数据，数据都通过需要密钥的接口读取：

| 路由 | 用途 |
| --- | --- |
| `GET /v0/management/wick/status` | 返回 `Manager.Status()`（配置、正在挑灯的对话、最近一次写入记录（挑灯或回来）的时间 `last_event`），外加插件版本 `version`、CPA 配置中的启用开关 `enabled`、配置错误 `config_error`、数据目录 `data_dir` 和插件时钟 `now`。对话中的 `session` 是完整的会话 ID，`key` 是对话键的前 8 位，`pings` 是本轮（上一个请求之后）的挑灯次数，`max_pings` 是这个对话每轮的上限，`total_pings` 是挑灯记录中这个对话键在保留期内的挑灯次数，由 `plugin.status` 从记录统计（`Store.CountByKey`，不排序）；进行中的挑灯尚未写入记录，因此累计次数不小于本轮次数 |
| `GET /v0/management/wick/history` | 参数 `range` 为 `24h`（默认）、`7d` 或 `all`（保留期内全部），`limit` 为返回的对话数（默认 20，最多 5000）。返回所选范围内的汇总 `totals`、价格状态 `prices`、对话数 `count`，以及最后挑灯时间最新的 `limit` 个对话 `conversations`，新的在前。`totals` 和每个对话都包含 `history.Totals`：挑灯次数、失败次数、读取和写入的缓存合计、轮数 `rounds`、按结论统计的轮数 `results`、花费 `cost`、省下的金额 `saved`、净值 `net` 和没有价格的轮数 `unpriced`，算法见[轮次和省耗](#轮次和省耗)。每个对话是 `history.Conversation`，另有首次和最后一次挑灯的时间、最后一次挑灯的上下文大小 `context`、最后一次的错误和熄灭原因、取自最后一次挑灯的描述，以及逐轮明细 `detail`（新的在前：起止时间、模型、次数、上下文、缓存读取、结论、回来的时间和空闲秒数、花费和省下的金额）。`prices` 是 `pricing.State`：所用价格的获取时间 `fetched`、来源、模型数和最近一次失败的错误 `error` |
| `POST /v0/management/wick/duration` | 请求体为 `{"key": "<对话键前 8 位>", "hours": <小时数>}`，`hours` 为 0 到 24，0 表示恢复默认时长。返回新的状态；找不到对话时返回 404 |
| `POST /v0/management/wick/stop` | 请求体为 `{"key": "<对话键前 8 位>"}`，熄灭该对话并返回新的状态；找不到对话时返回 404 |
| `GET /v0/resource/plugins/wick/page` | 管理页面，在管理中心菜单中显示为 Wick |

页面的实现：

- 页面运行在管理中心的同源 iframe 中。管理密钥取自管理中心的 `localStorage`（`cli-proxy-auth`，用主机和 User-Agent 派生的密钥做异或混淆）；取不到时让用户输入，只存在 `sessionStorage`。主题和语言读取父页面根元素的 `data-theme` 和 `lang`，单独打开时读取管理中心存在 `localStorage` 中的设置。这部分与 Lamplighter 的管理页面相同。
- 设置通过 CPA 的 `GET` / `PATCH /v0/management/plugins/wick/config` 读写。CPA 对顶层键做浅合并后写入 `config.yaml`，再热加载并调用 `plugin.reconfigure`。CPA 不校验插件配置，因此页面在保存前检查间隔范围、保留天数和整数；插件收到无效配置时使用默认值，并在状态中返回 `config_error`。
- 默认挑灯时长和子代理挑灯时长共用 `page.js` 中的预设 `PRESETS`，分别对应 `max_pings` 和 `subagent_max_pings`，两行的元素在 `DURATIONS` 中；单个对话的时长预设在 `DURATION_PRESETS` 中。估算的缓存保持时间为 `次数 × (interval_min + interval_max) / 2 + 300` 秒，页面按分钟向下取整显示。
- 页面每 5 秒刷新一次状态，标签页不可见时暂停。空闲时长和倒计时每秒在本地更新，按状态中的 `now` 校正与插件时钟的偏差。状态中的 `last_event` 变化时，或最早一个挑灯中的轮次到期（最后一次挑灯后 300 秒，结论在服务端按时间确定，不产生新事件）时，才重新读取挑灯记录。状态刷新不重建挑灯记录的表格，保留其中的焦点、选中的文字和滚动位置。
- 用户正在使用自定义时长的输入框，或 15 秒内操作过某个对话的时长菜单时，刷新不重建对话表格，否则菜单会被关闭。用方向键在收起的菜单上改值时，每一步都会触发 `change`，因此键盘修改在按 Enter 或菜单失去焦点时才提交。
- 对话以客户端和第一句话显示，完整的第一句话、项目目录、会话 ID 和对话键放在悬停提示中。入口与客户端名称的对应关系在 `page.js` 的 `CLIENTS` 中，未列出的入口原样显示。
- 空闲时长和倒计时都显示为 `分:秒`，一小时起为 `时:分:秒`。
- 上下文大小是一次挑灯的全部输入：`cache_read_input_tokens`、`cache_creation_input_tokens` 和 `input_tokens` 之和。正在挑灯的对话用状态中的 `last_usage` 计算，挑灯记录用 `context`。
- 挑灯记录的对话行可以点击，或用 Enter、空格键展开逐轮明细。展开状态按对话键保存在页面中，刷新后保留。页面只渲染接口返回的轮次和金额，不自己计算；价格未取得（`prices.fetched` 为空）时不显示金额，对话中有结论、有价格的轮次时才显示花费。
- 分段按钮（默认挑灯时长、子代理挑灯时长、记录范围）的滑块由 `placeThumb` 定位：测量选中按钮的 `offsetLeft`、`offsetTop`、宽和高，写入 CSS 变量。按钮宽度不同，还可能换行，所以不按序号计算位置。点击时滑块带回弹过渡滑过去，并播放挤压动画 `seg-squash`；首次显示、切换语言、窗口尺寸变化、从隐藏变为可见时由 ResizeObserver 触发，直接定位，不播动画。位置没有变化时跳过，避免打断正在进行的滑动。`prefers-reduced-motion` 时关闭过渡和动画。外观和动画曲线与 Lamplighter 的模式开关相同，两个项目各有一份实现。
- 标题旁的图标是 `internal/web/logo.png`（64 px），以 data URI 内嵌在页面中。仓库是私有的，页面无法从 GitHub 加载仓库中的文件。

## 挑灯请求

挑灯请求要命中原请求写入的缓存，发往上游的内容必须与原请求一致：

- 请求体是客户端原始请求体的副本，不做任何修改。对 Claude OAuth 账号，CPA 每次都对最终请求体重新计算 CCH（`claude_signing.go` 的 `signAnthropicMessagesBody`）。计费块中的 CCH 每轮对话都不同，不影响缓存命中。
- 请求头是记录下来的入站请求头，去掉 `Content-Length` 和客户端访问 CPA 的凭据（`Authorization`、`X-Api-Key`、`Proxy-Authorization`、`Cookie`）。Claude 执行器总是用账号自己的凭据替换这些请求头，所以去掉它们不影响请求，也避免把客户端密钥写进数据目录。CPA 依据 `X-App: cli`、`claude-cli/` 开头的 User-Agent、`anthropic-beta` 中的 `claude-code-20250219` 和请求体中的 `metadata.user_id` 判断原生 Claude Code，满足时不改写 `cache_control`。
- 请求和原请求一样以流式发送。读到 `message_start` 后关闭流，CPA 随之取消上游连接，在上游看来与客户端按 Esc 中断相同。`message_start` 在生成内容之前发出，此时缓存已经读完；关闭前上游可能已生成几个 token，按输出计费。
- 不能更早断开：`execute_stream` 要等第一个数据块（即 `message_start`）到达才返回流 ID；而且没有 `message_start` 就无法确认缓存已读、无法判断是否命中。
- 不修改 `max_tokens`。Anthropic 对流式请求拒绝 `max_tokens: 0`（400，`stream cannot be true when max_tokens is 0`），而 0 和 1 都不是 Claude Code 自己会发的取值。

读到 `message_start` 后，插件记录其中的 `usage`。`cache_creation_input_tokens` 大于 0 说明这次请求写入了缓存：缓存已经过期，或挑灯请求与原请求不一致。此时熄灭，避免反复支付写入费用。

## 请求筛选

满足以下全部条件的请求才会记录：

- `SourceFormat` 为 `claude`，并且是流式请求；
- 请求头 `X-App` 为 `cli`，User-Agent 以 `claude-cli/` 开头；
- 请求体不小于 `min_body_kb`；
- 能取得会话 ID（`X-Claude-Code-Session-Id`，缺失时用 `metadata.user_id`）和 `selected_auth_id`；
- 请求体中没有 `"ttl": "1h"`。出现 1 小时缓存时，删除该对话的记录。

## 对话识别和描述

对话键是会话 ID、agent ID、模型、`tools` 和 `system` 的 SHA-256。Claude Code 只在 subagent 的请求中发送 `X-Claude-Code-Agent-Id`，每个 subagent 的值不同，主对话没有这个请求头。`system` 中以 `x-anthropic-billing-header` 开头的计费块每次请求都不同，计算时跳过。同一对话前后的请求只有 `messages` 不同，键相同。

每个对话只保存最后一个请求。主对话（没有 agent ID 的请求）收到新请求时，同一会话中首次出现时间晚于主对话上一个请求的其他对话被删除：它们是主对话等待期间启动的 subagent，主对话继续说明它们已经返回。subagent 的请求不触发删除。如果 Claude Code 不发送 agent ID，所有请求都按主对话处理。

对话键包含工具、系统提示和模型。对话中途加载 MCP 服务器或延迟加载的工具（如通过 ToolSearch 加载 WebFetch）、换了系统提示或用 `/model` 换了模型时，后续请求得到新的键。缓存按 tools → system → messages 的顺序匹配前缀，旧键的缓存再也读不到，继续挑灯只会白花钱。因此出现新键时，`replaceLocked` 检查同一会话、同一 agent ID（都为空也算相同）下的其他对话：旧请求的消息除最后一条外，与新请求开头的消息逐条原样相同（比较原始 JSON），就判定为同一对话的延续，删除旧对话，日志中的熄灭原因为 `replaced`。最后一条不比较，因为 Claude Code 会把缓存标记移到新的最后一条消息上。只有一条消息的旧对话无从比较，不删除。只看会话 ID 不够：Claude Code 的后台辅助请求也带同一个会话 ID，但不是在原对话后面续写的。只有同一会话和 agent 下已有其他对话时才解析消息，普通请求不受影响。

内存中最多保存 32 个对话，超出时删除最后请求最早的一个；单独设了时长的对话最后删除。

页面上的描述在记录请求时取出（`describe.go`）：

- 客户端：User-Agent `claude-cli/<版本> (<用户类型>, <入口>, …)` 中的入口，如 `cli`、`claude-desktop`、`claude-vscode`、`sdk-ts`。

- 项目目录：第一次出现的 `Primary working directory:` 或 `Working directory:` 一行的路径，即会话开始时的目录。Claude Code 较新的版本把环境信息作为 `messages` 中 `role` 为 `system` 的消息发送，较早的版本写在 `system` 中，两处都查，`messages` 只查前 20 条。
- 第一句话：前 20 条消息中第一条用户文字。先删去 `<system-reminder>` 块，跳过本地命令的输出（`<local-command-…>`、`<bash-…>`）和 Claude Code 附加的 `Caveat: …` 说明；斜杠命令显示为 `<command-name>` 加 `<command-args>`。空白合并为一个空格，状态中最多 200 个字，挑灯记录中最多 80 个字。

## 调度

- 对话收到请求时，在请求开始时间之后的 `interval_min` 到 `interval_max` 秒之间均匀随机选一个时刻挑灯。缓存从上游收到请求时计时，所以起点是请求开始而不是响应结束；回答输出时间较长时，第一次挑灯可能在回答结束前发出。
- 每次挑灯成功后，从这次挑灯的开始时间起按同样规则安排下一次。
- 本轮次数在每个新请求时清零。每轮的上限由 `limitLocked` 决定：带 agent ID 的子代理用 `subagent_max_pings`，其余用 `max_pings`；`max_pings` 为 0 时子代理也不挑灯。上限在挑灯返回时按当前配置判断。修改配置后，本轮次数已达到新上限、又没有单独设置时长的对话立即熄灭；恢复保存的对话时同样跳过这类对话。
- 累计次数不存在管理器中：对话熄灭后被删除，再出现时是新的对话，所以状态接口从挑灯记录统计，记录不会因熄灭或重启丢失。
- 默认在本轮达到上限后熄灭。单独设了时长的对话改用截止时间 `until`（设置时刻加上小时数）：某次挑灯开始后 300 秒（缓存的有效期）不早于 `until` 时熄灭，因此缓存至少保持到 `until`。新请求清除 `until`；在页面上恢复默认时长时，如果本轮次数已达到上限，立即熄灭。
- 每次安排都从管理器的全局计数器取一个代数（`gen`）。计时器触发和挑灯返回时都核对代数，旧的计时器和结果不影响新的安排；对话被删除后重新出现时，进行中的旧挑灯也不会与它匹配。
- 上限为 0 时同理：`max_pings` 改为 0 时清空所有对话，`subagent_max_pings` 改为 0 时删除所有子代理。上限为 0 的请求不记录。
- 每次挑灯前读取 CPA 配置文件，插件被关闭时删除对话。CPA v8.0.39 在插件被关闭时卸载实例（与 `plugin.shutdown` 相同，见下文），这项检查用于卸载前的间隙和不卸载实例的 CPA 版本。
- `plugin.quiesce` 和 `plugin.shutdown` 先保存对话，再停止所有计时器、清空内存中的对话，并关闭进行中的挑灯流；磁盘上保存的对话供下一个实例恢复。之后的 `plugin.reconfigure` 恢复运行，并重新从磁盘恢复对话。插件被关闭后在缓存过期前重新启用，保存的对话也会恢复。
- 从管理页面熄灭对话时，删除该对话。进行中的挑灯照常完成，返回时因找不到对话而不再安排下一次。
- 因达到上限、到达单独设置的时长、修改配置、恢复默认时长、手动熄灭或淘汰而删除的对话，如果本轮挑过灯，`retireLocked` 把这一轮另存在 `ended` 中，保留到最后一次读取缓存后 300 秒，用来判断这段时间内的回来。挑灯未命中或失败、插件关闭、子代理结束和换键删除的对话不保留。`ended` 不保存到磁盘，重启或热更新时丢失，这期间的回来不会被记录。
- 计时器回调运行在独立的 goroutine 中，未恢复的 panic 会使 CPA 进程退出，因此回调带 `recover`。

## 轮次和省耗

### 回来

对话收到请求时，如果它本轮挑过灯（正在挑灯，或在 `ended` 中），`Manager.Observe` 往挑灯记录写一条 `kind` 为 `return` 的事件，记下 `saved`、距上一个请求的秒数 `idle`、本轮最后一次成功挑灯的输入 `context` 和本轮次数 `pings`，`ping` 为 0。`saved` 的条件是距最后一次成功读取缓存（`lastRead`）不到 300 秒，并且距上一个请求（`lastRequest`）至少 300 秒：前者说明缓存靠挑灯保持着，后者说明不挑灯缓存已经过期。

### 轮次重建

`history.Group` 从记录重建轮次，轮次本身不写进文件，旧记录也能拆分。同一对话键的事件按时间排序，`ping` 序号不大于上一条，或上一轮已有回来事件时，开始新的一轮；回来事件结束当前一轮。没有对应挑灯的回来事件（如挑灯在所选范围之外）被忽略。

每轮的结论 `Round.Result` 按以下顺序判断：

1. 本轮最后一次挑灯失败或写入了缓存：`failed` 或 `miss`。
2. 有回来事件：`saved` 为真时为 `saved`，否则为 `wasted`。
3. 最后一次挑灯没有 `returns` 标记，即由 v0.5.0 之前的版本写入：`unknown`，只显示轮次，不计金额。
4. 最后一次成功挑灯后不到 300 秒：`pending`。
5. 其余为 `wasted`：超过 300 秒没有回来，结论已定，不必等待。换键删除的对话不会再有回来事件，按这一条算作 `wasted`。

v0.5.0 起每条事件都带 `returns: true`，用来区分“没有回来”和“当时的版本不记录回来”。

### 金额

金额在查询时按当前价格计算，不写进事件：

- 每次挑灯的花费为 `cache_read × 读取价 + cache_creation × 5 分钟写入价 + input × 输入价`。挑灯在 `message_start` 断开，输出按 0 计。分档价格按这次挑灯的输入总量选档。
- `saved` 轮省下的金额为回来事件的 `context` × (5 分钟写入价 − 读取价)。
- 汇总中的 `cost` 和 `saved` 包括 `pending` 轮；`net` 为 `saved` 减去有结论的轮次的花费，不含 `pending` 轮。`unknown` 轮和模型没有价格的轮次不计入这三项，后者计入 `unpriced`。

### API 价格

`internal/pricing` 读取官方定价页的 Markdown 版本 `https://platform.claude.com/docs/en/about-claude/pricing.md`：

- 解析 `## Model pricing` 下的第一张表，按表头找到 Base input、5m cache writes、1h cache writes、Cache hits and refreshes 和 Output 五列，单价格式为 `$x / MTok`。
- 模型名去掉括注，如 “([limited availability](…))”。“(for prompts up to / over 100,000 tokens)” 这类括注解析为分档价格，按输入总量（读取、写入和未缓存的输入之和）选档。显示名拆成系列和版本号，如 “Claude Opus 5.5” 的系列为 `opus`、版本为 `5.5`，ID 为 `claude-opus-5-5`。
- 查价格时，CPA 的模型名转为小写，去掉方括号后缀（如 `[1m]`），拆成单词和版本号。版本号取第一段连续的一两位数字，之后的日期和数字（如 Bedrock ID 末尾的 `-v1:0`）不计入；表中和模型名的版本号都去掉末尾的 `.0`，所以 `claude-opus-4-0` 对应 Claude Opus 4。版本号相同、并且包含表中系列的全部单词时匹配，所以 `-thinking` 等后缀和老型号的词序（如 `claude-3-5-haiku`）都能匹配。匹配不到的模型不计金额。
- 只解析模型价格表。Claude Sonnet 4 和 4.5 的提示超过 200K token 时按长上下文价格计费，这部分加价不计入，金额偏低。
- 插件加载时获取一次，成功后每 24 小时获取一次，失败后 1 小时重试；使用标准 `net/http`，超时 15 秒。成功后写入数据目录的 `prices.json`（带获取时间），插件加载时先读这个文件。获取或解析失败时沿用当前价格，在日志和页面上提示。解析出的模型少于 5 个时按失败处理，避免定价页改版后覆盖可用的价格。
- `plugin.quiesce` 和 `plugin.shutdown` 停止获取；之后的 `plugin.reconfigure` 重新开始，并立即获取一次。

## 数据目录

数据目录为配置项 `data_dir`，为空时为 `<plugins.dir>/data/wick`（`config.DataDir`）。`plugins.dir` 从 CPA 配置文件读取，相对路径相对于 CPA 的工作目录；CPA 从临时目录中的副本加载插件，所以不能用动态库自身的路径推算。目录权限为 `0700`，文件为 `0600`。

### 正在挑灯的对话

`internal/keepalive/persist.go` 把内存中的对话保存在 `conversations/` 下：

- `index.json`：每个对话的键、首次出现时间、最后请求时间、最后一次读取缓存的时间 `last_read`、下次挑灯时间、本轮的挑灯次数、`until` 和小时数、上次的用量。
- `<对话键>.req`：第一行是请求的元数据（会话 ID、agent ID、模型、账号、请求 ID、目录、第一句话、请求头）的 JSON，之后是原样的请求体。请求体不经过 JSON 编码，重放时与原请求逐字节相同。

对话变化时（新请求、挑灯、设置时长、删除）管理器发出信号，后台 goroutine 等待 5 秒合并同一批变化，再在锁内取快照、在锁外写文件。请求文件只在该对话的代数变化时重写，其余变化只重写 `index.json`。写文件先写 `.tmp` 再改名；不在快照中的文件被删除；对话键必须是 64 位十六进制，否则不读取。

`plugin.quiesce` 和 `plugin.shutdown` 在清空对话之前取最后一次快照，释放锁后让 goroutine 退出并等待它结束，再同步写入这份快照。热更新时 CPA 在加载新实例之前对旧实例调用 `plugin.quiesce`，所以新实例读到的是旧实例停止时的状态。CPA 在调用 `plugin.quiesce` 之后、切换到新实例之前收到的请求不会被任何一个实例记录，这样的对话恢复后重放的是上一轮请求，直到它收到下一个请求。

`plugin.register` 或 `plugin.reconfigure` 设置目录后，管理器在本实例启动或恢复后第一次设置目录时读取保存的对话：

- 下次挑灯时间不早于加载后 3 秒，给 CPA 完成插件加载留出时间；也不晚于 `last_read` 之后 290 秒（缓存有效期减去 10 秒，留给请求到达上游），`interval_max` 大于 290 时保存的时间可能更晚。
- 如果加载后 3 秒已经晚于 `last_read` 之后 290 秒，缓存视为已过期，不恢复，下一次写入时删除其文件。
- 恢复的对话取新的代数，次数、`until` 和最后请求时间保持不变。

### 挑灯记录

`internal/history` 把每次挑灯和每次回来的 `keepalive.Event` 追加到 `history/<UTC 日期>.jsonl`，每行一个 JSON 对象，每次用一个 `O_APPEND` 写入一整行，热更新时新旧实例短暂并存也不会写坏文件。每天第一次写入时删除早于 `history_retention_days` 天的文件。查询时按日读取文件，按文件大小和修改时间缓存解析结果，文件未变时不重新解析。

记录接口把查询结果交给 `history.Group`，按对话键（前 8 位）汇总并拆分轮次，同一个对话在所选范围内只返回一项；没有对话键的记录按会话 ID 汇总。状态接口的累计次数（`Store.CountByKey`）和汇总都只计挑灯事件，不计回来事件。

### API 价格

`prices.json` 是最近一次成功获取的价格表（`pricing.Table`）：获取时间、来源和每个模型的名称、ID、系列、版本号和分档价格。

## 开发

### 环境

- Go 1.26。
- 构建动态库需要 cgo 和目标平台的 C 编译器。`internal/` 下的包都是纯 Go，测试不需要 cgo。

### 测试

```bash
go vet ./internal/...
go test ./internal/...
```

测试使用假的宿主和时钟，不发送真实请求，也不等待真实时间；持久化的测试在临时目录中直接调用写入函数，不等待后台 goroutine。覆盖内容：请求筛选和挑灯请求体（包括去掉客户端凭据）、对话键、项目目录和第一句话的提取、挑灯间隔和次数、子代理的上限（包括单独时长和 `subagent_max_pings: 0`）、从记录统计的累计次数、新请求重新计时、缓存未命中和错误时熄灭、subagent 的删除（包括带 agent ID 的并行 subagent）、失败和中断请求的处理、插件关闭和 `max_pings: 0`、过期的挑灯结果、单独时长的截止和清除、淘汰时保留单独时长的对话、对话的保存和恢复（计划保持、到期补发、晚于截止点时提前、缓存过期不恢复、停止前的最后状态、文件删除）、挑灯记录的写入、查询、按对话汇总和清理、SSE 解析、日志正文、配置校验和数据目录，手动熄灭对话，插件 RPC 的注册和拦截请求解码，管理接口和页面，换了工具后熄灭旧对话（辅助请求和其他 agent 不受影响），回来事件的判定（及时回来、过早、过晚、手动熄灭后回来、换键后不记录），轮次重建、结论和金额，以及定价页的解析（以 2026-10-24 的官方页面 `internal/pricing/testdata/pricing.md` 为样例）、模型名匹配和价格缓存。

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
- 修改挑灯请求的构造时，对照 CPA 的 `internal/runtime/executor/claude_executor*.go`，确认发往上游的内容仍与原请求一致。
- 修改 `conversation` 或 `request` 的字段时，同步修改 `persist.go` 中保存和恢复的字段。
- 新增的 goroutine 必须有 `recover`。
- 定价页的表格结构变化时，用新页面替换 `internal/pricing/testdata/pricing.md`，再修改解析。
- 更换 `assets/logo.png` 时，同步生成 64 px 的 `internal/web/logo.png`，并发布新版本，页面上的图标才会更新。
- 用户可见的行为、配置和限制写进 README（中英文同步），实现细节写进本文档（中英文同步）。
