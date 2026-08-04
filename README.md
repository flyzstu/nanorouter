# nanorouter

为 Agent、Open WebUI 等下游提供一个 OpenAI 兼容入口，并把逻辑模型名路由到多个独立上游。若上游在返回首个成功响应前发生网络错误，返回候选级的 `401/403/404`，或返回 `408`、`425`、`429`、`500/502/503/504`，聚合路由会切换至下一个可用候选；`400/422` 等请求错误不会触发切换。

已实现：

- `POST /v1/chat/completions`
- `POST /v1/responses`（包括 SSE 原样透传）
- `GET /v1/models`
- 每个上游的加权轮转、429/错误冷却与请求内指数退避重试
- SQLite 调用账本、Prometheus `/metrics`
- Chat Completions 与 Responses 的非流式/流式 `usage` 采集
- 将受支持的 Responses 请求适配至只提供 Chat Completions 的 SenseNova 上游
- llama.cpp `llama-server` 本地上游（可直接填写服务根地址）

## 依赖

- `net/http`：HTTP 客户端、代理与 SSE
- `github.com/go-chi/chi/v5`：本地 API 路由
- `github.com/ncruces/go-sqlite3/driver`：纯 Go/WASM SQLite 驱动（按项目要求）
- `github.com/prometheus/client_golang`：指标导出
- `go.uber.org/zap`：结构化 JSON 日志
- `gopkg.in/yaml.v3`：路由配置

## 启动

```sh
cp config.example.yaml config.yaml
export NANOROUTER_API_KEY='replace-with-a-local-secret'
export OPENROUTER_API_KEY='...'
export GROQ_API_KEY='...'
go run ./cmd/nanorouter -config config.yaml
```

日志语言和最低输出等级可在配置文件中设置：

```yaml
logging:
  language: zh-CN # en 或 zh-CN，默认 en
  level: INFO     # DEBUG、INFO、WARN 或 ERROR，默认 INFO
  format: text    # text 或 json，默认 text
  color: true     # 文本日志是否显示彩色等级，默认 true
```

中文模式只翻译日志消息；用于检索和告警的结构化字段名及 `event` 值保持英文稳定。默认输出
带颜色的 `key=value` 文本；日志采集系统需要 JSON 时可设置 `format: json`，此时 `color` 不生效。
`-debug` 会强制使用彩色文本日志，并将本次启动的日志等级覆盖为 `DEBUG`。

## 数据库

默认使用 SQLite；旧的字符串形式 `database: ./nanorouter.db` 仍兼容。新的对象形式可明确指定
驱动和连接信息：

```yaml
database:
  driver: sqlite
  dsn: ./nanorouter.db
```

也支持 PostgreSQL（使用 `github.com/lib/pq`）。连接串应通过环境变量提供，避免数据库凭据写入
YAML；`lib/pq` 支持 `postgres://` 与 `postgresql://` DSN：

```yaml
database:
  driver: postgres
  dsn_env: NANOROUTER_POSTGRES_DSN
```

```sh
export NANOROUTER_POSTGRES_DSN='postgresql://nanorouter:replace-me@postgres:5432/nanorouter?sslmode=require&connect_timeout=5'
go run ./cmd/nanorouter -config config.yaml
```

两种数据库共享 `llm_requests` 账本表和迁移逻辑；PostgreSQL 会使用其原生参数占位符，SQLite
继续使用原有的 `?` 占位符。

## 容器运行

镜像运行阶段使用 Distroless 静态 Debian 基础镜像，以非 root 用户运行，默认读取
`/etc/nanorouter/config.yaml`；请挂载自己的配置文件，并以环境变量传入其中
`api_key_env` 引用的密钥。SQLite 数据库默认写入
`/var/lib/nanorouter`，生产环境应为该目录挂载持久卷。

```sh
docker build -t nanorouter:local .
docker run --rm -p 8080:8080 \
  --env NANOROUTER_API_KEY \
  --env OPENROUTER_API_KEY \
  --mount type=bind,src="$PWD/config.yaml",dst=/etc/nanorouter/config.yaml,readonly \
  --mount type=volume,src=nanorouter-data,dst=/var/lib/nanorouter \
  nanorouter:local
```

仓库的 Gitea Actions 工作流会在 `main` 分支 push、Pull Request 和手动触发时构建镜像。
在 `main` 分支 push 或手动触发时，构建产物会推送到同名 Gitea 容器包：
`coding.fly97.fun/fly97/nanorouter:<commit-sha>` 与 `coding.fly97.fun/fly97/nanorouter:latest`。

推送前需在仓库 Actions Secrets 设置 `GITEA_PACKAGE_USERNAME`（PAT 所属用户名）和
`GITEA_PACKAGE_TOKEN`（具有 Package Read/Write 权限的个人访问令牌）。Gitea 的内置作业令牌
在部分版本无法发布 OCI 包，因此工作流不依赖它；Pull Request 仅构建验证，不访问上述密钥。

将 `config.yaml` 中每一个 `model` 改为供应商当前实际支持的模型 ID，并为非 OpenAI 兼容供应商填入其兼容 API 基地址。只有实际支持 Responses API 的候选才应设为 `responses: true`；否则该候选只会参与 Chat Completions 路由。

## llama-server 本地上游

启动模型时建议用 `--alias` 固定其模型 ID，并避免与网关监听端口冲突：

```sh
llama-server -m ./model.gguf --alias local-llama --host 127.0.0.1 --port 8081
```

配置 `llama_server: true` 后，`base_url` 填服务根地址即可，网关会请求
`http://127.0.0.1:8081/v1/chat/completions`；无需人为补写 `/v1`。未使用
`--api-key` 时省略 `api_key_env`，不会发送 Authorization 请求头：

```yaml
routes:
  local-llama:
    strategy: weighted_round_robin
    candidates:
      - id: llama-server-local
        base_url: http://127.0.0.1:8081
        model: local-llama
        llama_server: true
        responses_via_chat_completions: true
```

上述配置对各版本稳定的 Chat Completions 接口生效；`/v1/responses` 请求经受限的
Responses→Chat 适配。若部署的 llama-server 已验证原生 Responses 兼容性，可改为
`responses: true`，以保留原始 Responses 字段。

## 候选并发与排队

候选可用 `max_concurrency` 保护本地模型或限额较小的上游。槽位从请求发往上游开始占用，
直到完整非流式响应或 SSE 流结束后释放；满载时最多等待 `queue_timeout`，超时会尝试同一路由
的其他候选，仍无可用槽位则返回 `429`（错误码 `upstream_concurrency_limit`）。未配置
`max_concurrency` 时不限制并发；`queue_timeout: 0s` 表示不排队、立即拒绝。

```yaml
- id: llama-server-local
  max_concurrency: 1
  queue_timeout: 30s
```

`attempt_timeout` 限制一次候选调用的完整生命周期，包括等待响应头、非流式正文和 SSE 流。超时发生在尚未向下游发送内容前时可继续故障切换；SSE 已经开始后只能终止该流，不能安全拼接另一模型。`0s` 表示只使用全局 `request_timeout`。

可选的自适应并发使用 AIMD：出现排队且 TTFB 达标时逐步加一，收到 429 或 TTFB 超过目标两倍时减半，并始终限制在 `min_concurrency` 与 `max_concurrency` 之间：

```yaml
max_concurrency: 32
queue_timeout: 2s
attempt_timeout: 60s
adaptive_concurrency: true
min_concurrency: 2
target_ttfb: 2s
```

## 全局背压

候选并发限制保护单个上游，全局背压保护网关进程。它在读取请求体前占用准入槽，因此上游整体变慢时，最多只有 `max_inflight + max_queue` 个生成请求占用网关资源。队列已满或等待超过 `queue_timeout` 时返回 `429 gateway_overloaded` 并带 `Retry-After: 1`；`max_inflight: 0`（默认）表示关闭。

```yaml
backpressure:
  max_inflight: 512
  max_queue: 128
  queue_timeout: 1s
```

## SenseNova 的 Responses 适配

SenseNova 候选使用 `responses_via_chat_completions: true` 时，下游可以照常请求 `/v1/responses`，网关会向上游调用 Chat Completions，并把结果包装回 Responses 格式。

SenseNova 目前推荐使用 `token.sensenova.cn/v1` 端点，该端点为标准 OpenAI Chat Completions 接口，无需额外适配配置。当前支持两个模型：

| 模型 ID | 说明 |
|---|---|
| `sensenova-6.7-flash-lite` | 轻量多模态，支持图像输入、工具调用、并行工具调用 |
| `deepseek-v4-flash` | 高性能对话模型，默认启用思考模式，支持工具调用与 JSON Output |

```yaml
- id: sensenova-flash-lite
  base_url: https://token.sensenova.cn/v1
  api_key_env: SENSENOVA_API_KEY
  model: sensenova-6.7-flash-lite
  responses_via_chat_completions: true
  max_tokens_limit: 65536

- id: sensenova-deepseek-v4-flash
  base_url: https://token.sensenova.cn/v1
  api_key_env: SENSENOVA_API_KEY
  model: deepseek-v4-flash
  responses_via_chat_completions: true
  max_tokens_limit: 65536
```

两个模型均支持 `reasoning_effort`（`"low"` / `"medium"` / `"high"` / `"none"`）。DeepSeek V4 Flash 思考模式下 Chat Completions 响应中会额外包含 `reasoning_content` 字段（推理过程），普通 Chat 请求会直接透传；Responses 适配无法把它伪装成原生 reasoning Item。

`max_tokens_limit` 会在选中候选后，把 Chat Completions 的 `max_tokens` /
`max_completion_tokens`、Responses 的 `max_output_tokens`，以及适配器生成的自定义 token
字段约束到 `[1, limit]`。因此 SenseNova 不会再因下游请求超过 `65536`（或传入 `0`）而返回
`field MaxTokens invalid`，同时原始值仍可供故障切换到限制不同的其他候选使用。

若路由中同时存在不支持 `reasoning_effort` 的其他候选（如 Groq、Agnes、Mistral、Cerebras），应在那些候选上配置 `drop_request_fields: [reasoning_effort]`。清洗在选中候选后不影响故障转移时其他候选接收该字段的能力。

适配器支持：`input`（文本/图片消息及手动回传的 `output_text`）、`instructions`、常用采样参数、`max_output_tokens`、`reasoning.effort`、`text.verbosity`、`text.format` 结构化输出、自定义函数 `tools` / `tool_choice` / `parallel_tool_calls`、`function_call` / `function_call_output`、流式事件与 usage。Chat 返回的函数调用、拒绝和 token 截断状态也会转换为对应 Responses Item 或状态。

这不是 100% 的 API 等价层。`web_search`、`file_search`、Code Interpreter、MCP 等 OpenAI 托管工具，`previous_response_id` / Conversations 状态链、`background`、prompt 模板和 reasoning summary 没有纯 Chat Completions 等价物，会返回 `400 unsupported_response_feature`，不会静默丢弃。各上游对函数调用、严格 JSON Schema、推理参数的支持仍可能不同，可用候选级字段清洗处理无副作用的参数差异；需要完整 Responses 语义时应使用 `responses: true` 的原生候选。

Mistral Medium 3.5 使用 `https://api.mistral.ai/v1`、模型 ID `mistral-medium-3-5`，原生支持 Chat Completions；示例为其开启 Responses→Chat 适配。所有候选均可设置 `enabled: false` 临时下线，无需删除密钥或配置。未设置 `enabled` 时默认启用；全部候选均禁用的路由不会出现在 `/v1/models`。

Cerebras 的 `gpt-oss-120b` 使用 `https://api.cerebras.ai/v1`，原生支持 Chat Completions、流式、函数调用与结构化输出；示例为其启用 Responses→Chat 适配。普通 Chat 请求会完整透传工具调用；经适配的 Responses 请求仍只支持本文前述可无损映射的字段。

## Nous Research Hermes 上游

Nous Research 提供 OpenAI 兼容的 Chat Completions 接口，基础地址为
`https://inference-api.nousresearch.com/v1`。示例接入 `Hermes-4.3-36B`，并使用
`NOUS_API_KEY` 环境变量传入 API Key；下游请求 `/v1/responses` 时由网关适配到 Chat Completions：

```yaml
- id: nous-hermes-4-3-36b
  base_url: https://inference-api.nousresearch.com/v1
  api_key_env: NOUS_API_KEY
  model: Hermes-4.3-36B
  expose_as: hermes-4.3-36b
  weight: 1
  enabled: true
  responses_via_chat_completions: true
```

Nous 的 Free 级别有独立的 RPM/TPM 限制；请先确认账户额度和 `NOUS_API_KEY` 有效，再将生产部署模板中的候选设为 `enabled: true`。

## 智谱 GLM 上游

`zhipu-agent` 示例使用智谱通用端点 `https://open.bigmodel.cn/api/paas/v4` 和 `ZHIPUAI_API_KEY`。请将示例模型替换为账户实际可用的模型 ID。它原生使用 Chat Completions；`/v1/responses` 请求会经过受限适配，自定义函数调用可转换，但内建工具或 `previous_response_id` 会明确返回 `400`。使用智谱 Coding 套餐时，应把 `base_url` 改为 `https://open.bigmodel.cn/api/coding/paas/v4`。

OpenCode 会向部分 OpenAI 兼容上游发送 `promptCacheKey`。智谱候选默认删除该字段；智谱的上下文缓存由服务端按重复内容自动复用，不需要客户端传入该参数。

## 上游字段兼容性清洗

每个候选可用 `drop_request_fields` 删除不支持的顶层字段，用 `drop_message_fields` 删除消息对象内字段。清洗在选择上游后进行，故不会影响故障切换到其他上游时的原始请求。Groq 示例默认删除其文档列出的 `logprobs`、`logit_bias`、`top_logprobs` 与 `messages[].name`，并去除标为暂不支持的 `metadata`、`store`。

## 公开单个候选

候选配置 `expose_as` 后，默认仍会参与其父路由（如 `free-fast`）的加权和故障切换，但也会以该值作为独立模型 ID 出现在 `GET /v1/models`。例如 `expose_as: agnes-flash` 后，下游可用 `model: agnes-flash` 直接选择 Agnes，不会切换到 `free-fast` 的其他候选。设置 `aggregate_enabled: false` 可让候选只通过 `expose_as` 开放，而不进入父路由；`enabled: false` 仍会将两种入口一起关闭。公开 ID 必须在全局唯一，且不能与顶层路由同名。如果父路由没有任何已启用且参与聚合的候选，它也不会出现在模型列表中。

不会自动改写语义性参数：例如 Groq 的 `n` 只支持 `1`，如果下游传 `n > 1`，网关会让 Groq 返回明确错误，而不是静默篡改成单个候选。

下游配置为：

```text
Base URL: http://127.0.0.1:8080/v1
API key:  $NANOROUTER_API_KEY
Model:    free-fast
```

调用返回额外包含 `X-Nanorouter-Route`、`X-Nanorouter-Upstream` 和 `X-Nanorouter-Attempts`，可用于排障。

## 会话亲和

下游传入非空的 `X-Session-Affinity` 时，网关会结合路由名对该值做稳定哈希；只要可用候选集合
不变，同一个会话会持续选择同一上游。哈希仍按候选 `weight` 分布，因此不会绕过配置权重。
若粘性候选进入冷却、达到并发上限或返回可故障切换的错误，当前请求仍会尝试其他候选。

该 Header 完全可选。下游未传、传入空值或只包含空白字符时，网关继续使用原有加权轮转，
不会返回错误。直接请求候选的 `expose_as` 模型仍天然固定到该候选。

## 临时错误重试与日志

当某个上游返回 `429/502/503/504` 时，网关会先将它暂时冷却并尝试同一路由的其他候选；若一轮候选都不可用，
网关默认在**同一个下游请求**中最多重试 5 次。`401/403/404/408/425/500` 仍可触发聚合路由切换，但不会原地重试。指数退避上限依次为 1、2、4、8、16 秒，实际等待加入
50%–100% 等抖动，避免大量请求同时苏醒。上游给出 `Retry-After` 时，它是该候选的最小冷却时间；
多个候选同时限流时只等待最早可恢复的候选。整个过程受 `request_timeout` 和客户端取消约束；
超过重试次数后才返回 `429 upstream_rate_limited`，无需下游重复提交同一请求。

可通过全局配置调整流量放大与尾延迟；`max_rate_limit_retries: 0` 会禁用请求内重试，但仍保留候选间故障切换：

```yaml
retry:
  max_rate_limit_retries: 5
  base_delay: 1s
  max_delay: 16s
```

每个候选可以覆盖重试次数和退避窗口；省略的字段继承全局值。通过 `expose_as` 直接请求时只重试该候选，不会进入父路由的其他候选。`enabled: false` 可对单个候选关闭内部重试：

```yaml
- id: sensenova
  expose_as: sensenova
  retry:
    enabled: true
    max_retries: 2
    base_delay: 500ms
    max_delay: 4s

- id: openrouter
  expose_as: openrouter
  retry:
    max_retries: 5
    base_delay: 2s
    max_delay: 30s
```

上游 `Retry-After` 始终是最小等待时间。重试受 `request_timeout` 和父路由 Retry Budget 约束；SSE 一旦向下游写出任何内容便不再重试。

聚合路由还可启用 Retry Budget，限制故障切换造成的请求放大。每个初始请求补充 `ratio` 个令牌，第二次及以后真正发往上游的调用各消耗一个；令牌最多积累到 `burst`。预算耗尽时返回 `429 retry_budget_exhausted`。本地候选排队失败不扣令牌；默认关闭以保持兼容。

```yaml
routes:
  free-fast:
    retry_budget:
      enabled: true
      ratio: 0.1
      burst: 10
```

每个 Chat Completions / Responses 请求会按 `request_started`、`upstream_attempt_started`、
`upstream_response_received`、`upstream_attempt_finished`、`retry_scheduled`、`request_completed`
或 `request_failed` 写结构化日志，并用 `request_id` 关联。默认输出带颜色的 `key=value` 文本；
配置 `logging.format: json` 可切换为 JSON。
`request_started` 会以扁平字段记录方法、路径、直连地址、转发链首个客户端 IP、User-Agent、请求体大小，
以及是否提供会话亲和键；不会输出完整 Header map，也不会记录会话键的值。日志不会包含 Authorization、
Cookie、请求正文、prompt、模型回复或密钥。
文本日志使用 `2026-07-28 14:18:38.629 +08:00` 格式；可通过环境变量 `TZ=Asia/Shanghai`
输出北京时间。程序已内嵌 IANA 时区数据，因此在 distroless 镜像中也无需额外挂载
`/usr/share/zoneinfo`。JSON 日志继续使用 ISO 8601，数据库中的请求时间仍统一保存为 UTC。
上游拒绝请求时，日志会提取错误消息及 OpenRouter `metadata` 中的错误类别、供应商、供应商错误码和参数；
可能包含用户输入的 `flagged_input`、prompt、content 以及未解析的原始响应不会写入日志，完整错误体仍透明返回下游。

## 查看用量

```sh
sqlite3 nanorouter.db '
SELECT upstream_id, upstream_model,
       COUNT(*) AS requests,
       SUM(prompt_tokens) AS prompt_tokens,
       SUM(completion_tokens) AS completion_tokens,
       SUM(total_tokens) AS total_tokens
FROM llm_requests
WHERE status_code BETWEEN 200 AND 299
GROUP BY upstream_id, upstream_model
ORDER BY total_tokens DESC;'
```

Prometheus 抓取 `GET /metrics`。除请求数、故障切换和 token 外，还提供：

- `nanorouter_upstream_ttfb_seconds`：上游首字节/响应头延迟；
- `nanorouter_upstream_queue_seconds`：候选并发槽排队时间；
- `nanorouter_retry_delay_seconds`：实际安排的内部退避时间；
- `nanorouter_upstream_inflight`：各路由和上游当前在途量。
- `nanorouter_upstream_concurrency_limit`：当前固定或自适应并发窗口；
- `nanorouter_backpressure_inflight` / `nanorouter_backpressure_queued`：全局准入在途量和排队量；
- `nanorouter_backpressure_rejected_total`：按队列满、等待超时等原因统计的拒绝数；
- `nanorouter_retry_budget_tokens` / `nanorouter_retry_budget_denied_total`：路由重试令牌和拒绝数；
- `nanorouter_ledger_dropped_total` / `nanorouter_ledger_write_errors_total`：异步队列丢弃和数据库写入失败。

仅记录供应商实际返回的 `usage`；缺失时标为 `unavailable`，不会伪造估算 token。

日志默认写到标准错误输出，格式由 `logging.format` 控制；只包含路由、上游、模型、状态、耗时和用量，绝不输出 prompt、模型回复或 API Key。

实现思路与关键权衡见[设计文档](docs/design.md)。

## 异步账本与性能分析

SQLite 高 QPS 部署可选择批量写入。关闭服务时 `nanorouter` 会先停止 HTTP 接流量，再最多等待 5 秒排空账本。异步队列满时不会阻塞生成请求，而是丢弃账本记录并增加 Prometheus 指标：

```yaml
database:
  driver: sqlite
  dsn: ./nanorouter.db
  async_write: true
  batch_size: 100
  flush_interval: 100ms
  queue_size: 4096
```

运行四种完整协议基准：

```sh
./scripts/bench.sh
BENCHTIME=15s PROFILE_DIR=/tmp/nanorouter-profile ./scripts/profile.sh
```

在当前基准环境中，按 pprof 结果延迟构造关闭级别的日志字段、复用 SSE reader，并避免在未配置候选超时时创建额外 context 后，四类基准的分配空间下降约 26%–43%，分配次数下降 5%–11%。具体数值依 Go 版本、CPU 和上游响应大小变化，应以部署机器的 profile 为准。

需要在线 pprof 时显式设置 `profiling.enabled: true`，然后访问 `/debug/pprof/`。该端点包含运行时信息，不应在未配置 `auth_env` 的公网实例开启。

## 流式限制

一旦任何 SSE 字节已经发送给下游，网关不能再安全地改用另一模型，否则会产生重复内容或破坏工具调用。因此流式故障切换只覆盖上游尚未开始返回 SSE 前可识别的网络/HTTP 错误；已开始的流会保持透明转发。
