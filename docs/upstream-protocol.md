# 上游协议：解包与实测结论

本文件记录 `raccoon2api` 依据的事实，以及每条事实的来源。来源只有两类：

- **解包**：本机 `D:\Programe\raccoonoffice\@raccoonoffice-desktop\resources\` 下的
  Electron 产物（`app.asar`）与 ACP 运行时（`box-agent-runtime\bin\box-agent-acp.exe`，PyInstaller 单文件）。
  `app.asar` 直接按 asar 头解析；`box-agent-acp.exe` 从尾部 CArchive 解出内嵌 PYZ（4420 个条目，
  `box_agent` 216 个模块），按常量表与指令流读取，未做源码级还原。
- **实测**：用本机桌面端已登录的 token 直接请求 `xiaohuanxiong.com`（只读端点 + 少量对话请求）。

> 客户端版本：`@raccoon/office-desktop` **1.1.3**（`app.asar` 内 `package.json`）；
> ACP 运行时 `box-agent` 1.0.0（`box-agent-runtime/manifest.json`），内嵌 Python 3.12.13、openai-python 2.8.0。

## 1. 端点

| 用途 | 方法 + 路径 | 证据 |
| --- | --- | --- |
| 模型目录 | `GET https://xiaohuanxiong.com/api/web/llm/v2/model_catalog` | 桌面端 `build/electron/main/client/models/hostedModels.js`（`CATALOG_ENDPOINT`）；实测 200 |
| 对话 | `POST https://xiaohuanxiong.com/api/web/llm/v2/chat/completions` | ACP 用 `openai.AsyncOpenAI(base_url=<apiBase>)` 拼出（`box_agent/llm/openai_client.py`）；`apiBase` 由宿主写进 `config/model-profiles.json`（本机实测值就是上面的前缀）；实测 200 |
| token 刷新 | `POST https://xiaohuanxiong.com/api/web/auth/v1/refresh` | `box_agent/auth.py`（`_xiaohuanxiong_refresh_url`），body `{"refresh_token": ...}`，15s 超时，**不带 Authorization** |
| 登录态文件 | `~/.office-raccoon/agent/config/auth.json`（本机实际路径） | 桌面端 `agent/boxAgentConfig.js` 写 `authFile`；默认回退 `~/.box-agent/config/auth.json`（`box_agent/auth.py`） |
| 设备指纹 | `%APPDATA%\office-raccoon\desktop-device-identity.json` 的 `clientDeviceId` | 桌面端 `remote/connectors/mobile/device/desktopDeviceIdentity.js`；ACP 只从 `_meta.client_info.deviceId` 收值 |

`/api/web/llm/v2/models` 不存在（实测纯文本 `404 page not found`），模型清单只能走 `model_catalog`。

## 2. 请求头

ACP 侧分三组拼装，最终作为 `extra_headers` 交给 openai-python：

**A. 客户端元信息**（仅当 `api_base` 属于 `xiaohuanxiong.com`）— `box_agent/client_info.py`

```
x-client-name: <client_info.name>            # 桌面端传 package.json 的 name → @raccoon/office-desktop
x-client-platform: desktop-windows-x64       # normalizeDesktopPlatform(win32, x64)
x-client-version: v1.1.3                     # 必须匹配 v?<x.y.z>，不匹配整条丢弃
x-client-os-version: 10.0.26100              # Electron getSystemVersion()
x-client-channel: official                   # BOX_AGENT_CLIENT_CHANNEL
x-client-device-id: <clientDeviceId>
```

**B. 会话关联** — `box_agent/llm/base.py`

```
X-RACCOON-Session-ID: <upstream session id>  # 会话内稳定
X-RACCOON-Turn-ID: <session>-turn-<n>        # 缺省由 session + 轮次生成
X-RACCOON-Title: <会话标题>                   # 非 ASCII 以 UTF-8 字节发送
X-RACCOON-Call-Kind: <种类>                   # 仅 context_summary / title / follow_up_suggestions；主回合不发
```

**C. 鉴权** — `box_agent/auth.py`

```
Authorization: Bearer <auth.json 的 access_token>
X-Org-Code: <auth.json 的 office_identity>    # 值非空且不是 "personal" 才发
```

**D. openai-python 2.8.0 自带**（`openai/_base_client.py` 常量表）

```
Accept: application/json
Content-Type: application/json
User-Agent: AsyncOpenAI/Python 2.8.0
X-Stainless-Lang: python
X-Stainless-Package-Version: 2.8.0
X-Stainless-OS: Windows
X-Stainless-Arch: x64
X-Stainless-Runtime: CPython
X-Stainless-Runtime-Version: 3.12.13
x-stainless-retry-count: 0
x-stainless-read-timeout: <timeout 秒>
```

实测：单独去掉或改动上述任何一项都不会被拒；把非 ASCII 标题放进 `X-RACCOON-Title` 也不会被拒
（Go 的 HTTP/1.1 与 HTTP/2 传输都能发出 UTF-8 头值）。网关目前**不校验**这些头，但会话 id 每请求随机
会让上游看到「新会话 + 完整历史」的矛盾，属于该伪装的意义所在。

## 3. 请求体

ACP 的构造顺序（`box_agent/llm/openai_client.py`）：

| 字段 | 值 |
| --- | --- |
| `messages` | 必带 |
| `max_tokens` | 托管端点缺省 **80000**；用户自配端点缺省 63999 |
| `model` | 非空才写 |
| `tools` | 非空才写，标准 `{"type":"function","function":{...}}` |
| `reasoning_effort` | 按模型族：`kimi-k3`/`glm-5-3`/`gemini` 用 high/low；`gpt-6-sol|luna` + tools 用 `none` |
| `extra_body` | `deepseek`/`doubao` → `{"thinking":{"type":"enabled\|disabled"}}`；`qwen` → `{"enable_thinking":bool}` |
| `stream` / `stream_options` | 流式时 `true` / `{"include_usage":true}` |

**明确不发送**：`temperature`、`top_p`、`n`、`stop`、`presence_penalty`、`frequency_penalty`、
`tool_choice`、`user`、`metadata`、`response_format`、`parallel_tool_calls`、任何 session/conversation 字段。

**网关的字段校验（实测）**：未知顶层字段直接 500
`{"error":{"code":"500","message":"Unexpected keyword argument 'xxx'."}}`。
已确认被拒：`conversation_id`、`cache_control`。
已确认接受：`max_completion_tokens`、`temperature`、`top_p`、`n`、`stop`、`presence_penalty`、
`frequency_penalty`、`logit_bias`、`logprobs`、`top_logprobs`、`user`、`seed`、`response_format`、
`tool_choice`、`parallel_tool_calls`、`service_tier`、`store`、`metadata`、`modalities`、`audio`、
`prediction`、`reasoning`、`reasoning_effort`（含 `off`/`none`/`minimal`）、`web_search_options`、
`functions`、`function_call`、`thinking`、`enable_thinking`、`stream_options`、`prompt_cache_key`、
`verbosity`、`conversationId`（驼峰版）。

`role` 只接受 `system / assistant / user / tool / developer`；`function` 会被 400 拒绝。

## 4. 响应

非流式是标准 `chat.completion`，`message` 里除 `content` 外还带：

- `reasoning_content`（思考文本；部分模型用 `reasoning`）
- `function_call: {"arguments":"","name":""}`（恒在，空值）
- `provider_specific_fields`

`usage` 含 `completion_tokens_details.reasoning_tokens` 与 `prompt_tokens_details.cached_tokens`。

流式是标准 SSE：`data: {"object":"chat.completion.chunk",...}`，每个 delta 同时带
`content: ""` 与 `reasoning_content`，末尾一帧 `finish_reason`，随后一帧只带 `usage`，最后 `data: [DONE]`。

未知模型**不报错**，直接回落到目录默认模型（`raccoon-8c4485`）并在响应里改 `model` 字段——
所以模型名校验必须由反代自己做。

## 5. 模型目录

```json
{"code":0,"message":"success","data":{"categories":[{"type":"chat","default_model":"raccoon-8c4485",
  "models":[{"name":"...","model_name":"sn-glm-5-3-flash","description":"GLM-5-3-Flash",
             "display_description":"...","visible":true,"tags":["general","chat","fast"],
             "params":{"context_window":1000000,"max_tokens":100000},
             "billing_multiplier":0.2,"billing_effective_multiplier":0.2,
             "billing_category":"lite","ability_level":2,"billing_status":"normal"}]}]}}
```

- 取 `type == "chat"` 的分类；`model_name` 才是 wire id（`name` 与它目前相同，但不保证）。
- 上下文与输出上限在 `params.context_window` / `params.max_tokens`（实测各模型不同：`sn-sensenova-*` 是 256000/63999）。
- **`tags` 里的 `vision` 不可信**（实测 2026-10-24）：
  - `raccoon-8c4485`（有 `vision`）能看图；
  - `sn-glm-5-3`（有 `vision`）回答「我没有看到您上传的图片」；
  - `sn-glm-5-3-flash`（无 `vision`）实际能看图。
  插件默认把 `sn-glm-5-3` 放进 `vision_denylist`。

## 6. 鉴权与刷新

- `auth.json` 键：`access_token`（ACP 也认 `token` / `auth_token`）、`refresh_token`（可选）、`office_identity`。
- 本机这份是 **IAM 令牌**：`exp` 在 30 天后，**没有 `refresh_token`**，`office_identity = "personal"`。
  JWT claims 实测：`exp / nbf / iat / jti / sid / sub / iss / aud / ver`，其中
  `sub = pu-…`（账号 id，稳定）、`sid = ses-…`（登录会话）、`iss = xiaohuanxiong.com/iam`（所有账号相同）。
- ACP 的刷新窗口是 300s，刷新后写回同一个 `auth.json`（原子写、0600）。
- **风险**：桌面端与反代共用一份 `refresh_token` 时，两边刷新会互相顶掉。
  所以插件根本不读桌面端的登录态：账号由用户上传凭据 JSON 或授权码登录加入，各自用自己的 refresh_token。

## 6.5 登录链路（1.1.3 实测）

| 通道 | 状态 | 证据 |
| --- | --- | --- |
| 授权码 `GET /code/authorize?login_source=desktop&appname=办公小浣熊客户端` | **可用**（307 → `/authorize`，页面 200） | 桌面端 `account/desktopLogin.js`（`DESKTOP_AUTH_PARAMS`、`DESKTOP_AUTH_PATH = '/authorize'`）；实测 307/200 |
| 授权码兑换 `POST /api/web/auth/v1/login_with_authorization_code {"authorization_code": code}` | **可用** | 桌面端 `desktopLogin.js:exchangeDesktopLoginCode`；实测无效 code 返回 400 `200035 authorization_code_not_found_error` |
| 回调深链 `office-raccoon://auth/callback?code=…` | 桌面端自己消费 | 同上（`parseDesktopLoginCode`），**无 state 校验**，码一次性 |
| 微信扫码 `POST /login_with_qrcode_code {"qrcode_code": code}` | 轮询仍可用（实测 `status: pending`），但二维码页 `/login/mp` **404** | 桌面端 `account/accountQrHost.js` 仍拼 `/login/mp?code=…&appname=小浣熊官网`；实测该页 404，故插件不提供扫码 |

因此插件的「授权链路」只有一条：生成 `/code/authorize?...&state=<32hex>` 链接 → 用户登录 →
把回调地址里的 `code` 交给插件 → 插件调 `login_with_authorization_code` 换 token。
因为桌面端装了会抢先消费这个一次性码，抢不到时走「桌面端登录 + 插件自动从 `auth.json` 导入」这条更可靠的路。

## 6.6 授权页的 loopback 回调约定（`/authorize` 页面 bundle 逆向）

`_next/static/chunks/app/(identity)/authorize/page-*.js` 里 `y()` 是参数校验、`h()` 是拼回调：

```
允许的参数: login_source, appname, state, redirect, ide, org, lang, desktop_callback
k(redirect, state):
  redirect 必须匹配 ^http://127\.0\.0\.1:[1-9][0-9]{3,4}/auth/callback$（端口 ≥1024）
  state    必须匹配 ^[a-f0-9]{64}$
带 desktop_callback 时: 必须 == "loopback" 且 login_source=="desktop" 且 ide 为空 且 k(redirect,state)
最终跳转: desktop && !desktopCallback ? "office-raccoon://auth/callback" : redirect
         再 set("code", code)（desktop 分支用 code，web 分支用 authorization_code）+ set("state", state)
```

也就是说：**带上 `redirect=http://127.0.0.1:<port>/auth/callback&desktop_callback=loopback` 时，
页面会把一次性 code 直接交回这个 loopback 地址**，而不是丢给 `office-raccoon://` 深链。
服务端不做校验（两种参数都返回 200），校验全在前端 JS，所以按上表拼就能拿到回调。

## 6.7 积分（余额）接口

桌面端 `out/_next/static/chunks/4702-*.js` 里的 `PointsService`：

| 用途 | 请求 | 说明 |
| --- | --- | --- |
| 余额 | `GET /api/web/points/v1/balance` | 带 `X-Org-Code`（仅非 personal 组织）；`code:0` |
| 账单 | `GET /api/web/points/v2/bills?paging.limit=20&type=all[&cursor=…]` | 分页游标；`biz_id = <sessionId>:<turnId>`，`points` 负数为消耗 |

实测响应（2026-10，桌面端 1.1.3；数值为示例）：

```json
{"code":0,"message":"success","data":{"available_points":12345,"daily_points":345,
 "monthly_points":0,"reward_points":12000,"topup_points":0,"enterprise_topup_points":0,
 "total_chargeable_points":12345,"enterprise_reserved":false,"topup_frozen":false,
 "owner_type":"users","owner_id":1000001}}
```

所以「额度」是可查的真实数字，不必用 `max_tokens=1` 去试消费（那只是接口消失时的兜底）。

账单行形如：

```json
{"biz_id":"<sessionId>:<turnId>","biz_type":"chat","points":-12,"created_at":"2026-10-27 18:00:00"}
```

桌面端按 `biz_id` 的第一段分会话、第二段分 turn；一个 turn 可能有多行（输入/输出分开计费），控制台显示的是这些行的 `points` 之和。插件把 `X-RACCOON-Session-ID` / `X-RACCOON-Turn-ID` 写进每个请求，请求结束后用同一对值匹配 `biz_id`，所以不需要再发一次会产生消费的请求。

## 7. 用这些事实做了什么

| 事实 | 插件里的落点 |
| --- | --- |
| 端点与目录结构 | `catalog.go`（`parseCatalog`）、`executor.go`（`send`） |
| 头集合 | `identity.go`（`upstreamHeaders`） |
| 会话 id 稳定性 | `identity.go`（`conversationFrom` / `stableSessionID`，`ses-<uuidv7>`） |
| 字段白名单与 `max_tokens` 缺省 | `payload.go`（`chatFieldAllowlist`、`normalizeChatPayload`） |
| `max_tokens` 上限 | `catalog.go`（`modelMaxTokens`）+ `payload.go` 的 clamp |
| 思考参数方言 | `payload.go`（`normalizeReasoningEffort`，`off/none/minimal → low`） |
| 未知模型静默回落 | `catalog.go`（`resolveModel` 先于上游校验） |
| `vision` 标签不可信 | `catalog.go`（`imageInputEnabled` + `vision_denylist`） |
| IAM 令牌无 refresh_token | `accounts.go`（`ensureAccountFresh` 的兜底与错误文案） |
| 授权码登录链路 | `login.go`（`authorizeURL` / `exchangeAuthorizationCode` / `completeLogin`） |
| 积分余额接口 | `quota.go`（`fetchPointsBalance` / `checkQuota`） |
| 单次请求积分 | `quota.go`（`fetchPointsBills` / `pointsForTurn`）、`requests.go`（`requestEntry.Points`） |
| 扫码页已失效 | 不实现扫码，只保留授权码 |

## 8. 复现方式

```bash
# 只读端点
curl -s -H "Authorization: Bearer $TOKEN" https://xiaohuanxiong.com/api/web/llm/v2/model_catalog

# 一次最小对话（注意中文在 Git Bash 里要写进文件再 -d @file，避免编码被 shell 破坏）
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d @req.json https://xiaohuanxiong.com/api/web/llm/v2/chat/completions
```

插件的 live 测试（花积分，默认跳过）：

```bash
RACCOON2API_LIVE=1 \
RACCOON2API_AUTH_FILE="$HOME/.office-raccoon/agent/config/auth.json" \
RACCOON2API_DEVICE_FILE="$APPDATA/office-raccoon/desktop-device-identity.json" \
CGO_ENABLED=0 go test -run TestLive -v .
```
