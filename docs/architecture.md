# 架构

本文中的“挑灯”指一次保活请求：原样重发对话的最后一个请求，读取缓存后断开。代码和日志中称为 ping。

## 模块

| 路径 | 职责 |
| --- | --- |
| `main.go` | cgo 入口，导出 `cliproxy_plugin_init`，在 C 边界和 `plugin.Handle` 之间转发调用 |
| `internal/plugin` | RPC 方法分发：注册、拦截、请求结束、管理接口；把数据目录交给下面两个存储 |
| `internal/keepalive` | 请求筛选、对话识别和描述、挑灯调度、SSE 用量解析、请求用量的记录，以及正在挑灯的对话的保存和恢复 |
| `internal/history` | 挑灯记录：按日写入 JSON Lines 文件、按范围查询、按对话和轮次汇总、计算花费和省下的金额、计算三种做法的缓存花费、清理过期文件 |
| `internal/pricing` | 获取并解析 Anthropic 官方定价页，保存最近一次成功获取的价格，按模型名查价格 |
| `internal/host` | 宿主回调的封装：流式模型执行、读取、关闭和日志 |
| `internal/config` | 解析 `plugins.configs.wick`，读取 CPA 配置中的启用状态和插件目录 |
| `internal/web` | 管理页面。HTML、CSS、JavaScript 和图标分文件编辑，嵌入插件后合成一个文档返回 |

## 与 CPA 的接口

插件注册四项能力：`request_interceptor`、`request_lifecycle_plugin`、`usage_plugin` 和 `management_api`。最低 CPA 版本和 `go.mod` 中的 SDK 版本均为 v8.0.43，接口行为按该版本源码核对。

| 方法 | 用途 |
| --- | --- |
| `request.intercept_after` | 在 CPA 选定账号之后、执行器改写请求之前调用，带客户端原始请求体、入站请求头、`CallerScope` 和 `Metadata["selected_auth_id"]`。插件记录请求；原生 Claude Code 和跳过的请求返回空对象。其他客户端符合挑灯条件时返回 `Body`，CPA 用它替换请求体和 `OriginalRequest`，挑灯保存相同字节 |
| `request.intercept_before` | 能力要求实现，直接返回空对象 |
| `request.complete` | 请求结束时异步调用。`Outcome` 为 `failed` 或 `rejected` 时熄灭对应对话 |
| `usage.handle` | 请求完成后异步调用，带 `UsageRecord`。插件为它记录过的请求写一条 `request` 事件，其他请求的用量忽略，见[请求用量](#请求用量) |
| `host.model.execute_stream` | 发送挑灯请求，`auth_id` 锁定原账号，`caller_scope` 保持调用方身份 |
| `host.model.stream_read` / `stream_close` | 读取挑灯响应到 `message_start`，然后关闭 |
| `host.log` | 写 CPA 日志 |

注意事项：

- `RequestInterceptRequest`、`RequestCompletion` 和 `UsageRecord` 没有 JSON 标签，宿主发送的字段名是 Go 字段名（如 `Body`、`Metadata`），`Body` 为 base64。解码必须使用 `pluginapi` 中的类型。
- `request.intercept_after` 在请求的 goroutine 中同步调用，没有超时，CPA 每次换账号重试时会再调用一次。处理时只做筛选、计算对话键和描述、复制请求体，以及为符合条件的其他客户端补全 5 分钟 TTL 和 explicit 模式，保存对话由后台 goroutine 完成。请求结束了一轮挑灯时，释放锁之后向挑灯记录追加一行；出现新的对话键、且同一 caller scope、会话和 agent 下已有其他对话时，还要逐条比对消息（见[对话识别和描述](#对话识别和描述)）。
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
| `GET /v0/management/wick/status` | 返回 `Manager.Status()`（配置、正在挑灯的对话、最近一次写入记录（挑灯或回来）的时间 `last_event`），外加插件版本 `version`、CPA 配置中的启用开关 `enabled`、配置错误 `config_error`、数据目录 `data_dir` 和插件时钟 `now`。对话中的 `session` 是完整的会话 ID，`key` 是对话键的前 8 位，`pings` 是本轮（上一个请求之后）的挑灯次数，`last_read` 是最后一次读取缓存的时间，`until` 是挑灯的截止时间（最后一个请求加上时长），`minutes` 是页面上单独设置的分钟数（没有时省略），`default_minutes` 是配置中这个对话的时长，`total_pings` 是挑灯记录中这个对话键在保留期内的挑灯次数，由 `plugin.status` 从记录统计（`Store.CountByKey`，不排序）；进行中的挑灯尚未写入记录，因此累计次数不小于本轮次数 |
| `GET /v0/management/wick/history` | 参数 `range` 为 `24h`（默认）、`7d`、`1m`（上个月的同一时刻至今）或 `all`（保留期内全部），`limit` 为返回的对话数（默认 20，最多 5000）。返回价格状态 `prices`、对话数 `count`，以及最后挑灯时间最新的 `limit` 个对话 `conversations`，新的在前。每个对话是 `history.Conversation`，包含 `history.Totals`（见下一行），另有首次和最后一次挑灯的时间、最后一次挑灯的上下文大小 `context`、最后一次的错误和熄灭原因、取自最后一次挑灯的描述，以及逐轮明细 `detail`（新的在前：起止时间、模型、次数、上下文、缓存读取、结论、回来的时间和空闲秒数、花费和省下的金额）。`prices` 是 `pricing.State`：所用价格的获取时间 `fetched`、来源、模型数和最近一次失败的错误 `error` |
| `GET /v0/management/wick/savings` | 参数 `range` 同上。返回汇总 `totals`、花费对比 `compare` 和价格状态 `prices`。`totals` 是 `history.Totals`：挑灯次数、失败次数、读取和写入的缓存合计、轮数 `rounds`、按结论统计的轮数 `results`、花费 `cost`、省下的金额 `saved`、净值 `net` 和没有价格的轮数 `unpriced`，算法见[轮次和省耗](#轮次和省耗)。`compare` 是 `history.Comparison`：第一个记录了用量的请求的时间 `since`、请求数 `requests`、请求的缓存读写费用 `cache`、挑灯花费 `pings`、省下的金额 `saved`、1 小时缓存下的费用 `hour` 和没有价格的请求及轮次数 `unpriced`，算法见[花费对比](#花费对比) |
| `POST /v0/management/wick/duration` | 请求体为 `{"key": "<对话键前 8 位>", "minutes": <分钟数>}`，`minutes` 为 0 到 1440，0 表示恢复默认时长，时长从对话的最后一个请求起算。返回新的状态；缓存已经能保持到所选时长（包括恢复默认时长）时返回 400 和 `the duration has already passed`，对话不变；找不到对话时返回 404 |
| `POST /v0/management/wick/stop` | 请求体为 `{"key": "<对话键前 8 位>"}`，熄灭该对话并返回新的状态；找不到对话时返回 404 |
| `GET /v0/resource/plugins/wick/page` | 管理页面，在管理中心菜单中显示为 Wick |

页面的实现：

- 页面运行在管理中心的同源 iframe 中。管理密钥取自管理中心的 `localStorage`（`cli-proxy-auth`，用主机和 User-Agent 派生的密钥做异或混淆）；取不到时让用户输入，只存在 `sessionStorage`。主题和语言读取父页面根元素的 `data-theme` 和 `lang`，单独打开时读取管理中心存在 `localStorage` 中的设置。这部分与 Lamplighter 的管理页面相同。
- 设置通过 CPA 的 `GET` / `PATCH /v0/management/plugins/wick/config` 读写。CPA 对顶层键做浅合并后写入 `config.yaml`，再热加载并调用 `plugin.reconfigure`。CPA 不校验插件配置，因此页面在保存前检查间隔范围、保留天数和整数；插件收到无效配置时使用默认值，并在状态中返回 `config_error`。
- 默认挑灯时长和子代理挑灯时长共用 `page.js` 中的分钟数预设 `PRESETS`，分别对应 `duration_minutes` 和 `subagent_duration_minutes`，两行的元素在 `DURATIONS` 中。
- 单个对话的时长预设在 `DURATION_PRESETS` 中。菜单按长短排列，默认时长（状态中的 `default_minutes`）插在其中，与某个预设相同时合并为一项。缓存已经能保持到的时长禁用（当前选中的一项除外），界限 `minutesCovered` 为 `last_read` 减 `last_request` 再加 5 分钟；自定义的分钟数同样检查。
- 页面每 5 秒刷新一次状态，标签页不可见时暂停。空闲时长和倒计时每秒在本地更新，按状态中的 `now` 校正与插件时钟的偏差。状态中的 `last_event` 变化时，或最早一个挑灯中的轮次到期（最后一次挑灯后 300 秒，结论在服务端按时间确定，不产生新事件）时，才重新读取挑灯记录和花费对比。请求用量不更新 `last_event`，所以花费对比另外每 60 秒重新读取一次。花费对比和挑灯记录各有一组范围按钮，互不影响。状态刷新不重建挑灯记录的表格，保留其中的焦点、选中的文字和滚动位置。
- 用户正在使用自定义时长的输入框，或 15 秒内操作过某个对话的时长菜单时，刷新不重建对话表格，否则菜单会被关闭。用方向键在收起的菜单上改值时，每一步都会触发 `change`，因此键盘修改在按 Enter 或菜单失去焦点时才提交。
- 对话以客户端和第一句话显示，完整的第一句话、项目目录、会话 ID 和对话键放在悬停提示中。入口与客户端名称的对应关系在 `page.js` 的 `CLIENTS` 中，未列出的入口原样显示。
- 空闲时长和倒计时都显示为 `分:秒`，一小时起为 `时:分:秒`。
- 上下文大小是一次挑灯的全部输入：`cache_read_input_tokens`、`cache_creation_input_tokens` 和 `input_tokens` 之和。正在挑灯的对话用状态中的 `last_usage` 计算，挑灯记录用 `context`。
- 挑灯记录的对话行可以点击，或用 Enter、空格键展开逐轮明细。展开状态按对话键保存在页面中，刷新后保留。页面只渲染接口返回的轮次和金额，不自己计算；价格未取得（`prices.fetched` 为空）时不显示金额，对话中有结论、有价格的轮次时才显示花费。结论为 `unknown` 的轮次不显示标签。花费对比卡片的金额、差额、百分比和统计都直接使用 `compare` 的字段，横条只按最大金额缩放，桌面按标签、横条、金额、明细排列，窄屏把明细换到横条下方；`since` 为空或价格未取得时回退到 `totals` 汇总。覆盖时间晚于响应中的 `from` 时显示实际日期时间和 v0.6.0 起记录请求用量的限制。
- 分段按钮（默认挑灯时长、子代理挑灯时长、两组范围）的滑块由 `placeThumb` 定位：测量选中按钮的 `offsetLeft`、`offsetTop`、宽和高，写入 CSS 变量。按钮宽度不同，还可能换行，所以不按序号计算位置。点击时滑块带回弹过渡滑过去，并播放挤压动画 `seg-squash`；首次显示、切换语言、窗口尺寸变化、从隐藏变为可见时由 ResizeObserver 触发，直接定位，不播动画。位置没有变化时跳过，避免打断正在进行的滑动。`prefers-reduced-motion` 时关闭过渡和动画。外观和动画曲线与 Lamplighter 的模式开关相同，两个项目各有一份实现。
- 标题旁的图标是 `internal/web/logo.png`（64 px），以 data URI 内嵌在页面中。仓库是私有的，页面无法从 GitHub 加载仓库中的文件。

## 挑灯请求

挑灯请求要命中原请求写入的缓存，发往上游的内容必须与原请求一致：

- 原生 Claude Code 的请求体是客户端原始请求体的副本。符合接管条件的其他客户端由 `cache.go` 为全部合法缓存标记补全 `ttl: "5m"`，并设置 `prompt_cache_options.mode: "explicit"`；拦截响应、磁盘保存和挑灯重放使用相同字节。对 Claude OAuth 账号，CPA 每次都对最终请求体重新计算 CCH（`claude_signing.go` 的 `signAnthropicMessagesBody`）。计费块中的 CCH 每轮对话都不同，不影响缓存命中。
- 请求头是记录下来的入站请求头，去掉 `Content-Length` 和客户端访问 CPA 的凭据（`Authorization`、`X-Api-Key`、`Proxy-Authorization`、`Cookie`）。Claude 执行器总是用账号自己的凭据替换这些请求头，所以不需要用它们提供上游认证，也避免把客户端密钥写进数据目录；它们不能恢复原 HTTP 请求的调用方身份。CPA 的 `helps.DetectClaudeCodeRequest` 根据原生入口、有效的 User-Agent、`X-App: cli`、`anthropic-beta` 和合法的 `metadata.user_id` 等信号确认原生 Claude Code。正常请求和挑灯保留这些识别信号，确认后 CPA 不接管缓存标记，也不做伪装或工具别名。
- 请求和原请求一样以流式发送。读到 `message_start` 后关闭流，CPA 随之取消上游连接，在上游看来与客户端按 Esc 中断相同。`message_start` 在生成内容之前发出，此时缓存已经读完；关闭前上游可能已生成几个 token，按输出计费。
- 不能更早断开：`execute_stream` 要等第一个数据块（即 `message_start`）到达才返回流 ID；而且没有 `message_start` 就无法确认缓存已读、无法判断是否命中。
- 不修改 `max_tokens`。Anthropic 对流式请求拒绝 `max_tokens: 0`（400，`stream cannot be true when max_tokens is 0`），而 0 和 1 都不是 Claude Code 自己会发的取值。

读到 `message_start` 后，插件记录其中的 `usage`。只有 `cache_read_input_tokens` 大于 0 且 `cache_creation_input_tokens` 为 0 才确认续期成功并更新 `lastRead`。读取量不大于 0 或写入量大于 0 时（包括读写并存），都以 `cache_miss` 熄灭，保留原 `lastRead`，不保留可判定回来的轮次。日志写入 `ping missed the cache`，页面沿用“未命中”结论。此规则适用于原生 Claude Code 和其他客户端，避免反复支付未缓存输入或缓存写入费用。

### 工具别名与重放限制

`other_clients` 默认是 `false`，其他客户端须手动开启，并满足下文的缓存标记和显式会话要求。CPA v8.0.43 通过 `CallerScope` 保持普通请求与重放的工具别名一致：

- CPA 的 `session.CallerScope` 对去除首尾空白后的客户端 key 计算带域前缀的 SHA-256，输出不可逆的十六进制标识。相同 key 在 CPA 重启后保持相同 scope；空 key 得到空 scope，CPA 使用默认调用方身份。
- `parseRequest` 优先取 `RequestInterceptRequest.CallerScope`，为空时取 `Metadata[pluginapi.CallerScopeMetadataKey]` 的字符串值。这一步在 `ttl_1h` 提前返回之前完成，因为清理 1 小时缓存的对话时也要按调用方区分。
- `Manager.ping` 把 scope 原样放进 `HostModelExecutionRequest.CallerScope`。CPA 的 `internal/pluginhost/host_callbacks.go` 把它传入执行上下文，`internal/runtime/executor/claude_executor_request.go` 的工具别名密钥优先取 caller scope；`auth_id` 仍只选择上游账号。
- 经 CPA 确认的原生 Claude Code 请求跳过伪装和工具别名。其他请求仅在指纹策略启用 `MCPAlias` 且实际进行了伪装时改写工具名。

调用方身份一致不保证缓存一定可用：strict 伪装（`cloak_strict_mode`）等处理仍可能删除调用方 system 及其缓存标记，缓存前缀也必须达到最低长度。开启后须验证挑灯的 `cache_read_input_tokens > 0` 且 `cache_creation_input_tokens == 0`。

## 请求筛选

满足以下全部条件的请求才会记录：

- `SourceFormat` 为 `claude`，并且是流式请求；
- 原生条件保持为请求头 `X-App` 为 `cli`、User-Agent 以 `claude-cli/` 开头；其他客户端须启用 `other_clients`（默认 `false`）；
- 请求体不小于 `min_body_kb`；
- 能取得会话 ID（来源顺序见下文）和 `selected_auth_id`；
- 请求体中没有 `"ttl": "1h"`。出现 1 小时缓存时，删除该对话的记录。

其他客户端只有在配置时长超过 5 分钟、管理器运行中并通过上述条件后才可能返回改写的 `Body`。`pinCacheTTL` 遍历位置与 CPA v8.0.43 的 `forEachClaudeCacheControlBlock` 相同：`tools[]`、`system[]`、`messages[].content[]`；只接受对象形式、`type` 为 `ephemeral` 的 `cache_control`，不递归查找嵌套内容。接管还须同时满足：

- 合法标记总数为 1～4，且所有标记都没有 `ttl` 字段。
- `prompt_cache_options` 缺省或为对象；对象的 `mode` 缺省或为字符串 `explicit`，判断与 CPA 一致，忽略大小写及首尾空白。

无合法标记时以 `no_cache_control` 跳过；超过 4 个、任一标记已写 `ttl`（包括 `5m`、`1h`、`null` 或其他值）、mode 冲突或选项不是对象时不接管。其中请求体含 `"ttl": "1h"` 时沿用上述 1 小时缓存处理，其余缓存策略冲突返回 `cache_policy`。这些请求不返回 `Body`，也不作为新请求记录或安排挑灯。原生请求沿用原有筛选，不进入 `pinCacheTTL`，请求体不改写。

接管时为每个标记插入 `,"ttl":"5m"`，顶层没有 `prompt_cache_options` 时插入 `{"mode":"explicit"}`；已有对象时仅补 `mode`，其他字段保持原样，已有 explicit 模式也可接管。所有插入按字节偏移倒序执行，不重新序列化 JSON，原有空白、键顺序和数值表示保持不变，相同原始输入的重试得到相同输出。调用方直接提交带 TTL 的请求，即使字节等于改写结果，也按已有 TTL 放行。

CPA 的 explicit 模式保留仍在请求中的调用方 system 标记（默认 cloaking 迁移 system 时仍携带标记），不覆盖首条 user 的标记、不自动补断点、不升级为 1 小时，也不执行 TTL 顺序归一化；四断点上限仍执行。仅补 `5m` 无法约束 cloaking 对标记的重建，因此必须同时设置 explicit 模式。CPA 读取 `req.Payload` 和 `opts.OriginalRequest` 中的此选项，并在上游发送前删除整个 `prompt_cache_options`；正常请求与挑灯均携带该选项进入执行器。

explicit 模式不能阻止 strict 伪装（`cloak_strict_mode`）等处理删除调用方 system 及其标记；最后一个缓存断点前的前缀不足最低长度时，也可能读写量均为 0。Wick 根据挑灯响应的实际缓存读写量确认续期；读取量不大于 0 或发生缓存写入时自动熄灭，避免部分缓存过期或前缀变化后反复支付写入费用。

其他客户端改用显式 1 小时缓存时，只在同一 caller scope、会话、agent 和模型的非原生记录中查找旧对话。`cacheIdentityKey` 结构化比较 `tools` 和 `system`，忽略对象键顺序和空白；只在 `tools[]`、`system[]` 的直接 `cache_control` 对象中，将 `type: "ephemeral"` 标记的缺省、`5m` 和 `1h` TTL 视为等价。不递归处理嵌套标记；其他值（包括 `null`、自定义 TTL 和标记的其他字段）、数组顺序和数值表示仍参与匹配，计费块沿用对话键的排除规则。这份 identity 只用于删除匹配记录，不改变已保存的对话键、会话哈希或发给 CPA、保存和重放的请求字节，也不匹配原生记录。原生请求仍按原始对话键查找。

`request.Other` 标记非原生请求，并保存在 `.req` 元数据的 `other` 中；缺省时为 `false`，对应原生请求。关闭 `other_clients` 时立即删除这些对话，恢复时也跳过它们，不影响原生对话。

## 对话识别和描述

会话 ID 在 Wick 支持的来源中按 CPA v8.0.43 的顺序取第一个有效值：`X-Claude-Code-Session-Id` → Claude 格式 `metadata.user_id` 中提取的会话 ID → `Session-Id` → `X-Session-Id` → `X-Session-Affinity` → 普通 `metadata.user_id`。候选值先检查控制字符，再去除首尾空白；含 Unicode 控制字符、去除空白后为空或超过 256 字节的值无效。请求头名称忽略大小写，同一请求头的多个值依次校验。原生 Claude Code 的合法会话头始终优先。

Claude metadata 的解析与 CPA 的 `ClaudeMetadataIdentities` 一致：以 `{` 开头时取内层 `session_id`，旧式字符串取结尾 `_session_([a-f0-9-]+)` 捕获的部分，再对提取值校验；外层 metadata 长度不限制合法的内层会话 ID。提取失败后，整个 `metadata.user_id` 仍可作为末位不透明候选，但须通过同一校验。没有有效标识时，以 `no_session` 原样放行，不返回 `Body`，不记录对话或安排挑灯。pi 可启用 `compat.sendSessionAffinityHeaders`；pi-endpoints 0.2.13 及以上版本对 Anthropic 模型默认发送 `x-session-affinity`。

CPA 的 `sdk/cliproxy/session/identity.go` 在有显式会话信号时通过 `ExtractSessionInfo` 识别会话；无显式信号时，`DeriveID` 把 caller scope 纳入身份。Wick 只跟踪显式会话，不接管仅依靠内容派生身份的请求。挑灯同时重放原请求头、请求体和 caller scope。

对话键是 caller scope、会话 ID、agent ID、模型、`tools` 和 `system` 的 SHA-256；其他客户端的工具和系统提示使用补全 TTL 后的字节。agent ID 只取 `X-Claude-Code-Agent-Id`，不从其他客户端字段推测。Claude Code 只在 subagent 的请求中发送 `X-Claude-Code-Agent-Id`，每个 subagent 的值不同，主对话没有这个请求头。`system` 中以 `x-anthropic-billing-header` 开头的计费块每次请求都不同，计算时跳过。同一对话前后的请求只有 `messages` 不同，键相同。

每个对话只保存最后一个请求。主对话（没有 agent ID 的请求）收到新请求时，同一 caller scope 和会话中首次出现时间晚于主对话上一个请求的其他对话被删除：它们是主对话等待期间启动的 subagent，主对话继续说明它们已经返回。subagent 的请求不触发删除。客户端不发送 agent ID 时，所有请求都按主对话处理。pi-subagents 属于这种情况，使用 `duration_minutes`，不使用子代理时长。

对话键包含工具、系统提示和模型。对话中途加载 MCP 服务器或延迟加载的工具（如通过 ToolSearch 加载 WebFetch）、换了系统提示或用 `/model` 换了模型时，后续请求得到新的键。缓存按 tools → system → messages 的顺序匹配前缀，旧键的缓存再也读不到，继续挑灯只会白花钱。因此出现新键时，`replaceLocked` 检查同一 caller scope、同一会话、同一 agent ID（都为空也算相同）下的其他对话：旧请求的消息除最后一条外，与新请求开头的消息逐条原样相同（比较原始 JSON），就判定为同一对话的延续，删除旧对话，日志中的熄灭原因为 `replaced`。最后一条不比较，因为 Claude Code 会把缓存标记移到新的最后一条消息上。只有一条消息的旧对话无从比较，不删除。只看会话 ID 不够：Claude Code 的后台辅助请求也带同一个会话 ID，但不是在原对话后面续写的。只有同一 caller scope、会话和 agent 下已有其他对话时才解析消息，普通请求不受影响。

内存中最多保存 32 个对话，超出时删除最后请求最早的一个；单独设了时长的对话最后删除。

页面上的描述在记录请求时取出（`describe.go`）：

- 客户端：User-Agent `claude-cli/<版本> (<用户类型>, <入口>, …)` 中的入口，如 `cli`、`claude-desktop`、`claude-vscode`、`sdk-ts`；其他 User-Agent 取第一个产品名（如 `pi/0.60 node/24` 取 `pi`），页面通过 `CLIENTS` 显示友好名称，没有映射时显示原值。

- 项目目录：第一次出现的 `Primary working directory:` 或 `Working directory:` 一行的路径，即会话开始时的目录。Claude Code 较新的版本把环境信息作为 `messages` 中 `role` 为 `system` 的消息发送，较早的版本写在 `system` 中，两处都查，`messages` 只查前 20 条。
- 第一句话：前 20 条消息中第一条用户文字。先删去 `<system-reminder>` 块，跳过本地命令的输出（`<local-command-…>`、`<bash-…>`）和 Claude Code 附加的 `Caveat: …` 说明；斜杠命令显示为 `<command-name>` 加 `<command-args>`。空白合并为一个空格，状态中最多 200 个字，挑灯记录中最多 80 个字。

## 调度

- 对话收到请求时，在请求开始时间之后的 `interval_min` 到 `interval_max` 秒之间均匀随机选一个时刻挑灯。缓存从上游收到请求时计时，所以起点是请求开始而不是响应结束；回答输出时间较长时，第一次挑灯可能在回答结束前发出。
- 每次挑灯成功后，从这次挑灯的开始时间起按同样规则安排下一次。
- 每个对话挑灯到截止时间 `untilLocked`：最后一个请求的开始时间加上时长。时长取页面上单独设置的 `minutes`，没有时由 `defaultMinutesLocked` 按配置决定：带 agent ID 的子代理用 `subagent_duration_minutes`，其余用 `duration_minutes`；`duration_minutes` 为 0 时子代理也不挑灯。某次挑灯开始后 300 秒（缓存的有效期）不早于截止时间时熄灭（`doneLocked`），因此缓存至少保持到截止时间。截止时间在挑灯返回时按当前配置计算，修改配置后，缓存已能保持到新截止时间的对话立即熄灭；恢复保存的对话时同样跳过这类对话。
- 时长不超过 5 分钟时，请求本身的缓存已经够用，记录请求后立即删除对话，不挑灯。
- 累计次数不存在管理器中：对话熄灭后被删除，再出现时是新的对话，所以状态接口从挑灯记录统计，记录不会因熄灭或重启丢失。
- 新请求让本轮次数清零，并清除单独设置的时长。`SetDuration` 拒绝会让对话立即熄灭的时长（缓存已经能保持到它，包括恢复默认时长），返回 `ErrPassed`，熄灭只能通过“熄灭”按钮。
- 每次安排都从管理器的全局计数器取一个代数（`gen`）。计时器触发和挑灯返回时都核对代数，旧的计时器和结果不影响新的安排；对话被删除后重新出现时，进行中的旧挑灯也不会与它匹配。
- 时长为 0 时同理：`duration_minutes` 改为 0 时清空所有对话，`subagent_duration_minutes` 改为 0 时删除所有子代理。时长为 0 的请求不记录。
- 每次挑灯前读取 CPA 配置文件，插件被关闭时删除对话。CPA v8.0.43 在插件被关闭时卸载实例（与 `plugin.shutdown` 相同，见下文），这项检查用于卸载前的间隙和不卸载实例的 CPA 版本。
- `plugin.quiesce` 和 `plugin.shutdown` 先保存对话，再停止所有计时器、清空内存中的对话，并关闭进行中的挑灯流；磁盘上保存的对话供下一个实例恢复。之后的 `plugin.reconfigure` 恢复运行，并重新从磁盘恢复对话。插件被关闭后在缓存过期前重新启用，保存的对话也会恢复。
- 从管理页面熄灭对话时，删除该对话。进行中的挑灯照常完成，返回时因找不到对话而不再安排下一次。
- 因到达截止时间、修改配置、手动熄灭或淘汰而删除的对话，如果本轮挑过灯，`retireLocked` 把这一轮另存在 `ended` 中，保留到最后一次读取缓存后 300 秒，用来判断这段时间内的回来。挑灯未命中或失败、插件关闭、子代理结束和换键删除的对话不保留。`ended` 不保存到磁盘，重启或热更新时丢失，这期间的回来不会被记录。
- 计时器回调运行在独立的 goroutine 中，未恢复的 panic 会使 CPA 进程退出，因此回调带 `recover`。

## 轮次和省耗

### 回来

对话收到请求时，如果它本轮挑过灯（正在挑灯，或在 `ended` 中），`Manager.Observe` 往挑灯记录写一条 `kind` 为 `return` 的事件，记下 `saved`、距上一个请求的秒数 `idle`、本轮最后一次成功挑灯的输入 `context` 和本轮次数 `pings`，`ping` 为 0。`saved` 的条件是距最后一次成功读取缓存（`lastRead`）不到 300 秒，并且距上一个请求（`lastRequest`）至少 300 秒：前者说明缓存靠挑灯保持着，后者说明不挑灯缓存已经过期。

### 轮次重建

`history.Group` 从记录重建轮次，轮次本身不写进文件，旧记录也能拆分。同一对话键的事件按时间排序，`ping` 序号不大于上一条，或上一轮已有回来事件时，开始新的一轮；回来事件结束当前一轮。没有对应挑灯的回来事件（如挑灯在所选范围之外）被忽略。`request` 事件不参与轮次重建。

每轮的结论 `Round.Result` 按以下顺序判断：

1. 有回来事件：`saved` 为真时为 `saved`，否则为 `wasted`。请求与在途挑灯并发时，回来事件按请求到达时的缓存状态判定；该挑灯随后失败或未命中，不覆盖已确定的回来结论。
2. 没有回来事件，且本轮最后一次挑灯失败、未读到缓存或发生缓存写入：`failed` 或 `miss`；已记录的 `cache_miss` 停止原因也判为 `miss`。
3. 最后一次挑灯没有 `returns` 标记，即由 v0.5.0 之前的版本写入：`unknown`，只显示轮次，不计金额。
4. 最后一次成功挑灯后不到 300 秒：`pending`。
5. 其余为 `wasted`：超过 300 秒没有回来，结论已定，不必等待。换键删除的对话不会再有回来事件，按这一条算作 `wasted`。

v0.5.0 起每条事件都带 `returns: true`，用来区分“没有回来”和“当时的版本不记录回来”。

### 金额

金额在查询时按当前价格计算，不写进事件：

- 每次挑灯的花费为 `cache_read × 读取价 + cache_creation × 5 分钟写入价 + input × 输入价`。挑灯在 `message_start` 断开，输出按 0 计。分档价格按这次挑灯的输入总量选档。
- `saved` 轮省下的金额为回来事件的 `context` × (5 分钟写入价 − 读取价)。
- 汇总中的 `cost` 和 `saved` 包括 `pending` 轮；`net` 为 `saved` 减去有结论的轮次的花费，不含 `pending` 轮。`unknown` 轮和模型没有价格的轮次不计入这三项，后者计入 `unpriced`。

### 请求用量

花费对比需要每个请求读写缓存的 token 数，而插件只拦截请求，看不到响应。因此插件注册 `usage_plugin`，CPA 在每个请求完成后通过 `usage.handle` 发来用量记录（`internal/keepalive/usage.go`）：

- `Observe` 记录一个请求时（只记录 Wick 挑灯的对话），`awaitLocked` 保存一条待补全的 `request` 事件，键为入站 HTTP 请求的 trace ID，没有时为请求 ID：CPA 在用量记录中给每次上游执行另起一个请求 ID，与拦截时的请求 ID 不同。CPA 对同一请求的每次尝试（如换账号重试）都调用拦截器，trace ID 相同，只保留第一次尝试的事件，以免空闲时间被重试清零。上一个请求不到 1 小时前时，事件中写入距它的秒数 `idle` 和它的输入 `context`。上一个请求记在 `seen` 中；`seen` 中没有时（如热更新后），用正在挑灯的对话的最后请求时间，以及本轮最后一次挑灯的用量（本轮挑过灯时）。
- `Usage` 按同样的键查找用量记录对应的请求，补上 `cache_read_input_tokens`、`cache_creation_input_tokens` 和 `input_tokens` 后写入挑灯记录，并把这次的输入总量记为该对话上一个请求的输入。`Failed` 的记录忽略，请求继续等待，同一 trace 下的重试仍能匹配。找不到的记录（包括挑灯请求和未跟踪的客户端请求）忽略。
- 等待超过 1 小时的请求和 `seen` 中超过 1 小时的对话被删除，两者各最多保留 1000 项，都不保存到磁盘。

### 花费对比

`history.Compare` 用同一范围的事件和 `Group` 的结果计算 `Comparison`。金额按查询时的价格计算，分档价格按请求的输入总量选档：

- `cache`：每个 `request` 事件的 `cache_read × 读取价 + cache_creation × 5 分钟写入价`。
- `hour`：同一请求在 1 小时缓存下的费用。读取量默认与实际相同，其余缓存部分按 1 小时写入价计算。`idle` 不小于 300 秒、且实际读取量小于 `context` 时，5 分钟缓存已经过期，1 小时缓存还在，读取量改为 `context` 与缓存部分总量中较小的一个。`saved` 轮中 `idle` 不小于 3600 秒的，回来时 1 小时缓存已经过期，再加上 `context` × (1 小时写入价 − 读取价)。
- `pings` 和 `saved`：结束时间（回来的时间，没有时为最后一次挑灯的时间）不早于 `since` 的轮次的花费和省下的金额，`pending` 轮的花费计入，`unknown` 轮不计。
- `since` 是范围内第一个有用量的 `request` 事件的时间，在它之前结束的轮次不计入，三种做法覆盖同一段时间。模型没有价格的请求和轮次计入 `unpriced`。
- `ping_count`、`failed`、`rounds` 和 `results` 汇总结束时间不早于 `since` 的同一批轮次，包括无价格和 `unknown` 轮次；`Round.Failed` 统计轮内失败挑灯次数，结论为省下的轮次也可能包含失败。金额仍排除 `unknown` 和无价格轮次。
- `wick = cache + pings`、`none = cache + saved`；`difference = none - wick`，`hour_difference = hour - wick`，正值表示 Wick 更便宜。`none > 0` 时 `percent = difference / none × 100`，否则省略，避免除零。这些字段用于标题和三条横条，前端不计算业务差额或轮次。未缓存的输入和输出三者相同，不计入。

### API 价格

`internal/pricing` 读取官方定价页的 Markdown 版本 `https://platform.claude.com/docs/en/about-claude/pricing.md`：

- 解析 `## Model pricing` 下的第一张表，按表头找到 Base input、5m cache writes、1h cache writes、Cache hits and refreshes 和 Output 五列，单价格式为 `$x / MTok`。
- 模型名去掉括注，如 `([limited availability](…))`。“(for prompts up to / over 100,000 tokens)” 这类括注解析为分档价格，按输入总量（读取、写入和未缓存的输入之和）选档。显示名拆成系列和版本号，如 “Claude Opus 5.5” 的系列为 `opus`、版本为 `5.5`，ID 为 `claude-opus-5-5`。
- 查价格时，CPA 的模型名转为小写，去掉方括号后缀（如 `[1m]`），拆成单词和版本号。版本号取第一段连续的一两位数字，之后的日期和数字（如 Bedrock ID 末尾的 `-v1:0`）不计入；表中和模型名的版本号都去掉末尾的 `.0`，所以 `claude-opus-4-0` 对应 Claude Opus 4。版本号相同、并且包含表中系列的全部单词时匹配，所以 `-thinking` 等后缀和老型号的词序（如 `claude-3-5-haiku`）都能匹配。匹配不到的模型不计金额。
- 只解析模型价格表。Claude Sonnet 4 和 4.5 的提示超过 200K token 时按长上下文价格计费，这部分加价不计入，金额偏低。
- 插件加载时获取一次，成功后每 24 小时获取一次，失败后 1 小时重试；使用标准 `net/http`，超时 15 秒。成功后写入数据目录的 `prices.json`（带获取时间），插件加载时先读这个文件。获取或解析失败时沿用当前价格，在日志和页面上提示。解析出的模型少于 5 个时按失败处理，避免定价页改版后覆盖可用的价格。
- `plugin.quiesce` 和 `plugin.shutdown` 停止获取；之后的 `plugin.reconfigure` 重新开始，并立即获取一次。

## 数据目录

数据目录为配置项 `data_dir`，为空时为 `<plugins.dir>/data/wick`（`config.DataDir`）。`plugins.dir` 从 CPA 配置文件读取，相对路径相对于 CPA 的工作目录；CPA 从临时目录中的副本加载插件，所以不能用动态库自身的路径推算。目录权限为 `0700`，文件为 `0600`。

### 正在挑灯的对话

`internal/keepalive/persist.go` 把内存中的对话保存在 `conversations/` 下：

- `index.json`：每个对话的键、首次出现时间、最后请求时间、最后一次读取缓存的时间 `last_read`、下次挑灯时间、本轮的挑灯次数、单独设置的分钟数、上次的用量。
- `<对话键>.req`：第一行是请求的元数据（调用方标识 `caller_scope`、会话 ID、agent ID、非原生标记 `other`、模型、账号、请求 ID、目录、第一句话、请求头）的 JSON，之后是原样的请求体。请求体不经过 JSON 编码，重放时与原请求逐字节相同。

对话变化时（新请求、挑灯、设置时长、删除）管理器发出信号，后台 goroutine 等待 5 秒合并同一批变化，再在锁内取快照、在锁外写文件。请求文件只在该对话的代数变化时重写，其余变化只重写 `index.json`。写文件先写 `.tmp` 再改名；不在快照中的文件被删除；对话键必须是 64 位十六进制，否则不读取。

`plugin.quiesce` 和 `plugin.shutdown` 在清空对话之前取最后一次快照，释放锁后让 goroutine 退出并等待它结束，再同步写入这份快照。热更新时 CPA 在加载新实例之前对旧实例调用 `plugin.quiesce`，所以新实例读到的是旧实例停止时的状态。CPA 在调用 `plugin.quiesce` 之后、切换到新实例之前收到的请求不会被任何一个实例记录，这样的对话恢复后重放的是上一轮请求，直到它收到下一个请求。

`plugin.register` 或 `plugin.reconfigure` 设置目录后，管理器在本实例启动或恢复后第一次设置目录时读取保存的对话：

- 下次挑灯时间不早于加载后 3 秒，给 CPA 完成插件加载留出时间；也不晚于 `last_read` 之后 290 秒（缓存有效期减去 10 秒，留给请求到达上游），`interval_max` 大于 290 时保存的时间可能更晚。
- 如果加载后 3 秒已经晚于 `last_read` 之后 290 秒，缓存视为已过期，不恢复，下一次写入时删除其文件。
- 恢复的对话取新的代数，次数、单独设置的分钟数和最后请求时间保持不变，重放携带保存的 caller scope。
- 请求文件的首行总是写入 `caller_scope`，值为空也写。没有这个字段的文件是 v0.8.0 之前保存的，无法知道原来的调用方，恢复时全部丢弃，原生和其他客户端都一样，并写一条 info 日志说明丢弃了几个。

caller scope 只用于重放和区分对话，不出现在管理页面、status API、日志和 history 事件中。

### 挑灯记录

`internal/history` 把每次挑灯、每次回来和每个请求的用量（`keepalive.Event`）追加到 `history/<UTC 日期>.jsonl`，每行一个 JSON 对象，每次用一个 `O_APPEND` 写入一整行，热更新时新旧实例短暂并存也不会写坏文件。每天第一次写入时删除早于 `history_retention_days` 天的文件。查询时按日读取文件，按文件大小和修改时间缓存解析结果，文件未变时不重新解析。

记录接口把查询结果交给 `history.Group`，按对话键（前 8 位）汇总并拆分轮次，同一个对话在所选范围内只返回一项；没有对话键的记录按会话 ID 汇总。状态接口的累计次数（`Store.CountByKey`）和汇总都只计挑灯事件，不计回来和请求事件。

### API 价格

`prices.json` 是最近一次成功获取的价格表（`pricing.Table`）：获取时间、来源和每个模型的名称、ID、系列、版本号和分档价格。

## 开发

### 环境

- Go 1.26.10 或更新版本；Docker 构建镜像和 CI 工具链固定为 1.26.10。
- 构建动态库需要 cgo 和目标平台的 C 编译器。`internal/` 下的包都是纯 Go，测试不需要 cgo。

### 测试

```bash
go vet ./...
go test ./... -timeout=90s
```

测试使用假的宿主和时钟，不发送真实请求，也不等待真实时间；持久化的测试在临时目录中直接调用写入函数，不等待后台 goroutine。覆盖内容：其他客户端默认关闭、显式开启、原生请求不受开关影响和关闭后的恢复筛选、合法缓存位置的 1～4 断点接管和逐字节 5 分钟 TTL / explicit 模式插入、保留选项其他字段、已有 TTL / 无标记 / 超过四断点 / mode 冲突时原样放行、原生请求路径不变、原请求与持久化及重放字节一致、重试稳定性、CPA 会话来源优先级与长度和控制字符校验、结构化和旧式 Claude metadata 的会话提取、无效候选后的有效值及多值请求头、五种显式会话标识的接管和重放、无有效标识时原样放行、显式 1 小时 TTL 的结构化匹配及无关对话隔离、CallerScope 字段优先级和 metadata 回退（含空 scope）、保存恢复后重放身份、旧格式丢弃及日志、不同调用方同 session 的独立计时和三处清理隔离、同调用方清理及 RPC 身份解码、拦截响应 `Body`、花费对比范围统计和差额视图、请求筛选和挑灯请求体（包括去掉客户端凭据）、对话键、项目目录和第一句话的提取、挑灯间隔和按时长停止、5 分钟以内不挑灯、子代理的时长（包括单独时长和 `subagent_duration_minutes: 0`）、从记录统计的累计次数、新请求重新计时、零读零写、零读有写或读写并存时熄灭且不刷新 `lastRead`、仅读取无写入时续期、缓存未命中的页面结论和错误时熄灭、subagent 的删除（包括带 agent ID 的并行 subagent）、失败和中断请求的处理、插件关闭和 `duration_minutes: 0`、过期的挑灯结果、单独时长的截止和清除、拒绝已经过去的时长、淘汰时保留单独时长的对话、对话的保存和恢复（计划保持、到期补发、晚于截止点时提前、缓存过期不恢复、停止前的最后状态、文件删除）、挑灯记录的写入、查询、按对话汇总和清理、SSE 解析、日志正文、配置校验和数据目录，手动熄灭对话，插件 RPC 的注册和拦截请求解码，管理接口和页面，换了工具后熄灭旧对话（辅助请求和其他 agent 不受影响），回来事件的判定（及时回来、过早、过晚、手动熄灭后回来、换键后不记录），轮次重建、结论和金额，请求用量的记录（按 trace ID 和请求 ID 匹配、失败后重试、超过 1 小时）和 `usage.handle` 解码，花费对比，以及定价页的解析（以 2026-10-24 的官方页面 `internal/pricing/testdata/pricing.md` 为样例）、模型名匹配和价格缓存。

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
- 修改 `request` 事件的字段或 1 小时缓存的算法时，同步修改 `history.Compare` 和 README 的“省耗核算”。
- 新增的 goroutine 必须有 `recover`。
- 定价页的表格结构变化时，用新页面替换 `internal/pricing/testdata/pricing.md`，再修改解析。
- 更换 `assets/logo.png` 时，同步生成 64 px 的 `internal/web/logo.png`，并发布新版本，页面上的图标才会更新。
- 用户可见的行为、配置和限制写进 README（中英文同步），实现细节写进本文档。
