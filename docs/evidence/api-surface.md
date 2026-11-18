# 站点接口清单（浏览器实抓 + 逐个复验）

抓法：Playwright 起**干净上下文**、注入 `auth_session`，只读访问下列页面并记录所有 `/api/`
请求；随后用 curl 逐个复验响应形状。全程只读，没有下单/没有建任务。

页面：`/zh/app/dashboard`、`/zh/app/settings/account/billing`、
`/zh/app/settings/account/api-keys`、`/zh/ai-video-generator`、`/zh`

## 调用约定（三种，别混）

| 类型 | 方法 | 入参位置 | 形态 |
|---|---|---|---|
| query | **GET** | query `?batch=1&input=…` | `{"0":{"json":<入参>}}`；无参用 `{"0":{"json":null,"meta":{"values":["undefined"],"v":1}}}` |
| mutation | **POST** | body（query 只带 `batch=1`） | `{"0":{"json":<入参>}}` |
| 批量 | GET | query 里 procedure 用**逗号**连写 | `/api/a,b,c?batch=1&input={"0":…,"1":…,"2":…}` |

⚠️ 两个踩过的坑：
- 把 `input=` 也塞进 POST 的 query → 路由 **404**（不是参数错）。POST 只认 body。
- query 用 POST 打 → 统一回 `No "mutation"-procedure on path …`，别误判成"接口不存在"。

## 已确认的 procedure（18 个）

| procedure | 类型 | 页面 | 返回要点 |
|---|---|---|---|
| `auth.user` | query | 全局 | `{id,email,role,name,onboardingComplete,billingCatalog}` |
| `credits.getCredits` | query | dashboard / billing | `{totalAmount, totalRemaining, credits[]}` |
| `credits.getFutureCreditsSchedule` | query | billing | `[{source, effectiveAt, amount}]`（季度订阅下次到账） |
| `billing.plans` | query | billing | 套餐数组：`{id,name,tier,catalog,variants:[{price,currency,interval}]}` |
| `billing.subscription` | query | billing | `{status,planId,tier,price,nextPaymentDate,billingInterval,intervalCount}` |
| `billing.getNextPaymentAmount` | query | billing | 数字（分） |
| `billing.getReceiptLink` | query | billing | 收据链接 |
| `pack.getUserPack` | query | billing | 加油包数组（本例为空） |
| `apiManage.listApiKeys` | query | api-keys | `{keys:[{id,name,key:"ak_…"}]}`（入参要对象） |
| `ai.queryUserPermission` | query | 全局 | `{hasPermission, planName, maxQueueLength, times, subTimes}` |
| `banner.list` | query | 首页 | 横幅 |
| `model.needsCaptcha` | query | 提交前 | 布尔（闸门） |
| `ai.minimaxH3` | **mutation** | 生成器 | 建任务，返回上游 taskId |
| `model.getModel` | query | 生成器 | 任务详情（status/url/aiModel/paid/credits） |
| `auth.loginWithEmail` | **mutation** | 登录弹窗 | 200 + `null`，只发魔法链接 |
| `auth.verifyToken` | **mutation** | 魔法链接 | 200 + **Set-Cookie: auth_session** |
| `auth.verifyOtp` | **mutation** | 登录弹窗 | `{type:"LOGIN"\|"SIGNUP"\|"PASSWORD_RESET", identifier, code}` |
| `auth.loginWithPassword` | **mutation** | 登录弹窗 | 恒 404（见 `login-endpoint.md`） |

### 关键样本

```jsonc
// ai.queryUserPermission —— 4 并发的出处
{"hasPermission":true,"planName":"pro","maxQueueLength":4,"times":0,"subTimes":0,"planId":null}

// billing.subscription —— 这个号的真实套餐
{"status":"ACTIVE","planId":"prod_SymVhJHm7a7qEb","price":8999,"tier":"pro",
 "billingInterval":"month","billingIntervalCount":3,
 "startDate":"2026-09-30T08:32:14.000Z","nextPaymentDate":"2026-12-30T08:32:14.000Z"}

// billing.plans —— 注意 price 单位是**分**
{"id":"prod_VT4p5OKr1QIhuT","name":"Pro Month Pass","tier":"pro",
 "variants":[{"price":5499,"currency":"usd","interval":"year"}]}

// apiManage.listApiKeys
{"keys":[{"id":"cmuz9kzb50007q2fbyt5pf1ac","name":"default","key":"ak_62fe…"}]}
```

**对账**：`price: 8999`（分）= $89.99，`interval: month × 3` = 季度付 ⇒ 就是卖家说的"90 刀"。
额度 4 并发来自 `ai.queryUserPermission.maxQueueLength`，不是我们猜的。

## 抓不到的那两块：不是"没抓到"，是**根本没有接口**

对 `/zh/ai-video-generator` 与 `/zh/generations`（我的作品）做了三次尝试：
普通访问、深链整页导航、点侧边栏跳转 —— **`/api/` 请求数都是 0**。

验证：`curl` 拿 `/zh/generations`（带 cookie）→ 346KB HTML，含 `self.__next_f.push`
的 RSC payload，但**没有** `taskId`/`videoUrl`/`aiModel` 这类任务字段。

结论：这两页的数据是**服务端组件直接查库渲染**的（Next.js App Router），
不走 tRPC。所以：
- 没有"任务列表接口"可复用 —— 想要列表，只能自己记（本服务本来就把任务落在
  `data/tasks.jsonl`，够用）；
- 模型列表同理，是服务端/静态配置，不是接口。

⇒ 抓包到此为止，够用了。本服务需要的三件事都已经在
`ai.minimaxH3`（建）/ `model.getModel`（查）/ `auth.*`（登录）里。

## 官方 REST API（比网页 tRPC 更适合做网关）

规格：`https://aivideomaker.ai/docs/aivideo-openapi.json`（OpenAPI 3.1，`AI Video Maker API 2.1.0`）。
鉴权：**header `key: ak_…`**（`securitySchemes.apiKey`），不是 Bearer。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/api/v1/account` | 余额、key 限额（`dailyCreditLimit` / `generationRequestsPerMinute`）、`supportedModels` |
| POST | `/api/v1/quote/{model}` | 报价（`credits` / `billable` / `affordable`），不下单 |
| POST | `/api/v1/generate/{model}` | 下单，**同步**返回 `taskId` + `creditsCharged` |
| GET | `/api/v1/tasks` | 按 key 列任务 |
| GET | `/api/v1/tasks/{taskId}` | 任务详情（`status` / `output.url` / `creditsCharged`） |
| GET | `/api/v1/tasks/{taskId}/status` | 只要状态 |
| PUT | `/api/v1/tasks/{taskId}/cancel` | 取消 |

实测要点：

```jsonc
// GET /api/v1/account
{"ok":true,"currentBalance":2455,
 "key":{"name":"avm2api","status":"ACTIVE","dailyCreditLimit":-1,"generationRequestsPerMinute":-1},
 "supportedModels":["t2v","i2v","minimax","t2v_v3","i2v_v3","seedance20","wan30","happyhorse"]}

// POST /api/v1/quote/minimax  (720p/5s/turbo)
{"model":"minimax","credits":15,"listPriceUsd":0.15,"usdPerCredit":0.01,
 "currentBalance":2455,"affordable":true,"billable":false}   // ← billable=false = 不扣费

// POST /api/v1/generate/minimax → {"status":"SUBMITTED","taskId":"cmw8…","creditsCharged":15,…}
```

⚠️ 官方 `generate` 只收 **720p/1080p**（`resolution` 枚举里没有 480p），`duration` 5~20。

**为什么值得单独接**：没有 cookie 过期、没有 Turnstile 闸门、有 `idempotencyKey`（重复提交不重复扣费）、
还有网页端根本没有的 `seedance20` / `wan30` / `happyhorse`。

### 🔴 但它**全程收费**，`billable` 字段是坑

`Quote.billable` 在 schema 里是 `"const": false` —— **恒定 false，不是"免费"的意思**。
实测 `t2v 5s`：报价 `{"credits":15,"billable":false}`，下单后余额 **2436 → 2421**（真扣 15）。

| 组合 | 积分 |
|---|---|
| minimax 720p 5s / 10s | 15 / 30 |
| minimax 1080p 5s | 20 |
| minimax 720p 5s base | 20 |
| t2v 5s / 8s | 15 / 24 |
| i2v 5s | 15 |
| t2v_v3 5s / 20s | 20 / 80 |
| seedance20 5s @480 / @720 | 68 / 147 |
| wan30 5s @480 / @720 | 40 / 75 |
| happyhorse 5s @720 | 125 |

各模型的 `duration` 类型还不统一：minimax 是 **number**，t2v/i2v 是字符串枚举 `"5"|"8"`，
t2v_v3/i2v_v3 是 `"5"|"10"|"15"|"20"`，wan30 是 `"5"|"10"|"15"`；
分辨率大小写也不同（minimax 小写 `720p`，wan30/happyhorse 大写 `720P`，seedance20 是数字 `720`）。

## 官方各模型的入参差异（实测，别凭感觉）

| 模型 | `duration` | `resolution` | 必填字段 |
|---|---|---|---|
| minimax | number 5~20 | `720p` / `1080p`（小写） | `content` |
| t2v | **字符串** `"5"`\|`"8"` | 无 | `prompt`, `aspectRatio` |
| i2v | **字符串** `"5"`\|`"8"` | 无 | `image` |
| t2v_v3 | **字符串** `"5"`/`"10"`/`"15"`/`"20"` | 无 | `prompt`, `aspectRatio` |
| i2v_v3 | **字符串** `"5"`/`"10"`/`"15"`/`"20"` | 无 | `image` |
| seedance20 | number | **数字** `480`/`720`/`1080` | `duration`, `ratio`, `resolution` |
| wan30 | **字符串** `"5"`/`"10"`/`"15"` | `480P`/`720P`/`1080P`（大写） | `prompt`, `ratio`, `resolution` |
| happyhorse | number | `720P` / `1080P`（大写） | `prompt`, `resolution` |

同一件事三种写法（`720p` / `720P` / `720`），**是站点自己不一致**，不是我们没试对。

## 网页端的模型 procedure：只有 4 个是真的

用「未鉴权访问返回 401（存在）还是 404（不存在）」逐个探过：

| 槽位名 | procedure | 结果 |
|---|---|---|
| `minimaxH3` | `ai.minimaxH3` | ✅ 存在 |
| `seedance20` | `ai.seedance20` | ✅ 存在 |
| `seedance25` | `ai.seedance25` | ✅ 存在 |
| `HappyHorse` | `ai.happyhorse`（**小写**） | ✅ 存在 |
| `ltx23` | `ai.ltx23` | ✅ 存在 |
| `seedance2` / `wan27` / `kling3` / `kling2_5` / `veo3Fast` / `veo31Fast` / `default` | — | ❌ 404 |

⚠️ 站点 UI 的模型键 ≠ procedure 名：`HappyHorse` 的 procedure 是**全小写** `ai.happyhorse`，
照名字拼会 404。已在 `slots.verifiedProcedures` 收口。
