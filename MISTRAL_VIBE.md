# Mistral Vibe 集成

nanorouter 支持 Mistral Vibe 作为上游：通过浏览器登录换取 Mistral 平台
API key，之后走标准 OpenAI 兼容的 chat completions 协议，无需协议适配。

## 特性

- **浏览器登录（PKCE）**：对齐 mistral-vibe 客户端的 sign-in / poll /
  exchange 三步流程，自动打开浏览器，headless 机器可复制 URL 授权
- **标准协议**：上游就是 OpenAI 兼容 API，流式、工具调用等能力直接透传
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
        model: mistral-medium-3-5
        expose_as: mistral-medium-3-5
```

`base_url` 必须带 `/v1`；漏掉会命中上游 404
（`no Route matched with those values`）。

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
- 上游是标准 OpenAI 兼容 API：没有协议转换层，`anthropic`、
  `google_antigravity` 等开关不适用

## 安全说明

- 凭据文件权限 0600，写入是临时文件 + rename 的原子替换
- 网关日志不记录 prompt、completion 或 Authorization 头
- `mistral_token_file` 与容器部署：挂载 `$HOME/.nanorouter` 只读卷即可；
  key 不会由网关改写，无需可写卷
