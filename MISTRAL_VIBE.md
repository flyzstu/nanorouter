# Mistral Vibe 集成

nanorouter 支持 Mistral Vibe 作为上游：通过浏览器登录换取 Mistral 平台
API key，之后请求标准 OpenAI 兼容的 chat completions 协议；响应由可选的
`mistral_compat` 规整层收敛到 OpenAI 形状。

## 特性

- **浏览器登录（PKCE）**：对齐 mistral-vibe 客户端的 sign-in / poll /
  exchange 三步流程，自动打开浏览器，headless 机器可复制 URL 授权
- **协议规整**：`mistral_compat: true` 把 content parts 折成字符串、
  thinking 移入 `reasoning_content`、剥离私有 chunk 字段 `p`，并按
  `include_usage` 补发 `choices:[]` usage 尾帧
- **凭据文件**：API key 存 `~/.nanorouter/mistral_token.json`（0600），
  原子写入；请求路径带 5 秒进程内缓存
- **Origin 校验**：登录服务返回的 sign_in_url / poll_url 必须落在 console /
  api origin 下，防止授权过程被引到第三方页面

## 快速开始

### 1. 登录 Mistral

```bash
./nanorouter login mistral
```

浏览器会自动打开 console 登录页；授权完成后命令轮询到批准状态，用
exchange_token + code_verifier 换取 API key 并保存。

headless/远程服务器：

```bash
./nanorouter login mistral --no-browser
```

把终端打印的 sign-in URL 复制到本地浏览器打开并授权，命令会自动轮询
完成后续交换。

登录成功后，key 保存在 `~/.nanorouter/mistral_token.json`。

### 2. 配置路由

复制示例配置或手动添加：

```yaml
routes:
  mistral-vibe:
    candidates:
      - id: mistral-medium-3-5
        base_url: https://api.mistral.ai/v1
        mistral_token_file: true
        mistral_compat: true
        drop_request_fields: ["store"]
        model: mistral-medium-3-5
        expose_as: mistral-medium-3-5
```

`base_url` 必须带 `/v1`；漏掉会命中上游 404
（`no Route matched with those values`）。`mistral_compat` 只改写响应：
非流式 `content` parts 折成字符串、thinking 移入 `reasoning_content`、
剥掉 choice 级 `response_error` 与非流式 `tool_calls[].index`；流式剥离
私有字段 `p` 与内联 usage，并在下游请求了 `stream_options.include_usage`
时补发标准 `choices:[]` usage 尾帧。错误体同时被包成 `{"error": {...}}`
信封。未开启时响应原样透传。

请求方向唯一的例外是 `store`：OpenAI 客户端（omp、部分 SDK）会发送
`store: false`，Mistral 以 422 `invalid_request_error` 拒绝。在候选上配
`drop_request_fields: ["store"]` 即可，清洗只影响选中该候选的请求。

### 3. 验证

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"mistral-medium-3-5","messages":[{"role":"user","content":"hi"}]}'
```

## 凭据解析顺序

1. `MISTRAL_API_KEY` 环境变量（`api_key_env: MISTRAL_API_KEY`）
2. `~/.nanorouter/mistral_token.json`（`mistral_token_file: true`）

## 端点与语义

| 端点 | 用途 |
| --- | --- |
| `POST https://console.mistral.ai/api/vibe/sign-in` | 发起登录，携带 S256 code_challenge，返回 process_id / sign_in_url / poll_url / expires_at |
| `GET poll_url` | 轮询授权状态：pending / completed（带 exchange_token）/ expired / denied / error；HTTP 410 等价 expired |
| `POST .../vibe/sign-in/{process_id}/exchange` | 用 exchange_token + code_verifier 换取 API key；HTTP 403 表示 workspace 不符合 Vibe 条件 |

轮询间隔 3 秒，连续失败 3 次终止，等待授权总时长默认 5 分钟
（`--timeout` 可调）。

## 与 Google Antigravity 的区别

- 凭据是普通平台 API key，不是 OAuth refresh token：不自动刷新，失效后
  重新 `login mistral`
- 上游协议是 OpenAI 兼容的 Chat Completions：没有请求侧转换层，
  `anthropic`、`google_antigravity` 等开关不适用；响应侧可选的
  `mistral_compat` 仅做形状规整

## 安全说明

- 凭据文件权限 0600，写入是临时文件 + rename 的原子替换
- 网关日志不记录 prompt、completion 或 Authorization 头
- `mistral_token_file` 与容器部署：挂载 `$HOME/.nanorouter` 只读卷即可；
  key 不会由网关改写，无需可写卷
