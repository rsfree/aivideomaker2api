# 登录接口实测（2026-11-09）

## 结论：本站**没有可用的密码登录**

站点只提供两种登录：**Google OAuth** 与 **邮箱魔法链接**。
"邮箱 + 密码"这条路是死的，凭证本身没问题也一样。

## 证据 1：站点自己的前端也登录失败

在无痕上下文里走完整 UI 流程（首页 → 登录 → 使用邮箱继续 → 填邮箱密码 → 提交），
抓到的请求与响应：

```
POST https://aivideomaker.ai/api/auth.loginWithPassword?batch=1
content-type: application/json
origin: https://aivideomaker.ai

{"0":{"json":{"email":"<email>","password":"<password>"}}}

← HTTP 404
[{"error":{"json":{"message":"NOT_FOUND","code":-32004,
  "data":{"code":"NOT_FOUND","httpStatus":404,"path":"auth.loginWithPassword"}}}}]
```

**关键点**：这是站点前端自己发出的请求，不是我们构造的 —— 它同样 404。
所以不是"我不会调"，是这条路在服务端就没有实现（或已下线）。

## 证据 2：路径本身是活的，404 是"凭据被拒"的通用返回

用**空入参**打同一个路径，会先被 zod 拦住 —— 说明路由存在：

```
{"0":{"json":{}}}
← 400 invalid_type: path=["email"] Required（还有 password）

{"0":{"json":{"email":"nobody@example.com","password":"x"}}}
← 400 too_small: path=["password"] minimum 8
```

再把"原密码 / 小写邮箱 / 故意错的密码"三种都打一遍，**三次返回完全相同的 404**：

| 输入 | 结果 |
|---|---|
| 原邮箱 + 原密码 | 404 NOT_FOUND |
| 小写邮箱 + 原密码 | 404 NOT_FOUND |
| 原邮箱 + `wrongpass123` | 404 NOT_FOUND |

⇒ 404 是"这个号不能密码登录 / 凭据不匹配"的统一出口，不是路由错误。
（404 而非 401 是刻意的：避免枚举账号。）

⚠️ 边界：**不存在的邮箱 + 合规长度密码**同样是 404。所以从外部**无法区分**
"服务端关闭了密码登录"与"这些号都没设密码"。但站点自己的前端也 404，
更可能是前者（或前后端版本不一致）。结论只到"这条路不可用"，不延伸到内部实现。

## 证据 3：邮箱通道存在，但只发魔法链接

```
POST /api/auth.loginWithEmail?batch=1
{"0":{"json":{"email":"...","callbackUrl":"https://aivideomaker.ai/zh/home"}}}
← 200 {"json":null}，**无 Set-Cookie**
```

即：邮件发出去了，会话要等用户点链接才建立。

## 证据 4：这个邮箱读不了信（无法自动化魔法链接）

```
outlook.office365.com:993  → LOGIN failed: 'Basic authentication is disabled.'
imap-mail.outlook.com:993  → LOGIN failed: 'Basic authentication is disabled.'
```

微软已对 Outlook.com 关闭基础认证，只有 OAuth（需应用注册）才能读信。

## 因此"号池"的可行形态

账号不能由服务端用密码自助登录，只能：
1. **浏览器登录一次**（Google 或点邮件里的链接）→ 取 `auth_session` cookie → 灌进池子里；
2. 池子负责之后的：轮换、闸门绕行、余额/额度探测、失效识别。

对应工具：
- `tools/login.mjs --name acc3` —— 开干净浏览器、你登录、它抽 cookie 并**校验**（auth.user + credits），
  最后打印一行可直接粘进面板「批量导入」的 `acc3----auth_session=...`；
- 面板「批量导入」/ `POST /panel/api/accounts/import` —— 一次灌多行。
