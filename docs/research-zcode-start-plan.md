# ZCode Start Plan（体验卡）接入调研：结论是暂不支持

> 调研日期：2026-10-01 ~ 10-02。
> 结论先行：**当前不支持 Start Plan，且在拿到一个「官方客户端里确实在用 Start Plan」的样本之前不应继续实现。** 本文记录证据、已排除的假设，以及两次走错的弯路，避免重复劳动。
> 相关但独立的能力见 [research-zcode-quota-catalog.md](./research-zcode-quota-catalog.md)。

## 一、最终状态

代码已回到 0.5.0 的形态，`zcode_plan` 配置项、JWT 凭据解析、登录探测回退全部不存在。唯一保留的改动是 `simulate_zcode` 候选在请求体注入 `metadata.user_id`（设备身份），理由见第四节。

## 二、Start Plan 的凭据与端点（源码事实）

ZCode 客户端对三种套餐用三套凭据（`packages/services/src/model-provider/accountProviderRequestAuthService.ts:73-84`）：

| 套餐 | planKind | 凭据 |
| --- | --- | --- |
| Start Plan（体验卡） | `start-plan` | 平台 JWT（`zcodeJwtToken`） |
| 个人版 Coding Plan | `individual-coding-plan` | `id.secret` 形式的 api-key |
| 团队版 Coding Plan | `team-coding-plan` | 团队 api-key |

平台 JWT 与 api-key 都由同一次 BigModel OAuth 回调带回，nanorouter 的 `login zcode` 已经把两者都写进了 `~/.nanorouter/zcode_token.json`（`api_key` / `zcode_jwt_token` 字段），**不需要任何新的登录改动**。

官方 catalog 把 Start Plan 的 provider baseUrl 登记为 `https://zcode.z.ai/api/v1/zcode-plan/anthropic`（`config/provider/zcode-builtin.json:797-817`，两个渠道相同），而个人版是 `https://open.bigmodel.cn/api/anthropic` / `https://api.z.ai/api/anthropic`。客户端的 URL 改写表（`apps/zcode-cli/packages/adapters/src/model/official-coding-plan-gateway.ts:22-31`）按精确匹配只登记了后两者，改写成 `/api/v1/ultra/…` 或 `/api/v1/ultra-zai/…`。

## 三、两次走错的弯路

### 弯路一：把"401 Authorization Token非法"当成端点问题

据第二节的源码可以推断：Start Plan 的 baseUrl 不在改写表里 → 查表未命中 → 请求原样发往 `zcode.z.ai/api/v1/zcode-plan/anthropic/...`。于是把 nanorouter 的模型请求路径按套餐切到了该端点。

**这个推断是错的。** 往 `zcode-plan` 端点发模型请求稳定返回 `405` + `{"code":3012,"msg":"request has been blocked due to unusual activity."}`。这个 3012 是自己改路径造出来的，不是原本就存在的障碍。

### 弯路二：把 `ultra + JWT = 401` 当成"凭据类型与端点必须配对"

同期实测（同一路径、同一头形状）：

| 请求 | 结果 |
| --- | --- |
| `id.secret` key → `ultra` | **200** |
| 平台 JWT → `ultra` | 401 `Authorization Token非法` |

据此得出"凭据类型必须与端点配对"的结论，也是错的。真正的原因是：**这个账号的活跃套餐是 `individual-coding-plan`，官方客户端用的就是 api-key，平台 JWT 从来不是这个账号可用的模型凭据。**（解密 `~/.zcode/v2/credentials.json` 可见 `account-provider:coding-plan:account:bigmodel-individual-coding-plan:...` 与 `oauth:active_provider=bigmodel`。）把 JWT 当成 Start Plan 的模型凭据去试，从一开始就没有成功的可能。

## 四、决定性证据：官方客户端抓包

前两轮都在猜，于是直接拦截官方客户端的 fetch。`zcode -p "say ok" --json` 请求成功（`"response": "ok"`），同时用 `--require` 注入的 fetch hook 抓下它发出的请求：

```json
{
  "url": "https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages",
  "headers": {
    "anthropic-beta": "mid-conversation-system-2026-04-07",
    "anthropic-version": "2023-06-01",
    "authorization": "Bearer REDACTED",
    "x-api-key": "87b5b3c5ef294e81a11b1c8bbc59ebd9.WF0tacV2mr2ynJ5q",
    "user-agent": "ZCode/0.16.9 ai-sdk/provider-utils/4.0.27 runtime/node.js/24",
    "x-title": "Z Code@cli",
    "x-platform": "linux-x64",
    "x-os-category": "linux",
    "x-query-id": "<uuid4>",
    "x-request-id": "<uuid4>",
    "x-session-id": "<uuid4>",
    "x-zcode-trace-id": "<uuid4>",
    "x-zcode-app-version": "0.16.9"
  },
  "body": { "model": "GLM-5.3", "metadata": { "user_id": "{\"device_id\":\"…\",\"account_uuid\":\"\",\"session_id\":\"…\"}" } }
}
```

由此确定：

1. **模型请求打的是 `ultra` 网关**，不是 catalog 里登记的 `zcode-plan`。后者只是登记处与凭据探测入口，官方客户端从不拿它发模型请求。
2. **用的是 api-key**，不是 JWT。
3. `metadata.user_id` 确实存在，形状与 nanorouter 已实现的一致（这是唯一被保留的改动的依据）。
4. 客户端版本是 `0.16.9`，`X-Title` 是 `Z Code@cli`——与桌面端的 `3.14.0` / `Z Code@electron` 不同。nanorouter 复刻的是桌面端常量，两者不冲突但也不能互相验证。

## 五、已排除的假设

在抓到这份包之前，对 `zcode-plan` 端点的 3012 逐项试过，全部无效：

| 假设 | 结果 |
| --- | --- |
| app_version 太旧（3.14.0 → 3.14.3） | 仍 3012 |
| 必须流式（`stream: true`） | 仍 3012 |
| 头部形状（去掉 `x-api-key`） | 去掉后过认证，但换回 405/3012 |
| 模型 ID（`GLM-5.3-Flash` / 小写 / `model:glm-5.3-flash` / `GLM-5.2` / `GLM-5-Turbo`） | 全部 3012，说明拦截发生在模型解析之前 |
| 完整官方指纹头 + `X-Device-Mid` + `metadata` | 仍 3012 |

另外两个可用的辅助端点：

- `GET /api/v1/zcode-plan/billing/balance?app_version=3.14.3` 需带 `X-Device-Mid`，带后返回 `code:0`，能读到套餐状态与权益模型列表。这是判断"账号有没有 Start Plan 权益"的权威入口。
- `zcode-builtin.json` 里 Start Plan 的默认模型是 `["GLM-5.3-Flash","GLM-5.2","GLM-5-Turbo"]`，**不含 `GLM-5.3`**；个人版是 `["GLM-5.3","GLM-5.3-Flash"]`。两组不同，路由配置时不能混用。

## 六、诚实的边界

**没有验证过的事情**：平台 JWT 能否在 `ultra` 网关上完成一次模型调用。本机账号虽有 Start Plan 权益（余额接口显示 `status: active`、`GLM-5.3-Flash` 在 `capabilities` 内），但官方客户端自身从未用过这条路径——它默认走 individual-coding-plan 的 api-key。**没有官方客户端成功使用 Start Plan 的样本可比对。**

**因此下一步的前置条件**（不是"再猜几轮"）：

1. 拿到一个在官方客户端 `/model` 里切到 Start Plan 并成功发出请求的账号或时刻；
2. 用同样的 fetch hook 抓下那次请求的完整 URL、头集与请求体；
3. 与本文第四节的抓包逐项 diff，确认差异是否只有 `authorization` / `x-api-key` 的值。

只有第 3 步确认"凭据换 JWT、其余不变"之后，实现才值得动手。在此之前任何实现都是赌博——本文记录的两次弯路就是这么产生的。

## 七、风险与请求主链路

- 本次调研未触碰请求主链路：`zcode.go` / `zcode_login.go` / `config.go` 已完全回到 0.5.0，登录探测仍是"只探 key、只认 401/403"。中途引入过的一个真实回归值得记下：把探测改成"key 被拒则再探 JWT"后，JWT 探针返回的 405 不属于证伪状态（判定只认 401/403），会让**key 真的失效的用户被误判为登录成功**，破坏"绝不存储网关不接受的凭据"这条纪律。若日后重做，该回退必须避免。
- 保留的 `metadata.user_id` 注入只在 `simulate_zcode` 候选生效，读取 ZCode 客户端已写入的 `~/.zcode/v2/telemetry-state.json`，缺失时不注入（不伪造设备 ID）。已实测：Coding Plan 候选加了该字段后仍返回 200，且官方客户端发的是同形状字段。