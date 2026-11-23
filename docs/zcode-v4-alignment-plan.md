# ZCode adapter 对齐 v4.0.0 计划

基准：ZCode 仓库 `/root/dev/github/ZCode` @ `a4b44451`（`chore: sync ZCode source v4.0.0`）。
目标：nanorouter 发出的单次 `POST /api/v1/ultra[-zai]/anthropic/v1/messages` 在**请求行 + 全部头 + body 顶层键 + 鉴权材料生命周期**上与官方客户端一致。

不追求：TLS 指纹、HTTP/2 帧序、SDK 内部重试（官方 `maxRetries: 0`，本来就没有）。

---

## 0. 官方请求指纹（比对基准）

来源：`apps/zcode-cli/packages/adapters/src/model/model-execution.ts:346-352`、`runner-options.ts:88-93,148-153`、`model-config.ts:57-70`、`runner-attribution.ts:20-28`、`node_modules/@ai-sdk/anthropic@3.0.81/dist/index.mjs:5833-5841`、`@ai-sdk/provider-utils@4.0.27/dist/index.mjs:2375-2395`。

| 头 | 官方值 | nanorouter 现状 |
|---|---|---|
| `authorization` | `Bearer <PAT>` | 有，但是长期 key |
| `x-api-key` | `<PAT>` | 有 |
| `anthropic-version` | `2023-06-01` | 有 |
| `anthropic-beta` | `advanced-tool-use-2025-11-20`（coding plan 默认开） | **缺** |
| `user-agent` | `ZCode/4.0.0 ai-sdk/anthropic/3.0.81 ai-sdk/provider-utils/4.0.27 runtime/node.js/v24.x` | 仅 `ZCode/3.17.1` |
| `X-Title` | `Z Code@cli` | `Z Code@electron` |
| `X-ZCode-App-Version` | `4.0.0` | `3.17.1` |
| `HTTP-Referer` / `X-Release-Channel` / `X-Client-Language` / `X-Client-Timezone` / `X-ZCode-Agent` / `X-Platform` / `X-Os-Category` / `X-Os-Version` | 同左 | 一致 |
| `X-Device-Mid` | 条件 | 一致 |
| `x-request-id` / `x-session-id` / `x-zcode-trace-id` | 有 | 一致 |
| `x-query-id` / `x-zcode-query-source` / `x-zcode-session-features` | 有 | **缺** |
| `x-zcode-session-type` | `main`/`subagent`/`side_chat`/`other` 动态 | 恒 `main` |
| `x-session-affinity` | **无此头** | 多发了 |

body：唯一差异项 `metadata.user_id` 已是官方形状 `{"device_id","account_uuid":"","session_id"}`，不动。

---

## 1. 差异清单

| # | 位置 | 现状 | 目标 | 级别 |
|---|---|---|---|---|
| A1 | `internal/router/zcode_login.go:412-421` | `GET .../api_keys/copy/{id}` 取 `secretKey`，拼 `id.secret` 长期 key | `POST .../api_keys/{id}/access_tokens` → `data.accessToken` | P0 |
| A2 | `internal/router/zcode_login.go:403` | 创建体 `{"name":"zcode-api-key"}` | 补 `usageScene: 1`；团队补 `keyType: 2` | P0 |
| A3 | `internal/router/zcode_login.go:363-436` | 一套 headers 走全程 | 拆三套：customerInfo 裸 token / 个人 Key 列表 zai=Bearer / 团队 裸+biz 头 | P0 |
| A4 | `internal/router/zcode_login.go:429-` | 不筛项目类型 | 个人项目排除 `projectType === "2"` | P0 |
| A5 | 新增 `internal/router/project_token.go` | 无 | PAT 缓存与刷新（见 §3） | P0 |
| A6 | `internal/router/zcode_credential_store.go:240-254` | 8 列，无有效期 | 加 `api_key_id`/`organization_id`/`project_id`/`expires_at`，幂等 `ALTER TABLE` | P0 |
| B1 | `internal/router/zcode.go:29` | `ZcodeClientVersion = "3.17.1"` | `"4.0.0"` | P1 |
| B2 | `internal/router/zcode.go:39` | `zcodeTitle = "Z Code@electron"` | `"Z Code@cli"` | P1 |
| B3 | `internal/router/router.go:1632` | 发 `x-session-affinity` | 删除 | P1 |
| B4 | `internal/router/router.go:1626-1633` | 4 个 id 头 | 补 `x-query-id`、`x-zcode-query-source`、`x-zcode-session-features` | P1 |
| B5 | `internal/router/zcode.go:156` | `x-zcode-session-type` 恒 `main` | 按请求形态派生 | P1 |
| B6 | `internal/router/router.go:1590` | UA 只有 `ZCode/<ver>` | 追加 `ai-sdk/anthropic/3.0.81 ai-sdk/provider-utils/4.0.27 runtime/node.js/v24.x` | P1 |
| B7 | `internal/router/router.go:1595-1640` | 无 beta 头 | payload 含 `tools` 时加 `anthropic-beta: advanced-tool-use-2025-11-20` | P1 |
| C1 | `internal/router/zcode_usage.go:30` | `https://bigmodel.cn` 写死 | 按 channel 切，zai → `https://api.z.ai` | P2 |
| C2 | `internal/router/zcode_v2.go:104-140` | 按 key 名含 `zai` 归类 | 显式识别 `oauth:zai_cn:*` | P2 |
| D1 | 8 处渠道字面量 | 只认 `bigmodel`/`zai` | `zai_cn` 只做显式拒绝 + 注释（非套餐家族，无业务端点契约） | 不做 |

`zai_cn` 依据：`docs/zcode-login-rework.md:333` —"中国区个人业务端点尚无契约证据，本阶段只接已明确的身份授权和 BigModel 关联材料"。

---

## 2. 分阶段

### Phase 1 — 请求指纹对齐（B1–B7，无外部依赖）✅ 2026-11-23 已落地

改动：`zcode.go:29,39,150-160`、`router.go:1590,1626-1635`。
新增：`deriveZcodeRequestIDs` 扩出 `queryID`/`features`；`zcodeBetaHeaders(payload)`。
验证：`go test ./internal/router -run 'Zcode' -count=1`；`zcode_test.go:65-74` 断言改为显式 `"4.0.0"`，新增 beta/UA 断言。
落地说明：B4 的 `x-zcode-query-source`/`x-zcode-session-features` 网关无值可填，按官方省略语义不发（官方同样在无值时省略）；B5 下游透传合法值否则 `main`。全量 `go test ./...` 通过。

### Phase 2 — 换证链改 PAT（A1–A4）✅ 2026-11-23 已落地


```
zcodeExchangeChain
  ├─ getCustomerInfo         ← 裸 token（两渠道一致）
  ├─ zcodeChooseProject      ← 个人默认路径不过滤 projectType
  ├─ GET/POST api_keys       ← 个人: zai=Bearer / bigmodel=裸
  └─ POST {id}/access_tokens ← 新增，取 accessToken
```

创建体：`{name, usageScene: 1}`（个人默认路径；团队分支未实现）。
`probeZcodeKey` 的 `max_tokens:1` 探测改在签发 PAT 之后跑。
验证：`zcode_login_test.go` 的 `copy/111` 夹具已换成 `access_tokens`。
落地说明：`exchangeZcodeKey` 返回 PAT（`access_tokens` + `Bearer` + `{clientType,clientVersion}`）；`zcodeBizHeaders` 拆三套；`probe` 移到签发之后；A4 按官方默认路径**不过滤** `projectType`（官方仅团队/显式选择分支过滤，个人默认分支不过滤）；团队分支（`keyType: 2`、团队 headers）网关暂不支持——当前登录链只走个人默认路径，需要时再补。`zcode_login_test.go` 夹具已换 `access_tokens`（`pat-token-1`）。

### Phase 3 — PAT 生命周期（A5–A6）✅ 2026-11-23 已落地

`internal/router/zcode_project_token.go`（原计划文件名 `project_token.go`，按网关 `zcode_*` 命名习惯改名），语义照抄 `packages/shared/src/project-access-token.ts`：

- `expiresAt = min(data.expiresAt*1000, startedAt + data.expiresIn*1000)`
- `refreshAt = expiresAt - 120s - random()*60s`
- 临时故障（network / timeout / 429 / 5xx）且旧 PAT 未过期 → 降级复用
- 401 / 403 / 404 / `enable:false` / 畸形 → 立即失效
- `rejectedFingerprint`（SHA-256 hex）只废弃匹配的那一代

scope key = `[origin, family, org, project, apiKeyId, usageScene, loginGeneration]`。
`resolveCandidateAPIKey`（`zcode.go:267`）在 `ZcodeTokenFile` 命中 API key 时改走缓存换 PAT。
DB：`ensureZcodeCredentialTable` 加 4 列（`api_key_id`/`organization_id`/`project_id`/`pat_expires_at`），幂等迁移兼容 SQLite/PostgreSQL 错误文案。
验证：`zcode_project_token_test.go` 覆盖 6 条路径——首签成功（含缓存命中）、提前刷新降级、401 立即失效、`enable:false`、429 带 `retry-after`（等待约 1s 恰重试一次）、超限 retry-after（30s → 不重试、交还 `project_token_http_429`）。全量 `go test ./...` 通过。
落地说明：OAuth 世代变化（`session.generation`）使旧 scope 键自然失效；凭据文件/DB 同时落盘 PAT 位置与有效期供直读；401/403 回调经 `noteZcodePATRejected` 废弃当代币；zai 渠道仅在真正刷新时才换 business JWT。

### Phase 4 — 用量/凭据（C1–C2）✅ 2026-11-23 已落地

`zcode_usage.go:110-120` 加 `baseURLFor(channel)`；`:426-433` 的回退同时切域。
验证：`zcode_usage_test.go` 现有 8 个 Test 里 2 个 mock 了固定 baseURL。
落地说明：`fetch` 改按 channel 经 `baseFor` 选域（测试桩覆盖优先），缓存键按域隔离；v2 解析显式跳过 `zai_cn/zai-cn` 条目。`zai_cn` 仍只做显式拒绝（D1 不做）。

### Phase 5 — 文档

`docs/design.md:225-241`：版本声明改 4.0.0、`id.secret` 改 PAT、写明"为什么本地落 PAT 有效期"（官方不落盘，此处必然分叉）。

---

## 3. PAT 响应契约（抄自 `packages/shared/src/project-access-token-response.ts`）

```ts
projectTokenSuccessCodeSchema = z.union([200, 0, "200", "0"])
data 缺 enable → 预处理补 true
discriminatedUnion([
  { enable: false },
  { enable: true, accessToken: string(min1), tokenType: "Bearer",
    expiresIn: number(positive), expiresAt: number(positive) },
])
```

错误码：`project_token_disabled` / `project_token_project_unavailable` / `project_token_invalid_response` / `project_token_request_failed` / `project_token_network_error` / `project_token_timeout` / `project_token_http_<status>`。

---

## 4. 阻塞与决策点

A1/A3/A5 已按路径 2 落地（schema 实现 + httptest 夹具，全量测试通过），待活凭据回归验证：`zcode_credentials` 只有 `bigmodel` 一行 49 字符 key，两个地域直连都返回 `{"error":{"message":"身份验证失败。","type":"1000"}}`。风险项（`expiresAt` 秒 vs 毫秒、`enable` 缺省）已按官方 schema 处理，回归时重点核对。

回归步骤（凭据到位后）：备份 `~/.nanorouter/zcode_token.json` → 新 token 跑 `login` → 发一条 bigmodel/zai 请求看 `x-api-key` 是否为新签 PAT → 观察 PAT 缓存命中与刷新日志。

### 渠道选择

国内用户走 `bigmodel`（`zcode_channel: bigmodel`），网关路径 `/api/v1/ultra/anthropic/v1/messages`；海外走 `zai`（`/api/v1/ultra-zai/...`）。两渠道凭据互不通用——用 A 渠道的 key 打 B 渠道网关必然 401。

当前 `config.yaml:97,106` 两个候选都是 `zcode_channel: zai`，启动体检按该渠道探测 → 401 → 自动触发 zai 登录。国内部署应改 `bigmodel`。
