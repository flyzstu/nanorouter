# agy-bridge：把 Antigravity `agy` CLI 接成流式上游

`agy-bridge` 是一个独立的 Go sidecar，参考 [dsh-agy-link](https://github.com/amlyczz/dsh-agy-link) 的方式：

1. 每个请求启动一个 `agy --output-format stream-json` 子进程；
2. 解析 stdout 的 NDJSON 事件（text / thinking / tool / usage）；
3. 转成 OpenAI Chat Completions 或 Anthropic Messages 的 SSE 流。

它不修改 nanorouter 主网关，nanorouter 只需要把它当成普通 HTTP 候选。

bridge 有两种 profile：

- **`model`（默认，透明转发）**：下游编程 agent 拥有工具循环，bridge 只当无状态模型。强制 `plan`、不执行工具、不使用会话绑定。请求带 `tools` 时，默认用 prompt 工具协议把工具 schema 注入 prompt，并把 agy 输出的 `tool_call` JSON 转成标准 `tool_calls`；`-tool-bridge off` 则直接 400。
- **`agent`**：让 agy 自己当 agent，保留会话连续性、工具活动映射和项目 workdir。

## 前置条件

- 已安装官方 `agy` CLI，并且 `agy` 能正常访问 Google、已完成 Antigravity 登录；
- 如果运行环境需要代理，给 sidecar 进程设置 `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY`；
- 不需要 DSH，也不需要 Node.js；`agy` 自己负责账号和凭证。

## 构建与启动

```bash
go build -o bin/agy-bridge ./cmd/agy-bridge

# 透明转发（默认）：给下游编程 agent 当纯模型
./bin/agy-bridge \
  -profile model \
  -listen 127.0.0.1:8090 \
  -agy-bin agy \
  -tool-bridge prompt \
  -models gemini-3.8-flash,claude-sonnet-4-6 \
  -default-model gemini-3.8-flash \
  -token "$AGY_BRIDGE_TOKEN"

# agent 模式：agy 自己执行工具
./bin/agy-bridge \
  -profile agent \
  -listen 127.0.0.1:8090 \
  -agy-bin agy \
  -mode skip \
  -workdir /workspace/project \
  -tool-mode annotate \
  -models gemini-3.8-flash,claude-sonnet-4-6 \
  -default-model gemini-3.8-flash \
  -token "$AGY_BRIDGE_TOKEN"
```

默认只监听 `127.0.0.1:8090`。如果监听非回环地址且没有 `-token`，进程会拒绝启动；确需如此可加 `-allow-insecure`。

`-profile model` 会强制 `-mode plan`、`-tool-mode none`、忽略 `-session-file` 和 `-workdir`（使用隔离临时目录）。`-profile agent` 才使用这些参数。

model profile 的 tools 默认走 prompt 协议（`-tool-bridge prompt`）；完全不想暴露 tools 时用 `-tool-bridge off`。

## 配置

| Flag | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `-version` | 无 | `false` | 打印版本号并退出 |
| `-profile` | `AGY_BRIDGE_PROFILE` | `model` | `model`（透明转发，无 tools）或 `agent`（agy 自己执行工具） |
| `-listen` | `AGY_BRIDGE_LISTEN` | `127.0.0.1:8090` | HTTP 监听地址 |
| `-agy-bin` | `AGY_BIN` | `agy` | agy 可执行文件 |
| `-mode` | `AGY_BRIDGE_MODE` | `plan` | 仅 agent：`plan` / `accept-edits` / `skip` |
| `-effort` | `AGY_BRIDGE_EFFORT` | 空 | 仅对 Gemini 模型传 `--effort` |
| `-workdir` | `AGY_BRIDGE_WORKDIR` | 临时目录 | 仅 agent；model profile 强制隔离临时目录 |
| `-timeout` | `AGY_BRIDGE_TIMEOUT` | `10m` | 单请求超时 |
| `-token` | `AGY_BRIDGE_TOKEN` | 空 | 客户端 Bearer / x-api-key |
| `-models` | `AGY_BRIDGE_MODELS` | 空 | 逗号分隔，供 `/v1/models` 展示 |
| `-default-model` | `AGY_BRIDGE_DEFAULT_MODEL` | 空 | 请求未带 model 时使用 |
| `-emit-thinking` | `AGY_BRIDGE_EMIT_THINKING` | `true` | OpenAI 流里输出 `reasoning_content` |
| `-tool-mode` | `AGY_BRIDGE_TOOL_MODE` | `annotate` | 仅 agent：`annotate` / `passthrough` / `none`；model 强制 `none` |
| `-tool-bridge` | `AGY_BRIDGE_TOOL_BRIDGE` | `prompt` | 仅 model：`prompt`（注入 schema、解析 tool_call JSON）或 `off`（带 tools 直接 400） |
| `-session-file` | `AGY_BRIDGE_SESSION_FILE` | 空 | 仅 agent；model 不使用会话绑定 |
| `-session-ttl` | `AGY_BRIDGE_SESSION_TTL` | `24h` | 仅 agent；会话绑定有效期 |
| `-allow-insecure` | `AGY_BRIDGE_ALLOW_INSECURE` | `false` | 允许非回环监听且无 token |

## 日志

bridge 输出结构化文本日志到 stderr，每个请求带同一个 `request_id`（同时写入响应头 `X-Request-ID`），方便把 HTTP 请求和 agy 子进程对应起来：

```text
INFO request_prepared {"request_id":"req_...","protocol":"openai","model":"...","stream":true,"messages":3,"tools":5,"prompt_tools":true,"session":false,"request_bytes":1234}
INFO agy_start        {"request_id":"req_...","model":"...","mode":"plan","prompt_bytes":4567,"conversation":false,"timeout":"5m0s"}
INFO agy_finish       {"request_id":"req_...","duration":"11.08s","lines":5,"garbage":0,"auth_failure":false,"has_result":true,"exit_code":0,"result_ok":true,"response_bytes":9}
INFO request_result   {"request_id":"req_...","model":"...","outcome":"tool_calls","tool_calls":1,"prompt_tokens":12948,"completion_tokens":5,"reasoning_tokens":0,"total_tokens":12953}
INFO request_finished {"request_id":"req_...","method":"POST","path":"/v1/chat/completions","status":200,"response_bytes":300,"duration":"11.08s","remote":"127.0.0.1:8338"}
```

- `request_prepared`：请求协议、模型、流式与否、消息数、工具数、是否走 prompt 工具协议、是否带会话、请求体字节数。
- `agy_start` / `agy_finish`：agy 子进程的模型、mode、prompt 字节数、耗时、NDJSON 行数、无法解析的行数、认证失败、退出码、result 是否成功和响应字节数。
- `request_result`：最终 outcome（`stop` / `tool_calls` / `tool_use` / `end_turn`）、工具调用数、usage。
- `request_finished`：HTTP 状态、响应字节数、总耗时、客户端地址。
- 出错时额外输出 `request_error`，带 `error_kind` / `error_code` / 单行错误信息。

日志不会记录 prompt、模型回复、工具参数、工具输出、Authorization 或 API key；只记录长度、数量和状态。`/healthz` 不记请求日志，避免探活刷屏。

## nanorouter 接入

### 透明转发（推荐给编程 agent 的纯文本场景）

```yaml
routes:
  antigravity-text:
    candidates:
      - id: agy-bridge-model
        base_url: http://127.0.0.1:8090/v1
        model: gemini-3.8-flash
        api_key_env: AGY_BRIDGE_TOKEN
        max_concurrency: 2
        attempt_timeout: 10m
```

这个 profile 下，带 `tools` 的请求会走 prompt 工具协议：bridge 把工具 schema 注入 prompt，agy 需要输出严格的 `{"tool_call":...}` JSON，bridge 再转成标准 `tool_calls`。下一轮客户端把工具结果放回 messages 即可；bridge 无状态重拼完整 transcript。

如果设置 `-tool-bridge off`，带 `tools` / `tool_choice` 会返回 400 `tools_not_supported`。

`/v1/responses` 需要给候选加 `responses_via_chat_completions: true`，由 nanorouter 转成 Chat Completions；bridge 不实现原生 `/v1/responses`。

### agent 模式

```yaml
routes:
  antigravity-agent:
    candidates:
      - id: agy-bridge-agent
        base_url: http://127.0.0.1:8090/v1
        model: gemini-3.8-flash
        api_key_env: AGY_BRIDGE_TOKEN
        max_concurrency: 1
        attempt_timeout: 10m
```

这个模式适合把 agy 当编码 agent 用；下游客户端不要同时执行自己的工具循环。

### Anthropic / Messages

```yaml
routes:
  antigravity-claude:
    candidates:
      - id: agy-bridge-anthropic
        base_url: http://127.0.0.1:8090
        model: claude-sonnet-4-6
        anthropic: true
        anthropic_messages_path: /v1/messages
        api_key_env: AGY_BRIDGE_TOKEN
        max_concurrency: 2
        attempt_timeout: 10m
```

注意 OpenAI 候选的 `base_url` 带 `/v1`，Anthropic 候选不带，路径由 `anthropic_messages_path` 指定。

## prompt 工具协议（model profile）

请求带 tools 时，bridge 会：

1. 把工具名、描述、参数 schema 追加到 prompt；
2. 要求 agy 只输出：
   ```json
   {"tool_call":{"name":"lookup","arguments":{"id":1}}}
   ```
   或多个工具：
   ```json
   {"tool_calls":[{"name":"lookup","arguments":{"id":1}}]}
   ```
3. 解析整段输出，转成 OpenAI `tool_calls` / Anthropic `tool_use`；
4. 客户端执行后，把 tool result 作为下一轮 messages 发回来；bridge 重新拼完整 prompt，继续下一轮。

这是 **prompt 级工具协议**，不是原生 function calling。优点是不需要 MCP、无状态、客户端工具循环不变；代价是依赖模型严格遵守 JSON，agy 自己的工具仍可能干扰。所以 model profile 要配合 `plan` 和空 workdir；如果模型不遵守，需要升级到 MCP 工具桥。

真实链路已验证：

- `/v1/chat/completions` + tools → `tool_calls`
- `/v1/chat/completions` 流式 + tools → `tool_calls` + `finish_reason: tool_calls`
- `/v1/responses` + tools → `function_call`
- 回传 `function_call_output` 后 → 正常文本回答
- 客户端回传 tool result 后，bridge 无状态重拼完整 transcript，继续下一轮

## 会话连续性（仅 agent profile）

bridge 读取下游请求的 `X-Session-Affinity`（回退 `X-Session-Id`），把同一个会话键 + 模型绑定到 agy 的 `conversation_id`：

- 首轮：完整 messages 拼成 prompt，不传 `--conversation`；从 init/result 捕获 conversation ID 并保存。
- 后续轮：传 `--conversation <id>`，只发送最后一条 user 及其后的 tool 结果；历史由 agy 自己维护。
- conversation 过期/不存在时，自动删除绑定并用完整 prompt 重开一轮。
- `-session-file` 可以跨进程重启保留绑定（JSON，键是会话键的 SHA-256）；不配置则只存内存。
- 没有会话头时保持无状态，每轮发送完整 prompt。

nanorouter 会原样转发 `X-Session-Affinity` / `X-Session-Id`，客户端只要在请求头里带上稳定的会话键即可。

## 流式行为

- OpenAI：`chat.completion.chunk`，文本走 `delta.content`，思考走 `delta.reasoning_content`；结束时发 `finish_reason: "stop"` 和 usage（有 token 数据时），最后 `data: [DONE]`。
- 带 `tools` 的请求会先缓冲 agy 的完整输出，判断是普通文本还是 `tool_call` JSON，再决定输出 content 还是 tool_calls；不带 tools 的请求仍然逐段流式。
- Anthropic：`message_start` → `content_block_start` → `content_block_delta(text_delta)` → `content_block_stop` → `message_delta` → `message_stop`。
- usage：优先使用 step 的单轮用量；result 信封只补缺失字段，避免续聊时把累计值当成本轮。Anthropic 的 `message_start.input_tokens` 先发 0，最终 `message_delta` 带 `output_tokens`。
- 工具：`-tool-mode=annotate`（默认）把完成的工具步骤写进 reasoning；`passthrough` 映射成 OpenAI `tool_calls` / Anthropic `tool_use`，并以 `tool_calls` / `tool_use` 结束；`none` 完全忽略工具步骤。
- 错误：按 auth / rate_limit / context_window / eligibility / network / timeout / no_result 分类，响应头带 `X-Agy-Error-Kind`，429 带 `Retry-After`（如果能确定），错误体带稳定 code。

## 工具映射的语义（仅 agent profile）

agy 是一个会自己执行工具的 agent。`passthrough` 模式把 agy **已经执行过**的工具步骤回放成标准 tool call，适合日志、审计或只渲染工具卡片的客户端；**不要让客户端再次执行这些 tool call**，否则会重复执行命令/写文件。默认的 `annotate` 模式不会产生可执行的 tool call，是网关场景更安全的选择。

`model` profile 会直接拒绝带 tools 的请求，不会静默忽略，也不会把 agy 的工具回放给下游。

## 已知边界

- **model profile 的 tools 是 prompt 协议**：依赖 agy 输出严格 JSON，不是原生 function calling；`-tool-bridge off` 才会直接 400。模型不遵守时需要 MCP 工具桥。
- **图片不落盘**：消息里的图片统一记为 `[image omitted]`。
- **无 Anthropic thinking block**：Anthropic 的 thinking block 需要签名，agy 不提供；bridge 默认只在 OpenAI 侧输出 `reasoning_content`。
- **会话仅在 agent profile 生效**：没有 `X-Session-Affinity` / `X-Session-Id` 时无法绑定 agy conversation；客户端换了模型也会按“会话键 + 模型”重新绑定。model profile 始终无状态。
- **每请求一个 agy 进程**：延迟高于常驻 HTTP 上游，不适合高 QPS；建议 `max_concurrency` 控制在 2–4。
- **ToS 风险**：Antigravity 是 Google 的订阅服务，把它作为共享网关上游可能违反服务条款，请自行评估账号风险。

## 验证

```bash
go test ./internal/agybridge
```

测试使用一个假的 `agy` shell 脚本模拟 stream-json，不访问真实 Google 服务。
