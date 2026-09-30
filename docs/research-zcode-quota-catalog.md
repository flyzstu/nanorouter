# ZCode 配额/订阅/重置卡与模型目录：可行性调研记录

> 调研日期：2026-09-29。
> 背景：nanorouter 已完成 ZCode Coding Plan（Anthropic Messages 网关）的三档移植（`/v1/messages` 透传、Chat↔Messages 双向翻译、`login-zcode` 登录链）。本文记录在此基础上新增两项能力的调研过程：**① 配额/订阅/重置卡查询接口；② 模型目录元数据**。
> 结论先行：两项均可实现，合计约 4–6 天；配额面的凭据 nanorouter 已全部持有，模型目录需修正一个前提（详见发现三）。

## 一、调研方法

对以下三个来源做事实核查，所有结论均落到 file:line 证据：

1. 参考客户端 `zcode_coding_plan_quota.py`（dsh-zhipu-provider 仓库）——配额/重置卡的线上合同唯一成文处；
2. 参考客户端 `zcode_coding_plan_client.py`——模型清单的真实来源；
3. nanorouter 自身 `internal/router/zcode_login.go` 的 `storeZcodeToken`——确认登录后网关已持有哪些凭据。

## 二、事实发现

### 发现一：配额面的完整契约是五个端点、两种凭据

`zcode_coding_plan_quota.py:123-253` 逐端点核实：

| 端点 | 主机 | 认证 | 说明 |
| --- | --- | --- | --- |
| `GET /api/monitor/usage/quota/limit` | bigmodel.cn（zai 渠道为 api.z.ai） | Coding Plan key | 5 小时窗口、周配额、月度工具额度 |
| `GET /api/biz/subscription/list` | 同上 | Coding Plan key | 订阅、计费周期、有效期 |
| `GET /api/monitor/credit-usage/activity?startTime&endTime` | 同上 | Coding Plan key | 7 天 token 消耗与峰值日 |
| `GET /api/v1/coding-plan/reset/status` | 网关（zcode.z.ai） | **三头组合**（见下） | 重置卡余量与历史 |
| `POST /api/v1/coding-plan/reset/use` | 网关 | 同上 + body | **破坏性**：核销一张卡 |

重置卡的认证不是 Coding Plan key，而是**平台凭据对**（`zcode_coding_plan_quota.py:154-197`）：

```
Authorization: Bearer <zcode_jwt_token>
X-Bigmodel-Authorization: <oauth_access_token>
Bigmodel-Target-Type: PERSONAL
```

核销请求体为 `{"idempotency_key": <每次新生成的 uuid>, "reset_type": "FIVE_HOUR"|"WEEK"}`——`idempotency_key` 每次尝试新生成是参考客户端的既有语义（重试会合并为一次核销，而不是核销两次）。

**两种拒绝形态**（dsh-zhipu-provider AGENTS.md 的实测记录，移植时必须钉进测试）：厂商 biz 端点对坏凭据回 **HTTP 200 + 错误信封** `{"code":1000,"msg":"身份验证失败。","success":false}`；网关 reset 端点对坏凭据回**裸 401 空 body**。只看状态码会把拒绝渲染成全零卡片，只看信封会把登录问题报成网络错误。

### 发现二：重置卡需要的凭据 nanorouter 已经落盘

`internal/router/zcode_login.go` 的 `storeZcodeToken` 在 `login-zcode` 成功时写入渠道分组文件 `~/.nanorouter/zcode_token.json`（0600），每组已包含三件套：`api_key`、`zcode_jwt_token`、`oauth_access_token`。即：**配额面需要的全部凭据（key + 平台 JWT + OAuth token）现已存在，无需任何新的登录改动**。这是配额接口可行性高的根本原因。渠道来源同样现成：凭据文件按 `bigmodel`/`zai` 分组，调用厂商主机时从命中的分组推导即可。

### 发现三：模型目录不是登录时从服务端获取的

核查 `zcode_coding_plan_client.py:66-74`：参考客户端的模型清单是**硬编码静态值**：

```python
"default_model": "GLM-5.3",
"available_models": ["GLM-5.3", "GLM-5.3-Flash", "GLM-5.2", "GLM-4.7"],
```

登录链（init → poll → key 兑换）不返回任何目录：poll 回 access token + 平台 JWT + 用户名，兑换回 `id.secret` key。dsh-zhipu-provider 里的富元数据（context 窗口、价格、effort 档位、图片能力）是其 `src/catalog.ts` 的人工维护快照，AGENTS.md 明确要求与参考客户端和 pi-ai 条目双向对账。

**前提修正**：需求中"第一次登录时获取模型目录元数据"应落地为"**第一次登录时持久化并暴露**"——移植静态快照、登录成功时写入凭据文件（或伴生文件），而不是调用一个并不存在的服务端目录 API。可选实验：探测网关是否提供 Anthropic 形状的 `/v1/models`；但这是未验证合同，只能作为快照的补充，不能作为依赖。

## 三、方案评估

### 配额/订阅/重置卡接口（约 3–4 天）

- 新增 `internal/router/zcode_quota.go`：五个**容忍读取器**（不可信 JSON 进、归一化快照出），逐端点失败隔离——一个端点失败只清空自己的字段并进 `failures` 列表，绝不推导、绝不伪造数字。
- 暴露为 `GET /v1/zcode/quota`（聚合，标注每块来源与失败原因）+ `POST /v1/zcode/reset/five-hour`、`POST /v1/zcode/reset/weekly` 两个**无参端点**（沿用 dsh 的防误用设计：调用方只能要求两件事之一，不能传任意 `reset_type`）。端点由网关自身 `auth_env` 保护。
- 核销安全纪律：`idempotency_key` 每次尝试新生成；网关的任何自动路径（刷新、登录、错误恢复）不得触碰核销；因网关是无人值守服务，建议增加独立开关（配置项默认关闭，或要求独立口令），避免"花卡"暴露成扫描器可撞的常规 POST。
- 隐私注意：配额响应含订阅邮箱等账户级数据，文档需写明该端点必须在开启鉴权的实例上使用。

### 模型目录（约 1–1.5 天）

- 从 dsh `src/catalog.ts` 移植四条模型记录（GLM-5.3 / 5.3-Flash / 5.2 / 4.7 的 context、maxOutput、image、efforts、cost）为 Go 数据 + 读取器（约 200 行）。
- `login-zcode` 成功时把快照连同 `synced_at` 写入凭据文件 `catalog` 字段（或伴生 `~/.nanorouter/zcode_catalog.json`）。
- 暴露两处：`GET /v1/zcode/catalog` 独立端点；可选增强 `/v1/models` 条目（附 context/cost，OpenAI 客户端可读）。
- 维护取舍写进 design.md：目录是快照，上游加新模型需手动同步（与 dsh 同一取舍）。

## 四、风险与诚实边界

1. **配额形状是"移植而非实测"**：dsh AGENTS.md 明确记录当年没有订阅对着真实服务量过字段名，服务改字段名会让对应块降级（这正是容忍读取器存在的原因）。拿到真实 Coding Plan key 后需用真数据验证一轮，才能去掉这个标注。
2. **平台 JWT 过期**：重置卡块会随 JWT 过期而失效。按 dsh 的模式，这是**状态**而非错误——接口返回缺失原因（`reason` 字段），其余块照常渲染。
3. **请求主链路零接触**：配额是唯一触碰厂商 biz 主机的路径，生成请求路径不变，与 design.md 的"数据库只记账、不参与实时选路"边界一致。

## 五、建议实施顺序

1. `zcode_quota.go` 五个读取器 + httptest 双拒绝形态（200+信封 / 裸 401 空体）测试；
2. `/v1/zcode/quota` 聚合端点 + 失败隔离渲染；
3. 两个无参 reset 端点 + 默认关闭的开关 + 破坏性纪律测试（任何自动路径不核销）；
4. 目录快照移植 + 登录落盘 + `GET /v1/zcode/catalog`；
5. 真实 key 验证一轮字段形状，更新本文的"未实测"标注。
