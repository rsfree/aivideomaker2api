#!/usr/bin/env python3
"""aivideomaker 号池批量导入（魔法链接全自动）。

本站没有密码登录（见 docs/evidence/login-endpoint.md），只能走"邮箱收信点链接"。
但这个邮箱可以用 **MSA refresh token** 读（IMAP XOAUTH2），于是整条链路可以无人值守：

    refresh token ──► 访问令牌 ──► 读信 ──► 取 ?token= ──► auth.verifyToken ──► auth_session

输入文件一行一个号，字段用 `----` 分隔（顺序固定）：

    备注----邮箱----密码----clientId----refreshToken

`密码` 可以不填（纯 MSA token 就能读信）。例如：

    renee----a@outlook.com----pw----9e5f94bc-...----M.C512_...

用法：

    python3 tools/magic-import.py accounts.txt \
        --panel https://aivideomaker2api.1task.cn --password <面板口令>

    # 只看结果不入池
    python3 tools/magic-import.py accounts.txt --dry-run

依赖：仅标准库（imaplib / urllib / subprocess 调 curl）。
"""
from __future__ import annotations

import argparse
import email
import imaplib
import json
import re
import ssl
import subprocess
import sys
import time
import urllib.parse
import urllib.request

SITE = "https://aivideomaker.ai"
IMAP_HOST = "outlook.office365.com"
SCOPE = "https://outlook.office.com/IMAP.AccessAsUser.All https://outlook.office.com/SMTP.Send offline_access"


# ----------------------------------------------------------------- 基础 HTTP --
def http(method: str, url: str, *, data: bytes | None = None, headers: dict | None = None,
         timeout: int = 30) -> tuple[int, dict, str]:
    """直连（不走环境里的代理：沙箱代理会吞带 Authorization 的请求）。"""
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read().decode("utf-8", "ignore")
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode("utf-8", "ignore")
    except Exception as e:  # 网络层
        return 0, {}, f"{type(e).__name__}: {e}"


def trpc(proc: str, payload: dict, cookie: str = "", method: str = "POST") -> tuple[int, dict, str]:
    body = json.dumps({"0": {"json": payload}}, separators=(",", ":")).encode()
    h = {"content-type": "application/json", "origin": SITE, "referer": SITE + "/zh"}
    if cookie:
        h["cookie"] = cookie
    return http(method, f"{SITE}/api/{proc}?batch=1", data=body, headers=h)


def void_trpc(proc: str, cookie: str) -> tuple[int, dict, str]:
    void = {"0": {"json": None, "meta": {"values": ["undefined"], "v": 1}}}
    q = urllib.parse.quote(json.dumps(void, separators=(",", ":")), safe="")
    return http("GET", f"{SITE}/api/{proc}?batch=1&input={q}",
                headers={"cookie": cookie, "accept": "*/*"})


def json_of(text: str):
    """取出 tRPC 批量响应里的 json 字段。"""
    try:
        arr = json.loads(text)
        block = arr[0] if isinstance(arr, list) else arr
        return block.get("result", {}).get("data", {}).get("json")
    except Exception:
        return None


# --------------------------------------------------------------------- 邮箱 --
def ms_access_token(client_id: str, refresh_token: str) -> str:
    body = urllib.parse.urlencode({
        "client_id": client_id, "grant_type": "refresh_token",
        "refresh_token": refresh_token, "scope": SCOPE,
    }).encode()
    for host in ("https://login.microsoftonline.com/consumers/oauth2/v2.0/token",
                 "https://login.microsoftonline.com/common/oauth2/v2.0/token"):
        st, _, txt = http("POST", host, data=body,
                          headers={"content-type": "application/x-www-form-urlencoded"})
        if st == 200:
            return json.loads(txt)["access_token"]
    raise RuntimeError("refresh token 换不到访问令牌（可能已失效/被撤销）")


def imap_open(user: str, token: str) -> imaplib.IMAP4_SSL:
    m = imaplib.IMAP4_SSL(IMAP_HOST, 993, ssl_context=ssl.create_default_context(), timeout=30)
    m.authenticate("XOAUTH2",
                   lambda _: f"user={user}\x01auth=Bearer {token}\x01\x01".encode())
    m.select("INBOX", readonly=True)
    return m


def latest_ids(m: imaplib.IMAP4_SSL) -> set[bytes]:
    typ, ids = m.search(None, "ALL")
    return set(ids[0].split()) if ids and ids[0] else set()


def extract_link(m: imaplib.IMAP4_SSL, uid: bytes) -> tuple[str, str]:
    """从一封信里取 (魔法链接, OTP)。"""
    typ, data = m.fetch(uid, "(RFC822)")
    msg = email.message_from_bytes(data[0][1])
    body = ""
    for part in (msg.walk() if msg.is_multipart() else [msg]):
        if part.get_content_type() in ("text/plain", "text/html"):
            body += part.get_payload(decode=True).decode("utf-8", "ignore")
    links = [l.replace("&amp;", "&") for l in re.findall(r'https://aivideomaker\.ai/[^\s"\'<>)]+token=[^\s"\'<>)]+', body)]
    otp = ""
    m2 = re.search(r"One-time password:\s*(\d{4,8})", body)
    if m2:
        otp = m2.group(1)
    return (links[0] if links else ""), otp


# ---------------------------------------------------------------- 会话兑换 --
def session_from_token(token: str) -> str:
    st, hdr, txt = trpc("auth.verifyToken", {"token": token})
    if st != 200:
        raise RuntimeError(f"verifyToken 失败 {st}: {txt[:120]}")
    raw = hdr.get("Set-Cookie") or hdr.get("set-cookie") or ""
    m = re.search(r"auth_session=[^;,\s]+", raw)
    if not m:
        raise RuntimeError("verifyToken 返回 200 但没有 auth_session cookie")
    return m.group(0)


def session_from_otp(identifier: str, otp: str) -> str:
    st, hdr, txt = trpc("auth.verifyOtp", {"type": "LOGIN", "identifier": identifier, "code": otp})
    if st != 200:
        raise RuntimeError(f"verifyOtp 失败 {st}: {txt[:120]}")
    raw = hdr.get("Set-Cookie") or hdr.get("set-cookie") or ""
    m = re.search(r"auth_session=[^;,\s]+", raw)
    if not m:
        raise RuntimeError("verifyOtp 返回 200 但没有 auth_session cookie")
    return m.group(0)


def whoami(cookie: str) -> tuple[str, str]:
    st, _, txt = void_trpc("auth.user", cookie)
    j = json_of(txt) or {}
    return j.get("id", ""), j.get("email", "")


def credits(cookie: str) -> int | None:
    st, _, txt = void_trpc("credits.getCredits", cookie)
    j = json_of(txt) or {}
    v = j.get("totalRemaining")
    return v if isinstance(v, int) else None


# ------------------------------------------------------------------- 主流程 --
def parse_line(line: str):
    parts = [p.strip() for p in line.split("----")]
    if len(parts) < 5:
        raise ValueError("字段不足：需要 备注----邮箱----密码----clientId----refreshToken")
    label, mail, _pwd, cid, rt = parts[0], parts[1], parts[2], parts[3], parts[4]
    return label, mail, cid, rt


def onboard(label: str, mail: str, cid: str, rt: str, *, wait_s: int = 90) -> dict:
    """一个号的完整流程：读信 → 触发 → 等新信 → 换会话 → 校验。"""
    out = {"label": label, "email": mail}
    token = ms_access_token(cid, rt)
    m = imap_open(mail, token)
    try:
        before = latest_ids(m)
        st, _, txt = trpc("auth.loginWithEmail",
                          {"email": mail, "callbackUrl": SITE + "/zh/home"})
        if st != 200:
            raise RuntimeError(f"loginWithEmail 失败 {st}: {txt[:120]}")

        link = otp = ""
        deadline = time.time() + wait_s
        while time.time() < deadline:
            time.sleep(5)
            for uid in sorted(latest_ids(m) - before, key=int):
                link, otp = extract_link(m, uid)
                if link or otp:
                    break
            if link or otp:
                break
        if not link and not otp:
            raise RuntimeError(f"{wait_s}s 内没等到登录邮件")

        if link:
            tok = urllib.parse.parse_qs(urllib.parse.urlparse(link).query).get("token", [""])[0]
            if not tok:
                raise RuntimeError("邮件里的链接没有 token 参数")
            cookie = session_from_token(tok)
            out["via"] = "link"
        else:
            cookie = session_from_otp(mail, otp)
            out["via"] = "otp"
    finally:
        try:
            m.logout()
        except Exception:
            pass

    uid, mail2 = whoami(cookie)
    if not uid:
        raise RuntimeError("cookie 拿到了但 auth.user 读不出身份")
    out.update({"user_id": uid, "email": mail2 or mail, "credits": credits(cookie),
                "cookie": cookie})
    return out


def panel_import(panel: str, password: str, rows: list[dict]) -> dict:
    """登录面板 → 批量导入。返回接口原文。"""
    st, hdr, _ = http("POST", panel.rstrip("/") + "/panel/api/login",
                      data=json.dumps({"password": password}).encode(),
                      headers={"content-type": "application/json"})
    if st != 200:
        raise RuntimeError(f"面板登录失败 {st}")
    cookie = ""
    for k, v in hdr.items():
        if k.lower() == "set-cookie":
            cookie = v.split(";")[0]
    text = "\n".join(f"{r['label']}----{r['cookie']}" for r in rows)
    st, _, txt = http("POST", panel.rstrip("/") + "/panel/api/accounts/import",
                      data=json.dumps({"text": text}).encode(),
                      headers={"content-type": "application/json", "cookie": cookie})
    return json.loads(txt) if txt.strip().startswith("{") else {"raw": txt}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("file", help="一行一个号：备注----邮箱----密码----clientId----refreshToken")
    ap.add_argument("--panel", default="", help="面板地址，如 https://aivideomaker2api.1task.cn")
    ap.add_argument("--password", default="", help="面板口令")
    ap.add_argument("--dry-run", action="store_true", help="只跑流程，不入池")
    ap.add_argument("--wait", type=int, default=90, help="等邮件的最长秒数")
    args = ap.parse_args()

    lines = [l.strip() for l in open(args.file, encoding="utf-8") if l.strip() and not l.startswith("#")]
    print(f"共 {len(lines)} 行\n")

    ok, failed = [], []
    for i, line in enumerate(lines, 1):
        try:
            label, mail, cid, rt = parse_line(line)
        except ValueError as e:
            print(f"[{i}] ✗ 解析失败：{e}")
            failed.append({"line": line, "error": str(e)})
            continue
        print(f"[{i}] {label} <{mail}> …", flush=True)
        try:
            r = onboard(label, mail, cid, rt, wait_s=args.wait)
            # 名字必须**逐号唯一**：卖家的备注常常全一样（本例 5 个都叫"90刀"），
            # 拿备注当账号名会让第 2~5 个全部撞名被拒（实测踩到）。
            # 用邮箱 @ 前那段：天然唯一，且和"哪个号"一一对应。
            r["label"] = mail.split("@")[0].strip().lower() or label
            r["mail"] = mail
            print(f"     ✓ uid={r['user_id'][:12]}… 余额={r['credits']} via={r['via']} name={r['label']}")
            ok.append(r)
        except Exception as e:
            print(f"     ✗ {e}")
            failed.append({"line": line, "error": str(e)})

    print(f"\n成功 {len(ok)} / 失败 {len(failed)}")
    if ok and not args.dry_run:
        if not args.panel or not args.password:
            print("\n（未给 --panel/--password，下面这些行可手动粘进面板「批量导入」）")
            for r in ok:
                print(f"{r['label']}----{r['cookie']}")
        else:
            res = panel_import(args.panel, args.password, ok)
            print("面板导入：", json.dumps(res, ensure_ascii=False)[:600])
    return 0 if not failed else 1


if __name__ == "__main__":
    sys.exit(main())
