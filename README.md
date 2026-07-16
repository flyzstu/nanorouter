# nanorouter

为 Agent、Open WebUI 等下游提供一个 OpenAI 兼容入口，并把逻辑模型名路由到多个独立上游。若上游在返回首个成功响应前发生网络错误，或返回 `408`、`425`、`429`、`5xx`，请求会切换至下一个可用候选。

已实现：

- `POST /v1/chat/completions`
- `POST /v1/responses`（包括 SSE 原样透传）
- `GET /v1/models`
- 每个上游的加权轮转、429/错误冷却与 `Retry-After` 处理
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

仓库的 Gitea Actions 工作流会在 push、Pull Request 和手动触发时执行同一 Docker 构建，
仅验证镜像可构建，不会向未指定的镜像仓库推送。

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

## SenseNova 的 Responses 适配

SenseNova 候选使用 `responses_via_chat_completions: true` 时，下游可以照常请求 `/v1/responses`，但网关会向上游调用 Chat Completions，并把结果包装回 Responses 格式。

```yaml
- id: sensenova-chat
  base_url: https://api.sensenova.cn/v1/llm
  api_key_env: SENSENOVA_API_KEY
  model: "<SenseNova 模型 ID>"
  responses_via_chat_completions: true
  chat_max_tokens_field: max_new_tokens
  chat_completions_path: chat-completions
```

支持：`input`（文本/图片消息）、`instructions`、`temperature`、`top_p`、`max_output_tokens`、流式与 usage。`tools`、`tool_choice`、`previous_response_id`、`background` 等无法在 Chat Completions 中保持语义的字段会返回 `400 unsupported_response_feature`，不会静默降级。

Groq 同时原生支持 `/v1/chat/completions` 和 `/v1/responses`，应配置 `responses: true`，不使用适配器。Agnes 2.0 Flash 公开的是 Chat Completions 接口，示例中配置为 `responses_via_chat_completions: true`；它使用默认的 `max_tokens` 映射。SenseNova 原生接口则应额外指定 `chat_max_tokens_field: max_new_tokens`。

Mistral Medium 3.5 使用 `https://api.mistral.ai/v1`、模型 ID `mistral-medium-3-5`，原生支持 Chat Completions；示例为其开启 Responses→Chat 适配。所有候选均可设置 `enabled: false` 临时下线，无需删除密钥或配置。未设置 `enabled` 时默认启用；全部候选均禁用的路由不会出现在 `/v1/models`。

Cerebras 的 `gpt-oss-120b` 使用 `https://api.cerebras.ai/v1`，原生支持 Chat Completions、流式、函数调用与结构化输出；示例为其启用 Responses→Chat 适配。普通 Chat 请求会完整透传工具调用；经适配的 Responses 请求仍只支持本文前述可无损映射的字段。

## 智谱 GLM 上游

`zhipu-agent` 示例使用智谱通用端点 `https://open.bigmodel.cn/api/paas/v4` 和 `ZHIPUAI_API_KEY`。请将示例模型替换为账户实际可用的模型 ID。它原生使用 Chat Completions；`/v1/responses` 请求会经过受限适配，因此含工具调用或 `previous_response_id` 的 Responses 请求会明确返回 `400`。使用智谱 Coding 套餐时，应把 `base_url` 改为 `https://open.bigmodel.cn/api/coding/paas/v4`。

OpenCode 会向部分 OpenAI 兼容上游发送 `promptCacheKey`。智谱候选默认删除该字段；智谱的上下文缓存由服务端按重复内容自动复用，不需要客户端传入该参数。

## 上游字段兼容性清洗

每个候选可用 `drop_request_fields` 删除不支持的顶层字段，用 `drop_message_fields` 删除消息对象内字段。清洗在选择上游后进行，故不会影响故障切换到其他上游时的原始请求。Groq 示例默认删除其文档列出的 `logprobs`、`logit_bias`、`top_logprobs` 与 `messages[].name`，并去除标为暂不支持的 `metadata`、`store`。

## 公开单个候选

候选配置 `expose_as` 后，仍会参与其父路由（如 `free-fast`）的加权和故障切换，但也会以该值作为独立模型 ID 出现在 `GET /v1/models`。例如 `expose_as: agnes-flash` 后，下游可用 `model: agnes-flash` 直接选择 Agnes，不会切换到 `free-fast` 的其他候选。公开 ID 必须在全局唯一，且不能与顶层路由同名；候选禁用后该 ID 会从模型列表中隐藏。

不会自动改写语义性参数：例如 Groq 的 `n` 只支持 `1`，如果下游传 `n > 1`，网关会让 Groq 返回明确错误，而不是静默篡改成单个候选。

下游配置为：

```text
Base URL: http://127.0.0.1:8080/v1
API key:  $NANOROUTER_API_KEY
Model:    free-fast
```

调用返回额外包含 `X-Nanorouter-Route`、`X-Nanorouter-Upstream` 和 `X-Nanorouter-Attempts`，可用于排障。

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

Prometheus 抓取 `GET /metrics`。仅记录供应商实际返回的 `usage`；缺失时标为 `unavailable`，不会伪造估算 token。

日志默认写到标准错误输出，采用 Zap 的 JSON 格式；只包含路由、上游、模型、状态、耗时和用量，绝不输出 prompt、模型回复或 API Key。

实现思路与关键权衡见[设计文档](docs/design.md)。

## 流式限制

一旦任何 SSE 字节已经发送给下游，网关不能再安全地改用另一模型，否则会产生重复内容或破坏工具调用。因此流式故障切换只覆盖上游尚未开始返回 SSE 前可识别的网络/HTTP 错误；已开始的流会保持透明转发。
