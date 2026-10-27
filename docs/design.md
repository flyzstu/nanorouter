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
| `responses_adapter.go` | Responses→Chat 受限降级 | 承诺 100% Responses 语义 |
| `anthropic_adapter.go` | Chat↔Messages 双向翻译与 SSE 事件改写 | 映射不可表达的 OpenAI 特性（显式 400） |
| `zcode.go` / `zcode_login.go` | ZCode 网关常量、id 派生、凭据文件、登录链 | 写参考客户端的 token 文件 |
| `zcode_usage.go` / `zcode_reset.go` | ZCode 用量看板与重置卡查询/手动核销 | 自动核销，或在生成路径里消费重置卡 |

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
   ├─ Bearer/X-Api-Key 鉴权、16 MiB 正文限制、JSON 解析
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

1. 客户端向 `/v1/chat/completions`、`/v1/responses` 或 `/v1/messages` 发请求。网关只接受这三种生成协议（后两种均含无 `/v1` 的兼容形式），
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

收到可故障切换错误后，上游进入冷却。未启用主动探活时，冷却结束进入半开状态，由首个真实请求作为探针；探针成功关闭
熔断器，失败则重新冷却，避免并发流量在同一时刻冲回上游。当候选（或全局）配置 `probe: true` 启用健康探活时，网关转由后台异步探针（Prober）接管半开检查：候选在熔断冷却期间被严格隔离，真实用户请求直接故障切换（或在 `expose_as` 直连时本地秒回 503），绝不向上游发包傻等；冷却到期时由后台发起极简 Ping 请求（`max_tokens: 5`；OpenAI 候选额外关停思维链，Anthropic 候选发 Messages 形状、不注入推理参数），校验必须收到有效 token 生成或非空 `reasoning_content`（Messages 候选为非空内容块或正输出 token，拦截 200 伪成功与空 choices）。探测成功自动清零失败计数并恢复候选池；探测失败则按指数退避（30s、60s、120s... 上限 `max_backoff`）延后下一次探测，既避免真实用户当“探路小白鼠”，又保证健康期内零心跳、零额度消耗。对于 429，当前请求会先尝试其他候选；如果一轮候选都不可用，
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

网关公开三个生成入口：`/v1/chat/completions`、`/v1/responses` 和 `/v1/messages`。上游实际调用由候选能力决定：

| 下游请求 | 候选配置 | 上游请求 |
| --- | --- | --- |
| Chat Completions | 任意可用候选 | 默认 `/chat/completions`，仅替换 `model` |
| Responses | `responses: true` | 原生 `/responses`，尽量保留原始 schema |
| Responses | `responses_via_chat_completions: true` | 转换为 `/chat/completions`，再包装回 Responses |
| Messages（`/v1/messages`） | `anthropic: true` | 原样转发到 Anthropic Messages 路径 |
| Chat / Responses | `anthropic: true` | Chat↔Messages 双向翻译（Responses 经 Chat 链式） |

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

## 上游参数清洗与客户端仿真

兼容不等于每个字段都支持。候选可声明 `drop_request_fields` 与 `drop_message_fields`，网关仅在请求已选定该候选后删除这些字段。这样 Groq 的不支持字段不会导致 400，同时切换到其他供应商时仍保留原请求。清洗适合无副作用的观测、日志概率或元数据字段；会改变结果数量、工具执行或状态机行为的字段不应自动改写。

对于智谱 AI Coding Plan（GLM Coding Plan，聚焦 `glm-5.3` 与 `glm-5.3-flash`）这类具备严格客户端准入白名单和特化思维链交互的上游，候选可配置 `simulate_opencode: true`：
1. **Header 级白名单仿真**：自动发送 `User-Agent: opencode/1.18.30`，避免被智谱网关识别为非法代理或爬虫而拒绝请求；自动生成并传递 `X-Session-Affinity` 与 `X-Session-Id` 会话跟踪头（下游有传递时透明透传，未提供 Affinity 时自动回退使用 `X-Session-Id`），并透传子 Agent 树状调用所需的 `X-Parent-Session-Id`。支持候选级 `headers` 覆盖或增加自定义头。
2. **思维链（Thinking）载荷补齐与适配**：与 OpenCode 的 `ProviderTransform` 行为对齐，在请求未提供 `thinking` 时自动注入 `thinking: {"type": "enabled", "clear_thinking": false}`；若下游显式传入 OpenAI 规范的 `reasoning_effort: "none"`，自动转换为 `thinking: {"type": "disabled"}` 关闭思维链；并在多轮 Agent 对话中完整保留历史 assistant 消息中的 `reasoning_content`，避免长程推理思路断裂。
3. **流式统计与严格参数清洗**：流式请求自动补全 `stream_options: {"include_usage": true}` 以捕获真实用量；自动清洗智谱不支持的缓存键（`promptCacheKey`/`prompt_cache_key`）、OpenAI SDK 默认注入的 `store` 字段以及原生 `reasoning_effort` 参数，防止智谱网关报 400 校验错误。
4. **灵活端点接入**：下游既可在标准 `/v1/chat/completions` 指定 `model: zhipuaicoding`（或暴露出的真实模型如 `glm-5.3`、`glm-5.3-flash`），也可直接将客户端 Base URL 设为 `/zhipuaicoding` 或 `/zhipuaicoding/v1`，网关会在路径层面自动绑定默认路由。

## 流式响应的边界

SSE 首字节发给下游后不能再安全换模型：会重复文字，或破坏 function-call ID 和参数。因此只有连接建立前、或可见 HTTP 状态码阶段的故障可切换；已经成功开始的流只透明转发。

实现按行读取 SSE，不将长回答完整缓存在内存；它观察 `data:` 内 JSON，最后采集到的 `usage` 写入账本，但绝不改写下游收到的事件。`message_start` 与 `message_delta` 分段携带的 usage 按"各字段取已见最大值"合并——输出侧是累计值，合并对 OpenAI 单帧 usage 语义无影响。

下游客户端取消（断开连接）会取消请求 context，使对上游流的读取中断，但这不代表上游故障：网关区分 `client_canceled` 与真正的上游流错误，只有后者才会冷却候选并计入故障切换，避免一次客户端取消导致该路由在 30 秒内全部 503。

## ZCode Coding Plan（Anthropic Messages 上游）

智谱的 ZCode Coding Plan 网关说 Anthropic Messages 协议，与本项目其余上游的 OpenAI 方言不同。移植其访问链（常量、头契约、id 派生、登录兑换）时以 dsh-zhipu-provider 插件为唯一移植源——该插件又复制自参考客户端 `zcode_coding_plan_client.py`；网关路径、客户端版本与 anthropic-version 是上游合同，不做本地改名。

**两个端点、三种路径。** 下游新增 `/v1/messages`（含无 `/v1` 兼容形式）：同方言透传，只有头部注入、模型名替换与路径改写，SSE 逐字节转发、usage 旁观采集，是验证网关真实行为的最短路径。OpenAI 方言下游走双向翻译：Chat→Messages 请求翻译 + Messages→Chat 响应/SSE 翻译；Responses 请求先经既有 Responses→Chat 降级，再接 Chat→Messages，响应按反向链路折返——每一跳独立可测，不存在三方言一步转换。翻译在候选选中后执行（与其他适配一致），故障切换时下一候选看到的是未被污染的原始下游请求。

**显式失败优先。** 不可映射的下游特性（`n > 1`、`response_format` 结构化输出、非零惩罚、`logit_bias`、`logprobs`）在本地 400 `unsupported_openai_feature`，不产生上游调用——换模型会掩盖请求语义，静默丢弃会伪造等价。两处已知无声降级是经过权衡的例外：assistant 历史的 `reasoning_content`（Anthropic 拒绝未签名思维块，无签名可补）与 OpenAI 记账字段（`store`/`metadata`/`user`/`service_tier`/`seed`/`stream_options`，无生成语义）。`parallel_tool_calls` 与 `verbosity` 同属白名单式映射的自然舍弃：单候选语义下无观察差异，Anthropic 侧无对应参数。流式 Responses 链上的 thinking_delta 同理：合成的 `reasoning_content` chunk 在 Responses 事件合成器中静默丢弃（Responses 侧无 reasoning delta 事件可承载），文本与工具增量不受影响。错误信封同样按方言改写：Anthropic 的 `{"type":"error",...}` 发往 OpenAI 下游前改写为 `{"error":{...}}`，messages 端点保持原样。

**客户端仿真是头集而不是载荷改写。** `simulate_zcode` 复刻 ZCode 桌面客户端的 CLI 身份/平台指纹头、双凭据头与 `anthropic-version`；会话 id 从对话首个 user turn 的 JSON 做 sha256 截断派生（同一会话稳定、跨会话互异、不可逆），请求/追踪 id 每请求全新。与 `simulate_opencode` 不同，Messages 载荷无需注入 thinking 或补流式参数——那是 OpenAI 方言网关的特有要求。

**登录可在运行中发起，不需重启。** 设备码登录要等人在浏览器里点确认，HTTP 请求吊在那里几分钟是不合理的，故拆成两段：`POST /zcode/login` 抢占登录槽位、后台 goroutine 跑完整链路、把授权链接与各阶段进度写进内存任务并立刻返回；`GET /zcode/login` 读回进度。凭据落盘后即可被下一请求读到——凭据解析每次请求都走文件（5 秒进程内缓存），**热加载是既有语义，这里没有新增刷新机制**。

任务状态机（`idle → awaiting_authorization → exchanging → succeeded/failed`）单实例、内存态：登录是一次性人工动作，重启重来即可，持久化只会留下过期的设备码。设备码过期后 `active()` 转为 false，使下一次 POST 能重新触发——否则一次忘点的授权会把入口永久挡在 409 外面。抢占与占位必须在同一把锁内完成，否则并发 POST 会各起一条登录链、互相覆盖凭据文件。

登录链新增 `OnProgress` 回调暴露两个节点（拿到链接、进入兑换）；命令行登录不设该字段故行为完全不变。`Output` 接一个逐行写 zap 的 writer，于是授权链接与全部阶段提示自动进日志，无需为每一步新增日志语句。

**两个端点是特权面，不接受浏览器 Basic 回退。** 它们能让调用方以网关身份登录第三方账号，故不放进 `auth` 的 Basic 回退白名单（该白名单只服务统计页）。请求头认证 Bearer 与 `X-Api-Key` 皆可（与全站一致）。`auth_env` 未配置时中间件整体放行——此时这两个端点等于对所有人开放，文档已就此告警。

**凭据三套存储、一个写入方。** `login zcode` 子命令执行设备码轮询（init → 授权 URL → poll → 兑换 → 探测），把兑换并经 `max_tokens: 1` 网关探测验证的 `id.secret` key 写入 `~/.nanorouter/zcode_token.json`（0600，渠道分组，连同平台 JWT 与 OAuth token）；候选经 `zcode_token_file: true` 只读回退，ZCode 官方加密凭据 `~/.zcode/v2/credentials.json`（AES-256-GCM，种子串取 `ZCODE_CREDENTIAL_SECRET`，未设置时用 `zcode-credential-fallback:{platform}:{homedir}:{username}`；字段选择与解密 1:1 对齐 dsh-zhipu-provider 的 cipher.ts/auth.ts）只读次之，参考客户端的 `~/.zcode_coding_plan_token.json` 只读再次。三套都只读、只有 `~/.nanorouter` 一套可写：官方文件归 ZCode 客户端所有，写入会竞态。网关 401/403 能证伪 key，网关不可达不能——把瞬时故障误报成"key 无效"比多一次探测更糟，因此不可达视为通过。凭据文件带 5 秒进程内缓存：参考客户端至多每次登录重写一次，而请求路径每次环境变量未命中都会来读。

`ZCODE_CREDENTIAL_SECRET` 要填**种子串本身**，不是它的 SHA-256——那道哈希由 `deriveZcodeCipherKey` 内部再算一次。填成哈希会让每条记录解密失败，而 `decryptZcodeCredentialValue` 对失败只返回 false，坏条目按设计被跳过：整份文件于是静默降级为空凭据，上游只回一句 `SERVICE_AUTHENTICATION_INVALID`，本地毫无线索。这是实测踩过的坑，故在此写明。

**单实例边界不变。** Anthropic 候选的熔断、并发、重试预算、全局背压与其他候选共享同一套机制与状态；`/v1/messages` 端点的错误方言随端点切换（Anthropic 信封），鉴权中间件按路径后缀选方言。探测（Prober）对 Anthropic 候选发 Messages 形状的极简 Ping，有效性判定与 OpenAI 侧同语义（拦截 200 伪成功与空内容）。

## go-json 的位置

`github.com/goccy/go-json` 是 `encoding/json` 兼容实现，项目将它用在请求解码、真实模型名重编码、模型列表和 usage 解析这些热路径。HTTP/SSE 和数据库驱动仍各自使用专用库，故替换 JSON 库不会改变 API 行为。

## 数据库、日志与指标

数据库只承担历史调用账本，不参与实时选路。配置可以使用 SQLite 或 PostgreSQL；两者共享同一
迁移和记录结构，PostgreSQL 仅在 SQL 占位符上使用方言转换。生产环境通过 `dsn_env` 读取
PostgreSQL DSN，避免连接凭据进入 ConfigMap。

内置统计页（`GET /stats` + `GET /v1/stats/summary`）对台账做只读聚合：聚合 SQL 按
双方言可移植书写——时间分桶用 `substr(created_at, 1, N)`（RFC3339 文本前缀即时间桶），
百分位用 `ORDER BY ... LIMIT 1 OFFSET k` 而非方言 percentile 函数；空桶由应用层补零。
页顶另有实时区块（进行中/存活会话/活跃调用方）：进程内存态，与台账互补——
"进行中"是 acquire→release 之间的并发数；会话与调用方按 5 分钟滑动窗口去重
计数，只存计数不存明文（会话键与选路亲和同源：X-Session-Affinity →
X-Session-Id），重启清零，条目上限 5000 防异常流量撑内存。
`tools_count`/`tool_names`/`client_ip`/`client_agent` 是请求侧元数据：工具名取下游请求
的 `tools` 数组（兼容 OpenAI/Anthropic 形状），流式透传不解析内容，故不含响应侧
tool_calls 计数；工具榜聚合因 JSON 展开函数跨方言不统一（json_each vs
jsonb_array_elements_text），拉回 Go 侧归并。调用方 IP 直连记 TCP 对端地址，开启
`trust_forwarded_for` 后取 `X-Forwarded-For` 首值（仅限可信反代后部署，否则来源可
伪造）。统计与重置卡路径的认证在 Bearer/`X-Api-Key` 之外为浏览器放宽 Basic（仅
`/stats`、`/v1/stats/summary`、`/v1/zcode/usage`、`/v1/zcode/reset/status` 与
`/v1/zcode/reset/use`），生成端点不放宽。
`/healthz` 恒免鉴权：只回 200、无业务数据，k8s 存活/就绪探针无头可带，
`auth_env` 下若拦截探针会把自己杀死；`/metrics` 不豁免（含调用量）。
`stats_public: true` 时统计面三个路径整体豁免鉴权——为反代层或内网已自管访问控制的
部署准备；豁免面不含任何生成端点，`/v1/zcode/usage` 的账户级数据公开与匿名外呼
是开启者显式接受的取舍。重置卡面由独立开关 `zcode_reset.public` 控制，不随
`stats_public` 一起放开：统计面公开不等于允许匿名花卡。

ZCode 用量看板（`GET /v1/zcode/usage`，渲染在统计页下半区）是唯一主动外呼厂商
biz 主机的辅助路径：网关按 glm-usage 移植的八个只读端点（四个用量监控：配额、
模型用量、活动序列、性能；四个账户面：套餐订阅、自动续费状态、额度重置记录、
账户资金）从 bigmodel.cn 拉数据，凭据复用生成路径的解析链（数据库 → 凭据文件），
不引入新配置。该面刻意只读；getCustomerInfo 与 getTokenMagnitude 实测被上游
拒绝（403/系统异常），已移除不再外呼。设计取舍：按端点独立缓存（quota 60s、用量 5 分钟、
账户面 1 小时）并用单飞去重并发拉取，页面流量不放大上游流量；失败按块隔离并进
`failures`，非凭据失败在 stale 窗（10 分钟）内降级返回旧值并后台刷新，凭据被拒
（HTTP 200 信封 code 401/1001）不降级、如实报 401——旧配额数字被当成现状比
短暂报错更有害。数据原样透传（`sections` 内为上游 `data`，biz 端点的 data
可能是列表或布尔，缺省时回退整个信封体），不做字段猜测；生成请求路径零接触。
该端点与统计页同在 Basic 回退白名单。

额度重置卡是独立的第二条辅助路径（`GET /v1/zcode/reset/status`、
`POST /v1/zcode/reset/use`）。它不走 bigmodel.cn 的 API key，而是 ZCode 平台的
凭据对（`Authorization: Bearer <平台 JWT>` + `X-Bigmodel-Authorization: <MaaS
OAuth token>`）；两个 token 在 `login zcode` 时已随 key 一起落库/落盘，解析链为
「数据库 → login 文件 → ZCode v2 加密文件 → 参考客户端文件」，业务 token 严格
按渠道 family 取，禁止跨渠道回退。status 30 秒缓存且失败同样缓存，避免页面轮询
放大上游限流；use 每次真实上行，成功后立即失效 status 缓存。核销是破坏性操作：
`zcode_reset.allow_use` 默认 false，只接受显式 POST（要求 JSON 请求体以阻断
跨站表单），请求主链路（生成、故障切换、探活、登录）绝不自动调用。只有
`zcode_reset.public: true` 时这两个端点才跳过网关鉴权，访问控制完全交给边缘层；
部署时必须让 reset 路径强制经过反代鉴权（Traefik/Authelia），否则任何能直连
网关端口的人都可匿名核销。它刻意不复用 `stats_public`，避免“统计页公开”顺带
获得核销能力。上表“账户面只读”仅指 bigmodel.cn 的用量看板，不包含这条独立的
核销路径。

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

运行日志使用 Zap 的结构化 JSON；`--debug` 则用相同字段输出稳定的 `key=value` 文本。每个请求以
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
