# aivideomaker2api

把 [aivideomaker.ai](https://aivideomaker.ai) 的**网页端内部接口**包装成标准视频生成 API，
带**多账号池**与**内置控制台**。Go 单二进制，零第三方依赖。

> 本仓库是 `aivideomaker`（Python FastAPI + Node 适配层）的重构版：**业务逻辑不变**，
> 形态改成与 `workbuddy2api` 同一套（`cmd/` + `internal/`、`config.json`、账号池、go:embed 面板、
> 单二进制部署）。逐条对照见[「与旧版的对应关系」](#与旧版的对应关系)。

---

## 三条必须提前知道的硬约束

| 约束 | 事实 | 本服务的应对 |
|---|---|---|
| **没有取消端点** | 任务一旦提交，只能删本地记录，上游照跑照扣 | 对外只有创建 + 查询，**不提供**删除/取消的错觉入口；记录随保留窗口自然淘汰 |
| **并发按账号** | premium 2 / pro 4，槽位从创建**占到终态**（不是创建返回就释放） | 每账号一把闸门；选号优先挑"此刻真有免费槽位"的号 |
| **没有幂等键** | `POST` 重发一次就可能建出两个任务 ⇒ 重复扣费 | 重试只对"上游**确定没接单**"的失败生效（见下表） |

### 提交重试的许可边界

| 失败 | 重试 | 依据 |
|---|---|---|
| `queue is full` | ✅ 退避 | 账号并发满，隔几秒就有空位 |
| `CAPTCHA_REQUIRED` / 429 | ✅ 换 token | 上游根本没到建任务那一步 |
| 空 taskId（站点静默拒） | ✅ | 站点自己说"不收" |
| 连接类错误（连不上 / 连接超时） | ✅ | 请求一个字节都没发出去 |
| `ReadTimeout` / `WriteTimeout` | ⛔ | 请求可能已被接收 ⇒ 重发＝重复建任务 |
| 内容审核 | ⛔ | 重发还是拒 |
| 参数类 / 其他 4xx-5xx | ⛔ | 不是抖动 |

## 计费红线

| 情形 | 是否计费 | 实测单价（积分/秒） |
|---|---|---|
| `480p` @ 5s / 10s，或 `720p` @ 5s / 8s（`tier=turbo`） | ❌ **免费** | 0 |
| 其他时长（如 `480p/8s`、`720p/10s`） | ⛔ **入口 400，不会发出** | — |
| `1080p`（**任何时长**，仅显式 official 通道） | ✅ 计费 | 4 |
| 任意分辨率 + `tier=base` | ✅ 计费 | **上表对应单价 + 1** |

> 网页免费线**只认这四个组合**：`480p@5s`、`480p@10s`、`720p@5s`、`720p@8s`。
> 其余组合在入口直接 400（报文列出允许值），不再就近吸附 —— 吸附会让调用方拿到一条
> 与自己要求不符的片却毫不知情。

> ⚠️ **质量档（base）是"溢价"不是"固定单价"**：它在快速档单价之上加 1 积分/秒。
> 所以 `720p/base/5s` 是 5 积分（免费窗口内 0+1），而 `720p/base/15s` 是 **60** 积分
> （3+1=4/秒）。按"1 积分/秒"外推会把账单低估 4 倍 —— 实测抓到的就是这个坑。

判据只有一个：任务记录里的 **`paid`**。⚠️ `credits` 与它**反相**（免费任务记 1、扣费任务记 0），
不要拿 `credits` 判断是否花钱，更不要当金额读。

`effective` 里除了 `billed` 还给了 `estimated_credits`（按上表算出的**钱数**）；只有
实测过的组合才给值，未实测的组合是 `null` —— 不外推，因为调用方会拿它做预算。

> 实测（2026-11-08，真实账号，每例前后各读一次余额核对）：
>
> | 请求 | 实扣 |
> |---|---|
> | `480p / 15s / turbo` | **15** |
> | `480p / 5s / base` | **5** |
> | `720p / 11s / turbo` | **33** |
> | `720p / 5s / base` | **5** |
> | `480p / 15s / base` | **30** |
> | `720p / 15s / base` | **60** |
> | `1080p / 5s / turbo` | **20** |
> | `/v1/videos` `_720p` + `seconds=20` | **0**（钉到 8s，仍在免费区） |
> | MiniMax 面 `720p / 15s` | **45** |
>
> 🔴 **1080p 没有免费窗口**：`5s` 就扣 20 积分。早期判据只看时长，会把它报成
> `billed=false` —— 调用方读到"免费"、账单上却有钱。已按分辨率修正并加了回归。
> 🔴 **失败也可能已经扣费**：`720p/11s` 因内容审核失败，仍被扣 **33 积分**。
> 任务到终态时若 `paid=true` 且失败，服务会打 `task failed but was still billed` 警告。
>
> 另外，余额不足时站点对**付费档**同样是**静默拒**（空 taskId，不报错）——
> 本服务会主动查一次余额并写进报文，避免"参数错 vs 没钱"的盲猜。

- 免费档只有四个组合：**480p @ 5s / 10s**、**720p @ 5s / 8s**（`tier=turbo`）。
- 其他时长在**入口直接 400**，报文点名该分辨率允许的取值。**没有任何吸附路径**：
  不就近取整、不因 `extra_body.aivideomaker_prefer_free` 拉回、不因越界钳制 ——
  该开关现在只会在 `warnings` 里留一句"无效果"。
- `1080p` 不在网页线（它无论多短都计费）；要 1080p 必须显式
  `extra_body.aivideomaker_channel="official"`，走按秒计费的官方 REST。
- 🔴 **`/v1/videos` 只跑免费线**：模型名不参与选路（槽位恒为 `minimaxH3`），
  `_1080p` 降级成 `720p`、时长按分辨率钉死（480p→10s / 720p→8s）。
  方舟线 `/tasks` **不受**这条政策影响。
- 调试请求先带 `X-Avm-Dry-Run: 1`，构建完整请求体但不提交、不计费。
- 🔴 **验证码闸门是动态的**：连续提交约 7 条后翻起，之后提交被**静默拒**（空 taskId）。
  本服务会强制复探闸门并报 `429 RateLimitExceeded`；配了铸造服务就自动换 token 重试一次。
  闸门会自己衰减（实测 5~10 分钟）。详见 `docs/web-reverse.md` 的「实跑验证记录」。

## `GET /v1/models`：先看清，再发

两条通路的价差是十倍级，靠试出来的成本太高。这个接口把「有哪些模型 / 走哪条通路 /
要不要钱」一次说清（OpenAI 形状，附加信息在 `aivideomaker` 字段里）：

```jsonc
{"id":"seedance20","object":"model","owned_by":"aivideomaker",
 "aivideomaker":{"channel":"both","free":false,"credits_5s":68,
                 "resolutions":["480","720","1080"],"durations":["5","10"],
                 "note":"实测：网页档 480p/5s 扣 68 积分（与官方同价）"}}
```

| 例 | `channel` | `free` |
|---|---|---|
| `minimaxH3` | both | **true**（480p 5/10s、720p 5/8s + turbo） |
| `seedance20` / `HappyHorse` | both | false |
| `t2v` / `i2v` / `t2v_v3` / `i2v_v3` / `wan30` | official | false |
| `seedance25` / `ltx23` | web | false（网页有 procedure，本服务未适配入参） |

**路由不变**：默认走网页（它才有免费档）；要官方就在 body 里写
`"extra_body":{"aivideomaker_channel":"official"}`，或直接用 `/official/...` 端点。

### 固定组合别名（把参数编码进模型名）

| 模型名 | 分辨率 | 时长 |
|---|---|---|
| `minimax-h3-480p-5s` | 480p | 5s |
| `minimax-h3-480p-10s` | 480p | 10s |
| `minimax-h3-720p-5s` | 720p | 5s |
| `minimax-h3-720p-8s` | 720p | 8s |

给按模型名路由的下游（new-api / apiduo 这类）用：拿到名字就等于拿到合法参数，不必再记矩阵。

- 命中别名 ⇒ 分辨率与时长**钉死**；`body` 里同名参数若给出不同值 → **400**（不静默覆盖）。
- 响应 `model` 回显**调用方原名**，`effective.model_alias` 记录命中的别名 —— 内部槽位改写只发生在映射层。
- 枚举外的组合**不存在**：`minimax-h3-720p-10s` 返回 400 并列出四个合法名字（720p 只有 5s/8s）。
- 别名表与 `DurationAllowed` 同源，且有一条单测守着两者不许漂移。

## 面板的「调试」页

不用 curl 也能看清一次请求会发生什么：

- **Dry-run（免费）**：只走 `translate.Create` + `BillingView`，**不联网、不花钱**，
  直接给出 `通路 / 上游模型 / 是否计费 / 分辨率 / 时长 / 比例 / 提示`。
- **真跑**：复用与公网创建接口**完全相同**的链路（不是另写一份），
  两道闸把关 —— 勾「我知道这是真请求」，计费的单还要勾「允许计费」。

对应的两个接口：`POST /panel/api/debug/plan`（纯翻译）、`POST /panel/api/debug/submit`（真提交）。
提交侧在没有 `allow_billed` 时会**拒绝**并回一段说明，而不是默默扣积分。

**历史**：页面底部是账本（`GET /panel/api/debug/history`，落盘 `data/tasks.jsonl.debug`）。
调试页的 dry-run/真跑**和公网 API 的每一次真实提交**都记进同一份，字段含
模型 / 通路 / 是否计费 / 结果 / 任务号 —— 官方价差十倍，没有累计视图没法做取舍。

## 两条上游通路（**官方 API 全程收费**）

| 通路 | 鉴权 | 闸门 | 计费 | 模型 |
|---|---|---|---|---|
| **网页会话**（tRPC） | cookie `auth_session` | 有（10 秒 3~4 条） | **480p 5/10s 或 720p 5/8s + `tier=turbo` 免费** | minimax |
| **官方 REST**（`/api/v1/*`） | header `key: ak_…` | 无 | **每一种都扣积分** | minimax + `t2v` / `i2v` / `t2v_v3` / `i2v_v3` / `seedance20` / `wan30` / `happyhorse` |

🔴 **默认走网页会话**。官方通路必须**显式**点名：

```jsonc
{"model":"minimaxH3","resolution":"720p","duration":5,
 "extra_body":{"aivideomaker_channel":"official"}}
```

### 端点：自动 / 强制两种写法

方舟协议一套请求体，**端点决定通路**：

| 端点 | 行为 |
|---|---|
| `POST /api/v3/contents/generations/tasks` | **自动**：网页槽位认识的模型走网页；网页不认识的（t2v/wan30/happyhorse…）落官方 |
| `POST /web/api/v3/contents/generations/tasks` | **强制网页**（有免费档；模型必须网页支持） |
| `POST /official/api/v3/contents/generations/tasks` | **强制官方**（无闸门，**全程计费**） |

`GET .../{id}` 同样支持这三个前缀。自动路由的判据写在 `effective.channel` 里，
`X-Avm-Dry-Run: 1` 就能先看清楚会走哪条、要不要钱。

也可以在 body 里点名：`"extra_body": {"aivideomaker_channel": "official"}`。

### 官方模型一览（方舟协议，一个 `model` 字段切换）

| `model` | 必填 | `duration` | `resolution` |
|---|---|---|---|
| `minimax` / `minimaxH3` | `content` | 数字 5~20 | `720p` / `1080p` |
| `t2v` | `content` | `"5"` / `"8"` | — |
| `i2v` | `content` + 首帧图 | `"5"` / `"8"` | — |
| `t2v_v3` | `content` | `"5"`/`"10"`/`"15"`/`"20"` | — |
| `i2v_v3` | `content` + 首帧图 | `"5"`/`"10"`/`"15"`/`"20"` | — |
| `seedance20` | `content` | 数字 | `480`/`720`/`1080`（**数字**） |
| `wan30` | `content` | `"5"`/`"10"`/`"15"` | `480P`/`720P`/`1080P` |
| `happyhorse` | `content` | 数字 | `720P` / `1080P` |

调用方不必记这些差异：**分辨率写 `720p` / `720` / `720P` 都行**，翻译层按模型收口成上游要的写法。

### 官方 API 价目表（实测 `quote`，2026-11-09）

| 组合 | 积分 | ≈USD |
|---|---|---|
| minimax 720p / 5s（turbo） | 15 | $0.15 |
| minimax 720p / 10s | 30 | $0.30 |
| minimax 1080p / 5s | 20 | $0.20 |
| minimax 720p / 5s（base） | 20 | $0.20 |
| t2v 5s / 8s | 15 / 24 | $0.15 / $0.24 |
| i2v 5s | 15 | $0.15 |
| t2v_v3 5s / 20s | 20 / 80 | $0.20 / $0.80 |
| seedance20 5s @480 / @720 | 68 / 147 | $0.68 / $1.47 |
| wan30 5s @480 / @720 | 40 / 75 | $0.40 / $0.75 |
| happyhorse 5s @720 | 125 | $1.25 |

⚠️ `quote.billable` **不能当免费依据**：它在 schema 里是 `const: false`，恒定值。
实测 `t2v 5s` 报价 `billable=false` 但照样扣了 15 分（余额 2436 → 2421）。
判"免费"只有一个标准：**走网页会话的 480p/720p + turbo + ≤10s**。

### 官方接口的规矩（`/docs/aivideo-openapi.json`）

- `POST /api/v1/quote/{model}` 先报价（**下单前先问**，别凭感觉）；
- `POST /api/v1/generate/{model}` **同步**返回 `{taskId, creditsCharged}`；
- `GET /api/v1/tasks/{id}` 五态：`SUBMITTED / PROGRESS / COMPLETED / FAILED / CANCEL`；
- 还有 `PUT /api/v1/tasks/{id}/cancel`、`GET /api/v1/tasks`、`GET /api/v1/account`。

## 号池：怎么加号

🔴 **本站没有可用的密码登录**。实测（`docs/evidence/login-endpoint.md`）：
`auth.loginWithPassword` 对"真号+真密码 / 小写邮箱 / 错密码 / 不存在的邮箱"**一律返回同一个
NOT_FOUND**，而站点**自己的登录按钮**发出同样的请求、也拿到 404；
`loginWithEmail` 只发魔法链接（200，无 Set-Cookie）。

所以只有两条路拿到 `auth_session`：

### A. 有 MSA refresh token（可全自动，推荐）

卖家给的 `邮箱----密码----clientId----refreshToken` 里那只 token 能**读邮箱**
（IMAP XOAUTH2），于是整条链路无人值守：

```
refresh token → 访问令牌 → 收信 → 取 ?token= → auth.verifyToken → auth_session
```

```bash
python3 tools/magic-import.py accounts.txt \
  --panel https://aivideomaker2api.1task.cn --password <面板口令>
# accounts.txt 每行：备注----邮箱----密码----clientId----refreshToken
```

### B. 只有人（浏览器点一次）

```bash
node tools/login.mjs --name acc3     # 开干净浏览器，你登录，它校验并打印导入行
```

### 导入格式（宽进）

面板「批量导入」/ `POST /panel/api/accounts/import`（body `{"text": "..."}`），
**一行一个号，只要行里能找到 `auth_session` 就收**：

```
acc3----auth_session=xxxxx                    # 标准
acc4, auth_session=xxxxx                      # 逗号
acc5	auth_session=xxxxx                       # 制表符
acc6----0123456789abcdef0123456789abcdef       # 只贴裸 session 值
0123456789abcdef0123456789abcdef               # 连名字都不给（自动命名 acc-<尾6位>）
acc7----theme=dark; auth_session=xxxxx; lang=zh  # 整段 cookie 串
curl '…' -H 'cookie: auth_session=xxxxx'       # 整段 curl
{"name":"acc8","cookie":"auth_session=xxxxx"}  # 别的实例导出的 JSON
```

会被**明确拒绝**（并说明原因）的：`邮箱----密码`、`邮箱----密码----clientId----refreshToken`
（后者会指向 `tools/magic-import.py`）。不会入库成"永远 401"的死号。

### 落盘与去重

- 每个号落盘为 `auths/<name>.json`（0600），**重启后原样恢复**（已实测）。
- 同一份 cookie 换个名字导入 → 拒绝。
- **同一个用户的两份会话**（重新登录一次就会产生）→ 探测时按 `auth.user.id` 识别，
  保留余额高的那份，其余自动停用并在面板写明原因（`与 xxx 是同一个用户`）。
- 额度按账号走：`pro` ⇒ 4 并发/号（`capacity` 与 `limit` 都显示 4）。

## 轮询与闸门压力

闸门（Turnstile）数的是**请求速度**，而调用方按秒轮询 —— 重复 GET 会替你把闸门推上去。
本服务因此做了两件事：

- **同 id single-flight**：同一时刻 N 个客户端问同一条任务，只上网一次；
- **自适应 TTL**：非终态视图按任务年龄缓存 `10s / 15s / 20s`，终态永久复用
  （`task_store.cache_ttl` 只是基准值）。

线上实测：5 并发 × 30 秒 = **150 次客户端轮询 → 3 次上游查询**。
读数看 `/metrics` 的 `aivm_upstream_task_gets_total`（需 Bearer）。

### 提交速率上限（实测）

| 项 | 值 |
|---|---|
| 突发阈值 | **3~4 条 / 10 秒**（同一出口聚合，含所有会话） |
| 撞上之后 | 该 IP 上的会话一起进 Turnstile，**约 10 分钟**后自恢复（期间零请求） |
| 正常满载 | 8 槽 × 150s/条 ≈ 3.2 条/分钟 ⇒ **远低于阈值**，不打突发就不会撞 |

**给调用方的硬约束：提交速率 ≤ 3 条 / 10 秒。** 轮询不占这条线（已折叠 + 缓存），
但**不要高频探闸门**：探测本身就是一次上游请求，会把冷却窗口续上。

> 闸门是可以"顶"的：`extra_body.aivideomaker_captcha_token` 带一个新鲜 Turnstile token
> 就能过；但 Cloudflare 会在连续铸造后升级成**交互式挑战**，无头浏览器解不了就断供。
> 详见 `docs/web-reverse.md`。

## 快速开始

```bash
cp config.example.json config.json
export AVM2_API_KEY=sk-local            # 对外 Bearer 闸门（留空 = 不校验，仅本机自用）

# 一个账号一个文件；凭据至少要有 auth_session
mkdir -p auths
cat > auths/acc1.json <<'JSON'
{"name": "acc1", "cookie": "auth_session=<浏览器导出的会话>"}
JSON

go run ./cmd/server -config config.json
# 或：docker compose up -d
```

控制台：<http://127.0.0.1:8808/panel>（口令 = `panel.password`，留空回落到 `api_key`）。

零成本自检（**不创建任何任务**）：

```bash
go run ./cmd/probe -config config.json            # 余额 / 套餐 / 额度 / 闸门
go run ./cmd/probe -config config.json -dry-run   # 打印一次请求会被翻译成什么
```

## 对外接口

| 协议 | base_url | 端点 |
|---|---|---|
| 火山方舟 / Seedance | `http://host:8808/api/v3` | `POST /contents/generations/tasks`，`GET /{id}`，`GET` 列表 |
| OpenAI Videos | `http://host:8808` | `POST /v1/videos`，`GET /v1/videos/{id}` |
| MiniMax 官方 | `http://host:8808` | `POST /v1/video_generation`，`GET /v1/query/video_generation?task_id=`，`GET /v1/files/retrieve?file_id=` |
| 模型清单 | `http://host:8808` | `GET /v1/models` |
| 服务自身 | `http://host:8808` | `GET /healthz`（`?deep=1` 会打上游取余额）、`GET /metrics` |
| 成片出口 | `{public_base}/v/{id}.mp4` | 仅当配置了 `public_base` |

```bash
# 方舟形状（官方 SDK 把 base_url 指过来即可）
curl -X POST localhost:8808/api/v3/contents/generations/tasks \
  -H "Authorization: Bearer $AVM2_API_KEY" -H 'X-Avm-Dry-Run: 1' \
  -H 'content-type: application/json' \
  -d '{"model":"minimaxH3","content":[{"type":"text","text":"a red balloon"}],
       "resolution":"480p","duration":5,"ratio":"16:9"}'

# OpenAI Videos 形状（表单或 JSON）
curl -X POST localhost:8808/v1/videos \
  -H "Authorization: Bearer $AVM2_API_KEY" \
  -F model=doubao-seedance-1-0-pro_480p -F prompt='a red balloon' -F seconds=8
```

### 方舟线要点

- `content[]` 认 `text` / `image_url`（含 `role: first_frame` / `last_frame`）/ `video_url` / `audio_url`。
- **帧输入与参考素材互斥**（上游硬约束），混用前端 400。
- 参考素材按上游标称上限**截断**（图 4 / 视频 1 / 音频 2），但**每次截断都写进 warnings**
  并点名因此悬空的提示词占位符（`@视频n`）。
- 认不出的 `ratio` 一律 **400**（方舟是严格契约）；`5:4` 这类可读的比例串会被**就近吸附**
  并在 warnings 里写明偏差百分比 —— **绝不静默改**。
- 响应体是**官方 schema 白名单**（`id` / `status` / `error` / `content` / `duration` / `ratio` /
  `created_at` / `updated_at`）：官方 SDK 对 unknown field 是报错而非忽略。
  证据字段（`requested` / `effective` / `warnings` / `upstream_model`）只进日志与面板。

### MiniMax 面要点

- **HTTP 恒 200，错误装在 `base_resp` 里**（MiniMax 的风格）：`0` 成功 / `1002` 限流 /
  `1004` 鉴权 / `1008` 余额 / `1026` 内容 / `2013` 参数 / `1000` 内部。
  照方舟那套返回 400 会让调用方把"参数错了"当"网络错误"。
- 字段别名按官方 SDK 与 new-api 渠道实际会发的名字收：`prompt` / `content` / `input`、
  `first_frame_image` / `image`、`last_frame_image`、`subject_reference[].image`、
  `reference_*`、`size` / `aspect_ratio` / `resolution`（`768P` 归 720p）、`prompt_optimizer`。
- `video_width` / `video_height` **实测值优先**（480p/16:9 是 864×480，不是按比例算的 853×480），
  未实测的组合才按比例推算。
- 槽位恒为 `minimaxH3`（web 线唯一已实测的生成 procedure），调用方写的 `model` 名留证据但不选路。
- 流程与其他两面一致：**受理即返**，上游失败在查询时表现为 `status="Fail"` + `error_message`。

### 渠道选项头

```json
X-Channel-Options: {"model_map": {"doubao-seedance-2-5-260628": "seedance25", "*": "minimaxH3"}}
```

判定顺序：**精确键 → `*` 兜底 → 已知槽位透传 → 400**。`*` 兜底会覆盖调用方写出的槽位名
（这是渠道"强制一档"的唯一手段），每次改写都进 warnings。`model` 钉住键**已撤除**，
遗留即报错（静默忽略会让"以为指定了槽位"的运维踩空）。

## 账号池与并发

```
auths/acc1.json ──► Client（独立连接池）+ Queue（容量 = 该号额度）
auths/acc2.json ──► Client + Queue
                       │
POST ──► 选号（优先空闲槽 → 负载率低 → 余额多）──► 占槽 ──► 转存 ──► create ──► 盯到终态才放槽
```

- **额度自动发现**：`queue.max_in_flight = 0` 时读 `ai.queryUserPermission.maxQueueLength`
  （premium 2 / pro 4），读不到回落 2 并告警 —— **不猜成"没限制"**，那会把配额直接撞满。
- **所有号都满时阻塞排队**（`acquire_timeout` 内），而不是直接给客户端一个 429：
  上游的语义本来就是排队，客户端看到的是"还在 queued"。
- **故障冷却**：连续 3 次账号级失败（5xx / 连接类）才冷却，指数退避最长 6h；
  **内容审核不算账号的错**（那是调用方的输入问题），不会把健康号冷却掉。
- **同一账号不要同时给两个实例用**：闸门是进程内状态，合计会超该号额度 ⇒ 提交全 502。
  要扩容就按**账号**分片，不要按请求轮询。

## 部署

```bash
cp config.example.json config.json
# 填 public_base（如 https://avm2api.1task.cn）才会启用成片出口
docker compose up -d
docker compose logs -f
```

- 镜像：`ghcr.io/rsfree/aivideomaker2api`（linux/amd64 + arm64；tag 形如 `0.1.25` / `latest` / `sha-<短SHA>`）
- 面板与 API 同端口；反代样例见 `deploy/nginx.aivideomaker2api.1task.cn.conf`
  （要点：`proxy_buffering off` 给成片流式下载，`proxy_read_timeout` 要大于闸门等待）
- 端口挪动时**只需改 `AVM2_LISTEN` / `-listen`**：健康检查会从监听地址推导探测地址
- `ulimits.nofile` 已放开到 65536。接待能力的真天花板是 fd，不是 worker 数 ——
  goroutine 阻塞等待槽位不占 OS 线程，所以**不需要加 worker/副本**来换接待能力

## 目录结构

```
aivideomaker2api/
├── cmd/
│   ├── server/            入口：配置 → 日志 → 存储 → 池 → 面板 → HTTP
│   └── probe/             零成本自检（只读探测 + dry-run 翻译）
├── internal/
│   ├── config/            config.json + 环境变量覆盖，起服务前校验
│   ├── cookie/            会话凭据归一化（裸 token / Cookie 头 / jar JSON / cURL 包装）
│   ├── sniff/             magic bytes 嗅探（不信扩展名与 Content-Type）
│   ├── ratio/             比例 ↔ 像素：档位表 + resolver 链 + **实测**像素表
│   ├── slots/             模型槽位解析 + X-Channel-Options
│   ├── translate/         方舟请求翻译 / 任务视图 / 计费口径（纯函数）
│   ├── openaivideos/      /v1/videos 翻译（钉死时长、1080p 降级、六字段契约）
│   │                      （MiniMax 形状在 server/minimax.go：base_resp 约定 + 字段别名）
│   ├── upstream/          tRPC 客户端 + 提交闸门 + 素材转存 + Turnstile 铸造
│   ├── pool/              多账号池（选号 / 额度 / 冷却 / 状态持久化）
│   ├── store/             任务表（JSONL 追加 + 紧凑重写，无 CGO）
│   ├── media/             成片流式出口（响应头白名单 + Range 透传）
│   ├── server/            路由、错误信封与脱敏、受理/推进分离
│   └── panel/             内置控制台（go:embed + 严格 CSP）
├── config.example.json
├── Dockerfile / docker-compose.yml / Makefile
├── deploy/                nginx 反代样例
└── docs/web-reverse.md    上游接口事实（tRPC 形状、procedure、上传、状态词表）
```

## 与旧版的对应关系

| 旧（aivideomaker） | 新（本仓库） | 说明 |
|---|---|---|
| `src/ark_compat/ratio.py` | `internal/ratio` | 档位表 / resolver 链 / 实测像素表逐条搬；`Mode` 与留痕语义不变 |
| `src/ark_compat/translate.py` | `internal/translate` | 纯函数；`requested` / `effective` / `warnings` / `unsupported` 结构一致 |
| `src/ark_compat/channel_options.py` | `internal/slots` | 判定顺序、撤除键报错、`*` 兜底覆盖语义一致 |
| `src/ark_compat/openai_videos.py` | `internal/openaivideos` | 钉死时长 / 1080p 降级 / 六字段契约一致 |
| `src/web-adapter` 的 MiniMax 壳 | `internal/server/minimax.go` | `base_resp` 码表 / 字段别名 / `subject_reference` 一致 |
| `src/ark_compat/web_client.py` | `internal/upstream` | tRPC + 幂等安全边界内的重试 + 转存链路 |
| `src/ark_compat/web_queue.py` | `internal/upstream/queue.go` | 每账号一把闸门；槽位占到终态 |
| `src/ark_compat/store.py` | `internal/store` | SQLite → JSONL（任务量每小时几条，不需要 SQL 引擎） |
| `src/ark_compat/media_proxy.py` | `internal/media` | 响应头白名单 / Range / 出口闸门一致 |
| `src/ark_compat/app.py` | `internal/server` | 两线共用一条管线；受理/推进分离；租约 |
| `AVM_AUTH` 三态 | `api_key`（open / key） | **多租户由账号池取代透传**：池里每号独立上下文，不再需要"调用方自带 cookie" |
| 一号一实例 | 一个实例 N 个号 | 上游语义按账号隔离，池化不改变单号的闸门语义 |
| gunicorn 多 worker | 单进程 goroutine | 原版「多 worker 会让并发变成 N×2」的矛盾从设计上消失 |

### 有意的差异

1. **只差 OpenAI Responses 一种壳**。旧仓 Node 适配层（`src/web-adapter`）的
   **MiniMax 官方形状已一并搬过来**（`/v1/video_generation` 三端点，见上）；只把
   **OpenAI Responses 形状**（`/v1/responses` + SSE）留下了 —— 它的调用方是 Responses
   客户端，而视频生成走 Responses 的用法在 `workbuddy2api` 生态里不是主路径，SSE 那一套
   还会多出一条长连接路径与两倍回归面。要加回来是薄薄一层：`internal/openaivideos` 旁边
   再加一个壳，共用同一条提交/查询管线。**这条是刻意取舍，不是遗漏**。
2. **认证只剩 open / key**。旧版 `AVM_AUTH=passthrough` 的用途（每个调用方用自己的账号）
   由账号池承接；池里只有一个号时行为与单账号部署一致。
3. **任务表是 JSONL 而不是 SQLite**。零 CGO、零依赖；`retention_days` 到期做一次紧凑重写。
   需要跨实例查询时再换后端（接口只有六个方法）。
4. **模型槽位里 `1080p` 仍保留**，但 `/v1/videos` 面上会被降级（与旧版一致）。
5. **`duration` / `frames` 的非法类型返回 400**。旧版这里会把 `TypeError` 漏成 500
   （源码里登记为已知缺陷）—— 把"客户端传错参数"报成"服务端故障"是纯损失。

## 开发

```bash
make            # fmt + vet + test + build
make test       # 全部单测（零外发：上游一律 httptest 假站点）
make probe-run  # 零成本自检
```

测试覆盖的关键不变量：比例/时长吸附表、计费红线、素材截断留痕、模型映射判定顺序、
提交重试边界（**审核/读超时不重试**）、闸门容量与额度上调、任务表跨重启、媒体出口脱敏、
面板 CSP 与登录流程。

## 发版

纯自动：**push 到 `main` 即发一个 patch 版本**，无需人工打 tag。

```bash
git push origin main      # ⇒ 读最新 tag、patch+1 ⇒ 多架构镜像 ⇒ tag ⇒ Release
```

流水线（`.github/workflows/release.yml`）顺序是**先推镜像、后打 tag**，构建失败不会留下
「有 tag、没镜像」的半成品：

1. `resolve` 算出版本号与 changelog（`[skip release]` 命中则整条跳过）
2. `test` 跑 `go vet` + `go test`
3. `image` 构建 `linux/amd64,linux/arm64` 并推 GHCR，三个 tag 同 digest：
   `<version>` / `latest` / `sha-<短SHA>`
4. `release` 打 annotated tag、建 GitHub Release

约束与逃生阀：

- 版本号**只递增 patch**；需要 minor/major 时人工打 tag 即可作为新基线
- 提交信息含 `[skip release]`（整条 message 匹配）⇒ 不发版
- 同一个提交重跑（`gh run rerun <id>`）会**沿用已有 tag**，不会二次自增

## 许可

内部使用；上游接口的所有权与使用条款归 aivideomaker.ai。
