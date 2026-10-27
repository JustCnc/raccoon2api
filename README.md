# raccoon2api — 商汤小浣熊 → OpenAI 兼容反代的 CLIProxyAPI 插件

把本机已登录的「商汤小浣熊」桌面端（`xiaohuanxiong.com` 的 LLM 网关）注册成 CPA 的一个 provider，
对外提供 `/v1/chat/completions`（流式 + 非流式）、`/v1/models`、工具调用透传，并自带一个内嵌控制台。

- 上游：`https://xiaohuanxiong.com/api/web/llm/v2`（模型目录 `model_catalog`，对话 `chat/completions`）
- 账号：只从「上传凭据 JSON」或「授权码登录」进入账号池，插件不读桌面端的登录态
- 伪装：请求头与请求体逐项对齐官方运行时（见下文「伪装细节」）
- 多账号：优先级调度、失败自动换号、按账号冷却、真实积分余额查询，每条账号同步为一条 CPA 凭据
- 模型命名空间：对外暴露为 `raccoon/<上游模型 id>`（如 `raccoon/sn-glm-5-3-flash`），每条凭据在 CPA 的「支持的模型」里能看到这 10 个模型

## 账号池

一个账号一条 CPA 凭据（`raccoon2api-<alias>.json`），池子有三个来源：

| 来源 | 怎么进来 | 说明 |
| --- | --- | --- |
| 上传 JSON | 控制台「添加账号 → 上传账号 JSON」（粘贴或选文件），或 `POST /v0/management/raccoon2api/accounts/add` | 接受本插件导出的记录、CPA 凭据文件、任意带 `access_token` 的 JSON；导入前向上游验证一次 token |
| 授权码 | 控制台「添加账号 → 授权码登录」，或往 CPA 加一条含 `authorization_code` 的凭据 | 走官方 `/code/authorize` → `login_with_authorization_code` |
| 账号目录 | 往 `accounts_dir` 丢 `*.json` | 文件名即别名，热增删；文件里可写 `priority`；在控制台删除会连源文件一起删 |

插件不会去读桌面端的登录态：账号只能由你上传或登录获得，避免两边抢刷新把桌面端顶掉。

- **优先级**：与 CPA 一致，**数值越大越先用**。配置里的主账号默认 10，目录里的账号默认 5，上传/授权码新增的默认 1。
- **选号**：宿主先按自己的调度挑一个（`req.StorageJSON`），插件优先用它；它不可用或失败时，插件按优先级在自己的池子里挑下一个。
- **冷却**：429/5xx 让该账号冷却 `cooldown_seconds`（有 `Retry-After` 就按它，最长 2h）；401 刷新失败判死并冷却 `cooldown_seconds`；额度不足（`1000007`/402）冷却 `quota_cooldown_seconds` 并**直接失败、不换号**，避免把整池的额度一起烧掉。冷却状态落盘，重启不丢。
- **积分余额**：直接读账号自己的积分接口 `GET /api/web/points/v1/balance`（桌面端显示的就是它），拿到 `available_points / daily_points / reward_points / topup_points / total_chargeable_points`，**查询不消耗积分**；可用积分为 0 记「积分不足」并冷却 `quota_cooldown_seconds`。接口不可用时退回一次 `max_tokens=1` 的最小请求（成功即正常、402/`1000007` 即不足）。每 `quota_probe_seconds` 自动跑一轮，也可在控制台手动触发（多账号并发）。
- **单次请求积分**：每次成功请求结束后读一次 `GET /api/web/points/v2/bills`，按 `biz_id = <X-RACCOON-Session-ID>:<X-RACCOON-Turn-ID>` 把本次 turn 的多行 `points` 相加，写进控制台请求记录的「积分」列；账单写入有延迟时最多重试 3 次。查询免费，不需要额外的消费探测；可用 `points_logging: false` 关闭。
- **自愈**：宿主里的凭据被删会按池子重建；目录里的文件删掉即移出池子。

## 登录（授权链接）

控制台「添加账号 → 授权码登录」点「生成并打开」，浏览器登录完会**自动**回到插件监听的
`http://127.0.0.1:18763/auth/callback`，插件接住一次性 code、换成 token、把账号写进池子，
控制台弹窗自己关掉。全程不需要复制任何东西。

这依赖上游授权页的 loopback 约定（从页面 bundle 逆出来的硬条件，全中才生效）：

| 参数 | 要求 |
| --- | --- |
| `login_source` | 必须是 `desktop` |
| `desktop_callback` | 必须是 `loopback` |
| `redirect` | 必须精确匹配 `http://127.0.0.1:<1024-99999>/auth/callback` |
| `state` | 必须是 64 位小写十六进制 |
| `ide` | 必须为空 |

所以：

- **Docker 部署**必须把端口映射出来，否则浏览器连不上（compose 里已加 `127.0.0.1:18763:18763`）
- 端口被占用时插件只打一条警告，登录退回「复制回调地址手动粘贴」，不会失败
- `login_callback_port: 0` 可以显式关闭自动回调
- 没有这个回调时浏览器只会跳 `office-raccoon://auth/callback?code=…` 深链：装了桌面端就被它抢走，
  没装则页面一片空白、用户拿不到码 —— 这就是自动回调要解决的问题

## 模型命名

模型 id 统一加 `raccoon/` 前缀，避免和 CPA 里其他 provider 的同名模型打架：

```
raccoon/sn-glm-5-3-flash
raccoon/sn-deepseek-v4-1-flash-1017
raccoon/raccoon-8c4485
```

客户端要用带前缀的 id 调用（CPA 按注册的模型名路由）；插件内部会剥掉前缀再请求上游，两种写法在插件层都认。

## 它是怎么工作的

```
客户端 ──/v1/chat/completions──► CPA ──executor.execute(_stream)──► 插件
                                                                    │
                            账号池（上传 / 授权码 / 目录）──► 凭据 ─┤
                            模型目录 model_catalog（缓存 30 分钟） ──┤
                                                                    ▼
                                          POST /api/web/llm/v2/chat/completions
                                          （x-client-* + X-RACCOON-* + OpenAI SDK 默认头）
```

- **凭据**：启动时载入插件数据目录里的账号池并写入 CPA auth 记录；后台每 `import_interval_seconds`
  扫一遍账号目录、对齐宿主里的凭据。
  账号 token 临近过期（`refresh_margin_seconds`）时用各自的 `refresh_token` 刷新（每个账号独立单飞，
  refresh_token 一次性，并发刷新会把登录顶掉），结果写回插件数据目录与 CPA 凭据。
- **模型目录**：`GET /api/web/llm/v2/model_catalog`，按 `categories[].type == "chat"` 取模型，
  上下文窗口与最大输出取自 `params.context_window` / `params.max_tokens`。目录落盘缓存，上游不可达时用缓存。
- **请求整形**：只转发上游认识的字段（上游对未知字段直接 500 `Unexpected keyword argument`），
  `max_completion_tokens` 改名为 `max_tokens`，缺 `max_tokens` 时补 `max_tokens_default`，
  流式强制 `stream_options.include_usage`，`function` 角色改成 `tool`/`user`。

## 伪装细节

> 完整的解包结论（端点、头、字段、响应形状、目录结构、JWT claims）与逐条证据在
> [`docs/upstream-protocol.md`](docs/upstream-protocol.md)。


逐项对齐本机客户端 1.1.3 的真实抓包（Electron 主进程 + `box-agent` ACP 运行时）：

| 头部 | 值 |
| --- | --- |
| `User-Agent` | `AsyncOpenAI/Python 2.8.0`（官方运行时用 openai-python 2.8.0） |
| `X-Stainless-Lang` / `-Package-Version` / `-OS` / `-Arch` / `-Runtime` / `-Runtime-Version` | `python` / `2.8.0` / `Windows` / `x64` / `CPython` / `3.12.13` |
| `X-Stainless-Retry-Count` / `X-Stainless-Read-Timeout` | `0` / 请求超时秒数 |
| `x-client-name` / `x-client-platform` / `x-client-version` | `@raccoon/office-desktop` / `desktop-windows-x64` / `v1.1.3` |
| `x-client-os-version` / `x-client-channel` / `x-client-device-id` | 系统版本 / `official` / 桌面端设备指纹 |
| `X-RACCOON-Session-ID` / `X-RACCOON-Turn-ID` / `X-RACCOON-Title` | 会话内稳定 / 轮次自增 / ASCII 标题 |
| `X-Org-Code` | 仅当 `office_identity` 非空且不是 `personal` |

- **会话 id 跨请求稳定**：优先用客户端自己的 `X-Session-Id` / `X-Conversation-Id` 等提示，
  否则用「system + 首条 user 消息」派生 `ses-<uuidv7>`，按账号隔离。每请求随机 session id 会被上游当成异常流量。
- **轮次**：`X-RACCOON-Turn-ID = <session>-turn-<用户消息条数>`。
- **请求体字段顺序**：按官方运行时的产出顺序（`messages, max_tokens, model, tools, ...`）输出，
  其余字段按字母序补在后面，便于逐次对比。
- 不做 TLS 指纹伪装：该网关无证书 pinning / 请求签名（三个参考实现与本次实测都确认），标准 Go TLS 即可。

客户端升级后需要同步 `client_version` / `client_os_version`（以及必要时 `user_agent`、`python_version`）。

## 构建

```bash
./build.sh              # linux/amd64 + linux/arm64 + windows/amd64
./build.sh linux/amd64  # 只出一个目标
BUILD_PROXY=http://host.docker.internal:7897 ./build.sh   # 需要代理时
```

产物：`dist/plugins/<goos>/<goarch>/raccoon2api.{so,dll}`。

打包发布：

```bash
./package.sh              # 构建 + 出 zip
SKIP_BUILD=1 ./package.sh # 只打包已有产物
```

## 安装

把动态库放到 CPA 的插件根目录（CPA 按 `<root>/<goos>/<goarch>/` 查找）：

```bash
cp dist/plugins/linux/amd64/raccoon2api.so <CPA>/plugins/linux/amd64/raccoon2api.so
```

`config.yaml`：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    raccoon2api:
      enabled: true
      priority: 2
      data_dir: "/CLIProxyAPI/plugin-data/raccoon2api"
      # 设备指纹（只读挂载），用于对齐上游 x-client-device-id
      desktop_device_file: "/raccoon-device/desktop-device-identity.json"
```

`docker-compose.yml` 增加只读挂载（Windows 路径）：

```yaml
    volumes:
      - "C:/Users/<you>/AppData/Roaming/office-raccoon/desktop-device-identity.json:/raccoon-device/desktop-device-identity.json:ro"
      - "./accounts:/raccoon-accounts"
```

> 设备指纹文件在 `%APPDATA%\office-raccoon\` 下，只读挂进去即可；账号不需要挂桌面端目录。
> `./accounts` 要可写，这样在控制台删除账号时能一并删掉源文件。

## 控制台

`GET /v0/resource/plugins/raccoon2api/console`（无需密钥即可打开页面，页面里的数据请求带管理密钥）。
页面提供：概览（账号池 / 额度 / 模型 / 指纹）、请求记录（每次调用一行：模型、思考强度、首字延迟、总耗时、Token 出入、本次积分、缓存命中与命中率、账号、状态；另有一个原始日志子页签）、账号池（导入、导出、查询积分、测试、复位、删除，全部走按钮 + 弹窗）、
模型清单、日志。界面固定浅色，图标全部为内联 SVG。

导出是「导入」的逆操作：单个账号导出成 `<id>.json`，「导出全部」导出整包；两者都能直接拖回导入弹窗，
在另一台 CPA 上还原同样的账号池（含别名与优先级）。

管理密钥只在需要时才出现：页面先用已保存的密钥（没有就匿名）请求一次，被拒才弹出输入框；
如果 CPA 根本没配 `management.secret-key`（管理接口未注册，返回 404），页面会直接提示去改 `config.yaml`，
而不是让你在浏览器里填一个不存在的密钥。密钥只存 localStorage，填错会立刻停止轮询（CPA 对连续失败封 IP 30 分钟）。

管理接口（需 `management.secret-key`，前缀 `/v0/management/raccoon2api`）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/status` | 账号池概况、目录、指纹、统计 |
| GET | `/models` | 目录原始条目与已上架模型 |
| GET | `/accounts` | 账号池明细（优先级、到期、冷却、最近错误） |
| POST | `/accounts/reset` | 清冷却/失效标记，body `{"id":"..."}`，空 body 清全部 |
| POST | `/accounts/import` | 重新扫描账号目录并重新对齐 CPA 凭据 |
| POST | `/accounts/add` | 导入一条账号 JSON（body `{"json":…,"alias","priority","verify"}`，或直接贴凭据对象） |
| POST | `/accounts/delete` | 删除账号并移除对应 CPA 凭据，body `{"id":"…"}` |
| GET | `/accounts/export` | 导出账号凭据：带 `?id=` 导出单个（可直接再导入），不带则导出整包 `{"accounts":[…]}` |
| GET | `/quota` | 积分汇总（`points_available` / `points_chargeable`）与每账号余额 |
| POST | `/quota/check` | 查询积分，body `{"id":"…"}` 或空（全部） |
| POST | `/quota/grant` | 领取每日赠送积分，body `{"id":"…"}` 或空（全部） |
| POST | `/accounts/refresh` | 刷新指定账号的 token |
| POST | `/login/start` | 生成授权链接（返回 `url` 与 `state`） |
| POST | `/login/complete` | 提交授权码，body `{"state","code","alias"}` |
| POST | `/login/poll` | 轮询登录结果（控制台自动回调后自检），body `{"state"}` |
| POST | `/catalog/refresh` | 立即刷新模型目录 |
| POST | `/test` | 用某个账号发一条最小对话（body 可选 `{"id"}`） |
| GET | `/requests?limit=N` | 最近请求记录（模型 / 思考强度 / 首字 / 总耗时 / Token / 积分 / 缓存）+ 汇总 |
| GET | `/logs?limit=N` | 最近日志 |
| GET | `/config` | 生效配置（脱敏） |

## 配置项

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `data_dir` | `plugin-data/raccoon2api` | 插件数据目录（凭据副本、模型缓存、设备指纹） |
| `base_url` | `https://xiaohuanxiong.com/api/web/llm/v2` | LLM 网关基址 |
| `auth_base_url` | `https://xiaohuanxiong.com/api/web/auth/v1` | token 刷新基址 |
| `desktop_device_file` | 空（自动探测） | 桌面端设备指纹文件 |
| `access_token` / `refresh_token` / `office_identity` | 空 | 手工指定主账号凭据（也可以完全留空，只靠控制台上传） |
| `credential_alias` | `default` | CPA 凭据别名（文件名 `raccoon2api-<alias>.json`） |
| `import_interval_seconds` | 30 | 维护循环间隔：扫描账号目录、对齐 CPA 凭据 |
| `refresh_margin_seconds` | 600 | 到期前多久主动刷新 |
| `quota_probe_seconds` | 600 | 积分查询间隔；0 关闭 |
| `daily_grant_enabled` | `true` | 每个账号每天自动领取桌面端的「每日登录赠送积分」（幂等接口） |
| `client_name` / `client_platform` / `client_version` / `client_os_version` / `client_channel` | 见上表 | 客户端指纹 |
| `client_device_id` | 空 | 覆盖设备指纹（空则读桌面端文件） |
| `user_agent` / `sdk_version` / `python_version` | 见上表 | SDK 指纹 |
| `refresh_seconds` | 1800 | 模型目录刷新间隔 |
| `model_allowlist` / `model_denylist` | 空 | 模型白/黑名单 |
| `image_modalities` | `auto` | `auto` 按目录 `vision` 标签，`all` 全部声明，`none` 都不声明 |
| `vision_denylist` | `["sn-glm-5-3"]` | 目录标了 vision 但实测看不见图的模型：`sn-glm-5-3` 会回答「我没看到图片」。反向也不成立——没打标签的 `sn-glm-5-3-flash` 实际能看图，只是不会被声明 |
| `accounts_dir` | 空 | 账号目录：每个 `*.json` 一个账号（文件名即别名） |
| `credential_priority` | 10 | 桌面端账号优先级（越大越先用） |
| `max_attempts` | 3 | 单次请求最多尝试几个账号 |
| `retry_jitter` | true | 重试退避加随机抖动 |
| `cooldown_seconds` | 600 | 429/5xx/401 后该账号冷却时长 |
| `quota_cooldown_seconds` | 3600 | 积分不足后的冷却时长（该次请求直接失败，不换号） |
| `app_name` | 办公小浣熊客户端 | 授权页上报的客户端名 |
| `login_timeout_seconds` | 600 | 授权链接有效期 |
| `request_timeout_seconds` | 1200 | 上游请求超时（同时作为 `X-Stainless-Read-Timeout`） |
| `max_tokens_default` | 80000 | 客户端没给 `max_tokens` 时补的值（0 = 不补） |
| `reasoning_effort` | `low` | 客户端未指定思考强度时下发的默认值（官方客户端默认关深度思考 = `low`）；空则不下发 |
| `debug_dump` | false | 日志里记录出站头与正文（排障用） |

## 排障

- **客户端看到模型但请求 401 `未找到小浣熊登录态`**：账号池是空的。打开控制台「添加账号」上传一份凭据 JSON。
- **`账号 xxx 没有 refresh_token`**：IAM 令牌是 30 天的，本来就不带 refresh_token；到期前重新上传一份新的凭据 JSON。
- **429/5xx 后请求变慢**：该账号在冷却，插件会换下一个；`/accounts/reset` 可以立即解除。
- **导入 JSON 报「上游拒绝了这个 token」**：token 已失效或被吊销；导入前插件会用该 token 拉一次模型目录，
  401/403 直接拒绝，网络不通则只给警告仍然导入。
- **授权码兑换报 200035**：码是一次性的，桌面端已经先消费掉了。直接让桌面端登录，插件会从 `auth.json` 导入。
- **`模型 "x" 不在小浣熊目录里`**：目录来自上游，模型改名后旧的 id 会失效；控制台「模型」页看当前可用 id。
- **偶发 500 `Unexpected keyword argument`**：说明有客户端字段漏过白名单；开 `debug_dump` 看日志里的出站正文。

## 每日赠送积分

桌面端每次打开都会领一次当天的免费积分（默认 300，`biz_type: daily_grant`），当天 23:59:59 过期作废。插件替账号池里的每个账号领：

- 后台每轮维护（`quota_probe_seconds`，默认 10 分钟）检查一次，只处理「今天还没领过」的账号，领完写回账号文件；
- 请求头与桌面端一致（`X-Client-Platform: desktop-windows`、`X-Client-Version: v1.1.3`，非个人主体才带 `X-Org-Code`）；
- 接口按天幂等，重复调用返回 `granted: false`，不会重复发也不会报错；失败（网络、401、业务错误）**不记账**，下一轮重试；
- 领到后立刻回查一次余额，控制台「积分」列显示「今日已领 +300」；
- 控制台「领取每日积分」按钮可手动触发（单账号行内也有「领取今日积分」）。

日期边界按上游的 +08:00 算（`grant_at 00:00`、`expire_at 23:59:59`），与本机时区无关。

## 已知边界

- 只声明 `chat-completions`：Responses / Anthropic 协议由 CPA 宿主翻译。
- 不写回桌面端 `auth.json`：桌面端与插件同时刷新同一个 refresh token 会互相顶掉，插件只读。
- 只调用「每日登录赠送积分」这一个写端点（`POST /api/web/desktop/v1/login/points/grant`，按天幂等）；出图、一次性奖励等其他写操作不碰。
- 上游请求头**顺序**由 Go 的 `net/http` 决定（按名排序），名称与取值与客户端一致。
- 协议细节与证据（哪个文件、哪条实测）见 [`docs/upstream-protocol.md`](docs/upstream-protocol.md)。
