# Nanorouter 设计文档

本文是 Nanorouter 的架构与关键取舍记录。代码、配置示例和部署模板分别回答“怎么实现”“怎么接入”和“怎么运行”；本文回答“为什么这样设计”。当路由、协议适配、账本、可靠性或安全边界发生变化时，应同步更新本文。

## 目标与全景

Nanorouter 是一个面向 OpenAI 兼容客户端的多上游网关。下游只知道逻辑模型名
`free-fast`，网关才知道真实模型、供应商密钥和故障状态。这样替换免费模型、调整权重或
下线上游不需要改 Agent，也不需要让下游保存多家供应商的 API Key。

```text
Agent / Open WebUI
       │ model=free-fast
       ▼
 Nanorouter :8080/v1
       │ 鉴权、准入、选路、适配、故障切换
       ├── OpenRouter / Agnes / SenseNova
       ├── Mistral / Cerebras / Nous Research
       └── 本地 llama-server
       │
       ├── SQLite 或 PostgreSQL 调用账本
       ├── Zap 结构化日志
       └── Prometheus 指标
```

## 设计原则

1. **逻辑模型与供应商解耦。** 下游使用稳定的路由名；供应商模型 ID、权重、密钥和熔断状态
   都属于候选配置，不泄露到客户端协议之外。
2. **透明转发优先。** 请求先以通用 JSON 保存，只有路由正确性、协议适配和已确认的供应商
   兼容性问题才修改字段；未知字段默认保留。
3. **能力差异必须显式。** 原生 Responses、Responses→Chat 降级和仅 Chat 三种能力通过配置
   区分。不能无损映射的 Responses 特性返回明确的 400，不能静默丢弃。
4. **故障切换只发生在安全边界内。** 上游尚未向下游写出 SSE 内容时可以切换；流式内容开始后
   只能结束当前流，不能把两个模型的输出拼接起来。
5. **有界资源优先于无限等待。** 全局背压、候选并发、请求超时、重试预算和请求体上限共同
   限制慢上游造成的内存、连接和请求放大。
6. **内容与密钥分离。** 日志和数据库只保存排障所需的元数据，不保存 prompt、回复、Cookie、
   Authorization 或 API Key。

非目标包括：实现所有供应商的完整私有协议、把网关变成通用 tokenizer 服务、自动裁剪或摘要
任意长上下文，以及在没有共享协调层的情况下提供跨实例一致的熔断状态。

## 模块边界

| 模块 | 职责 | 刻意不负责 |
| --- | --- | --- |
| `config.go` | 读 YAML、验证必要字段 | 确认供应商模型 ID 是否仍可用 |
| `router.go` | 鉴权、选路、转发、切换 | 解释每一种模型参数语义 |
| `usage.go` | 从 JSON/SSE 提取用量 | 用 tokenizer 猜缺失 token |
| `store.go` | 建表、写调用账本 | 保存 prompt、回复或密钥 |

代理只将 JSON 解为 `map[string]any`，修改 `model` 后重新编码，保留其余字段。因此 `tools`、图片、结构化输出等新增字段不需要随网关升级逐一建模。

## 分层与数据流

运行时按四层组织：

| 层 | 主要内容 | 设计重点 |
| --- | --- | --- |
| HTTP 边界 | `/v1/chat/completions`、`/v1/responses`、`/v1/models`、`/healthz`、`/metrics` | 鉴权、请求体上限、客户端取消和响应头 |
| 路由层 | 逻辑路由、权重、会话亲和、熔断、重试和并发槽 | 决定“发给谁”，不解释供应商字段语义 |
| 适配层 | Responses→Chat、字段清洗、模型名和 token 参数改写 | 每个候选使用独立请求副本，避免污染故障切换的原始请求 |
| 记录层 | usage 解析、调用账本、日志和 Prometheus | 记录事实，不用估算值冒充供应商 usage |

一次请求的核心路径如下：

```text
下游请求
   │
   ├─ 全局背压准入（读取正文前）
   ├─ Bearer 鉴权、16 MiB 正文限制、JSON 解析
   ├─ model → 逻辑路由
   │
   └─ 候选循环
       ├─ 检查 enabled / endpoint 能力 / 熔断状态
       ├─ Responses 适配、字段清洗、输出 token 限制
       ├─ 候选并发槽与 Retry Budget
       ├─ 替换真实 model，注入候选 API Key
       ├─ 发起上游请求
       └─ 成功回传或按错误策略切换/重试
```

选择候选后才进行供应商级清洗和改写，是为了保证：同一请求切换到下一候选时，下一候选仍能
看到未被上一候选修改的原始字段。网关当前也不做输入上下文 token 预检查或自动截断；缺少
可靠的供应商 tokenizer 时，宁可让上游返回明确的上下文错误，也不把不准确的估算写入账本
或悄悄删除对话历史。

## 一次请求的生命周期

1. 客户端向 `/v1/chat/completions` 或 `/v1/responses` 发请求。网关只接受这两种生成协议，
   不提供旧式 `/v1/completions` 入口。
2. 网关先通过可选的全局背压准入，再限制请求体最大 16 MiB，解析 JSON 并读取 `model` 作为
   逻辑路由名。未知路由、非法 JSON 或鉴权失败在本地结束，不产生上游请求。
3. 从未冷却、且支持目标端点的候选中选择一个：存在非空 `X-Session-Affinity` 时按路由名和
   会话键做稳定的加权哈希，否则按权重轮转。
4. 为该候选创建请求副本，执行协议适配、字段清洗和最大输出 token 限制，然后将逻辑名替换为
   真实模型 ID，以该上游自己的 Bearer Key 转发。
5. 收到上游响应头后，先决定是否可以切换；成功响应直接回传，SSE 则逐行转发。非流式响应和
   SSE 结束时解析上游 `usage` 并写调用账本。
6. 网络错误或可重试 HTTP 错误时，将候选冷却后尝试下一个；不可安全切换的错误直接返回，并
   保留上游的诊断响应。

候选可以通过 `max_concurrency` 设置独立的在途请求上限。满载请求会最多排队
`queue_timeout`；超时后优先尝试路由中的其他候选，所有候选均满载时返回 429。局部并发
限制不是上游故障，因此不会触发候选冷却。SSE 从开始转发到结束始终占用一个槽位。

`attempt_timeout` 是候选级完整调用期限，防止一个上游独占全局请求预算。自适应并发开启后，
limiter 以 `min_concurrency` 起步：有排队且 TTFB 达标时加性增长，429 或超过两倍目标 TTFB 时
乘性下降，最大不超过 `max_concurrency`。该状态仅在单实例内维护。

全局背压使用固定在途槽和有界等待队列，并在读取请求体前准入，以限制慢上游期间的 goroutine 与正文内存增长。路由级 Retry Budget 使用令牌桶：初始请求按比例补充令牌，真实额外上游调用消耗令牌，既允许有限突发故障切换，又限制长期请求放大率。这两类状态均为单实例内状态。

聚合路由会在网络错误、`401/403/404` 候选认证或模型错误，以及
`408/425/429/500/502/503/504` 临时错误时切换候选。`400/422` 往往代表请求参数不兼容，
换模型可能掩盖客户端或清洗配置问题，因此直接回传。通过 `expose_as` 明确选择候选时，
任何 HTTP 错误都原样返回，不进行故障切换或内部重试。

## 权重和熔断

`weight: 3` 的候选在轮转序列中占三个逻辑槽位，大约承受三倍请求。实现使用累计权重定位，不会按 weight 展开候选切片。`Router.mu` 保护轮转游标和冷却状态，防止并发请求同时涌向刚发生 429 的上游。

`X-Session-Affinity` 是可选提示，不是可用性约束。同一路由和候选集合下，相同键稳定落到同一
加权槽位；候选冷却、拥塞或请求内失败后仍允许选择其他候选。缺少或空白 Header 时保持原有轮转行为。

收到可故障切换错误后，上游进入冷却。冷却结束进入半开状态，只有一个请求能作为探针；探针成功关闭
熔断器，失败则重新冷却，避免并发流量在同一时刻冲回上游。对于 429，当前请求会先尝试其他候选；如果一轮候选都不可用，
则按 `retry` 配置在网关内重试。默认指数窗口为 1、2、4、8、16 秒，每次实际等待随机落在该窗口的
50%–100%，避免并发实例同步重试。每个候选的 `Retry-After` 都是其最小冷却时间；当前请求只等到最早可恢复的候选，而不会被最慢候选拖住。每次重试都受整个 `request_timeout` 和客户端取消信号约束，因而不会要求下游盲目重复提交。
其他可重试错误仍采用 30 秒冷却（有 `Retry-After` 时优先服从）。这是适合单实例的轻量熔断器；多实例时
各自状态不共享，下一步可迁移到 Redis。

内部原地重试白名单为 `429/502/503/504`。候选级 `retry` 的已配置字段覆盖全局策略，未配置字段继承全局；`expose_as` 只重新调用明确指定的候选。每个候选分别计算退避和 `Retry-After`，调度器选择最早可恢复者。所有第二次及后续真实上游调用均消耗 Retry Budget；已经开始向下游写 SSE 后不再进入调度器。

## 配置模型与上游接入

配置分为三层：

| 层级 | 典型字段 | 作用 |
| --- | --- | --- |
| 全局 | `listen`、`auth_env`、`request_timeout`、`retry`、`backpressure`、`database` | 进程边界、默认超时、默认重试、资源上限和账本位置 |
| 路由 | `strategy`、`candidates`、`retry_budget` | 一个下游逻辑模型对应的候选池和请求放大预算 |
| 候选 | `base_url`、`model`、`api_key_env`、`weight`、`responses`、并发与清洗字段 | 一个真实供应商模型的协议、容量和运维策略 |

候选的 `id` 是内部稳定标识，`model` 是转发给供应商的真实模型 ID，`expose_as` 是可选的
下游直连别名。`enabled` 用于整体上下线；`aggregate_enabled` 只控制是否加入父路由的候选池。
密钥只通过 `api_key_env` 从环境变量读取，YAML、日志、账本和测试文件都不放真实密钥。

接入一个新的 OpenAI 兼容 Chat 上游，通常只需增加候选配置和文档；如果它的字段、错误响应或
路径不兼容，再在 `internal/router/` 增加小范围适配，而不是把供应商分支塞进 HTTP 入口。以
Nous Research 为例，网关请求其 `/v1/chat/completions`，`/v1/responses` 通过受限 Chat 适配器
转发；配置示意如下：

```yaml
- id: nous-hermes-4-3-36b
  base_url: https://inference-api.nousresearch.com/v1
  api_key_env: NOUS_API_KEY
  model: Hermes-4.3-36B
  expose_as: hermes-4.3-36b
  weight: 1
  responses_via_chat_completions: true
```

生产部署模板可以包含候选但保持 `enabled: false`，直到对应 Secret 存在并完成小流量验证。
这样配置发布不会因为缺少某一个可选供应商的密钥而影响现有候选。

## 协议边界与能力声明

网关公开两个生成入口：`/v1/chat/completions` 和 `/v1/responses`。上游实际调用由候选能力决定：

| 下游请求 | 候选配置 | 上游请求 |
| --- | --- | --- |
| Chat Completions | 任意可用候选 | 默认 `/chat/completions`，仅替换 `model` |
| Responses | `responses: true` | 原生 `/responses`，尽量保留原始 schema |
| Responses | `responses_via_chat_completions: true` | 转换为 `/chat/completions`，再包装回 Responses |

配置中的 `responses: true` 是上游能力声明；Responses 请求会跳过没有该标记的候选。Chat
Completions 与原生 Responses 不会互相转换 schema，只有显式启用 `responses_via_chat_completions`
时才进入受限适配器。这样供应商能力差异会在配置中可见，而不是在请求失败后隐式猜测。

对于 Nous、Agnes、SenseNova 这类只提供 Chat Completions 的上游，可以改用
`responses_via_chat_completions: true`。这是一个受限适配器：`input`、`instructions`、图片/文本
content part、采样与推理强度参数、结构化输出、自定义函数定义/调用/结果和最大输出 token 能够
映射；结果会从标准 `choices` 或供应商扩展的 `data.choices` / `data.usage` 转成 Responses
对象或完整的文本/函数调用 SSE 事件。OpenAI 托管工具、后台任务、状态链、prompt 模板和
reasoning summary 没有纯 Chat Completions 等价表示，必须返回 400，而不能假装成功。

`chat_max_tokens_field` 解决供应商参数命名差异：默认是 `max_tokens`（适用于 Agnes、SenseNova 新版接口一类标准化服务）；对于少数使用非标准命名的上游（如旧版 SenseNova 原生接口的 `max_new_tokens`）需在候选中显式配置。`chat_completions_path` 则解决路径差异，默认 `chat/completions`，旧版 SenseNova 原生接口是 `chat-completions`。当前 SenseNova 已全面迁移至 `token.sensenova.cn/v1` 标准化端点，无需配置这两个字段。Groq 提供原生 `/responses`，应使用 `responses: true` 而不是该适配器。

候选的 `enabled` 是总运维开关：省略时默认开启，设为 `false` 时聚合路由和 `expose_as` 入口都会跳过它。`aggregate_enabled` 是独立的聚合开关，省略时默认开启；设为 `false` 后候选不参与父路由的轮转与故障切换，但仍可通过 `expose_as` 直接访问。父路由没有任何已启用且参与聚合的候选时，不会出现在 `/v1/models`，但其中已启用的公开候选仍会单独列出。

Cerebras `gpt-oss-120b` 与 Mistral 一样使用原生 Chat Completions 加 Responses 适配的组合。它的普通 Chat 路径可保留工具调用与结构化输出；若下游一定需要完整的 Responses 工具/状态语义，应优先路由到 Groq 或 OpenRouter 的原生 Responses 候选。

## 上游参数清洗

兼容不等于每个字段都支持。候选可声明 `drop_request_fields` 与 `drop_message_fields`，网关仅在请求已选定该候选后删除这些字段。这样 Groq 的不支持字段不会导致 400，同时切换到其他供应商时仍保留原请求。清洗适合无副作用的观测、日志概率或元数据字段；会改变结果数量、工具执行或状态机行为的字段不应自动改写。

## 流式响应的边界

SSE 首字节发给下游后不能再安全换模型：会重复文字，或破坏 function-call ID 和参数。因此只有连接建立前、或可见 HTTP 状态码阶段的故障可切换；已经成功开始的流只透明转发。

实现按行读取 SSE，不将长回答完整缓存在内存；它观察 `data:` 内 JSON，最后采集到的 `usage` 写入账本，但绝不改写下游收到的事件。

下游客户端取消（断开连接）会取消请求 context，使对上游流的读取中断，但这不代表上游故障：网关区分 `client_canceled` 与真正的上游流错误，只有后者才会冷却候选并计入故障切换，避免一次客户端取消导致该路由在 30 秒内全部 503。

## go-json 的位置

`github.com/goccy/go-json` 是 `encoding/json` 兼容实现，项目将它用在请求解码、真实模型名重编码、模型列表和 usage 解析这些热路径。HTTP/SSE 和数据库驱动仍各自使用专用库，故替换 JSON 库不会改变 API 行为。

## 数据库、日志与指标

数据库只承担历史调用账本，不参与实时选路。配置可以使用 SQLite 或 PostgreSQL；两者共享同一
迁移和记录结构，PostgreSQL 仅在 SQL 占位符上使用方言转换。生产环境通过 `dsn_env` 读取
PostgreSQL DSN，避免连接凭据进入 ConfigMap。

## 用量与隐私

`llm_requests` 一行代表一次**上游尝试**。一次请求先 429 后成功会有两行，便于计算故障率和切换次数。

- `route`：下游逻辑模型名；
- `upstream_id` / `upstream_model`：实际落点；
- `attempt`：第几次候选尝试；
- `usage_source`：`upstream` 或 `unavailable`；
- `error_kind`：常见值包括 `network`、`attempt_timeout`、`upstream`、`client`、`stream`、
  `concurrency` 和 `client_canceled`；它描述网关观察到的失败阶段，不等同于供应商的完整错误类型。
  `client_canceled` 表示下游客户端在请求完成前断开了连接，不触发候选冷却。

账本不保存 prompt、回复正文或 API Key，避免用户内容和密钥落入普通运维数据库。Prometheus 用于实时请求率、失败率、token 速率，以及上游 TTFB、并发槽排队、内部退避、动态并发窗口、全局背压和重试预算观测；数据库用于按模型汇总历史使用量。数据库记录的是上游实际返回的 usage；缺失时标为 `unavailable`，不把本地估算值伪装成真实用量。

SQLite 可选异步批量写：正常关闭会排空队列，队列溢出或异常退出可能丢失尚未提交的记录，因此
生产环境应结合 `nanorouter_ledger_dropped_total` 和 `nanorouter_ledger_write_errors_total` 监控
账本完整性。账本写入失败（队列满、已关闭或数据库错误）会额外记录 `ledger_write_failed` 事件，
携带 `request_id`、路由、上游和错误类别，便于按请求定位丢失的账本记录。同步账本写入使用独立的
有界 context（不受客户端取消影响），因此下游断开连接时记录仍会落库。实时熔断、并发窗口、
背压队列和 Retry Budget 当前都是单实例内存状态，数据库不会改变它们。

运行日志使用 Zap 的结构化 JSON；`-debug` 则用相同字段输出稳定的 `key=value` 文本。每个请求以
`request_started` 开始，随后记录 `upstream_attempt_started`、`upstream_attempt_finished` 和必要时的
`retry_scheduled`，并以 `request_completed` 或 `request_failed` 收束。所有事件共享 `request_id`，方便
Loki、Elasticsearch 或 `jq` 追踪。开始事件将客户端 IP、User-Agent、请求体大小和会话亲和键是否存在
记录为扁平字段，不输出完整 Header map 或会话键值；日志始终不记录 Authorization、Cookie、prompt、
回复或密钥。

## 测试与变更方法

测试不调用真实供应商 API。路由测试使用 `httptest` 模拟上游，数据库测试使用内存 SQLite；重点
覆盖成功和失败两条路径：

- 429、5xx、网络错误和超时的候选切换、冷却与内部重试；
- 权重轮转、会话亲和、禁用候选、`expose_as` 和并发排队；
- Chat 请求的真实模型替换、鉴权头、候选级字段清洗和最大输出 token 限制；
- 原生 Responses、Responses→Chat 适配、工具调用、结构化输出和 SSE usage；
- 数据库账本的逐次上游尝试、错误阶段和实际 usage 记录；
- 不支持的 Responses 特性必须在本地显式失败，且不应产生上游调用。

新增供应商时，先判断它是否完全符合 Chat/Responses 协议：完全兼容只增加配置、文档和
`httptest` 案例；路径、字段或响应格式不兼容时才增加候选级适配。每次改动至少运行：

```sh
gofmt -w cmd internal
go test ./...
go vet ./...
go build -buildvcs=false ./cmd/nanorouter
```

## 部署边界

本地运行从 `config.yaml` 读取配置；Kubernetes 部署将非敏感配置放入 ConfigMap，将网关鉴权密钥、
PostgreSQL DSN 和上游 API Key 放入 Secret，并通过 `envFrom` 注入。生产模板中的可选上游可以先
以 `enabled: false` 发布，待密钥和小流量验证完成后再启用。ConfigMap 变化触发滚动更新，但
路由器的熔断、并发和重试预算不会跨 Pod 共享，因此多副本部署需要接受每个 Pod 独立决策，或
后续引入共享协调层。

## 推荐的下一步

1. 增加管理 API，用于查看候选、临时禁用和解除冷却。
2. 增加按路由的 RPM/token 限制，保护免费额度。
3. 为候选声明视觉、工具调用、JSON Schema 和上下文长度等能力。
4. 将单实例背压、预算和熔断状态按需迁移到共享协调层。
