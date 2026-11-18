#!/usr/bin/env node
/**
 * aivideomaker 登录助手 —— 把一个账号变成 `auth_session` cookie。
 *
 * 为什么需要它：本站**没有密码登录**。
 *   - `auth.loginWithPassword` 对任何凭据都返回 NOT_FOUND（站点自己的登录按钮同样失败）；
 *   - `auth.loginWithEmail` 只发魔法链接（要能读邮箱）；
 *   - 剩下可用的是 Google OAuth 与邮箱魔法链接，两者都必须**在浏览器里点一次**。
 * 所以"加号"的唯一可靠姿势是：开一个干净浏览器 → 人完成登录 → 把 cookie 拿出来。
 *
 * 用法：
 *   node tools/login.mjs --name acc3                  # 开窗口，登录后自动抓 cookie
 *   node tools/login.mjs --name acc3 --url https://aivideomaker.ai/zh/home
 *   node tools/login.mjs --name acc3 --out ./auths    # 同时落盘 auths/acc3.json (0600)
 *
 * 输出两种东西：
 *   1) 一行可直接粘进面板「批量导入」的  `<name>----auth_session=...`
 *   2) 可选落盘文件（--out）
 *
 * 依赖：本机已有的 playwright（复用 avm-proxy 那份），不新装东西。
 */
import { chromium } from '/Users/betterme/PycharmProjects/AI/aivideomaker/avm-proxy/node_modules/playwright/index.mjs';
import fs from 'node:fs';
import path from 'node:path';

const args = process.argv.slice(2);
const opt = (k, d) => {
  const i = args.indexOf('--' + k);
  return i >= 0 && args[i + 1] && !args[i + 1].startsWith('--') ? args[i + 1] : d;
};
const NAME = opt('name', '');
const URL_ = opt('url', 'https://aivideomaker.ai/zh/home');
const OUT = opt('out', '');
const TIMEOUT_MIN = Number(opt('timeout', '10'));

if (!NAME) {
  console.error('用法: node tools/login.mjs --name <账号名> [--url ...] [--out ./auths] [--timeout 10]');
  process.exit(2);
}

// 用**全新上下文**，不碰你日常 Chrome 的 cookie；这样也不会把已有登录态带进去。
const browser = await chromium.launch({
  executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  headless: false,
  args: ['--no-proxy-server'],
});
const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
const page = await ctx.newPage();
await page.goto(URL_, { waitUntil: 'domcontentloaded', timeout: 60000 }).catch(() => {});

console.log(`
──────────────────────────────────────────────
  请在打开的窗口里完成登录（Google 或邮箱魔法链接）
  账号名：${NAME}
  登录成功后脚本会自动读取 auth_session 并校验
  最长等待 ${TIMEOUT_MIN} 分钟
──────────────────────────────────────────────
`);

const deadline = Date.now() + TIMEOUT_MIN * 60_000;
let cookie = '';
while (Date.now() < deadline) {
  const cookies = await ctx.cookies('https://aivideomaker.ai');
  const hit = cookies.find((c) => c.name === 'auth_session' && c.value);
  if (hit) { cookie = hit.value; break; }
  await page.waitForTimeout(2000);
}

if (!cookie) {
  console.error('✗ 超时：没等到 auth_session。窗口保持打开，请确认登录是否成功。');
  await new Promise((r) => setTimeout(r, 30_000));
  await browser.close();
  process.exit(1);
}

// 校验：拿 cookie 打一次 auth.user + credits，确认它真的能用（不只看"有个 cookie"）。
const verify = async () => {
  const res = await fetch('https://aivideomaker.ai/api/auth.user?batch=1&input=' +
    encodeURIComponent(JSON.stringify({ 0: { json: null } })), {
    headers: { cookie: 'auth_session=' + cookie, accept: '*/*' },
  });
  const text = await res.text();
  let uid = '';
  try {
    const j = JSON.parse(text);
    const json = j?.[0]?.result?.data?.json;
    uid = json?.id || json?.userId || '';
  } catch {}
  const c = await fetch('https://aivideomaker.ai/api/credits.getCredits?batch=1&input=' +
    encodeURIComponent(JSON.stringify({ 0: { json: null } })), {
    headers: { cookie: 'auth_session=' + cookie, accept: '*/*' },
  });
  let credits = null, plan = null;
  try {
    const j = await c.json();
    const json = j?.[0]?.result?.data?.json;
    credits = json?.totalRemaining ?? null;
    plan = json?.planName ?? null;
  } catch {}
  return { uid, credits, plan, status: res.status };
};

const v = await verify();
if (!v.uid) {
  console.error(`✗ cookie 拿到了但校验失败（auth.user status=${v.status}）。可能是没登录完，或站点改了接口。`);
  await browser.close();
  process.exit(1);
}

console.log(`✓ 登录成功  uid=${v.uid}  余额=${v.credits ?? '?'}  套餐=${v.plan ?? '?'}`);
console.log('\n把下面这行粘进面板「批量导入」：\n');
console.log(`${NAME}----auth_session=${cookie}\n`);

if (OUT) {
  fs.mkdirSync(OUT, { recursive: true });
  const file = path.join(OUT, `${NAME}.json`);
  fs.writeFileSync(file, JSON.stringify({ name: NAME, cookie: 'auth_session=' + cookie }, null, 2), { mode: 0o600 });
  fs.chmodSync(file, 0o600);
  console.log(`✓ 已写入 ${file}（0600）`);
}

await browser.close();
