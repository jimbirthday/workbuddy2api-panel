'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_THEME = 'wb2api.theme';
/* 管理密钥只放内存：localStorage 会跨会话、跨标签页常驻，任何一次 XSS 或共用
   浏览器都能直接拿到完整管理权限。页面刷新后重新输入一次，是刻意换来的安全边界。 */
let memKey = '';
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
let logPin = true, loginState = null, loginTimer = null;
let refTimer = null;
/* 视图级筛选状态（模块级声明放在文件顶部，避免顶层 go() 早于声明执行时踩 TDZ）。 */
let mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
let mdAll = [], mdProbes = {}, mdProbeOf = () => undefined;
let reqFilter = { q: '', outcome: '', key: '' };
let reqEntries = [];
let reqPage = 1, reqPageSize = 50;
// 过滤结果缓存挂在函数属性上：区域切片测试只求值 renderRequestTable，
// 模块级 let 不会随之进入测试上下文。翻页/重绘直接复用上一轮结果。
let lastLogMetrics = null, lastLogRows = { entries: [] }, lastLogErrors = {};
let usDim = 'account', usCreditDim = 'account', usSort = 'total';
let usDimPage = 1, usDimPageSize = 100;
let usCreditKind = 'spend';
let creditHistData = null;
let creditSpendStatsHtml = '', creditSpendNote = '';
let usageData = null;
let reqRangeState = null; // 请求记录的时间范围（用量页的见 trangeState）
// 并发/重建守卫（易用性修复）：
//  - usageSeq/tasksSeq：慢响应回来时若已有更新的请求发出，丢弃旧渲染（防"越刷越旧"）。
//  - busyKeys：账号行动作在途键（"动作|uid"）。5 秒轮询会用 renderAccounts 重建整张表，
//    把按钮的 disabled 清掉，所以「进行中」必须记在模块级，渲染时据它恢复。
//  - accAll/accFilter：账号表最近一次数据 + 搜索/排序条件（前端筛选，零额外请求）。
let usageSeq = 0;
let tasksSeq = 0;
let overviewSeq = 0;
let overviewInFlight = false;
// overviewETag/overviewFetchedAt：/overview 条件请求。快照未变时服务端回 304，
// 省掉 470 账号下每 5 秒约 285 KB 的整表传输；运行时长据此本地递增。
let overviewETag = '', overviewFetchedAt = 0, lastUptimeSec = 0;
// overviewPending：单飞期间有新的刷新请求（动作完成、手动刷新）先记下，当前
// 请求结束后立即补跑一次——否则动作成功后正好撞上轮询在途，要白等 5 秒才更新。
let overviewPending = false;
let logsReq = false;
// logsPending：轮询在途时用户点了刷新或切换日志频道，不能静默丢掉操作。
// 当前请求结束后立即补跑一次；轮询自身重入不设 pending，避免慢盘上无限排队。
let logsPending = false;
let logSig = '';
let reqKeySig = '';
let queueReattachReq = false;
let actSeq = 0, actCtrl = null;
let actLimit = 200, actRows = [];
let usageCtrl = null;
let perfSeq = 0, perfCtrl = null;
let creditHistSeq = 0, creditCtrl = null;
let creditGrantSeq = 0, creditGrantCtrl = null;

// remDim 是「剩余额度」的维度状态（account/model）。此前只在渲染函数里读取、
// 切换回调里赋值，却没有任何声明：脚本以严格模式运行时首次 renderRemaining
// 就抛 ReferenceError: remDim is not defined，直接中断整个 loadRemaining。
let remReq = false, remSeq = 0, remCtrl = null, remData = null, remDim = 'account';
let cxSeq = 0, cxData = null;
let scanReattachReq = false;
const busyKeys = new Set();
let accAll = [];
let accFilter = { q: '', sort: 'default', state: '', realm: '' };
// 账号勾选（uid 集合）与分页状态。勾选跨分页保留；筛选条件变化时仍保留——
// 用户可能先搜一批再改条件继续选，批量动作只作用于明确勾选的 UID。
let accSelected = new Set();
let accPage = 1;
let accPageSize = 100;
// 虚拟表格状态：当前页行、真实行高、上次渲染窗口。
// 只影响 DOM 中保留多少行，不改变筛选、排序和分页语义。
let accVirtualRows = [];
let accVirtualRowH = 44;
let accVirtualStart = 0;
let accVirtualEnd = 0;
let accVirtualSignature = '';
let accVirtualBound = false;
try {
  const saved = Number(localStorage.getItem('wb-acc-page-size'));
  if ([0, 50, 100, 200].includes(saved)) accPageSize = saved;
} catch (_) { /* 隐私模式 */ }

// syncAccSelectionUI 同步勾选/分页控件。只改属性与短文本，不重建表格，
// 因而不会在轮询或勾选时丢焦点。
function syncAccSelectionUI(shown, pageRows) {
  const n = accSelected.size;
  const note = $('accSelNote');
  if (note) {
    note.hidden = n === 0;
    note.textContent = n ? '已选 ' + n + ' 个' : '';
    note.className = n ? 'note src-off' : 'note';
  }
  for (const id of ['btnSelEnable', 'btnSelPause', 'btnSelDisable']) {
    const b = $(id);
    if (b) b.disabled = n === 0;
  }
  const clear = $('btnAccClearSelect');
  if (clear) clear.hidden = n === 0;
  const pickAll = $('accPickAll');
  if (pickAll) {
    const all = pageRows.length > 0 && pageRows.every(s => accSelected.has(s.uid));
    pickAll.checked = all;
    pickAll.indeterminate = !all && pageRows.some(s => accSelected.has(s.uid));
  }
  const pageNote = $('accPageNote');
  if (pageNote) {
    const size = accPageSize > 0 ? accPageSize : Math.max(1, shown.length);
    const pages = Math.max(1, Math.ceil(shown.length / size));
    pageNote.textContent = shown.length ? (accPage + ' / ' + pages) : '';
  }
  const prev = $('btnAccPrev'), next = $('btnAccNext');
  if (prev) prev.disabled = accPage <= 1;
  if (next) next.disabled = accPage >= Math.max(1, Math.ceil(shown.length / (accPageSize > 0 ? accPageSize : Math.max(1, shown.length))));
  const sizeSel = $('accPageSize');
  if (sizeSel) sizeSel.value = String(accPageSize);
}

const $ = id => document.getElementById(id);

/* 顶栏与页内导航都吸顶：secnav 的 top 必须等于顶栏实际高度，否则会滑到
   顶栏底下（z-index 更低，看起来直接消失）。窗口变化时重新测量。 */
function syncTopbarHeight() {
  const bar = $('topbar');
  const root = document && document.documentElement;
  if (!bar || !root || !root.style || !root.style.setProperty) return;
  try {
    const h = Number(bar.offsetHeight || 0);
    if (h > 0 && isFinite(h)) root.style.setProperty('--topbar-h', h + 'px');
  } catch (_) { /* inert/测试 DOM：保持 CSS 默认偏移 */ }
}
syncTopbarHeight();
// 脚本在 <body> 末尾执行时字体/导航可能还没完成布局，offsetHeight 会偏小；
// 首帧和 load 后再测一次，避免移动端两行顶栏下的 secnav 贴错位置。
if (typeof requestAnimationFrame === 'function') requestAnimationFrame(syncTopbarHeight);
addEventListener('load', syncTopbarHeight, { passive: true });
addEventListener('resize', syncTopbarHeight, { passive: true });

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* Hub 同款三态主题：浅色 / 深色 / 跟随系统。旧实现是单按钮在浅深之间翻转，
   用户无法回到“跟随系统”，与 Hub 的行为不一致。这里保留 localStorage 键，
   只把选择改成下拉菜单，并同步 aria-expanded 与当前项。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function setThemeMenu(open) {
  const menu = $('themeDropdown');
  const btn = $('btnTheme');
  if (menu) menu.classList.toggle('open', !!open);
  if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  const icon = $('icoTheme');
  if (icon) icon.innerHTML = theme === 'auto'
    ? '<rect x="2" y="3" width="12" height="9" rx="1.5"/><path d="M6 14h4M8 12v2"/>'
    : eff === 'light'
      ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
      : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  document.querySelectorAll('#themeDropdown [data-theme-opt]').forEach(el => {
    el.classList.toggle('active', el.dataset.themeOpt === theme);
    el.setAttribute('aria-checked', el.dataset.themeOpt === theme ? 'true' : 'false');
  });
}
addEventListener('change', applyTheme);
const themeBtn = $('btnTheme');
if (themeBtn) themeBtn.onclick = ev => {
  if (ev) { ev.preventDefault(); ev.stopPropagation(); }
  const menu = $('themeDropdown');
  setThemeMenu(!!menu && !menu.classList.contains('open'));
};
const themeDropdown = $('themeDropdown');
if (themeDropdown) themeDropdown.addEventListener('click', ev => {
  const b = ev.target && ev.target.closest ? ev.target.closest('[data-theme-opt]') : null;
  if (!b) return;
  theme = b.dataset.themeOpt;
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
  setThemeMenu(false);
});
addEventListener('click', ev => {
  const wrap = $('themeDropdownWrap');
  if (wrap && ev.target && !wrap.contains(ev.target)) setThemeMenu(false);
});
addEventListener('keydown', ev => { if (ev.key === 'Escape') setThemeMenu(false); });
applyTheme();

/* ── 请求 ─────────────────────────────────────────────────────────── */
function authHeaders(extra) {
  const h = Object.assign({}, extra || {});
  const k = memKey;
  if (k) h['Authorization'] = 'Bearer ' + k;
  return h;
}
async function api(path, opts = {}) {
  const h = authHeaders(opts.headers);
  if (opts.body) h['Content-Type'] = 'application/json';
  // 状态轮询必须有自己的截止时间：后端偶发挂住时，fetch 可能长时间不返回，
  // 扫描/队列按钮会永久停在「扫描中…」。调用方显式 abort 的请求沿用原 signal，
  // 其余请求在 15s 后中止并给出可重试的错误文案。
  let signal = opts.signal;
  let timer = null;
  let timed = false;
  if (!signal && typeof AbortController === 'function') {
    const ctl = new AbortController();
    signal = ctl.signal;
    const limit = Number(opts.timeoutMs) > 0 ? Number(opts.timeoutMs) : 15000;
    timer = setTimeout(() => {
      timed = true;
      try { ctl.abort(); } catch (e) { /* ignore */ }
    }, limit);
  }
  const o = Object.assign({}, opts, { headers: h });
  if (signal) o.signal = signal;
  let r;
  try {
    r = await fetch('/panel/api/' + path, o);
  } catch (e) {
    if (timed) throw new Error('请求超时（' + Math.round((Number(opts.timeoutMs) > 0 ? Number(opts.timeoutMs) : 15000) / 1000) + ' 秒），请稍后重试');
    throw e;
  } finally {
    if (timer) clearTimeout(timer);
  }
  if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
  const d = await r.json().catch(() => ({}));
    if (!r.ok) {
      // 调用方用 status 区分 501/403 等可操作状态；只带 message 会让专门的
      // 空态文案永远不可达，用户只看到技术错误。
      const err = new Error(d.error || ('HTTP ' + r.status));
      err.status = r.status;
      throw err;
    }
  return d;
}
// isAbortError 判断一次请求是否被「新一轮请求」主动取消。取消不是失败：
// 它只代表结果已过期，调用方必须静默返回，不能把上一轮刚渲染好的表清空，
// 更不能把它当网络错误弹给用户（快速切时间范围/连点刷新时必现）。
function isAbortError(e) {
  return !!e && (e.name === 'AbortError' || e.code === 20 || e.code === 'ABORT_ERR');
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  const host = $('toasts');
  if (host) host.appendChild(el);
  else if (typeof document !== 'undefined' && document.body) document.body.appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
// 弹窗焦点管理：打开时记住触发元素，关闭后归还；打开后把焦点送到首个输入/按钮。
// 键盘用户不再需要在整页 Tab 一圈才能找到弹窗内容。按 veil id 分开记录，
// 避免鉴权弹窗与业务弹窗交替打开时把焦点还错地方。
const modalFocusReturn = new Map();
function showModal(id, focusSel) {
  const veil = $(id);
  if (!veil) return;
  // 同一个弹窗已打开时不再覆盖归还目标：重复调用（如轮询刷新）会让
  // activeElement 变成弹窗内部元素，关闭后焦点就回不到触发按钮。
  if (!veil.classList.contains('on') && !modalFocusReturn.has(id)) {
    modalFocusReturn.set(id, document.activeElement);
  }
  veil.classList.add('on');
  if (focusSel) setTimeout(() => {
    const el = $(focusSel);
    if (el && typeof el.focus === 'function' && el.offsetParent !== null) el.focus();
  }, 60);
}
function hideModal(id) {
  const veil = $(id);
  if (!veil) return;
  veil.classList.remove('on');
  const back = modalFocusReturn.get(id);
  modalFocusReturn.delete(id);
  if (back && typeof back.focus === 'function' && (!document.contains || document.contains(back))) back.focus();
}
/* 弹窗内 Tab 循环。CSP 禁止内联事件，统一在 document 的 keydown 上处理；
   焦点不在当前弹窗时不动，避免抢走页面按键。 */
function modalFocusables(veil) {
  if (!veil || typeof veil.querySelectorAll !== 'function') return [];
  const sel = 'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
  return Array.from(veil.querySelectorAll(sel)).filter(el => el && !el.hidden && el.getAttribute('aria-hidden') !== 'true');
}
function trapModalTab(ev) {
  if (!ev || ev.key !== 'Tab' || typeof document === 'undefined') return;
  const open = document.querySelectorAll ? document.querySelectorAll('.veil.on') : [];
  const veil = open && open.length ? open[open.length - 1] : null;
  if (!veil || (typeof veil.contains === 'function' && !veil.contains(document.activeElement))) return;
  const list = modalFocusables(veil);
  if (!list.length) return;
  const first = list[0], last = list[list.length - 1];
  if (ev.shiftKey && document.activeElement === first) {
    ev.preventDefault(); last.focus();
  } else if (!ev.shiftKey && document.activeElement === last) {
    ev.preventDefault(); first.focus();
  }
}
if (typeof document !== 'undefined' && document.addEventListener) document.addEventListener('keydown', trapModalTab);
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}
function parseAPITime(value) {
  const text = String(value || '');
  if (!text || text.startsWith('0001-')) return 0;
  const ms = Date.parse(text);
  return Number.isFinite(ms) ? ms : 0;
}
function fmtLocalDateTime(ms) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}

/* fmtClock 只取本地 HH:MM，用于"缓存于 HH:MM"这类短标注（易用性修复 8）。 */
function fmtClock(ms) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return p(d.getHours()) + ':' + p(d.getMinutes());
}

/* ── 时间范围控件（用量 / 请求记录共用）────────────────────────────────
   预设项：今天 / 本周 / 本月 / 近 24 小时 / 近 3 天 / 近 7 天 / 近 30 天 /
   全部历史 / 自定义。本周自周一 00:00、本月自 1 号 00:00（浏览器本地时区）。

   为什么区间一律由前端算好再发：
     - 「今天」必须是**浏览器本地时区**的 00:00 起。服务端时区未必与浏览器一致
       （容器常挂 TZ=Asia/Shanghai，而浏览器可能在任何时区），让服务端算"今天"
       会在跨时区时切错日子。
     - 「自定义」本来就是用户挑的具体时刻，没有任何服务端推导空间。

   滚动预设（近 N 小时/天）则保留 hours 参数：服务端按整点对齐的滚动窗口与旧
   行为逐位一致，前端自己减 N 小时会多算/少算一个边界桶。 */
const TRANGE_PRESETS = [
  ['today', '今天'],
  ['week', '本周'],
  ['month', '本月'],
  ['24', '近 24 小时'],
  ['72', '近 3 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];
const ACT_TRANGE_PRESETS = [
  ['today', '今天'],
  ['24', '近 1 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['2160', '近 90 天'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];
const TRANGE_DEFAULT = '72';
const trangeStates = new Map(); // hostId → { preset, from: Date|null, to: Date|null }

// dtLocalValue / dtLocalParse 与 <input type=datetime-local> 的取值格式互转
// （YYYY-MM-DDTHH:mm，本地时区；ES 里"带时间的日期串"按本地解析，正是我们要的）。
function dtLocalValue(d) {
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}
function dtLocalParse(s) {
  if (!s) return null;
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}

// trangeMidnight 今天 00:00（本地时区）。
function trangeMidnight() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

function trangeState(id) {
  if (!trangeStates.has(id)) {
    // 自定义区间的初值在切到 custom 时再填（见 preset.onchange）；滚动/今天/
    // 本周/本月预设不读 from/to，避免把陈旧日期区间误当成有效输入。
    trangeStates.set(id, { preset: TRANGE_DEFAULT, from: null, to: null });
  }
  return trangeStates.get(id);
}

// trangeRender 画出控件骨架（幂等：重复调用会保留当前状态）。
function trangeRender(id) {
  const host = $(id);
  if (!host) return;
  const st = trangeState(id);
  const custom = st.preset === 'custom';
  const presets = id === 'actRange' ? ACT_TRANGE_PRESETS : TRANGE_PRESETS;
  host.innerHTML =
    '<select class="tr-preset" aria-label="时间范围">' +
    presets.map(([v, label]) =>
      '<option value="' + v + '"' + (v === st.preset ? ' selected' : '') + '>' + esc(label) + '</option>').join('') +
    '</select>' +
    '<span class="tr-custom"' + (custom ? '' : ' hidden') + '>' +
    '<input type="datetime-local" class="tr-from" value="' + esc(st.from ? dtLocalValue(st.from) : '') + '" aria-label="起始时间">' +
    '<span class="tr-sep">→</span>' +
    '<input type="datetime-local" class="tr-to" value="' + esc(st.to ? dtLocalValue(st.to) : '') + '" aria-label="结束时间">' +
    '</span>';
  const preset = host.querySelector('.tr-preset');
  if (preset) preset.onchange = () => {
    st.preset = preset.value;
    // 从别的预设切到自定义时，把区间重置为"今天 00:00 → 现在"，
    // 免得用户上次留下的半年区间被无声沿用，也避免首次切过去是空输入框。
    if (st.preset === 'custom' && (!st.from || !st.to)) { st.from = trangeMidnight(); st.to = new Date(); }
    trangeRender(id);
    trangeEmit(id);
  };
  const fromEl = host.querySelector('.tr-from');
  const toEl = host.querySelector('.tr-to');
  const readCustom = () => {
    if (!fromEl || !toEl) return;
    st.from = dtLocalParse(fromEl.value);
    st.to = dtLocalParse(toEl.value);
    // 起止颠倒就地标红（不静默纠正：用户可能正输到一半）。
    const bad = st.from && st.to && st.from > st.to;
    fromEl.classList.toggle('tr-bad', !!bad);
    toEl.classList.toggle('tr-bad', !!bad);
    if (bad) return;
    trangeEmit(id);
  };
  if (fromEl) fromEl.onchange = readCustom;
  if (toEl) toEl.onchange = readCustom;
}

const trangeHandlers = new Map();
// trangeBind 渲染控件并登记变化回调。**不**在绑定时触发回调：各视图的首次加载
// 由 go() 统一驱动，这里再触发一次会让打开页面时打两遍接口。
function trangeBind(id, onChange, preset) {
  trangeHandlers.set(id, onChange);
  if (preset) trangeState(id).preset = preset;
  trangeRender(id);
}
function trangeEmit(id) {
  const fn = trangeHandlers.get(id);
  if (fn) fn();
}

// trangeCalendarStart 返回本周（周一）或本月（1 号）的本地 00:00。
function trangeCalendarStart(preset) {
  const d = trangeMidnight();
  if (preset === 'tomorrow') { d.setDate(d.getDate() + 1); return d; }
  if (preset === 'next') {
    if (trangeState('actRange').preset === 'week') d.setDate(d.getDate() + 7);
    else { d.setMonth(d.getMonth() + 1, 1); d.setHours(0, 0, 0, 0); }
    return d;
  }
  if (preset === 'week') {
    // getDay()：0=周日；把周日映射到 6，周一为 0。
    d.setDate(d.getDate() - ((d.getDay() + 6) % 7));
  } else if (preset === 'month') {
    d.setDate(1);
  }
  return d;
}

// trangeQuery 把当前选择翻译成查询参数。
//   rolling=true  → 滚动预设发 hours（服务端整点对齐），今天/自定义发 from/to
//   rolling=false → 一律发 from/to（归档是线性日志，前端算区间更直观）
// 「全部历史」两者都不发。
function trangeQuery(id, rolling) {
  const st = trangeState(id);
  const q = new URLSearchParams();
  const sec = d => Math.floor(d.getTime() / 1000);
  if (st.preset === 'custom') {
    if (st.from) q.set('from', sec(st.from));
    if (st.to) q.set('to', sec(st.to));
    return q;
  }
  if (st.preset === 'today' || st.preset === 'week' || st.preset === 'month') {
    // Calendar boundaries use an explicit end for per-day buckets. Otherwise a
    // "today" query would include tomorrow's rows after midnight rollover.
    const start = trangeCalendarStart(st.preset);
    q.set('from', sec(start));
    if (id === 'actRange') q.set('to', sec(trangeCalendarStart(st.preset === 'today' ? 'tomorrow' : 'next')));
    return q;
  }
  // 全部历史：滚动端点（用量）必须**显式**传 hours=0。
  //
  // 后端对「什么都不给」的缺省是 72 小时（见 panel.go 的说明：
  // 「都不给：等同于 hours=72（保持旧调用方行为）」），所以这里返回空 query 会被
  // 当成「近 3 天」—— 正是 issue #121 报的现象：选了「全部历史」，数字却和
  // 「近 3 天」一模一样。
  //
  // 非滚动端点（请求记录）没有缺省窗口：不传 from/to 即"不限起点"，保持空 query。
  if (st.preset === '0') {
    if (rolling) q.set('hours', '0');
    return q;
  }
  if (rolling) { q.set('hours', st.preset); return q; }
  q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
  return q;
}

// trangeLabel 人读口径，用于「用量总览」右上角这类需要回显区间的位置。
function trangeLabel(id) {
  const st = trangeState(id);
  const found = TRANGE_PRESETS.find(p => p[0] === st.preset);
  if (st.preset !== 'custom') return found ? found[1] : '';
  if (!st.from && !st.to) return '自定义';
  const f = d => d ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0') : '…';
  return f(st.from) + ' → ' + f(st.to);
}
function rateLimitMeta(row, now) {
  const model = String(row && row.model || '未知模型');
  const kind = String(row && row.kind || 'rate_limit');
  const resetAt = parseAPITime(row && row.reset_at);
  const until = parseAPITime(row && row.until);
  const deadline = resetAt || until;
  const remaining = deadline > now ? Math.round((deadline - now) / 1000) : 0;
  if (kind === 'model_unavailable') {
    return {
      model,
      kind,
      detail: remaining ? '预计 ' + dur(remaining) + ' 后重试' : '等待重新探测',
      title: model + '\n模型当前不可用' + (deadline ? '\n最早重试：' + fmtLocalDateTime(deadline) : ''),
    };
  }
  let detail = resetAt
    ? '预计 ' + fmtLocalDateTime(resetAt) + ' 解封' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '')
    : (until ? '预计 ' + fmtLocalDateTime(until) + ' 恢复（剩余 ' + dur(remaining) + '）' : '预计解封时间未知');
  const title = [model, resetAt ? '上游重置：' + fmtLocalDateTime(resetAt) : '上游重置：时间未知'];
  if (until && resetAt && until < resetAt) {
    detail += ' · 网关最快 ' + dur(Math.max(0, Math.round((until - now) / 1000))) + ' 后重试';
    title.push('网关最早重试：' + fmtLocalDateTime(until));
  }
  return { model, kind, detail, title: title.join('\n') };
}
function rateLimitRowsHtml(rows, now) {
  const list = Array.isArray(rows) ? rows.filter(row => row && row.model) : [];
  if (!list.length) return '';
  return '<div class="rate-limits">' + list.map(row => {
    const m = rateLimitMeta(row, now);
    return '<div class="rate-limit ' + (m.kind === 'model_unavailable' ? 'model-unavailable' : '') +
      '" title="' + esc(m.title) + '"><b>' + esc(m.model) + '</b><span>' + esc(m.detail) + '</span></div>';
  }).join('') + '</div>';
}

function formatTokenCount(tokens) {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
  let unit = units[0];
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let value = n / unit[1];
  let rounded = Number(value.toFixed(1));
  // 999999 → 1m，而不是 1000k；四舍五入后自动升级单位。
  const next = units[units.indexOf(unit) + 1];
  if (next && rounded >= 1000) {
    unit = next;
    value = n / unit[1];
    rounded = Number(value.toFixed(1));
  }
  return rounded + unit[0];
}

function formatLatency(ms) {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { showModal('keyVeil', 'keyInput'); }
// 密钥一旦验过就不再留在 DOM 里：共用浏览器/截图/扩展读页时，输入框中的
// 明文比内存变量更容易被拿走。重新打开弹窗时再让用户输入。
function clearKeyInput() {
  const el = $('keyInput');
  if (el) el.value = '';
}
if ($('btnKey')) $('btnKey').onclick = async () => {
  const input = $('keyInput');
  const v = input ? input.value.trim() : '';
  if (!v) return;
  memKey = v;
  try {
    await api('overview');
    clearKeyInput();
    const err = $('keyErr');
    if (err) err.hidden = true;
    hideModal('keyVeil');
    start();
  } catch (e) {
    memKey = '';
    const err = $('keyErr');
    if (err) { err.hidden = false; err.textContent = e && e.message ? e.message : '密钥无效'; }
  }
};
/* 表单提交兜底：阻止隐式提交导航（CSP script-src 'self' 下内联 onsubmit 不生效，
   故统一由同源 JS 兜底）。密钥框是单输入框表单：按 Enter 的默认行为是提交，无兜底
   会触发一次导航尝试（CSP form-action 'none' 拒之亦非预期交互）。挂 submit 而非只挂
   keydown：隐式提交不经过 keydown 逻辑（如焦点在别处时的回车），必须在 submit 上兜底。
   逐 form 绑定（而非某个 id）：不依赖具体 id，新增表单无需再改这里。 */
document.querySelectorAll('form').forEach(f => f.addEventListener('submit', e => e.preventDefault()));
if ($('keyInput')) $('keyInput').addEventListener('keydown', e => {
  if (e.key === 'Enter') { e.preventDefault(); const b = $('btnKey'); if (b) b.click(); }
});

/* ── 路由 ─────────────────────────────────────────────────────────── */
/* 积分包明细共享缓存：「积分构成」视图与首页「积分到期提醒」卡片共用同一份
   /panel/api/packages 数据（逐账号实时查上游，能省一次是一次）。声明在路由区
   是因为 go() 的初始调用就会触发首页卡片的渲染，必须先于它就位。 */
let lastPackages = null;
let lastPackagesAt = 0;
let expFetching = false;                       // 到期卡片在途标记（防重复打上游）
const EXP_FRESH_MS = 2 * 60 * 1000;            // 缓存新鲜窗口：2 分钟内复用
/* invalidatePackagesCache 账号增删后作废 packages 缓存（易用性修复 8）：新加/移除的
   账号不在旧快照里，继续复用会让到期卡片与积分构成表里挂着已删除的账号、少了新账号。
   下一次 loadPackages / loadExpiry 会重新实时查上游。 */
function invalidatePackagesCache() {
  lastPackages = null;
  lastPackagesAt = 0;
}

// loadPackagesData 所有「逐账号实时查 packages」的入口共享这一份在途 Promise：
// 首页到期检查、积分构成刷新、套餐页刷新以前各用各的锁，同时点会重复打一整套
// 上游请求，先回来的还可能覆盖后回来的。这里只允许一个真实查询，其余调用复用
// 同一结果；force 语义由调用方决定是否接受缓存。
let packagesInFlight = null;
function loadPackagesData() {
  if (packagesInFlight) return packagesInFlight;
  const run = (async () => {
    const d = await api('packages');
    lastPackages = d;
    lastPackagesAt = Date.now();
    return d;
  })();
  packagesInFlight = run;
  return run.finally(() => { if (packagesInFlight === run) packagesInFlight = null; });
}

const TITLES = { accounts: '账号池', activity: '活动记录', usage: '用量', packages: '积分构成', taskscenter: '任务与福利', connect: '接入', models: '模型与档位', config: '配置', logs: '运行日志' };
// cfgDirty 标记配置页是否有未保存修改。页面内切视图用 confirm，关闭/刷新用
// beforeunload 的浏览器原生提示。loadConfig 回填后清除，避免误报。
let cfgDirty = false;
function go(v) {
  if (view === 'config' && v !== 'config' && cfgDirty) {
    if (!confirm('配置页有未保存的修改，离开将丢失。确定离开吗？')) return false;
    cfgDirty = false;
  }
  // 离开任务中心时停掉扫描轮询：扫描本身在后端继续，但 500ms 一次的轮询
  // 没有理由在别的视图里继续跑；回到任务中心会重新接上进度。
  // 切到任务页时由 reattachScanView 决定是否恢复进度；切到任何别的视图都要
  // 停表并复位按钮，避免首次初始化或异常路径留下「扫描中…」。
  if (v !== 'taskscenter') {
    stopScanPolling();
    // Older embedded frontends may not declare the welfare poll state; guard
    // the cleanup so switching views can never stop working on a stale page.
    if (typeof benefitsBatchTimer !== 'undefined' && benefitsBatchTimer) {
      clearTimeout(benefitsBatchTimer); benefitsBatchTimer = null;
    }
    if (typeof benefitsBatchSeq !== 'undefined') benefitsBatchSeq++;
    const sb = $('btnScanAll');
    if (sb) { sb.disabled = false; sb.textContent = '扫描待办'; }
  }
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => {
    const on = a.dataset.view === v;
    a.classList.toggle('on', on);
    if (on) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
  // Hub 把页面标题与品牌放在同一个顶栏 h1 里；切换视图时更新副标题即可。
  const navSub = $('navSub');
  if (navSub) navSub.textContent = TITLES[v] || '控制台';
  setupSectionNav(v);
  const mdBody = $('mdBody');
  if (v === 'models' && (!mdBody || !mdBody.children || !mdBody.children.length)) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'usage') loadUsage();
  if (v === 'activity') { renderActivityAccountOptions(); loadActivity(); }
  if (v === 'taskscenter' && typeof loadBenefits === 'function') loadBenefits();
  if (v === 'connect') loadConnect();
  if (v === 'packages') loadPackages();
  // 到期提醒卡片：默认可见，进页面即用 overview 快照渲染（零上游请求，随首页统计
  // 每轮刷新）。不再「打开账号池就打上游」——逐账号实时查在账号多时会卡死面板。
  // 完整逐包明细由卡片上的「检查」按钮显式触发（loadPackages/loadExpiry）。
  if (v === 'taskscenter') reattachQueueView();
  return true;
}
// 导航用事件委托而不是逐个 onclick：旧 index.html 可能没有 .nav a，
// 或脚本在 DOMContentLoaded 之前执行导致 querySelectorAll 为空，之后
// 所有页签都会失效。委托在 document 上，节点晚到也能工作。
if (typeof document !== 'undefined' && document.addEventListener) {
  document.addEventListener('click', e => {
    const a = e.target && e.target.closest ? e.target.closest('.nav a[data-view]') : null;
    if (!a) return;
    e.preventDefault();
    if (go(a.dataset.view) !== false && history && history.pushState) {
      history.pushState(null, '', '#' + a.dataset.view);
    }
  });
}
// hashchange：深链与浏览器前进/后退。此前只有点击委托会 go()，脚本或用户
// 直接改 location.hash 时页面不切视图，地址栏与内容不一致。
if (typeof addEventListener === 'function') addEventListener('hashchange', () => {
  // 页内导航也用 #sec-* 锚点；主路由只认 9 个视图，其他 hash 不能被当成
  // 切页信号（否则点击区块导航会触发一次无意义的分支/刷新）。
  const next = (location.hash || '#accounts').slice(1);
  if (!(next in TITLES)) return;
  if (next === view) return;
  go(next);
});
if (typeof addEventListener === 'function') addEventListener('popstate', () => {
  const next = (location.hash || '#accounts').slice(1);
  if (!(next in TITLES) || next === view) return;
  go(next);
});
if (typeof addEventListener === 'function') addEventListener('beforeunload', e => {
  if (!cfgDirty) return;
  e.preventDefault();
  e.returnValue = '';
});
/* 首次进入延到本轮脚本求值之后再 go()。
   原因：go() 会同步触发视图的数据加载（loadUsage/loadLogs/loadPackages…），而这些
   函数读到的模块级 let/const（usageRateWarmAt、PK_* 等）在文件后半段才初始化——
   直接深链 #usage / #packages 打开页面时会踩 TDZ（"Cannot access 'x' before
   initialization"），表现为该页永远显示"读取失败"，而点导航进去一切正常。
   延迟 0ms 让整份脚本先求值完，是修这一类问题最省事也最不容易再犯的办法。 */
setTimeout(() => {
  const hash = (location.hash || '#accounts').slice(1);
  go(hash in TITLES ? hash : 'accounts');
}, 0);

/* ── 账号池 ───────────────────────────────────────────────────────── */
/* 账号表搜索/排序（易用性修复 11，照抄模型页 #mdFilter 的做法）：账号数据一次
   从 /overview 拿全，筛选与排序全在前端完成——改条件零延迟，也不额外打上游。
   accFilter.q 是空格分词的 AND 包含匹配（昵称/uid/原因/模型名）。 */
function accSearchText(s) {
  return [s.nickname, s.uid, s.note, s.reason, (s.rate_limited_models || []).map(m => m && m.model).join(' ')]
    .filter(Boolean).join(' ').toLowerCase();
}

function accMatch(s, f) {
  s = s || {};
  f = f || accFilter;
  if (f.q) {
    const text = accSearchText(s);
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.state) {
    const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
    const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
    const cooling = !!s.cooling || Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0) > 0;
    if (f.state === 'disabled' && !s.disabled) return false;
    if (f.state === 'paused' && (!s.paused || s.disabled)) return false;
    if (f.state === 'cooling' && (s.disabled || !cooling)) return false;
    if (f.state === 'healthy' && (s.disabled || s.paused || cooling)) return false;
  }
  if (f.realm === 'enterprise' && !s.enterprise) return false;
  if (f.realm && f.realm !== 'enterprise' && s.realm !== f.realm) return false;
  return true;
}

function accSortList(list, f) {
  f = f || accFilter;
  const out = list.slice();   // 不改动入参：上游顺序保持可回溯
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'credits') out.sort((a, b) => num(b.credits) - num(a.credits));
  else if (f.sort === 'weight') out.sort((a, b) => num(b.weight || 1) - num(a.weight || 1));
  else if (f.sort === 'requests') out.sort((a, b) => num((b.token_usage || {}).request_count) - num((a.token_usage || {}).request_count));
  else if (f.sort === 'name') out.sort((a, b) => String(a.nickname || a.uid || '').localeCompare(String(b.nickname || b.uid || '')));
  return out;
}

/* accountRowSig 账号行重建签名：5 秒轮询只比较可见字段，避免整表重建丢焦点；
   但所有会改变显示内容的字段都必须进入签名（realm/企业标记/成本提示/限流类型/
   最近用量）。同一分钟内的相对时间不变化，故不必包含逐秒时钟。

   这里刻意不用 JSON.stringify：470 行每 5 秒序列化 token_usage/model_costs/
   rate_limited_models 会产生明显的临时对象与字符串。用长度前缀拼接普通标量，
   嵌套数组只取渲染实际读取的字段；分隔符 \x1f 不会出现在这些字段里。 */
function accountRowSig(s) {
  if (!s) return '';
  const parts = [
    s.uid, s.nickname, s.realm, s.enterprise ? 1 : 0, s.weight || 1, s.note || '', s.has_tokens ? 1 : 0, s.credits, s.credits_total, s.credits_expiring,
    s.cooling ? 1 : 0, s.cool_remaining_sec, s.cool_kind || '', s.disabled ? 1 : 0, s.paused ? 1 : 0, s.in_flight,
    s.success_count, s.err_total, s.last_success || '', s.checkin_done ? 1 : 0, s.breaker_until || '', s.degrade_until || '',
    s.reason || '',
  ];
  const usage = s.token_usage || {};
  parts.push(usage.request_count, usage.total_tokens, usage.last_latency_ms, usage.last_tokens_per_second);
  const costs = s.model_costs || [];
  parts.push(costs.length);
  for (const c of costs) parts.push(c.model, c.cost_per_1k, c.last_seen, c.samples);
  const limits = s.rate_limited_models || [];
  parts.push(limits.length);
  for (const l of limits) parts.push(l.model, l.kind, l.until, l.reset_at);
  let out = '';
  for (const v of parts) {
    const text = v == null ? '' : String(v);
    out += text.length + ':' + text + '\x1f';
  }
  return out;
}

// accRenderSig 记录上一轮渲染的账号顺序+行签名。5 秒轮询若数据未变，直接跳过
// 整表 innerHTML 重建（470 行时这是每 5 秒一次的大块 DOM 分配与布局）。
let accRenderSig = '';

// accVirtualWindow 计算当前页需要写入 DOM 的行区间。固定视口 + 上下占位行
// 保持完整滚动高度。Virtualization is unconditional for non-empty pages: even
// 50-row pages keep a bounded DOM, and narrow layouts keep the same fixed
// viewport instead of falling back to a page-height table.
function accVirtualWindow() {
  const rows = accVirtualRows;
  const vp = $('accViewport');
  if (!vp || !rows.length) {
    accVirtualStart = 0; accVirtualEnd = rows.length;
    return { start: 0, end: rows.length, topPad: 0, bottomPad: 0 };
  }
  const rowH = Math.max(24, Number(accVirtualRowH) || 44);
  const viewportH = Number(vp.clientHeight) || 520;
  const first = Math.max(0, Math.floor(Number(vp.scrollTop || 0) / rowH) - 6);
  const count = Math.ceil(viewportH / rowH) + 14;
  const end = Math.min(rows.length, first + count);
  accVirtualStart = first;
  accVirtualEnd = end;
  return { start: first, end, topPad: first * rowH, bottomPad: Math.max(0, (rows.length - end) * rowH) };
}

// bindAccVirtualScroll 在滚动时只重算窗口。签名没变时 renderAccounts 会
// 提前返回，因此这里直接调用 renderAccounts(accAll)，复用同一套筛选与
// 焦点保护逻辑；5 秒轮询与此路径不会并发重建两份 DOM。
function bindAccVirtualScroll() {
  if (accVirtualBound) return;
  const vp = $('accViewport');
  if (!vp || typeof vp.addEventListener !== 'function') return;
  accVirtualBound = true;
  let raf = 0;
  vp.addEventListener('scroll', () => {
    if (raf) return;
    const run = () => {
      raf = 0;
      if (!accAll || !accAll.length) return;
      accRenderSig = '';
      // 滚动是纯视图动作：焦点可能正停在表内按钮上（用户刚点过某行）。
      // renderAccounts 的焦点保护会直接返回，导致窗口停在旧位置、继续滚动
      // 看到空白。这里显式跳过焦点保护，只重建虚拟窗口。
      renderAccounts(accAll, true);
    };
    raf = typeof requestAnimationFrame === 'function'
      ? requestAnimationFrame(run) : setTimeout(run, 16);
  }, { passive: true });
  if (typeof window !== 'undefined' && window.addEventListener) {
    window.addEventListener('resize', () => {
      if (!accAll || !accAll.length) return;
      accRenderSig = '';
      renderAccounts(accAll, true);
    }, { passive: true });
  }
}

function renderAccounts(list, forceRender) {
  const tb = $('accBody');
  if (!tb) { noteDOM('accBody'); return; }
  accAll = list || [];
  // 缓存最近一次全量数据：搜索/排序在已拉到的数据上做，改条件不必重新打 /overview。
  if (typeof renderActivityAccountOptions === 'function') renderActivityAccountOptions();
  const shown = accSortList(accAll.filter(s => accMatch(s)));
  const live = new Set(accAll.map(s => s && s.uid));
  for (const uid of Array.from(accSelected)) if (!live.has(uid)) accSelected.delete(uid);
  const pageSize = accPageSize > 0 ? accPageSize : Math.max(1, shown.length);
  const pageCount = Math.max(1, Math.ceil(shown.length / pageSize));
  if (accPage > pageCount) accPage = pageCount;
  if (accPage < 1) accPage = 1;
  const start = (accPage - 1) * pageSize;
  const pageRows = shown.slice(start, start + pageSize);
  syncAccSelectionUI(shown, pageRows);
  // 注意筛选/排序控件文本也要跟着更新，所以先算完 accCount 再决定是否跳过表格。
  const tableSig = list.length || shown.length
    ? pageRows.map(s => s.uid + '\x00' + accountRowSig(s)).join('\x01') + '\x02' + start
    : '';
  const accCount = $('accCount');
  if (accCount) {
    const filtered = shown.length !== accAll.length;
    accCount.textContent = !accAll.length ? '' : (filtered ? '命中 ' + shown.length + ' / ' + accAll.length + ' 个账号' : accAll.length + ' 个账号');
    accCount.className = filtered ? 'note src-off' : 'note';
  }
  // 5 秒轮询会走到这里。若用户正聚焦在账号表内（按钮/tab/链接），整表 innerHTML
  // 重建会丢焦点、标题悬停与横向滚动位置；本轮先只更新统计，保留 DOM，等焦点
  // 移出后再按最新数据重建。accAll 已在上面更新，不会因此显示旧快照。
  // 注意：焦点在表内时只跳过「远端数据未变」的整表重建；若可见数据已变化
  //（动作完成、冷却状态切换），仍需重建，否则表内按钮聚焦期间状态会长期陈旧。
  // document/activeElement 在旧骨架、无头测试或极早求值阶段可能不存在；这里只是
  // 焦点保护，缺能力时按「焦点不在表内」处理，不能让它阻断整表渲染。
  const activeEl = (document && document.activeElement) || null;
  const focusInTable = !!activeEl && typeof tb.contains === 'function' && tb.contains(activeEl);
  const wrap = (tb.closest && tb.closest('.tbl-wrap')) || tb;
  const scrollTop = wrap.scrollTop || 0;
  const scrollLeft = wrap.scrollLeft || 0;
  const restoreScroll = () => {
    // 横向滚动容器是 .tbl-wrap 而不是 tbody：只读 tbody.scrollLeft 会让每次轮询
    // 重建都把表格滚回最左。两个节点都恢复，兼容测试里的简化 DOM 桩。
    try { wrap.scrollTop = scrollTop; wrap.scrollLeft = scrollLeft; } catch (e) { /* inert DOM */ }
    try { tb.scrollTop = scrollTop; } catch (e) { /* inert DOM */ }
  };
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="10"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div></td></tr>';
    accVirtualRows = [];
    accVirtualSignature = '';
    accRenderSig = '';
    restoreScroll();
    return;
  }
  if (!shown.length) {
    tb.innerHTML = '<tr><td colspan="10" class="empty-cell"><div class="empty">没有符合当前筛选条件的账号' +
      '<div style="margin-top:10px"><button type="button" class="xs" data-a="clear-filter">清除筛选</button></div></div></td></tr>';
    accVirtualRows = [];
    accVirtualSignature = '';
    accRenderSig = '';
    restoreScroll();
    return;
  }
  // 数据与上一轮一致时不需要重建。筛选/排序变化会改变 uid 顺序或集合，签名随之
  // 变化；busyKeys 也会进入 action 按钮文案，但它不是远端数据，故把 busyKeys
  // 变化显式纳入签名，避免动作开始/结束后行不刷新。焦点保护命中且数据已变时
  // 不能提前写 accRenderSig——否则焦点移出后签名已相等，会永久停在旧 DOM。
  const busySig = Array.from(busyKeys).sort().join('\x02');
  const fullSig = tableSig + '\x03' + busySig + '\x04' + accFilter.q + '\x05' + accFilter.sort +
    '\x06' + accFilter.state + '\x07' + accFilter.realm;
  if (focusInTable && !forceRender) return;
  if (fullSig === accRenderSig && tb.childNodes && tb.childNodes.length) { bindAccVirtualScroll(); return; }
  accRenderSig = fullSig;
  // 有总额度（credits_total）→ 进度条按自身 剩余/总额 百分比；旧数据无总额 → 退回池内最高=100%
  // 企业版不限量（credits_total === -1）的 credits 是 1<<40 量级的哨兵值，若计入会把
  // 池内最高额度抬到 1.1e12，导致所有普通账号的进度条恒为 0%。求 maxCred 时排除它们。
  const maxCred = Math.max(1, ...shown.filter(s => s.credits_total !== -1).map(s => s.credits || 0));
  // 分页语义保持不变；窗口化只缩小「当前页」实际写入 DOM 的行数。
  accVirtualRows = pageRows;
  const virtual = accVirtualWindow();
  const rowHtml = accVirtualRows.slice(virtual.start, virtual.end).map(s => accRowHtml(s, maxCred));
  let bodyHtml = '';
  if (virtual.topPad > 0) bodyHtml += '<tr class="virtual-pad" aria-hidden="true"><td colspan="10"><i style="height:' + virtual.topPad + 'px"></i></td></tr>';
  bodyHtml += rowHtml.join('');
  if (virtual.bottomPad > 0) bodyHtml += '<tr class="virtual-pad" aria-hidden="true"><td colspan="10"><i style="height:' + virtual.bottomPad + 'px"></i></td></tr>';
  tb.innerHTML = bodyHtml;
  // 首次渲染后按真实行高校准窗口；只在测量值与当前值不同时再渲染一轮，
  // 避免每帧「渲染→测量→重渲染」循环。
  if (virtual.end > virtual.start && typeof tb.querySelector === 'function') {
    const firstRow = tb.querySelector('tr[title^="uid:"]');
    const measured = firstRow && firstRow.getBoundingClientRect
      ? Math.round(firstRow.getBoundingClientRect().height || 0) : 0;
    if (measured > 20 && measured < 400 && Math.abs(measured - accVirtualRowH) > 1) {
      accVirtualRowH = measured;
      accVirtualSignature = '';
      accRenderSig = '';
      tb.innerHTML = '';
      renderAccounts(accAll);
      return;
    }
  }
  bindAccVirtualScroll();
  restoreScroll();
}

// accRowHtml 渲染单个账号行。字段与旧内联实现逐项一致；窗口化渲染复用
// 同一函数，避免两份展示口径漂移。
function accRowHtml(s, maxCred) {
  {

    const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
    const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
    const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
    let cls = '', tag;
    if (s.disabled) { cls = 'off'; tag = '<span class="tag bad">已禁用</span>'; }
    else if (s.paused) { cls = 'off'; tag = '<span class="tag warn">已暂停选号</span>'; }
    else if (cool > 0) {
      cls = 'cool';
      const kind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? '熔断'
        : (dg > (s.cool_remaining_sec || 0) ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
      tag = '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>';
    } else tag = '<span class="tag ok">可用</span>' + (s.in_flight ? '' : '');
    const note = s.reason ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.reason) + '</div>' : '';
    const rateLimits = rateLimitRowsHtml(s.rate_limited_models, Date.now());
    const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
    // 企业版不限量：上游 limitNum == -1，网关以 credits_total=-1 透出（见 upstream
    // enterpriseUnlimitedTotal）。此时剩余额度不参与展示，直接标「不限」。
    const unlimited = s.credits_total === -1;
    const cred = unlimited ? '不限'
      : (s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits)));
    // 企业版口径与个人不同：credits=本周期剩余、credits_total=分配额度。
    // 仍写「剩余/总额」会让运维误以为是可结转积分；这里直接标为分配额度。
    const credLabel = s.enterprise ? '企业额度' : '积分';
    const credValue = s.enterprise && !unlimited && s.credits != null
      ? String(s.credits) + (s.credits_total > 0 ? '<span class="of"> / 分配 ' + s.credits_total + '</span>' : '')
      : cred;
    const pct = unlimited ? 100
      : (s.credits_total > 0
        ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
        : Math.round((s.credits || 0) / maxCred * 100));
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
    // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
    let credTip;
    if (unlimited) credTip = '企业版不限量（上游 limitNum=-1）';
    else if (s.credits_total > 0) {
      credTip = (s.enterprise ? '企业版剩余额度 ' : '剩余 ')
        + s.credits + ' / ' + (s.enterprise ? '分配 ' : '总额 ') + s.credits_total + '（' + pct + '%）';
    } else credTip = '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    const tu = s.token_usage || {};
    const req = tu.request_count || 0;
    const totalTok = formatTokenCount(tu.total_tokens);
    const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
    const latency = formatLatency(tu.last_latency_ms);
    const rate = formatRate(tu.last_tokens_per_second);
    const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
    const weightTag = (s.weight || 1) > 1 ? '<span class="wtag" title="手动调度权重">W' + esc(s.weight) + '</span>' : '';
    const noteLine = s.note ? '<div class="note-line" title="' + esc(s.note) + '">' + esc(s.note) + '</div>' : '';
    const credTag = s.has_tokens === false ? '<span class="wtag" style="color:var(--bad);background:var(--bad-soft);border-color:color-mix(in srgb,var(--bad) 25%,transparent)" title="缺少 accessToken 或 refreshToken，无法接单">凭证不全</span>' : '';
    return '<tr class="' + cls + (accSelected.has(s.uid) ? ' picked' : '') + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="pick" data-label="选择"><input type="checkbox" data-pick="' + esc(s.uid) + '" aria-label="选择 ' + esc(s.nickname || s.uid) + '"' + (accSelected.has(s.uid) ? ' checked' : '') + '></td>' +
      '<td class="who" data-label="账号"><div class="nm">' + (s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + (s.enterprise ? ' <span class="realm-tag">企业版</span>' : '') + weightTag + credTag + '</div><div class="id">' + esc(short) + '</div>' + noteLine + '</td>' +
      '<td data-label="状态">' + tag + note + rateLimits + '</td>' +
      '<td class="cred" data-label="' + credLabel + '" title="' + esc(credTip) + '"><div class="n">' + credValue + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num" data-label="成功 / 失败">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
      '<td class="num" data-label="在途">' + (s.in_flight || 0) + '</td>' +
      '<td class="num usage-cell" data-label="用量" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
        '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
        '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
        '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
        '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
      '</span></td>' +
      '<td class="num" data-label="最近成功" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
      '<td class="acts" data-label="操作">' + actButtons(s, frozen) +
      '</td></tr>';
  }
}

/* actButtons 渲染单行的动作按钮。在途动作（busyKeys 命中）恢复 disabled 与
   "执行中…" 文案：5 秒轮询会用 renderAccounts 重建整张表，DOM 上的 disabled
   会被清掉，若不按模块级 busyKeys 复原则用户能重复点（易用性修复 5）。 */
function actButtons(s, frozen) {
  const btn = (act, label, cls, extra) => {
    const key = act + '|' + s.uid;
    const busy = busyKeys.has(key);
    return '<button class="xs ' + cls + '" data-a="' + act + '" data-u="' + esc(s.uid) + '"' +
      (busy ? ' disabled' : '') + (extra || '') + '>' + (busy ? '执行中…' : label) + '</button>';
  };
  return (
        // 企业版无个人成长体系（签到 400「企业账号不支持该操作」/ 成长任务 403）：
        // 不渲染「签到」「任务」按钮，只留「额度」——点它走 /balance，企业额度由
        // upstream 的 get-enterprise-user-usage 口径填充。
        (s.enterprise ? '' :
          btn('checkin', s.checkin_done ? '已签' : '签到', 'ghost', s.checkin_done ? ' title="今日已签到；点击可重新签到并刷新余额"' : '')) +
        btn('balance', s.enterprise ? '额度' : '余额', 'ghost', s.enterprise ? ' title="刷新企业版已分配额度（上游 get-enterprise-user-usage）"' : '') +
        (s.enterprise ? '' :
          btn('tasks', '任务', 'ghost')) +
        btn('profile', '设置', 'ghost', ' title="备注 / 调度权重"') +
        (frozen ? btn('revive', '解冻', 'primary')
                : (s.paused ? btn('resume', '恢复选号', 'primary')
                            : btn('pause', '暂停选号', 'ghost', ' title="' + (s.enterprise ? '退出选号，但照常保活 / 刷新额度' : '退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额') + '"'))) +
        (s.disabled ? '' : btn('disable', '禁用', 'ghost')) +
        btn('remove', '移除', 'ghost danger')
  );
}

// renderModelLocks 模型锁池：哪些模型不能用、锁了几个号、还要锁多久。
// 后端 model_locks 已按「整池不可用 → 没号可用 → 部分限流」排好序，这里只做展示。
function renderModelLocks(rows) {
  const tb = $('mlBody');
  if (!tb) return;
  const note = $('mlNote');
  if (!rows || !rows.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">当前没有模型级限流 —— 所有模型均可选</div></td></tr>';
    if (note) note.textContent = '';
    return;
  }
  const STATE = { locked: ['bad', '整池不可用'], starved: ['warn', '没号可用'], partial: ['warn', '部分限流'] };
  const left = iso => {
    const ms = parseAPITime(iso);
    return ms ? dur(Math.max(0, Math.round((ms - Date.now()) / 1000))) : '—';
  };
  tb.innerHTML = rows.map(r => {
    const st = STATE[r.state] || ['mute', r.state || '—'];
    const realm = r.realm === 'global' ? '国际版' : '国内版';
    return '<tr>' +
      '<td>' + esc(r.model) + '</td>' +
      '<td><span class="realm-tag">' + realm + '</span></td>' +
      '<td><span class="tag ' + st[0] + '">' + st[1] + '</span></td>' +
      '<td class="num">' + (r.servable || 0) + ' / ' + (r.total || 0) + '</td>' +
      '<td class="num">' + (r.locked || 0) + '</td>' +
      '<td class="num">' + left(r.unlock_at || r.fully_unlock_at) + '</td>' +
      '<td class="num">' + left(r.fully_unlock_at) + '</td>' +
      '<td>' + (r.reason ? '<div class="note">' + esc(r.reason) + '</div>' : '—') + '</td>' +
      '</tr>';
  }).join('');
  if (note) {
    const bad = rows.filter(r => r.state === 'locked' || r.state === 'starved').length;
    note.textContent = bad ? bad + ' 个模型整池不可用' : rows.length + ' 个模型部分限流';
  }
}

async function loadOverview(quiet) {
  // 后台 5 秒刷新叠加在途请求会把 /overview 打成串行排队；慢响应回来时
  // 页面反而更旧。单飞：当前请求未结束时直接跳过这一轮（手动动作仍会触发
  // 下一轮），与 logs/queue 的保护一致。
  if (overviewInFlight) { overviewPending = true; return; }
  const my = ++overviewSeq;
  overviewInFlight = true;
  try {
    const h = authHeaders();
    if (overviewETag) h['If-None-Match'] = overviewETag;
    const r = await fetch('/panel/api/overview', { headers: h });
    if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
    if (r.status === 304) {
      if (my !== overviewSeq) return;
      // 304 表示账号/模型/余额都没变；只推进「运行 N」这类纯时间字段，
      // 不重渲染账号表，也不清空已渲染内容。
      if (lastUptimeSec) renderUptime(lastUptimeSec + Math.floor((Date.now() - overviewFetchedAt) / 1000));
      return;
    }
    const d = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
    // 5 秒轮询、账号动作后的刷新和手动刷新可能并发：晚到的旧响应不得回写
    // 账号表/统计条，否则页面会整体倒退到上一份快照。
    if (my !== overviewSeq) return;
    overviewData = d;
    overviewETag = r.headers.get('ETag') || '';
    overviewFetchedAt = Date.now();
    lastUptimeSec = Number(d.uptime_sec) || 0;
    setText('sTotal', d.total);
    setText('sHealthy', d.healthy);
    setText('sCooling', d.cooling);
    setText('sDisabled', d.disabled);
    // 暂停选号单列（issue #125）：它只关选号、照常签到保活，与禁用是两种状态。
    if ($('sPaused')) $('sPaused').textContent = d.paused == null ? '-' : d.paused;
    // 企业版不限量：credits_total === -1（上游 limitNum=-1）。把它计进总额会得到
    // 负/荒谬的合计（501 + (-1) = 500 之类），必须跳过求和并单独告知"不限量 N 个"
    // （易用性修复 6）。
    const accts = d.accounts || [];
    const limited = accts.filter(s => s.credits_total !== -1);
    // 只对有限额账号求和：企业版不限量（credits_total === -1）的 credits 是 1<<40 哨兵值，
    // 计进去会让顶栏「积分剩余」显示约 1.1 万亿（易用性修复）。
    const remSum = limited.reduce((a, s) => a + (s.credits || 0), 0);
    const totSum = limited.reduce((a, s) => a + (s.credits_total || 0), 0);
    const unlimitedN = accts.length - limited.length;
    const creditsText = (totSum > 0 ? remSum + ' / ' + totSum : String(remSum)) +
      (unlimitedN ? ' · 不限量 ' + unlimitedN + ' 个' : '');
    setText('sCredits', creditsText);
    // 统计卡在窄屏会被压到一列；长金额必须可换行并保留完整值，
    // 不能只靠 CSS ellipsis 静默截断（运维无法据此判断额度）。
    const creditsEl = $('sCredits');
    if (creditsEl) {
      creditsEl.title = creditsText;
      creditsEl.setAttribute('aria-label', '积分 / 企业已分配：' + creditsText);
    }
    setText('sSticky', d.sticky_sessions);
    if ($('navVer')) $('navVer').textContent = 'v' + d.version;
    if ($('navRedis')) $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    if ($('navState')) $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    const statusDot = $('statusDot');
    if (statusDot) statusDot.classList.toggle('err', !(d.healthy > 0));
    const p = $('navPulse');
    if (p) p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    const dot = $('statusDot');
    if (dot) dot.className = 'hub-dot' + (d.healthy > 0 ? '' : ' err');
    setText('accNote', d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '');
    renderUptime(lastUptimeSec);
    renderAccounts(d.accounts || []);
    renderModelLocks(d.model_locks);
    // 到期卡片跟随首页统计同步刷新（用同一份响应里的到期快照，不额外发请求）。
    // 已有逐包明细缓存时不再覆盖——明细信息量更大，别把它降级成快照。
    if ($('expBox') && !lastPackages) renderExpirySnapshot(d);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
  finally {
    overviewInFlight = false;
    if (overviewPending) { overviewPending = false; loadOverview(true); }
  }
}

function renderUptime(sec) {
  const up = Math.max(0, Math.floor(Number(sec) || 0));
  const text = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') +
    Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
  // Hub 顶栏没有独立页面副标题；把运行时长放进右侧 meta 的版本行，
  // 既保留可观测信息，也避免用不存在的 #subMeta 触发空节点异常。
  const sub = $('subMeta');
  if (sub) sub.textContent = text;
  const ver = $('navVer');
  if (ver && !ver.title) ver.title = text;
}

/* checkAuthGate 在未配置管理密钥时给出明确提示。管理 API fail-closed（403），
   前端若仍静默轮询只会看到账号表一直为空；提示条告诉用户该改哪个配置项。 */
async function checkAuthGate() {
  const guard = $('authGate');
  try {
    const r = await fetch('/panel/api/overview', { headers: authHeaders() });
    if (r.status === 403) {
      if (guard) guard.hidden = false;
      return;
    }
    if (!r.ok) return;
    if (guard) guard.hidden = true;
  } catch (_) { /* 网络错误由常规轮询提示，不误报配置问题 */ }
}

// 筛选控件绑定：搜索框防抖 120ms（整表重渲染），下拉即时（照抄模型页 #mdFilter）。
let accQTimer = null;
if ($('accQ')) $('accQ').oninput = () => {
  clearTimeout(accQTimer);
  accQTimer = setTimeout(() => {
    const q = $('accQ');
    if (!q) return;
    accFilter.q = q.value.trim();
    accPage = 1;
    renderAccounts(accAll);
  }, 120);
};
if ($('accSort')) $('accSort').onchange = () => {
  accFilter.sort = $('accSort').value;
  accPage = 1;
  renderAccounts(accAll);
};
if ($('accRealm')) $('accRealm').onchange = () => {
  accFilter.realm = $('accRealm').value;
  accPage = 1;
  renderAccounts(accAll);
};
if ($('accState')) $('accState').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-state]');
  if (!b) return;
  accFilter.state = b.dataset.state || '';
  accPage = 1;
  const accState = $('accState');
  if (accState) accState.querySelectorAll('button[data-state]').forEach(x => {
    x.setAttribute('aria-pressed', x === b ? 'true' : 'false');
  });
  renderAccounts(accAll);
});
/* 行密度偏好存本地：账号多时「紧凑」能一屏多看几行，刷新后保持选择。 */
let accDense = false;
try { accDense = localStorage.getItem('wb-acc-dense') === '1'; } catch (_) { /* 隐私模式 */ }
function applyAccDensity() {
  const t = $('accTable'), b = $('btnAccDensity');
  if (t) t.classList.toggle('dense', accDense);
  if (b) b.setAttribute('aria-pressed', accDense ? 'true' : 'false');
}
applyAccDensity();
if ($('btnAccDensity')) $('btnAccDensity').onclick = () => {
  accDense = !accDense;
  try { localStorage.setItem('wb-acc-dense', accDense ? '1' : '0'); } catch (_) { /* ignore */ }
  applyAccDensity();
};

/* 账号分页/全选/批量动作。分页在前端切片，切页不额外请求上游；筛选变化
   回到第一页。勾选跨分页保留，批量动作只提交明确选中的 UID，绝不隐含全池。 */
function accShownRows() { return accSortList(accAll.filter(s => accMatch(s))); }
if ($('accPageSize')) $('accPageSize').onchange = () => {
  accPageSize = Math.max(0, Number($('accPageSize').value) || 0);
  accPage = 1;
  try { localStorage.setItem('wb-acc-page-size', String(accPageSize)); } catch (_) { /* ignore */ }
  renderAccounts(accAll);
};
const accPageSizeEl = $('accPageSize');
if (accPageSizeEl) {
  try {
    const v = Number(localStorage.getItem('wb-acc-page-size'));
    if ([0, 50, 100, 200].includes(v)) { accPageSize = v; accPageSizeEl.value = String(v); }
  } catch (_) { /* ignore */ }
}
if ($('btnAccPrev')) $('btnAccPrev').onclick = () => { if (accPage > 1) { accPage--; renderAccounts(accAll); } };
if ($('btnAccNext')) $('btnAccNext').onclick = () => {
  const shown = accShownRows();
  const size = accPageSize > 0 ? accPageSize : Math.max(1, shown.length);
  if (accPage < Math.ceil(shown.length / size)) { accPage++; renderAccounts(accAll); }
};
if ($('accPickAll')) $('accPickAll').onchange = () => {
  const shown = accShownRows();
  const size = accPageSize > 0 ? accPageSize : Math.max(1, shown.length);
  const start = (accPage - 1) * size;
  const pageRows = shown.slice(start, start + size);
  const on = !!$('accPickAll').checked;
  for (const s of pageRows) { if (on) accSelected.add(s.uid); else accSelected.delete(s.uid); }
  syncAccSelectionUI(shown, pageRows);
  syncAccSelectionRows();
};
if ($('btnAccSelectPage')) $('btnAccSelectPage').onclick = () => {
  const shown = accShownRows();
  for (const s of shown) accSelected.add(s.uid);
  syncAccSelectionUI(shown, shown.slice(0, Math.max(1, accPageSize > 0 ? accPageSize : shown.length)));
  syncAccSelectionRows();
};
if ($('btnAccClearSelect')) $('btnAccClearSelect').onclick = () => {
  const shown = accShownRows();
  accSelected.clear();
  syncAccSelectionUI(shown, []);
  syncAccSelectionRows();
};

// syncAccSelectionRows 只更新当前页已渲染行的 checkbox 与高亮 class。勾选不是
// 表格内容变化，重建 200 行 innerHTML 会让用户每点一次都丢焦点、产生大块 DOM
// 分配。rows 选择器不存在（测试桩/空表）时静默跳过。
function syncAccSelectionRows() {
  const tb = $('accBody');
  if (!tb || typeof tb.querySelectorAll !== 'function') return;
  const rows = tb.querySelectorAll('tr');
  if (!rows || !rows.length) return;
  for (const row of Array.from(rows)) {
    const cb = row.querySelector ? row.querySelector('input[data-pick]') : null;
    if (!cb) continue;
    const uid = cb.dataset && cb.dataset.pick;
    if (!uid) continue;
    const on = accSelected.has(uid);
    if (cb.checked !== on) cb.checked = on;
    if (row.classList) row.classList.toggle('picked', on);
  }
}

async function accSelectedAction(kind) {
  const uids = Array.from(accSelected);
  if (!uids.length) return;
  const labels = { enable: '启用（解冻）', pause: '暂停选号', disable: '禁用' };
  if (!confirm('将对选中的 ' + uids.length + ' 个账号执行「' + labels[kind] + '」。确认？')) return;
  const btn = $({ enable: 'btnSelEnable', pause: 'btnSelPause', disable: 'btnSelDisable' }[kind]);
  const label = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '执行中…'; }
  try {
    if (kind === 'pause') {
      // 有界并发：最多 8 个请求同时在途。旧实现 Promise.all 对最多 2000 个 UID
      // 同时打，浏览器连接池被瞬时占满，轮询和其他操作只能排队。这里保持简单
      // worker 池，失败逐号收集，最终提示里带上可重试的 UID 数量。
      const results = [];
      let next = 0;
      const worker = async () => {
        while (next < uids.length) {
          const uid = uids[next++];
          try {
            await api('accounts/' + encodeURIComponent(uid) + '/pause', { method: 'POST' });
            results.push({ uid, ok: true });
          } catch (e) {
            results.push({ uid, ok: false, error: e.message });
          }
        }
      };
      await Promise.all(Array.from({ length: Math.min(8, uids.length) }, worker));
      const failed = results.filter(x => !x.ok);
      toast(failed.length ? '已暂停 ' + (uids.length - failed.length) + ' 个，' + failed.length + ' 个失败' : '已暂停 ' + uids.length + ' 个账号', failed.length ? 'err' : 'ok');
    } else {
      const r = await api('accounts/set_all', { method: 'POST', body: JSON.stringify({ enabled: kind === 'enable', uids }) });
      toast('已' + (kind === 'enable' ? '启用 ' : '禁用 ') + (r.changed || 0) + ' 个账号', 'ok');
    }
    invalidatePackagesCache();
    loadOverview(true);
  } catch (e) { toast(e.message, 'err'); }
  // 批量动作失败也要恢复按钮：此前只还原文案、不解除 disabled，一次网络
  // 错误后「启用/暂停/禁用」在整页生命周期内都点不动（轮询重建账号表也不会
  // 重建工具栏按钮）。
  finally { if (btn) { btn.disabled = false; btn.textContent = label; } }
}
if ($('btnSelEnable')) $('btnSelEnable').onclick = () => accSelectedAction('enable');
if ($('btnSelPause')) $('btnSelPause').onclick = () => accSelectedAction('pause');
if ($('btnSelDisable')) $('btnSelDisable').onclick = () => accSelectedAction('disable');

if ($('accBody')) $('accBody').addEventListener('click', async ev => {
  const pick = ev.target.closest && ev.target.closest('input[data-pick]');
  if (pick) {
    const uid = pick.dataset.pick;
    if (pick.checked) accSelected.add(uid); else accSelected.delete(uid);
    const shown = accShownRows();
    const size = accPageSize > 0 ? accPageSize : Math.max(1, shown.length);
    const start = (accPage - 1) * size;
    syncAccSelectionUI(shown, shown.slice(start, start + size));
    syncAccSelectionRows();
    return;
  }
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  if (b.dataset.a === 'clear-filter') {
    accFilter.q = ''; accFilter.state = ''; accFilter.realm = ''; accFilter.sort = 'default';
    if ($('accQ')) $('accQ').value = '';
    if ($('accRealm')) $('accRealm').value = '';
    if ($('accSort')) $('accSort').value = 'default';
    if ($('accState')) $('accState').querySelectorAll('button[data-state]').forEach(x => {
      x.setAttribute('aria-pressed', x.dataset.state === '' ? 'true' : 'false');
    });
    renderAccounts(accAll);
    return;
  }
  if (b.dataset.a === 'profile') {
    const s = accAll.find(x => x.uid === b.dataset.u);
    if (s) openProfile(s);
    return;
  }
  const u = b.dataset.u, a = b.dataset.a;
  const acct = (accAll || []).find(x => x && x.uid === u) || null;
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？')) return;
  // 在途键：5 秒轮询重建行时会按 busyKeys 恢复 disabled（易用性修复 5）。
  const busyKey = a + '|' + u;
  if (busyKeys.has(busyKey)) return;                 // 已在执行：忽略重复点击
  busyKeys.add(busyKey);
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      const ent = acct && acct.enterprise;
      const amount = r.credits_total === -1 ? '不限'
        : String(r.credits) + (r.credits_total > 0 ? (ent ? ' / 分配 ' : ' / ') + r.credits_total : '');
      toast((ent ? '企业额度已更新：' : '余额已更新：') + amount, 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'pause') {
      await api('accounts/' + encodeURIComponent(u) + '/pause', { method: 'POST' });
      toast('已暂停选号（签到 / 保活照常）', 'ok');
    } else if (a === 'resume') {
      await api('accounts/' + encodeURIComponent(u) + '/resume', { method: 'POST' });
      toast('已恢复选号', 'ok');
    } else if (a === 'tasks') {
      openTasks(u, acct);
    } else if (a === 'remove') {
      try {
        const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
        invalidatePackagesCache();   // 账号已删：旧 packages 快照里还挂着它（易用性修复 8）
        toast(r.removed === false ? (r.message || '移除未完成，账号仍保留') : '已移除', r.removed === false ? 'err' : 'ok');
      } catch (e) {
        // 500 表示账号已出池但凭证文件仍在：必须先失效缓存刷新表格，再提示
        // 用户手动处理文件；不能只显示一句泛化错误让表格继续挂着旧账号。
        invalidatePackagesCache();
        loadOverview(true);
        toast(e.message || '移除失败', 'err');
      }
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { busyKeys.delete(busyKey); b.disabled = false; loadOverview(true); }
});

/* 四个「全部」动作（全部签到 / 旅行巡检 / 活跃上报 / 全部保活）。
   后端对并发重复调用返回 409 already_running，所以点击前先 confirm，点击后立刻
   disabled + "执行中…"：既避免误触把全池任务又跑一遍，也让用户看得出正在执行
   （易用性修复 7）。任务在后台异步跑，无法从 overview 轮询恢复按钮状态（header
   按钮不在 renderAccounts 里），所以无论成功失败都在本轮请求返回后恢复按钮，
   否则成功后按钮永久停在「执行中…」只能刷新页面（易用性修复）。 */
function bindAllAction(id, path, confirmText, okText) {
  const el = $(id);
  if (!el) return;
  const label = el.textContent;
  el.onclick = async () => {
    if (!confirm(confirmText)) return;
    el.disabled = true;
    el.textContent = '执行中…';
    try {
      await api(path, { method: 'POST' });
      toast(okText, 'ok');
    } catch (e) {
      // 409 already_running：已有同任务在跑。文案直接透出后端原文更准确。
      toast(e.message, 'err');
    }
    // 后端闸门保证并发重复调用返回 409，前端无需长期禁用；成功/失败都恢复按钮，
    // 避免成功后永久卡在「执行中…」。
    el.disabled = false;
    el.textContent = label;
  };
}
bindAllAction('btnCheckinAll', 'checkin_all', '将对全部个人账号执行签到（企业账号无签到体系，会自动跳过；已签到的会跳过）。确认开始？', '全部签到已开始（企业账号已跳过），结果见日志');
bindAllAction('btnTravelAll', 'travel_all', '将对全部账号执行旅行巡检（含领养链路，耗时较长）。确认开始？', '旅行巡检已开始（含领养链路），结果见日志');
bindAllAction('btnActivityAll', 'activity_all', '将对全部账号执行活跃上报。确认开始？', '活跃上报已开始，结果见日志');
bindAllAction('btnKeepaliveAll', 'keepalive_all', '将对全部账号执行保活（维持会话活跃）。确认开始？', '全部保活已开始，结果见日志');
bindAllAction('btnBfCheckinAll', 'checkin_all', '将对全部个人账号执行签到（企业账号与已签到账号自动跳过）。确认开始？', '全部签到已开始，结果见活动记录');
bindAllAction('btnBfTravelAll', 'travel_all', '将对全部账号执行旅行巡检（含领养链路，耗时较长）。确认开始？', '旅行巡检已开始，结果见活动记录');
bindAllAction('btnBfActivityAll', 'activity_all', '将对全部账号执行成长活跃上报。确认开始？', '活跃上报已开始，结果见活动记录');
bindAllAction('btnBfKeepaliveAll', 'keepalive_all', '将对全部账号执行 Token 保活。确认开始？', '全部保活已开始，结果见活动记录');
bindAllAction('btnBfStreakAll', 'streak_all', '将执行连登补签、档位兑换与抽奖（未解锁档位/无次数会自动跳过）。确认开始？', '连登与抽奖已开始，结果见活动记录');
bindAllAction('btnBfDailyChatAll', 'daily_chat_all', '将对全部国际版个人账号发起一次每日活跃对话（不计入真实流量模型额度）。确认开始？', '国际版每日活跃已开始，结果见活动记录');

// 批量启停：后端逐号执行，返回 changed 数；确认文案明确区分「启用=解冻」与「禁用=判死需手动解冻」。
function bindSetAll(id, enabled, confirmText) {
  const el = $(id);
  if (!el) return;
  const label = el.textContent;
  el.onclick = async () => {
    if (!confirm(confirmText)) return;
    // 红线：accounts/set_all 只接受显式 uids，不能再把空列表解释成整池。
    // 使用最近一次 /overview 快照（页面首屏/5 秒轮询已加载）；全选语义由
    // 前端显式列出 UID 表达，接口作用域仍是 selected。
    const uids = ((overviewData && overviewData.accounts) || []).map(a => a && a.uid).filter(Boolean);
    if (!uids.length) { toast('账号列表尚未加载，请稍后重试', 'err'); return; }
    if (!confirm('将作用于当前 ' + uids.length + ' 个账号，确认继续？')) return;
    el.disabled = true; el.textContent = '执行中…';
    try {
      const r = await api('accounts/set_all', { method: 'POST', body: JSON.stringify({ enabled, uids }) });
      toast((enabled ? '已启用 ' : '已禁用 ') + (r.changed || 0) + ' 个账号', 'ok');
      invalidatePackagesCache();
      loadOverview(true);
    } catch (e) { toast(e.message, 'err'); }
    finally { el.disabled = false; el.textContent = label; }
  };
}
bindSetAll('btnAllEnable', true, '将解冻全部账号：清除人工禁用、冷却与熔断。确认启用？');
bindSetAll('btnAllDisable', false, '将禁用全部账号：它们不再接流量，保号任务也跳过，需手动解冻恢复。确认禁用？');

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* rateCell 倍率列：牌价 vs 生效价。上游 credits 是牌价（转正后基准倍率），
   modelPromotions 给当前生效折扣（限时免费 factor=0 / 夜间五折 0.5 等）——
   WorkBuddy 客户端显示的正是生效价。有折扣：生效价大字 + 标签 + 划线牌价，
   悬停带时段说明；无 factor 只有标签（错峰类）：牌价 + 标签。 */
function rateCell(m) {
  const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
    return '<span' + tip + ' style="cursor:help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span' + tip + ' style="cursor:help">' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
  }
  return m.credits ? esc(m.credits) : '—';
}

async function loadModels() {
  if (mdInFlight) { mdPending = true; return mdInFlight; }
  mdInFlight = loadModelsRun();
  try { return await mdInFlight; }
  finally {
    mdInFlight = null;
    if (mdPending) { mdPending = false; loadModels(); }
  }
}
let mdInFlight = null, mdPending = false;
async function loadModelsRun() {
  const tb = $('mdBody');
  if (!tb) { noteDOM('mdBody'); return; }
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 探测数据是可选增强：拉取失败不影响模型列表本身
    const [d, pr] = await Promise.all([api('models'), api('model_probes').catch(() => ({}))]);
    mdAll = d.models || [];
    mdProbes = pr.probes || {};
    if (!mdAll.length) {
      tb.innerHTML = '<tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>';
      setText('mdCount', '');
      setText('mdNote', '上游未返回模型');
      return;
    }
    // 探测键带域前缀（cn:glm-5.2），模型表显示裸名，按「精确命中或 :后缀」关联。
    const probeKeys = Object.keys(mdProbes);
    mdProbeOf = id => mdProbes[id] || mdProbes[probeKeys.find(k => k.endsWith(':' + id))];
    const hit = mdAll.filter(m => mdProbeOf(m.id)).length;
    setText('mdNote', mdAll.length + ' 个模型 · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : ''));
    renderModels();
  } catch (e) {
    mdAll = [];
    // 空池导致的 503 不是“模型接口坏了”，给可操作的空态，而不是上游英文原文。
    const msg = e && e.status === 503
      ? '账号池暂无可用于查询模型的账号。请先添加账号，再回到本页点击刷新。'
      : (e && e.message ? e.message : String(e));
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(msg) + '</div></td></tr>';
    setText('mdCount', '');
  }
}

/* ── 模型筛选（按条件查询）───────────────────────────────────────────
   模型目录一次拉全（几十条），筛选与排序全部在前端完成：改条件零延迟，且不会
   因为调一次筛选就打一次上游——/panel/api/models 是直连上游的实时查询，很贵。
   条件之间是 AND；每个条件为空即不参与判定。 */
// mdRateValue 当前生效的积分倍率数值：优先促销价（限时免费 = 0），无倍率记为
// Infinity 排到最后（排序时"没有价格"不该冒充最便宜）。
function mdRateValue(m) {
  const raw = (m.promo_credits != null && m.promo_credits !== '') ? m.promo_credits : m.credits;
  const n = parseFloat(String(raw == null ? '' : raw).replace(/[^\d.]/g, ''));
  return Number.isFinite(n) ? n : Infinity;
}

// mdSearchText 参与关键字搜索的字段（ID / 展示名 / 厂商 / 描述 / 标签）。
function mdSearchText(m) {
  return [m.id, m.name, m.vendor, m.description, (m.tags || []).join(' ')]
    .filter(Boolean).join(' ').toLowerCase();
}

// mdMatch 单个模型是否满足全部筛选条件。
function mdMatch(m, f) {
  f = f || mdFilter;
  if (f.q) {
    const text = mdSearchText(m);
    // 空格分词后逐个匹配：多关键词是 AND，便于"cn 视觉"这类组合查询。
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.realm && !String(m.id || '').startsWith(f.realm + ':')) return false;
  if (f.cap === 'tool' && !m.supports_tool_call) return false;
  if (f.cap === 'vision' && !m.supports_images) return false;
  if (f.cap === 'reasoning' && !m.supports_reasoning) return false;
  if (f.cap === 'default' && !m.is_default) return false;
  if (f.effort === 'off') {
    if (!m.can_disable_thinking) return false;
  } else if (f.effort && !(m.supported_efforts || []).includes(f.effort)) {
    return false;
  }
  const factor = m.promo_factor == null ? null : Number(m.promo_factor);
  if (f.promo === 'promo' && factor == null && !m.promo_label) return false;
  if (f.promo === 'free' && !(factor === 0)) return false;
  if (f.promo === 'discount' && !(factor != null && factor > 0)) return false;
  return true;
}

// mdSortList 按当前排序条件返回新数组（不改动入参，保持上游原始顺序可回溯）。
function mdSortList(list, f) {
  f = f || mdFilter;
  const out = list.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'rate') out.sort((a, b) => mdRateValue(a) - mdRateValue(b));
  else if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}

// mdRowHtml 单个模型行（纯渲染，便于独立测试）。
function mdRowHtml(m, pr) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
  const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
    : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
  // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）
  const caps = [];
  if (m.is_default) caps.push('<span class="tag ok">默认</span>');
  if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
  if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
  const capHtml = caps.length ? caps.join(' ') : '<span style="color:var(--ink-3)">—</span>';
  const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
  // Hub 的模型表把「名称 / 模型 ID / 能力」拆成独立列，窄屏再靠 data-label 卡片化；
  // 旧版把三者堆在「模型」一列，长名称会把倍率和上下文挤到换行。
  // 名称列用模型 ID 作为硬兜底；能力列独立一行，避免把倍率挤到下一行。
  return '<tr><td class="mark" aria-hidden="true"><i></i></td>' +
    '<td class="who"' + tip + ' data-label="名称"><div class="nm">' + esc(m.name || '—') + '</div>' +
      (m.vendor ? '<div class="id">' + esc(m.vendor) + '</div>' : '') + '</td>' +
    '<td class="mono" data-label="模型 ID"><div class="nm">' + esc(m.id) + '</div></td>' +
    '<td class="num" data-label="积分倍率">' + rateCell(m) + '</td>' +
    '<td data-label="能力"><div class="id" style="margin-top:0">' + capHtml + '</div></td>' +
    '<td data-label="默认档">' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
    '<td class="efs" style="white-space:normal" data-label="支持的思考档位">' + effs + '</td>' +
    '<td class="num" data-label="上下文">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
    outCell(m, pr).replace('<td class="num">', '<td class="num" data-label="最大输出">') + '</tr>';
}

function renderModels() {
  const tb = $('mdBody');
  if (!tb) { noteDOM('mdBody'); return; }
  const list = mdSortList(mdAll.filter(m => mdMatch(m)));
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="9"><div class="empty">没有符合当前筛选条件的模型</div></td></tr>';
  } else {
    tb.innerHTML = list.map(m => mdRowHtml(m, mdProbeOf(m.id))).join('');
  }
  const filtered = list.length !== mdAll.length;
  const count = $('mdCount');
  if (count) {
    count.textContent = !mdAll.length ? ''
      : filtered ? '命中 ' + list.length + ' / ' + mdAll.length + ' 个模型'
      : mdAll.length + ' 个模型';
    count.className = filtered ? 'note src-off' : 'note';
  }
}

function resetModelFilter() {
  mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
  for (const id of ['mdQ', 'mdRealm', 'mdCap', 'mdEffort', 'mdPromo']) {
    const el = $(id); if (el) el.value = '';
  }
  const sort = $('mdSort'); if (sort) sort.value = 'default';
  renderModels();
}

// 筛选控件：输入框防抖 120ms（长列表逐字符重排不必每键一次），下拉即时。
let mdQTimer = null;
if ($('mdQ')) $('mdQ').oninput = () => {
  clearTimeout(mdQTimer);
  mdQTimer = setTimeout(() => { const el = $('mdQ'); mdFilter.q = el ? el.value.trim() : ''; renderModels(); }, 120);
};
for (const [id, key] of [['mdRealm', 'realm'], ['mdCap', 'cap'], ['mdEffort', 'effort'], ['mdPromo', 'promo'], ['mdSort', 'sort']]) {
  const el = $(id);
  if (!el) continue;
  el.onchange = () => { mdFilter[key] = el.value; renderModels(); };
}
if ($('mdReset')) $('mdReset').onclick = resetModelFilter;
if ($('btnModels')) $('btnModels').onclick = loadModels;

/* ── 日志（频道：全部/任务/对话/系统 + 级别 / 关键字） ─────────────── */
let logCh = 'all';
// logLevel / logQ 是纯前端筛选：服务端只按频道归档，级别和关键字在已拉取的
// 最近 N 行上过滤即可，避免为了看一条 ERROR 重新扫盘。
let logLevel = '', logQ = '';
let logQT = null;
function logLevelOf(e) {
  const txt = String((e && e.text) || '');
  if (/\b(error|fatal|panic)\b|失败|错误/i.test(txt)) return 'ERROR';
  if (/\bwarn(ing)?\b|冷却|熔断|限流/i.test(txt)) return 'WARN';
  if (/\bdebug\b|调试/i.test(txt)) return 'DEBUG';
  return 'INFO';
}
function logEntryText(e) {
  return [e && e.ch, e && e.ts, e && e.text].filter(Boolean).join(' ').toLowerCase();
}
function logMatches(e) {
  const ch = typeof logCh === 'string' ? logCh : 'all';
  const lvl = typeof logLevel === 'string' ? logLevel : '';
  const q = typeof logQ === 'string' ? logQ : '';
  if (ch !== 'all' && (e && e.ch) !== ch) return false;
  if (lvl && logLevelOf(e) !== lvl) return false;
  if (q && !logEntryText(e).includes(q.toLowerCase())) return false;
  return true;
}
if ($('logChips')) $('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  renderLogs(lastLogEntries);
});
// 级别立即重绘；关键字防抖 120ms，输入长正文时不必每个键都重建 500 行。
const logLevelEl = $('logLevel');
if (logLevelEl) logLevelEl.onchange = () => { logLevel = logLevelEl.value; renderLogs(lastLogEntries); };
const logQEl = $('logQ');
if (logQEl) logQEl.oninput = () => {
  clearTimeout(logQT);
  logQT = setTimeout(() => { logQ = String(logQEl.value || '').trim(); renderLogs(lastLogEntries); }, 120);
};
/* logRenderHTML 是日志渲染的唯一模板。loadLogs 和本地筛选都调它：
   纯函数只依赖入参，便于 loadLogs 单函数切片测试，也避免两份 HTML 慢慢漂移。 */
function logRenderHTML(allEntries, ch, lvlFilter, q) {
  const pick = (Array.isArray(allEntries) ? allEntries : []).filter(e => {
    if (ch !== 'all' && (e && e.ch) !== ch) return false;
    if (lvlFilter && logLevelOf(e) !== lvlFilter) return false;
    if (q && !logEntryText(e).includes(String(q).toLowerCase())) return false;
    return true;
  });
  if (!pick.length) {
    return {
      html: '<span class="ln" style="color:var(--ink-3)">' +
        (Array.isArray(allEntries) && allEntries.length ? '没有符合筛选条件的日志' : '暂无日志') + '</span>',
      count: 0,
      filtered: !!(q || lvlFilter || ch !== 'all'),
    };
  }
  return {
    html: pick.map(e => {
      const lvl = logLevelOf(e);
      const t = e.ts ? fmtLogTime(e.ts) : '';
      const chLabel = ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch || '系统');
      return '<span class="ln lvl-' + lvl + '">' +
        '<i class="lt">' + esc(t) + '</i>' +
        '<i class="lv lvl-' + lvl + '">' + lvl + '</i>' +
        '<i class="lch c-' + esc(e.ch || 'sys') + '">' + esc(chLabel) + '</i>' +
        '<i class="lm">' + esc(e.text || '') + '</i>' +
        '</span>';
    }).join(''),
    count: pick.length,
    filtered: !!(q || lvlFilter || ch !== 'all'),
  };
}
function renderLogs(allEntries) {
  const box = $('logBox');
  if (!box) { if (typeof noteDOM === 'function') noteDOM('logBox'); return; }
  // 测试/旧骨架会把本函数单独求值：局部回退到模块默认值，不能因缺少
  // logCh/logLevel/logQ 就抛 ReferenceError 让整页日志停摆。
  const ch = typeof logCh === 'string' ? logCh : 'all';
  const lvlFilter = typeof logLevel === 'string' ? logLevel : '';
  const q = typeof logQ === 'string' ? logQ : '';
  const view = logRenderHTML(allEntries, ch, lvlFilter, q);
  box.innerHTML = view.html;
  if (typeof logPin === 'undefined' || logPin) box.scrollTop = box.scrollHeight;
  // 空结果只把「已筛选 · 0 行」写在工具条；不要用占位行覆盖日志区，
  // 否则用户取消筛选时会误以为原始日志已被清空。
  const note = view.filtered ? '已筛选 · ' + view.count + ' 行' : '';
  if (typeof setText === 'function') setText('logFilterNote', note);
  else { const n = $('logFilterNote'); if (n) n.textContent = note; }
}
// 最近一次成功拉取的日志，供频道/级别/关键字切换时本地重绘，不重复扫盘。
let lastLogEntries = [];
async function loadLogs(manual) {
  // 归档是多个 JSONL 的全量扫描：连点刷新或 5 秒轮询不能在慢盘上叠加多轮。
  if (logsReq) {
    if (manual && typeof logsPending !== 'undefined') logsPending = true;
    return;
  }
  logsReq = true;
  const box = $('logBox');
  if (!box) { logsReq = false; noteDOM('logBox'); return; }
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  const limit = ($('reqLimit') && $('reqLimit').value) || 100;
  // 时间范围由归档侧过滤（不是前端筛已拉取的条目）：区间落在更早的时间段时，
  // 「最近 N 条」里根本不会有那些记录，必须让服务端按时间取。
  const rq = trangeQuery('reqRange', false);
  rq.set('limit', limit);
  let failMetrics = null, failArchive = null;
  try {
    // 三块数据互不依赖：运行日志优先渲染（它最常用且最轻），指标/归档再各自
    // 收尾。此前 Promise.all 让一个慢 JSONL 归档扫描拖住整页，连运行日志都不
    // 显示——用户看到的是「日志加载失败/空白」，实际只是归档慢。现在任一失败
    // 只影响自己的区块。
    const pLogs = api('logs').then(d => ({ d }), e => ({ e }));
    const pMetrics = api('request_metrics').then(m => ({ m }), e => { failMetrics = e; return { m: {} }; });
    const pArchive = api('request_logs?' + rq.toString()).then(r => ({ r }), e => { failArchive = e; return { r: { entries: [] } }; });
    // 先挂上拒绝处理：pLogs 先行渲染时，指标/归档若已失败而尚未 await，不能
    // 让 Node/浏览器报 unhandled rejection（同一任务内两个 fetch 都失败即可复现）。
    pMetrics.catch(() => {});
    pArchive.catch(() => {});

    const lr = await pLogs;
    if (lr.e) {
      // 运行日志自己的错误态；请求指标/归档仍会独立渲染。
      box.innerHTML = '<span class="ln e">日志加载失败：' + esc(lr.e.message) + '</span>';
      setText('logNote', '加载失败');
    } else {
      const d = lr.d || {};
      lastLogEntries = d.entries || [];
      // 日志容器整体 innerHTML 替换会把正在向上翻阅的用户拉回旧位置，也产生无谓
      // 的 DOM/布局开销。签名未变（同一批行）时跳过重绘；滚动位置只在用户仍位于
      // 底部（或显式开启自动滚动）时跟随。
      const sig = lastLogEntries.map(e => [e.ts || '', e.ch || '', e.text || ''].join('\u0001')).join('\u0002');
      // 首次渲染时容器里是空内容（或占位提示），不能只凭签名跳过；用初始哨兵
      // 区分「尚未画过」与「画过同一批」。否则首屏日志会一直是空白占位。
      // 旧版单测只求值 loadLogs 这一段，不能依赖外部的 logRenderHTML /
      // logLevelOf；这里保留自包含模板，renderLogs 只服务本地筛选控件。
      if (sig !== logSig || !logSig) {
        box.innerHTML = lastLogEntries.length
        ? lastLogEntries.map(e => {
          const txt = String(e.text || '');
          const lvl = /\b(error|fatal|panic)\b|失败|错误/i.test(txt) ? 'ERROR'
            : /\bwarn(ing)?\b|冷却|熔断|限流/i.test(txt) ? 'WARN'
            : /\bdebug\b|调试/i.test(txt) ? 'DEBUG' : 'INFO';
          const t = e.ts ? fmtLogTime(e.ts) : '';
          const chLabel = ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch || '系统');
          return '<span class="ln lvl-' + lvl + '">' +
            '<i class="lt">' + esc(t) + '</i>' +
            '<i class="lv lvl-' + lvl + '">' + lvl + '</i>' +
            '<i class="lch c-' + esc(e.ch || 'sys') + '">' + esc(chLabel) + '</i>' +
            '<i class="lm">' + esc(txt) + '</i>' +
            '</span>';
        }).join('')
        : '<span style="color:var(--ink-3)">暂无日志</span>';
        logSig = sig;
        if (logPin && atEnd) box.scrollTop = box.scrollHeight;
      } else if (logPin && atEnd) {
        box.scrollTop = box.scrollHeight;
      }
      const counts = {};
      for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
      const logChCur = typeof logCh === 'string' ? logCh : 'all';
      const logLvlCur = typeof logLevel === 'string' ? logLevel : '';
      const logQCur = typeof logQ === 'string' ? logQ : '';
      // 已筛选时这里不能用 entries：它是请求指标/归档区块的局部变量，直到
      // 两个 await 之后才声明。此前一开筛选或频道就抛 ReferenceError，
      // 整页日志停摆。筛选计数应以当前日志窗口为准，且只用本作用域已有的值。
      const filtered = logChCur !== 'all' || !!logLvlCur || !!logQCur;
      const visibleCount = filtered && typeof logRenderHTML === 'function'
        ? logRenderHTML(lastLogEntries, logChCur, logLvlCur, logQCur).count
        : lastLogEntries.length;
      setText('logNote', logChCur === 'all' && !logLvlCur && !logQCur
        ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
        : visibleCount + ' 行 / 共 ' + lastLogEntries.length + ' 行');
    }

    const [mr, ar] = await Promise.all([pMetrics, pArchive]);
    const metrics = mr.m || {};
    const requestRows = ar.r || { entries: [] };
    lastLogErrors = { metrics: failMetrics, archive: failArchive };
    // 归档开启时以归档为准——「区间内没有记录」是一个真实结果，不能回落成内存里
    // 的最近 100 条（那会把筛选条件之外、时间范围之外的请求显示出来）。
    // 只有归档关闭时才回落到内存指标，保证没有归档的部署仍能看到最近请求。
    const archiveOn = !!(metrics && metrics.archive && metrics.archive.enabled);
    // unchanged=true 时服务端省略 entries 以省传输；不能把空数组覆盖掉上一轮
    // 已渲染的行，否则静置 5 秒后请求表会被清空。此时复用 lastLogRows。
    const archiveRows = (requestRows.unchanged && (requestRows.entries || []).length === 0 && lastLogRows && Array.isArray(lastLogRows.entries))
      ? lastLogRows.entries : (requestRows.entries || []);
    const recent = archiveOn ? archiveRows : (metrics.recent || []);
    lastLogMetrics = metrics;
    // 只缓存完整窗口。服务端在 unchanged=true 时会省略 entries，把这份空窗口
    // 存回 lastLogRows 会让下一轮再也找不到旧行，表格被永久清空。
    if (!requestRows.unchanged || (requestRows.entries || []).length) lastLogRows = requestRows;
    // request_logs 的 unchanged=true 表示归档目录签名未变、窗口与上一轮逐条相同。
    // 此时无需重建请求表（最多 1000 行）；时间变化仍由下面的 metrics 更新。
    renderRequestMetrics(metrics, recent, !requestRows.unchanged);
  } catch (e) {
    // 兜底：上面的分区错误已就地处理，这里只兜意外异常。失败必须可见：此前
    // catch 是空块，读取失败时日志区保持「暂无日志」，用户以为"确实没有记录"。
    box.innerHTML = '<span class="ln e">日志加载失败：' + esc(e.message) + '</span>';
    setText('logNote', '加载失败');
    lastLogErrors = { metrics: e, archive: e };
    renderRequestMetrics(lastLogMetrics, lastLogRows.entries || []);
  } finally {
    logsReq = false;
    if (typeof logsPending !== 'undefined' && logsPending) { logsPending = false; loadLogs(); }
  }
}

function renderRequestMetrics(m, entries, rowsChanged) {
  m = m || {};
  const a = m.archive || {};
  const errors = lastLogErrors || {};
  setText('reqSummary',
    '已完成 ' + fmtTok(m.completed) +
    ' · 成功 ' + (m.success_rate == null ? '—' : Number(m.success_rate).toFixed(1) + '%') +
    ' · HTTP ' + (m.http_success_rate == null ? '—' : Number(m.http_success_rate).toFixed(1) + '%') +
    ' · 平均 ' + fmtMs(m.avg_duration_ms) +
    ' · 进行中 ' + String(m.in_flight || 0));
  const parts = [];
  if (errors.metrics) parts.push('指标读取失败：' + errors.metrics.message);
  if (errors.archive) parts.push('归档读取失败：' + errors.archive.message);
  if (a.enabled) {
    parts.push('JSONL 归档 ' + fmtBytes(a.bytes) + (a.dropped_writes ? ' · 丢弃 ' + a.dropped_writes + ' 条' : '') +
      (a.last_error ? ' · 错误：' + a.last_error : ''));
  } else {
    parts.push('仅内存指标，JSONL 归档已关闭');
  }
  // 读取失败必须是错误态，不能与“归档已关闭”共用灰色说明——后者会让用户
  // 以为确实没有请求记录（P1 假成功）。
  const reqNote = $('reqNote');
  if (reqNote) {
    reqNote.textContent = parts.join(' · ');
    reqNote.className = errors.metrics || errors.archive ? 'note err' : 'note';
  }

  reqEntries = entries || [];
  // rowsChanged=false 时保留现有表格 DOM：数据完全一致，重建只会丢滚动/选中态。
  if (rowsChanged !== false) renderRequestTable();
  if (errors.metrics || errors.archive) {
    const msg = [errors.metrics && '指标读取失败', errors.archive && '归档读取失败'].filter(Boolean).join('；');
    const cur = $('reqCount');
    if (cur && !cur.textContent) { cur.textContent = msg; cur.className = 'note err'; }
  }
}

/* reqMatch 请求记录筛选：q 对 IP/UA/模型/账号/请求 ID 做空格分词的 AND 包含匹配，
   outcome 精确匹配。两者都在已拉取的条目上做（最多 1000 条），不发新请求。 */
function reqMatch(e, f) {
  f = f || reqFilter;
  if (f.outcome && String(e && e.outcome || '') !== f.outcome) return false;
  if (f.key) {
    const key = String(e && e.key_id || '');
    if (f.key === '__empty__' ? key !== '' : key !== f.key) return false;
  }
  if (f.q) {
    const text = [e && e.client_ip, e && e.user_agent, e && e.model, e && e.account, e && e.request_id]
      .filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  return true;
}

function reqOutcomeTag(e) {
  const outcome = String(e && e.outcome || '');
  const label = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' }[outcome] || outcome || '—';
  const cls = outcome === 'success' ? 'ok'
    : outcome === 'interrupted' ? 'warn'
    : outcome ? 'bad' : 'mute';
  return '<span class="tag ' + cls + '">' + esc(String(e && e.status || '—') + ' ' + label) + '</span>';
}

function reqTokenCell(e) {
  const total = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  return total ? fmtTok(total) : '—';
}

function reqCreditCell(e) {
  if (!e || !e.credit_known) return '<span class="muted">—</span>';
  const v = Number(e.credit);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '<span class="muted">—</span>';
}

/* renderRequestTable 渲染请求记录表。来源列是这一版的重点：IP 用等宽字体方便扫，
   UA 单行截断（完整值在 title 里，行本身用 requestLogText 作 tooltip）。 */
function renderRequestTable() {
  // 服务端可能返回条数相同但内容完全不同的窗口（例如时间范围切换、归档轮转）。
  // 只比较长度会让旧过滤结果被错误复用；对象身份变化必须强制失效。
  if (renderRequestTable.cache && renderRequestTable.src !== reqEntries) {
    renderRequestTable.cache = null;
  }
  renderRequestTable.src = reqEntries;
  const fsig = (reqEntries || []).length + '|' + reqFilter.q + '|' + reqFilter.outcome + '|' + reqFilter.key;
  if (!renderRequestTable.cache || fsig !== renderRequestTable.cache.sig) {
    renderRequestTable.cache = { sig: fsig, rows: reqEntries.filter(e => reqMatch(e)) };
  }
  const list = renderRequestTable.cache.rows;
  const tb = $('reqBody');
  if (!tb) return;
  const size = reqPageSize > 0 ? reqPageSize : Math.max(1, list.length);
  const pages = Math.max(1, Math.ceil(list.length / size));
  if (reqPage > pages) reqPage = pages;
  if (reqPage < 1) reqPage = 1;
  const start = (reqPage - 1) * size;
  const pageRows = list.slice(start, start + size);
  const errors = lastLogErrors || {};
  if (errors.metrics || errors.archive) {
    const msg = [errors.metrics && '指标读取失败：' + errors.metrics.message,
      errors.archive && '归档读取失败：' + errors.archive.message].filter(Boolean).join('；');
    tb.innerHTML = '<tr><td colspan="11" class="empty err">' + esc(msg) + '</td></tr>';
    if ($('reqCount')) { $('reqCount').textContent = msg; $('reqCount').className = 'note err'; }
    syncRequestKeyOptions();
    return;
  }
  tb.innerHTML = pageRows.map(e => {
    const when = e && e.time ? fmtLogTime(e.time) : '—';
    const ip = e && e.client_ip ? e.client_ip : '';
    const ua = e && e.user_agent ? e.user_agent : '';
    const rid = e && e.request_id ? e.request_id : '';
    return '<tr title="' + esc(requestLogText(e)) + '">' +
      '<td class="num" data-label="时间">' + esc(when) + '</td>' +
      '<td data-label="结果">' + reqOutcomeTag(e) + '</td>' +
      '<td data-label="Key">' + reqKeyText(e) + '</td>' +
      '<td data-label="模型">' + esc(e && e.model || '—') + '</td>' +
      '<td data-label="账号">' + esc(e && e.account || '—') + '</td>' +
      '<td data-label="来源 IP">' + (ip ? '<span class="clip ip" title="' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td data-label="User-Agent">' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="num" data-label="耗时">' + fmtMs(e && e.duration_ms) + '</td>' +
      '<td class="num" data-label="Token">' + reqTokenCell(e) + '</td>' +
      '<td class="num" data-label="积分">' + reqCreditCell(e) + '</td>' +
      '<td data-label="请求 ID">' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="11" class="empty">' +
      (reqEntries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录') + '</td></tr>';

  const filtered = list.length !== reqEntries.length;
  // 归档里的旧条目没有来源字段（该功能上线前写入）：这时提示开关/历史原因，
  // 而不是让人以为筛选坏了。
  const hasSource = reqEntries.some(e => e && (e.client_ip || e.user_agent));
  const reqCount = $('reqCount');
  if (reqCount) reqCount.textContent = !reqEntries.length ? ''
    : (filtered ? '命中 ' + list.length + ' / ' + reqEntries.length + ' 条' : reqEntries.length + ' 条') +
      (hasSource ? '' : ' · 来源未记录');
  if (reqCount) reqCount.className = (filtered || !hasSource) ? 'note src-off' : 'note';
  const page = $('reqPage');
  if (page) page.textContent = list.length ? (reqPage + ' / ' + pages) : '—';
  const prev = $('btnReqPrev'), next = $('btnReqNext');
  if (prev) prev.disabled = reqPage <= 1;
  if (next) next.disabled = reqPage >= pages;
  const sizeSel = $('reqPageSize');
  if (sizeSel) sizeSel.value = String(reqPageSize);
  syncRequestKeyOptions();
}

/* Key 列只显示归档里的 key_id（身份标签），从不读取或回显密钥原文。 */
function reqKeyText(e) {
  const key = e && e.key_id ? String(e.key_id) : '';
  return key ? '<span class="tag mute">' + esc(key) + '</span>' : '<span class="muted">—</span>';
}

/* 请求记录的 Key 下拉：从当前已加载条目去重生成，保留用户当前选择。
   不能请求 /panel/api/config 来取 key 列表——那会把管理配置耦合进日志页。 */
function syncRequestKeyOptions() {
  const sel = $('reqKey');
  if (!sel) return;
  const cur = reqFilter.key || '';
  const keys = Array.from(new Set((reqEntries || []).map(e => e && e.key_id).filter(Boolean))).sort();
  // 5 秒轮询每轮都走这里。Key 集合没变时保留现有 <option> 节点：下拉正在展开
  // 时整队清空再追加会闪断，也会让键盘焦点落回第一项。
  const sig = keys.join('\u0000') + '\u0001' + cur;
  if (sig === reqKeySig) { sel.value = cur; return; }
  reqKeySig = sig;
  // 用 DOM API 而不是 innerHTML：key_id 来自归档，属性转义已安全，但用
  // textContent 赋值能彻底避免“标签被当成 HTML”这一类回归。
  sel.textContent = '';
  const add = (value, label) => { const o = document.createElement('option'); o.value = value; o.textContent = label; sel.appendChild(o); };
  add('', '全部 Key');
  for (const k of keys) add(k, k);
  add('__empty__', '未记录 Key');
  sel.value = cur;
}

/* 请求记录筛选控件。搜索框防抖 150ms：最多 1000 行重渲染，不必每键一次。
   这段顶层绑定放在 requestLogText 之前，是为了让"纯函数切片"式前端测试
   （slice requestLogText → fmtBytes）只拿到无副作用的格式化函数。 */
let reqQTimer = null;
const reqQEl = $('reqQ');
if (reqQEl) reqQEl.oninput = () => {
  clearTimeout(reqQTimer);
  reqQTimer = setTimeout(() => { reqFilter.q = reqQEl.value.trim(); reqPage = 1; renderRequestTable(); }, 150);
};
const reqOutcomeEl = $('reqOutcome');
if (reqOutcomeEl) reqOutcomeEl.onchange = () => {
  reqFilter.outcome = reqOutcomeEl.value;
  reqPage = 1;
  renderRequestTable();
};
const reqLimitEl = $('reqLimit');
if (reqLimitEl) reqLimitEl.onchange = () => { reqPage = 1; loadLogs(); };
const reqReloadEl = $('btnReqReload');
if (reqReloadEl) reqReloadEl.onclick = () => loadLogs();
const reqKeyEl = $('reqKey');
if (reqKeyEl) reqKeyEl.onchange = () => { reqFilter.key = reqKeyEl.value; reqPage = 1; renderRequestTable(); };
const reqPageSizeEl = $('reqPageSize');
if (reqPageSizeEl) reqPageSizeEl.onchange = () => {
  reqPageSize = Math.max(0, Number(reqPageSizeEl.value) || 0);
  reqPage = 1;
  renderRequestTable();
};
if ($('btnReqPrev')) $('btnReqPrev').onclick = () => { if (reqPage > 1) { reqPage--; renderRequestTable(); } };
if ($('btnReqNext')) $('btnReqNext').onclick = () => {
  // 复用 renderRequestTable 的过滤缓存：翻页不应再次 O(n) 过滤，也不能
  // 绕过“同条数不同窗口”的缓存失效逻辑。
  renderRequestTable();
  const list = (renderRequestTable.cache && renderRequestTable.cache.rows) || [];
  const size = reqPageSize > 0 ? reqPageSize : Math.max(1, list.length);
  if (reqPage < Math.ceil(list.length / size)) { reqPage++; renderRequestTable(); }
};
/* 导出：服务端按当前时间窗/结果/Key 重新读取归档（搜索词只在本地过滤，
   导出不携带它）。按钮在下载完成前禁用，失败也要恢复原文案。 */
async function withExportBusy(btn, run) {
  const label = btn && btn.textContent ? btn.textContent : '导出';
  if (btn) { btn.disabled = true; btn.textContent = '导出中…'; }
  try {
    await run();
    toast('导出已开始', 'ok');
  } catch (e) {
    toast('导出失败：' + (e && e.message ? e.message : String(e)), 'err');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = label; }
  }
}
function downloadHiddenURL(url) {
  const a = document.createElement('a');
  a.href = url;
  a.rel = 'noopener';
  a.style.display = 'none';
  document.body.appendChild(a);
  a.click();
  a.remove();
}
function exportURL(path, params) {
  const q = params ? params.toString() : '';
  return '/panel/api/' + path + (q ? '?' + q : '');
}
function requestExportQuery() {
  const q = trangeQuery('reqRange', false);
  const limitEl = $('reqLimit');
  q.set('limit', (limitEl && limitEl.value) || 100);
  if (reqFilter.outcome) q.set('outcome', reqFilter.outcome);
  if (reqFilter.key) q.set('key_id', reqFilter.key === '__empty__' ? '' : reqFilter.key);
  return q;
}
if ($('btnReqExportCsv')) $('btnReqExportCsv').onclick = () => withExportBusy($('btnReqExportCsv'), async () => {
  const q = requestExportQuery(); q.set('format', 'csv'); downloadHiddenURL(exportURL('request_logs/export', q));
});
if ($('btnReqExportJsonl')) $('btnReqExportJsonl').onclick = () => withExportBusy($('btnReqExportJsonl'), async () => {
  const q = requestExportQuery(); q.set('format', 'jsonl'); downloadHiddenURL(exportURL('request_logs/export', q));
});
if ($('btnUsageExport')) $('btnUsageExport').onclick = () => withExportBusy($('btnUsageExport'), async () => {
  const q = trangeQuery('usRange', true); q.set('dim', usDim); downloadHiddenURL(exportURL('usage/export', q));
});
if ($('btnAccountsExport')) $('btnAccountsExport').onclick = () => withExportBusy($('btnAccountsExport'), async () => {
  downloadHiddenURL(exportURL('accounts/export'));
});
// 时间范围：默认「全部历史」——请求记录页的历史行为就是"取最近 N 条"，
// 加一个默认收窄的区间会让打开页面时看到的条数凭空变少。
if ($('reqRange')) trangeBind('reqRange', loadLogs, '0');

function requestLogText(e) {
  const when = e && e.time ? fmtLogTime(e.time) : '—';
  const outcomeLabel = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' };
  const token = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  let credit = 'credit —';
  if (e && e.credit_known) {
    const value = Number(e.credit);
    if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
  }
  return [
    when,
    String(e && e.status || '—') + ' ' + (outcomeLabel[e && e.outcome] || (e && e.outcome) || '—'),
    e && e.model || '—',
    e && e.account || '—',
    e && e.client_ip || '—',
    e && e.user_agent || '—',
    fmtMs(e && e.duration_ms),
    fmtTok(token) + ' tok',
    credit,
    cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens) === '—' ? '' : '命中 ' + cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens),
    e && e.request_id || '—',
  ].filter(Boolean).join(' | ');
}

/* fmtLogTime 渲染成含日期的 MM-DD HH:MM:SS（本地时区）。只给时分秒时跨天日志
   无法分辨先后（易用性修复 9）。放在 requestLogText 之后、fmtBytes 之前：前端
   harness 的 [requestLogText, fmtBytes) 切片含它，函数声明在切片内被提升。 */
function fmtLogTime(value) {
  const ms = typeof value === 'number' ? value : Date.parse(String(value || ''));
  if (!Number.isFinite(ms)) return '\u2014';
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
}

/* 缓存命中率纯文本（issue #92）：requestLogText 与积分表/kpi 卡共用。
   自包含（不依赖 trimFixed）：前端纯函数切片测试只截取本段。 */
function cacheRateText(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '—';
  return String(Math.round(h / total * 1000) / 10) + '%';
}

function fmtBytes(bytes) {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}
if ($('btnLogPin')) $('btnLogPin').onclick = () => {
  logPin = !logPin;
  const b = $('btnLogPin');
  if (b) b.textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  listen: ['listen'], api_key: ['api_key'],
  package_detail_limit: ['panel', 'package_detail_limit'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'], growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  blackcat_hours: ['schedule', 'blackcat_hours'], blackcat_enabled: ['schedule', 'blackcat_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  credit_floor: ['pool', 'credit_floor'],
  daily_token_limit: ['pool', 'daily_token_limit'], model_daily_token_limit: ['pool', 'model_daily_token_limit'],
  prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  background_model_names: ['background_models', 'names'], background_model_target: ['background_models', 'target'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  request_client_info: ['logging', 'request_client_info'],
  request_archive_enabled: ['logging', 'request_archive_enabled'],
  request_retention_days: ['logging', 'request_retention_days'],
  request_archive_max_mb: ['logging', 'request_archive_max_mb'],
};
/* 「覆盖型」文本字段：空串本身是有意义的取值（= 回落到内置默认），必须照发。
 *
 * 其余文本字段保持「空 = 不下发」的既有语义——那是防误清空的保护，不是 bug：
 * 表单里某个框没填，通常意味着"没改"，把它当成"请清空"会静默抹掉配置。
 *
 * 但覆盖型字段正好相反：清空 = 明确要求回到默认。漏发它们会让面板显示"已保存"
 * 而值其实没变（issue #102 附带发现 2：user_agent 清空后 config.json 里仍是旧值）。
 *
 * 刻意不含 api_key：清空它 = 关闭整个鉴权，误触代价是网关变成无鉴权公开服务。
 * 该字段（以及提示文案"留空 = 不鉴权"与现状不符的问题）单独处理。
 */
const CLEARABLE_CFG = new Set(['user_agent', 'prompt_file']);

/* ── 客户端 API Key 表格 ───────────────────────────────────────────
   GET /panel/api/config 只回显 *** + key_set；表格里留空表示保持原值，
   新增行留空则在提交前拒绝。这里绝不把掩码哨兵写进 password 输入框
   （浏览器会把它当真实值回传，且 shoulder-surfing 时无意义）。 */
const API_KEY_REALMS = [['', '不限制'], ['cn', 'CN'], ['global', 'Global']];
const MASKED_SECRET = '***';

function apiKeyEnabled(v) { return v == null ? true : !!v; }
function apiKeyModelsText(models) {
  return Array.isArray(models) ? models.join(', ') : String(models || '');
}
function apiKeyRows(list) {
  return (Array.isArray(list) ? list : []).map((row, i) => {
    const k = row || {};
    const id = String(k.id || '');
    const hasKey = !!k.key_set || (k.key && k.key !== MASKED_SECRET);
    const enabled = apiKeyEnabled(k.enabled);
    return '<tr class="ki-row" data-key-id="' + esc(id) + '" data-key-set="' + (hasKey ? '1' : '') + '">' +
      '<td class="k-act" data-label="操作"><button type="button" class="xs danger" data-ki="del" data-idx="' + i + '">删除</button></td>' +
      '<td class="k-id" data-label="ID"><input class="ki-id" value="' + esc(id) + '" placeholder="client-a" aria-label="Key ID"></td>' +
      '<td data-label="名称"><input class="ki-name" value="' + esc(k.name || '') + '" placeholder="可选" aria-label="Key 名称"></td>' +
      '<td data-label="Key"><input class="ki-key" type="password" value="" placeholder="' + (hasKey ? '留空 = 保持不变' : '至少 4 位') + '" autocomplete="new-password" aria-label="Key 密钥"></td>' +
      '<td data-label="Realm"><select class="ki-realm" aria-label="限制 Realm">' +
        API_KEY_REALMS.map(([v, label]) => '<option value="' + v + '"' + (String(k.realm || '') === v ? ' selected' : '') + '>' + label + '</option>').join('') +
      '</select></td>' +
      '<td data-label="模型白名单"><input class="ki-models" value="' + esc(apiKeyModelsText(k.models)) + '" placeholder="留空 = 不限模型" aria-label="模型白名单"></td>' +
      '<td data-label="启用"><input class="ki-enabled" type="checkbox"' + (enabled ? ' checked' : '') + ' aria-label="启用"></td>' +
    '</tr>';
  }).join('');
}

function renderAPIKeys(list) {
  const body = $('apiKeysBody');
  if (!body) return;
  body.innerHTML = apiKeyRows(list) || '<tr><td colspan="7" class="empty">还没有客户端 Key；点击「添加 Key」创建</td></tr>';
}

function apiKeyRowValues(row) {
  const val = sel => { const el = row.querySelector(sel); return el ? el.value.trim() : ''; };
  const keyEl = row.querySelector('.ki-key');
  const enabledEl = row.querySelector('.ki-enabled');
  const rawKey = keyEl ? keyEl.value.trim() : '';
  const hadKey = row.dataset.keySet === '1';
  return {
    id: val('.ki-id'), name: val('.ki-name'), realm: val('.ki-realm'),
    modelsRaw: val('.ki-models'), enabled: enabledEl ? !!enabledEl.checked : true,
    key: rawKey, hadKey,
  };
}

/* validate 返回人读错误列表；空数组 = 可提交。ID 不能为空/重复，不能占用
   顶层 api_key 的保留 id=legacy；新增条目的 key 至少 4 位（与后端同口径）。 */
function validateAPIKeyRows(rows) {
  const errs = [];
  const seen = new Set();
  for (let i = 0; i < rows.length; i++) {
    const r = rows[i];
    const label = '第 ' + (i + 1) + ' 行';
    if (!r.id) { errs.push(label + '：ID 不能为空'); continue; }
    if (r.id === 'legacy') errs.push(label + '：ID=legacy 是顶层 api_key 的保留 ID');
    if (seen.has(r.id)) errs.push(label + '：ID 重复（' + r.id + '）');
    seen.add(r.id);
    if (!/^[A-Za-z0-9._-]{1,64}$/.test(r.id)) errs.push(label + '：ID 只能使用字母、数字、. _ -');
    if (!r.hadKey && r.key.length < 4) errs.push(label + '：新 Key 至少 4 位');
    if (r.key && r.key.length < 4) errs.push(label + '：Key 至少 4 位');
    if (r.realm && r.realm !== 'cn' && r.realm !== 'global') errs.push(label + '：Realm 只能是 cn / global');
  }
  return errs;
}

function collectAPIKeys() {
  const body = $('apiKeysBody');
  if (!body || typeof body.querySelectorAll !== 'function') return [];
  const rows = Array.from(body.querySelectorAll('.ki-row')).map(apiKeyRowValues);
  const errs = validateAPIKeyRows(rows);
  if (errs.length) { const e = new Error(errs.join('；')); e.validation = true; throw e; }
  return rows.map(r => {
    const models = r.modelsRaw ? r.modelsRaw.split(/[,，\s]+/).filter(Boolean) : [];
    return {
      id: r.id, name: r.name, realm: r.realm, models,
      enabled: r.enabled,
      // 留空且已有 key → 提交哨兵并由后端保留原值；新行无 key 已在上面拒绝。
      key: r.hadKey && !r.key ? MASKED_SECRET : r.key,
    };
  });
}

function addAPIKeyRow() {
  const body = $('apiKeysBody');
  if (!body) return;
  const empty = body.querySelector('.empty');
  if (empty) empty.parentElement.remove();
  body.insertAdjacentHTML('beforeend', apiKeyRows([{ id: '', name: '', enabled: true }]));
  // 新增行不是表单 input 事件；不显式标脏会让离开配置页/刷新时静默丢改动。
  if (typeof cfgMarkDirty === 'function') cfgMarkDirty();
}
function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  if (cfgReq) return cfgReq;
  const run = (async () => {
    try {
      const d = await api('config');
      cfgLoaded = d.config;
      setText('cfgPath', d.path || '');
      const f = $('cfgForm');
      if (!f) { noteDOM('cfgForm'); return; }
      for (const [name, path] of Object.entries(CFG_MAP)) {
        const el = f.elements[name];
        if (!el) continue;
    const v = dig(cfgLoaded, path);
    // api_key 回显的是 *** 哨兵：把它塞进输入框会让「只改别的字段」的保存
    // 把真实密钥写成 ***，随后所有请求 401。留空 + *_set 标记才是保持原值。
    if (name === 'api_key') {
      el.value = '';
      el.placeholder = cfgLoaded && cfgLoaded.api_key_set ? '已设置（留空保持不变）' : '未设置';
      el.dataset.hadKey = cfgLoaded && cfgLoaded.api_key_set ? '1' : '';
      continue;
    }
        if (el.type === 'checkbox') el.checked = !!v;
        else if (Array.isArray(v)) el.value = v.join(', ');
        else el.value = v == null ? '' : v;
      }
      renderAPIKeys(cfgLoaded && cfgLoaded.api_keys);
      markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
      markHoursFields();
      setText('cfgNote', '');
      cfgDirty = false;
    } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
    finally { if (cfgReq === run) cfgReq = null; }
  })();
  cfgReq = run;
  return run;
}
let cfgReq = null;
if ($('btnAddAPIKey')) $('btnAddAPIKey').onclick = addAPIKeyRow;
const apiKeysBody = $('apiKeysBody');
if (apiKeysBody) apiKeysBody.addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ki="del"]');
  if (!b) return;
  const row = b.closest('.ki-row');
  // 删除是立即从 DOM 移除、保存后不可恢复；先确认行 ID，避免误触把生产 Key
  // 删掉后客户端集体 401。新建未保存的行（无 ID）也确认一次，语义统一。
  const idEl = row && row.querySelector ? row.querySelector('.ki-id') : null;
  const label = (idEl && String(idEl.value || '').trim()) || '这一行';
  if (typeof confirm === 'function' && !confirm('删除 API Key「' + label + '」？保存配置后该 Key 立即失效。')) return;
  if (row) row.remove();
  if (!apiKeysBody.querySelector('.ki-row')) renderAPIKeys([]);
  if (typeof cfgMarkDirty === 'function') cfgMarkDirty();
});
if (apiKeysBody) apiKeysBody.addEventListener('change', ev => {
  // 启用勾选/Realm 下拉/密钥输入都走 change；input 事件只覆盖文本输入，
  // 勾选框在部分浏览器不触发 input，离开保护必须同时听 change。
  if (ev.target && ev.target.closest && ev.target.closest('.ki-row') && typeof cfgMarkDirty === 'function') cfgMarkDirty();
});
function collectConfig() {
  const f = $('cfgForm'), out = {};
  if (!f) { noteDOM('cfgForm'); return out; }
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      // 覆盖型字段空串照发（见 CLEARABLE_CFG）；其余空 = 不下发。
      if (raw === '') v = CLEARABLE_CFG.has(name) ? '' : undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  // 数组是整体替换：无行时也必须显式提交 []，否则删除最后一条 Key 会因
  // 深合并而复活。校验失败在这里抛出，submit 处理器负责聚焦并提示。
  if ($('apiKeysBody')) out.api_keys = collectAPIKeys();
  else out.api_keys = [];
  return out;
}

/* 小时列表字段（checkin_hours / growth_hours / …）即时光校验（易用性修复 3）：
   后端只接受 0–23 的整数列表，而表单里写错（"9点"、"25"、"9,,21"）要到保存被拒
   才发现。这里与后端同口径（分隔符 [,，\s]+、每段必须是 0–23 整数），脏值就地
   标红并阻止提交。collectConfig 本身不引用本段（避免旧的纯函数切片求值缺依赖），
   只在表单 submit 与 input 事件里使用。 */
const HOURS_FIELDS = ['checkin_hours', 'growth_hours', 'travel_hours', 'activity_hours', 'blackcat_hours', 'keepalive_hours'];
const HOURS_TIP = '格式应为小时列表：9,21 或 9 21（每个 0–23 的整数）';
function hoursBad(name) {
  const form = $('cfgForm');
  const el = form && form.elements ? form.elements[name] : null;
  if (!el) return false;
  const raw = el.value.trim();
  if (raw === '') return false;                       // 空 = 沿用现值
  // 不能先 filter(Boolean)：`9,,21` 与 `9,` 会被悄悄当成合法列表，和后端
  // 解析结果不一致。先按分隔符切分，每一段都必须是 0–23 的纯整数。
  const parts = raw.split(/[,，\s]/);
  if (!parts.length) return false;
  return parts.some(p => !/^\d+$/.test(p) || Number(p) > 23);
}
function markHoursFields() {
  const form = $('cfgForm');
  if (!form || !form.elements) return;
  for (const name of HOURS_FIELDS) {
    const el = form.elements[name];
    if (!el) continue;
    const bad = hoursBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? HOURS_TIP : '';
  }
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const form = $('cfgForm');
  const el = form && form.elements ? form.elements[name] : null;
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  const form = $('cfgForm');
  if (!form || !form.elements) return;
  for (const name of DURATION_FIELDS) {
    const el = form.elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
if ($('cfgForm')) $('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
  if (HOURS_FIELDS.includes(ev.target.name)) markHoursFields();
});
/* 配置项定点查找：配置页有 40+ 字段，纯靠滚动很难定位。输入关键词后按字段名/
   label/提示文案做 AND 包含匹配，命中项标亮并滚到第一个；不隐藏任何字段
   （避免用户误以为配置项被删除），清空即恢复。 */
function cfgFindNode() {
  const input = $('cfgFind');
  if (!input) return null;
  const root = $('cfgForm');
  if (!root || typeof root.querySelectorAll !== 'function') return null;
  return { input, root, nodes: Array.from(root.querySelectorAll('label.fld, .switch')) };
}
function cfgFindRun() {
  const x = cfgFindNode();
  if (!x) return;
  const kws = x.input.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
  x.nodes.forEach(node => node.classList.remove('cfg-hit'));
  if (!kws.length) {
    if ($('cfgFindNote')) $('cfgFindNote').textContent = '';
    return;
  }
  const hits = x.nodes.filter(node => {
    const text = String(node.textContent || '').toLowerCase();
    return kws.every(k => text.includes(k));
  });
  hits.forEach(node => node.classList.add('cfg-hit'));
  if ($('cfgFindNote')) $('cfgFindNote').textContent = hits.length ? '命中 ' + hits.length + ' 项' : '没有匹配的配置项';
  if (hits.length && hits[0].scrollIntoView) hits[0].scrollIntoView({ block: 'center', behavior: 'smooth' });
}
if ($('cfgFind')) $('cfgFind').addEventListener('input', cfgFindRun);

/* 配置保存反馈：保存成功后把「已保存」保持到下一次修改，避免只闪一条 toast。
   重启项由后端返回（restart_required），逐项列在保存栏里；不重启也能继续跑，
   但用户必须知道哪些改动尚未生效。 */
function cfgMarkDirty() {
  cfgDirty = true;
  const btn = $('btnCfgSave'), note = $('cfgNote');
  if (btn) btn.textContent = '保存配置';
  if (note) { note.textContent = '有未保存的修改'; note.className = 'restart-note pending'; }
}
if ($('cfgForm')) $('cfgForm').addEventListener('input', cfgMarkDirty);
if ($('btnEye')) $('btnEye').onclick = () => {
  const el = $('cfgKey');
  if (!el) return;
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  const b = $('btnEye');
  if (b) b.textContent = show ? '隐藏' : '显示';
};
if ($('btnCfgReload')) $('btnCfgReload').onclick = loadConfig;
// 放弃修改：有脏值时先确认，避免误触把当前编辑整页丢弃。
if ($('btnCfgReload')) {
  const reloadBtn = $('btnCfgReload');
  const original = reloadBtn.onclick;
  reloadBtn.onclick = () => {
    if (cfgDirty && !confirm('放弃所有未保存的配置修改？')) return;
    cfgDirty = false;
    if (typeof original === 'function') original();
  };
}
const cfgForm = $('cfgForm');
if (cfgForm) cfgForm.onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  markHoursFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = cfgForm.elements[firstBad];
    if (el) el.focus();
    toast('「' + (el && el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  // 小时列表字段同口径前置拦截（易用性修复 3）。
  const firstBadHours = HOURS_FIELDS.find(hoursBad);
  if (firstBadHours) {
    const el = cfgForm.elements[firstBadHours];
    if (el) el.focus();
    toast('「' + (el && el.closest('.fld')?.querySelector('.lb')?.textContent || firstBadHours) + '」' + HOURS_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  if (!btn) { toast('页面缺少保存按钮，请刷新页面', 'err'); return; }
  let sent = false;
  try {
    let body;
    try { body = collectConfig(); }
    catch (e) {
      const first = e && e.validation && String(e.message).split(/[；;]/)[0];
      const target = first && $('apiKeysBody') ? $('apiKeysBody').querySelector('.ki-id') : null;
      if (target) target.focus();
      toast(e.message, 'err');
      return;
    }
    btn.disabled = true; btn.textContent = '保存中…';
    sent = true;
    const r = await api('config', { method: 'POST', body: JSON.stringify(body) });
    const need = r.restart_required || [];
    const n = need.length;
    toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    const note = $('cfgNote');
    if (note) {
      note.textContent = n ? '已保存 · 需重启：' + need.join('、') : '已保存 · 已立即生效';
      note.className = 'restart-note ' + (n ? 'saved-pending' : 'saved-ok');
    }
    const saveBtn = $('btnCfgSave');
    if (saveBtn) saveBtn.textContent = '已保存';
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const keyEl = $('cfgKey');
    const k = keyEl ? keyEl.value.trim() : '';
    if (k && k !== MASKED_SECRET) memKey = k;
    // 保存成功不立即回读：用户在保存请求期间继续输入时，回读会把新输入
    // 覆盖成服务端快照。需要重新载入时由「放弃修改」或重新进入配置页触发。
    cfgDirty = false;
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally {
    const saveBtn = $('btnCfgSave');
    if (saveBtn && (sent || saveBtn.textContent === '保存中…')) {
      saveBtn.disabled = false;
      saveBtn.textContent = '保存配置';
    }
  }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
/* setHidden 只改存在的节点，供弹窗这类旧骨架可能缺块的可选区域使用。 */
function setHidden(id, v) { const el = $(id); if (el) el.hidden = v; }
function openAdd() {
  showModal('addVeil', 'btnStartLogin');
  // 重置到登录标签
  switchAddTab('login');
  setHidden('addPick', false);
  setHidden('addLoad', true); setHidden('addReady', true);
  setHidden('addDone', true); setHidden('addErr', true);
  setHidden('importDone', true); setHidden('importErr', true);
  setHidden('btnCopyUrl', true); setHidden('btnOpenUrl', true);
  setHidden('btnStartLogin', false);
  const start = $('btnStartLogin'); if (start) start.disabled = false;
  stopPoll();
}
function switchAddTab(tab) {
  document.querySelectorAll('#addTabs .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === tab));
  const login = $('addTabLogin'), imp = $('addTabImport');
  if (login) login.hidden = tab !== 'login';
  if (imp) imp.hidden = tab !== 'import';
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
  b.onclick = () => switchAddTab(b.dataset.tab);
});
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  const start = $('btnStartLogin');
  if (start) start.disabled = true;
  setHidden('addLoad', false); setHidden('addErr', true);
  api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
    loginState = r.state;
    setText('addUrl', r.url);
    setHidden('addPick', true); // 选域锁定（会话已按该域发起）
    setHidden('addLoad', true); setHidden('addReady', false);
    setHidden('btnStartLogin', true);
    setHidden('btnCopyUrl', false); setHidden('btnOpenUrl', false);
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    setHidden('addLoad', true);
    if (start) start.disabled = false;
    setHidden('addErr', false);
    setText('addErr', e.message);
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      invalidatePackagesCache();   // 新账号入池：旧快照里没有它（易用性修复 8）
      setHidden('addReady', true);
      setHidden('addDone', false);
      setText('addDone', '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中');
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    setHidden('addReady', true);
    setHidden('addErr', false);
    setText('addErr', e.message + '（关闭后重新添加）');
  }
}
function closeAdd() { stopPoll(); loginState = null; hideModal('addVeil'); }
if ($('btnCloseAdd')) $('btnCloseAdd').onclick = closeAdd;
if ($('btnStartLogin')) $('btnStartLogin').onclick = startAddLogin;
if ($('btnOpenUrl')) $('btnOpenUrl').onclick = () => { const el = $('addUrl'); if (el) open(el.textContent, '_blank'); };
if ($('btnCopyUrl')) $('btnCopyUrl').onclick = () => {
  const el = $('addUrl');
  if (!el || !navigator.clipboard) { toast('复制失败，请手动选择复制', 'err'); return; }
  navigator.clipboard.writeText(el.textContent)
    .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));
};
/* 导入摘要：dry-run 必须明确标“预览”，不能被当成已完成。 */
function importPreviewText(d) {
  d = d || {};
  const p = d.dry_run ? '预览：' : '';
  return p + '共 ' + Number(d.total || 0) + ' 个 · 新增 ' + Number(d.added || 0) +
    ' · 更新 ' + Number(d.updated || 0) + ' · 跳过 ' + Number(d.skipped || 0) +
    ' · 无效 ' + Number(d.invalid || 0);
}
async function importCockpitFile() {
  const input = $('importFile');
  const file = input && input.files && input.files[0];
  if (!file) return;
  setHidden('importDone', true); setHidden('importErr', true);
  // 先 dry-run 只做分类，再把摘要交给用户确认；确认后才真正写入。
  const fd = new FormData();
  fd.append('file', file);
  fd.append('dry_run', 'true');
  const h = {};
  const k = memKey;
  if (k) h['Authorization'] = 'Bearer ' + k;
  try {
    let r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd, headers: h });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
    setHidden('importErr', true);
    setHidden('importDone', false);
    setText('importDone', importPreviewText(d));
    const canCommit = (d.added || 0) + (d.updated || 0) > 0;
    if (!canCommit) {
      if (d.invalid) { const el = $('importDone'); if (el) el.textContent += '；没有可写入的账号'; }
      return;
    }
    if (!confirm(importPreviewText(d) + '\n\n确认导入这些账号？')) return;
    const fd2 = new FormData();
    fd2.append('file', file);
    fd2.append('overwrite', 'true');
    r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd2, headers: h });
    const committed = await r.json();
    if (!r.ok) throw new Error(committed.error || ('HTTP ' + r.status));
    if (d.imported) invalidatePackagesCache();   // 导入即入池（易用性修复 8）
    if (committed.imported) invalidatePackagesCache();
    setText('importDone', importPreviewText(committed));
    loadOverview(true);
  } catch (e) {
    setHidden('importErr', false);
    setText('importErr', '导入失败：' + e.message);
  }
  const fileInput = $('importFile');
  if (fileInput) fileInput.value = '';
}
if ($('importFile')) $('importFile').onchange = importCockpitFile;

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
if ($('btnAdd')) $('btnAdd').onclick = openAdd;
if ($('btnRefresh')) $('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  if (b) { b.disabled = true; b.textContent = '刷新中…'; }
  try {
    await api('balance_all', { method: 'POST' });
    await loadOverview(true);
    toast('余额已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { if (b) { b.disabled = false; b.textContent = '刷新'; } }
  if (view === 'logs') loadLogs();
};

/* ── 轮询 ─────────────────────────────────────────────────────────── */
function refreshVisible() {
  // 后台标签页没有人看，继续每 5 秒拉 /overview、日志和队列只会白占连接与
  // 上游配额；回到前台时立刻补一轮，避免看到过期数据。
  if (document.visibilityState === 'hidden') return;
  if (view === 'accounts') loadOverview(true);
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') { reattachQueueView(); loadBenefits(); }
}
function start() {
  // 首轮数据延后一拍：页面脚本求值完成后立即进入轮询单飞判定，避免初始请求
  // 与用户 5 秒内的刷新互相阻塞；“刷新”按钮仍可随时手动加载。
  setTimeout(() => loadOverview(true), 0);
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
  checkAuthGate();
}
/* 页签隐藏时暂停全部轮询：除了 5 秒总轮询，任务/队列扫描和登录轮询也各有
   自己的定时器。这里不销毁它们的状态，只暂停节拍并在可见时补一拍，避免回页
   后还要等 3–5 秒才看到最新结果。 */
let hiddenPaused = false;
function onVisibilityChange() {
  if (document.visibilityState === 'hidden') {
    hiddenPaused = true;
    if (loginTimer) { clearInterval(loginTimer); loginTimer = null; }
    // 任务中心/队列/扫描各自有 0.5–3s 的定时器；隐藏页时继续请求只会白占
    // 连接与面板资源。扫描与队列本身在后端继续，回页由 reattach* 接回。
    if (queueTimer) stopQueuePolling(true);
    stopScanPolling();
    return;
  }
  if (!hiddenPaused) return;
  hiddenPaused = false;
  refreshVisible();
  if (loginState && !loginState.done && !loginTimer) loginTimer = setInterval(pollLogin, 3000);
  // 队列只在本页发起过（lastQueueSeq 非空）时接回，避免刷新页面后接管
  // 别处启动的旧队列；扫描同理由 reattachScanView 判断 running。
  if (view === 'taskscenter' && lastQueueSeq) startQueuePolling();
}
document.addEventListener('visibilitychange', onVisibilityChange);
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）',
  'school_season': '校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_1': '小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_2': '小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）',
  'Sequential_Tasks_3': '小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）',
  'Sequential_Tasks_4': '小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_5': '小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_6': '小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖',
  'Sequential_Tasks_7': '体验灵感功能（预留，疑 PC 口径）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）'
};

function openTasks(uid, account) {
  taskUID = uid;
  benefitsTaskRef = null;
  openTasks.account = account || null;
  setText('taskWho', String(uid || '').slice(0, 16));
  // 企业号没有个人成长体系。打开弹窗时就按账号属性预隐藏批量操作，避免慢响应
  // 期间按钮闪现、被误点后打出一轮注定 403 的请求；个人号仍恢复显示。
  const ent = !!(account && account.enterprise);
  ['btnTaskAcceptAll', 'btnTaskAutoAll'].forEach(id => { const b = $(id); if (b) b.hidden = ent; });
  const veil = $('taskVeil');
  // 旧骨架没有任务弹窗时，不能在这里继续 loadTasks 后对空的表格写内层 HTML；
  // 统一提示页面版本不一致，由用户刷新，避免一次缺块中断整页任务入口。
  if (!veil) { noteDOM('taskVeil'); return; }
  showModal('taskVeil', 'btnCloseTask');
  const reload = $('btnTaskReload');
  if (reload) reload.hidden = false;
  loadTasks();
}
function closeTasks() { hideModal('taskVeil'); taskUID = null; benefitsTaskRef = null; }
if ($('btnCloseTask')) $('btnCloseTask').onclick = closeTasks;

// 账号设置弹窗：备注 + 手动调度权重。只提交这两个字段；weight=1 即不干预。
let profileUID = null;
function openProfile(s) {
  if (!s) return;
  profileUID = s.uid;
  setText('profileWho', s.nickname || String(s.uid || '').slice(0, 16));
  const noteEl = $('profileNote'), weightEl = $('profileWeight');
  if (noteEl) noteEl.value = s.note || '';
  if (weightEl) weightEl.value = String(s.weight || 1);
  const err = $('profileErr');
  if (err) { err.hidden = true; err.textContent = ''; }
  const ops = $('accountOpsResult');
  if (ops) { ops.hidden = true; ops.textContent = ''; ops.className = 'state'; }
  showModal('profileVeil', 'profileNote');
}
function closeProfile() { hideModal('profileVeil'); profileUID = null; }
if ($('btnProfileClose')) $('btnProfileClose').onclick = closeProfile;

if ($('btnProfileSave')) $('btnProfileSave').onclick = async () => {
  if (!profileUID) return;
  const weightEl = $('profileWeight');
  if (!weightEl) { noteDOM('profileWeight'); return; }
  const w = Number(weightEl.value || 1);
  if (!Number.isInteger(w) || w < 1 || w > 100) {
    const err = $('profileErr');
    if (err) { err.hidden = false; err.textContent = '权重必须是 1-100 的整数'; }
    return;
  }
  const btn = $('btnProfileSave');
  if (btn) { btn.disabled = true; btn.textContent = '保存中…'; }
  try {
    const r = await api('accounts/' + encodeURIComponent(profileUID) + '/profile', {
      method: 'POST', body: JSON.stringify({ weight: w, note: ($('profileNote') || {}).value || '' })
    });
    toast(r.saved === false ? '已应用；凭证文件写入失败，请检查挂载权限' : '账号设置已保存', r.saved === false ? 'err' : 'ok');
    closeProfile();
    loadOverview(true);
  } catch (e) {
    const err = $('profileErr');
    if (err) { err.hidden = false; err.textContent = e.message; }
  }
  finally { if (btn) { btn.disabled = false; btn.textContent = '保存'; } }
};

/* 账号运维：连通性测试与昵称同步都在设置弹窗内完成，避免账号行按钮继续膨胀。
   测试失败必须显示真实原因（空回复/限流/HTTP 错误），不得提示“成功”。 */
function accountOpsBusy(btn, busyLabel, fn) {
  if (!profileUID || !btn) return;
  const label = btn.textContent;
  btn.disabled = true; btn.textContent = busyLabel;
  const box = $('accountOpsResult');
  if (box) { box.hidden = true; box.textContent = ''; box.className = 'state'; }
  Promise.resolve(fn()).catch(e => {
    if (box) {
      box.hidden = false;
      box.className = 'state err';
      box.textContent = e && e.message ? e.message : String(e);
    }
  }).finally(() => { btn.disabled = false; btn.textContent = label; });
}

if ($('btnAccountTest')) $('btnAccountTest').onclick = () => accountOpsBusy($('btnAccountTest'), '测试中…', async () => {
  const r = await api('accounts/' + encodeURIComponent(profileUID) + '/test', { method: 'POST' });
  const box = $('accountOpsResult');
  if (!box) return;
  box.hidden = false;
  if (r.ok) {
    box.className = 'state ok';
    box.textContent = '连接正常：' + (r.model || '') + ' 用时 ' + (r.elapsed_ms || 0) + 'ms，回复「' + (r.reply || '') + '」';
    loadOverview(true);
  } else {
    box.className = 'state err';
    const suffix = r.kind ? '（' + r.kind + '）' : '';
    box.textContent = '连接失败' + suffix + '：' + (r.error || '未知错误');
    loadOverview(true);
  }
});

if ($('btnAccountSyncProfile')) $('btnAccountSyncProfile').onclick = () => accountOpsBusy($('btnAccountSyncProfile'), '同步中…', async () => {
  const r = await api('accounts/' + encodeURIComponent(profileUID) + '/sync_profile', { method: 'POST' });
  const box = $('accountOpsResult');
  if (box) {
    box.hidden = false;
    box.className = 'state ok';
    box.textContent = r.updated ? '昵称已更新为「' + (r.nickname || '') + '」' : '昵称未变化：「' + (r.nickname || '') + '」';
  }
    if ($('profileWho')) $('profileWho').textContent = r.nickname || $('profileWho').textContent || '';
  loadOverview(true);
});
if ($('btnTaskReload')) $('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
if ($('btnTaskAcceptAll')) $('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID && !benefitsTaskRef) return;
  const btn = $('btnTaskAcceptAll');
  if (!btn) return;
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const path = taskUID
      ? 'accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all'
      : benefitsTaskPath('accept_all');
    const r = await api(path, { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
if ($('btnTaskAutoAll')) $('btnTaskAutoAll').onclick = async () => {
  if (!taskUID && !benefitsTaskRef) return;
  const btn = $('btnTaskAutoAll');
  if (!btn) return;
  if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟（含真实对话），确认继续？')) return;
  btn.disabled = true; btn.textContent = '执行中…';
  try {
    const path = taskUID
      ? 'accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all'
      : benefitsTaskPath('auto_all');
    const r = await api(path, { method: 'POST' });
    const okN = (r.results || []).filter(x => x.status === 'done').length;
    const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
    const errN = (r.results || []).filter(x => x.status === 'error').length;
    toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '一键完成可自动任务'; loadTasks(); }
};

async function loadTasks() {
  if (!taskUID) return;
  // 请求序号守卫：快速切换账号/重复刷新时，先发的慢响应可能后到，把新账号的
  // 任务表覆盖回旧账号的（易用性修复 2）。
  const my = ++tasksSeq;
  const st = $('taskState'), tb = $('taskTable');
  if (!st || !tb) { noteDOM(!st ? 'taskState' : 'taskTable'); return; }
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  try {
    const path = taskUID
      ? 'accounts/' + encodeURIComponent(taskUID) + '/tasks'
      : benefitsTaskPath('');
    const d = await api(path);
    if (my !== tasksSeq) return;
    const list = d.tasks || [];
    const enterprise = !!d.enterprise || !!(openTasks.account && openTasks.account.enterprise);
    const acts = ['btnTaskAcceptAll', 'btnTaskAutoAll'];
    acts.forEach(id => { const b = $(id); if (b) b.hidden = enterprise; });
    if (!list.length) {
      st.className = 'state';
      // 企业号没有个人成长体系：后端会返回空列表与 enterprise=true。这里明确
      // 说明原因，避免用户把“空任务”误以为接口坏了；同时隐藏对它无意义的操作。
      st.textContent = enterprise
        ? (d.message || '企业账号无个人成长任务体系，请用「额度」刷新企业已分配额度')
        : '该账号暂无任务';
      return;
    }
    if (enterprise) {
      // 代理/旧缓存可能返回非空任务列表；企业属性仍以打开弹窗时的账号快照为准。
      st.hidden = true;
      tb.hidden = true;
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    if (!setHTML('taskBody', taskRowsHTML(list))) return;
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    if (my !== tasksSeq) return;
    st.className = 'state err';
    st.textContent = e.message;
  }
}

function taskRowsHTML(list) {
  return (Array.isArray(list) ? list : []).map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push('+' + t.credit + ' 分');
      if (t.energy) parts.push('+' + t.energy + ' 能');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' ') : '—';
      const badge = t.claimed ? '<span class="tag ok">已领取</span>'
        : t.claimable ? '<span class="tag warn">可领取</span>'
        : t.locked ? '<span class="tag mute">未解锁</span>'
        : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
        : '<span class="tag mute">未接受</span>';
      const acted = t.claimed || t.locked ? ''
        : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
        : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
        : t.accept_status === 'accepted' ? ''
        : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>';
  }).join('');
}

if ($('taskBody')) $('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || (!taskUID && !benefitsTaskRef)) return;
  const kind = b.dataset.t, code = b.dataset.c;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const path = taskUID
        ? 'accounts/' + encodeURIComponent(taskUID) + '/tasks/auto'
        : benefitsTaskPath('auto');
      const r = await api(path, {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      loadOverview(true);
    } else {
      const path = taskUID
        ? 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept')
        : benefitsTaskPath(kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') loadOverview(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});

/* ── 任务中心：开学季 + 全账号扫描/队列 ──────────────────────────── */
// 开学季任务单元：✓ 已领（绿）｜◐ x/y 进行中（琥珀）｜○ 未做（灰）
function staskHTML(t) {
  if (!t) return '<span class="stask todo"><span class="mark">·</span>—</span>';
  if (t.status === 'claimed') return '<span class="stask ok"><span class="mark">✓</span>已领</span>';
  if (t.status === 'completed') return '<span class="stask warn"><span class="mark">◆</span>可领</span>';
  if (t.status === 'in_progress') {
    const fr = t.target_count ? '<span class="fr">' + t.progress + '/' + t.target_count + '</span>' : '';
    return '<span class="stask warn"><span class="mark">◐</span>' + fr + '</span>';
  }
  return '<span class="stask todo"><span class="mark">○</span>未做</span>';
}
/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证（强制 byte
   模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性；券码为短文本，v1-5（26 字节起）绰绰有余。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);


  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  if (!body) { noteDOM('vcBody'); return; }
  showModal('vcVeil', 'btnVcClose');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  if ($('vcNote')) $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">🎟️</div>还没有抽到券</div>';
    if ($('vcNote')) $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note" style="color:var(--warn);margin-top:8px">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(e.message) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
  }
}
if ($('btnSchoolVouchers')) $('btnSchoolVouchers').onclick = loadSchoolVouchers;
if ($('btnVcClose')) $('btnVcClose').onclick = () => hideModal('vcVeil');

/* Esc 关闭最上层弹窗（易用性修复 10）。「最上层」= DOM 里最后一个可见的 .veil
   （同一 z-index 下后写者覆盖先写者）。data-lock="1" 的是鉴权网关（keyVeil），
   关掉它等于绕过输入密钥，必须跳过。关闭走各弹窗自己的清理函数，避免留下轮询
   定时器或残留的账号上下文。 */
document.addEventListener('keydown', ev => {
  if (ev.key !== 'Escape') return;
  const shown = document.querySelectorAll('.veil.on');
  if (!shown.length) return;
  const top = shown[shown.length - 1];
  if (top.dataset && top.dataset.lock === '1') return;
  ev.preventDefault();
  if (top.id === 'taskVeil') closeTasks();
  else if (top.id === 'addVeil') closeAdd();
  else hideModal(top.id);
});
if ($('btnVcRefresh')) $('btnVcRefresh').onclick = loadSchoolVouchers;

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0, queuePolling = false;
// queuePollSeq 是队列轮询代次：停止轮询（切页/又启动一轮/点击扫描）时递增。
// setTimeout 链发出的旧请求晚到时据此丢弃，不能把扫描结果或新队列状态覆盖。
let queuePollSeq = 0;
let queueReqInFlight = false;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）
// 扫描是后台任务：POST 只负责启动，随后每 500ms 拉 scan_status。
// 账号多时一次扫描可能 2 分钟（470 号 × 2 个上游接口 / 并发 3），同步请求
// 期间前端没有任何反馈，用户会以为卡死。进度直接写在按钮上。
let scanTimer = null;
// scanPollSeq 给轮询代次：离开任务中心会让在途请求的响应过期；若它晚于
// reattachScanView 返回，不能把刚恢复的按钮又改回「扫描中…」。
let scanPollSeq = 0;
// scanReqSeq 标记当前真正在途的 scan_status 请求代次（0 = 无在途请求）。
// 用一个独立的请求代次而不是布尔值：旧代请求的 finally 不能清掉新代请求的
// 在途标记，否则 5 秒刷新会把请求叠加起来。
let scanReqSeq = 0;
// lastScanRenderSeq 记录本页已渲染的扫描结果代次：reattach 每 5 秒访问
// scan_status 时，同一结果只渲染一次，避免滚动位置丢失和重复 toast。
let lastScanRenderSeq = null;
// 扫描结果 → 分组条目（无执行状态）
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
function renderScanResult(d) {
  // 后端从未扫描过时也会返回 running=false 的零值快照（accounts 为 nil）。
  // 此时不能渲染「全部完成」，否则首次进入任务中心就误报今日无待办。
  if (!d || !Array.isArray(d.accounts)) {
    renderQueue([], null);
    setText('qcSummary', '');
    const failBox = $('qcScanFail');
    if (failBox) { failBox.hidden = true; failBox.textContent = ''; }
    return;
  }
  const groups = groupItems(d);
  // error_count>0：本次扫描有账号查不到任务（上游超时/风控/mp 列表失败）。
  // 后端契约（panel/taskcenter.go）：pending_count==0 && error_count==0 才是
  // 「全部完成」；有查询失败时必须区分——否则把「没查到」显示成「全做完了」，
  // 用户以为今日无待办、实际是漏扫。
  const errN = Number(d.error_count || 0);
  const failBox = $('qcScanFail');
  if (failBox) {
    const failed = (d.accounts || []).filter(a => a && a.growth_error);
    if (failed.length) {
      failBox.hidden = false;
      failBox.textContent = '以下账号任务查询失败：' + failed.map(a =>
        (a.nickname || String(a.uid || '').slice(0, 8) || '未知账号') +
        (a.uid && a.nickname ? '（' + String(a.uid).slice(0, 8) + '）' : '') +
        '：' + String(a.growth_error || '未知错误').slice(0, 160)).join('；');
    } else {
      failBox.hidden = true;
      failBox.textContent = '';
    }
  }
  if (!groups.length && errN > 0) {
    renderQueue([], null, '扫描不完整（' + errN + ' 个账号查询失败）',
      '这些账号的成长任务没拉到（上游超时/风控等），不代表没有待办。请稍后重新扫描。');
    toast(errN + ' 个账号的任务查询失败，结果可能不完整', 'err');
  } else {
    renderQueue(groups, null, '没有待办任务 🎉', '全部账号的成长任务与开学季活动都已完成，明日再来。');
    if (errN > 0) toast(errN + ' 个账号的任务查询失败，结果可能不完整', 'err');
  }
}
// setScanProgress 统一更新常驻进度条。扫描进行中展示 done/total、当前账号与
// 耗时；进度文本必须常驻在页面上（不能只放按钮 title，触屏无法悬停）。
function setScanProgress(d, running) {
  const prog = $('qProg');
  const bar = $('qBarFill');
  const text = $('qProgText');
  if (!prog) return;
  prog.hidden = !running;
  if (!running) return;
  const done = Number(d.done || 0), total = Number(d.total || 0);
  const pct = total ? Math.min(100, Math.max(0, done / total * 100)) : 0;
  if (bar && bar.style) bar.style.width = pct.toFixed(1) + '%';
  if (!text) return;
  const cur = String(d.current_uid || '');
  const sec = Number(d.elapsed_sec || 0);
  text.textContent = (total ? '扫描 ' + done + ' / ' + total : '正在扫描') +
    (cur ? ' · 当前 ' + cur : '') + (sec ? ' · 已用 ' + sec + ' 秒' : '');
}
function stopScanPolling() {
  scanPollSeq++;
  scanReqSeq = 0;
  if (scanTimer) { clearTimeout(scanTimer); scanTimer = null; }
}
// startScanPolling 接上一个正在后台执行的扫描（首次点击或离开后回到任务中心）。
// POST 只负责启动；真正的进度与结果都经 scan_status 拉取。
function startScanPolling() {
  const b = $('btnScanAll');
  if (!b) return;
  // 已有排队中的轮询链时不要重复开链；在途请求可能是上一代（已 stop），
  // 允许新代次继续，旧响应会由 scanPollSeq 丢弃。
  if (scanTimer) return;
  b.disabled = true; b.textContent = '扫描中…';
  stopScanPolling();
  const seq = scanPollSeq;
  const req = ++scanReqSeq;
  // 首帧同步给出进度条占位，网络请求返回前也不会看起来毫无反应；随后
  // poll 立即拉取真实 done/total。
  setScanProgress({}, true);
  const poll = async () => {
    let d;
    try { d = await api('tasks/scan_status'); }
    catch (e) {
      if (seq !== scanPollSeq) return;
      if (scanTimer) { clearTimeout(scanTimer); scanTimer = null; }
      b.disabled = false; b.textContent = '扫描待办';
      setScanProgress({}, false);
      toast(e.message, 'err');
      return;
    }
    if (seq !== scanPollSeq) return;
    if (req !== scanReqSeq) return;
    if (d.running) {
      const done = Number(d.done || 0), total = Number(d.total || 0);
      b.textContent = total ? '扫描中 ' + done + '/' + total : '扫描中…';
      setScanProgress(d, true);
      // 当前账号与耗时同时写进按钮 title，鼠标用户也能快速看到；常驻展示
      // 仍由 setScanProgress 负责（触屏 hover 不可用）。
      const cur = String(d.current_uid || '');
      const sec = Number(d.elapsed_sec || 0);
      b.title = (cur ? '正在读取 ' + cur + ' · ' : '') + (sec ? '已用 ' + sec + ' 秒' : '');
      // setTimeout 链而不是 setInterval：上一次请求没回来就不发下一次，
      // 避免慢网络下扫描状态请求叠加。
      if (view === 'taskscenter') scanTimer = setTimeout(poll, 500);
      else { scanTimer = null; b.disabled = false; b.textContent = '扫描待办'; }
      return;
    }
    stopScanPolling();
    b.disabled = false; b.textContent = '扫描待办';
    setScanProgress(d, false);
    if (d.error) { toast('扫描失败：' + d.error, 'err'); return; }
    renderScanResult(d);
  };
  // 首次状态立即拉取：POST 返回后进度条不应再空等 500ms；后续仍由
  // setTimeout 链串行调度，避免慢请求叠加。首个 tick 仍交给调度器，
  // 便于测试与生产共用同一条生命周期。
  scanTimer = setTimeout(poll, 500);
}
// reattachScanView 回到任务中心时接回正在跑的扫描；已结束则恢复按钮，
// 不主动发 scan_all（那会启动一轮新扫描，违背用户只是切页的预期）。
function reattachScanView() {
  // scanTimer 代表轮询链已经在续跑；由它自己负责。scanReqSeq 表示已有
  // scan_status 请求在途（可能是另一个 reattach）——此时拒绝重复请求，避免
  // 5 秒刷新与用户切页叠加。
  if (scanTimer || scanReqSeq) return;
  const seq = scanPollSeq;
  const req = ++scanReqSeq;
  (async () => {
    try {
      const d = await api('tasks/scan_status');
      // 请求返回前用户可能已经切走：此时代次可能还没变（go() 已 stop，
      // 但 await 前还没执行到），必须再校验视图，不能重新起轮询。
      if (seq !== scanPollSeq || req !== scanReqSeq || view !== 'taskscenter') return;
      if (d.running) {
        setScanProgress(d, true);
        startScanPolling();
      } else {
        setScanProgress(d, false);
        const b = $('btnScanAll');
        if (b) { b.disabled = false; b.textContent = '扫描待办'; }
        // 已完成的扫描结果仍保存在后端；切回任务中心应展示它，而不是只剩按钮。
        // 只渲染有 accounts 的终态，避免把「从未扫描」当成「零待办」。
        if (Array.isArray(d.accounts) && d.seq !== lastScanRenderSeq) {
          renderScanResult(d);
          lastScanRenderSeq = d.seq;
        }
      }
    } catch (e) { /* 静默：轮询/手动点击会再次暴露错误 */ }
    finally {
      if (req === scanReqSeq) scanReqSeq = 0;
      if (seq === scanPollSeq && !scanTimer && view === 'taskscenter') {
        const b = $('btnScanAll');
        if (b) { b.disabled = false; b.textContent = '扫描待办'; }
      }
    }
  })();
}
if ($('btnScanAll')) $('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  if (!b) return;
  // 停掉队列轮询：显式扫描 = 切到待办视图。否则在途队列的下一 tick 会把扫描
  // 结果冲掉重渲染回队列视图（服务端执行不受影响，只是不再实时回写本视图）。
  stopQueuePolling(true);
  // 手动扫描是新一轮：先清掉旧轮询/在途标记，避免旧请求返回后覆盖新结果。
  stopScanPolling();
  b.disabled = true; b.textContent = '扫描中…';
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    if (!d.running) {
      // 后端判定已在扫描：直接接上现有进度，不重复启动。
      startScanPolling();
      return;
    }
  } catch (e) {
    b.disabled = false; b.textContent = '扫描待办';
    toast(e.message, 'err');
    return;
  }
  startScanPolling();
};
if ($('btnRunQueue')) $('btnRunQueue').onclick = async () => {
  const concEl = $('qcConc') || $('bfQcConc');
  const conc = Number(concEl && concEl.value) || 1;
  if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
  const b = $('btnRunQueue');
  if (!b) { toast('页面缺少队列按钮，请刷新页面', 'err'); return; }
  b.disabled = true; b.textContent = '启动中…';
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) {
      const msg = r.message || '没有待办任务';
      toast(msg, /失败|不完整|不可用|超时/.test(msg) ? 'err' : 'warn');
      setQueueRunBusy(false);
      return;
    }
    lastQueueSeq = r.seq || 0;
    // 异步预扫阶段后端返回 total=0；不要谎报「队列已启动：0 项」。
    toast(r.total > 0 ? '队列已启动：' + r.total + ' 项（并发 ' + conc + '）'
      : '正在预扫账号任务，完成后自动开始执行（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { if (!queueTimer) setQueueRunBusy(false); }
};
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
function qrowHTML(it) {
  const isSchool = it.kind === 'school';
  const title = isSchool ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  // 失败原因不能只靠原生 title（触屏无 hover、键盘不可达）：给消息单元格加
  // tabindex，聚焦后 CSS 展开完整文本，同时保留 title 供鼠标悬停。
  const msgAttr = it.message ? ' tabindex="0" role="note" title="' + esc(it.message) + '"' : '';
  return '<div class="qrow">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">开学季</span>' : '') + '</span>' +
    '<span class="prog">' + esc(it.prog || '') + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg"' + msgAttr + '>' + esc(it.message || '') + '</span>' +
    '</div>';
}
// 队列行签名只包含 qrowHTML 会展示的字段；3 秒轮询里大部分行都不变，
// 不必整块重建数百个 .qrow（会反复丢滚动/焦点，也产生大块 DOM 分配）。
function queueSig(groups) {
  const parts = [];
  for (const g of groups) {
    parts.push(g.uid, g.nick || '', g.rows.length);
    for (const it of g.rows) parts.push(it.kind, it.code, it.prog || '', it.status, it.message || '');
  }
  return parts.join('\x1f');
}
let queueRenderSig = '';
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  groups = Array.isArray(groups) ? groups : [];
  const empty = $('tcEmpty'), list = $('qcList');
  if (!groups.length) {
    if (empty) empty.style.display = '';
    if (emptyTitle) {
      const t = empty && empty.querySelector ? empty.querySelector('.t') : null;
      if (t) t.textContent = emptyTitle;
    }
    if (emptyDesc) {
      const d = empty && empty.querySelector ? empty.querySelector('.d') : null;
      if (d) d.textContent = emptyDesc;
    }
    if (list) list.innerHTML = '';
    const prog = $('qProg'); if (prog) prog.hidden = true;
    setText('qcSummary', '');
    queueRenderSig = '';
    return;
  }
  if (empty) empty.style.display = 'none';
  const sig = queueSig(groups);
  const total = groups.reduce((n, g) => n + g.rows.length, 0);
  if (list && (sig !== queueRenderSig || !list.childNodes || !list.childNodes.length)) {
    list.innerHTML = groups.map(g => {
      return '<div class="qgroup"><header><span class="nm">' + esc(g.nick || String(g.uid || '').slice(0, 12)) + '</span><span class="cnt">' + g.rows.length + ' 项待办</span></header>' +
        g.rows.map(qrowHTML).join('') + '</div>';
    }).join('');
    queueRenderSig = sig;
  }
  setText('qcSummary', total + ' 项');
  updateProgress(progress);
}
// setQueueRunBusy 统一维护「执行全部待办」的禁用态。队列是后台任务：
// 轮询终态、轮询异常、切回页面接管都要能恢复，否则按钮会永久卡在
// 「执行中…」——账号表 busyKeys 解决过同类问题，这里用同一个思路。
function setQueueRunBusy(busy) {
  const b = $('btnRunQueue');
  if (!b) return;
  b.disabled = busy;
  b.textContent = busy ? '执行中…' : '执行全部待办';
}
function updateProgress(q) {
  const prog = $('qProg');
  if (!q) { if (prog) prog.hidden = true; return; }
  // 执行队列的预扫阶段：items 尚未产生，进度来自 pre_done/pre_total。
  // 旧实现看到空 items 就直接隐藏进度条，用户会误以为扫描卡住。
  if (q.phase === 'scan' && Number(q.pre_total || 0) > 0) {
    const pd = Number(q.pre_done || 0), pt = Number(q.pre_total || 0);
    if (prog) prog.hidden = false;
    const fill = $('qBarFill');
    if (fill && fill.style) fill.style.width = Math.min(100, Math.round(pd / pt * 100)) + '%';
    const progText = $('qProgText');
    if (progText) progText.textContent = '正在扫描账号 ' + pd + ' / ' + pt + '（读取任务列表）' +
      (q.pre_warn ? ' · ' + q.pre_warn : '');
    return;
  }
  const items = Array.isArray(q.items) ? q.items : null;
  if (!items) { if (prog) prog.hidden = true; return; }
  const total = items.length;
  const ok = items.filter(it => it.status === 'done').length;
  const bad = items.filter(it => it.status === 'error' || it.status === 'skipped').length;
  const finished = ok + bad;
  if (prog) prog.hidden = false;
  const fill = $('qBarFill');
  if (fill && fill.style) fill.style.width = (total ? Math.round(finished / total * 100) : 0) + '%';
  const progText = $('qProgText');
  if (!progText) return;
  progText.textContent = (q.running ? '执行中 ' : '已结束 ') +
    finished + ' / ' + total + (bad ? '（成功 ' + ok + '，失败/跳过 ' + bad + '）' : '') +
    (q.pre_warn ? ' · ' + q.pre_warn : '');
  // 部分账号预扫失败时队列仍会启动；持续显示漏扫警告，避免把“已启动”
  // 误读成“所有账号都已纳入”。空态/终态 toast 仍由调用方负责。
  if (q.pre_warn) {
    progText.dataset = progText.dataset || {};
    progText.dataset.warn = '1';
  } else if (progText.dataset) {
    delete progText.dataset.warn;
  }
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code,
      prog: it.progress || '—',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
function stopQueuePolling(silent) {
  queuePollSeq++;
  queueReqInFlight = false;
  if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
  queuePolling = false;
  if (!silent) setQueueRunBusy(false);
}
function startQueuePolling() {
  stopQueuePolling(true);
  const seq = queuePollSeq;
  setQueueRunBusy(true);
  queuePolling = true;
  const poll = async () => {
    // 上一次队列查询还没回来就跳过这一拍：慢网络下旧响应晚于新响应会把进度
    // 回退。setInterval 仅作节拍器，实际请求由 queueReqInFlight 单飞。
    if (seq !== queuePollSeq || queueReqInFlight) return;
    queueReqInFlight = true;
    let q;
    try { q = await api('tasks/queue'); }
    catch (e) {
      queueReqInFlight = false;
      if (seq !== queuePollSeq) return;
      stopQueuePolling();
      toast('队列状态查询失败：' + e.message, 'err');
      return;
    }
    queueReqInFlight = false;
    if (seq !== queuePollSeq) return;
    // 只渲染本页启动过的那轮队列（刷新页面后不再接管旧队列）。
    if (lastQueueSeq && q.seq !== lastQueueSeq) {
      // 别页/上一轮队列接管了状态：停止本轮轮询并恢复按钮，不能留下永不
      // 停止的定时器（页签切回时会持续叠加请求）。
      stopQueuePolling();
      return;
    }
    if (view !== 'taskscenter' && q.running) {
      // 队列在后端继续跑，但隐藏的任务中心没有逐拍重绘的必要；定时器继续，
      // 回视图后下一次拍自然恢复渲染。终态仍要在下面处理一次（恢复按钮、提示）。
      return;
    }
    if (q.running) {
      const groups = groupsFromQueue(q.items || []);
      // 预扫阶段还没有 items：用显式进行中文案而不是“没有待办”，并让进度条
      // 继续显示 pre_done/pre_total；renderQueue 的空列表分支会隐藏进度条，
      // 所以在它之后显式恢复。
      if (q.phase === 'scan' && !groups.length) {
        renderQueue([], null, '正在扫描账号任务…',
          '并发读取每个账号的成长任务，完成后会列出待办；期间可继续查看进度。');
        updateProgress(q);
      } else {
        renderQueue(groups, q);
      }
      return;
    }
    // 结束：终态只渲染这一次，随即停表。此后残留的 items（running=false）不再
    // 回写视图——曾把用户刚点开的「扫描待办」结果在下一个 tick 冲掉。
    renderQueue(groupsFromQueue(q.items || []), q);
    stopQueuePolling();
    // 预扫失败/无待办等终态带 message：必须显示失败原因，不能只报“执行结束”。
    if (q.message) toast(q.message, 'err');
    else toast('任务队列执行结束', 'ok');
  };
  queueTimer = setInterval(poll, 3000);
  poll();
}
// reattachQueueView 切回任务中心视图时恢复队列进度：仅当本页启动的队列仍在
// 执行才重新开轮询（残留态/别页队列不接管——视图不被旧结果冲掉）。
function reattachQueueView() {
  // 5 秒刷新会在队列查询未返回时再次调用；单飞复用同一个在途查询，避免叠加。
  // 全程异步：go() 在顶层被调用时文件下方 let/const 尚未初始化，await 之后才碰。
  // 每次调用先同步同步在途标记：harness 直接调用该函数时，两个 () 之间没有
  // 微任务间隙，异步体内部才置位会让多次调用叠加。
  if (typeof queueReattachReq === 'undefined') globalThis.queueReattachReq = false;
  if (queueReattachReq) { reattachScanView(); return; }
  queueReattachReq = true;
  (async () => {
    try {
      const q = await api('tasks/queue');
      if (queueTimer || queueReqInFlight) return; // 轮询/请求已在途
      if (q.started && q.running && (!lastQueueSeq || q.seq === lastQueueSeq)) startQueuePolling();
      else setQueueRunBusy(false);
    } catch (e) { /* 静默 */ }
    finally { queueReattachReq = false; }
  })();
  reattachScanView();
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */
/* 性能分位与积分流水各自带独立取消器：手动刷新只作废自己的旧请求，切时间窗
   时由 loadUsage 统一作废两者，避免 /usage 尚未返回时旧子区块先回写。 */
function perfQueryFromUsage() {
  return trangeQuery('usRange', true);
}
function perfPct(block) {
  if (!block) return { p50: '—', p90: '—', p99: '—', max: '—' };
  const ms = v => v == null ? '—' : Math.round(Number(v)) + 'ms';
  return { p50: ms(block.p50), p90: ms(block.p90), p99: ms(block.p99), max: ms(block.max) };
}
function renderPerfStats(d) {
  d = d || {};
  const models = Object.keys(d.by_model || {}).sort();
  const empty = !Number(d.sampled || 0) || !models.length;
  const table = $('usPerfTable');
  const box = $('usPerfEmpty');
  if (table) table.hidden = empty;
  if (box) box.hidden = !empty;
  if (empty) {
    if (box) box.textContent = '当前窗口暂无请求归档；确认 logging.request_archive_enabled 已开启。';
    const body = $('usPerfBody');
    if (body) body.innerHTML = '';
    const note = $('usPerfNote');
    if (note) note.textContent = '0 个样本';
    return;
  }
  const body = $('usPerfBody');
  if (body) body.innerHTML = models.map(model => {
    const agg = d.by_model[model] || {};
    const wall = perfPct(agg.wall_ms);
    const ttft = perfPct(agg.ttft_ms);
    const tps = agg.tokens_per_sec && agg.tokens_per_sec.p50 != null
      ? String(Math.round(Number(agg.tokens_per_sec.p50) * 10) / 10) + ' tok/s' : '—';
    return '<tr><td>' + esc(model) + '</td><td class="num">' + fmtTok(agg.requests) + '</td>' +
      '<td class="num">' + esc(wall.p50) + '</td><td class="num">' + esc(wall.p90) + '</td><td class="num">' + esc(wall.p99) + '</td>' +
      '<td class="num">' + esc(ttft.p50) + '</td><td class="num">' + esc(ttft.p99) + '</td><td class="num">' + esc(tps) + '</td></tr>';
  }).join('');
  const note = $('usPerfNote');
  if (note) note.textContent = fmtTok(d.sampled) + ' 个样本' +
    (d.sample_capped ? ' · 已达 ' + fmtTok(d.sample_limit) + ' 条上限，结果已截断' : '') +
    ' · ' + models.length + ' 个模型';
}
async function loadPerf() {
  const my = ++perfSeq;
  if (perfCtrl) { try { perfCtrl.abort(); } catch (e) { /* 旧实现无 abort 时忽略 */ } }
  perfCtrl = typeof AbortController === 'function' ? new AbortController() : null;
  try {
    const d = await api('usage/perf?' + perfQueryFromUsage().toString(), perfCtrl ? { signal: perfCtrl.signal } : {});
    if (my !== perfSeq) return;
    renderPerfStats(d);
  } catch (e) {
    if (my !== perfSeq) return;
    const box = $('usPerfEmpty');
    if (box) { box.hidden = false; box.textContent = '性能分位读取失败：' + (e && e.message ? e.message : String(e)); }
    const table = $('usPerfTable');
    if (table) table.hidden = true;
    const note = $('usPerfNote');
    if (note) note.textContent = '读取失败';
  }
}

const CREDIT_GRAN_LABEL = { hourly: '按小时聚合', daily: '按日聚合', hour: '按小时聚合', day: '按日聚合' };
/* 积分获取历史来自账号最近一次积分包快照（/credits/grants），不是逐笔领取账本：
   同一包可能由签到/活跃任务发放，也可能来自套餐或活动。UI 明确标注快照口径，
   账号单元格只显示昵称；UID 只截前 8 位放 title，绝不把凭证带进 DOM。
   扣减档仍只展示服务端聚合桶，不把同一桶拆成逐请求账单。 */
const CREDIT_GRANT_STATUS = {
  active: ['可用', 'ok'], expiring: ['快到期', 'warn'],
  expired: ['已过期', 'bad'], used: ['已用完', 'mute'],
};
const CREDIT_GRANT_ACTION = { checkin: '签到', activity: '活跃上报' };
function creditGrantName(r) {
  const nickname = String(r.nickname || '').trim();
  if (nickname) return nickname;
  return '未命名账号';
}
function creditGrantUIDTitle(r) {
  const uid = String(r.uid || '');
  return uid ? 'UID ' + uid.slice(0, 8) : '';
}
function creditGrantTime(v) {
  const text = String(v || '').trim();
  if (!text) return '—';
  // expires_at 既可能是毫秒也可能是秒；优先用上游原始 ISO，数字仅作兜底。
  if (/^\d+$/.test(text)) {
    const n = Number(text);
    if (Number.isFinite(n) && n > 0) return fmtLocalDateTime(n > 1e12 ? n : n * 1000);
  }
  const ms = Date.parse(text);
  if (!Number.isFinite(ms)) return text.slice(0, 16).replace('T', ' ');
  return fmtLocalDateTime(ms);
}
function creditGrantExpiry(r) {
  if (r.no_expiry) return '<span class="tag mute">长期有效</span>';
  const shown = creditGrantTime(r.expire_iso || r.expires_at);
  if (shown === '—') return '<span class="tag mute">无到期时间</span>';
  // 上游 EndTime 在 DeductionEndTime 缺失时会回落到 CycleEndTime，那只是周期
  // 重置边界，不等于积分失效；不能把它写成“到期”。
  if (r.expiry_kind === 'cycle') {
    return '<span class="tag mute">周期边界</span><div class="note">' + esc(shown) + '</div>';
  }
  const st = CREDIT_GRANT_STATUS[r.status] || ['', 'mute'];
  const days = Number(r.days_left);
  const left = Number.isFinite(days) && r.status !== 'expired' && r.status !== 'used'
    ? '<div class="note">' + (days <= 0 ? '今天到期' : '剩 ' + days + ' 天') + '</div>' : '';
  return esc(shown) + left + (st[0] ? '<div><span class="tag ' + st[1] + '">' + esc(st[0]) + '</span></div>' : '');
}
function creditGrantAction(a) {
  if (!a || !a.task) return '<span class="muted">—</span>';
  const task = CREDIT_GRANT_ACTION[a.task] || a.task;
  const when = a.ts ? '<div class="note">' + esc(creditGrantTime(a.ts)) + '</div>' : '';
  return '<span class="tag ' + (a.ok ? 'ok' : 'bad') + '">' + esc(task) +
    (a.ok ? '' : '失败') + '</span>' + when;
}
function creditHistoryGrantUnavailable() {
  creditHistData = null;
  const table = $('usCreditHistTable');
  if (table) table.hidden = true;
  const empty = $('usCreditHistEmpty');
  if (empty) {
    empty.hidden = false;
    empty.textContent = '积分获取历史暂不可用：本地只有按小时/日聚合的 usage.credit 观测，无法可靠还原逐笔领取。';
  }
  const note = $('usCreditHistNote');
  if (note) note.textContent = '接口未提供 · 不伪造';
}
function renderCreditHistory(d) {
  d = d || {};
  creditHistData = d;
  const records = Array.isArray(d.records) ? d.records : [];
  const table = $('usCreditHistTable');
  const empty = $('usCreditHistEmpty');
  const note = $('usCreditHistNote');
  const kinds = { spend: '仅扣减', all: '全部扣减' };
  const kindLabel = kinds[usCreditKind] || '全部扣减';
  if (note) note.textContent = records.length
    ? records.length + ' 条聚合记录 · 汇总扣减 ' + fmtCredit(d.credits)
    : '0 条 · ' + kindLabel;
  if (table) table.hidden = !records.length;
  if (empty) {
    empty.hidden = !!records.length;
    empty.textContent = '当前时间窗暂无积分扣减记录。旧版仅有 Token 的历史不会伪造积分；聚合桶也不会被拆成逐请求行。';
  }
  const bodyHtml = records.map(r => {
    const when = String(r.time || '').replace('T', ' ').slice(0, 16);
    const gran = CREDIT_GRAN_LABEL[r.granularity] || CREDIT_GRAN_LABEL[r.scope] || '聚合';
    const amount = Number(r.credit || 0);
    return '<tr>' +
      '<td data-label="时间" class="num">' + esc(when) + '</td>' +
      '<td data-label="粒度">' + esc(gran) + '</td>' +
      '<td data-label="账号">' + esc(r.account || '—') + (r.realm ? '<div class="note">' + esc(r.realm) + '</div>' : '') + '</td>' +
      '<td data-label="模型">' + esc(r.model || '—') + '</td>' +
      '<td data-label="请求" class="num">' + fmtTok(r.requests) + '</td>' +
      '<td data-label="扣减积分" class="num">' + (amount > 0 ? '−' : '') + fmtCredit(Math.abs(amount)) + '</td>' +
      '<td data-label="Token" class="num">' + fmtTok(r.tokens) + '</td>' +
      '</tr>';
  }).join('');
  // 局部兜底：本函数会被前端切片测试单独求值，不能假定全局 setHTML 已定义。
  const written = typeof setHTML === 'function'
    ? setHTML('usCreditHistBody', bodyHtml)
    : (() => { const el = $('usCreditHistBody'); if (!el) return false; el.innerHTML = bodyHtml; return true; })();
  if (!written && typeof noteUsageDomMismatch === 'function') noteUsageDomMismatch('usCreditHistBody');
}
async function loadCreditHistory() {
  if (usCreditKind === 'grant') { return loadCreditGrants(); }
  const my = ++creditHistSeq;
  if (creditCtrl) { try { creditCtrl.abort(); } catch (e) { /* ignore */ } }
  creditCtrl = typeof AbortController === 'function' ? new AbortController() : null;
  const btn = $('btnCreditHist');
  if (btn) { btn.disabled = true; btn.textContent = '读取中…'; btn.setAttribute('aria-busy', 'true'); }
  try {
    const q = trangeQuery('usRange', true);
    q.set('kind', usCreditKind);
    q.set('limit', '200');
    const d = await api('credits/history?' + q.toString(), creditCtrl ? { signal: creditCtrl.signal } : {});
    if (my !== creditHistSeq) return;
    renderCreditHistory(d || { records: [] });
  } catch (e) {
    if (my !== creditHistSeq) return;
    creditHistData = null;
    const table = $('usCreditHistTable');
    if (table) table.hidden = true;
    const empty = $('usCreditHistEmpty');
    if (empty) {
      empty.hidden = false;
      empty.textContent = e && e.status === 501
        ? '积分获取历史暂不可用：本地聚合桶无法可靠还原逐笔领取。'
        : '积分扣减历史读取失败：' + (e && e.message ? e.message : String(e));
    }
    const note = $('usCreditHistNote');
    if (note) note.textContent = e && e.status === 501 ? '接口未提供' : '读取失败';
  } finally {
    if (my === creditHistSeq && btn) {
      btn.disabled = false; btn.textContent = '刷新流水'; btn.removeAttribute('aria-busy');
    }
  }
}

async function loadCreditGrants() {
  const my = ++creditGrantSeq;
  if (creditGrantCtrl) { try { creditGrantCtrl.abort(); } catch (e) { /* ignore */ } }
  creditGrantCtrl = typeof AbortController === 'function' ? new AbortController() : null;
  const btn = $('btnCreditHist');
  if (btn) { btn.disabled = true; btn.textContent = '读取中…'; btn.setAttribute('aria-busy', 'true'); }
  try {
    // 默认只取最近 200 行；服务端返回 has_more/count，需要更多时由调用方显式放大。
    // 固定 1000 会把整表 DOM 与 JSON 都放大 5 倍，和轻量快照的定位冲突。
    const d = await api('credits/grants', creditGrantCtrl ? { signal: creditGrantCtrl.signal } : {});
    if (my !== creditGrantSeq) return;
    renderCreditGrants(d || { rows: [] });
  } catch (e) {
    if (my !== creditGrantSeq) return;
    const table = $('usCreditHistTable');
    if (table) table.hidden = true;
    const empty = $('usCreditHistEmpty');
    if (empty) {
      empty.hidden = false;
      empty.textContent = e && e.status === 403
        ? '积分获取历史需要管理密钥，请重新登录面板。'
        : '积分获取历史读取失败：' + (e && e.message ? e.message : String(e));
    }
    const note = $('usCreditHistNote');
    if (note) note.textContent = '读取失败';
  } finally {
    if (my === creditGrantSeq && btn) {
      btn.disabled = false; btn.textContent = '刷新流水'; btn.removeAttribute('aria-busy');
    }
  }
}

function renderCreditGrants(d) {
  d = d || {};
  const rows = Array.isArray(d.rows) ? d.rows : [];
  const table = $('usCreditHistTable');
  const empty = $('usCreditHistEmpty');
  const note = $('usCreditHistNote');
  // 整个聚合流水块在旧骨架里可能整块缺失：表头/表体都走 setHTML，
  // 缺任一节点只落一次版本提示，绝不能对 null 写 innerHTML。
  setHTML('usCreditHistHead', '<tr><th>发放时间</th><th>账号</th><th>包 / 来源</th>' +
    '<th class="num">面额</th><th class="num">剩余 / 已用</th><th>到期</th><th>关联动作</th></tr>');
  if (note) {
    const stamp = d.fetched_at || d.generated_at;
    const shown = d.returned == null ? rows.length : Number(d.returned);
    const total = d.count == null ? rows.length : Number(d.count);
    note.textContent = (d.has_more ? shown + ' / ' + total : String(shown)) + ' 个积分包' +
      (stamp ? ' · 快照 ' + creditGrantTime(stamp) : '') +
      (d.has_more ? ' · 仅展示最近 ' + shown + ' 个' : '');
  }
  if (table) table.hidden = !rows.length;
  if (empty) {
    empty.hidden = !!rows.length;
    empty.textContent = '当前没有可展示的积分包快照。请先到「积分构成」刷新余额/套餐，或等待后台余额刷新后再试。';
  }
  const count = Number(d.count == null ? rows.length : d.count);
  const returned = Number(d.returned == null ? rows.length : d.returned);
  setHTML('usCreditStats',
    usKpi(fmtCredit(d.total_size), '累计面额', 'c-accent', '快照内全部积分包面额') +
    usKpi(fmtCredit(d.total_remain), '剩余积分', 'c-ok', '当前仍可用的部分') +
    usKpi(fmtCredit(d.total_used), '已用积分', 'c-mute', '面额 − 剩余') +
    usKpi(String(count), '积分包', 'c-soft',
      (d.has_more ? '当前展示最近 ' + returned + ' 个' : returned + ' 行逐包明细')));
  if (!setHTML('usCreditHistBody', rows.map(r => {
    const status = CREDIT_GRANT_STATUS[r.status] || ['', 'mute'];
    const name = creditGrantName(r);
    const uidTitle = creditGrantUIDTitle(r);
    const pack = [r.name || '积分包', r.product, r.grant_reason].filter(Boolean);
    const size = Number(r.size || 0);
    const remain = Number(r.remain || 0);
    const used = Number.isFinite(Number(r.used)) ? Number(r.used) : Math.max(0, size - remain);
    const statusTag = status[0]
      ? '<div><span class="tag ' + status[1] + '">' + esc(status[0]) + '</span></div>' : '';
    return '<tr>' +
      '<td data-label="发放时间" class="num">' + esc(creditGrantTime(r.created_at || r.created_iso)) + '</td>' +
      '<td data-label="账号"' + (uidTitle ? ' title="' + esc(uidTitle) + '"' : '') + '>' + esc(name) + '</td>' +
      '<td data-label="包 / 来源">' + esc(pack[0] || '积分包') +
        (pack.slice(1).length ? '<div class="note">' + esc(pack.slice(1).join(' · ')) + '</div>' : '') + '</td>' +
      '<td data-label="面额" class="num">' + fmtCredit(size) + '</td>' +
      '<td data-label="剩余 / 已用" class="num">' + fmtCredit(remain) + ' / ' + fmtCredit(used) + '</td>' +
      '<td data-label="到期">' + creditGrantExpiry(r) + statusTag + '</td>' +
      '<td data-label="关联动作">' + creditGrantAction(r.action) + '</td>' +
      '</tr>';
  }).join(''))) {
    noteUsageDomMismatch('usCreditHistBody');
  }
}


function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }
function trimFixed(s) {
  if (!String(s).includes('.')) return String(s);
  return String(s).replace(/0+$/, '').replace(/\.$/, '');
}
function fmtCredit(n) {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return '—';
  return trimFixed(v.toFixed(2));
}
function fmtCreditRatio(v, samples, tokens) {
  if (!samples || !tokens) return '—';
  const n = Number(v || 0);
  if (!Number.isFinite(n)) return '—';
  return trimFixed(n.toFixed(4)) + ' / 1M';
}
function fmtModelRate(rate) {
  const s = String(rate || '').trim();
  return s ? 'x' + s : '—';
}

/* usKpi 用量页的指标卡。比账号池的 .stat 多两样：语义色轨（cls）与副标题（sub，
   放"占比 / 均速率"这类解释性数字）；bar 是卡片内的构成条 HTML，只有需要时才传。 */
function usKpi(v, k, cls, sub, bar) {
  return '<div class="kpi ' + (cls || '') + '">' +
    '<div class="k">' + esc(k) + '</div>' +
    '<div class="v">' + esc(v) + '</div>' +
    (bar || '') +
    (sub ? '<div class="s">' + esc(sub) + '</div>' : '') +
    '</div>';
}

/* usMixBar prompt/completion 占比条。宽度按百分比而不是固定像素：表列宽随窗口变化，
   像素宽度在窄屏会溢出、宽屏又显得没信息。 */
function usMixBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-mix" title="prompt ' + pp.toFixed(1) + '% · completion ' + pc.toFixed(1) + '%">' +
    '<i class="p" style="width:' + pp.toFixed(2) + '%"></i>' +
    '<i class="c" style="width:' + pc.toFixed(2) + '%"></i>' +
    '</span>';
}

/* usPct 占比文案（0 值不显示 "0.0%"，直接 —，避免一行全是零）。 */
function usPct(part, total) {
  const t = Number(total || 0);
  if (!t) return '—';
  return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制延迟/速率两列；列开关显式传入，避免调用方改动后与表头错列。 */
function usRow(name, sub, a, mid, withPerf) {
  // 每格都带 data-label：窄屏 .cardify 靠它把宽表变成逐行卡片（Hub data-cards 同款）。
  // mid 由调用方提供，必须自带 data-label；账号维度的「域」列在这里统一标注。
  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td data-label="名称">' + esc(name) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid ? mid.replace('<td class="num">', '<td class="num" data-label="域">') : '') +
    '<td class="num" data-label="请求">' + fmtTok(a.requests) + '</td>' +
    '<td class="num" data-label="失败">' + (a.errors ? '<span style="color:var(--warn)">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num" data-label="Prompt">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num" data-label="Completion">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num" data-label="合计">' + fmtTok(a.total_tokens) +
      usMixBar(a.prompt_tokens, a.completion_tokens, a.total_tokens) + '</td>' +
    (withPerf
      ? '<td class="num" data-label="均延迟">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num" data-label="均速率">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}

/* ── 用量明细：三个维度共用一张表 + 页内切换 ──────────────────────────
   账号 / 模型 / 域三张表此前各自占一个 box，页面纵向拉得很长且表头结构几乎一样。
   现在合成一个 box：表头由维度定义生成，行渲染复用 usRow，切换零请求。 */
const US_DIMS = {
  account: { key: 'by_account', title: '账号', withRealm: true, withPerf: true, span: 10 },
  key: { key: 'by_key', title: '客户端 Key', withRealm: false, withPerf: true, span: 10 },
  model: { key: 'by_model', title: '模型', withRealm: false, withPerf: false, span: 7 },
  realm: { key: 'by_realm', title: 'realm', withRealm: false, withPerf: false, span: 7 },
};

// usSortRows 按当前排序字段降序（默认合计 Token，最大者最相关）。
/* usFilterRows 用量明细的本地筛选：空格分词 AND，匹配账号昵称/UID、模型与域。
   过滤只影响当前列表，导出仍按服务端时间窗重新读取，不携带搜索词。 */
function usFilterRows(rows, q) {
  const words = String(q || '').toLowerCase().split(/\s+/).filter(Boolean);
  if (!words.length) return (rows || []).slice();
  return (rows || []).filter(row => {
    const text = [row && row.key, row && row.extra, row && row.realm]
      .filter(Boolean).join(' ').toLowerCase();
    return words.every(w => text.includes(w));
  });
}

function usSortRows(rows, sort) {
  const out = rows.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  const val = a => sort === 'requests' ? num(a.requests)
    : sort === 'errors' ? num(a.errors)
    : sort === 'latency' ? num(a.avg_latency_ms)
    : num(a.total_tokens);
  out.sort((a, b) => val(b) - val(a));
  return out;
}

function usDimHead(dim) {
  const m = US_DIMS[dim] || US_DIMS.account;
  return '<tr><th class="mark" aria-hidden="true"></th><th>' + esc(m.title) + '</th>' +
    (m.withRealm ? '<th>域</th>' : '') +
    '<th class="num">请求</th><th class="num">失败</th>' +
    '<th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计</th>' +
    (m.withPerf ? '<th class="num">均延迟</th><th class="num">均速率</th>' : '') +
    '</tr>';
}

/* usTabsHtml 维度切换按钮（带条数徽标）。
   容器 #usDimTabs 是页面骨架里固定的 <span>，只允许重写它的子按钮，绝不能
   反过来把容器当成按钮生成——线上 2706 行的 innerHTML-null 就是升级后骨架
   与脚本版本不一致时，这里直接对不存在的容器写属性炸出来的。 */
function usTabsHtml(dims, active, counts) {
  return dims.map(([k, label]) => {
    const n = counts ? counts[k] : null;
    return '<button data-dim="' + k + '" aria-pressed="' + (k === active ? 'true' : 'false') + '"' +
      (k === active ? ' class="on"' : '') + '>' +
      esc(label) + (n == null ? '' : '<span class="cnt">' + n + '</span>') + '</button>';
  }).join('');
}

/* ── DOM 契约防护 ───────────────────────────────────────────────────
   面板是 go:embed 的二进制内资源，但升级时浏览器可能仍缓存旧 index.html，
   反向代理也可能只替换了二进制而没有同步 HTML。新版 app.js 跑在旧骨架上时，
   任何 `$('id').innerHTML = ...` 都会抛 TypeError，随后整个用量视图与按钮
   绑定都停摆。这里提供统一的探测/写入入口：缺节点时只提示一次，绝不让一次
   缺块变成整页未处理异常。

   注意：这里不自动 reload。页面版本错配时浏览器可能持续拿到旧 HTML，自动
   刷新会形成「刷新→仍缺块→再刷新」的循环；提示用户手动刷新（或清缓存）才
   可控。CSP 不允许内联脚本，也无法在旧页面里注入版本探测。 */
/* setHTML / setText 缺节点时返回 false，由调用方决定是否继续；不抛异常。 */
function setHTML(id, html) {
  const el = $(id);
  if (!el) { noteUsageDomMismatch(id); return false; }
  el.innerHTML = html;
  return true;
}
function setText(id, text) {
  const el = $(id);
  if (!el) { noteUsageDomMismatch(id); return false; }
  el.textContent = text;
  return true;
}
/* 页面升级时浏览器可能缓存旧 index.html，或反代只替换了二进制。任何
   `if ($(id)) $(id).innerHTML = ...` 都有“两次查询之间节点被移除”的窗口：
   第一次拿到节点、第二次却拿到 null，于是仍然抛 TypeError。所有清理逻辑
   必须只取一次节点；applyNode 只负责这一次写入，缺节点时安静跳过。 */
function applyNode(id, apply) {
  const el = $(id);
  if (!el) return false;
  try { apply(el); } catch (_) { /* inert DOM / 页面正在卸载 */ }
  return true;
}
function clearNode(id, key) {
  return applyNode(id, el => { el[key] = key === 'innerHTML' ? '' : '—'; });
}
/* expectNode 只用于「缺了它整份脚本就不可用」的顶层绑定。 */
function expectNode(id) {
  const el = $(id);
  if (el) return el;
  noteDOM(id);
  return null;
}
function domVersionTip() {
  return '页面版本与脚本不一致，请刷新页面（Ctrl/Cmd+Shift+R 可强制刷新）。';
}
function noteDOM(id) {
  // 去重必须按「节点」而不是整页：升级错配时通常缺多个区块，但页面提示
  // 只需一次；反之，若先出现的是可选块（例如旧骨架缺 usNote），之后的
  // 关键入口（accBody、taskVeil）缺失不能被同一个布尔值静默吞掉。早期
  // 实现用 noteDOM.shown 全局短路，结果第二个真实故障只剩控制台报错。
  if (!noteDOM.seen) noteDOM.seen = new Set();
  if (id) {
    if (noteDOM.seen.has(id)) return;
    noteDOM.seen.add(id);
  }
  if (noteDOM.shown) return;
  noteDOM.shown = true;
  const msg = domVersionTip() + '（缺少 ' + id + '）';
  // 提示必须落在用户当前看得见的地方。旧骨架在账号页缺 accBody 时，
  // #usError/#view-usage 往往存在但 hidden；写进去等于“静默失败”。
  // 先用可见视图/顶栏，最后才退回 toast（toast 本身由 body 承载）。
  // 用量页在旧骨架里可能整块缺失。此时不要退到 toast：noteUsageDomMismatch
  // 还会为同一个缺块再提示一次，用户会看到两条同义告警。visibleDomHost 已经
  // 只选择当前可见视图/顶栏；都找不到时由调用方决定是否另发 toast。
  const top = $('topbar');
  const visible = typeof document !== 'undefined' && document.querySelector
    ? document.querySelector('.view:not([hidden])') : null;
  const host = visible || top;
  if (host) {
    try {
      const tip = document.createElement('div');
      tip.className = 'auth-gate inline';
      tip.setAttribute('role', 'alert');
      tip.textContent = msg;
      if (visible && visible.parentNode && typeof visible.parentNode.insertBefore === 'function') {
        visible.parentNode.insertBefore(tip, visible);
      } else if (visible && typeof visible.prepend === 'function') {
        visible.prepend(tip);
      } else if (host.id === 'topbar' && typeof host.appendChild === 'function') {
        host.appendChild(tip);
      } else {
        host.textContent = msg;
      }
    } catch (_) { /* inert DOM */ }
  }
}

/* noteUsageDomMismatch 把「页面骨架与 app.js 不匹配」收敛成一条页面级提示。
   浏览器可能缓存了旧 index.html、反向代理也可能只替换了二进制而没刷新静态
   资源；这类错误不应反复弹 toast，也不该静默丢掉整页用量。提示挂在用量页
   仍存在的主容器/图表上，并由 noteDOM 统一触发一次自动刷新。 */
let usageDomMismatch = false;
let usageDomMismatchHost = null;
let usageDomMismatchTip = null;
let usageDomMismatchTipEl = null;
// visibleDomHost 选择用户当前能看到的提示落点。用量页专用错误位只有在用量页
// 可见时才可用；否则账号页缺 accBody 的告警会被写进隐藏的 #usError，用户只看到
// 空白页。优先当前可见视图，其次顶栏，最后才交给 toast（body 级）。
function visibleDomHost() {
  if (typeof document === 'undefined' || !document.querySelector) return null;
  const v = document.querySelector('.view:not([hidden])');
  if (v) return v;
  const top = $('topbar');
  if (top) return top;
  return typeof toast === 'function' ? { id: 'toasts' } : null;
}
/* domQuarantine 记录本次页面生命周期内确认「模板与脚本不匹配」的节点。
   旧 index.html 往往缺多个可选区块；若每个缺失都各自插入提示，用量页会堆出
   多条同义告警。记录后，同一节点只提示一次，同时让调用方可以明确区分
   「本次写入被跳过」与「写入成功」，不再靠 innerHTML 之外的空节点判断。 */
const domQuarantine = new Set();
function usageDomHost() {
  // 只有当前确实在用量页时才写用量页节点；否则退到可见视图/顶栏/toast。
  const usage = $('view-usage');
  const onUsage = !!usage && !usage.hidden;
  if (onUsage) return $('usError') || $('usChart') || usage || $('usNote');
  return visibleDomHost();
}
function noteUsageDomMismatch(id) {
  if (domQuarantine.has(id)) return;
  domQuarantine.add(id);
  // 页面级版本提示是否已发过，由下方 usageDomMismatch / domQuarantine 自己
  // 判断。这里不能复用 noteDOM.shown：那是「入口级缺节点」的全局标记，
  // 一旦被某个可选块提前置位，后续真正缺少用量页节点的提示会被静默吞掉。
  const host = usageDomHost();
  if (!host || typeof document === 'undefined') {
    // 页面连任何可用落点都没有：不要永久锁死提示，否则后续渲染出容器时
    // 用户仍然看不到可操作信息（只剩控制台 TypeError）。
    if (host && host.id === 'toasts' && typeof toast === 'function') {
      const message = domVersionTip() + '（缺少 ' + id + '）';
      if (!usageDomMismatchTip) {
        usageDomMismatchTip = message;
        usageDomMismatch = true;
        toast(message, 'err');
      }
    }
    return;
  }
  // 同一落点只保留一条提示，但不同缺块要补充节点名：升级错配时用户需要
  // 知道是哪个区块没渲染，而不是只看到第一条（可能是可选块）后就再无下文。
  // 同一节点重复写入仍由 domQuarantine 短路，避免每轮渲染刷屏。
  if (usageDomMismatch && usageDomMismatchHost === host) {
    const base = domVersionTip();
    if (host.id === 'usError') {
      const next = host.textContent || base;
      if (!next.includes(id)) host.textContent = next + '；另缺 ' + id;
    } else if (usageDomMismatchTip) {
      if (!usageDomMismatchTip.includes(id)) usageDomMismatchTip += '；另缺 ' + id;
      if (usageDomMismatchTipEl && 'textContent' in usageDomMismatchTipEl) {
        usageDomMismatchTipEl.textContent = usageDomMismatchTip;
      }
    }
    return;
  }
  // 页面入口已用 noteDOM 报过同一次错配（缺 view-usage 等关键节点），这里
  // 不再为可选块叠加第二条视觉告警，但仍记录节点，避免同一缺块反复探测。
  if (noteDOM.shown && !usageDomMismatch) return;
  usageDomMismatch = true;
  usageDomMismatchHost = host;
  const message = domVersionTip() + '（缺少 ' + id + '）';
  // 首选专用错误位（直接写内容），其余容器插入到它前面，避免打乱表格/图表结构。
  if (host.id === 'usError') {
    host.textContent = message;
    if ('hidden' in host) host.hidden = false;
    usageDomMismatchTipEl = host;
    return;
  }
  if (host.id === 'toasts' && typeof toast === 'function') {
    toast(message, 'err');
    return;
  }
  // createElement 在最小 DOM 桩、被冻结的旧页面里也可能不可用或抛错。
  // 缺块提示本身就是异常退路，不能反过来成为新的未处理异常源。
  try {
    if (typeof document.createElement !== 'function') throw new Error('createElement unavailable');
    const tip = document.createElement('div');
    tip.className = 'auth-gate inline';
    tip.setAttribute('role', 'alert');
    tip.textContent = message;
    usageDomMismatchTip = message;
    usageDomMismatchTipEl = tip;
    if (host.parentNode && typeof host.parentNode.insertBefore === 'function') {
      host.parentNode.insertBefore(tip, host);
    } else {
      host.textContent = message;
    }
  } catch (_) {
    // 落点无法插入时退回纯文本；仍失败则保留去重标记，避免连续刷屏。
    try { host.textContent = message; } catch (_) { /* inert DOM */ }
  }
}

const US_DIM_TABS = [['account', '按账号'], ['key', '按 Key'], ['model', '按模型'], ['realm', '按域']];

/* usKeyLabel 把归档里的 key_id 变成可读名称。只使用配置页已加载的
   api_keys 元数据（id/name/enabled），绝不接触密钥原文；未命中时显示
   「未归属」/裸 id，不能凭空补一个名字。 */
function usKeyLabel(id) {
  const key = String(id || '');
  if (!key) return { name: '未归属', sub: '历史记录或未启用鉴权' };
  const list = (cfgLoaded && Array.isArray(cfgLoaded.api_keys)) ? cfgLoaded.api_keys : [];
  const hit = list.find(k => k && String(k.id) === key);
  if (!hit) return { name: key, sub: 'Key 已删除或未在当前配置' };
  const enabled = apiKeyEnabled(hit.enabled);
  return { name: hit.name ? hit.name + ' (' + key + ')' : key, sub: enabled ? '已启用' : '已禁用' };
}

function renderUsageDim() {
  const d = usageData || {};
  // 升级后最危险的调用链是 go('usage') → loadUsage() → renderUsage() →
  // renderUsageDim()：旧页面骨架若缺少下表任一节点，直接 `$('id').innerHTML`
  // 会在 async 渲染里抛出未处理异常，按钮后续也不再可用。这里把整块渲染视为
  // 可选增强：先在入口确认表头/表体存在，缺块只提示一次并返回。
  if (!$('usDimHead') || !$('usDimBody')) {
    noteUsageDomMismatch(!$('usDimHead') ? 'usDimHead' : 'usDimBody');
    return;
  }
  // 数据对象身份变化（手动刷新/筛选结果被替换）时必须失效；只比较行数会让
  // server 返回同长度但不同内容的新快照继续用旧缓存。
  if (renderUsageDim.data !== d) {
    renderUsageDim.cache = null;
    renderUsageDim.data = d;
  }
  const m = US_DIMS[usDim] || US_DIMS.account;
  // 翻页不改变数据/维度/排序/搜索词；缓存过滤+排序后的结果，切页只切片。
  const q = $('usQ') ? $('usQ').value.trim() : '';
  const usSig = usDim + '|' + usSort + '|' + q + '|' + ((d[m.key] || []).length);
  if (!renderUsageDim.cache || renderUsageDim.cache.sig !== usSig) {
    renderUsageDim.cache = { sig: usSig, rows: usSortRows(usFilterRows(d[m.key] || [], q), usSort) };
  }
  const rows = renderUsageDim.cache.rows;
  const size = usDimPageSize > 0 ? usDimPageSize : Math.max(1, rows.length);
  const pages = Math.max(1, Math.ceil(rows.length / size));
  if (usDimPage > pages) usDimPage = pages;
  if (usDimPage < 1) usDimPage = 1;
  const start = (usDimPage - 1) * size;
  const pageRows = rows.slice(start, start + size);
  // 注意：不能把 usTabsHtml 的结果写进不存在的容器；setHTML 会返回 false 并
  // 触发一次版本提示，但不会中断下面已经确认存在的表头/表体更新。
  setHTML('usDimTabs', usTabsHtml(US_DIM_TABS, usDim, {
    account: (d.by_account || []).length,
    key: (d.by_key || []).length,
    model: (d.by_model || []).length,
    realm: (d.by_realm || []).length,
  }));
  setHTML('usDimHead', usDimHead(usDim));
  setHTML('usDimBody', pageRows.map(x =>
    usRow(usDim === 'account' ? String(x.key || '').slice(0, 8)
        : usDim === 'key' ? usKeyLabel(x.key).name : x.key,
      usDim === 'account' ? (x.extra || '')
        : usDim === 'key' ? usKeyLabel(x.key).sub : '',
      x,
      usDim === 'account' ? '<td class="num">' + esc(x.realm || '') + '</td>' : '',
      m.withPerf)
  ).join('') || '<tr><td colspan="' + m.span + '" class="empty">暂无数据</td></tr>');
  const totalRows = (d[m.key] || []).length;
  const dimNote = $('usDimNote');
  if (dimNote) dimNote.textContent = (rows.length === totalRows ? rows.length + ' 行' : '命中 ' + rows.length + ' / ' + totalRows + ' 行') +
    ' · 请求数含失败尝试';
  const page = $('usDimPage');
  if (page) page.textContent = rows.length ? (usDimPage + ' / ' + pages) : '';
  const prev = $('btnUsDimPrev'), next = $('btnUsDimNext');
  if (prev) prev.disabled = usDimPage <= 1;
  if (next) next.disabled = usDimPage >= pages;
  const sizeSel = $('usDimSize');
  if (sizeSel) sizeSel.value = String(usDimPageSize);
}

/* 积分扣除：按账号 / 按模型两个维度共用一张表（同上，两个 box 合成一个）。 */
function usCreditHead(dim) {
  return dim === 'model'
    ? '<tr><th class="mark" aria-hidden="true"></th><th>模型</th><th>积分倍率</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>'
    : '<tr><th class="mark" aria-hidden="true"></th><th>账号</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>';
}

/* 缓存命中率（issue #92）：颜色即健康度——≥90% 绿 / 80–90% 黄 / <80% 红，
   样本不足灰。title 带命中/未命中绝对量，供逐项核对。 */
function cacheRateCell(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '<span class="muted">—</span>';
  const pct = h / total * 100;
  const color = pct >= 90 ? 'var(--ok)' : (pct >= 80 ? 'var(--warn)' : 'var(--bad)');
  const txt = trimFixed(pct.toFixed(1)) + '%';
  return '<span style="color:' + color + '" title="命中 ' + fmtTok(h) + ' / 未命中 ' + fmtTok(m) + ' tok">' + txt + '</span>';
}

function renderCreditDim() {
  const d = usageData || {};
  // 积分扣减汇总在「积分流水」里是可选折叠区：旧二进制/旧缓存版本可能没有
  // #usCreditHead / #usCreditBody。缺块时整块跳过，不能把可选展示变成必需契约。
  if (!$('usCreditHead') || !$('usCreditBody')) {
    noteUsageDomMismatch(!$('usCreditHead') ? 'usCreditHead' : 'usCreditBody');
    return;
  }
  setHTML('usCreditHead', usCreditHead(usCreditDim));
  const empty = '暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。';
  if (usCreditDim === 'model') {
    const models = d.credit_by_model || [];
    setHTML('usCreditBody', models.map(row =>
      '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td data-label="模型">' + esc(row.key || '—') + '</td>' +
        '<td data-label="积分倍率">' + esc(fmtModelRate(row.rate)) + '</td>' +
        '<td class="num" data-label="请求">' + fmtTok(row.requests) + '</td>' +
        '<td class="num" data-label="扣除积分">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num" data-label="有效样本 Token">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num" data-label="积分 / 1M Token">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num" data-label="缓存命中率">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
      '</tr>'
    ).join('') || '<tr><td colspan="8" class="empty">' + empty + '</td></tr>');
  } else {
    const accounts = d.credit_by_account || [];
    setHTML('usCreditBody', accounts.map(row => {
      const uid = String(row.key || '');
      const account = row.nickname || uid.slice(0, 8) || '—';
      return '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td data-label="账号">' + esc(account) + '<div class="note">' + esc(row.realm || '') + ' · ' + esc(uid.slice(0, 8)) + '</div></td>' +
        '<td class="num" data-label="请求">' + fmtTok(row.requests) + '</td>' +
        '<td class="num" data-label="扣除积分">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num" data-label="有效样本 Token">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num" data-label="积分 / 1M Token">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num" data-label="缓存命中率">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
        '</tr>';
    }).join('') || '<tr><td colspan="7" class="empty">' + empty + '</td></tr>');
  }
  setHTML('usCreditTabs', usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, {
    account: (d.credit_by_account || []).length,
    model: (d.credit_by_model || []).length,
  }));
}

function renderUsage(d) {
  usageData = d || {};
  // 成功渲染即清除上一轮失败横幅；否则网络恢复后用户仍会看到旧错误。
  const errBox = $('usError');
  if (errBox) { errBox.hidden = true; errBox.textContent = ''; }
  // renderUsage 是成功路径的入口。旧 index.html 可能只保留部分用量节点；
  // 除最外层 #view-usage 外，其余区块都是可选增强：缺哪块只提示一次，其他
  // 仍存在的卡片/图表照常渲染。绝不能把“缺一块”升级成整页放弃或 null 写入。
  if (!document.querySelector || !document.querySelector('#view-usage')) {
    noteUsageDomMismatch('view-usage');
    return;
  }
  for (const id of ['usStats', 'usChart', 'usNote', 'usDimHead', 'usDimBody', 'usCreditHead', 'usCreditBody', 'usCreditStats']) {
    if (!$(id)) noteUsageDomMismatch(id);
  }
  const t = usageData.totals || {};
  const total = Number(t.total_tokens || 0);
  const pt = Number(t.prompt_tokens || 0);
  const ct = Number(t.completion_tokens || 0);
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const okRate = reqs ? (reqs - errs) / reqs * 100 : null;
  // 构成条要的是合法 CSS 宽度，usPct 在无样本时返回 "—"，不能直接拼进 style。
  const pctW = (part) => total ? Math.max(0, Math.min(100, Number(part || 0) / total * 100)).toFixed(2) + '%' : '0%';
  // 六张卡：主指标用强调色，completion 用成功色（与图表里的绿柱呼应），
  // 失败/延迟只在有值时上语义色——全绿全黄的仪表盘等于没有重点。
  setHTML('usStats',
    usKpi(fmtTok(reqs), '请求数', 'c-accent',
      errs ? '其中失败 ' + errs + ' 次' : '全部成功') +
    usKpi(fmtTok(total), '总 token', 'c-accent',
      'prompt ' + usPct(pt, total) + ' · completion ' + usPct(ct, total),
      '<div class="kbar"><i style="width:' + pctW(pt) + ';background:var(--accent)"></i>' +
      '<i style="width:' + pctW(ct) + ';background:var(--ok)"></i></div>') +
    usKpi(fmtTok(pt), 'prompt', 'c-soft', '占比 ' + usPct(pt, total)) +
    usKpi(fmtTok(ct), 'completion', 'c-ok', '占比 ' + usPct(ct, total)) +
    usKpi(String(errs), '失败尝试', errs ? 'c-warn' : 'c-mute',
      okRate == null ? '—' : (errs ? '成功率 ' + okRate.toFixed(1) + '%' : '成功率 100%')) +
    usKpi(fmtMs(t.avg_latency_ms), '平均延迟', 'c-soft',
      t.avg_tokens_per_second ? '吐字 ' + fmtRate(t.avg_tokens_per_second) : '无速率样本'));

  // 卡片、明细表与时序图全部按所选窗口统计（切窗口数字随之变化）；
  // 「全部历史」含 90 天前折叠出的日桶。这里标注当前口径与数据起点。
  const winLabel = trangeLabel('usRange');
  // 服务端回显的实际区间优先（自定义区间下它就是权威口径）；滚动窗口没有回显，
  // 用控件自己的标签。
  const rangeEcho = usageData.window_from
    ? String(usageData.window_from).replace('T', ' ').slice(0, 16) +
      (usageData.window_to ? ' → ' + String(usageData.window_to).replace('T', ' ').slice(0, 16) : ' → 现在')
    : '';
  const note = (rangeEcho || winLabel ? (rangeEcho || winLabel) + ' · ' : '') +
    (usageData.buckets || 0) + ' 个分桶' +
    (usageData.since ? ' · 数据自 ' + usageData.since.replace('T', ' ') : '') +
    (usageData.file_bytes ? ' · 文件 ' + (usageData.file_bytes / 1024).toFixed(1) + ' KB' : '');
  const noteEl = $('usNote');
  if (noteEl) {
    noteEl.textContent = note;
    noteEl.title = note; // 窄屏单行截断时靠悬停看全
  } else {
    noteUsageDomMismatch('usNote');
  }

  // 积分扣除的四张卡片与说明。
  creditSpendStatsHtml =
    usKpi(fmtCredit(t.credits), '扣除积分', 'c-accent', '按上游 usage.credit 累计') +
    usKpi(fmtTok(t.credit_tokens), '匹配 Token', 'c-mute', '与积分同时观测到的 Token') +
    usKpi(fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens),
      '平均积分 / 1M Token', 'c-ok', '越低越划算') +
    usKpi(String(t.credit_samples || 0), '有效积分样本', 'c-mute', '缺字段的历史不参与折算') +
    usKpi(cacheRateText(t.cache_hit_tokens, t.cache_miss_tokens), '缓存命中率', 'c-mute',
      '上游前缀缓存命中 / (命中+未命中)；低命中意味着费用数倍放大');
  creditSpendNote =
    (usageData.credit_by_account || []).length + ' 个账号 · ' +
    (usageData.credit_by_model || []).length + ' 个模型倍率分组 · 仅统计与积分同时观测到的 Token';
  if (usCreditKind !== 'grant') {
    setHTML('usCreditStats', creditSpendStatsHtml);
    const spendNote = $('usCreditNote');
    if (spendNote) spendNote.textContent = creditSpendNote;
    else noteUsageDomMismatch('usCreditNote');
  }

  // 总览已经成功写入后，明细/流水/图表是可选增强块：任何一块因旧骨架缺节点
  // 抛错，都只能影响它自己，不能冒泡到 loadUsage 的 catch 把统计卡清空。
  const optionalRender = (name, fn) => {
    try { fn(); } catch (e) {
      // 缺块由各渲染函数自己调用 noteUsageDomMismatch；这里只兜住意外异常，
      // 保留控制台线索但不改写已成功渲染的总览。
      if (typeof console !== 'undefined' && console.warn) console.warn('[usage] ' + name + ' render failed', e);
    }
  };
  optionalRender('dim', renderUsageDim);
  optionalRender('credit', renderCreditDim);
  optionalRender('chart', () => renderUsageChart(usageData.series || []));
}

// 维度切换 / 排序控件。
if ($('usDimTabs')) $('usDimTabs').addEventListener('click', ev => {
  const target = ev.target && ev.target.closest ? ev.target.closest('button[data-dim]') : null;
  const b = target || (ev.currentTarget && ev.currentTarget.querySelector ? ev.currentTarget.querySelector('button[data-dim]') : null);
  if (!b) return;
  usDim = b.dataset.dim;
  usDimPage = 1;
  renderUsageDim();
});
if ($('usCreditTabs')) $('usCreditTabs').addEventListener('click', ev => {
  const target = ev.target && ev.target.closest ? ev.target.closest('button[data-dim]') : null;
  const b = target || (ev.currentTarget && ev.currentTarget.querySelector ? ev.currentTarget.querySelector('button[data-dim]') : null);
  if (!b) return;
  usCreditDim = b.dataset.dim;
  renderCreditDim();
});
if ($('usCreditKindTabs')) $('usCreditKindTabs').addEventListener('click', ev => {
  const target = ev.target && ev.target.closest ? ev.target.closest('button[data-kind]') : null;
  const b = target || (ev.currentTarget && ev.currentTarget.querySelector ? ev.currentTarget.querySelector('button[data-kind]') : null);
  if (!b) return;
  const kind = b.dataset.kind;
  if (kind === usCreditKind) return;
  usCreditKind = kind;
  // 两类流水共用同一张表：切换时必须同时作废另一个请求。否则“获取”的迟到
  // 响应会在“扣减”页签下覆盖表头与行，看起来像数据串档。
  creditHistSeq++;
  creditGrantSeq++;
  if (creditCtrl) { try { creditCtrl.abort(); } catch (e) { /* ignore */ } }
  if (creditGrantCtrl) { try { creditGrantCtrl.abort(); } catch (e) { /* ignore */ } }
  // 只改已有按钮的选中态；旧骨架/最小化 DOM 可能没有 querySelectorAll，
  // 不能因为一次 UI 同步把整次数据刷新打断。
  if (document && typeof document.querySelectorAll === 'function') {
    document.querySelectorAll('#usCreditKindTabs button[data-kind]').forEach(x => x.classList.toggle('on', x === b));
  }
  const head = $('usCreditHistHead');
  const table = $('usCreditHistTable');
  const empty = $('usCreditHistEmpty');
  const detail = $('usCreditDetail');
  const agg = $('usCreditAgg');
  const spendNote = $('usCreditNote');
  if (spendNote) spendNote.hidden = kind === 'grant';
  // 切页签先清旧表，避免快照请求返回前仍显示上一档的行（看起来像成功）。
  if (table) table.hidden = true;
  if (empty) { empty.hidden = false; empty.textContent = '正在读取…'; }
  if (kind === 'grant') {
    if (head) head.innerHTML = '<tr><th>发放时间</th><th>账号</th><th>包 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余 / 已用</th><th>到期</th><th>关联动作</th></tr>';
    if (detail) detail.hidden = true;
    if (agg) agg.hidden = true;
    loadCreditGrants();
    return;
  }
  if (detail) detail.hidden = false;
  if (agg) agg.hidden = false;
  if (empty) { empty.hidden = true; empty.textContent = ''; }
  if (usCreditKind === 'spend' || usCreditKind === 'all') {
    setHTML('usCreditStats', creditSpendStatsHtml);
    const spendNote = $('usCreditNote');
    if (spendNote) spendNote.textContent = creditSpendNote;
    else noteUsageDomMismatch('usCreditNote');
    if (head) head.innerHTML = '<tr><th>时间</th><th>粒度</th><th>账号</th><th>模型</th>' +
      '<th class="num">请求</th><th class="num">扣减积分</th><th class="num">Token</th></tr>';
  }
  loadCreditHistory();
});
if ($('usSort')) $('usSort').onchange = () => {
  usSort = $('usSort').value;
  usDimPage = 1;
  renderUsageDim();
};
let usQTimer = null;
if ($('usQ')) $('usQ').oninput = () => {
  clearTimeout(usQTimer);
  usQTimer = setTimeout(() => { usDimPage = 1; renderUsageDim(); }, 120);
};
if ($('usDimSize')) $('usDimSize').onchange = () => {
  usDimPageSize = Math.max(0, Number($('usDimSize').value) || 0);
  usDimPage = 1;
  renderUsageDim();
};
if ($('btnUsDimPrev')) $('btnUsDimPrev').onclick = () => { if (usDimPage > 1) { usDimPage--; renderUsageDim(); } };
if ($('btnUsDimNext')) $('btnUsDimNext').onclick = () => {
  renderUsageDim();
  const rows = (renderUsageDim.cache && renderUsageDim.cache.rows) || [];
  const size = usDimPageSize > 0 ? usDimPageSize : Math.max(1, rows.length);
  if (usDimPage < Math.ceil(rows.length / size)) { usDimPage++; renderUsageDim(); }
};

/* renderUsageChart 画堆叠柱状图。
 *
 * x 轴是**真实时间轴**，不是按序号等距。这一点很重要：数据里存在 1 小时的
 * 间隔，也存在 6~8 小时的断档（没请求的时段不产生桶），等距排布会把 8 小时
 * 画得和 1 小时一样宽，让「什么时候用的」完全失真。
 *
 * 另外不再用 preserveAspectRatio="none"：那会把 viewBox 横向拉伸到容器宽度，
 * 柱子和文字都变形。改为固定比例、按容器宽度自适应高度。
 *
 * viewBox 取 1200×200（原 760×180）：SVG 以 width:100% 渲染，高宽比决定实际
 * 高度——旧比例在 1500px 宽的主区里会撑到 ~355px，只有一两根柱子时整块几乎是
 * 空白。宽 viewBox 把同宽度下的高度压到 ~250px，与下方表格的视觉重量相当。
 *
 * 时间轴用本地时间解析（后端返回的就是本地时区），day 点按当天 00:00 参与定位，
 * 与 hour 点在同一个连续轴上——日桶本来就是他那天所有小时的聚合。
 */

/* parsePointTime 把后端的 t 解析成毫秒时间戳。 */
function parsePointTime(p) {
  // hour: "2026-09-16T13"  day: "2026-09-16"
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

/* fmtTokTimeLabel 时间桶的短标签，与 x 轴刻度同一口径（日桶 MM-DD，小时桶 HH:00）。 */
function fmtTokTimeLabel(p) {
  const d = new Date(p.t);
  return p.scope === 'day'
    ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
    : String(d.getHours()).padStart(2, '0') + ':00';
}

function renderUsageChart(series) {
  const host = $('usChart');
  const noteEl = $('usChartNote');
  if (!host) { noteDOM('usChart'); return; }

  // 丢掉时间解析不出来的点，而不是让 NaN 传染整张图。
  const pts = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    const pt = Number(p.prompt_tokens || 0);
    const ct = Number(p.completion_tokens || 0);
    pts.push({ t, scope: p.scope, raw: p.t, pt, ct, tt: Number(p.total_tokens || 0) || (pt + ct),
               req: p.requests || 0 });
  }
  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    if (noteEl) noteEl.textContent = '—';
    return;
  }

  const W = 1200, H = 200, PL = 58, PR = 14, PT = 18, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB;

  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);

  const max = Math.max(1, ...pts.map(p => p.tt));
  const peak = pts.reduce((a, b) => (b.tt > a.tt ? b : a), pts[0]);
  const avg = pts.reduce((s, p) => s + p.tt, 0) / pts.length;
  if (noteEl) noteEl.textContent =
    pts.length + ' 个点 · 峰值 ' + fmtTok(peak.tt) + ' @ ' + fmtTokTimeLabel(peak) +
    ' · 均值 ' + fmtTok(avg);

  // 柱宽取「最小真实间隔」的 70%，并夹在合理区间内——窗口拉到 30 天时柱子会
  // 变细，但不会细到看不见。
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(2, Math.min(30, slot * 0.7));

  // 首尾各让出半个柱宽：否则第一个点和最后一个点的柱子会各有一半跑到绘图区外
  // （末点柱子贴着卡片右边缘被切掉），刻度仍用同一个 xOf，标签与柱子始终对齐。
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
  const yOf = v => PT + ih - ih * (v / max);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" ' +
            'preserveAspectRatio="xMidYMid meet">';

  // 柱体渐变：顶部实、底部略透，堆叠时两段仍能一眼分清（纯色块并排会糊成一片）。
  // 注意 stop-color 必须走 style 而不是 presentation 属性——Blink/WebKit 不解析
  // 属性里的 var()，写成 stop-color="var(--accent)" 会整条渐变失效（柱子全透明）。
  out += '<defs>' +
    '<linearGradient id="usGradP" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--accent);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--accent);stop-opacity:.6"/></linearGradient>' +
    '<linearGradient id="usGradC" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--ok);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--ok);stop-opacity:.6"/></linearGradient>' +
    '</defs>';

  // y 轴网格 + 刻度
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
  }

  // 均值参考线：一眼看出"这根是不是异常高"，比只给刻度省心。
  // 标签放左侧：右侧常被峰值柱占用（峰值柱往往就是最后一根），贴左不会被压住。
  if (avg > 0 && avg < max) {
    const y = yOf(avg);
    out += '<line class="avg" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk-avg" x="' + (PL + 5) + '" y="' + (y - 4).toFixed(1) +
           '" text-anchor="start">均值 ' + fmtTok(avg) + '</text>';
  }

  // 柱子
  const yBase = PT + ih;
  for (const p of pts) {
    const x = xOf(p.t) - bw / 2;
    const hTot = ih * (p.tt / max);
    const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
    const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
    // 每根柱子包一个 <g>，把 <title> 放进去。
    //
    // 为什么必须包裹：SVG 里 <title> 描述的是它的**父元素**。此前 <title> 是
    // <rect> 的兄弟节点（rect 自闭合，无法包含子节点），于是全部平铺在 <svg>
    // 根下 —— 整张图只有一个 tooltip（浏览器取第一个），悬停任何柱子都显示同一
    // 份数据（issue #128）。包进 <g> 后 tooltip 跟随该柱，且堆叠的两段
    //（prompt + completion）共用同一个提示。
    out += '<g><title>' + esc(p.raw) + '  ' + fmtTok(p.pt) + ' prompt / ' +
           fmtTok(p.ct) + ' completion / ' + p.req + ' 次</title>';
    // 圆角只给堆叠顶端（贴轴的底边保持方角，柱子才像"立"在基线上）。
    // 类名用 usbar 而不是 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
    // height 是 rect 的 CSS 几何属性，同名类会把每根柱子压成 3px 高（踩过）。
    if (hP > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hP.toFixed(2) +
      '" fill="url(#usGradP)"' + (hC > 0 ? '' : ' rx="1.5"') + '/>';
    if (hC > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP - hC).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hC.toFixed(2) +
      '" fill="url(#usGradC)" rx="1.5"/>';
    out += '</g>';
  }

  // 峰值标注：柱子够窄时文字压在柱顶，够宽时贴右侧避免和柱体重叠。
  {
    const px = xOf(peak.t);
    const py = yOf(peak.tt);
    const anchor = px > W - PR - 90 ? 'end' : 'middle';
    out += '<text class="tk-peak" x="' + Math.max(PL, Math.min(W - PR, px)).toFixed(1) +
           '" y="' + Math.max(10, py - 5).toFixed(1) + '" text-anchor="' + anchor + '">' +
           '峰值 ' + fmtTok(peak.tt) + '</text>';
  }

  // x 轴基线画在柱子之后，避免压在柱底
  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
         '" y2="' + yBase + '"/>';

  // x 轴刻度：按真实时间等距取 6 个位置，取该位置**最近的实际柱子**做标签，
  // 所以标签永远落在有数据的点上，不会指到空档里。
  const TICKS = Math.min(6, pts.length);
  const usedLabel = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) { best = d; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = pts[bi];
    // 首尾标签靠边对齐，避免被裁掉
    const cx = xOf(p.t);
    const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
           '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' + esc(fmtTokTimeLabel(p)) + '</text>';
  }

  // 跨天时补一条日期分隔线，让「日界」在长窗口里可见
  let prevDay = null;
  for (const p of pts) {
    const d = new Date(p.t).getDate();
    if (prevDay !== null && d !== prevDay) {
      const x = xOf(p.t).toFixed(1);
      out += '<line class="gl" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
             (PT + ih) + '" style="opacity:.45"/>';
    }
    prevDay = d;
  }

  out += '</svg>';
  host.innerHTML = out;
}

let usageRateWarmAt = 0;
async function warmUsageModelRates() {
  if (Date.now() - usageRateWarmAt < 10 * 60 * 1000) return;
  try {
    await api('models');
  } catch (e) {
    // 倍率回填是可选增强；失败不阻塞用量统计，10 分钟后再试。
  }
  usageRateWarmAt = Date.now();
}

async function loadUsage() {
  // 请求序号守卫：用户快速切时间范围时多个请求在飞，先发的慢响应可能后到，
  // 把新窗口的数字覆盖回旧窗口（"越刷新越旧"）。只有最新一次请求可以渲染。
  const my = ++usageSeq;
  const btn = $('btnUsage');
  const btnLabel = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '读取中…'; btn.setAttribute('aria-busy', 'true'); }
  // 入口先确认用量页骨架存在：旧 index.html + 新 app.js 时整页都可能没有
  // #view-usage。此时既不该打上游，也不该在失败分支对 null 写 innerHTML；
  // 只提示一次版本错配并安静返回。旧版 2706 行就是这么炸出未处理异常的。
  if (!$('view-usage') && !$('usChart') && !$('usError') && !$('usNote')) {
    // 直接调用缺节点提示：本函数就是探针本身，若委托给 noteUsageDomMismatch
    // 会被它内部的「连提示落点都没有」分支静默吞掉，旧骨架反而没有任何反馈。
    noteDOM('view-usage');
    if (btn) { btn.disabled = false; btn.textContent = btnLabel; btn.removeAttribute('aria-busy'); }
    return;
  }
  // 入口级缺节点提示只发一次，但按钮仍保持可用：用户修复后（反代恢复/硬刷新）
  // 再点一次就能恢复；禁用按钮只会让页面看起来永久坏掉。
  if (usageCtrl) { try { usageCtrl.abort(); } catch (e) { /* 旧浏览器没有 abort 时忽略 */ } }
  usageCtrl = typeof AbortController === 'function' ? new AbortController() : null;
  // 子区块使用各自世代：切窗后旧的分位/流水请求不能晚到回写。先作废它们，
  // 再等 /usage 返回；离页时不启动新的归档/分片扫描。
  ++perfSeq;
  ++creditHistSeq;
  if (typeof creditGrantSeq === 'number') ++creditGrantSeq;
  if (perfCtrl) { try { perfCtrl.abort(); } catch (e) { /* ignore */ } }
  if (creditCtrl) { try { creditCtrl.abort(); } catch (e) { /* ignore */ } }
  if (typeof creditGrantCtrl !== 'undefined' && creditGrantCtrl) {
    try { creditGrantCtrl.abort(); } catch (e) { /* ignore */ }
  }
  const q = trangeQuery('usRange', true);
  try {
    const d = await api('usage?' + q.toString(), usageCtrl ? { signal: usageCtrl.signal } : {});
    if (my !== usageSeq) return;
    if (typeof view === 'string' && view !== 'usage') return;
    // renderUsage 内部按块隔离：缺一个可选节点只是跳过该块，不再让成功响应
    // 变成未处理异常。若整块骨架都不在，入口已经提前返回。
    renderUsage(d);
    loadRemaining();
    loadPerf();
    if (typeof usCreditKind !== 'undefined' && usCreditKind === 'grant') loadCreditGrants();
    else loadCreditHistory();
    warmUsageModelRates();
  } catch (e) {
    if (my !== usageSeq) return;
    // 被下一次 loadUsage 取消的旧请求不是错误：它没有拿到数据，也不该覆盖
    // 新请求即将渲染的内容。AbortError 若继续走下面的失败清理，会在
    // 「快速切范围」时先把 usDimTable/usCreditTable 写成失败行；新请求随后
    // 成功也会短暂闪一下错误，若新请求先失败则留下错误来源错位的提示。
    if (isAbortError(e)) return;
    // 只有请求本身失败才清空总览。renderUsage 内部已经按块隔离渲染异常；
    // 若异常从这里冒出来而非 network/API 错误，保留已有的成功数字比整页清空
    // 更符合用户预期，也避免旧骨架缺一个可选块就丢掉整张统计卡。
    // 这里也可能是旧脚本片段/最小沙箱：未声明 usageData 时读取同样会抛
    // ReferenceError。用 typeof 探针，绝不能把“清理失败态”变成新异常。
    if (typeof usageData !== 'undefined' && usageData && e && e.__render) return;
    // 失败时三块都要清干净：只改图表会留下上一次窗口的数字，看起来像"刷新成功"。
    // 失败清理也必须在旧骨架/最小化测试沙箱里自保：usageData 是顶层 let，
    // 若脚本被按片段求值（或旧版本尚未声明该变量），直接赋值会抛
    // ReferenceError，反而把一次可恢复的读取失败变成未处理异常。
    try { usageData = null; } catch (e) { /* 片段/旧骨架环境：没有该状态变量 */ }
    // 所有写入都经过空节点保护：升级后的页面可能没有旧版用量区块（或浏览器
    // 缓存了旧 index.html）。失败清理本身绝不能成为新的 innerHTML-null 崩溃源。
    const failHTML = '<div class="us-empty">读取用量失败：' + esc(e && e.message ? e.message : String(e)) + '</div>';
    // 错误只落在仍存在的专用错误位/图表上；旧骨架没有这些区块时，用一次页面级
    // 版本提示代替「对 null 写 innerHTML」。这里复用一个探针函数，保证新增
    // 区块时不会再漏掉空节点判断。
    let shown = false;
    shown = applyNode('usError', el => { el.innerHTML = failHTML; el.hidden = false; }) || shown;
    shown = applyNode('usChart', el => { el.innerHTML = failHTML; }) || shown;
    if (!shown) noteUsageDomMismatch('usage-error');
    clearNode('usChartNote', 'textContent');
    clearNode('usStats', 'innerHTML');
    clearNode('usCreditStats', 'innerHTML');
    clearNode('usNote', 'textContent');
    clearNode('usCreditNote', 'textContent');
    clearNode('usDimNote', 'textContent');
    clearNode('usDimHead', 'innerHTML');
    clearNode('usCreditHead', 'innerHTML');
    // 页签按钮按维度重绘；只用一次节点引用，成功后不再写 innerHTML。
    applyNode('usDimTabs', el => { el.innerHTML = usTabsHtml(US_DIM_TABS, usDim, null); });
    applyNode('usCreditTabs', el => { el.innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, null); });
    applyNode('usDimBody', el => { el.innerHTML = '<tr><td colspan="10" class="empty">读取用量失败</td></tr>'; });
    applyNode('usCreditBody', el => { el.innerHTML = '<tr><td colspan="7" class="empty">读取用量失败</td></tr>'; });
  } finally {
    // 按钮状态属于本次请求而非全局：只有仍是最新请求才恢复，避免旧请求的
    // finally 把新请求的“读取中…”提前清掉。取消/失败/成功都走同一出口。
    if (my === usageSeq && btn) {
      btn.disabled = false;
      btn.textContent = btnLabel || '刷新';
      btn.removeAttribute('aria-busy');
    }
  }
}

// 页面骨架与脚本版本错配时（浏览器缓存旧 index.html、反代只替换二进制等），
// 直接 `$('id').onclick=` 会在顶层抛 TypeError，整份 app.js 停止执行，后续
// 所有视图都不再可用。入口绑定一律先判断元素是否存在。
if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
if ($('btnUsageReload')) $('btnUsageReload').onclick = loadUsage;
if ($('btnPerfReload')) $('btnPerfReload').onclick = loadPerf;
if ($('btnCreditHist')) $('btnCreditHist').onclick = loadCreditHistory;
// 时间范围控件绑定：任何改动（预设切换 / 自定义起止）都重新拉一次用量。
if ($('usRange')) trangeBind('usRange', loadUsage);

/* ── 活动记录 ─────────────────────────────────────────────────────── */
/* 只读活动历史：签到/活跃/旅行/保活每次真实尝试。筛选全部由服务端按同一口径
   执行（task/result/uid/from	o），前端不缓存旧页结果，避免“看起来筛选了”。 */
const ACT_TASK_LABEL = { checkin: '签到', activity: '活跃上报', travel: '猫猫旅行', keepalive: 'Token 保活', daily_chat: '国际版每日活跃' };
const ACT_TRIGGER_LABEL = { schedule: '定时', manual: '手动', unknown: '未知' };
function actRowKey(r) {
  r = r || {};
  return [r.ts, r.nickname, r.realm, r.task, r.trigger, r.ok, r.message].join('\u0000');
}
function actRowHtml(r) {
  const ok = !!r.ok;
  const task = ACT_TASK_LABEL[r.task] || r.task || '—';
  const trigger = ACT_TRIGGER_LABEL[r.trigger] || r.trigger || '—';
  const when = r.ts ? fmtLogTime(r.ts) : '—';
  const name = r.nickname || '未命名账号';
  const msg = r.message || '';
  return '<tr' + (ok ? '' : ' class="off"') + '><td class="mark"><i></i></td>' +
    '<td data-label="时间" class="num">' + esc(when) + '</td>' +
    '<td data-label="账号">' + esc(name) +
    (r.realm ? '<div class="note">' + esc(r.realm) + '</div>' : '') + '</td>' +
    '<td data-label="任务">' + esc(task) + '</td><td data-label="触发">' + esc(trigger) + '</td>' +
    '<td data-label="结果"><span class="tag ' + (ok ? 'ok' : 'bad') + '">' + (ok ? '成功' : '失败') + '</span></td>' +
    '<td data-label="消息" title="' + esc(msg) + '"><span class="msg-clip">' + esc(msg || '—') + '</span></td></tr>';
}
function actEmptyHtml(d) {
  // 空态分两种：有历史但筛选无命中 vs 从未执行任务。后者给下一步动作，
  // 避免用户把「筛选条件太窄」误判成功能坏了。
  const txt = d.total ? '没有符合筛选条件的记录，试试放宽时间或任务类型'
    : '暂无活动记录；执行签到 / 活跃上报 / 保活后会在这里留下每次尝试';
  return '<tr><td colspan="7" class="empty">' + txt + '</td></tr>';
}
function renderActivity(d, opts) {
  d = d || {};
  const incoming = Array.isArray(d.records) ? d.records : [];
  const append = !!(opts && opts.append);
  let added = incoming;
  if (!append) actRows = incoming.slice();
  else {
    const seen = new Set(actRows.map(actRowKey));
    added = [];
    for (const r of incoming) {
      const key = actRowKey(r);
      if (seen.has(key)) continue;
      seen.add(key);
      actRows.push(r);
      added.push(r);
    }
  }
  const note = $('actNote');
  const body = $('actBody');
  if (body) {
    if (!append || !body.insertAdjacentHTML) body.innerHTML = actRows.map(actRowHtml).join('') || actEmptyHtml(d);
    else {
      const placeholder = body.querySelector && body.querySelector('.empty');
      const placeholderRow = placeholder && placeholder.closest ? placeholder.closest('tr') : null;
      if (placeholderRow) placeholderRow.remove();
      if (added.length) body.insertAdjacentHTML('beforeend', added.map(actRowHtml).join(''));
      if (!actRows.length) body.innerHTML = actEmptyHtml(d);
    }
  }
  if (note) note.textContent = actRows.length + ' 条' +
    (d.has_more ? ' / 共 ' + Number(d.total || 0) + ' 条' : '') +
    (d.total ? '' : ' · 尚未执行任务');
  const more = $('btnActMore');
  if (more) {
    more.disabled = false;
    more.textContent = d.has_more
      ? '加载更多（已显示 ' + actRows.length + ' / ' + Number(d.total || 0) + '）'
      : '已加载全部';
    const bar = more.parentElement;
    if (bar) bar.hidden = !d.has_more;
  }
}
async function loadActivity(more) {
  // 与配置页同理：切页、刷新、筛选同时触发时共享同一个在途请求；不同筛选
  // 条件会改变 URL，不能用旧结果顶替，故仅在完全相同的查询上复用。
  const q = trangeQuery('actRange', false);
  if (more !== true) { actLimit = 200; actRows = []; }
  const prevLimit = actLimit;
  if (more === true) actLimit = Math.min(1000, actLimit + 200);
  q.set('limit', String(actLimit));
  const task = $('actTask') && $('actTask').value;
  const result = $('actResult') && $('actResult').value;
  if (task) q.set('task', task);
  if (result) q.set('result', result);
  const search = $('actQ') && String($('actQ').value || '').trim();
  if (search) q.set('q', search.slice(0, 64));
  const uid = $('actUID') && String($('actUID').value || '').trim();
  if (uid) q.set('uid', uid.slice(0, 32));
  const path = 'activity/history?' + q.toString();
  if (actReq && actReq.path === path) return actReq.promise;
  const my = ++actSeq;
  if (actCtrl) { try { actCtrl.abort(); } catch (e) { /* ignore */ } }
  actCtrl = typeof AbortController === 'function' ? new AbortController() : null;
  const req = { path, promise: null };
  req.promise = (async () => {
    try {
      const d = await api(path, actCtrl ? { signal: actCtrl.signal } : {});
      if (my !== actSeq || view !== 'activity') return;
      renderActivity(d || {}, { append: more === true });
    } catch (e) {
      // 加载更多失败时回滚窗口：否则 200→400 失败后下一次重试直接变成
      // 401–600，永久跳过 201–400，连续失败最终卡死在 1000。回滚后重试
      // 仍是同一窗口请求。
      if (more === true) actLimit = prevLimit;
      if (my !== actSeq || view !== 'activity') return;
      // 「加载更多」失败不能清掉已经看见的旧行；只在表底追加可重试提示。
      // 首次加载失败才用整表错误态。
      const body = $('actBody');
      const msg = '读取活动记录失败：' + esc(e && e.message ? e.message : String(e));
      if (more === true && body) {
        // 加载更多失败时无论窗口多大都保留已看见的行；第一次（200）只在
        // 按钮上给出重试入口，避免把整表换成一行错误把上下文全清掉。
        if (prevLimit > 200) {
          const old = body.querySelector && body.querySelector('.act-more-err');
          if (old) old.remove();
          body.insertAdjacentHTML('beforeend', '<tr class="act-more-err"><td colspan="7" class="empty err">' + msg + '（已保留已加载记录，可重试）</td></tr>');
        }
      } else if (body) {
        body.innerHTML = '<tr><td colspan="7" class="empty err">' + msg + '</td></tr>';
      }
      const note = $('actNote');
      if (note) note.textContent = '读取失败';
      const btnMore = $('btnActMore');
      if (btnMore) {
        // 只要服务端可能还有下一页（more === true），失败后就必须保留按钮并
        // 改成重试文案。此前可见条件要求 prevLimit > 200，首次「加载更多」
        // 从 200 起步，失败后按钮直接消失，用户只能整页刷新才能再试。
        btnMore.disabled = false;
        btnMore.textContent = more === true ? '重试加载更多' : '加载更多';
        if (btnMore.parentElement) btnMore.parentElement.hidden = more !== true;
      }
    } finally {
      if (actReq === req) actReq = null;
    }
  })();
  actReq = req;
  return req.promise;
}
let actReq = null;
function renderActivityAccountOptions() {
  const sel = $('actUID');
  if (!sel || !accAll) return;
  const cur = sel.value || '';
  const opts = ['<option value="">全部账号</option>'].concat((accAll || []).map(a =>
    '<option value="' + esc(String(a && a.uid || '').slice(0, 8)) + '">' +
    esc(((a && a.nickname) || '未命名') + ' · ' + String(a && a.uid || '').slice(0, 8)) + '</option>')).join('');
  if (sel.innerHTML !== opts) sel.innerHTML = opts;
  sel.value = cur;
}
if ($('btnActivityReload')) $('btnActivityReload').onclick = loadActivity;
if ($('actTask')) $('actTask').onchange = loadActivity;
if ($('actResult')) $('actResult').onchange = loadActivity;
let actQTimer = null;
if ($('actQ')) $('actQ').oninput = () => {
  clearTimeout(actQTimer);
  actQTimer = setTimeout(loadActivity, 180);
};
if ($('actUID')) $('actUID').onchange = () => loadActivity();
if ($('btnActMore')) $('btnActMore').onclick = () => loadActivity(true);
if ($('actRange')) trangeBind('actRange', loadActivity, '168');

/* ── 任务与福利 ─────────────────────────────────────────────────────── */
let benefitsReq = null;
let benefitsSeq = 0;
let benefitsBusy = new Set();
let lastBenefits = null;
// Benefits table virtualization mirrors the account table: only the current
// viewport is written to the DOM, while the full filtered list stays in
// benefitsRows. The 5s snapshot refresh must not reset the scroll position.
let benefitsRows = [];
let benefitsRowH = 56;
let benefitsVirtualBound = false;
let benefitsBatchTimer = null;
let benefitsBatchSeq = 0;
// 当前聚焦的国内个人号（服务端短引用）。focus 随请求发送，页面只保存短引用，
// 完整 UID 始终留在服务端；空值表示由后端选择第一个可用国内号。
let benefitsFocusRef = 'all';
let benefitsTaskData = null;
let benefitsRunSeq = 0;
let benefitsLogFilter = { task: '', result: '', ref: '' };
let benefitsTaskRows = [];
let benefitsFocusLast = null;
let benefitsBatchDoneSig = '';
// 福利弹窗当前绑定的服务端短引用。任务动作仍允许 accept/claim/auto；这些
// 请求改走 benefits/* 路由，完整 UID 始终留在服务端。
let benefitsTaskRef = null;

function benefitRealmLabel(realm) {
  return realm === 'global' ? '国际版' : '国内版';
}

function benefitsVirtualWindow() {
  const rows = benefitsRows;
  const vp = $('bfViewport');
  if (!vp || !rows.length) {
    return { start: 0, end: rows.length, topPad: 0, bottomPad: 0 };
  }
  // Narrow/未布局的视口必须回落到整页渲染；否则 scrollTop=0、clientHeight=0
  // 时窗口会被压成 0 行，页面显示空白。
  const rowH = Math.max(28, Number(benefitsRowH) || 56);
  const viewportH = Number(vp.clientHeight) || 0;
  if (viewportH <= 0) {
    return { start: 0, end: rows.length, topPad: 0, bottomPad: 0 };
  }
  const first = Math.max(0, Math.floor(Number(vp.scrollTop || 0) / rowH) - 5);
  const count = Math.ceil(viewportH / rowH) + 12;
  const end = Math.min(rows.length, first + count);
  return { start: first, end, topPad: first * rowH, bottomPad: Math.max(0, (rows.length - end) * rowH) };
}

function bindBenefitsVirtualScroll() {
  if (benefitsVirtualBound) return;
  const vp = $('bfViewport');
  if (!vp || typeof vp.addEventListener !== 'function') return;
  benefitsVirtualBound = true;
  let raf = 0;
  vp.addEventListener('scroll', () => {
    if (raf) return;
    const run = () => {
      raf = 0;
      if (!benefitsRows.length) return;
      renderBenefitsAccounts(benefitsRows);
    };
    raf = typeof requestAnimationFrame === 'function'
      ? requestAnimationFrame(run) : setTimeout(run, 16);
  }, { passive: true });
  if (typeof window !== 'undefined' && window.addEventListener) {
    window.addEventListener('resize', () => {
      if (!benefitsRows.length) return;
      renderBenefitsAccounts(benefitsRows);
    }, { passive: true });
  }
}

function renderBenefitsAccounts(rows) {
  const body = $('bfAccountsBody');
  if (!body) return;
  rows = Array.isArray(rows) ? rows : [];
  benefitsRows = rows;
  const note = $('bfAcctNote');
  if (note) note.textContent = rows.length ? rows.length + ' 个账号' : '';
  const vp = $('bfViewport');
  const scrollTop = vp ? Number(vp.scrollTop || 0) : 0;
  if (!rows.length) {
    body.innerHTML = '<tr><td colspan="10"><div class="empty">暂无账号；先在账号池添加或导入凭证。</div></td></tr>';
    return;
  }
  const virtual = benefitsVirtualWindow();
  const rowHtml = rows.slice(virtual.start, virtual.end).map(a => {
    const who = a.nickname ? esc(a.nickname) : esc(a.uid || '—');
    const realm = benefitRealmLabel(a.realm);
    const checkin = a.realm === 'global'
      ? '<span class="tag mute">无签到体系</span>'
      : (a.enterprise ? '<span class="tag mute">企业号</span>'
        : (a.checkin_done ? '<span class="tag ok">已签到</span>' : '<span class="tag warn">待签到</span>'));
    const pending = a.growth_pending ? a.growth_pending + ' 项' : (a.enterprise || a.realm === 'global' ? '—' : '0 项');
    const daily = a.realm === 'global' && !a.enterprise
      ? (a.daily_chat_today ? '<span class="tag ok">已活跃</span>' : '<span class="tag warn">待活跃</span>')
      : '<span class="tag mute">—</span>';
    const growth = a.growth_available
      ? ('连登 ' + (a.streak_days || 0) + ' 天 · 补签 ' + (a.makeup_cards || 0) +
        ' · 抽奖 ' + (a.lottery_chances || 0) + ' · 能量 ' + fmtTok(a.energy || 0))
      : (a.growth_error ? '<span class="tag bad" title="' + esc(a.growth_error) + '">查询失败</span>' : '—');
    const cat = a.growth_available
      ? (a.cat_state ? esc(a.cat_state) + (a.cat_reward ? ' · +' + fmtTok(a.cat_reward) : '') : '—')
      : '—';
    const lastTask = a.last_task ? benefitTaskLabel(a.last_task) : '—';
    const result = a.last_task
      ? (a.last_ok ? '<span class="tag ok">成功</span>' : '<span class="tag bad">失败</span>')
      : '—';
    const ref = a.ref || '';
    const busy = ref && benefitsBusy.has(ref);
    const btn = (act, label, cls, title) => ref
      ? '<button type="button" class="xs ' + (cls || '') + '" data-bf-act="' + esc(act) +
        '" data-bf-ref="' + esc(ref) + '"' + (busy && act !== 'tasks' ? ' disabled' : '') +
        (title ? ' title="' + esc(title) + '"' : '') + '>' + esc(label) + '</button>'
      : '';
    let acts = '';
    if (!a.enterprise && a.realm !== 'global') {
      acts += btn('checkin', '签到', '', '执行 Buddy 签到并刷新余额');
      acts += btn('streak', '连登', '', '连登兑换与抽奖');
      acts += btn('travel', '旅行', '', '猫猫旅行巡检');
      acts += btn('tasks', '成长任务', '', '查看成长任务队列');
    } else if (a.realm === 'global' && !a.enterprise) {
      acts += btn('daily_chat', '每日活跃', 'primary', '国际版每日活跃对话');
      acts += btn('daily_chat_web', '网页打卡', '', '仅走网页通道补打每日活跃');
    }
    if (!acts) acts = '—';
    return '<tr>' +
      '<td><div class="nm">' + who + '</div><div class="id">' + esc(a.uid || '') + '</div></td>' +
      '<td><span class="realm-tag">' + esc(realm) + '</span></td>' +
      '<td>' + checkin + '</td>' +
      '<td>' + daily + '</td>' +
      '<td class="num">' + esc(pending) + '</td>' +
      '<td>' + growth + '</td>' +
      '<td>' + cat + '</td>' +
      '<td>' + (a.last_task ? '<div>' + esc(lastTask) + '</div><div class="note">' + esc(benefitsTime(a.last_at)) + '</div>' : '—') + '</td>' +
      '<td>' + result + '</td>' +
      '<td class="bf-acts">' + acts + '</td>' +
      '</tr>';
  });
  let bodyHtml = '';
  if (virtual.topPad > 0) bodyHtml += '<tr class="virtual-pad" aria-hidden="true"><td colspan="10"><i style="height:' + virtual.topPad + 'px"></i></td></tr>';
  bodyHtml += rowHtml.join('');
  if (virtual.bottomPad > 0) bodyHtml += '<tr class="virtual-pad" aria-hidden="true"><td colspan="10"><i style="height:' + virtual.bottomPad + 'px"></i></td></tr>';
  body.innerHTML = bodyHtml;
  if (vp) {
    try { vp.scrollTop = scrollTop; } catch (e) { /* inert DOM */ }
    const firstRow = typeof body.querySelector === 'function' ? body.querySelector('tr') : null;
    const measured = firstRow && firstRow.getBoundingClientRect
      ? Math.round(firstRow.getBoundingClientRect().height || 0) : 0;
    if (measured > 24 && measured < 400 && Math.abs(measured - benefitsRowH) > 1) {
      benefitsRowH = measured;
      renderBenefitsAccounts(rows);
      return;
    }
  }
  bindBenefitsVirtualScroll();
}

// renderBenefitFocus 渲染 Hub 式国内账号选择器与成长任务明细。账号选项的 value
// 是服务端短引用，text 只显示昵称 + uid8；用户切换后按 focus 重新拉取快照。
function renderBenefitFocus(d) {
  const sel = $('bfGrowthAcct');
  const accounts = Array.isArray(d && d.accounts) ? d.accounts.filter(a => a && a.realm === 'cn' && !a.enterprise && !a.disabled) : [];
  benefitsFocusLast = d || null;
  benefitsTaskData = d || null;
  if (sel) {
    let want = benefitsFocusRef || 'all';
    if (want !== 'all' && !accounts.some(a => a.ref === want)) want = 'all';
    benefitsFocusRef = want;
    const opts = ['<option value="all">全部国内账号（批量）</option>'].concat(
      accounts.map(a => '<option value="' + esc(a.ref || '') + '">' +
        esc((a.nickname || '未命名') + ' · ' + String(a.uid || '').slice(0, 8)) + '</option>'));
    const next = opts.join('');
    if (sel.innerHTML !== next) sel.innerHTML = next;
    sel.value = want;
    sel.disabled = !accounts.length;
  }
  const summary = (d && d.summary) || {};
  const cur = benefitsFocusRef === 'all' ? null : accounts.find(a => a.ref === benefitsFocusRef) || null;
  const sum = $('bfFocusSummary');
  if (sum) {
    if (!cur) {
      const bits = [
        '范围 ' + (summary.accounts || accounts.length) + ' 个国内账号',
        '今日签到 ' + (summary.checkin_done || 0) + ' / ' + ((summary.checkin_done || 0) + (summary.checkin_pending || 0)),
        '待签到 ' + (summary.checkin_pending || 0),
      ];
      sum.innerHTML = bits.map(x => '<span class="bf-chip">' + esc(x) + '</span>').join('');
    } else {
      const bits = [
        '连登 ' + (summary.streak_days || 0) + ' 天',
        '补签卡 ' + (summary.makeup_cards || 0),
        '抽奖 ' + (summary.lottery_chances || 0) + ' 次',
        '能量 ' + fmtTok(summary.energy || 0),
        '猫猫 ' + ((summary.cat_daily_done ? '今日已完成' : (summary.cat_reward ? '本轮 +' + summary.cat_reward : '待巡检'))),
      ];
      if (cur.growth_error) bits.push('查询：' + cur.growth_error);
      sum.innerHTML = bits.map(x => '<span class="bf-chip">' + esc(x) + '</span>').join('');
    }
  }
  renderBenefitTaskRows(cur ? (d && d.tasks) : [], cur, d && d.tasks_error);
  const disabled = !benefitsFocusRef;
  ['bfBtnFocusCheckin', 'bfBtnFocusRun', 'bfBtnFocusTravel', 'bfBtnFocusStreak'].forEach(id => {
    const b = $(id);
    if (b) b.disabled = disabled;
  });
}

function benefitTaskStatus(t) {
  if (t.claimed) return '<span class="tag ok">已领取</span>';
  if (t.claimable || (t.target > 0 && t.current >= t.target)) return '<span class="tag warn">待领奖</span>';
  if (t.locked) return '<span class="tag mute">未解锁</span>';
  if (t.accept_status === 'accepted' || t.accept_status === 'in_progress') return '<span class="tag mute">进行中</span>';
  return '<span class="tag mute">未接取</span>';
}

function renderBenefitTaskRows(rows, account, err) {
  const body = $('bfTaskBody');
  if (!body) return;
  benefitsTaskRows = Array.isArray(rows) ? rows : [];
  if (!account) {
    body.innerHTML = '<tr><td colspan="5"><div class="empty">已选择全部国内账号；选择单个账号可查看成长任务明细。</div></td></tr>';
    return;
  }
  if (err) {
    body.innerHTML = '<tr><td colspan="5"><div class="empty err">读取成长任务失败：' + esc(err) + '</div></td></tr>';
    return;
  }
  if (!benefitsTaskRows.length) {
    body.innerHTML = '<tr><td colspan="5"><div class="empty">该账号暂无成长任务；可直接点「一键做任务」或刷新。</div></td></tr>';
    return;
  }
  body.innerHTML = benefitsTaskRows.map(t => {
    const code = t.task_code || '';
    const cur = t.current ?? 0, tgt = t.target ?? 0;
    const prog = tgt ? cur + ' / ' + tgt : (cur > 0 ? String(cur) : '—');
    const reward = (t.credit ? '+' + t.credit + ' 分' : '') + (t.energy ? (t.credit ? ' · ' : '') + '+' + t.energy + ' 能' : '');
    const desc = t.task_desc || t.description || '';
    const tip = [t.title, desc, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
    return '<tr title="' + esc(tip) + '">' +
      '<td><div class="nm">' + esc(t.title || code) + '</div><div class="id">' + esc(code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
      '<td class="task-desc">' + esc(desc || '—') + '</td>' +
      '<td class="task-prog">' + esc(prog) + '</td>' +
      '<td>' + esc(reward || '—') + '</td>' +
      '<td>' + benefitTaskStatus(t) + '</td></tr>';
  }).join('');
}

function benefitTaskLabel(task) {
  return ({checkin: '签到', activity: '活跃上报', travel: '猫猫旅行', keepalive: 'Token 保活', daily_chat: '每日活跃'})[task] || task || '—';
}

function benefitsTime(v) {
  if (!v) return '—';
  const d = new Date(v);
  if (isNaN(d.getTime())) return String(v);
  return d.toLocaleString('zh-CN', {month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit'});
}

function renderBenefitGrants(rows, summary) {
  const body = $('bfGrantsBody');
  if (!body) return;
  rows = Array.isArray(rows) ? rows : [];
  const s = summary || {};
  const note = $('bfGrantNote');
  if (note) {
    note.textContent = !s.welfare_grants_available
      ? '尚未刷新积分包：先在积分构成页刷新'
      : (s.welfare_snapshot_at ? '快照 ' + benefitsTime(s.welfare_snapshot_at) : (rows.length + ' 笔'));
  }
  if (!rows.length) {
    body.innerHTML = '<tr><td colspan="7"><div class="empty">' +
      (s.welfare_grants_available ? '暂无福利发放记录' : '暂无积分包快照；刷新积分后可查看') +
      '</div></td></tr>';
    return;
  }
  body.innerHTML = rows.map(r => {
    const status = ({active: '生效中', available: '可用', used: '已用完', used_up: '已用完', expired: '已过期'})[r.status] || String(r.status || '—');
    const tone = (r.status === 'expired' || r.status === 'used' || r.status === 'used_up') ? 'bad' : (r.status === 'active' ? 'ok' : '');
    const expiry = r.no_expiry ? '不过期' : String(r.expire_at || '').slice(0, 10) || '—';
    const source = r.grant_reason || r.name || '—';
    return '<tr>' +
      '<td>' + esc(String(r.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
      '<td title="' + esc(r.uid || '') + '">' + esc(r.nickname || r.uid || '—') + '</td>' +
      '<td title="' + esc(source) + '">' + esc(source) + '</td>' +
      '<td class="num">+' + fmtTok(r.size || 0) + '</td>' +
      '<td class="num">' + fmtTok(r.remain || 0) + '</td>' +
      '<td>' + esc(expiry) + '</td>' +
      '<td><span class="tag ' + tone + '">' + esc(status) + '</span></td>' +
      '</tr>';
  }).join('');
}

const BENEFIT_BATCH_LABELS = {
  checkin: '全部签到', travel: '旅行巡检', activity: '活跃上报',
  keepalive: 'Token 保活', daily_chat: '国际版每日活跃', streak: '连登与抽奖',
};

function renderBenefitBatches(runs) {
  const box = $('bfBatchList');
  if (!box) return;
  runs = Array.isArray(runs) ? runs : [];
  const note = $('bfBatchNote');
  if (note) note.textContent = runs.length ? '最近 ' + runs.length + ' 轮' : '—';
  if (!runs.length) {
    box.innerHTML = '<div class="empty">尚未执行批量动作</div>';
    return;
  }
  box.innerHTML = runs.map(r => {
    const label = BENEFIT_BATCH_LABELS[r.kind] || r.kind || '批量动作';
    const status = r.running ? '<span class="tag warn">执行中</span>'
      : (r.note === '执行状态未知（可能已重启）' ? '<span class="tag mute">状态未知</span>'
        : (r.failed ? '<span class="tag bad">' + r.failed + ' 失败</span>' : '<span class="tag ok">完成</span>'));
    const counts = r.done ? ('成功 ' + (r.ok || 0) + ' · 失败 ' + (r.failed || 0) + ' · 共 ' + (r.total || 0)) : '正在逐一执行';
    return '<div class="bf-batch-row">' +
      '<div><div class="bf-batch-k">' + esc(label) + '</div><div class="bf-batch-meta">' +
      esc(benefitsTime(r.started_at)) + (r.note ? ' · ' + esc(r.note) : '') + '</div></div>' +
      '<div class="bf-batch-stat">' + status + '<div class="bf-batch-meta">' + esc(counts) + '</div></div>' +
      '</div>';
  }).join('');
}

async function loadBenefitBatches() {
  const my = ++benefitsBatchSeq;
  try {
    const d = await api('benefits/batches');
    if (my !== benefitsBatchSeq || view !== 'taskscenter') return;
    const runs = (d && d.runs) || [];
    renderBenefitBatches(runs);
    if (runs.some(r => r && r.running)) {
      if (!benefitsBatchTimer) benefitsBatchTimer = setTimeout(() => {
        benefitsBatchTimer = null;
        loadBenefitBatches();
      }, 3000);
    } else if (runs.length && !benefitsBatchDoneSig) {
      // 批量刚结束：活动记录与账号状态已变化，自动刷新一次福利快照和流水。
      benefitsBatchDoneSig = runs.map(r => r.kind + ':' + r.started_at + ':' + (r.failed || 0)).join('|');
      loadBenefits(true);
      loadBenefitLog();
    } else if (runs.length) {
      const sig = runs.map(r => r.kind + ':' + r.started_at + ':' + (r.failed || 0)).join('|');
      if (sig !== benefitsBatchDoneSig) { benefitsBatchDoneSig = sig; loadBenefits(true); loadBenefitLog(); }
    }
  } catch (e) {
    const box = $('bfBatchList');
    if (box && view === 'taskscenter') box.innerHTML = '<div class="empty">读取执行结果失败：' + esc(e.message) + '</div>';
  }
}

function renderBenefitLog(rows) {
  const box = $('bfLogList');
  if (!box) return;
  rows = Array.isArray(rows) ? rows : [];
  const note = $('bfLogNote');
  if (note) note.textContent = rows.length ? '最近 ' + rows.length + ' 条' : '—';
  if (!rows.length) {
    box.innerHTML = '<div class="empty">暂无执行记录；手动执行签到/任务/旅行后会出现在这里</div>';
    return;
  }
  box.innerHTML = rows.map(r => {
    const task = benefitTaskLabel(r.task);
    const when = benefitsTime(r.ts);
    const who = r.nickname || (r.realm === 'global' ? '国际账号' : '国内账号');
    const msg = r.message || (r.ok ? '完成' : '失败');
    const tone = r.ok ? 'ok' : 'bad';
    const trigger = r.trigger === 'manual' ? '手动' : (r.trigger === 'scheduler' || r.trigger === 'schedule' ? '排程' : '');
    return '<div class="bf-log-row">' +
      '<span class="t">' + esc(when) + '</span>' +
      '<span class="n" title="' + esc(who) + '">' + esc(who) + '</span>' +
      '<span class="m"><span class="tag ' + tone + '">' + (r.ok ? '成功' : '失败') + '</span> ' +
      esc(task) + (trigger ? ' · ' + esc(trigger) : '') + ' · ' + esc(msg) + '</span>' +
      '</div>';
  }).join('');
}

async function loadBenefitLog() {
  try {
    const qs = ['limit=40'];
    if (benefitsLogFilter.task) qs.push('task=' + encodeURIComponent(benefitsLogFilter.task));
    if (benefitsLogFilter.result) qs.push('result=' + encodeURIComponent(benefitsLogFilter.result));
    if (benefitsLogFilter.ref) qs.push('ref=' + encodeURIComponent(benefitsLogFilter.ref));
    const d = await api('benefits/log?' + qs.join('&'));
    if (view !== 'taskscenter') return;
    renderBenefitLog(d && d.rows);
  } catch (e) {
    const box = $('bfLogList');
    if (box && view === 'taskscenter') box.innerHTML = '<div class="empty">读取执行流水失败：' + esc(e.message) + '</div>';
  }
}


// renderWelfareSummary puts the welfare totals into the current Hub-style
// task page. The old summary cards are gone, so their ids must not be probed
// as if the page were stale.
function renderWelfareSummary(s) {
  const note = $('bfActionNote');
  if (!note) return;
  const bits = [
    '账号 ' + (s.accounts || 0),
    '今日签到 ' + (s.checkin_done || 0) + ' / ' + ((s.checkin_done || 0) + (s.checkin_pending || 0)),
  ];
  if (s.welfare_grants_available) {
    bits.push('福利积分 +' + fmtTok(s.welfare_credits_total || 0));
    bits.push((s.welfare_grant_count || 0) + ' 笔发放');
  } else {
    bits.push('福利积分未刷新');
  }
  note.textContent = bits.join(' · ');
}

function renderBenefits(d) {
  lastBenefits = d || {};
  const s = (d && d.summary) || {};
  // The Hub-style task page no longer has the old summary cards. Only update
  // them when an older embedded skeleton actually provides them; calling
  // setText unconditionally makes a correct new page report a stale-version
  // error for every removed card.
  const legacyCards = {
    bfAccounts: String(s.accounts || 0),
    bfAccountsSub: '国内 ' + (s.cn || 0) + ' / 国际 ' + (s.global || 0) + (s.enterprise ? ' · 企业 ' + s.enterprise : ''),
    bfCheckinDone: String(s.checkin_done || 0),
    bfCheckinSub: '待签到 ' + (s.checkin_pending || 0),
    bfDailyChatDone: String(s.daily_chat_done || 0),
    bfDailyChatSub: '待活跃 ' + (s.daily_chat_pending || 0),
    bfGrowthPending: String(s.growth_pending || 0),
    bfGrowthSub: s.growth_running ? '队列执行中' : (s.growth_done || s.growth_failed ? '完成 ' + (s.growth_done || 0) + ' · 失败 ' + (s.growth_failed || 0) : '扫描后更新'),
    bfTodayFailed: String(s.today_failed || 0),
    bfLastActivity: s.last_activity_at ? '最近 ' + benefitsTime(s.last_activity_at) : '最近活动 —',
    bfWelfareCredits: s.welfare_grants_available ? '+' + fmtTok(s.welfare_credits_total || 0) : '-',
    bfWelfareSub: s.welfare_grants_available ? (s.welfare_grant_count || 0) + ' 笔福利发放' : '先刷新积分包快照',
  };
  Object.keys(legacyCards).forEach(id => {
    if ($(id)) setText(id, legacyCards[id]);
  });
  renderBenefitGrants(d && d.recent_grants, s);
  renderWelfareSummary(s);
  // The task page is served by /tasks; keep the legacy renderer only for the
  // account welfare table so its virtual-window state remains compatible.
  renderBenefitsAccounts(d && d.accounts);
  loadBenefitBatches();
  loadBenefitLog();
}

async function loadBenefits(force, refresh) {
  if (benefitsReq && !force) return benefitsReq;
  if (force) benefitsReq = null;
  const my = ++benefitsSeq;
  const req = { promise: null, path: '' };
  req.promise = (async () => {
    try {
      const qs = [];
      if (benefitsFocusRef && benefitsFocusRef !== 'all') qs.push('focus=' + encodeURIComponent(benefitsFocusRef));
      if (refresh) qs.push('refresh=1');
      const q = qs.length ? ('?' + qs.join('&')) : '';
      // 任务页的成长快照会同时查询多个国内账号；后端在 8 秒返回部分快照，
      // 前端给接口留出比通用 15 秒更宽的余量，避免浏览器先中止、整页变空。
      // 两个接口互不依赖，独立 settle：一个超时不能丢掉另一个的成功结果。
      const [benefits, tasks] = await Promise.allSettled([
        api('benefits' + q, { timeoutMs: 45000 }),
        api('tasks' + q, { timeoutMs: 45000 }),
      ]);
      if (my !== benefitsSeq || view !== 'taskscenter') return;
      if (benefits.status === 'fulfilled') {
        renderBenefits(benefits.value || {});
      } else {
        const err = benefits.reason;
        const body = $('bfAccountsBody');
        if (body) {
          body.innerHTML = '<tr><td colspan="10"><div class="empty">读取福利概览失败：' + esc(err && err.message ? err.message : String(err)) + '</div></td></tr>';
        }
        const grants = $('bfGrantsBody');
        if (grants) {
          grants.innerHTML = '<tr><td colspan="7"><div class="empty">读取积分发放记录失败：' + esc(err && err.message ? err.message : String(err)) + '</div></td></tr>';
        }
      }
      if (tasks.status === 'fulfilled') {
        renderTaskWorkbench(tasks.value || {});
      } else {
        const err = tasks.reason;
        const sum = $('bfFocusSummary');
        if (sum) sum.innerHTML = '<span class="bf-chip">任务查询失败：' + esc(err && err.message ? err.message : String(err)) + '</span>';
      }
    } finally {
      if (benefitsReq === req) benefitsReq = null;
    }
  })();
  benefitsReq = req;
  return req.promise;
}

// renderTaskWorkbench binds the Hub-shaped /tasks response to the visible
// workbench. It deliberately reuses the existing account table renderer for
// the lower overview section and only replaces the selector, summary, task
// rows and execution log.
function renderTaskWorkbench(d) {
  const accounts = Array.isArray(d && d.accounts) ? d.accounts : [];
  const sel = $('bfGrowthAcct');
  const logAcct = $('bfLogAcct');
  if (logAcct) {
    const prev = benefitsLogFilter.ref || '';
    const opts = ['<option value="">全部账号</option>'].concat(accounts.map(a => '<option value="' + esc(a.ref || '') + '">' + esc((a.nickname || '未命名') + ' · ' + String(a.uid || '').slice(0, 8)) + '</option>')).join('');
    if (logAcct.innerHTML !== opts) logAcct.innerHTML = opts;
    logAcct.value = accounts.some(a => a.ref === prev) ? prev : '';
    benefitsLogFilter.ref = logAcct.value;
  }
  if (sel) {
    const want = benefitsFocusRef || 'all';
    const opts = ['<option value="all">全部国内账号（批量）</option>'].concat(
      accounts.map(a => '<option value="' + esc(a.ref || '') + '">' +
        esc((a.nickname || '未命名') + ' · ' + String(a.uid || '').slice(0, 8)) + '</option>'));
    const next = opts.join('');
    if (sel.innerHTML !== next) sel.innerHTML = next;
    sel.value = accounts.some(a => a.ref === want) ? want : 'all';
    benefitsFocusRef = sel.value;
  }
  const current = benefitsFocusRef === 'all' ? null : accounts.find(a => a.ref === benefitsFocusRef) || null;
  const summary = (d && d.summary) || {};
  const sum = $('bfFocusSummary');
  if (sum) {
    const bits = (current ? [
      '连登 ' + (summary.streak_days || 0) + ' 天',
      '补签卡 ' + (summary.makeup_cards || 0),
      '抽奖 ' + (summary.lottery_chances || 0) + ' 次',
      '能量 ' + fmtTok(summary.energy || 0),
      '本轮猫猫 +' + fmtTok(summary.cat_reward || 0),
    ] : [
      '范围 ' + (summary.accounts || accounts.length) + ' 个国内账号',
      '今日签到 ' + (summary.checkin_done || 0) + ' / ' + ((summary.checkin_done || 0) + (summary.checkin_pending || 0)),
      '待签到 ' + (summary.checkin_pending || 0),
    ]).concat(summary.welfare_grants_available ? ['福利积分 +' + fmtTok(summary.welfare_credits_total || 0)] : []);
    sum.innerHTML = bits.map(x => '<span class="bf-chip">' + esc(x) + '</span>').join('');
  }
  benefitsTaskData = d || null;
  renderBenefitTaskRows(current ? (d && d.tasks) : [], current, d && d.tasks_error);
  ['bfBtnFocusCheckin', 'bfBtnFocusRun', 'bfBtnFocusTravel', 'bfBtnFocusStreak'].forEach(id => {
    const b = $(id);
    if (b) b.disabled = !benefitsFocusRef || !accounts.length;
  });
}

async function runTaskAction(action, btn) {
  const ref = benefitsFocusRef || 'all';
  if (!ref) return;
  const seq = ++benefitsRunSeq;
  const label = btn ? btn.textContent : action;
  if (btn) { btn.disabled = true; btn.textContent = '执行中…'; }
  const box = $('bfBatchList');
  if (box) box.innerHTML = '<div class="empty">正在执行 ' + esc(label) + '…</div>';
  try {
    const body = { scope: ref, action };
    const r = await api('tasks/run', { method: 'POST', body: JSON.stringify(body) });
    if (seq !== benefitsRunSeq) return;
    const results = Array.isArray(r && r.results) ? r.results : [];
    const logs = Array.isArray(r && r.logs) ? r.logs : [];
    const ok = results.filter(x => x && x.ok).length;
    const bad = results.length - ok;
    if (box) {
      box.innerHTML = '<div class="bf-batch-row"><div><div class="bf-batch-k">' + esc(label) + '</div>' +
        '<div class="bf-batch-meta">' + esc(benefitsTime(new Date().toISOString())) + ' · 范围 ' + esc(ref === 'all' ? '全部国内账号' : ref.slice(0, 8)) + '</div></div>' +
        '<div class="bf-batch-stat">' + (bad ? '<span class="tag bad">' + bad + ' 失败</span>' : '<span class="tag ok">完成</span>') +
        '<div class="bf-batch-meta">成功 ' + ok + ' · 失败 ' + bad + ' · 共 ' + results.length + '</div></div></div>' +
        (logs.length ? logs.map(x => '<div class="bf-log-row"><span class="t">' + esc(benefitsTime(new Date().toISOString())) + '</span><span class="n"></span><span class="m">' + esc(x) + '</span></div>').join('') : '');
    }
    toast((r && (r.message || (ok + ' 个账号成功' + (bad ? '，' + bad + ' 个失败' : '')))) || '执行完成', bad ? 'err' : 'ok');
  } catch (e) {
    if (box) box.innerHTML = '<div class="empty err">执行失败：' + esc(e && e.message ? e.message : String(e)) + '</div>';
    toast(e && e.message ? e.message : String(e), 'err');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = label; }
    loadOverview(true);
    await loadBenefits(true);
    loadBenefitLog();
  }
}

// 福利行内动作统一走短引用：页面 DOM 不出现完整 UID；动作在途时同一行的
// 按钮禁用，完成后刷新快照，避免 5 秒轮询把「执行中」状态冲掉。
async function runBenefitAction(ref, act, label) {
  if (!ref || benefitsBusy.has(ref)) return;
  benefitsBusy.add(ref);
  const confirmText = {
    checkin: '确认执行签到并刷新余额？',
    streak: '确认执行连登兑换与抽奖？未解锁档位/无次数会自动跳过。',
    daily_chat: '确认对该国际版账号执行每日活跃对话？',
    daily_chat_web: '确认对该国际版账号执行网页通道打卡？',
    travel: '确认执行猫猫旅行巡检？',
  }[act];
  if (confirmText && !confirm(confirmText)) {
    benefitsBusy.delete(ref);
    return;
  }
  renderBenefitsAccounts((lastBenefits && lastBenefits.accounts) || []);
  try {
    if (act === 'tasks') {
      benefitsTaskRef = ref;
      const d = await api('benefits/' + encodeURIComponent(ref) + '/tasks');
      openTasksForRef(ref, d && d.tasks, d && d.enterprise);
    } else {
      let path = '';
      if (act === 'daily_chat') path = 'benefits/' + encodeURIComponent(ref) + '/daily_chat';
      else if (act === 'daily_chat_web') path = 'benefits/' + encodeURIComponent(ref) + '/daily_chat_web';
      else if (act === 'streak') path = 'benefits/' + encodeURIComponent(ref) + '/streak';
      else if (act === 'travel') path = 'benefits/' + encodeURIComponent(ref) + '/travel';
      else if (act === 'checkin') path = 'benefits/' + encodeURIComponent(ref) + '/checkin';
      const r = await api(path, { method: 'POST' });
      const msg = (r && (r.message || r.checkin_message)) || (label + '完成');
      toast(msg, r && r.success === false ? 'err' : 'ok');
    }
  } catch (e) {
    toast(e && e.message ? e.message : String(e), 'err');
  } finally {
    benefitsBusy.delete(ref);
    loadBenefits();
    loadOverview(true);
  }
}

// openTasksForRef 用短引用打开成长任务弹窗。任务弹窗内部按完整 UID 调接口，
// 因此后端先解析引用，前端只需要拿回任务列表并复用现有渲染。
function openTasksForRef(ref, tasks, enterprise) {
  taskUID = null;
  benefitsTaskRef = ref || null;
  setText('taskWho', '福利账号 ' + String(ref || '').slice(0, 8));
  const veil = $('taskVeil');
  if (!veil) { noteDOM('taskVeil'); return; }
  showModal('taskVeil', 'btnCloseTask');
  const st = $('taskState'), tb = $('taskTable'), body = $('taskBody');
  const list = Array.isArray(tasks) ? tasks : [];
  const acts = ['btnTaskAcceptAll', 'btnTaskAutoAll'];
  acts.forEach(id => { const b = $(id); if (b) b.hidden = !!enterprise; });
  if (!list.length) {
    if (tb) tb.hidden = true;
    if (st) {
      st.hidden = false;
      st.className = 'state';
      st.textContent = enterprise ? '企业账号无个人成长任务体系' : '该账号暂无任务';
    }
    return;
  }
  if (st) st.hidden = true;
  if (tb) tb.hidden = false;
  if (body) body.innerHTML = taskRowsHTML(list);
  if ($('btnTaskReload')) $('btnTaskReload').hidden = true;
}

// 福利弹窗内的任务动作：按短引用走同一福利路由，后端解析后复用账号动作。
function benefitsTaskPath(suffix) {
  return 'benefits/' + encodeURIComponent(benefitsTaskRef) + '/tasks/' + suffix;
}

if ($('bfGrowthAcct')) $('bfGrowthAcct').addEventListener('change', ev => {
  benefitsFocusRef = ev.target && ev.target.value ? String(ev.target.value) : 'all';
  loadBenefits(true);
});

async function runFocusTask(kind, code, btn) {
  const ref = benefitsFocusRef;
  if (!ref) return;
  if (kind === 'auto_all' && !confirm('确认对该账号执行全部可自动成长任务？会接取任务并进行真实对话，可能耗时数分钟。')) return;
  if (btn) { btn.disabled = true; btn.textContent = '执行中…'; }
  try {
    const path = 'benefits/' + encodeURIComponent(ref) + '/tasks/' +
      (kind === 'auto_all' ? 'auto_all' : kind);
    const body = kind === 'auto' ? { task_code: code }
      : kind === 'accept' ? { task_codes: [code] }
      : kind === 'claim' ? { task_code: code } : undefined;
    const r = await api(path, body === undefined
      ? { method: 'POST' } : { method: 'POST', body: JSON.stringify(body) });
    if (kind === 'auto_all') {
      const results = Array.isArray(r && r.results) ? r.results : [];
      const done = results.filter(x => x && x.status === 'done').length;
      const bad = results.filter(x => x && (x.status === 'error' || x.claim_error)).length;
      let msg = (r && r.message) || ('已执行 ' + done + ' 项任务');
      if (bad) msg += '；' + bad + ' 项失败或领奖异常';
      toast(msg, bad ? 'err' : 'ok');
    } else if (kind === 'auto') {
      let msg = (r && r.message) || '任务已执行';
      if (r && r.progress_after) msg += '（进度 ' + r.progress_before + ' → ' + r.progress_after + '）';
      if (r && r.claimed) msg += '，奖励已自动到账';
      const advanced = !!r && !!r.progress_after && r.progress_after !== r.progress_before;
      toast(msg, r && (r.claimed || advanced) && !r.claim_error ? 'ok' : 'err');
    } else {
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
    }
    loadOverview(true);
  } catch (e) {
    toast(e && e.message ? e.message : String(e), 'err');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = kind === 'claim' ? '领取' : (kind === 'auto' ? '一键完成' : '接受'); }
    loadBenefits(true);
  }
}

if ($('bfBtnFocusRun')) $('bfBtnFocusRun').onclick = () => runTaskAction('tasks', $('bfBtnFocusRun'));
if ($('bfBtnFocusCheckin')) $('bfBtnFocusCheckin').onclick = () => runTaskAction('checkin', $('bfBtnFocusCheckin'));
if ($('bfBtnFocusTravel')) $('bfBtnFocusTravel').onclick = () => runTaskAction('travel', $('bfBtnFocusTravel'));
if ($('bfBtnFocusStreak')) $('bfBtnFocusStreak').onclick = () => runTaskAction('streak', $('bfBtnFocusStreak'));

if ($('bfAccountsBody')) $('bfAccountsBody').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-bf-act]');
  if (!btn) return;
  runBenefitAction(btn.dataset.bfRef, btn.dataset.bfAct, btn.textContent || '动作');
});

if ($('btnBenefitsRefresh')) $('btnBenefitsRefresh').onclick = () => { loadBenefits(true, true); loadOverview(true); toast('福利概览已刷新', 'ok'); };
if ($('bfBtnLogRefresh')) $('bfBtnLogRefresh').onclick = () => { loadBenefitLog(); loadBenefitBatches(); };
['bfLogTask', 'bfLogResult', 'bfLogAcct'].forEach(id => {
  const el = $(id);
  if (!el) return;
  el.addEventListener('change', () => {
    benefitsLogFilter.task = $('bfLogTask') ? $('bfLogTask').value : '';
    benefitsLogFilter.result = $('bfLogResult') ? $('bfLogResult').value : '';
    benefitsLogFilter.ref = $('bfLogAcct') ? $('bfLogAcct').value : '';
    loadBenefitLog();
  });
});
if ($('bfBtnRefreshGrants')) $('bfBtnRefreshGrants').onclick = async () => {
  const b = $('bfBtnRefreshGrants');
  const label = b.textContent;
  b.disabled = true; b.textContent = '刷新中…';
  try {
    await loadPackagesData();
    await loadBenefits();
    toast('积分包快照已刷新', 'ok');
  } catch (e) {
    toast(e && e.message ? e.message : String(e), 'err');
  } finally {
    b.disabled = false; b.textContent = label;
  }
};
if ($('btnBfDailyChatAll')) $('btnBfDailyChatAll').onclick = async () => {
  const b = $('btnBfDailyChatAll'), label = b ? b.textContent : '全部每日活跃';
  if (b) { b.disabled = true; b.textContent = '执行中…'; }
  try {
    const r = await api('daily_chat_all', { method: 'POST', body: '{}' });
    toast((r && r.message) || '国际版每日活跃已触发', 'ok');
  } catch (e) {
    toast(e && e.message ? e.message : String(e), 'err');
  } finally {
    if (b) { b.disabled = false; b.textContent = label; }
    loadBenefits(true);
    loadBenefitBatches();
  }
};
if ($('btnBenefitsActivity')) $('btnBenefitsActivity').onclick = () => go('activity');

/* ── 页内区块导航 ─────────────────────────────────────────────────── */
/* 长页（用量/配置）顶部生成 sticky 区块导航：只在 760px 以上显示，标签取每块
   第一个 h3；用固定偏移的锚点而不是 scrollIntoView，避免被 sticky 顶栏遮住。 */
const SECNAV_LABELS = {
  usage: { '用量总览': '总览', '用量明细': '明细', '剩余额度': '额度', '性能分位': '性能', '积分流水': '流水' },
  config: { '服务': '服务', '客户端 API Key': '客户端 Key', '定时任务': '定时', '账号池与流量治理': '选号', '上游与高级': '上游', '日志': '日志' },
};
// 侧栏收起偏好与「有导航」状态：只有 items >= 2 才给 .main 加 has-secnav，
// 否则宽屏正文不留 174px 空栏。收起状态持久化到 localStorage，缺失时默认展开。
const SIDEBAR_COLLAPSE_KEY = 'wb-panel-sidebar-collapsed';
function sidebarCollapsedPref() {
  try { return localStorage.getItem(SIDEBAR_COLLAPSE_KEY) === '1'; } catch (e) { return false; }
}
function applySidebarCollapsed(main, collapsed) {
  if (!main || !main.classList || typeof main.classList.toggle !== 'function') return;
  main.classList.toggle('sidebar-collapsed', !!collapsed);
  const btn = $('btnPageSidebarToggle');
  if (btn) {
    btn.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    btn.title = collapsed ? '展开本页导航' : '收起本页导航';
    btn.textContent = collapsed ? '›' : '‹';
  }
}
function toggleSidebarCollapsed() {
  const main = document.querySelector ? document.querySelector('.main') : null;
  const next = !(main && main.classList && main.classList.contains('sidebar-collapsed'));
  applySidebarCollapsed(main, next);
  try { localStorage.setItem(SIDEBAR_COLLAPSE_KEY, next ? '1' : '0'); } catch (e) { /* ignore */ }
}
if ($('btnPageSidebarToggle')) $('btnPageSidebarToggle').onclick = toggleSidebarCollapsed;
if ($('btnPageTop')) $('btnPageTop').onclick = () => {
  const t = $('topbar');
  const top = t && typeof t.getBoundingClientRect === 'function' ? t.getBoundingClientRect().bottom + 12 : 0;
  if (typeof window !== 'undefined' && typeof window.scrollTo === 'function') window.scrollTo({ top, behavior: 'smooth' });
};
let secNavView = '', secNavTimer = null;
// 滚动监听用 rAF 合并：一次滚动事件里对每个区块 getBoundingClientRect 会触发
// 多次布局；高频触控滚动时还会反复改 nav.scrollLeft。合并到每帧最多一次，
// 且只有高亮区块变化时才真正写 DOM。
let secNavFrame = 0, secNavActive = '';
function setupSectionNav(v) {
  const nav = $('secNav');
  const sidebar = $('pageSidebar');
  const main = document.querySelector ? document.querySelector('.main') : null;
  // #secNav 是必需契约；缺它时只提示一次，不能让页内导航把整页切路由打断。
  if (!nav) { noteDOM('secNav'); return; }
  const host = document.getElementById ? document.getElementById('view-' + v) : null;
  // 旧 index.html 可能没有 .page-sidebar 包裹层；此时仍把导航当作 doc flow 内的
  // 顶部胶囊（nav 自身保留 .secnav 样式），不要因为缺父容器整块跳过。
  const boxes = host && host.querySelectorAll ? Array.from(host.querySelectorAll('.box')) : [];
  const items = [];
  boxes.forEach((box, i) => {
    const h = box.querySelector ? box.querySelector('header h3') : null;
    // 只取 h3 自身的标题文本，不能混入 .hint：用量卡片的说明很长，混进来会
    // 被侧栏当成标题换行成一整段正文（截图里「剩余额度」就是这样溢出的）。
    const label = sectionNavTitle(h);
    if (!label) return;
    if (!box.id) box.id = 'sec-' + v + '-' + i;
    const short = (SECNAV_LABELS[v] || {})[label] || label;
    items.push({ id: box.id, label: short });
  });
  if (!host || items.length < 2) {
    nav.hidden = true; nav.innerHTML = '';
    if (sidebar) sidebar.hidden = true;
    if (main && main.classList) main.classList.remove('has-secnav');
    secNavView = v; return;
  }
  nav.hidden = false;
  if (sidebar) sidebar.hidden = false;
  if (main && main.classList) {
    main.classList.add('has-secnav');
    applySidebarCollapsed(main, sidebarCollapsedPref());
  }
  nav.innerHTML = items.map(it => '<a href="#' + esc(it.id) + '" data-sec="' + esc(it.id) + '">' +
    esc(it.label) + '</a>').join('');
  const links = nav.querySelectorAll ? Array.from(nav.querySelectorAll('a[data-sec]')) : [];
  const mark = id => links.forEach(a => {
    a.classList.toggle('on', a.dataset.sec === id);
    if (a.dataset.sec === id) {
      a.setAttribute('aria-current', 'location');
      // 横向导航在手机上会滚动：高亮项必须保持在可视区内，否则用户不知道
      // 当前区块是哪一个。scrollIntoView 不可用时静默跳过（测试桩/旧浏览器）。
      if (nav.scrollLeft != null && a.offsetLeft != null) {
        const left = a.offsetLeft - nav.clientWidth / 2 + a.offsetWidth / 2;
        nav.scrollLeft = Math.max(0, left);
      }
    }
    else a.removeAttribute('aria-current');
  });
  const boxMap = new Map(items.map(it => {
    const el = boxes.find(b => b.id === it.id);
    return [it.id, el];
  }));
  links.forEach(a => {
    a.onclick = ev => {
      if (ev && ev.preventDefault) ev.preventDefault();
      const el = boxMap.get(a.dataset.sec);
      if (!el) return;
      mark(a.dataset.sec);
      const top = Math.max(0, el.getBoundingClientRect().top - 122);
      if (window.scrollTo) window.scrollTo({ top: top, behavior: 'smooth' });
      // 区块锚点只用于滚动，不写进 URL：主路由只认 9 个视图，刷新 #sec-*
      // 会回落到账号页，看起来像导航丢失。
    };
  });
  const spy = () => {
    secNavFrame = 0;
    if (secNavView !== v || !nav || nav.hidden) return;
    const y = (window.scrollY || window.pageYOffset || 0) + 96;
    let cur = items[0].id;
    for (const it of items) {
      const el = boxMap.get(it.id);
      if (!el) continue;
      if (el.getBoundingClientRect().top <= y) cur = it.id;
    }
    if (cur !== secNavActive) {
      secNavActive = cur;
      mark(cur);
    }
  };
  const onScroll = () => {
    if (secNavFrame) return;
    if (typeof requestAnimationFrame === 'function') secNavFrame = requestAnimationFrame(spy);
    else spy();
  };
  if (secNavView !== v) {
    if (secNavTimer) removeEventListener('scroll', secNavTimer);
    secNavTimer = onScroll;
    addEventListener('scroll', onScroll, { passive: true });
  }
  secNavView = v;
  // 切页/重绘后强制重算一次高亮（不能沿用上一页的 secNavActive）。
  secNavActive = '';
  if (typeof requestAnimationFrame === 'function') secNavFrame = requestAnimationFrame(spy);
  else spy();
}

/* sectionNavTitle 取区块标题的纯文本：复制 h3 并移除 .hint/.note 后再读
   textContent。直接读 h3.textContent 会把补充说明一起拼进导航项。 */
function sectionNavTitle(h) {
  if (!h) return '';
  const clone = h.cloneNode ? h.cloneNode(true) : null;
  if (clone && clone.querySelectorAll) {
    clone.querySelectorAll('.hint, .note').forEach(x => x.remove());
  }
  const text = String((clone ? clone.textContent : h.textContent) || '').replace(/\s+/g, ' ').trim();
  return text;
}

/* ── 接入页 ───────────────────────────────────────────────────────── */
/* 复制按钮只复制对应 <pre>/<input> 的已渲染文本；服务端的示例统一使用
   $WB_API_KEY 占位符，页面不会从配置接口取真实密钥来拼示例。 */
function connectCopyButtons() {
  const root = document.getElementById ? document.getElementById('view-connect') : null;
  if (!root || typeof root.querySelectorAll !== 'function') return;
  root.querySelectorAll('button[data-copy-el]').forEach(btn => {
    btn.onclick = async () => {
      const target = $(btn.dataset.copyEl);
      if (!target) return;
      const text = target.value != null && target.value !== '' ? String(target.value) : String(target.textContent || '');
      try {
        await copyText(text);
        toast('已复制', 'ok');
      } catch (e) {
        toast('复制失败，请手动选择文本', 'err');
      }
    };
  });
}
function renderConnect(d) {
  d = d || {};
  cxData = d;
  const ex = d.examples || {};
  const set = (id, v) => { const el = $(id); if (el) { if ('value' in el) el.value = v || ''; else el.textContent = v || ''; } };
  set('cxV1', d.v1_url);
  set('cxBase', d.base_url);
  set('cxAnthropic', d.anthropic_url);
  set('cxOpenAI', ex.openai_sdk);
  set('cxCurl', ex.curl);
  set('cxAnthropicEnv', ex.anthropic_env);
  set('cxAnthropicMessages', ex.anthropic_messages);
  set('cxResponses', ex.responses_api);
  const models = (d.models || []).map(m => m && m.id).filter(Boolean);
  const modelsText = models.join(', ');
  set('cxModels', modelsText);
  const modelsBox = $('cxModels');
  // input 只写 value：对 <input> 写 innerHTML 在部分浏览器无效或触发解析，
  // 会导致展示值与复制值不一致。set() 已按元素类型写入 value。
  if (modelsBox && modelsBox.value !== modelsText) modelsBox.value = modelsText;
  const note = $('cxNote');
  if (note) note.textContent = d.auth_required
    ? '客户端需要 API Key（Bearer 或 x-api-key）'
    : '客户端鉴权未启用';
  const notes = $('cxNotes');
  if (notes) notes.textContent = (d.notes || []).join(' · ');
  if (modelsBox) modelsBox.title = models.join('\n');
  connectCopyButtons();
}
async function loadConnect() {
  const my = ++cxSeq;
  try {
    const d = await api('connect');
    if (my !== cxSeq) return;
    renderConnect(d || {});
  } catch (e) {
    if (my !== cxSeq) return;
    const note = $('cxNote');
    if (note) note.textContent = '读取接入信息失败：' + (e && e.message ? e.message : String(e));
  }
}
if ($('btnConnectReload')) $('btnConnectReload').onclick = loadConnect;

/* ── 积分构成 ─────────────────────────────────────────────────────── */

/* ── 剩余额度 ─────────────────────────────────────────────────────── */
/* loadRemaining 是独立区块：单飞、世代守卫、离页丢弃。它不依赖 /usage 的返回，
   因此用量主请求失败时仍可单独刷新；但通常由 loadUsage 在成功后触发。 */
/* 每天零点的本地额度快照是运维最直接的护栏：账号或模型打满时必须先看到，
   而不是等下一条 429 才知道。未知用量单独显示，绝不当成 0。 */
const REM_DIM_TABS = [['account', '按账号'], ['model', '按模型']];
function remFilteredRows(d, dim, q) {
  const words = String(q || '').toLowerCase().split(/\s+/).filter(Boolean);
  const rows = dim === 'model' ? ((d && d.models) || []) : ((d && d.accounts) || []);
  if (!words.length) return rows.slice();
  return rows.filter(row => {
    const text = [row && row.model, row && row.nickname, row && row.uid, row && row.realm]
      .filter(Boolean).join(' ').toLowerCase();
    return words.every(w => text.includes(w));
  });
}
function renderRemaining(d) {
  d = d || {};
  remData = d;
  const tabs = $('remTabs');
  if (tabs) tabs.innerHTML = REM_DIM_TABS.map(([k, label]) =>
    '<button data-rem-dim="' + k + '"' + (k === remDim ? ' class="on"' : '') + '>' + esc(label) + '</button>').join('');
  const empty = $('remEmpty');
  const table = $('remTable');
  const summary = $('remSummary');
  if (d.data_available === false) {
    if (empty) { empty.hidden = false; empty.textContent = '剩余额度不可用：' + (d.reason || '本地用量快照尚未建立'); }
    if (table) table.hidden = true;
    if (summary) summary.innerHTML = '用量快照不可用';
    return;
  }
  if (empty) empty.hidden = true;
  if (table) table.hidden = false;
  const limits = d.limits || {};
  const qEl = $('remQ');
  const rows = remFilteredRows(d, remDim, qEl && typeof qEl.value === 'string' ? qEl.value.trim() : '')
    .slice().sort((a, b) => (b.limited ? 1 : 0) - (a.limited ? 1 : 0));
  const head = $('remHead');
  const body = $('remBody');
  if (remDim === 'model') {
    if (head) head.innerHTML = '<tr><th>模型 / 账号</th><th>域</th><th class="num">已用</th><th class="num">限额</th><th class="num">剩余</th></tr>';
    if (body) body.innerHTML = rows.map(r => {
      const limit = r.limit_enabled ? fmtTok(r.limit) : '未启用';
      const used = r.used == null ? '用量未知' : fmtTok(r.used);
      const left = r.limit_enabled ? (r.remaining == null ? '用量未知' : fmtTok(r.remaining)) : '未启用';
      return '<tr' + (r.limited ? ' class="hit"' : '') + '><td>' + esc(r.model || '—') +
        '<div class="note">' + esc(r.nickname || String(r.uid || '').slice(0, 8)) + '</div></td>' +
        '<td>' + esc(r.realm || '—') + '</td><td class="num">' + esc(used) + '</td>' +
        '<td class="num">' + esc(limit) + '</td><td class="num">' + esc(left) + '</td></tr>';
    }).join('') || '<tr><td colspan="5" class="empty">暂无模型用量</td></tr>';
  } else {
    if (head) head.innerHTML = '<tr><th>账号</th><th>域</th><th class="num">已用</th><th class="num">限额</th><th class="num">剩余</th></tr>';
    if (body) body.innerHTML = rows.map(r => {
      const limit = r.limit_enabled ? fmtTok(r.limit) : '未启用';
      const used = r.used == null ? '用量未知' : fmtTok(r.used);
      const left = r.limit_enabled ? (r.remaining == null ? '用量未知' : fmtTok(r.remaining)) : '未启用';
      return '<tr' + (r.limited ? ' class="hit"' : '') + '><td>' + esc(r.nickname || String(r.uid || '').slice(0, 8)) +
        '<div class="note">' + esc(r.uid || '') + '</div></td><td>' + esc(r.realm || '—') +
        '</td><td class="num">' + esc(used) + '</td><td class="num">' + esc(limit) +
        '</td><td class="num">' + esc(left) + '</td></tr>';
    }).join('') || '<tr><td colspan="5" class="empty">暂无账号用量</td></tr>';
  }
  if (summary) {
    const s = d.summary || {};
    summary.innerHTML = '账号达限 <b>' + Number(s.limited_accounts || 0) + '</b> / ' + Number(s.accounts || 0) +
      ' · 模型达限 <b>' + Number(s.limited_models || 0) + '</b> / ' + Number(s.model_rows || 0) +
      (limits.account_enabled === false && limits.model_enabled === false ? ' · 限额未启用' : '');
  }
}
async function loadRemaining() {
  if (view !== 'usage' || remReq) return;
  remReq = true;
  const my = ++remSeq;
  if (remCtrl) { try { remCtrl.abort(); } catch (e) { /* ignore */ } }
  remCtrl = typeof AbortController === 'function' ? new AbortController() : null;
  try {
    const d = await api('usage/remaining', remCtrl ? { signal: remCtrl.signal } : {});
    if (my !== remSeq || view !== 'usage') return;
    renderRemaining(d || {});
  } catch (e) {
    if (my !== remSeq || view !== 'usage') return;
    renderRemaining({ data_available: false, reason: e && e.message ? e.message : String(e), accounts: [], models: [], limits: {}, summary: {} });
  } finally {
    if (my === remSeq) remReq = false;
  }
}
if ($('remTabs')) $('remTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-rem-dim]');
  if (!b || b.dataset.remDim === remDim) return;
  remDim = b.dataset.remDim;
  if (remData) renderRemaining(remData);
});
if ($('btnRemReload')) $('btnRemReload').onclick = () => { remReq = false; loadRemaining(); };
let remQTimer = null;
if ($('remQ')) $('remQ').oninput = () => {
  clearTimeout(remQTimer);
  remQTimer = setTimeout(() => { if (remData) renderRemaining(remData); }, 120);
};


/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个
   包名一个稳定配色，跨账号对比时同色即同类。 */

const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
                   '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];
const PK_ACCOUNT_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6',
                           '#e2607a', '#20a4a4', '#8fbf3f', '#d4785c',
                           '#7c83db', '#c48a2f', '#b45f8c', '#5aa9e6'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

// pkAccountColorMap 按 UID 稳定分配颜色：排序后分配，账号刷新/重排不会换色。
function pkAccountColorMap(list) {
  const uids = (list || [])
    .filter(a => a && !a.error && a.uid)
    .map(a => String(a.uid))
    .sort();
  const colors = new Map();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
  return colors;
}

/* pkBySource 把包按名称归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    // 分组键用 code + name，而不是只 name：上游给「首登赠送」和普通活动包用了
    // **同一个 PackageName 和同一个 PackageCode**，只按 name 会把两类混成一类，
    // 那正是当初「两个号为何差 1500」看不出来的原因。这里至少把 code 带进键里，
    // 并在卡片上显示最早的发放时间。
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

const PK_DEFAULT_DETAIL_LIMIT = 5;

function pkDetailLimitValue(raw) {
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : PK_DEFAULT_DETAIL_LIMIT;
}

function pkDetailLimit(cfg) {
  return pkDetailLimitValue(cfg && cfg.panel && cfg.panel.package_detail_limit);
}

const PK_DAY_MS = 24 * 3600 * 1000;

function pkExpiryMs(p) {
  const raw = Number(p && p.expires_at);
  if (Number.isFinite(raw) && raw > 0) return raw;
  const text = String((p && p.end_time) || '').trim();
  if (!text) return null;
  let iso = text.includes('T') ? text : text.replace(' ', 'T');
  if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00';
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

// pkDetailCompare 只服务单账号逐包明细：正余额包先按到期时间挑选默认展示项，
// 其余正余额包与已用完包分别折叠；同一到期时间按面额降序。主键跟随视图排序
// 模式（pkSortMode，声明在本区块末尾的绑定块）：end_asc 到期升序、size_desc
// 面额降序（同面额按到期升序）。typeof 守卫：前端 harness 的区域切片求值里
// 没有该全局，回落 end_asc（= 上游原有语义，切片测试的期望序不受影响）。
function pkDetailCompare(a, b) {
  const sizeOf = p => {
    const n = Number(p && p.size);
    return Number.isFinite(n) ? n : 0;
  };
  const mode = (typeof pkSortMode === 'string' && pkSortMode) || 'end_asc';
  if (mode === 'size_desc') {
    const d = sizeOf(b) - sizeOf(a);
    if (d !== 0) return d;
  }
  const ea = pkExpiryMs(a), eb = pkExpiryMs(b);
  if (ea == null && eb != null) return 1;
  if (ea != null && eb == null) return -1;
  if (ea != null && eb != null && ea !== eb) return ea - eb;
  return sizeOf(b) - sizeOf(a);
}

function pkDetailGroups(packs, limit) {
  const active = [], used = [];
  let usedSize = 0, restSize = 0, restRemain = 0;
  for (const p of packs || []) {
    const remain = Number(p && p.remain);
    if (remain > 0) {
      active.push(p);
      continue;
    }
    used.push(p);
    const size = Number(p && p.size);
    if (Number.isFinite(size)) usedSize += size;
  }
  active.sort(pkDetailCompare);
  used.sort(pkDetailCompare);
  const visible = active.slice(0, pkDetailLimitValue(limit));
  const rest = active.slice(visible.length);
  for (const p of rest) {
    const size = Number(p && p.size);
    if (Number.isFinite(size)) restSize += size;
    const remain = Number(p && p.remain);
    if (Number.isFinite(remain)) restRemain += remain;
  }
  return { visible, rest, used, restSize, restRemain, usedSize };
}

function pkCreditOpacity(days) {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

function pkExpiryText(expiresAt) {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}

function pkExpiryDateTime(expiresAt) {
  if (!expiresAt) return '—';
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai', hour12: false,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  });
}

function pkAccountSegments(a, now) {
  let balance = Math.max(0, Number(a.remain || 0));
  const out = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

// summarizeCreditDays 对齐 WorkDaddy：按精确剩余天数逐行聚合，无有效到期时间的余额
// 不进入图表，也不猜测到期日。账号内先按总余额约束逐包金额，避免上游重复记录膨胀。
function summarizeCreditDays(list, now) {
  const buckets = new Map();
  let unavailable = 0;
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) {
      unavailable++;
      continue;
    }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue;
      let row = buckets.get(segment.days);
      if (!row) {
        row = { days: segment.days, credits: 0, segments: [] };
        buckets.set(segment.days, row);
      }
      row.credits += segment.amount;
      row.segments.push(segment);
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
  for (const row of rows) {
    row.segments.sort((a, b) =>
      (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
      a.accountName.localeCompare(b.accountName) ||
      a.source.localeCompare(b.source));
  }
  return { rows, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
  const summary = summarizeCreditDays(list, now);
  const colors = pkAccountColorMap(list);
  const rows = summary.rows.map(row => {
    const total = row.credits || 1;
    const nodes = row.segments.map(segment => {
      const color = colors.get(segment.uid) || 'var(--accent)';
      const title = segment.source + '\n' + fmtTok(segment.amount) + ' 积分\n到期时间 ' +
        pkExpiryDateTime(segment.expiresAt) + '（' + pkExpiryText(segment.expiresAt) + '）\n' +
        segment.accountName;
      return '<span class="pk-expiry-seg" style="--seg-color:' + color +
        ';opacity:' + pkCreditOpacity(segment.days).toFixed(5) +
        ';flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
        ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
    }).join('');
    return '<div class="pk-expiry-row"><span>' + esc(row.days === 0 ? '已到期' : row.days + ' 天') +
      '</span><div class="pk-expiry-track">' + nodes + '</div><b>' + esc(fmtTok(row.credits)) +
      '</b></div>';
  }).join('');
  const foot = summary.accountCount + ' 个账号' +
    (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');
  const legend = (list || []).filter(a =>
    a && !a.error && a.uid && pkAccountSegments(a, now).some(s => s.days != null)
  ).map(a => '<span><i style="background:' + (colors.get(String(a.uid)) || 'var(--accent)') +
    '"></i>' + esc(a.nickname || String(a.uid).slice(0, 8)) + '</span>').join('');
  const hdr = '<div class="pk-expiry-hdr"><span>剩余天数</span><span style="text-align:center">各账号该批剩余</span><b>剩余积分</b></div>';
  const expiryHost = $('pkExpiry');
  if (!expiryHost) { noteDOM('pkExpiry'); return; }
  expiryHost.innerHTML = (rows
    ? hdr + '<div class="pk-expiry-chart">' + rows + '</div>'
    : '<div class="pk-expiry-empty">暂无可汇总积分</div>') +
    (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
    '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

function renderPackages(d, detailLimit) {
  const all = (d.accounts || []);
  const q = pkQuery.trim().toLowerCase();
  // 搜索只影响下方账号对比/逐包明细；到期分布仍展示全池（它是全局视角）。
  const list = q ? all.filter(a => String((a.nickname || '') + ' ' + (a.uid || '') + ' ' + (a.realm || '')).toLowerCase().includes(q)) : all;
  const now = Date.now();
  const expiryColors = pkAccountColorMap(list);
  renderExpiryDistribution(all, now);
  if (!list.length) {
    setHTML('pkSummary', '<div class="empty">没有账号' + (q ? '符合“' + esc(pkQuery) + '”' : '') + '</div>');
    setHTML('pkDetail', '');
    if ($('pkDetailCount')) $('pkDetailCount').textContent = '';
    return;
  }

  // 每个账号的来源归并只算一次：下面建色号表、最大面额表、标签表、摘要卡片
  // 和逐包明细都要用。此前每处各自调用 pkBySource，470 号 × 数十包时同一轮
  // 渲染要重复归并并重建 Map 十几万次（搜索逐键触发时尤其明显）。
  const srcsByUID = new Map();
  const srcsOf = a => {
    const key = String(a.uid || '');
    let v = srcsByUID.get(key);
    if (!v) { v = pkBySource(a.packages || []); srcsByUID.set(key, v); }
    return v;
  };
  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of srcsOf(a)) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  // 键 → 池内最大面额：用于给包名排序（面额大的来源排前面）。先算好一张 Map，
  // 排序比较器里直接查表——原来在每次比较里重建 pkBySource 并线性查找，
  // 复杂度是 O(来源数² × 账号数 × 包数)（易用性修复 12）。
  const keyMaxSize = new Map();
  for (const a of list) for (const s of srcsOf(a)) {
    const cur = keyMaxSize.get(s.key);
    if (cur == null || s.size > cur) keyMaxSize.set(s.key, s.size);
  }
  names.sort((x, y) => (keyMaxSize.get(y) || 0) - (keyMaxSize.get(x) || 0));
  const colorOf = n => pkColor(names.indexOf(n));
  // 键 → 展示名，供卡片与明细表共用（同一来源必然同色同名）。
  const labelOf = {};
  for (const a of list) for (const s of srcsOf(a)) labelOf[s.key] = s;

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  setHTML('pkSummary', list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc((a.nickname || a.uid.slice(0, 8))) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const srcs = srcsOf(a);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
      colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
    ).join('');
    const legend = srcs.map(s =>
      '<span><i style="background:' + colorOf(s.key) + '"></i>' +
      esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
      (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>'
    ).join('');
    const expiry = pkAccountSegments(a, now);
    const expiryTotal = Math.max(1, expiry.reduce((sum, s) => sum + s.amount, 0));
    const expiryColor = expiryColors.get(String(a.uid)) || 'var(--accent)';
    const expiryBar = expiry.length ? '<div class="expirybar" role="img" aria-label="积分到期分布">' +
      expiry.map(s => {
        const title = s.source + '\n' + fmtTok(s.amount) + ' 积分\n到期时间 ' +
          pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）';
        return '<i style="background:' + expiryColor +
          ';opacity:' + pkCreditOpacity(s.days).toFixed(5) +
          ';flex:' + Math.max(0.008, s.amount / expiryTotal).toFixed(4) +
          ' 1 0" title="' + esc(title) + '"></i>';
      }).join('') + '</div>' : '';
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtTok(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
      ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      expiryBar +
      '<div class="pk-legend">' + legend + '</div>' +
      '</div>';
  }).join(''));

  // 缓存时间戳（易用性修复 8）：有缓存时告知"缓存于 HH:MM"，让用户知道看到的
  // 是不是最新数据；没有时间戳（理论不会发生，走了 renderPackages 就有缓存）退回原文案。
  if ($('pkNote')) $('pkNote').textContent = list.length + ' 个账号 · ' + (lastPackagesAt ? '缓存于 ' + fmtClock(lastPackagesAt) : '实时查询上游');

  // 逐包明细：每个账号一个表，排序规则由视图顶部的选择器决定（默认到期近的在前）。
  // 默认只渲染前 PK_DETAIL_ACCOUNT_PAGE 个账号，避免 470 账号 × 数十包把 DOM 撑爆；
  // 用户点「展开全部账号」后再追加剩余（不是重新向后端查询，数据本就在内存）。
  const detailList = pkDetailAll ? list : list.slice(0, PK_DETAIL_ACCOUNT_PAGE);
  const more = list.length - detailList.length;
  if ($('pkDetailCount')) {
    setText('pkDetailCount', detailList.length + ' / ' + list.length + ' 个账号' +
      (more > 0 ? '（还有 ' + more + ' 个未渲染）' : '') +
      (q ? ' · 筛选“' + pkQuery.trim() + '”' : ''));
  }
  const toggle = $('btnPkDetailToggle');
  if (toggle) {
    toggle.hidden = list.length <= PK_DETAIL_ACCOUNT_PAGE;
    toggle.textContent = pkDetailAll ? '收起明细' : '展开全部账号';
    toggle.setAttribute('aria-expanded', String(pkDetailAll));
  }
  setHTML('pkDetail', detailList.map(a => {
    if (a.error) return '';
    const groups = pkDetailGroups(a.packages || [], detailLimit);
    const rowOf = (p, rowGroup) => {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                  (p.package_code || '').replace(/^TCACA_/, '');
      return '<tr' + (rowGroup ? ' class="pk-hidden-row pk-' + rowGroup +
        '-row" data-pk-row="' + rowGroup + '" hidden' : '') +
        '><td class="mark" aria-hidden="true"><i style="background:' +
        colorOf(k) + '"></i></td>' +
      '<td>' + esc(p.name || '(未命名)') +
        (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
      '<td class="num">' + fmtTok(p.size) + '</td>' +
      '<td class="num">' + fmtTok(p.remain) + '</td>' +
      '<td class="num">' + fmtTok(p.used) + '</td>' +
      '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
      '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
      '</tr>';
    };
    const groupSummary = (group, label, count, size, remain) =>
      '<tr class="pk-group-summary"><td colspan="7"><button type="button" class="pk-group-toggle"' +
      ' data-pk-group="' + group + '" data-count="' + count + '" data-size="' + size +
      '" data-remain="' + remain + '" aria-expanded="false">' + label + '，展开</button></td></tr>';
    const rows = groups.visible.map(p => rowOf(p, '')).join('');
    const restSummary = groups.rest.length
      ? groupSummary('rest', '其余未用完 ' + groups.rest.length + ' 个包（面额合计 ' +
          fmtTok(groups.restSize) + ' · 剩余 ' + fmtTok(groups.restRemain) + '）',
          groups.rest.length, groups.restSize, groups.restRemain) +
        groups.rest.map(p => rowOf(p, 'rest')).join('')
      : '';
    const usedSummary = groups.used.length
      ? groupSummary('used', '已用完 ' + groups.used.length + ' 个包（面额合计 ' +
          fmtTok(groups.usedSize) + '）', groups.used.length, groups.usedSize, 0) +
        groups.used.map(p => rowOf(p, 'used')).join('')
      : '';
    return '<div class="box"><header><h3>' +
      esc(a.nickname || a.uid.slice(0, 8)) + ' · ' + esc(a.realm || '') +
      '</h3><span class="grow"></span><span class="note">余额 ' + fmtTok(a.remain) +
      ' / 总额 ' + fmtTok(a.size) + ' · 可用 ' + (groups.visible.length + groups.rest.length) + ' 个包' +
      (groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : '') +
      ' · 默认展示最早到期 ' + pkDetailLimitValue(detailLimit) + ' 条（' +
      esc(PK_SORT_LABELS[pkSortMode] || '') + '）</span>' +
      '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + rows + restSummary + usedSummary + '</tbody></table></div></div>';
  }).join(''));
}

if ($('pkDetail')) $('pkDetail').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-pk-group]');
  if (!btn) return;
  const body = btn.closest('tbody');
  if (!body) return;
  const group = btn.dataset.pkGroup;
  const expanded = btn.getAttribute('aria-expanded') === 'true';
  body.querySelectorAll('tr[data-pk-row="' + group + '"]').forEach(row => { row.hidden = expanded; });
  const count = btn.dataset.count || '0';
  const size = btn.dataset.size || '0';
  const remain = btn.dataset.remain || '0';
  btn.setAttribute('aria-expanded', String(!expanded));
  if (group === 'rest') {
    btn.textContent = expanded
      ? '其余未用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + ' · 剩余 ' +
        fmtTok(remain) + '），展开'
      : '收起其余未用完 ' + count + ' 个包';
  } else {
    btn.textContent = expanded
      ? '已用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + '），展开'
      : '收起已用完 ' + count + ' 个包';
  }
});

/* ── 逐包明细排序模式 ─────────────────────────────────────────────── */
/* 逐包明细的排序规则（选择持久化在 localStorage，跨会话记住）：
   end_asc   到期升序（默认）——快过期的包排最前，提醒优先消耗；无到期时间的
             包（上游没下发 end_time）没有可比的日期，统一垫底，不掺进日期序里；
   size_desc 面额降序——原来的展示口径，看「钱从哪来」。
   行序统一由 pkDetailCompare 实现（明细折叠分组共用同一比较器，切换模式时
   折叠组内行序同步跟随）。本块整体放在 renderPackages / renderExpiryDistribution
   之后：前端 harness 按区域切片求值（[PK_ACCOUNT_COLORS, renderExpiryDistribution)
   与 [PK_DEFAULT_DETAIL_LIMIT, renderPackages)），顶层 localStorage/$ 语句落进
   切片区会让无 DOM 桩的求值环境 ReferenceError——上游测试的切片边界不动。 */
const LS_PK_SORT = 'pkSortMode';
let pkSortMode = localStorage.getItem(LS_PK_SORT) || 'end_asc';
// 积分页账号筛选/明细展开宽度（前端过滤，不改请求）。470 账号时逐包明细默认
// 只渲染第一屏账号，展开全部才追加；否则每个账号一张表会把 DOM 撑到数万行。
let pkQuery = '';
let pkDetailAll = false;
const PK_DETAIL_ACCOUNT_PAGE = 24;
const PK_SORT_LABELS = { end_asc: '按到期升序 · 近的在前', size_desc: '按面额降序' };
// 排序切换时重排明细需要 detailLimit（上游折叠配置），loadPackages 拉到后缓存。
let lastDetailLimit = PK_DEFAULT_DETAIL_LIMIT;

// 排序规则控件：恢复上次选择并绑定切换。
const pkSortEl = $('pkSort');
if (pkSortEl) {
  pkSortEl.value = pkSortMode;
  if (pkSortEl.value !== pkSortMode) {        // localStorage 里存了废弃值：回落默认
    pkSortMode = 'end_asc';
    localStorage.setItem(LS_PK_SORT, pkSortMode);   // 不用 removeItem：harness 桩无此方法
  }
  pkSortEl.onchange = () => {
    pkSortMode = pkSortEl.value;
    localStorage.setItem(LS_PK_SORT, pkSortMode);
    if (lastPackages) renderPackages(lastPackages, lastDetailLimit);   // 数据在内存，直接重排
  };
}

// 470 账号 × 每号数十包时，逐键重排整页会产生数万次 pkBySource/排序。与模型页
// 同口径做 120ms 防抖；输入框值仍在触发时即时读取，不会丢最后一次输入。
let pkQTimer = null;
if ($('pkQ')) $('pkQ').oninput = () => {
  clearTimeout(pkQTimer);
  pkQTimer = setTimeout(() => {
    pkQuery = $('pkQ').value || '';
    if (lastPackages) renderPackages(lastPackages, lastDetailLimit);
  }, 120);
};
if ($('btnPkDetailToggle')) $('btnPkDetailToggle').onclick = () => {
  pkDetailAll = !pkDetailAll;
  if (lastPackages) renderPackages(lastPackages, lastDetailLimit);
};

// loadPackages 按需拉取：有缓存（上次任意入口拉到的 packages 数据）先直接渲染，
// 不再「切进积分构成页就打一次上游」——逐账号实时查询在账号多时既慢又压上游。
// 无缓存（首次进入）才自动查一次；显式刷新（force=true，点「刷新」按钮）始终实时查。
// packages 请求逐账号实时打上游，重复点击「刷新」/切页会叠加很长时间的请求，
// 旧响应晚到还可能把新数据覆盖回去。这里做单飞 + 代次：同一时刻只允许一个
// 请求；新的强制刷新让旧代次的结果被丢弃（但不能中断已在途的上游查询）。
let pkReqSeq = 0, pkInFlight = false;
async function loadPackages(force) {
  if (!force && lastPackages) {
    renderPackages(lastPackages, lastDetailLimit);
    const ageMin = Math.floor((Date.now() - lastPackagesAt) / 60000);
    if (ageMin > 0 && $('pkNote')) $('pkNote').textContent = lastPackages.accounts.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，点「刷新」更新';
    return;
  }
  if (pkInFlight) { pkPending = true; return; }
  pkInFlight = true;
  const seq = ++pkReqSeq;
  setHTML('pkSummary', '<div class="empty">查询中…（逐账号向上游实时查询）</div>');
  setHTML('pkDetail', '');
  setHTML('pkExpiry', '<div class="pk-expiry-empty">查询中…</div>');
  try {
    const [d, c] = await Promise.all([
      loadPackagesData(),
      api('config').catch(() => null),
    ]);
    if (seq !== pkReqSeq) return;
    // 缓存由 loadPackagesData 统一写入，避免两处在途查询各自覆盖 lastPackagesAt。
    lastDetailLimit = pkDetailLimit(c && c.config);
    renderPackages(d, lastDetailLimit);
  } catch (e) {
    if (seq !== pkReqSeq) return;
    // 失败时清空旧明细：否则刷新失败后仍显示上一次的逐包表，用户会误以为是
    // 本次查询结果。摘要与到期卡明确标失败原因。
    setHTML('pkDetail', '');
    setHTML('pkSummary', '<div class="empty">读取失败：' + esc(e.message) + '</div>');
    setHTML('pkExpiry', '<div class="pk-expiry-empty">读取失败：' + esc(e.message) + '</div>');
    setText('pkNote', '读取失败 · 可点「刷新」重试');
  } finally {
    if (seq === pkReqSeq) {
      pkInFlight = false;
      if (pkPending) { pkPending = false; loadPackages(true); }
    }
  }
}
let pkPending = false;

/* ── 积分到期提醒（首页卡片）────────────────────────────────────────── */
/* 积分不是永久的：签到/任务发的裂变包约一个月失效。只看「剩余积分 ÷ 日消耗」
   会系统性偏乐观——用不完的部分到期直接蒸发。这里把「最近要过期的是哪批、
   有多少、到期前每天要至少消耗多少」顶到首页，数据源与「积分构成」共用
   （lastPackages 缓存，EXP_FRESH_MS 内复用，不重复打上游）。 */

// expBatches 把某账号的包聚合成「到期日 → 该日作废积分」升序列表。
// 只统计 remain>0 且有到期时间的包——没余额/长期包到期没有任何影响。
function expBatches(packs) {
  const byDay = new Map();
  for (const p of packs || []) {
    const r = Number(p.remain || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (r <= 0 || !t) continue;
    byDay.set(t, (byDay.get(t) || 0) + r);
  }
  return [...byDay.entries()]
    .map(([date, remain]) => ({ date, remain }))
    .sort((a, b) => (a.date < b.date ? -1 : 1));
}

function expDaysLeft(dateStr, today) {
  return Math.round((new Date(dateStr + 'T00:00:00') - today) / 86400000);
}

function renderExpiry(d) {
  const list = (d.accounts || []);
  const today = new Date(); today.setHours(0, 0, 0, 0);
  // 提醒收敛（首页信息密度）：只把 3 天内到期的账号逐行展示，4–7 天合并成一行
  // 汇总，7 天外与无到期批次折叠成一行。逐个账号平铺在 470 个账号时会把首页变成
  // 一堵墙，真正需要行动的少数反而被淹没；完整逐包明细仍在「积分构成」页。
  // 后端 /panel/api/packages 按余额降序返回，所以这里仍按最早到期日重排（issue #125）。
  const urgent = [], soon = [], failed = [];
  let quiet = 0;
  for (const a of list) {
    if (a.error) { failed.push(a); continue; }
    const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
    const week = bs.filter(b => expDaysLeft(b.date, today) <= 7);
    if (!week.length) { quiet++; continue; }
    const item = { a, first: week[0], week, days: expDaysLeft(week[0].date, today) };
    (item.days <= 3 ? urgent : soon).push(item);
  }
  const byDate = (x, y) => x.first.date < y.first.date ? -1 : x.first.date > y.first.date ? 1 : 0;
  urgent.sort(byDate); soon.sort(byDate);
  const rows = expUrgentRows(urgent, ({ a, first, week }) => {
    const days = expDaysLeft(first.date, today);
    const daily = Math.ceil(first.remain / Math.max(1, days));
    const weekRemain = week.reduce((s, b) => s + b.remain, 0);
    // 只有 ≤3 天需要红色逐行提醒；4–7 天汇总成一行，避免整页红点。
    const cls = 'var(--bad)';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    const more = week.length > 1
      ? '<div class="note">另有 ' + (week.length - 1) + ' 批 · 7 天内合计 ' + fmtTok(weekRemain) + '</div>'
      : '';
    return '<div class="exp-row"><span class="exp-dot" style="background:' + cls + '"></span>' +
      '<span class="exp-nm">' + esc(a.nickname || String(a.uid || '').slice(0, 8)) + '</span>' +
      '<span class="exp-main">最近到期 <b>' + esc(first.date) + '</b>（' + dayWord +
      '）· 该批 <b>' + fmtTok(first.remain) + '</b> 积分 · 到期前日均需耗 ≥<b>' +
      fmtTok(daily) + '</b>' + more + '</span></div>';
  });
  if (soon.length) {
    const weekRemain = soon.reduce((s, x) => s + x.week.reduce((a, b) => a + b.remain, 0), 0);
    const names = soon.slice(0, 3).map(x => x.a.nickname || String(x.a.uid || '').slice(0, 8)).join('、');
    const detail = soon.map(x => (x.a.nickname || String(x.a.uid || '').slice(0, 8)) + '：' +
      x.first.date + ' 到期 ' + fmtTok(x.first.remain) + ' 积分').join('；');
    rows.push(expSummaryRow('var(--warn)', '另有 <b>' + soon.length + '</b> 个账号 4–7 天内到期 · 合计 <b>' +
      fmtTok(weekRemain) + '</b> 积分', esc(names) + (soon.length > 3 ? ' 等' : ''), detail));
  }
  if (failed.length) {
    const names = failed.slice(0, 3).map(a => a.nickname || String(a.uid || '').slice(0, 8)).join('、');
    const detail = failed.map(a => (a.nickname || String(a.uid || '').slice(0, 8)) + '：' + a.error).join('；');
    rows.push(expSummaryRow('var(--ink-3)', failed.length + ' 个账号查询失败',
      esc(names) + (failed.length > 3 ? ' 等' : '') + ' · 点「实时检查」重试', detail));
  }
  if (quiet) {
    rows.push(expSummaryRow('var(--ok)', '其余 <b>' + quiet + '</b> 个账号 7 天内无到期积分'));
  }
  const headline = (urgent.length ? '<b>' + urgent.length + '</b> 个账号 3 天内到期' : '3 天内无到期') +
    (urgent.length ? ' · 最近 <b>' + esc(urgent[0].first.date) + '</b>' : '');
  expSetSummary('<div class="headline">' + headline + '</div>', rows.join('') || '<div class="empty">没有账号</div>');
  // 数据新鲜度透明化：走缓存时标注年龄，免得把旧数据误当实时。
  const ageMin = lastPackages ? Math.floor((Date.now() - lastPackagesAt) / 60000) : 0;
  const state = (urgent.length ? urgent.length + ' 个 3 天内有到期' : '3 天内无到期') +
    (soon.length ? ' · ' + soon.length + ' 个 4–7 天内' : '') +
    (failed.length ? ' · ' + failed.length + ' 个查询失败' : '') +
    (lastPackagesAt && ageMin > 0 ? ' · ' + ageMin + ' 分钟前' : '');
  const note = $('expNote');
  if (note) {
    note.textContent = state;
    note.title = '共 ' + list.length + ' 个账号 · ' +
      (lastPackagesAt && ageMin > 0 ? '缓存数据，可点「实时检查」刷新' : '实时查询上游');
  }
  const box = $('expBox');
  if (box) box.hidden = false;
}

/* 快照版到期提醒：不查上游，直接用 /overview 已有的 credits_earliest_expiry /
   credits_earliest_remaining（由签到与余额后台刷新写入 state.json，默认每 5 分钟
   更新一次）。打开面板即可看到「最近哪批要过期」，成本与首页统计条相同为零。

   与 renderExpiry（逐包明细版）的差异：这里只有**最早一批**的日期与数量，没有
   后续批次、也没法算「7 天内合计」——那是 packages 接口才有的信息。因此卡片上
   保留「检查」按钮作为增强入口，用户想要完整明细再显式触发一次上游查询。 */
function renderExpirySnapshot(d) {
  const list = (d && d.accounts) || [];
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const urgent = [], soon = [], failed = [];
  let quiet = 0;
  for (const a of list) {
    if (a.error) { failed.push(a); continue; }
    // 后端给的是 RFC3339 带时区；无到期信息（长期包/未刷新）的账号排在最后。
    //
    // 必须走 parseAPITime 而不是裸 new Date()：CreditsEarliestExpiry 是 time.Time，
    // json tag 上的 omitempty 对 time.Time **无效**，零值会序列化成
    // "0001-01-01T00:00:00Z"。裸 new Date 把它解析成 -62135596800000（不是 NaN），
    // 「无到期批次」的账号反而排到最前，真正快过期的被挤到最后——功能被反转。
    // parseAPITime 对 "0001-" 前缀显式返回 0，这里只认 ms > 0（= 真有到期时间）。
    const ms = parseAPITime(a.credits_earliest_expiry);
    const remain = Number(a.credits_earliest_remaining || 0);
    if (ms <= 0 || remain <= 0) { quiet++; continue; }
    const rawDays = Math.ceil((ms - today.getTime()) / 86400000);
    if (rawDays > 7) { quiet++; continue; }
    const item = { a, ms, remain, days: Math.max(0, rawDays) };
    (item.days <= 3 ? urgent : soon).push(item);
  }
  urgent.sort((x, y) => x.ms - y.ms);
  soon.sort((x, y) => x.ms - y.ms);
  const rows = expUrgentRows(urgent, ({ a, ms, remain, days }) => {
    const daily = Math.ceil(remain / Math.max(1, days));
    const cls = 'var(--bad)';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    const when = localDate(ms);
    return '<div class="exp-row"><span class="exp-dot" style="background:' + cls + '"></span>' +
      '<span class="exp-nm">' + esc(a.nickname || String(a.uid || '').slice(0, 8)) + '</span>' +
      '<span class="exp-main">最近到期 <b>' + esc(when) + '</b>（' + dayWord +
      '）· 该批 <b>' + fmtTok(remain) + '</b> 积分 · 到期前日均需耗 ≥<b>' +
      fmtTok(daily) + '</b></span></div>';
  });
  if (soon.length) {
    const remainSum = soon.reduce((s, x) => s + x.remain, 0);
    const names = soon.slice(0, 3).map(x => x.a.nickname || String(x.a.uid || '').slice(0, 8)).join('、');
    const detail = soon.map(x => (x.a.nickname || String(x.a.uid || '').slice(0, 8)) + '：' +
      localDate(x.ms) + ' 到期 ' + fmtTok(x.remain) + ' 积分').join('；');
    rows.push(expSummaryRow('var(--warn)', '另有 <b>' + soon.length + '</b> 个账号 4–7 天内到期 · 合计 <b>' +
      fmtTok(remainSum) + '</b> 积分', esc(names) + (soon.length > 3 ? ' 等' : ''), detail));
  }
  if (failed.length) {
    const names = failed.slice(0, 3).map(a => a.nickname || String(a.uid || '').slice(0, 8)).join('、');
    const detail = failed.map(a => (a.nickname || String(a.uid || '').slice(0, 8)) + '：' + a.error).join('；');
    rows.push(expSummaryRow('var(--ink-3)', failed.length + ' 个账号查询失败',
      esc(names) + (failed.length > 3 ? ' 等' : '') + ' · 点「实时检查」重试', detail));
  }
  if (quiet) {
    rows.push(expSummaryRow('var(--ok)', '其余 <b>' + quiet + '</b> 个账号 7 天内无到期积分'));
  }
  const headline = (urgent.length ? '<b>' + urgent.length + '</b> 个账号 3 天内到期' : '3 天内无到期') +
    (urgent.length ? ' · 最近 <b>' + esc(localDate(urgent[0].ms)) + '</b>' : '');
  expSetSummary('<div class="headline">' + headline + '</div>', rows.join('') || '<div class="empty">没有账号</div>');
  const note = $('expNote');
  if (note) {
    note.textContent = (urgent.length ? urgent.length + ' 个 3 天内有到期' : '3 天内无到期') +
      (soon.length ? ' · ' + soon.length + ' 个 4–7 天内' : '') +
      (failed.length ? ' · ' + failed.length + ' 个查询失败' : '') + ' · 快照';
    note.title = '共 ' + list.length + ' 个账号 · 数据来自本地状态；点「实时检查」拉取逐包明细';
  }
  const box = $('expBox');
  if (box) box.hidden = false;
}

/* 到期提醒默认只显示一句摘要；明细按账号折叠，避免首页一打开就是一堵墙。
   摘要本身只描述数量与最紧急日期，不重复明细内容；展开状态用户主动选择后保持。 */
let expDetailsOpen = false;
// overview 每 5 秒轮询一次；到期批次（最早日期/剩余量）通常数小时不变，但此前
// 每轮都无条件重写摘要与最多 5 行明细的 innerHTML，导致持续布局与焦点抖动。
// 缓存上一轮 HTML 签名，内容相同只更新折叠状态/提示文本。
let expSummarySig = '', expDetailSig = '';
function expSetSummary(summaryHtml, detailHtml) {
  const line = $('expSummaryLine');
  const s = summaryHtml || '', d = detailHtml || '';
  if (line && s !== expSummarySig) { line.innerHTML = s; expSummarySig = s; }
  const list = $('expList');
  if (list && d !== expDetailSig) { list.innerHTML = d; expDetailSig = d; }
  const btn = $('btnExpToggle');
  if (btn) {
    btn.hidden = false;
    btn.textContent = expDetailsOpen ? '收起明细' : '展开明细';
    btn.setAttribute('aria-expanded', expDetailsOpen ? 'true' : 'false');
  }
  // 用户主动展开的明细要跨 overview 轮询保持打开；每轮刷新都重置成收起会让
  // 正在看明细的人每个 5 秒就被打断一次。首次渲染仍默认收起。
  if (list) list.hidden = !expDetailsOpen;
}
function expToggleDetails() {
  const list = $('expList');
  const btn = $('btnExpToggle');
  if (!list || !btn) return;
  expDetailsOpen = !expDetailsOpen;
  list.hidden = !expDetailsOpen;
  btn.textContent = expDetailsOpen ? '收起明细' : '展开明细';
  btn.setAttribute('aria-expanded', expDetailsOpen ? 'true' : 'false');
}
// expUrgentRows 只让前 5 个 3 天内到期的账号逐行展示，其余合并成一行可悬停明细；
// 4–7 天、查询失败与无到期账号继续只进汇总，保持首页首屏可扫描。
function expUrgentRows(items, renderRow) {
  const MAX = 5;
  const all = items || [];
  const rows = all.slice(0, MAX).map(renderRow);
  const rest = all.slice(MAX);
  if (rest.length) {
    const detail = rest.map(x => {
      const a = x.a || x;
      const name = a.nickname || String(a.uid || '').slice(0, 8);
      const dayLabel = x.days == null ? '' : '：' + (x.days === 0 ? '今天' : x.days + ' 天后') + '到期';
      return name + dayLabel + '（' + fmtTok(x.remain) + ' 积分）';
    }).join('；');
    rows.push(expSummaryRow('var(--bad)', '其余 <b>' + rest.length + '</b> 个账号 3 天内到期',
      esc(rest.slice(0, 2).map(x => (x.a || x).nickname || '').filter(Boolean).join('、')) +
        (rest.length > 2 ? ' 等' : ''), detail));
  }
  return rows;
}

// expSummaryRow 渲染被折叠的账号汇总（无到期 / 查询失败），避免首页逐账号平铺。
// main/note 是调用方拼好的 HTML；title 是纯文本且在这里统一转义。
function expSummaryRow(color, main, note, title) {
  return '<div class="exp-row exp-sum"' + (title ? ' title="' + esc(title) + '"' : '') + '>' +
    '<span class="exp-dot" style="background:' + color + '"></span>' +
    '<span class="exp-main">' + main + (note ? '<span class="note">' + note + '</span>' : '') +
    '</span></div>';
}

/* localDate 按**本地时区**取 YYYY-MM-DD。不能用 toISOString().slice(0, 10)：
   那是 UTC 日期，东八区 00:00–08:00 之间的时刻会被算成前一天（易用性修复 1）。
   放在 renderExpirySnapshot 之后是刻意的：前端 harness 按 [renderExpirySnapshot,
   loadExpiry) 切片求值，函数声明会被提升，跟着切片一起进入求值上下文。 */
function localDate(d) {
  const t = d instanceof Date ? d : new Date(d);
  const p = n => String(n).padStart(2, '0');
  return t.getFullYear() + '-' + p(t.getMonth() + 1) + '-' + p(t.getDate());
}

async function loadExpiry(force) {
  if (!$('expBox')) return;
  if (expFetching) return;
  const fresh = lastPackages && (Date.now() - lastPackagesAt) < EXP_FRESH_MS;
  if (fresh && !force) { renderExpiry(lastPackages); return; }
  expFetching = true;
  const box = $('expBox');
  if (box) box.hidden = false;
  const list = $('expList');
  if (list && (!list.children || !list.children.length)) list.innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  setText('expNote', '查询中…');
  try {
    const d = await loadPackagesData();
    renderExpiry(d);
  } catch (e) {
    // textContent 不解引用 HTML 实体：这里再 esc 一次只会让用户看到 &amp; 这类字面量。
    setText('expNote', '查询失败：' + (e && e.message ? e.message : String(e)));
  }
  expFetching = false;
}

if ($('btnExp')) $('btnExp').onclick = () => loadExpiry(true);
if ($('btnExpToggle')) $('btnExpToggle').onclick = expToggleDetails;
if ($('btnExpTasks')) $('btnExpTasks').onclick = () => go('taskscenter');

// 「刷新」始终实时查（显式 force，不依赖事件对象真值）
if ($('btnPk')) $('btnPk').onclick = () => loadPackages(true);
