# Nanorouter 设计文档

本文从“为什么这样拆分”出发，帮助理解这个兼容层，而不只是说明如何运行它。

## 目标与全景

下游只知道逻辑模型名 `free-fast`，网关才知道真实模型、供应商密钥和故障状态。这样替换免费模型、调整权重或下线上游不需要改 Agent。

```text
Agent / Open WebUI
       │ model=free-fast
       ▼
 Nanorouter :8080/v1
       │ 选择候选、替换 model、注入上游密钥
       ├── OpenRouter
       ├── SenseNova
       └── Groq
       │
       └── SQLite 账本 + Prometheus 指标
```

## 模块边界

| 模块 | 职责 | 刻意不负责 |
| --- | --- | --- |
| `config.go` | 读 YAML、验证必要字段 | 确认供应商模型 ID 是否仍可用 |
| `router.go` | 鉴权、选路、转发、切换 | 解释每一种模型参数语义 |
| `usage.go` | 从 JSON/SSE 提取用量 | 用 tokenizer 猜缺失 token |
| `store.go` | 建表、写调用账本 | 保存 prompt、回复或密钥 |

代理只将 JSON 解为 `map[string]any`，修改 `model` 后重新编码，保留其余字段。因此 `tools`、图片、结构化输出等新增字段不需要随网关升级逐一建模。

## 一次请求的生命周期

1. 客户端向 `/v1/chat/completions` 或 `/v1/responses` 发请求。
2. 网关限制请求体最大 16 MiB，读取 `model` 作为路由名。
3. 从未冷却、且支持目标端点的候选中按权重轮转选择一个。
4. 将逻辑名替换为真实模型 ID，以该上游自己的 Bearer Key 转发。
5. 成功时原样回传，解析上游 `usage` 并写 SQLite。
6. 网络错误或可重试 HTTP 错误时，将候选冷却后尝试下一个。

候选可以通过 `max_concurrency` 设置独立的在途请求上限。满载请求会最多排队
`queue_timeout`；超时后优先尝试路由中的其他候选，所有候选均满载时返回 429。局部并发
限制不是上游故障，因此不会触发候选冷却。SSE 从开始转发到结束始终占用一个槽位。

可重试的是网络错误和 `408/425/429/500/502/503/504`。`400` 往往代表参数不兼容，`401/403` 往往代表密钥或权限问题；切到另一个模型会掩盖配置错误，因此直接回传。

## 权重和熔断

`weight: 3` 的候选在轮转序列中占三个槽位，大约承受三倍请求。`Router.mu` 保护轮转游标和冷却状态，防止并发请求同时涌向刚发生 429 的上游。

收到 429/5xx 后，上游进入冷却；若响应给出 `Retry-After` 则优先服从，否则为 30 秒。这是适合单实例的轻量熔断器。多实例时各自状态不共享，下一步可迁移到 Redis。

## 两种 API 与能力声明

Chat Completions 与 Responses 共享选路器，但不会互相转换 schema。配置中的 `responses: true` 是上游能力声明；Responses 请求会跳过没有该标记的候选。这样可以完整保留 Responses 特有的 `input`、`previous_response_id`、内建工具和输出事件，但需要你确认每家供应商的兼容性。

对于 SenseNova 这类只提供 Chat Completions 的上游，可以改用 `responses_via_chat_completions: true`。这是一个受限适配器：`input`、`instructions`、图片/文本 content part、采样参数和最大输出 token 能够映射；结果会从 SenseNova 的 `data.choices` / `data.usage` 转成 Responses 对象或 SSE 事件。工具调用、后台任务和 `previous_response_id` 没有等价表示，必须返回 400，而不能假装成功。

`chat_max_tokens_field` 解决供应商参数命名差异：默认是 `max_tokens`（适用于 Agnes 一类 OpenAI Chat 兼容服务）；SenseNova 原生接口使用 `max_new_tokens`，需在候选中显式配置。`chat_completions_path` 则解决路径差异，默认 `chat/completions`，SenseNova 原生接口是 `chat-completions`。Groq 提供原生 `/responses`，应使用 `responses: true` 而不是该适配器。

候选的 `enabled` 是运维开关：省略时默认开启，设为 `false` 时选路器跳过它；一条路由的所有候选都禁用后，该路由也不再出现在 `/v1/models`。这让你可以安全地下线失效模型，而不需要删掉配置。

Cerebras `gpt-oss-120b` 与 Mistral 一样使用原生 Chat Completions 加 Responses 适配的组合。它的普通 Chat 路径可保留工具调用与结构化输出；若下游一定需要完整的 Responses 工具/状态语义，应优先路由到 Groq 或 OpenRouter 的原生 Responses 候选。

## 上游参数清洗

兼容不等于每个字段都支持。候选可声明 `drop_request_fields` 与 `drop_message_fields`，网关仅在请求已选定该候选后删除这些字段。这样 Groq 的不支持字段不会导致 400，同时切换到其他供应商时仍保留原请求。清洗适合无副作用的观测、日志概率或元数据字段；会改变结果数量、工具执行或状态机行为的字段不应自动改写。

## 流式响应的边界

SSE 首字节发给下游后不能再安全换模型：会重复文字，或破坏 function-call ID 和参数。因此只有连接建立前、或可见 HTTP 状态码阶段的故障可切换；已经成功开始的流只透明转发。

实现按行读取 SSE，不将长回答完整缓存在内存；它观察 `data:` 内 JSON，最后采集到的 `usage` 写入账本，但绝不改写下游收到的事件。

## go-json 的位置

`github.com/goccy/go-json` 是 `encoding/json` 兼容实现，项目将它用在请求解码、真实模型名重编码、模型列表和 usage 解析这些热路径。HTTP/SSE 和 SQLite 仍各自使用专用库，故替换 JSON 库不会改变 API 行为。

## 用量与隐私

`llm_requests` 一行代表一次**上游尝试**。一次请求先 429 后成功会有两行，便于计算故障率和切换次数。

- `route`：下游逻辑模型名；
- `upstream_id` / `upstream_model`：实际落点；
- `attempt`：第几次候选尝试；
- `usage_source`：`upstream` 或 `unavailable`；
- `error_kind`：`network`、`upstream` 或 `client`。

账本不保存 prompt、回复正文或 API Key，避免用户内容和密钥落入普通运维数据库。Prometheus 用于实时请求率、失败率和 token 速率；SQLite 用于按模型汇总历史使用量。

运行日志使用 Zap 的结构化 JSON。成功请求记录 `info`，可重试故障与被上游拒绝的请求记录 `warn`；字段与账本一样只含元数据，便于 Loki、Elasticsearch 或 `jq` 查询。

## 推荐的下一步

1. 增加管理 API，用于查看候选、临时禁用和解除冷却。
2. 增加按路由的并发/RPM/token 限制，保护免费额度。
3. 为候选声明视觉、工具调用、JSON Schema 和上下文长度等能力。
4. 高并发时为 SQLite 写入加队列与批量提交，降低锁竞争。
