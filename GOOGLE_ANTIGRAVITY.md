# Google Antigravity 集成

nanorouter 现在支持 Google Antigravity 作为上游，可以直接使用 Google Gemini 模型。

## 特性

- **OAuth 认证**：通过浏览器完成 Google 授权，自动管理 token
- **协议转换**：自动在 OpenAI Chat Completions 和 Gemini API 之间转换
- **流式响应**：支持 SSE 流式输出
- **多模态支持**：支持文本和图片输入（计划中）

## 快速开始

### 1. 登录 Google 账号

```bash
./nanorouter login google --no-browser
```

按照提示在浏览器中完成授权，然后将回调 URL 粘贴回终端。

登录成功后，token 会保存在 `~/.nanorouter/google_token.json`。

### 2. 配置路由

复制示例配置或手动添加：

```yaml
routes:
  google-antigravity:
    strategy: weighted_round_robin
    candidates:
      - id: antigravity-gemini-2.0-flash
        base_url: https://daily-cloudcode-pa.googleapis.com
        google_antigravity: true
        google_token_file: true
        model: gemini-2.0-flash-exp
        expose_as: google-gemini-flash
        enabled: true
        attempt_timeout: 60s
```

完整示例见 `config.example.google.yaml`。

### 3. 启动并使用

```bash
# 启动 nanorouter
./nanorouter

# 在其他终端测试（使用 OpenAI 兼容格式）
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "google-gemini-flash",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

## 支持的模型

- `gemini-2.0-flash-exp` - 快速响应模型
- `gemini-1.5-pro` - 高质量模型
- `gemini-1.5-flash` - 平衡模型

更多模型请参考 Google Cloud 文档。

## 配置说明

### 必需字段

- `google_antigravity: true` - 启用 Google Antigravity 上游
- `google_token_file: true` - 使用 OAuth token 文件认证
- `base_url` - Cloud Code Assist API 端点，配裸域名：`https://daily-cloudcode-pa.googleapis.com`（主端点）或 `https://cloudcode-pa.googleapis.com`（非 Antigravity 通道）；请求路径由网关按 `v1internal:*` 协议拼接，不要在 base_url 里带版本路径

### 可选字段

- `model` - 指定 Gemini 模型 ID
- `expose_as` - 对外暴露的模型名称
- `attempt_timeout` - 请求超时时间

## Token 管理

- Token 自动保存在 `~/.nanorouter/google_token.json`
- 支持 token 刷新（TODO）
- 包含 project_id 用于 API 调用

## 限制

- 需要有效的 Google Cloud 账号和权限
- 某些高级特性（如 function calling）可能需要额外适配
- 多模态输入（图片）支持正在开发中

## 故障排除

### 登录失败

检查是否正确复制了完整的回调 URL。URL 应该包含 `code=` 参数。

### 401 未授权

Token 可能已过期，重新运行 `./nanorouter login google --no-browser`。

### 模型不可用

检查 Google Cloud 账号是否有相应模型的访问权限。

## 技术细节

- 协议转换在 `protocol_google.go` 中实现
- OAuth 流程在 `login_google.go` 中实现
- 路由集成在 `router.go` 的 `prepareAttempt` 和 `do` 方法中
