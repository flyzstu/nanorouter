# agy-bridge：把 Antigravity `agy` CLI 接成流式上游

`agy-bridge` 是一个独立的 Go sidecar，参考 [dsh-agy-link](https://github.com/amlyczz/dsh-agy-link) 的方式：

1. 每个请求启动一个 `agy --output-format stream-json` 子进程；
2. 解析 stdout 的 NDJSON 事件（text / thinking / tool / usage）；
3. 转成 OpenAI Chat Completions 或 Anthropic Messages 的 SSE 流。

它不修改 nanorouter 主网关，nanorouter 只需要把它当成普通 HTTP 候选。

## 前置条件

- 已安装官方 `agy` CLI，并且 `agy` 能正常访问 Google、已完成 Antigravity 登录；
- 如果运行环境需要代理，给 sidecar 进程设置 `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY`；
- 不需要 DSH，也不需要 Node.js；`agy` 自己负责账号和凭证。

## 构建与启动

```bash
go build -o bin/agy-bridge ./cmd/agy-bridge

./bin/agy-bridge \
  -listen 127.0.0.1:8090 \
  -agy-bin agy \
  -mode plan \
  -models gemini-3.8-flash,claude-sonnet-4-6 \
  -default-model gemini-3.8-flash \
  -token "$AGY_BRIDGE_TOKEN"
```

默认只监听 `127.0.0.1:8090`。如果监听非回环地址且没有 `-token`，进程会拒绝启动；确需如此可加 `-allow-insecure`。

默认 `-mode plan`，agy 不会执行写文件/命令。需要完整 agent 行为时显式使用 `-mode skip` 或 `-mode accept-edits`，并自行承担风险。

## 配置

| Flag | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `-listen` | `AGY_BRIDGE_LISTEN` | `127.0.0.1:8090` | HTTP 监听地址 |
| `-agy-bin` | `AGY_BIN` | `agy` | agy 可执行文件 |
| `-mode` | `AGY_BRIDGE_MODE` | `plan` | `plan` / `accept-edits` / `skip` |
| `-effort` | `AGY_BRIDGE_EFFORT` | 空 | 仅对 Gemini 模型传 `--effort` |
| `-workdir` | `AGY_BRIDGE_WORKDIR` | 临时目录 | agy 的 cwd；默认每次启动创建隔离临时目录 |
| `-timeout` | `AGY_BRIDGE_TIMEOUT` | `10m` | 单请求超时 |
| `-token` | `AGY_BRIDGE_TOKEN` | 空 | 客户端 Bearer / x-api-key |
| `-models` | `AGY_BRIDGE_MODELS` | 空 | 逗号分隔，供 `/v1/models` 展示 |
| `-default-model` | `AGY_BRIDGE_DEFAULT_MODEL` | 空 | 请求未带 model 时使用 |
| `-emit-thinking` | `AGY_BRIDGE_EMIT_THINKING` | `true` | OpenAI 流里输出 `reasoning_content` |
| `-emit-tool-activity` | `AGY_BRIDGE_EMIT_TOOL_ACTIVITY` | `false` | 把完成的 agy 工具步骤写成 reasoning 注解 |
| `-allow-insecure` | `AGY_BRIDGE_ALLOW_INSECURE` | `false` | 允许非回环监听且无 token |

## nanorouter 接入

### OpenAI / Chat Completions

```yaml
routes:
  antigravity:
    candidates:
      - id: agy-bridge-openai
        base_url: http://127.0.0.1:8090/v1
        model: gemini-3.8-flash
        api_key_env: AGY_BRIDGE_TOKEN
        max_concurrency: 2
        attempt_timeout: 10m
```

客户端调用 nanorouter 的 `/v1/chat/completions`，候选侧走 bridge 的 `/v1/chat/completions`。

如果下游使用 `/v1/responses`，给候选加：

```yaml
        responses_via_chat_completions: true
```

bridge 目前不实现原生 `/v1/responses`。

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

## 流式行为

- OpenAI：`chat.completion.chunk`，文本走 `delta.content`，思考走 `delta.reasoning_content`；结束时发 `finish_reason: "stop"` 和 usage（有 token 数据时），最后 `data: [DONE]`。
- Anthropic：`message_start` → `content_block_start` → `content_block_delta(text_delta)` → `content_block_stop` → `message_delta` → `message_stop`。
- usage：agy 的 result 信封优先；否则取各 step 的最大值。Anthropic 的 `message_start.input_tokens` 先发 0，最终 `message_delta` 带 `output_tokens`。

## 已知边界

- **下游 tools 不会被执行**：agy 使用自己的工具；下游传入的 `tools` 只作为上下文保留，bridge 不会把它们映射成可执行的 `tool_calls`。需要看 agy 工具活动时可开 `-emit-tool-activity`（仅 OpenAI reasoning）。
- **图片不落盘**：消息里的图片统一记为 `[image omitted]`。
- **无 Anthropic thinking block**：Anthropic 的 thinking block 需要签名，agy 不提供；bridge 默认只在 OpenAI 侧输出 `reasoning_content`。
- **无会话持久化**：每次请求把完整 messages 拼成 prompt，不传 `--conversation`；多轮上下文由调用方每轮完整发送。后续可按 `X-Session-Affinity` 增加 `--conversation` 绑定。
- **每请求一个 agy 进程**：延迟高于常驻 HTTP 上游，不适合高 QPS；建议 `max_concurrency` 控制在 2–4。
- **ToS 风险**：Antigravity 是 Google 的订阅服务，把它作为共享网关上游可能违反服务条款，请自行评估账号风险。

## 验证

```bash
go test ./internal/agybridge
```

测试使用一个假的 `agy` shell 脚本模拟 stream-json，不访问真实 Google 服务。
