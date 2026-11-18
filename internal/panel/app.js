// 面板前端：零依赖、零内联脚本（严格 CSP 只允许同源 app.js）。
//
// 两条纪律：
//   1. 所有来自服务端的字符串一律走 textContent / 属性赋值，**不用 innerHTML 拼数据**
//      —— prompt / 账号名 / 错误原文都可能带尖括号；
//   2. 自动刷新用 setTimeout 串起来（不是 setInterval）：一轮没跑完不会叠出第二轮，
//      标签页不可见时直接跳过（别在后台白烧请求）。
'use strict';

const REFRESH_MS = 5000;
const STATUS_TEXT = {
  queued: ['排队', 'neutral'],
  running: ['生成中', 'warn'],
  succeeded: ['完成', 'ok'],
  failed: ['失败', 'bad'],
  cancelled: ['已取消', 'neutral'],
  expired: ['已超时', 'neutral'],
};

const state = {
  requiresPassword: false,
  authed: true,
  busy: false,
};

// ---------------------------------------------------------------- 小工具 --

function el(tag, props, children) {
  const node = document.createElement(tag);
  if (props) {
    for (const [key, value] of Object.entries(props)) {
      if (key === 'class') node.className = value;
      else if (key === 'text') node.textContent = value;
      else if (key === 'dataset') Object.assign(node.dataset, value);
      else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
      else node.setAttribute(key, value);
    }
  }
  for (const child of children || []) {
    if (child == null) continue;
    node.append(typeof child === 'string' ? document.createTextNode(child) : child);
  }
  return node;
}

function badge(text, tone) {
  return el('span', { class: 'badge ' + (tone || 'neutral'), text });
}

function toast(message) {
  const node = document.getElementById('toast');
  node.textContent = message;
  node.classList.remove('hidden');
  clearTimeout(toast._timer);
  toast._timer = setTimeout(() => node.classList.add('hidden'), 2600);
}

async function api(path, options) {
  const opts = Object.assign({ method: 'GET', headers: {} }, options || {});
  if (opts.body && typeof opts.body !== 'string') {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(opts.body);
  }
  const resp = await fetch('/panel/api/' + path, Object.assign({ credentials: 'same-origin' }, opts));
  let payload = null;
  try { payload = await resp.json(); } catch (_) { /* 非 JSON 响应按空处理 */ }
  if (resp.status === 401) {
    state.authed = false;
    showLogin();
    throw new Error('未授权');
  }
  if (!resp.ok) {
    throw new Error((payload && (payload.error || payload.message)) || ('HTTP ' + resp.status));
  }
  return payload;
}

// ---------------------------------------------------------------- 渲染 --

function renderCards(overview) {
  const cards = [
    ['账号', String(overview.accounts_enabled) + ' / ' + String(overview.accounts_total), '已启用 / 总数'],
    ['提交槽位', String(overview.slots_in_use) + ' / ' + String(overview.slots_capacity), '占用 / 额度'],
    ['任务记录', String(overview.tasks), overview.task_store && overview.task_store.kind === 'jsonl' ? 'JSONL · 跨重启可读' : '内存 · 重启即丢'],
    ['成片出口', overview.media_enabled ? '已开启' : '透传上游', overview.media_enabled ? '对外给本服务地址' : '未配 public_base'],
  ];
  const box = document.getElementById('cards');
  box.replaceChildren(...cards.map(([label, value, sub]) =>
    el('div', { class: 'card' }, [
      el('div', { class: 'label', text: label }),
      el('div', { class: 'value', text: value }),
      el('div', { class: 'sub', text: sub }),
    ])));
}

function accountRow(acc) {
  const enabledBadge = acc.enabled ? badge('启用', 'ok') : badge('停用', 'neutral');
  const credits = acc.credits == null ? '—' : String(acc.credits);
  const plan = [acc.plan || '未知套餐', acc.limit ? '额度 ' + acc.limit : '额度自动'].join(' · ');
  const gate = acc.captcha == null ? '—' : (acc.captcha ? badge('需过闸门', 'warn') : '关闭');
  const running = String(acc.in_use) + ' / ' + String(acc.capacity);
  const probed = acc.last_probe ? new Date(acc.last_probe).toLocaleTimeString() : '—';

  const actions = el('td', { dataset: { label: '操作' } }, [
    el('button', {
      class: 'btn tiny', type: 'button', dataset: { action: 'toggle', name: acc.name, enabled: String(acc.enabled) },
      text: acc.enabled ? '停用' : '启用',
    }),
    document.createTextNode(' '),
    el('button', {
      class: 'btn tiny', type: 'button', dataset: { action: 'probe', name: acc.name }, text: '探测',
    }),
    document.createTextNode(' '),
    el('button', {
      class: 'btn tiny danger', type: 'button', dataset: { action: 'remove', name: acc.name }, text: '删除',
    }),
  ]);

  const cells = [
    el('td', { dataset: { label: '账号' } }, [
      el('div', { class: 'mono', text: acc.name }),
      acc.note ? el('div', { class: 'muted', text: acc.note }) : null,
      el('div', { class: 'muted mono', text: acc.cookie_masked || '' }),
    ]),
    el('td', { dataset: { label: '状态' } }, [
      enabledBadge,
      acc.cooldown_until ? el('div', {}, [badge('冷却至 ' + new Date(acc.cooldown_until).toLocaleTimeString(), 'warn')]) : null,
      acc.last_error ? el('div', { class: 'muted trunc', text: acc.last_error, title: acc.last_error }) : null,
    ]),
    el('td', { class: 'num', dataset: { label: '余额' }, text: credits }),
    el('td', { dataset: { label: '套餐 / 额度' }, text: plan }),
    el('td', { dataset: { label: '闸门' } }, [typeof gate === 'string' ? document.createTextNode(gate) : gate]),
    el('td', { class: 'num', dataset: { label: '在跑' }, text: running }),
    el('td', { class: 'nowrap', dataset: { label: '最近探测' }, text: probed }),
    actions,
  ];
  return el('tr', {}, cells);
}

function renderAccounts(accounts) {
  const body = document.getElementById('accountsBody');
  if (!accounts.length) {
    body.replaceChildren(el('tr', {}, [el('td', { colspan: '8', class: 'empty', text: '还没有账号 —— 点右上「添加账号」，或把 auths/*.json 放进去' })]));
    return;
  }
  body.replaceChildren(...accounts.map(accountRow));
}

function taskRow(task) {
  const [text, tone] = STATUS_TEXT[task.status] || [task.status || '未知', 'neutral'];
  return el('tr', {}, [
    el('td', { class: 'mono', dataset: { label: '任务 id' }, text: task.id }),
    el('td', { dataset: { label: '模型' }, text: task.model || '—' }),
    el('td', { dataset: { label: '状态' } }, [
      badge(text, tone),
      task.error ? el('span', { class: 'muted', text: ' ' + task.error }) : null,
    ]),
    billingCell(task),
    el('td', { class: 'mono', dataset: { label: '账号' }, text: task.account || '—' }),
    el('td', { class: 'mono', dataset: { label: '上游任务' }, text: task.upstream_task || '—' }),
    el('td', { class: 'nowrap', dataset: { label: '创建' }, text: task.created_at }),
    el('td', { class: 'num', dataset: { label: '告警' }, text: String(task.warnings || 0) }),
  ]);
}

// billingCell 画「这条任务有没有花钱」。
//
// 三态必须分开画：true=已扣费（要显眼，它是钱）、false=免费（确认过）、
// null/undefined=还没到终态（**不知道**，不能画成免费）。
function billingCell(task) {
  const td = el('td', { dataset: { label: '扣费' } });
  if (task.paid === true) {
    // ⚠️ **不要**把上游的 `credits` 当金额显示：它与 `paid` 反相（免费任务记 1、
    // 扣费任务记 0）—— 显示成"已扣费 0"会让人以为没花钱。真正的金额只能在台账侧
    // 用余额差核对，这里只给是与否。
    td.append(badge('已扣费', 'warn'));
    td.title = '上游记录 paid=true（该任务确实扣了积分）。上游的 credits 字段与 paid 反相，不能当金额读。';
  } else if (task.paid === false) {
    td.append(badge('免费', 'ok'));
    td.title = '上游记录 paid=false（该任务没有扣费）。';
  } else {
    td.append(el('span', { class: 'muted', text: '—' }));
    td.title = '还没到终态，上游尚未给出 paid —— 这不等于免费。';
  }
  return td;
}

function renderTasks(tasks) {
  const body = document.getElementById('tasksBody');
  if (!tasks.length) {
    body.replaceChildren(el('tr', {}, [el('td', { colspan: '8', class: 'empty', text: '还没有任务' })]));
    return;
  }
  body.replaceChildren(...tasks.map(taskRow));
}

// ---------------------------------------------------------------- 加载 --

async function loadOverview() {
  const overview = await api('overview');
  document.getElementById('version').textContent = 'v' + (overview.version || '?');
  document.getElementById('metaAuth').textContent = '鉴权：' + (overview.auth || '—');
  document.getElementById('metaUptime').textContent = '运行 ' + formatUptime(overview.uptime_sec || 0);
  const dot = document.getElementById('healthDot');
  dot.classList.remove('ok', 'bad');
  dot.classList.add(overview.accounts_enabled > 0 ? 'ok' : 'bad');
  renderCards(overview);
  return overview;
}

function formatUptime(seconds) {
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return d + '天' + h + '小时';
  if (h > 0) return h + '小时' + m + '分';
  return m + '分' + (seconds % 60) + '秒';
}

let timer = null;

async function refresh(silent) {
  if (state.busy || !state.authed) return;
  state.busy = true;
  try {
    const [overview, accounts] = await Promise.all([loadOverview(), api('accounts')]);
    renderAccounts(accounts.accounts || []);
    const limit = document.getElementById('taskLimit').value;
    const tasks = await api('tasks?limit=' + encodeURIComponent(limit));
    renderTasks(tasks.tasks || []);
    document.getElementById('logoutBtn').classList.toggle('hidden', !state.requiresPassword);
    if (!silent) toast('已刷新');
  } catch (err) {
    if (err.message !== '未授权') toast('刷新失败：' + err.message);
  } finally {
    state.busy = false;
    schedule();
  }
}

function schedule() {
  clearTimeout(timer);
  timer = setTimeout(() => {
    if (document.hidden) { schedule(); return; }
    refresh(true);
  }, REFRESH_MS);
}

// ---------------------------------------------------------------- 交互 --

document.getElementById('refreshBtn').addEventListener('click', () => refresh(false));
document.getElementById('addToggle').addEventListener('click', () => {
  document.getElementById('addForm').classList.toggle('hidden');
});
document.getElementById('taskLimit').addEventListener('change', () => refresh(true));

document.getElementById('addForm').addEventListener('submit', async (event) => {
  event.preventDefault();
  const name = document.getElementById('accName').value.trim();
  const cookie = document.getElementById('accCookie').value.trim();
  const note = document.getElementById('accNote').value.trim();
  try {
    await api('accounts', { method: 'POST', body: { name, cookie, note } });
    toast('已保存，正在探测额度');
    document.getElementById('addForm').reset();
    document.getElementById('addForm').classList.add('hidden');
    refresh(true);
  } catch (err) {
    toast('保存失败：' + err.message);
  }
});

document.getElementById('accountsBody').addEventListener('click', async (event) => {
  const btn = event.target.closest('button[data-action]');
  if (!btn) return;
  const name = btn.dataset.name;
  try {
    if (btn.dataset.action === 'toggle') {
      const enabled = btn.dataset.enabled !== 'true';
      await api('accounts/' + encodeURIComponent(name) + '/toggle', { method: 'POST', body: { enabled } });
      toast(enabled ? '已启用 ' + name : '已停用 ' + name);
    } else if (btn.dataset.action === 'probe') {
      await api('accounts/' + encodeURIComponent(name) + '/probe', { method: 'POST' });
      toast('探测已开始：' + name);
    } else if (btn.dataset.action === 'remove') {
      if (!confirm('删除账号 ' + name + '？这会同时删掉 auths/' + name + '.json（正在跑的任务不受影响）。')) return;
      await api('accounts/' + encodeURIComponent(name), { method: 'DELETE' });
      toast('已删除 ' + name);
    }
    refresh(true);
  } catch (err) {
    toast('操作失败：' + err.message);
  }
});

document.getElementById('logoutBtn').addEventListener('click', async () => {
  await api('logout', { method: 'POST' });
  state.authed = false;
  showLogin();
});

// ---------------------------------------------------------------- 登录 --

function showLogin() {
  document.getElementById('loginOverlay').classList.remove('hidden');
}

document.getElementById('importForm').addEventListener('submit', async (event) => {
  event.preventDefault();
  const text = document.getElementById('importText').value;
  const out = document.getElementById('importResult');
  out.textContent = '导入中…';
  try {
    const res = await api('accounts/import', { method: 'POST', body: { text } });
    const lines = (res.results || []).map((r) => (r.ok ? '✓ ' : '✗ ') + (r.name || r.line) +
      (r.error ? ' — ' + r.error : ''));
    out.textContent = `成功 ${res.added} 个\n` + lines.join('\n');
    if (res.added) {
      document.getElementById('importText').value = '';
      refresh(true);
    }
  } catch (err) {
    out.textContent = '导入失败：' + err.message;
  }
});

document.getElementById('loginForm').addEventListener('submit', async (event) => {
  event.preventDefault();
  const password = document.getElementById('loginPassword').value;
  const errorNode = document.getElementById('loginError');
  errorNode.textContent = '';
  try {
    await api('login', { method: 'POST', body: { password } });
    state.authed = true;
    document.getElementById('loginOverlay').classList.add('hidden');
    document.getElementById('loginPassword').value = '';
    refresh(true);
    route();
  } catch (err) {
    errorNode.textContent = err.message;
  }
});

// ---------------------------------------------------------------- 启动 --

(async function init() {
  try {
    const session = await api('session');
    state.requiresPassword = !!session.requires_password;
    state.authed = !!session.authed;
  } catch (_) {
    state.authed = false;
  }
  if (state.requiresPassword && !state.authed) {
    showLogin();
    return;
  }
  refresh(true);
  route();
})();

// ---------------------------------------------------------------- 视图路由 --
//
// 目录结构对齐 workbuddy2api：运营（账号池 / 任务中心）、观测（用量 / 闸门）、
// 配置（官方 API / 配置）。用 hash 路由，刷新后停在原页面。
const VIEWS = ['accounts', 'tasks', 'usage', 'limits', 'official', 'debug', 'config'];

function currentView() {
  const v = (location.hash || '#accounts').replace('#', '');
  return VIEWS.includes(v) ? v : 'accounts';
}

function route() {
  const active = currentView();
  for (const v of VIEWS) {
    const node = document.getElementById('view-' + v);
    if (node) node.classList.toggle('hidden', v !== active);
  }
  document.querySelectorAll('.sidenav a[data-view]').forEach((a) =>
    a.classList.toggle('on', a.dataset.view === active));
  if (active === 'usage') renderUsage();
  if (active === 'limits') renderLimits();
  if (active === 'official') renderOfficial();
  if (active === 'debug') loadDebugHistory();
  if (active === 'config') renderConfig();
}

function fillCards(boxId, items) {
  const box = document.getElementById(boxId);
  if (!box) return;
  box.replaceChildren(...items.map(([label, value, sub]) =>
    el('div', { class: 'card' }, [
      el('div', { class: 'label', text: label }),
      el('div', { class: 'value', text: value }),
      el('div', { class: 'sub', text: sub || '' }),
    ])));
}

async function renderUsage() {
  try {
    const o = await loadOverview();
    fillCards('usageCards', [
      ['账号', String(o.accounts_enabled) + ' / ' + String(o.accounts_total), '已启用 / 总数'],
      ['槽位', String(o.slots_in_use) + ' / ' + String(o.slots_capacity), '占用 / 额度'],
      ['任务记录', String(o.tasks), '本地 JSONL'],
      ['提交线程', String(o.submit_workers || '—'), '并发推进上限'],
    ]);
  } catch (err) {
    fillCards('usageCards', [['读取失败', '—', err.message]]);
  }
}

async function renderLimits() {
  const body = document.getElementById('limitBody');
  if (!body) return;
  try {
    const { accounts = [] } = await api('accounts');
    if (!accounts.length) {
      body.replaceChildren(el('tr', {}, [el('td', { class: 'empty', colspan: '5', text: '还没有账号' })]));
      return;
    }
    body.replaceChildren(...accounts.map((a) => el('tr', {}, [
      el('td', { text: a.name }),
      el('td', {}, [a.captcha == null ? el('span', { text: '—' })
        : (a.captcha ? badge('需过闸门', 'warn') : badge('正常', 'ok'))]),
      el('td', { text: a.enabled ? '可被选中' : '已停用' }),
      el('td', { text: a.last_probe || '—' }),
      el('td', { text: a.last_error || '' }),
    ])));
  } catch (err) {
    body.replaceChildren(el('tr', {}, [el('td', { class: 'empty', colspan: '5', text: '读取失败：' + err.message })]));
  }
}

async function renderOfficial() {
  try {
    const d = await api('official');
    if (!d.enabled) {
      fillCards('officialCards', [['官方 API', '未配置', '设置 AVM_OFFICIAL_API_KEY 即启用']]);
      return;
    }
    if (d.error) {
      fillCards('officialCards', [['官方 API', '读取失败', d.error]]);
      return;
    }
    const a = d.account || {};
    fillCards('officialCards', [
      ['Key', a.key_name || '—', '不含密钥原文'],
      ['余额', String(a.balance == null ? '—' : a.balance), '积分'],
      ['可跑模型', (a.models || []).join(' · ') || '—', '来自 /api/v1/account'],
      ['限额', (a.rpm < 0 ? '不限速' : a.rpm + ' rpm'), a.daily_limit < 0 ? '日限不限' : '日限 ' + a.daily_limit],
    ]);
  } catch (err) {
    fillCards('officialCards', [['官方 API', '读取失败', err.message]]);
  }
}

async function renderConfig() {
  const body = document.getElementById('configBody');
  if (!body) return;
  try {
    const c = await api('config');
    const rows = Object.entries(c).map(([k, v]) =>
      el('tr', {}, [el('td', { text: k }), el('td', { text: String(v) })]));
    body.replaceChildren(...rows);
  } catch (err) {
    body.replaceChildren(el('tr', {}, [el('td', { class: 'empty', colspan: '2', text: '读取失败：' + err.message })]));
  }
}

window.addEventListener('hashchange', route);

// ---------------------------------------------------------------- 调试 --

function debugRequest() {
  const body = {
    model: document.getElementById('dbgModel').value.trim(),
    content: [{ type: 'text', text: document.getElementById('dbgPrompt').value }],
    duration: Number(document.getElementById('dbgDur').value) || 5,
  };
  const res = document.getElementById('dbgRes').value.trim();
  if (res) body.resolution = res;
  const ratio = document.getElementById('dbgRatio').value.trim();
  if (ratio) body.ratio = ratio;
  const img = document.getElementById('dbgImage').value.trim();
  if (img) body.content.push({ type: 'image_url', image_url: { url: img } });
  const ch = document.getElementById('dbgChannel').value;
  if (ch) body.extra_body = { aivideomaker_channel: ch };
  return body;
}

function renderDebug(res) {
  const out = document.getElementById('dbgOut');
  if (!res) { out.textContent = '（空）'; return; }
  const lines = [];
  if (res.ok === false) {
    lines.push('✗ ' + (res.error || '失败'));
    if (res.plan) lines.push('', prettyPlan(res.plan));
  } else if (res.plan) {
    lines.push(prettyPlan(res.plan));
  } else if (res.result) {
    lines.push('✓ 已创建：' + (res.result.id || JSON.stringify(res.result)));
  }
  out.textContent = lines.join('\n');
}

function prettyPlan(plan) {
  const e = plan.effective || {};
  const rows = [
    '通路      : ' + (e.channel || 'web（默认）'),
    '上游模型  : ' + (e.model || e.upstream_model || '—'),
    '是否计费  : ' + (e.billed ? '会扣积分' : '免费档'),
    '分辨率    : ' + (e.resolution || '—'),
    '时长      : ' + (e.duration == null ? '—' : e.duration + 's'),
    '比例      : ' + (e.aspectRatio || '—'),
  ];
  if (plan.procedure) rows.push('procedure : ' + plan.procedure);
  if (e.billing_note) rows.push('计费说明  : ' + e.billing_note);
  const warn = (plan.warnings || []).filter(Boolean);
  if (warn.length) { rows.push('', '提示：'); warn.forEach((w) => rows.push('  · ' + w)); }
  const inc = plan.incompatible || [];
  if (inc.length) { rows.push('', '不支持：'); inc.forEach((w) => rows.push('  · ' + w)); }
  return rows.join('\n');
}

document.addEventListener('click', async (ev) => {
  if (ev.target.id === 'dbgPlan') {
    const out = document.getElementById('dbgOut');
    out.textContent = 'dry-run 中…';
    try {
      renderDebug(await api('debug/plan', { method: 'POST', body: { request: debugRequest() } }));
    } catch (err) { out.textContent = '失败：' + err.message; }
  }
  if (ev.target.id === 'dbgSubmit') {
    const out = document.getElementById('dbgOut');
    const confirm = document.getElementById('dbgConfirm').checked;
    const allowBilled = document.getElementById('dbgAllowBilled').checked;
    if (!confirm) { out.textContent = '先勾选「我知道这是真请求」。'; return; }
    out.textContent = '提交中…';
    try {
      renderDebug(await api('debug/submit', {
        method: 'POST',
        body: { request: debugRequest(), confirm: true, allow_billed: allowBilled },
      }));
    } catch (err) { out.textContent = '失败：' + err.message; }
  }
});

async function loadDebugHistory() {
  const body = document.getElementById('dbgHistBody');
  if (!body) return;
  try {
    const { history = [] } = await api('debug/history');
    if (!history.length) {
      body.replaceChildren(el('tr', {}, [el('td', { class: 'empty', colspan: '6', text: '还没有记录' })]));
      return;
    }
    body.replaceChildren(...history.map((h) => el('tr', {}, [
      el('td', { text: (h.at || '').replace('T', ' ').replace('+08:00', '') }),
      el('td', { text: h.kind === 'submit' ? '真跑' : 'dry-run' }),
      el('td', { text: h.model || '—' }),
      el('td', { text: h.channel || 'web（默认）' }),
      el('td', {}, [h.billed ? badge('计费', 'warn') : badge('免费', 'ok')]),
      el('td', { text: h.ok ? (h.task_id || '✓') : ('✗ ' + (h.error || '').slice(0, 40)) }),
    ])));
  } catch (err) {
    body.replaceChildren(el('tr', {}, [el('td', { class: 'empty', colspan: '6', text: '读取失败：' + err.message })]));
  }
}

document.addEventListener('click', (ev) => {
  if (ev.target.id === 'dbgHistReload') loadDebugHistory();
});
