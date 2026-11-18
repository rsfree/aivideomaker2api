# 上游接口事实（web 线）

本文件只记**会影响实现**的事实：形状、词表、上限、以及"为什么不能想当然"。
每条都标注了它在代码里的落点，方便改的时候一眼找到该同步哪里。

## 认证

- 唯一凭据：网页会话 cookie `auth_session`（TTL 约 400 天）。
- 服务端把凭据**逐字**当 `Cookie` 头发出去 —— 它是一个请求头，不是一个 token。
  格式不对时**不会报错**，只是被当成未登录，而这与"会话过期"表现完全一致。
  ⇒ `internal/cookie` 收各种实际粘贴形态（裸 token / `Cookie:` 标签 / cURL 包装 / jar JSON /
  `Set-Cookie` 属性），**修过就打日志**，认不出的一律原样下发让 HTTP 层报错。
- 服务端**不校验** `visitorId`：任意 32 位十六进制都行（代码里给了一个固定默认值）。

## 协议形状：tRPC over `/api`

```
GET  /api/{procedure}?batch=1&input={"0":{"json":<input>}}
POST /api/{procedure}?batch=1&input=…      body: {"0":{"json":<input>}}
```

- 路径**没有** `/trpc` 前缀，procedure 直接拼在 `/api` 之后（如 `/api/auth.user`）。
- 响应是 batch 数组；成功取 `[0].result.data.json`，失败是 `[0].error.json`
  （`message` + `data.code` + `data.httpStatus`）。
- 无参数调用要编码成 `null` + `meta.values=["undefined"]`（`auth.user` / `credits.getCredits` /
  `billing.subscription` 都是这种"位置式"形态）。

### 用到 / 实测过的 procedure

| procedure | 用途 | 备注 |
|---|---|---|
| `auth.user` | 会话身份（id / email） | 无参数（undefined 形态） |
| `credits.getCredits` | 余额 `totalRemaining` | 与官方 API 共用同一份积分池 |
| `billing.subscription` | 套餐 / 状态 / 下次扣费 | 返回里**没有套餐名**，套餐名由 `planId` 映射 |
| `ai.queryUserPermission` | `maxQueueLength` = 该号并发额度 | premium 2 / pro 4 |
| `model.needsCaptcha` | Turnstile 闸门是否开启 | **动态**开关（按速率翻转），不是账号属性 |
| `ai.minimaxH3` | 建任务（**唯一已实测**的生成 procedure） | 模型编在路径上，创建体里没有 model 字段 |
| `model.getModel` | 单条任务（首选读法） | 1 次请求、权威、不需要 userId |
| `model.listModel` | 生成记录分页 | 要 userId，且任务得在请求的那一页里 |
| `model-status/token` + `model-status` | SSE 状态流 | 上游未接单时**不发帧也不断开**（会挂死），`/token` 会 429 ⇒ 只作兜底 |
| `uploads.getPresignedUrl` | 换预签名上传地址 | 路由名是**复数** `uploads`；URL 只有 **60 秒**有效期 |
| `download` | 取参考文件链接 | 4xx 通常是"调用方给的链接坏了" |

### 建任务的 body 白名单

站点创建体的键是**白名单**（多余字段没有意义）：

```json
{"content":"…","imageUrl":null,"lastFrameUrl":null,"referenceImageUrls":[],
 "referenceVideoUrl":null,"referenceAudioUrls":[],"aspectRatio":"16:9","duration":5,
 "resolution":"480p","tier":"turbo","promptEnrichment":true,"visitorId":"…","token":null}
```

- `token` 为空时发 `null`：闸门开启但没有 token 时，站点会**返回空串**（不报错、不建任务）
  ⇒ 必须自己先探闸门，把"静默拒"变成显式失败。
- 拿到**空 taskId** 之后要先**强制**真探一次闸门，再换新 token 重试一次；还是空串就不再试。

## 任务记录与状态词表

站点字段自成一套：`taskStatus` / `aiModel` / `url` / `credits` / `paid` / `kelingKeyId` / `createdAt` / `completedAt`。

| 站点 `taskStatus` | 归一化 |
|---|---|
| `submitted` / `pending` / `preparing` / `queueing` / `queued` | `queued` |
| `processing` / `running` / `in_progress` | `running` |
| `succeed` / `success` / `completed` | `succeeded` |
| `failed` / `fail` / `error` | `failed` |
| `cancelled` / `canceled` | `cancelled` |
| `expired` / `timeout` / `timed_out` | `expired` |

两个必须知道的坑：

1. **`succeed` 但没有 `url`**（`taskStatusMsg="not found url"`）要降级成 `failed` ——
   照搬映射会让调用方看到"succeeded 但 video_url 为 null"这种自相矛盾的状态。
2. **`kelingKeyId` 才是真实分辨率**（`"480"` / `"1080"`）：请求里写的未必等于实际产出的档位。
3. `credits` 与 `paid` **不是一回事**（`paid=false` 时也记 `credits=1`）⇒ 判断是否花钱只看 `paid`。

## 比例与时长的真实取值域

比例档位（站点 UI 的 Aspect Ratio 选择器，六档，16:9 默认选中）：

```
16:9  4:3  1:1  3:4  9:16  21:9      （另有 adaptive = 跟随输入图，不参与吸附比较）
```

- 吸附用**相对距离**（对数差），平手取靠前者。用绝对差会让横屏那一端被系统性放大。
- ⚠️ `21:9` 本身可约（gcd 3 ⇒ `7:3`），所以**任何 WxH 都不可能约分出 21:9** —— 只能靠吸附到达。

时长：

| 分辨率 | 合法取值 |
|---|---|
| `480p` | **离散** `5 / 10 / 15 / 20`（站点原话：`480p supports 5s, 10s, 15s, or 20s duration.`） |
| `720p` | 连续 `5–20` |
| `1080p` | 连续 `5–20`（**是否离散未实测**，别顺手改成离散表） |

免费窗口：`turbo` 且 `duration ≤ 10s`。免费组合 **480p / 5s / turbo**；
`/v1/videos` 上钉死为 480p→10s、720p→8s（这两档实测 `paid=false`）。

## 成片尺寸：只填实测值

| 分辨率 | 比例 | 实测像素 |
|---|---|---|
| 480p | 16:9 / 4:3 / 3:4 / 21:9 | 864×480 / 640×480 / 480×640 / 1120×480 |
| 720p | 16:9 / 4:3 / 1:1 / 3:4 / 9:16 | 1248×704 / **928×704** / 704×704 / 704×928 / 704×1248 |
| 1080p | 16:9 / 1:1 / 9:16 | **1904×1080** / 1080×1080 / 1080×1904 |

两个反例（这张表存在的理由）：站点把边长对齐到 **16 的倍数**，
4:3@720p 按比例算得 `960×720`、实测 `928×704`；16:9@1080p 按比例算得 `1920×1080`、实测 `1904×1080`。
**没测过的一律返回空，绝不推算。**

## 参考素材

| 类型 | 上游上限 | 传递字段 |
|---|---|---|
| 图片 | 4 | `referenceImageUrls[]`（首帧之外的多图也合并到这里） |
| 视频 | 1 | `referenceVideoUrl` |
| 音频 | 2 | `referenceAudioUrls[]` |

- 站点**只收自己 CDN 的地址**：外链会按 Content-Type 白名单被拒
  （实测过一个 `.jpg` 外链返回非标准 MIME `image/jpg`，字节其实是 PNG）
  ⇒ 必须走 `uploads.getPresignedUrl` → `PUT` 转存。
- 上传上限按类型分档（站点预签名响应里的 `maxBytes`）：图 10MB / 音频 15MB / 视频 50MB。
- **按 magic bytes 嗅探真实类型**，不信扩展名与 Content-Type。
- 提示词占位符 `@图像n` / `@视频n` / `@音频n` 按 `content[]` 里同类素材的**出现顺序**解析
  （1-based）；截断后超出范围的编号必须被点名 —— "参考错了素材"极难归因。

## Turnstile 闸门

- 是**动态开关**（付费号连续生成约 7 条后翻转），不是账号属性；会随时间衰减。
- 探测结果只能短缓存（默认 5s）：缓存"开"的代价是多铸一个 token（幂等），
  缓存"关"的代价是一个 TTL 窗口内的静默拒（已有换 token 兜底）。
- 铸造服务是**独立的常驻服务**（不在本仓库内），调用约定见 `upstream.Minter`：
  `POST {url}/v1/turnstile/mint?reserve=N` + `X-Minter-Key`。
- 🔴 回环访问**绝不能走代理**：`HTTP_PROXY` 连 `127.0.0.1` 也会代理 ⇒ 铸造客户端的
  Transport 显式 `Proxy: nil`。

## 观测口径

- `GET /tasks/{id}` 会被按秒轮询 ⇒ 服务端做了两层节流：
  - **视图缓存**：非终态 TTL 内复用，终态永久复用（记录已定格）；
  - **访问日志**：轮询路径的**成功**响应不打日志（失败照打 —— 那是"调用方看到了什么"的唯一记录）。
- 上游错误对外**必须脱敏**：原文里有站点 procedure 名、铸造服务主机端口、运维口令、
  runbook 编号。对外只给"类别级事实 + Request ID"，全量原文留在服务日志里。

## 实跑验证记录（2026-11-08，真实账号 / pro / 4 并发 / 初始余额 1 积分）

这一节只记**跑出来的**事实，不引用文档口径。每条都对应上面某一条，改动时用它当回归基线。

### 四个免费任务（并发 4，全部成功）

| 面 | 请求 | 实测成片 | 说明 |
|---|---|---|---|
| 方舟 | 480p / 5s / 16:9 | h264 **864×480** / 5.17s / 247KB | 与实测像素表逐字一致 |
| OpenAI | `_480p` + `seconds=20` | **864×480 / 10.12s** / 1153KB | 20s 被钉成 10s（免费档上界） |
| MiniMax | 480p / 5s | 864×480 / 5.17s | `video_width/height` 取实测值 |
| 方舟 + 首帧 | data URI PNG 640×360 | 864×480 / 5.17s | 走完 presign → PUT → 站点 CDN |

- **余额 1 → 1**：免费档确实不扣费（`tier=turbo` 且 ≤10s）。
- 成片出口（配置 `public_base`）：`video_url` 是本服务地址；`content-disposition` 是我们自己的
  `cgt-….mp4`；`server` / `cf-ray` 等上游头不透传；`Range: bytes=0-65535` → **206 + Content-Range**；
  未知 id → 404。

### 闸门（Turnstile）的真实行为

- 连续提交约 **7 条**后闸门翻起（`model.needsCaptcha=true`），之后提交被**静默拒**：
  HTTP 200、`result.data.json = ""`（空 taskId），**不报错**。
- 空 taskId 时必须**强制复探**闸门（不能信 5s 缓存），否则会把"闸门"误报成"参数错"。
- 有铸造服务时：换新 token 重试一次通常能过（实测有一次成功出片）。
- 🔴 **CF 会升级到交互式挑战**（minter `/healthz` 的 `last_failure.reason = "interactive"`）。
  此时新铸的 token 也会被静默丢弃 —— 本服务把它归为 `CAPTCHA_REJECTED`（**不重试**）：
  继续重试只会再烧 token，而每次铸造都是一次站点侧风控信号。
- 闸门会**自己衰减**（实测约 5~10 分钟后恢复可直接提交）。衰减后无需 token。

### 余额不足也是静默拒

`480p/15s`（付费档）在闸门关闭时提交，仍被空串拒绝 —— 站点对**余额不足**同样不报错。
本服务此时会主动查一次 `credits.getCredits`，把**当前余额**写进报文
（`… (current account balance: 1 credits — a paid-tier request may exceed it)`），
避免运维在"参数错"与"没钱"之间盲猜。

### 并发与冷却

- 4 条并发提交全部被受理（`maxQueueLength=4`），槽位占到终态才释放。
- **闸门失败不计入账号失败**：否则 3 条 429 就会把账号冷却 30s，后续请求变成
  "all accounts are cooling down"，把站点问题伪装成账号问题（实测踩到并修掉）。

## 付费档实测（2026-11-08，余额充足账号 pro / 4 并发 / 起始 2677 积分）

方法：每条任务**前后各探一次余额**，用差值核对扣费；同时比对 `effective.billed` 与
`usage.paid`。全部为真实提交，总消耗 **243 积分**（2677 → 2459，含一条提交超时后仍被扣的 5 积分）。

| 请求 | 状态 | `billed`（我们） | 实扣 | 单价 |
|---|---|---|---|---|
| `/v1/videos` `_720p` + `seconds=20` | completed | — | **0** | 钉到 8s，仍在免费区 |
| `ark 1080p / 5s / turbo` | succeeded | **false** ← 错 | **20** | 4 积分/秒 |
| `ark 720p / 11s / turbo` | **failed（内容审核）** | true | **33** | 3 积分/秒 |
| `ark 480p / 5s / base` | succeeded | true | **5** | 0+1 积分/秒 |
| `ark 720p / 5s / base` | succeeded | true | **5** | 0+1 积分/秒 |
| `ark 480p / 15s / base` | succeeded | true | **30** | 1+1 积分/秒 |
| `ark 720p / 15s / base` | succeeded | true | **60** | 3+1 积分/秒 |
| `ark 480p / 15s / turbo` | succeeded | true | **15** | 1 积分/秒 |
| `minimax 720p / 15s` | Success | — | **45** | 3 积分/秒 |

- **质量档（base）= 快速档单价 + 1 积分/秒**，是**溢价**而不是固定单价。四个实测点：
  `720p/5s→5`（0+1）、`720p/15s→60`（3+1）、`480p/5s→5`（0+1）、`480p/15s→30`（1+1）。
  ⚠️ 中途曾按"base 恒为 1 积分/秒"外推 `720p/15s`，算出 15 —— 实际 **60**，低估 4 倍。
  这正是"用单点外推当价格表"的危害：规则错一次，账单就错一倍。
- 另有两条 720p/base 提交在**提交阶段超时**（`upstream did not respond in time`）：
  `status=failed`、`paid=null`、**扣 0**。它和"生成后被审核拒却照扣"是两种不同情形，
  数据模型里用 `paid=null` 与 `paid=true` 区分 —— 面板上分别显示「—」与「已扣费」。

### 两个必须记住的事实

1. 🔴 **1080p 没有免费窗口**。`1080p / 5s / turbo` 扣 **20 积分**，而原判据
   （只看档位与时长）报的是 `billed=false` —— 调用方读到"免费"、账单上却有钱。
   这是最坏方向的错误。已改为 `Billed(tier, duration, resolution)`，把
   `1080p` 列为永远计费，并补 `resolution ... has no free window` 的 warning。
   成片实测 **1904×1080**（与像素表一致，不是 1920×1080）。
2. 🔴 **失败也可能已经扣费**。`720p / 11s` 因内容审核失败
   （`The input or output was flagged as sensitive`），上游**照扣 33 积分**。
   终态写回时若 `paid=true` 且 `status=failed`，服务打
   `task failed but was still billed` 警告 —— 否则这笔钱只有对账单时才被发现。

### 顺带补齐的两件事

- **`usage` 落盘**：`paid` 只在终态出现一次，不落盘则重启后再也答不出"这条花没花钱"。
  已加 `store.Entry.Usage`，面板「扣费」列按**三态**显示（已扣费 / 免费 / —），
  未到终态显示「—」而不是「免费」。
- **`estimated_credits`**：`effective` 直接给钱数（按上表），未实测的组合给 `null`，
  不外推。实测核对：4 组预测值与扣费**逐条相等**。
- ⚠️ 面板**不显示**上游 `credits` 字段：它与 `paid` 反相（免费记 1、扣费记 0），
  显示成"已扣费 0"会让人以为没花钱。API 里该字段改名为 `upstream_credits` 以阻误读。

## 闸门（Turnstile）是**会话级 + 整体速率**，不是按 IP（2026-11-08 晚受控实验）

最初的判断是"闸门按出口 IP 计"，**已被推翻**。实验设计：同一 cookie、同一条只读探测
（`model.needsCaptcha`），只改出口 IP；以及同一时刻两个 cookie 在同一出口上对照。

```
时刻        出口                          会话    结果
22:35:11   本机 112.86.208.37            主号    captcha=ON
22:35:11   本机 112.86.208.37            备用号  captcha=off
22:35:17   节点 210.121.44.245           主号    captcha=ON
22:35:17   节点 210.121.44.245           备用号  captcha=off
22:34:28   节点→美国代理 64.83.38.158     主号    captcha=ON     ← 换 IP 无效
22:32:06   本机 112.86.208.37            主号    提交成功        ← 换 IP 前同一 cookie 正常
```

- **IP 不是开关**：同一 cookie 在三个出口上状态一致；美国代理（64.83.38.158）与 ai-prod
  同一时刻都是 ON。
- **会话是主键**：同一时刻、同一出口，主号 ON、备用号 off。
- **还有整体速率成分**：25 秒内 8 条（两号各 4）→ 前 4 条受理成功，第 5 条起**两号同时翻 ON**。
  所以不是"每 cookie 独立预算"，同一出口的聚合提交速度也参与判定
  （是否把轮询 GET 计入，未单独验证）。

**可用的手段只有两个**：多会话轮换（各号窗口独立）+ 控速（5~15 分钟自衰减）。
换出口 IP / 挂代理**无效**。铸造服务只能救"token 过期"，救不了"整体速率超了"。

对应实现：`internal/pool` 的闸门感知选号（撞 `CAPTCHA_*` 后开 2 分钟绕行窗口，
成功或探测到关闭立刻解除；全部被挡时仍照常出手）。实测：接入第二个会话后 4 条并发
被分摊到两个号、全部出片。

## 轮询折叠 + 自适应缓存（0.1.3）：闸门压力的止血

闸门按**请求速度**翻，而调用方按秒轮询 —— 重复 GET 就是白送给闸门的计数。两条改动：

1. **同 id single-flight**：同一时刻 N 个客户端问同一条任务，只上网一次，其余等结果
   （`Server.coalesce`）；缓存写回前到达的请求不再各自打一次上游。
2. **自适应 TTL**（`pollTTL`）：非终态按任务年龄 10s / 15s / 20s，终态永久。

线上实测（5 并发 × 30 秒，同一条任务）：

| 指标 | 值 |
|---|---|
| 客户端轮询请求 | 150 |
| 实际打到上游 | **3** |
| 增量出口 | `/metrics` 的 `aivm_upstream_task_gets_total` |

单测锁住不变量：`TestConcurrentPollsHitUpstreamOnce`（8 并发 → 1 次上游，且紧跟一次缓存命中）。

## 闸门能不能"绕"？——能一阵，绕不了长期

用本机铸造服务 + 真实提交实测（2026-11-08 23:00）：

```
22:54:15  submitted                        ← 还不需要 token
22:56:32  turnstile minted (source=pool)   ← 闸门翻起
22:56:35  submitted                         ← 带 token 过了
22:56:44  turnstile minted (source=live)    ← 池空，现铸
22:56:54  submitted
```

- **能绕**：`extra_body.aivideomaker_captcha_token` + 铸造服务，每条约一个 token。
- **有天花板**：连续铸十几次后 Cloudflare 升级成**交互式挑战**，铸造侧日志变成
  `失败[interactive]` / `ERR 300010`，无头浏览器解不了 ⇒ token 断供 ⇒ 绕不过去。

⇒ 铸造是"救偶发"的手段；**长期高频只能降速 + 多会话**。

⚠️ 反面教训：给一个**空/伪造**的 token 反而更糟 —— 上游会回
"rejected even though the captcha gate is closed"，比直说 429 更难排查。
别在 token 可能为空时把它塞进请求。

## 闸门限速的**具体数字**（2026-11-08 23:00-23:35 实测）

两台会话（`prod-1` / `prod-2`，同出口 210.121.44.245）上的真实提交，全部免费档（0 积分）。

### 阈值：**同一出口 10 秒内 3~4 条**

| 轮次 | 提交节奏 | 结果 |
|---|---|---|
| 22:38 | 8 条 / 2s 间隔 | 前 **4** 条受理（03.96→13.88s，跨 9.9s），第 5 条（+0.8s）起全部 429 |
| 23:29 | 6 条 / 2s 间隔 | 前 **3** 条受理（t+1.2→8.1s），第 4 条（t+11.4s）起全部 429 |

⇒ 触发条件是**突发**，不是总量。保守上限取 **3 条 / 10 秒**。

### 触发范围：同一出口的**聚合**速度，两个会话一起翻

两轮突发里 `prod-1` 与 `prod-2` 都是**同一秒内**一起变成 `captcha=true`
（`gate_after_burst: {'prod-1': True, 'prod-2': True}`）。
所以计数器看的是"这个 IP 上的提交速度"，不是每个 cookie 各算一份。

但**状态与恢复是会话级的**：23:12 抓到过 `prod-1=True / prod-2=False` 的中间态。
合起来的模型是：**触发按出口聚合速度，状态记在每个会话上、各自计时恢复**。

### 冷却：**10.3 分钟**（完全静默、零请求）

```
[起始] prod-1=True prod-2=False
静默 5 分钟 → 探一次：prod-1 仍 True
再静默 5 分钟 → 探一次：两个都 False（累计 621s）
```

⚠️ 之前量到"17 分钟还没恢复"是**测量方法错了**：那一版每 22 秒探一次，
而 `model.needsCaptcha` 本身就是一次上游请求 —— **探测会把窗口续上**。
这一版全程零请求，只等固定时长再探一次，才拿到干净的 10.3 分钟。

**第二次复现（带业务轮询）**：23:29:29 撞闸门 → 23:39:50 探到恢复 = **10.4 分钟**。
期间有 3 条任务在跑、服务每 10s 查一次（已折叠+缓存，约 18 次/分钟）。

⇒ **轮询不占这条线，只有提交算。** 这同时说明 0.1.3 的折叠/缓存是"免费"的：
它省下的请求不会换来更长的冷却，但也不会加重闸门。

### 实际吞吐上限（按槽位算，比闸门更松）

- 槽位：2 个号 × 4 = **8 并发**；
- 5s 任务端到端约 150s ⇒ 理论上限 ≈ `8 / 150s` ≈ **3.2 条/分钟**；
- 3.2 条/分钟 = 0.53 条/10s，**远低于 3 条/10s 的突发阈值**。

⇒ **只要不刻意打突发，正常满载不会撞闸门**。撞闸门几乎都是"一次灌 5 条以上"造成的。

### 因此服务侧的控制点

| 控制点 | 值 | 在哪 |
|---|---|---|
| 单号并发 | 4（按账号额度自动） | `queue.max_in_flight=0` |
| 撞闸门后绕行 | 2 分钟 | `captchaCooldown`（`pool`） |
| 上游查询折叠 | 同 id 并发 → 1 次 | `Server.coalesce` |
| 上游查询缓存 | 10/15/20s（按任务年龄） | `pollTTL` |

要更保险，把调用方的提交速率限制在 **3 条/10 秒** 以内即可 —— 这是实测出来的线。
