package panel

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func runNodeScript(t *testing.T, name, script string, args ...string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; frontend UX test skipped")
	}
	f, err := os.CreateTemp(t.TempDir(), name+"-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cmdArgs := append([]string{f.Name()}, args...)
	out, err := exec.Command(node, cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s node test failed: %v\n%s", name, err, out)
	}
	return strings.TrimSpace(string(out))
}

// 管理密钥只跨当前标签页的刷新保存：sessionStorage 用于恢复，401 后必须清理；
// 旧版本写在 localStorage 的明文密钥也要在新页面加载时移除。
func TestAppJSKeySessionStorageLifecycle(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const KEY_STORAGE');
const stateEnd = src.indexOf('let theme =', start);
const requestStart = src.indexOf('function authHeaders', stateEnd);
const requestEnd = src.indexOf('// isAbortError', requestStart);
if (start < 0 || stateEnd < 0 || requestStart < 0 || requestEnd < 0) throw new Error('key handling region not found');
const values = { 'wb2api.key': 'stored-key' };
const sessionStorage = {
  getItem: k => Object.prototype.hasOwnProperty.call(values, k) ? values[k] : null,
  setItem: (k, v) => { values[k] = String(v); },
  removeItem: k => { delete values[k]; },
};
let removedLegacy = false, opened = 0;
const localStorage = {
  getItem: () => null,
  setItem() {},
  removeItem: k => { if (k === 'wb2api.key') removedLegacy = true; },
};
const ctx = {
  sessionStorage, localStorage,
  openKey: () => { opened++; },
  fetch: () => Promise.resolve({ status: 401, ok: false }),
  AbortController: undefined,
  setTimeout,
  clearTimeout,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, stateEnd) + src.slice(requestStart, requestEnd) +
  '\nthis.api=api; this.rememberKey=rememberKey; this.forgetKey=forgetKey; this.getMemKey=() => memKey;', ctx);
(async () => {
  const restored = ctx.getMemKey();
  ctx.rememberKey('new-key');
  const saved = values['wb2api.key'];
  ctx.rememberKey('next-key');
  let err = '';
  try { await ctx.api('overview'); } catch (e) { err = e.message; }
  process.stdout.write(JSON.stringify({
    restored,
    saved,
    after401: ctx.getMemKey(),
    storedAfter401: values['wb2api.key'] || '',
    opened,
    legacyRemoved: removedLegacy,
    error: err,
  }));
})();`
	out := runNodeScript(t, "key-session", script, appJSPath(t))
	const want = `{"restored":"stored-key","saved":"new-key","after401":"","storedAfter401":"","opened":1,"legacyRemoved":true,"error":"密钥无效或未填写"}`
	if out != want {
		t.Fatalf("key session lifecycle=%s\nwant %s", out, want)
	}
}

// 日志频道选择必须作用于整页：选「系统」只看系统日志并隐藏请求记录表，
// 选「全部 / 对话」时请求记录表恢复；轮询刷新期间也不能被重新显示。
func TestAppJSLogChannelControlsRequestBox(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadLogs');
const end = src.indexOf('function renderRequestMetrics', start);
if (start < 0 || end < 0) throw new Error('loadLogs not found');
const box = {innerHTML: '', scrollTop: 0, clientHeight: 0, scrollHeight: 0};
const reqBox = {hidden: true};
const chips = [];
const ctx = {
  logsReq: false, logSig: '', logPin: true, logCh: 'sys', logLevel: '', logQ: '',
  api: path => String(path) === 'logs'
    ? Promise.resolve({entries: [
      {ts: '2026-01-01T00:00:00Z', ch: 'sys', text: 'system line'},
      {ts: '2026-01-01T00:00:01Z', ch: 'chat', text: 'chat line'},
    ]})
    : String(path) === 'request_metrics'
      ? Promise.resolve({archive: {enabled: true}})
      : Promise.resolve({entries: [{request_id: 'r1'}]}),
  $: id => id === 'logBox' ? box : id === 'reqBox' ? reqBox : {textContent: '', className: '', value: '100', hidden: false},
  setText: () => {}, noteDOM: () => {},
  trangeQuery: () => ({set() {}, toString: () => ''}),
  syncLogChannelView: () => { reqBox.hidden = ctx.logCh !== 'all' && ctx.logCh !== 'chat'; },
  renderRequestMetrics: () => {}, lastLogMetrics: null, lastLogRows: {entries: []}, lastLogErrors: {},
  esc: s => String(s), fmtLogTime: s => String(s), Promise, Array, Object, String, Number,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadLogs=loadLogs;', ctx);
(async () => {
  await ctx.loadLogs();
  const systemOnly = box.innerHTML.includes('system line') && !box.innerHTML.includes('chat line');
  const hiddenAtSys = reqBox.hidden;
  ctx.logCh = 'all';
  await ctx.loadLogs();
  const allRows = box.innerHTML.includes('system line') && box.innerHTML.includes('chat line');
  const shownAtAll = reqBox.hidden === false;
  process.stdout.write(JSON.stringify({systemOnly, hiddenAtSys, allRows, shownAtAll}));
})();`
	out := runNodeScript(t, "log-channel", script, appJSPath(t))
	const want = `{"systemOnly":true,"hiddenAtSys":true,"allRows":true,"shownAtAll":true}`
	if out != want {
		t.Fatalf("log channel=%s\nwant %s", out, want)
	}
}

func appJSPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// 首页 3 天内到期过多时只逐行展示前 5 个，其余折叠成一行并在 title 里保留明细。
func TestAppJSExpiryUrgentCollapsed(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderExpirySnapshot');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry snapshot region not found');
const ptStart = src.indexOf('function parseAPITime');
const ptEnd = src.indexOf('function fmtLocalDateTime');
if (ptStart < 0 || ptEnd < 0 || ptEnd < ptStart) throw new Error('parseAPITime not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', title: '', hidden: true, attrs: {}, setAttribute(k, v) { this.attrs[k] = v; } });
    const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN,
};
vm.createContext(ctx);
vm.runInContext(src.slice(ptStart, ptEnd) + src.slice(start, end) +
  '\nthis.renderExpirySnapshot = renderExpirySnapshot;', ctx);
const day = n => {
  const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + n);
  return d.toISOString();
};
const accounts = [];
for (let i = 0; i < 8; i++) accounts.push({
  uid: 'u' + i, nickname: '账号' + (i + 1),
  credits_earliest_expiry: day(1), credits_earliest_remaining: 100 + i,
});
ctx.renderExpirySnapshot({ accounts });
const html = sinks['expList'].innerHTML;
const names = [...html.matchAll(/<span class="exp-nm">([^<]*)<\/span>/g)].map(m => m[1]);
const summary = html.match(/<div class="exp-row exp-sum" title="([^"]*)"/);
process.stdout.write(JSON.stringify({
  names,
  rows: names.length,
  summary: !!summary,
  summaryText: summary ? summary[1].includes('账号6：') && summary[1].includes('账号8：') : false,
  countText: sinks['expNote'].textContent,
}));`
	out := runNodeScript(t, "expiry-urgent", script, appJSPath(t))
	const want = `{"names":["账号1","账号2","账号3","账号4","账号5"],"rows":5,"summary":true,"summaryText":true,"countText":"8 个 3 天内有到期 · 快照"}`
	if out != want {
		t.Fatalf("expiry urgent collapse=%s\nwant %s", out, want)
	}
}

// 活动记录首次「加载更多」（200→400）失败后必须保留重试入口：按钮不能因
// prevLimit==200 被隐藏，否则用户只能刷新整页。第二次失败（已有 400 行）还要
// 追加可重试错误行并替换旧的错误行。
func TestAppJSActivityLoadMoreRetry(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadActivity(more)');
const end = src.indexOf('let actReq = null;', start);
if (start < 0 || end < 0) throw new Error('loadActivity not found');
let oldErr = null;
let removedOld = false;
let appended = 0;
const body = {
  innerHTML: '<tr data-old="1">old</tr>',
  querySelector: () => oldErr,
  insertAdjacentHTML: (where, html) => { appended++; body.innerHTML += html; },
};
const note = { textContent: '' };
const btn = { disabled: false, textContent: '', parentElement: { hidden: false }, onclick: null };
const ctx = {
  view: 'activity',
  $: id => id === 'actBody' ? body : id === 'actNote' ? note : id === 'btnActMore' ? btn : null,
  trangeQuery: () => ({ set() {}, toString() { return 'range=1'; } }),
  api: () => Promise.reject(new Error('boom')),
  esc: s => String(s == null ? '' : s),
  Promise, Number, String, Object, Math,
};
vm.createContext(ctx);
vm.runInContext('let actLimit = 200, actRows = [{n: 1}], actSeq = 0, actCtrl = null, actReq = null;\n' +
  src.slice(start, end) + '\nthis.loadActivity = loadActivity;' +
  '\nthis.setLimit = n => { actLimit = n; }; this.getLimit = () => actLimit;', ctx);
(async () => {
  await ctx.loadActivity(true);
  const first = {
    hidden: btn.parentElement.hidden,
    text: btn.textContent,
    disabled: btn.disabled,
    limit: ctx.getLimit(),
    keptRows: body.innerHTML.includes('data-old="1"'),
    appended,
  };
  ctx.setLimit(400);
  oldErr = { remove() { removedOld = true; } };
  await ctx.loadActivity(true);
  const second = {
    hidden: btn.parentElement.hidden,
    text: btn.textContent,
    limit: ctx.getLimit(),
    removedOld,
    appended: body.innerHTML.includes('act-more-err'),
  };
  process.stdout.write(JSON.stringify({ first, second, note: note.textContent }));
})();`
	out := runNodeScript(t, "activity-retry", script, appJSPath(t))
	const want = `{"first":{"hidden":false,"text":"重试加载更多","disabled":false,"limit":200,"keptRows":true,"appended":0},"second":{"hidden":false,"text":"重试加载更多","limit":400,"removedOld":true,"appended":true},"note":"读取失败"}`
	if out != want {
		t.Fatalf("activity load-more retry=%s\nwant %s", out, want)
	}
}

// 用量明细关键词：空格分词 AND，匹配账号昵称/模型/域，空词返回全部。
func TestAppJSUsageFilterRows(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function usFilterRows');
const end = src.indexOf('function usDimHead');
if (start < 0 || end < 0 || end < start) throw new Error('usFilterRows not found');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.usFilterRows=usFilterRows;', ctx);
const rows = [
  { key: 'aaaaaaaa-1111', extra: '秋叶', realm: 'cn', requests: 5 },
  { key: 'bbbbbbbb-2222', extra: '春风', realm: 'global', requests: 3 },
  { key: 'glm-5.3', extra: '', realm: 'cn', requests: 9 },
];
const ids = f => ctx.usFilterRows(rows, f).map(x => x.key);
process.stdout.write(JSON.stringify({
  all: ids(''),
  byNick: ids('秋叶'),
  byRealm: ids('global'),
  byModel: ids('glm 5.3'),
  andMiss: ids('秋叶 global'),
}));`
	out := runNodeScript(t, "usage-filter", script, appJSPath(t))
	const want = `{"all":["aaaaaaaa-1111","bbbbbbbb-2222","glm-5.3"],"byNick":["aaaaaaaa-1111"],"byRealm":["bbbbbbbb-2222"],"byModel":["glm-5.3"],"andMiss":[]}`
	if out != want {
		t.Fatalf("usage filter=%s want %s", out, want)
	}
}

// 导出按钮在下载完成前 disabled + 「导出中…」，成功 toast，finally 恢复原文案；
// 失败同样恢复且不显示成功提示。
func TestAppJSExportBusyState(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function withExportBusy');
const end = src.indexOf('if ($(\'btnReqExportCsv\'))');
if (start < 0 || end < 0 || end < start) throw new Error('withExportBusy not found');
const toasts = [];
const ctx = { toast: (m, c) => toasts.push([m, c]), Promise };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.withExportBusy=withExportBusy;', ctx);
const btn = { disabled: false, textContent: '导出 CSV' };
const states = [];
let release;
const pending = ctx.withExportBusy(btn, () => new Promise(resolve => { release = resolve; }));
(async () => {
  await Promise.resolve();
  states.push(btn.disabled && btn.textContent === '导出中…');
  release();
  await pending;
  states.push(!btn.disabled && btn.textContent === '导出 CSV');
  const okToast = toasts.length === 1 && toasts[0][1] === 'ok';
  const errBtn = { disabled: false, textContent: '导出 JSONL' };
  await ctx.withExportBusy(errBtn, () => Promise.reject(new Error('boom')));
  process.stdout.write(JSON.stringify({
    busy: states[0], restored: states[1], okToast,
    failedRestored: !errBtn.disabled && errBtn.textContent === '导出 JSONL',
    failToast: toasts.length === 2 && toasts[1][0].includes('boom') && toasts[1][1] === 'err',
  }));
})().catch(e => { process.stdout.write('ERR ' + (e && e.stack || e)); process.exitCode = 1; });`
	out := runNodeScript(t, "export-busy", script, appJSPath(t))
	const want = `{"busy":true,"restored":true,"okToast":true,"failedRestored":true,"failToast":true}`
	if out != want {
		t.Fatalf("export busy=%s want %s", out, want)
	}
}

// 弹窗 Tab 焦点循环：Tab 在末项回首项，Shift+Tab 在首项回末项；焦点不在弹窗内不拦截。
func TestAppJSModalFocusTrap(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function modalFocusables');
const end = src.indexOf('function ago(');
if (start < 0 || end < 0 || end < start) throw new Error('modal focus trap not found');
const first = { id: 'first', hidden: false, getAttribute: () => null, focus() { active = this; } };
const last = { id: 'last', hidden: false, getAttribute: () => null, focus() { active = this; } };
let active = last;
const veil = {
  contains: el => el === first || el === last,
  querySelectorAll: () => [first, last],
};
const ctx = {
  document: {
    querySelectorAll: sel => sel === '.veil.on' ? [veil] : [],
    activeElement: last,
  },
  Array, String,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.trapModalTab=trapModalTab;', ctx);
let prevented = false;
ctx.trapModalTab({ key: 'Tab', shiftKey: false, preventDefault() { prevented = true; } });
const wrappedForward = prevented && active === first;
active = first; ctx.document.activeElement = first; prevented = false;
ctx.trapModalTab({ key: 'Tab', shiftKey: true, preventDefault() { prevented = true; } });
const wrappedBackward = prevented && active === last;
const outside = { id: 'outside', focus() {} };
ctx.document.activeElement = outside;
prevented = false;
ctx.trapModalTab({ key: 'Tab', shiftKey: false, preventDefault() { prevented = true; } });
process.stdout.write(JSON.stringify({ wrappedForward, wrappedBackward, outsideUntouched: !prevented }));`
	out := runNodeScript(t, "modal-focus", script, appJSPath(t))
	const want = `{"wrappedForward":true,"wrappedBackward":true,"outsideUntouched":true}`
	if out != want {
		t.Fatalf("modal focus trap=%s want %s", out, want)
	}
}

// 账号表轮询去重：所有可见字段变化都必须触发重建；同一分钟内的同一份快照
// 仍跳过。签名漏字段会让 realm/企业标记/tooltip/相对时间长期停在旧值。
func TestAppJSAccountRenderSignature(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function accountRowSig');
const end = src.indexOf('function renderAccounts');
if (start < 0 || end < 0 || end < start) throw new Error('accountRowSig not found');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.accountRowSig=accountRowSig;', ctx);
const base = {
  uid: 'u1', nickname: '秋叶', realm: 'cn', enterprise: false, has_tokens: true,
  credits: 100, credits_total: 200, credits_expiring: 10,
  cooling: false, cool_remaining_sec: 0, cool_kind: '', disabled: false, paused: false,
  in_flight: 0, success_count: 7, err_total: 1, last_success: '2026-10-18T10:00:00Z',
  checkin_done: true, breaker_until: '', degrade_until: '', reason: '',
  token_usage: { request_count: 3, total_tokens: 900, last_latency_ms: 120, last_tokens_per_second: 50 },
  model_costs: [{ model: 'glm-5.3', cost_per_1k: 0.5, last_seen: '2026-10-18T10:00:00Z', samples: 4 }],
  rate_limited_models: [{ model: 'glm-5.3', kind: 'rate_limit', until: '2026-10-18T12:00:00Z', reset_at: '' }],
};
const sig = o => ctx.accountRowSig(o);
const same = sig(base) === sig(JSON.parse(JSON.stringify(base)));
const changed = {
  realm: sig({ ...base, realm: 'global' }),
  enterprise: sig({ ...base, enterprise: true }),
  lastSuccess: sig({ ...base, last_success: '2026-10-18T11:00:00Z' }),
  cost: sig({ ...base, model_costs: [{ model: 'glm-5.3', cost_per_1k: 0.25, last_seen: '2026-10-18T10:00:00Z', samples: 4 }] }),
  limitKind: sig({ ...base, rate_limited_models: [{ model: 'glm-5.3', kind: 'model_unavailable', until: '2026-10-18T12:00:00Z', reset_at: '' }] }),
  usage: sig({ ...base, token_usage: { ...base.token_usage, last_latency_ms: 300 } }),
  weight: sig({ ...base, weight: 5 }),
  note: sig({ ...base, note: '客户A' }),
  tokens: sig({ ...base, has_tokens: false }),
  costCount: sig({ ...base, model_costs: [...base.model_costs, { model: 'kimi-k3', cost_per_1k: 1, last_seen: '2026-10-18T10:00:00Z', samples: 1 }] }),
  limitCount: sig({ ...base, rate_limited_models: [...base.rate_limited_models, { model: 'kimi-k3', kind: 'rate_limit', until: '2026-10-18T12:00:00Z', reset_at: '' }] }),
};
const allChanged = Object.values(changed).every(s => s !== sig(base));
process.stdout.write(JSON.stringify({ same, allChanged }));`
	out := runNodeScript(t, "account-render-sig", script, appJSPath(t))
	const want = `{"same":true,"allChanged":true}`
	if out != want {
		t.Fatalf("account render signature=%s want %s", out, want)
	}
}

// 到期卡默认只显示摘要：明细必须 hidden，摘要不能为空；展开按钮可切换。
func TestAppJSExpirySummaryCollapsed(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function expSetSummary');
const end = src.indexOf('function expUrgentRows');
if (start < 0 || end < 0 || end < start) throw new Error('expiry helper region not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', hidden: true, attrs: {}, setAttribute(k, v) { this.attrs[k] = v; } });
const ctx = { expDetailsOpen: false, expSummarySig: '', expDetailSig: '', esc: s => String(s == null ? '' : s), fmtTok: v => String(v), $: id => sinks[id] || mk(id), Date, Math, Number, String, Map, Array, Object };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.expSetSummary=expSetSummary;this.expToggleDetails=expToggleDetails;', ctx);
ctx.expSetSummary('<div class="headline">2 个账号 3 天内到期</div>', '<div class="exp-row">明细</div>');
const collapsed = sinks.expList.hidden === true && sinks.btnExpToggle.hidden === false &&
  sinks.expSummaryLine.innerHTML.includes('2 个账号') && sinks.btnExpToggle.textContent === '展开明细';
ctx.expToggleDetails();
const opened = sinks.expList.hidden === false && sinks.btnExpToggle.textContent === '收起明细' && sinks.btnExpToggle.attrs['aria-expanded'] === 'true';
ctx.expToggleDetails();
const closed = sinks.expList.hidden === true && sinks.btnExpToggle.textContent === '展开明细' && sinks.btnExpToggle.attrs['aria-expanded'] === 'false';
process.stdout.write(JSON.stringify({ collapsed, opened, closed }));`
	out := runNodeScript(t, "expiry-collapse", script, appJSPath(t))
	const want = `{"collapsed":true,"opened":true,"closed":true}`
	if out != want {
		t.Fatalf("expiry collapse=%s want %s", out, want)
	}
}

// 请求归档读取失败时必须显式报错，不能回落成「暂无请求记录」。
func TestAppJSRequestLogFailureVisible(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderRequestMetrics');
const end = src.indexOf('/* reqMatch');
if (start < 0 || end < 0 || end < start) throw new Error('request metrics region not found');
const sinks = { reqSummary: { textContent: '', className: '' }, reqNote: { textContent: '', className: '' }, reqCount: { textContent: '', className: '' }, reqBody: { innerHTML: '' } };
const ctx = {
  $: id => sinks[id] || { textContent: '', className: '', innerHTML: '' },
  setText: (id, v) => { const el = sinks[id]; if (el) el.textContent = v; },
  fmtTok: v => String(v == null ? 0 : v), fmtMs: v => String(v == null ? 0 : v), fmtBytes: v => String(v),
  reqEntries: [], reqFilter: {}, esc: s => String(s == null ? '' : s),
  requestLogText: () => '', reqOutcomeTag: () => '', reqTokenCell: () => '—', reqCreditCell: () => '—',
  renderRequestTable: () => {},
  lastLogErrors: { metrics: null, archive: new Error('disk read failed') },
  Date, Number, String, Math, Array, Object,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderRequestMetrics=renderRequestMetrics;', ctx);
ctx.renderRequestMetrics({ completed: 0, archive: { enabled: true } }, [], ctx.lastLogErrors);
process.stdout.write(JSON.stringify({
  note: sinks.reqNote.textContent.includes('归档读取失败'),
  errClass: sinks.reqNote.className.includes('err'),
  count: sinks.reqCount.textContent.includes('归档读取失败'),
}));`
	out := runNodeScript(t, "req-log-error", script, appJSPath(t))
	const want = `{"note":true,"errClass":true,"count":true}`
	if out != want {
		t.Fatalf("request log error=%s want %s", out, want)
	}
}

// 窄屏规则与提示文案必须真实存在于 index.html，防止只在 JS 里改了但没有 CSS/说明。
func TestPanelIndexUXMarkup(t *testing.T) {
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		`id="usQ"`,
		`id="view-activity"`,
		`id="actRange"`,
		`<a href="#activity" data-view="activity">`,
		`搜索仅影响列表，导出不含搜索词`,
		`@media (max-width: 700px)`,
		`<header class="topbar"`,
		// 日志行必须是 flex 列：后置的 display:block 会把时间/级别/频道/正文
		// 压成一行纯文本，Hub 的终端式分列在本页完全失效。
		`#logBox .ln { display: flex; align-items: flex-start; gap: 8px;`,
	} {
		if !bytes.Contains(b, []byte(needle)) {
			t.Errorf("index.html missing %q", needle)
		}
	}
}

// 性能分位卡：空数据要明确说明原因；有数据时按模型渲染，并标出 p99/截断。
func TestAppJSPerfStatsRendering(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function perfPct');
const end = src.indexOf('async function loadPerf');
if (start < 0 || end < 0 || end < start) throw new Error('perf render region not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', value: '', title: '', hidden: false });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  fmtMs: v => v == null ? '—' : Math.round(Number(v)) + 'ms',
  $: id => (sinks[id] || mk(id)),
  Object, Number, String, Math, Array,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderPerfStats=renderPerfStats;', ctx);
ctx.renderPerfStats({ sampled: 0, by_model: {} });
const empty = { hidden: sinks.usPerfTable.hidden, text: sinks.usPerfEmpty.textContent };
ctx.renderPerfStats({
  sampled: 12, sample_limit: 50000, sample_capped: true,
  by_model: { 'glm-5.3': {
    requests: 12, wall_ms: { p50: 1000, p90: 2000, p99: 3000, max: 4000, samples: 12 },
    ttft_ms: { p50: 100, p90: 200, p99: 300, max: 400, samples: 12 },
    tokens_per_sec: { p50: 42.5 },
  } },
});
const row = sinks.usPerfBody.innerHTML;
process.stdout.write(JSON.stringify({
  empty,
  tableVisible: !sinks.usPerfTable.hidden,
  rowHasModel: row.includes('glm-5.3'),
  rowHasP99: row.includes('3000ms') && row.includes('300ms'),
  rowHasTps: row.includes('42.5 tok/s'),
  noteCapped: sinks.usPerfNote.textContent.includes('已截断'),
}));`
	out := runNodeScript(t, "perf-render", script, appJSPath(t))
	const want = `{"empty":{"hidden":true,"text":"当前窗口暂无请求归档；确认 logging.request_archive_enabled 已开启。"},"tableVisible":true,"rowHasModel":true,"rowHasP99":true,"rowHasTps":true,"noteCapped":true}`
	if out != want {
		t.Fatalf("perf render=%s\nwant %s", out, want)
	}
}

// 导入必须先预览、确认按钮只在有新增/更新时出现；dry-run 提示不能伪装成已完成。
func TestAppJSImportPreviewState(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function importPreviewText');
const end = src.indexOf('async function importCockpitFile');
if (start < 0 || end < 0 || end < start) throw new Error('import preview region not found');
const ctx = { Object, Array, Number, String };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.importPreviewText=importPreviewText;', ctx);
const preview = ctx.importPreviewText({ dry_run: true, total: 5, added: 2, updated: 1, skipped: 2, invalid: 0 });
const committed = ctx.importPreviewText({ dry_run: false, total: 5, added: 2, updated: 1, skipped: 2, invalid: 0 });
process.stdout.write(JSON.stringify({ preview, committed }));`
	out := runNodeScript(t, "import-preview", script, appJSPath(t))
	const want = `{"preview":"预览：共 5 个 · 新增 2 · 更新 1 · 跳过 2 · 无效 0","committed":"共 5 个 · 新增 2 · 更新 1 · 跳过 2 · 无效 0"}`
	if out != want {
		t.Fatalf("import preview=%s\nwant %s", out, want)
	}
}

// 积分扣减历史：记录行必须带聚合粒度与请求数，金额/Token 落在各自行；
// 空数据走明确空态，grant 不渲染任何伪造领取行。
func TestAppJSCreditHistoryRendering(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const CREDIT_GRAN_LABEL');
const end = src.indexOf('async function loadCreditHistory');
if (start < 0 || end < 0 || end < start) throw new Error('credit history render region not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', hidden: false });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtCredit: v => String(Number(v || 0).toFixed(2)),
  fmtTok: v => String(Number(v || 0)),
  creditHistData: null,
  usCreditKind: 'all',
  $: id => (sinks[id] || mk(id)),
  Date, Number, String, Math, Array, Object,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderCreditHistory=renderCreditHistory;', ctx);
process.stdout.write(JSON.stringify((() => {
  ctx.renderCreditHistory({
    total: 2, limit: 200, truncated: false, credits: 4.5,
    records: [
      {time: '2026-10-18T08:00:00+08:00', scope: 'hour', granularity: 'hourly', kind: 'spend',
       account: '秋叶(uid-1234)', realm: 'cn', model: 'glm-5.3', credit: 1.5, tokens: 100, requests: 3},
      {time: '2026-10-17T00:00:00+08:00', scope: 'day', granularity: 'daily', kind: 'spend',
       account: '春风(uid-5678)', realm: 'global', model: 'kimi-k3', credit: 3, tokens: 200, requests: 9},
    ],
  });
  const rows = sinks.usCreditHistBody.innerHTML;
  const renderedNote = sinks.usCreditHistNote.textContent;
  const first = {hidden: sinks.usCreditHistTable.hidden, emptyHidden: sinks.usCreditHistEmpty.hidden};
  ctx.renderCreditHistory({total: 0, records: []});
  const emptyAllNote = sinks.usCreditHistNote.textContent;
  ctx.usCreditKind = 'spend';
  ctx.renderCreditHistory({total: 0, records: []});
  const emptySpendNote = sinks.usCreditHistNote.textContent;
  return {
    emptyAllNote, emptySpendNote,
    first,
    rowOrder: rows.indexOf('glm-5.3') >= 0 && rows.indexOf('glm-5.3') < rows.indexOf('kimi-k3'),
    hourlyLabel: rows.includes('按小时聚合') && rows.includes('10-18 08:00'),
    dailyLabel: rows.includes('按日聚合') && rows.includes('10-17'),
    requestsVisible: rows.includes('>3<') && rows.includes('>9<'),
    amountOwnRow: rows.includes('−1.50') && rows.includes('−3.00'),
    tokenOwnRow: rows.includes('>100<') && rows.includes('>200<'),
    noGrantRows: !rows.includes('获取'),
    note: renderedNote,
    emptyText: sinks.usCreditHistEmpty.textContent,
    emptyHidden: sinks.usCreditHistEmpty.hidden,
    tableHidden: sinks.usCreditHistTable.hidden,
  };
})()));`
	out := runNodeScript(t, "credit-history-render", script, appJSPath(t))
	const want = `{"emptyAllNote":"0 条 · 全部扣减","emptySpendNote":"0 条 · 仅扣减","first":{"hidden":false,"emptyHidden":true},"rowOrder":true,"hourlyLabel":true,"dailyLabel":true,"requestsVisible":true,"amountOwnRow":true,"tokenOwnRow":true,"noGrantRows":true,"note":"2 条聚合记录 · 汇总扣减 4.50","emptyText":"当前时间窗暂无积分扣减记录。旧版仅有 Token 的历史不会伪造积分；聚合桶也不会被拆成逐请求行。","emptyHidden":false,"tableHidden":true}`
	if out != want {
		t.Fatalf("credit history render=%s\nwant %s", out, want)
	}
}

// grant 没有可靠数据源：切到该页签必须显示 501 口径的空态，且按钮保持可用。
func TestAppJSCreditGrantUnavailable(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function creditHistoryGrantUnavailable');
const end = src.indexOf('async function loadCreditHistory');
if (start < 0 || end < 0 || end < start) throw new Error('grant unavailable region not found');
const sinks = {};
const mk = id => (sinks[id] = {innerHTML: '', textContent: '', hidden: false, disabled: false, attrs: {}, setAttribute(k, v) { this.attrs[k] = v; }, removeAttribute(k) { delete this.attrs[k]; }});
const ctx = { creditHistData: {records: [{kind: 'spend'}]}, $: id => (sinks[id] || mk(id)), String };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.creditHistoryGrantUnavailable=creditHistoryGrantUnavailable;', ctx);
ctx.creditHistoryGrantUnavailable();
process.stdout.write(JSON.stringify({
  tableHidden: sinks.usCreditHistTable.hidden,
  emptyVisible: !sinks.usCreditHistEmpty.hidden,
  emptyText: sinks.usCreditHistEmpty.textContent,
  note: sinks.usCreditHistNote.textContent,
  cleared: ctx.creditHistData === null,
}));`
	out := runNodeScript(t, "credit-grant-unavailable", script, appJSPath(t))
	const want = `{"tableHidden":true,"emptyVisible":true,"emptyText":"积分获取历史暂不可用：本地只有按小时/日聚合的 usage.credit 观测，无法可靠还原逐笔领取。","note":"接口未提供 · 不伪造","cleared":true}`
	if out != want {
		t.Fatalf("grant unavailable=%s\nwant %s", out, want)
	}
}

// busy 必须覆盖成功/失败两条路径：fetch 未决时 disabled + aria-busy，
// 成功/失败后都恢复，失败时显示错误态而不是旧表格。
func TestAppJSCreditHistoryBusyRestore(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadCreditHistory');
const end = src.indexOf('function renderUsage(d)');
if (start < 0 || end < 0 || end < start) throw new Error('loadCreditHistory region not found');
const sinks = {};
const mk = id => (sinks[id] = {innerHTML: '', textContent: '', hidden: false, disabled: false, attrs: {}, setAttribute(k, v) { this.attrs[k] = v; }, removeAttribute(k) { delete this.attrs[k]; }});
let reject = null;
const ctx = {
  usCreditKind: 'spend', creditHistSeq: 0, creditCtrl: null,
  trangeQuery: () => new URLSearchParams(),
  api: () => new Promise((resolve, rejectFn) => { reject = rejectFn; }),
  renderCreditHistory: () => {},
  $: id => (sinks[id] || mk(id)),
  URLSearchParams, String, Number, Object, Error,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadCreditHistory=loadCreditHistory;', ctx);
(async () => {
  const p = ctx.loadCreditHistory();
  const during = {disabled: sinks.btnCreditHist.disabled, text: sinks.btnCreditHist.textContent, busy: sinks.btnCreditHist.attrs['aria-busy']};
  const err = new Error('boom');
  err.status = 500;
  reject(err);
  await p;
  const after = {disabled: sinks.btnCreditHist.disabled, text: sinks.btnCreditHist.textContent, busy: sinks.btnCreditHist.attrs['aria-busy']};
  const failure = {tableHidden: sinks.usCreditHistTable.hidden, emptyVisible: !sinks.usCreditHistEmpty.hidden, emptyText: sinks.usCreditHistEmpty.textContent};
  const p2 = ctx.loadCreditHistory();
  ctx.api = () => Promise.resolve({records: []});
  const err2 = new Error('gone');
  err2.status = 501;
  reject(err2);
  await p2;
  const grantFailure = {text: sinks.usCreditHistEmpty.textContent, note: sinks.usCreditHistNote.textContent};
  process.stdout.write(JSON.stringify({during, after, failure, grantFailure}));
})();`
	out := runNodeScript(t, "credit-busy", script, appJSPath(t))
	const want = `{"during":{"disabled":true,"text":"读取中…","busy":"true"},"after":{"disabled":false,"text":"刷新流水"},"failure":{"tableHidden":true,"emptyVisible":true,"emptyText":"积分扣减历史读取失败：boom"},"grantFailure":{"text":"积分获取历史暂不可用：本地聚合桶无法可靠还原逐笔领取。","note":"接口未提供"}}`
	if out != want {
		t.Fatalf("credit busy=%s\nwant %s", out, want)
	}
}

// 分位请求只守自己的世代：第二次刷新必须中止第一次，且旧响应晚到也不能覆盖新结果。
func TestAppJSPerfRequestGeneration(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadPerf');
const end = src.indexOf('async function loadUsage');
if (start < 0 || end < 0 || end < start) throw new Error('loadPerf region not found');
class TestAbortController {
  constructor() { this.signal = { aborted: false }; }
  abort() { this.signal.aborted = true; }
}
const calls = [];
const rendered = [];
const ctx = {
  perfSeq: 0, perfCtrl: null,
  perfQueryFromUsage: () => ({toString: () => 'from=usage'}),
  api: (path, opts) => new Promise((resolve, reject) => calls.push({path, opts, resolve, reject})),
  renderPerfStats: d => rendered.push(d),
  $: () => null,
  AbortController: TestAbortController,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadPerf=loadPerf;', ctx);
(async () => {
  const first = ctx.loadPerf();
  const firstSignal = calls[0].opts.signal;
  const second = ctx.loadPerf();
  const secondSignal = calls[1].opts.signal;
  const firstAborted = firstSignal.aborted;
  calls[1].resolve({sampled: 2, by_model: {newer: {}}});
  await second;
  calls[0].resolve({sampled: 1, by_model: {older: {}}});
  await first;
  process.stdout.write(JSON.stringify({
    firstAborted,
    secondAborted: secondSignal.aborted,
    paths: calls.map(c => c.path),
    rendered: rendered.map(d => d.sampled),
  }));
})();`
	out := runNodeScript(t, "perf-generation", script, appJSPath(t))
	const want = `{"firstAborted":true,"secondAborted":false,"paths":["usage/perf?from=usage","usage/perf?from=usage"],"rendered":[2]}`
	if out != want {
		t.Fatalf("perf generation=%s\nwant %s", out, want)
	}
}

// 流水使用独立取消器：手动刷新不能碰 /usage，旧流水响应也不能覆盖新流水。
func TestAppJSCreditHistoryRequestGeneration(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadCreditHistory');
const end = src.indexOf('function renderUsage(d)');
if (start < 0 || end < 0 || end < start) throw new Error('loadCreditHistory region not found');
class TestAbortController {
  constructor() { this.signal = { aborted: false }; }
  abort() { this.signal.aborted = true; }
}
const calls = [];
const rendered = [];
let usageAborted = false;
const sink = () => ({ disabled: false, textContent: '', hidden: false, attrs: {}, setAttribute(k, v) { this.attrs[k] = v; }, removeAttribute(k) { delete this.attrs[k]; } });
const ctx = {
  usCreditKind: 'all', creditHistSeq: 0, creditCtrl: null,
  usageCtrl: {abort() { usageAborted = true; }},
  trangeQuery: () => new URLSearchParams('range=usage'),
  api: (path, opts) => new Promise((resolve, reject) => calls.push({path, opts, resolve, reject})),
  renderCreditHistory: d => rendered.push(d),
  $: () => sink(),
  AbortController: TestAbortController, URLSearchParams,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadCreditHistory=loadCreditHistory;', ctx);
(async () => {
  const first = ctx.loadCreditHistory();
  const usageAbortedAfterStart = usageAborted;
  const firstSignal = calls[0].opts.signal;
  const second = ctx.loadCreditHistory();
  const firstAborted = firstSignal.aborted;
  calls[1].resolve({records: [{id: 'newer'}]});
  await second;
  calls[0].resolve({records: [{id: 'older'}]});
  await first;
  process.stdout.write(JSON.stringify({
    usageAbortedAfterStart,
    firstAborted,
    paths: calls.map(c => c.path),
    rendered: rendered.map(d => d.records[0].id),
  }));
})();`
	out := runNodeScript(t, "credit-generation", script, appJSPath(t))
	const want = `{"usageAbortedAfterStart":false,"firstAborted":true,"paths":["credits/history?range=usage&kind=all&limit=200","credits/history?range=usage&kind=all&limit=200"],"rendered":["newer"]}`
	if out != want {
		t.Fatalf("credit generation=%s\nwant %s", out, want)
	}
}

// TestAppJSCreditKindSwitchInvalidatesBoth 扣减/获取共用一张表，切页签时必须
// 同时作废两类请求；否则迟到响应会在另一页签下覆盖表头与行。
func TestAppJSCreditKindSwitchInvalidatesBoth(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf("if ($('usCreditKindTabs'))");
const end = src.indexOf("if ($('usSort'))", start);
if (start < 0 || end < 0 || end < start) throw new Error('credit kind region not found');
let spendAborted = false, grantAborted = false, grantLoads = 0, historyLoads = 0;
const el = () => ({ hidden: false, innerHTML: '', textContent: '' });
const ctx = {
  usCreditKind: 'spend', creditHistSeq: 4, creditGrantSeq: 7,
  creditCtrl: { abort() { spendAborted = true; } },
  creditGrantCtrl: { abort() { grantAborted = true; } },
  $: () => el(), document: { querySelectorAll: () => [] },
  loadCreditGrants: () => { grantLoads++; }, loadCreditHistory: () => { historyLoads++; },
  setHTML: () => {}, noteUsageDomMismatch: () => {}, setText: () => {},
  creditSpendStatsHtml: '', creditSpendNote: '',
};
vm.createContext(ctx);
const tabs = { addEventListener(_, fn) { ctx.__fn = fn; } };
ctx.$ = id => id === 'usCreditKindTabs' ? tabs : el();
vm.runInContext(src.slice(start, end), ctx);
ctx.__fn({ target: { closest: () => ({ dataset: { kind: 'grant' } }) }, currentTarget: { querySelector: () => null } });
process.stdout.write(JSON.stringify({
  spendAborted, grantAborted, spendSeq: ctx.creditHistSeq, grantSeq: ctx.creditGrantSeq,
  grantLoads, historyLoads,
}));`
	out := runNodeScript(t, "credit-kind-switch", script, appJSPath(t))
	const want = `{"spendAborted":true,"grantAborted":true,"spendSeq":5,"grantSeq":8,"grantLoads":1,"historyLoads":0}`
	if out != want {
		t.Fatalf("credit kind switch=%s\nwant %s", out, want)
	}
}

// 新的用量窗口一开始就作废旧分位/流水请求，避免 /usage 未返回时旧子区块先回写。
func TestAppJSUsageInvalidatesChildRequests(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadUsage');
const end = src.indexOf('/* ── 积分构成', start);
if (start < 0 || end < 0 || end < start) throw new Error('loadUsage region not found');
class TestAbortController {
  constructor() { this.signal = { aborted: false }; }
  abort() { this.signal.aborted = true; }
}
let perfAborted = false, creditAborted = false, perfCalls = 0, creditCalls = 0;
const ctx = {
  usageSeq: 0, usageCtrl: null, perfSeq: 10, creditHistSeq: 20,
  perfCtrl: {abort() { perfAborted = true; }},
  creditCtrl: {abort() { creditAborted = true; }},
  $: id => id === 'view-usage' ? { hidden: false } : null,
  esc: s => String(s), fmtTok: v => String(v),
  loadRemaining: () => {}, noteDOM: () => {}, noteUsageDomMismatch: () => {},
  trangeQuery: () => ({toString: () => 'range=usage'}),
  warmUsageModelRates: () => {},
  api: () => Promise.resolve({}),
  renderUsage: () => {},
  loadRemaining: () => {},
  loadPerf: () => { perfCalls++; },
  loadCreditHistory: () => { creditCalls++; },
  AbortController: TestAbortController,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadUsage=loadUsage;', ctx);
(async () => {
  await ctx.loadUsage();
  process.stdout.write(JSON.stringify({
    perfAborted, creditAborted,
    perfSeq: ctx.perfSeq, creditHistSeq: ctx.creditHistSeq,
    perfCalls, creditCalls,
  }));
})();`
	out := runNodeScript(t, "usage-invalidates-children", script, appJSPath(t))
	const want = `{"perfAborted":true,"creditAborted":true,"perfSeq":11,"creditHistSeq":21,"perfCalls":1,"creditCalls":1}`
	if out != want {
		t.Fatalf("usage invalidation=%s\nwant %s", out, want)
	}
}

// 导航分组、积分流水折叠汇总和窄屏卡片式表格必须真实存在于标记里。
func TestPanelCreditHistoryMarkup(t *testing.T) {
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		`<ul class="main-nav">`,
		`id="themeDropdown"`,
		`data-theme-opt="light"`,
		`data-theme-opt="dark"`,
		`data-theme-opt="auto"`,
		`id="usCreditKindTabs"`,
		`data-kind="grant" title="本地用量桶无法还原领取流水"`,
		`data-kind="spend"`,
		`id="usCreditHistTable"`,
		`<th>粒度</th>`,
		`每行是一个时间桶（小时或日）内的聚合扣减，不是逐请求账单`,
		`<details class="us-fold" id="usCreditAgg">`,
		`<ul class="main-nav">`,
		`.us-credit-hist .acc td::before { content: attr(data-label);`,
	} {
		if !bytes.Contains(b, []byte(needle)) {
			t.Errorf("index.html missing %q", needle)
		}
	}
}

// 客户端密钥表格必须真实存在，并且用动态行承载凭证：HTML 里不能写死密钥
// 输入框（否则配置回显/脱敏路径容易漏掉），操作列必须在最前面，窄屏横向
// 滚动时“删除/停用”仍然可达。
func TestPanelMultiKeyMarkup(t *testing.T) {
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, needle := range []string{
		`id="apiKeysBody"`,
		`id="btnAddAPIKey"`,
		`<th class="k-act">操作</th><th class="k-id">ID</th>`,
		`id="reqKey"`,
		`<th>Key</th>`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("index.html missing %q", needle)
		}
	}
	form := html[strings.Index(html, `<form id="cfgForm">`):]
	form = form[:strings.Index(form, "</form>")]
	if strings.Contains(form, `class="ki-key"`) {
		t.Error("api key inputs must be rendered dynamically, not embedded in config form markup")
	}
}

// 面板只允许同源脚本：index.html 不得出现内联事件处理器；所有 id 必须唯一，
// 否则 getElementById 会静默指向错误节点。
func TestPanelStrictCSPAndUniqueIDs(t *testing.T) {
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	inlineEvent := regexp.MustCompile(`(?i)\s(on(?:click|change|input|submit|load|error|keydown|keyup|mouseover|focus|blur))\s*=`)
	if m := inlineEvent.FindStringSubmatch(html); len(m) > 0 {
		t.Fatalf("index.html contains inline event handler %q; CSP script-src 'self' would block it", m[1])
	}
	ids := regexp.MustCompile(`\bid="([^"]+)"`).FindAllStringSubmatch(html, -1)
	seen := map[string]bool{}
	for _, m := range ids {
		if seen[m[1]] {
			t.Fatalf("duplicate id %q in index.html", m[1])
		}
		seen[m[1]] = true
	}
}

// 多 Key 表格的纯函数必须能被独立加载：掩码哨兵不能进入输入框，空密钥对
// 已有条目表示保留、对新增条目表示缺失。这里只验证脚本可加载，行为由
// 后端 roundtrip 与前端保存路径的集成测试覆盖。
func TestAppJSAPIKeyModelsFromSelect(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const API_KEY_REALMS');
const end = src.indexOf('async function loadConfig');
if (start < 0 || end < 0 || end < start) throw new Error('api key helpers not found');
const ctx = { Set, String, Array, Object, TextEncoder };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.collectAPIKeys=collectAPIKeys;this.validateAPIKeyRows=validateAPIKeyRows;this.apiKeyRows=apiKeyRows;', ctx);
process.stdout.write(JSON.stringify({ok: true}));`
	out := runNodeScript(t, "api-key-models", script, appJSPath(t))
	if out != `{"ok":true}` {
		t.Fatalf("api key helper load=%s", out)
	}
}

// TestAppJSUsageOffViewSkipsChildScans 主用量请求在途时用户切走：即使浏览器
// 不支持 AbortController，也不能再启动分位（扫归档）与流水（扫分片桶）。
func TestAppJSUsageOffViewSkipsChildScans(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadUsage');
const end = src.indexOf('/* ── 积分构成', start);
if (start < 0 || end < 0 || end < start) throw new Error('loadUsage region not found');
let resolveUsage;
let perfCalls = 0, creditCalls = 0, rendered = 0;
const ctx = {
  usageSeq: 0, usageCtrl: null, perfSeq: 10, creditHistSeq: 20,
  perfCtrl: null, creditCtrl: null,
  $: id => id === 'view-usage' ? { hidden: false } : null,
  esc: s => String(s), fmtTok: v => String(v),
  loadRemaining: () => {}, noteDOM: () => {}, noteUsageDomMismatch: () => {},
  view: 'usage',
  trangeQuery: () => ({toString: () => 'range=usage'}),
  warmUsageModelRates: () => {},
  api: () => new Promise(resolve => { resolveUsage = resolve; }),
  renderUsage: () => { rendered++; },
  loadRemaining: () => {},
  loadPerf: () => { perfCalls++; },
  loadCreditHistory: () => { creditCalls++; },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadUsage=loadUsage;', ctx);
(async () => {
  const p = ctx.loadUsage();
  ctx.view = 'accounts';
  resolveUsage({});
  await p;
  process.stdout.write(JSON.stringify({perfCalls, creditCalls, rendered}));
})();`
	out := runNodeScript(t, "usage-offview", script, appJSPath(t))
	const want = `{"perfCalls":0,"creditCalls":0,"rendered":0}`
	if out != want {
		t.Fatalf("off-view usage children=%s want %s", out, want)
	}
}

// TestAppJSRequestErrorEscaped 请求记录空态会把服务端错误文本写入
// innerHTML；任何底层/代理错误都必须先 esc，不能成为脚本注入点。
func TestAppJSRequestErrorEscaped(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderRequestTable');
const end = src.indexOf("/* 请求记录的 Key 下拉", start);
if (start < 0 || end < 0 || end < start) throw new Error('renderRequestTable not found');
let body = '';
const sinks = {reqBody: {set innerHTML(v) { body = v; }, get innerHTML() { return body; }}, reqCount: {textContent: '', className: ''}, reqKey: null};
const ctx = {
  $: id => sinks[id] || null,
  reqEntries: [], reqFilter: {}, reqPage: 1, reqPageSize: 50,
  lastLogErrors: {metrics: null, archive: new Error('<img src=x onerror=alert(1)>')},
  esc: s => String(s == null ? '' : s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
  reqMatch: () => true, reqKeyText: () => '', reqOutcomeTag: () => '', reqTokenCell: () => '—', reqCreditCell: () => '—',
  fmtLogTime: () => '', requestLogText: () => '', fmtMs: () => '—', syncRequestKeyOptions: () => {},
  Date, Number, String, Math, Array, Object,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderRequestTable=renderRequestTable;', ctx);
ctx.renderRequestTable();
process.stdout.write(JSON.stringify({escaped: body.includes('&lt;img'), raw: body.includes('<img src=x')}));
`
	out := runNodeScript(t, "request-error-escaped", script, appJSPath(t))
	const want = `{"escaped":true,"raw":false}`
	if out != want {
		t.Fatalf("request error escaping=%s want %s", out, want)
	}
}

// TestAppJSRequestCacheTracksWindowIdentity 归档窗口可能条数相同但内容换了
// （时间范围切换、归档轮转）；过滤缓存必须按 reqEntries 对象身份失效，
// 否则会继续显示上一窗口的旧行。
func TestAppJSRequestCacheTracksWindowIdentity(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderRequestTable');
const end = src.indexOf("/* 请求记录的 Key 下拉", start);
if (start < 0 || end < 0 || end < start) throw new Error('renderRequestTable not found');
let body = '';
const sinks = {
  reqBody: {set innerHTML(v) { body = v; }, get innerHTML() { return body; }},
  reqCount: {textContent: '', className: ''},
  reqPage: {textContent: ''}, reqKey: null,
};
const mk = () => ({disabled: false, value: ''});
const ctx = {
  $: id => sinks[id] || (sinks[id] = mk()),
  reqEntries: [], reqFilter: {}, reqPage: 1, reqPageSize: 50,
  lastLogErrors: {}, esc: s => String(s == null ? '' : s),
  reqMatch: e => e.keep, reqKeyText: () => '', reqOutcomeTag: () => '',
  reqTokenCell: () => '—', reqCreditCell: () => '—', fmtLogTime: () => '',
  requestLogText: e => e.request_id, fmtMs: () => '—', syncRequestKeyOptions: () => {},
  Date, Number, String, Math, Array, Object,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderRequestTable=renderRequestTable;', ctx);
ctx.reqEntries = [{request_id: 'old', keep: true}];
ctx.renderRequestTable();
ctx.reqEntries = [{request_id: 'new', keep: true}];
ctx.renderRequestTable();
process.stdout.write(JSON.stringify({new: body.includes('new'), old: body.includes('>old<')}));
`
	out := runNodeScript(t, "request-cache-window", script, appJSPath(t))
	const want = `{"new":true,"old":false}`
	if out != want {
		t.Fatalf("request cache window=%s want %s", out, want)
	}
}

// TestAppJSScanReattachFirstView 首次进入任务中心时，后端已有完整扫描结果
// （例如刷新页面/换浏览器）必须直接展示，而不是只剩「还没有扫描过」空态。
func TestAppJSScanReattachFirstView(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderScanResult');
const end = src.indexOf("$('btnScanAll').onclick", start);
if (start < 0 || end < 0 || end < start) throw new Error('scan reattach region not found');
const btn = { disabled: true, textContent: '扫描中…' };
const inert = new Proxy(function () {}, {get() { return inert; }, set() { return true; }, apply() { return inert; }});
let qcWrites = 0;
const qcList = { set innerHTML(v) { qcWrites++; }, get innerHTML() { return ''; } };
const empty = { style: {}, querySelector: () => ({textContent: ''}) };
const ctx = {
  view: 'taskscenter', scanTimer: null, scanPollSeq: 0, scanReqSeq: 0, lastScanRenderSeq: null,
  api: () => Promise.resolve({running: false, seq: 7, accounts: [{uid: 'u1', nickname: 'n1', growth: [{task_code: 't1', title: 'T', current: 0, target: 1}]}], pending_count: 1, error_count: 0}),
  $: id => id === 'btnScanAll' ? btn : id === 'qcList' ? qcList : id === 'tcEmpty' ? empty : inert,
  toast: () => {}, esc: s => String(s == null ? '' : s), GROWTH_TITLES: {},
  Promise, Array, Number, String, Object,
};
vm.createContext(ctx);
const groupStart = src.indexOf('function groupItems');
const groupEnd = src.indexOf('function groupsFromQueue', groupStart);
const queueStart = src.indexOf('function renderQueue');
const queueEnd = src.indexOf('// setQueueRunBusy', queueStart);
if (groupStart < 0 || groupEnd < 0 || queueStart < 0 || queueEnd < 0) throw new Error('scan render helpers not found');
vm.runInContext(src.slice(start, end) + src.slice(groupStart, groupEnd) + src.slice(queueStart, queueEnd) + '\nthis.reattachScanView=reattachScanView;', ctx);
(async () => {
  ctx.reattachScanView();
  for (let i = 0; i < 30; i++) await Promise.resolve();
  process.stdout.write(JSON.stringify({rendered: qcWrites > 0, button: !btn.disabled && btn.textContent === '扫描待办'}));
})();`
	out := runNodeScript(t, "scan-first-view", script, appJSPath(t))
	const want = `{"rendered":true,"button":true}`
	if out != want {
		t.Fatalf("first view reattach=%s want %s", out, want)
	}
}

// TestAppJSNeverScannedKeepsInitialEmptyState 后端从未扫描时返回 accounts=null；
// 这不能被渲染成“全部完成”，否则用户首次进入任务中心就误以为今日无待办。
func TestAppJSNeverScannedKeepsInitialEmptyState(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderScanResult');
const end = src.indexOf('// setScanProgress', start);
if (start < 0 || end < 0 || end < start) throw new Error('scan render region not found');
const empty = { style: {}, textContent: '', querySelector: () => ({ textContent: '' }) };
let writes = 0;
const list = { set innerHTML(v) { writes++; }, get innerHTML() { return ''; } };
const ctx = {
  $: id => id === 'tcEmpty' ? empty : id === 'qcList' ? list : null,
  renderQueue: (groups, progress, title, desc) => {
    if (title) throw new Error('never-scanned must not render a completion title: ' + title);
  },
  groupItems: () => { throw new Error('never-scanned must not group accounts'); },
  setText: () => {}, toast: () => {}, GROWTH_TITLES: {}, Array, Number, String, Object,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderScanResult=renderScanResult;', ctx);
ctx.renderScanResult({running: false, seq: 0, accounts: null, pending_count: 0, error_count: 0});
process.stdout.write(JSON.stringify({ok: true, writes}));`
	out := runNodeScript(t, "scan-never-ran", script, appJSPath(t))
	if out != `{"ok":true,"writes":0}` {
		t.Fatalf("never scanned render=%s", out)
	}
}

// 扫描进行中时当前账号/耗时/百分比必须常驻在进度条里，不能只藏在按钮 title；
// 触屏无法悬停读取 title，用户只能看到 done/total 会误以为卡住。
func TestAppJSScanProgressVisible(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function groupItems');
const end = src.indexOf("$('btnScanAll').onclick", start);
if (start < 0 || end < 0) throw new Error('scan region not found');
const btn = { disabled: false, textContent: '扫描待办', title: '' };
const bar = { style: {} };
const prog = { hidden: true };
const text = { textContent: '' };
let polls = 0;
const ctx = {
  view: 'taskscenter', scanTimer: null, scanPollSeq: 0, scanReqSeq: 0, lastScanRenderSeq: null,
  api: () => { polls++; return Promise.resolve(polls === 1
    ? {running: true, done: 23, total: 47, current_uid: 'u9', elapsed_sec: 41}
    : {running: false, done: 47, total: 47, pending_count: 0, error_count: 0, accounts: []}); },
  $: id => id === 'btnScanAll' ? btn : id === 'qBarFill' ? bar : id === 'qProg' ? prog : id === 'qProgText' ? text : null,
  toast: () => {}, setTimeout: fn => { globalThis.__tick = fn; return 1; }, clearTimeout() {},
  renderScanResult: () => {}, renderQueue: () => {}, Promise, Array, Number, String, Object,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.startScanPolling=startScanPolling;', ctx);
(async () => {
  ctx.startScanPolling();
  for (let i = 0; i < 10; i++) await Promise.resolve();
  const firstTick = globalThis.__tick;
  globalThis.__tick = null;
  firstTick();
  for (let i = 0; i < 10; i++) await Promise.resolve();
  const during = {
    visible: prog.hidden === false,
    width: bar.style.width,
    text: text.textContent,
    button: btn.textContent,
  };
  globalThis.__tick();
  for (let i = 0; i < 10; i++) await Promise.resolve();
  process.stdout.write(JSON.stringify({during, hiddenAfter: prog.hidden}));
})();`
	out := runNodeScript(t, "scan-progress", script, appJSPath(t))
	const want = `{"during":{"visible":true,"width":"48.9%","text":"扫描 23 / 47 · 当前 u9 · 已用 41 秒","button":"扫描中 23/47"},"hiddenAfter":true}`
	if out != want {
		t.Fatalf("scan progress=%s\nwant %s", out, want)
	}
}

// TestAppJSQueueFailureMessageVisible 预扫失败/无待办时队列以 running=false
// 结束，但 message 必须展示给用户，不能只弹「执行结束」的成功提示。
func TestAppJSQueueFailureMessageVisible(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; queue failure test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const btn = { disabled: false, textContent: '', onclick: null };
const inert = new Proxy(function () {}, {get() { return inert; }, set() { return true; }, apply() { return inert; }});
const toasts = [];
const live = new Map(); let nextTimer = 1;
const state = {
  location: {hash: '#taskscenter'}, history: {replaceState() {}},
  localStorage: {getItem: () => null, setItem() {}},
  navigator: {clipboard: {writeText: () => Promise.resolve()}},
  document: {querySelectorAll: () => [], querySelector: () => inert,
    getElementById: id => id === 'btnRunQueue' ? btn : inert,
    addEventListener() {}, documentElement: inert, head: inert, body: inert,
    createElement: () => ({style: {}, classList: {add() {}, remove() {}}, appendChild() {}, remove() {}}), cookie: ''},
  fetch: () => Promise.resolve({ok: true, status: 200, json: () => Promise.resolve({started: true, running: false, seq: 1, message: '预扫不完整：1 个账号任务查询失败，未启动队列（可重试）', items: []})}),
  addEventListener() {}, removeEventListener() {}, matchMedia: () => ({matches: false, addEventListener() {}}),
  setInterval: fn => {const id = nextTimer++; live.set(id, fn); return id;}, clearInterval: id => live.delete(id),
  setTimeout: fn => {const id = nextTimer++; return id;}, clearTimeout() {},
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
};
state.window = state; state.globalThis = state;
vm.createContext(state); vm.runInContext(src, state, {filename: 'app.js'});
state.toast = (m, c) => toasts.push([m, c]);
const flush = async () => {for (let i = 0; i < 20; i++) await Promise.resolve();};
(async () => {
  state.lastQueueSeq = 1;
  state.startQueuePolling();
  await flush();
  process.stdout.write(JSON.stringify({toasts: toasts.length, failure: toasts.some(t => String(t && t[0]).includes('预扫不完整')), busy: btn.disabled}));
})();`
	f, err := os.CreateTemp(t.TempDir(), "queuefail-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("queue failure node test failed: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != `{"toasts":1,"failure":true,"busy":false}` {
		t.Fatalf("queue failure state=%s", strings.TrimSpace(string(out)))
	}
}

// TestAppJSQueueReattachSingleFlight 5 秒刷新会在队列请求未返回时再次调用
// reattachQueueView；必须复用同一个在途查询，不能叠加 /tasks/queue。
func TestAppJSQueueReattachSingleFlight(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function reattachQueueView');
const end = src.indexOf('/* ── 用量', start);
if (start < 0 || end < 0) throw new Error('reattachQueueView not found');
let calls = 0;
const ctx = {
  queuePolling: false, queueTimer: null, queueReqSeq: 0, lastQueueSeq: 0,
  $: () => null,
  api: () => { calls++; return new Promise(() => {}); },
  startQueuePolling: () => {}, setQueueRunBusy: () => {}, reattachScanView: () => {},
  queueTimer: null,
  $: () => null,
  Promise,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.reattachQueueView=reattachQueueView;', ctx);
ctx.reattachQueueView();
ctx.reattachQueueView();
ctx.reattachQueueView();
process.stdout.write(JSON.stringify({calls}));
`
	out := runNodeScript(t, "queue-reattach-singleflight", script, appJSPath(t))
	if out != `{"calls":1}` {
		t.Fatalf("queue reattach calls=%s want 1", out)
	}
}

// TestAppJSLogsSingleFlight 日志页归档查询慢时，连点刷新/5 秒轮询不能叠加
// 多轮全量扫盘；首个请求完成后再允许下一轮。
func TestAppJSLogsSingleFlight(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadLogs');
const end = src.indexOf('function renderRequestMetrics', start);
if (start < 0 || end < 0) throw new Error('loadLogs not found');
let calls = 0, release;
const box = {scrollTop: 0, clientHeight: 0, scrollHeight: 0, innerHTML: ''};
const sink = () => ({textContent: '', className: '', value: '100', hidden: false});
const pending = new Promise(resolve => { release = resolve; });
const ctx = {
  logsReq: false, logSig: '', logPin: true, logCh: 'all',
  api: path => { if (String(path) === 'logs') calls++; return String(path) === 'logs' ? pending : Promise.resolve({archive: {enabled: false}, entries: []}); },
  $: id => id === 'logBox' ? box : sink(),
  setText: () => {}, noteDOM: () => {},
  trangeQuery: () => ({set() {}, toString: () => ''}),
  renderRequestMetrics: () => {}, lastLogMetrics: null, lastLogRows: {entries: []}, lastLogErrors: {},
  esc: s => String(s), Promise, Array, Object, String,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadLogs=loadLogs;', ctx);
(async () => {
  const first = ctx.loadLogs();
  ctx.loadLogs();
  ctx.loadLogs();
  const during = calls;
  release({entries: []});
  await first;
  await ctx.loadLogs();
  process.stdout.write(JSON.stringify({during, after: calls}));
})();`
	out := runNodeScript(t, "logs-singleflight", script, appJSPath(t))
	const want = `{"during":1,"after":2}`
	if out != want {
		t.Fatalf("logs singleflight=%s want %s", out, want)
	}
}

// TestAppJSHiddenTabSkipsPolling 后台标签页不应继续拉 /overview。旧实现只靠
// 5 秒定时器无差别刷新，用户切走后仍持续请求；visible 时必须立即补一拍。
func TestAppJSHiddenTabSkipsPolling(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function refreshVisible');
const end = src.indexOf('/* ── 积分任务', start);
if (start < 0 || end < 0) throw new Error('polling region not found');
  let overview = 0, logs = 0, queue = 0;
const ctx = {
  view: 'accounts',
  document: { visibilityState: 'visible', addEventListener() {} },
  loadOverview: () => { overview++; }, loadLogs: () => { logs++; }, reattachQueueView: () => { queue++; },
  setInterval: () => 0, clearInterval() {}, setTimeout: () => 0,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end).replace(/\nstart\(\);/, '') + '\nthis.refreshVisible=refreshVisible;', ctx);
ctx.refreshVisible();                    // visible → accounts 加载一次
ctx.document.visibilityState = 'hidden';
ctx.refreshVisible();                    // 隐藏 → 跳过
ctx.view = 'logs'; ctx.refreshVisible();
ctx.view = 'taskscenter'; ctx.refreshVisible();
ctx.document.visibilityState = 'visible';
ctx.view = 'accounts'; ctx.refreshVisible();  // 回到前台 → 立即补一拍
process.stdout.write(JSON.stringify({overview, logs, queue}));`
	out := runNodeScript(t, "hidden-tab-poll", script, appJSPath(t))
	const want = `{"overview":2,"logs":0,"queue":0}`
	if out != want {
		t.Fatalf("hidden tab polling=%s want %s", out, want)
	}
}

// TestAppJSLogsSkipIdenticalHTML 日志内容未变时不要整块替换 innerHTML；
// 滚动位置必须保持，避免 5 秒轮询把正在向上翻看的日志拉回底部。
func TestAppJSLogsSkipIdenticalHTML(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadLogs');
const end = src.indexOf('function renderRequestMetrics', start);
if (start < 0 || end < 0) throw new Error('loadLogs not found');
let writes = 0;
const box = {scrollTop: 10, clientHeight: 50, scrollHeight: 100, set innerHTML(v) { if (String(v).includes('same')) writes++; }, get innerHTML() { return ''; }};
const sink = () => ({textContent: '', className: '', value: '100', hidden: false});
const entry = e => { if (String(e) === 'logs') return Promise.resolve({entries: [{ts: '2026-01-01T00:00:00Z', ch: 'sys', text: 'same'}]}); return Promise.resolve({archive: {enabled: false}, entries: []}); };
const ctx = {
  logsReq: false, logSig: '', logPin: true, logCh: 'all',
  api: entry, $: id => id === 'logBox' ? box : sink(),
  setText: () => {}, noteDOM: () => {},
  trangeQuery: () => ({set() {}, toString: () => ''}),
  renderRequestMetrics: () => {}, lastLogMetrics: null, lastLogRows: {entries: []}, lastLogErrors: {},
  esc: s => String(s), fmtLogTime: s => String(s), document: { getElementById: () => null }, Promise, Array, Object, String, Number,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadLogs=loadLogs;', ctx);
(async () => {
  await ctx.loadLogs();
  const first = writes;
  box.scrollTop = 10;                 // 用户向上滚动，不在底部
  await ctx.loadLogs();               // 同一批日志
  process.stdout.write(JSON.stringify({first, total: writes, scroll: box.scrollTop}));
})();`
	out := runNodeScript(t, "logs-skip-identical", script, appJSPath(t))
	const want = `{"first":1,"total":1,"scroll":10}`
	if out != want {
		t.Fatalf("logs identical render=%s want %s", out, want)
	}
}

// TestAppJSLogsFilteredSummaryDoesNotThrow 日志页启用频道/级别/关键字筛选时，
// 汇总行曾引用请求指标区块的局部变量 entries，触发 ReferenceError 并让整页
// 日志停摆。筛选后必须只使用当前日志窗口内可见的计数。
func TestAppJSLogsFilteredSummaryDoesNotThrow(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadLogs');
const end = src.indexOf('function renderRequestMetrics', start);
if (start < 0 || end < 0) throw new Error('loadLogs not found');
const box = {scrollTop: 0, clientHeight: 0, scrollHeight: 0, innerHTML: ''};
let note = '';
const sink = id => {
  if (id === 'logNote') return {set textContent(v) { note = v; }, get textContent() { return note; }};
  return {textContent: '', className: '', value: '100', hidden: false};
};
const entries = [
  {ts: '2026-01-01T00:00:00Z', ch: 'task', text: 'one'},
  {ts: '2026-01-01T00:00:01Z', ch: 'task', text: 'two'},
  {ts: '2026-01-01T00:00:02Z', ch: 'sys', text: 'three'},
];
const api = path => {
  const p = String(path);
  if (p === 'logs') return Promise.resolve({entries});
  if (p === 'request_metrics') return Promise.resolve({archive: {enabled: false}, recent: []});
  return Promise.resolve({entries: []});
};
const ctx = {
  logsReq: false, logSig: '', logPin: true, logCh: 'task', logLevel: '', logQ: '',
  api, $: id => id === 'logBox' ? box : sink(id),
  setText: (id, v) => { if (id === 'logNote') note = v; }, noteDOM: () => {},
  trangeQuery: () => ({set() {}, toString: () => ''}),
  renderRequestMetrics: () => {}, lastLogMetrics: null, lastLogRows: {entries: []}, lastLogErrors: {},
  logRenderHTML: (all, ch) => ({html: '', count: all.filter(e => e.ch === ch).length}),
  esc: s => String(s), fmtLogTime: s => String(s), Promise, Array, Object, String, Number,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadLogs=loadLogs;', ctx);
(async () => {
  let err = '';
  try { await ctx.loadLogs(); } catch (e) { err = String(e && e.message); }
  process.stdout.write(JSON.stringify({err, note}));
})();`
	out := runNodeScript(t, "logs-filtered-summary", script, appJSPath(t))
	const want = `{"err":"","note":"2 行 / 共 3 行"}`
	if out != want {
		t.Fatalf("logs filtered summary=%s want %s", out, want)
	}
}

// TestAppJSBulkActionRestoresButtonOnError 批量启停失败后必须解除 disabled；
// 否则一次网络错误会让工具栏按钮在整页生命周期内都无法再次点击。
func TestAppJSBulkActionRestoresButtonOnError(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function accSelectedAction');
const end = src.indexOf("if ($('btnSelEnable'))", start);
if (start < 0 || end < 0) throw new Error('accSelectedAction not found');
const btn = {textContent: '禁用', disabled: false};
const ctx = {
  accSelected: new Set(['u1']), toast: () => {}, confirm: () => true,
  api: () => Promise.reject(new Error('network down')),
  $: id => id === 'btnSelDisable' ? btn : null,
  invalidatePackagesCache: () => {}, loadOverview: () => {},
  Promise, Array, Set, Object, String, Error,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.run=accSelectedAction;', ctx);
(async () => {
  await ctx.run('disable');
  process.stdout.write(JSON.stringify({disabled: btn.disabled, label: btn.textContent}));
})();`
	out := runNodeScript(t, "bulk-action-button-restore", script, appJSPath(t))
	const want = `{"disabled":false,"label":"禁用"}`
	if out != want {
		t.Fatalf("bulk action button=%s want %s", out, want)
	}
}

// TestAppJSConfigLoadSingleFlight 首次进入配置页、手动重载与保存后重载可能
// 在同一轮里同时触发；配置读取应共享一个在途请求，结束后允许再次刷新。
func TestAppJSConfigLoadSingleFlight(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadConfig');
const end = src.indexOf("if ($('btnAddAPIKey'))", start);
if (start < 0 || end < 0) throw new Error('loadConfig not found');
let calls = 0, release;
const form = { elements: {}, querySelectorAll: () => [], querySelector: () => null };
const sink = () => ({ textContent: '', className: '', value: '', checked: false, type: '',
  classList: { toggle() {} }, setAttribute() {}, removeAttribute() {}, focus() {}, closest: () => null });
const ctx = {
  cfgReq: null, cfgLoaded: null, CFG_MAP: {},
  api: () => { calls++; return new Promise(resolve => { release = () => resolve({config: {}, path: 'config.json'}); }); },
  $: id => id === 'cfgForm' ? form : sink(),
  renderAPIKeys: () => {}, markDurationFields: () => {}, markHoursFields: () => {},
  toast() {}, Object, Promise, Array,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadConfig=loadConfig;', ctx);
(async () => {
  const a = ctx.loadConfig();
  const b = ctx.loadConfig();
  await new Promise(r => setImmediate(r));
  const during = calls;
  release();
  await Promise.all([a, b]);
  const again = ctx.loadConfig();
  await new Promise(r => setImmediate(r));
  release();
  await again;
  process.stdout.write(JSON.stringify({during, after: calls}));
})();`
	out := runNodeScript(t, "config-singleflight", script, appJSPath(t))
	const want = `{"during":1,"after":2}`
	if out != want {
		t.Fatalf("config singleflight=%s want %s", out, want)
	}
}

// TestAppJSActivitySameQuerySingleFlight 活动页搜索防抖/切页可能与上一次
// 查询重叠；相同查询必须合并，但不能因为一次请求在途就丢弃后续不同筛选。
func TestAppJSActivitySameQuerySingleFlight(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadActivity');
const end = src.indexOf("if ($('btnActivityReload'))", start);
if (start < 0 || end < 0) throw new Error('loadActivity not found');
let calls = 0, release;
const ctx = {
  actSeq: 0, actCtrl: null, actReq: null, view: 'activity',
  api: () => { calls++; return new Promise(resolve => { release = () => resolve({records: [], total: 0}); }); },
  trangeQuery: () => new URLSearchParams('limit=200'),
  $: () => ({value: ''}), renderActivity: () => {}, esc: s => String(s),
  AbortController: function () { this.abort = () => {}; },
  Promise, Array, Object, String,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadActivity=loadActivity;', ctx);
(async () => {
  const a = ctx.loadActivity();
  const b = ctx.loadActivity();
  await new Promise(r => setImmediate(r));
  const during = calls;
  release();
  await Promise.all([a, b]);
  process.stdout.write(JSON.stringify({during}));
})();`
	out := runNodeScript(t, "activity-singleflight", script, appJSPath(t))
	if out != `{"during":1}` {
		t.Fatalf("activity singleflight=%s want 1 call", out)
	}
}

// TestAppJSActivityUIDFilter 活动历史支持按脱敏 uid8 精确筛选；前端输入必须
// 进入 uid 查询参数，且与搜索框共用防抖，避免每次按键都打归档。
func TestAppJSActivityUIDFilter(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadActivity');
const end = src.indexOf("if ($('btnActivityReload'))", start);
if (start < 0 || end < 0) throw new Error('loadActivity not found');
let seen = '';
const values = {actUID: 'abc12345', actQ: ''};
const ctx = {
  actSeq: 0, actCtrl: null, actReq: null, view: 'activity', actLimit: 200, actRows: [], actQTimer: null,
  api: path => { seen = path; return Promise.resolve({records: [], total: 0}); },
  trangeQuery: () => new URLSearchParams('limit=200'),
  $: id => ({value: values[id] == null ? '' : values[id]}),
  renderActivity: () => {},
  AbortController: function () { this.abort = () => {}; },
  Promise, Array, Object, String, URLSearchParams,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadActivity=loadActivity;', ctx);
(async () => {
  await ctx.loadActivity();
  process.stdout.write(JSON.stringify({path: seen}));
})();`
	out := runNodeScript(t, "activity-uid-filter", script, appJSPath(t))
	if !strings.Contains(out, `"path":"activity/history?limit=200&uid=abc12345"`) {
		t.Fatalf("activity uid filter=%s", out)
	}
}

// 剩余额度表格：账号/模型两维、限额 0 显示「未启用」、无快照显示「未知」、
// 达限行高亮；所有动态文本必须先经 esc。
func TestAppJSRemainingRenderingAndFilter(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const REM_DIM_TABS');
const end = src.indexOf('async function loadRemaining', start);
if (start < 0 || end < 0) throw new Error('remaining render region not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', title: '', hidden: false });
const ctx = {
  remDim: 'account', remQ: '',
  esc: s => String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;'),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => sinks[id] || mk(id),
  Array, Math, Number, String, Object,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderRemaining=renderRemaining;this.remFilteredRows=remFilteredRows;', ctx);
const data = {
  day: '2026-10-18', data_available: true,
  limits: {account: 100, model: 50, account_enabled: true, model_enabled: true},
  accounts: [
    {uid: 'u1', nickname: '<b>甲</b>', realm: 'cn', used: 100, limit: 100, limit_enabled: true, remaining: 0, limited: true, model_limited: true},
    {uid: 'u2', nickname: '乙', realm: 'global', used: 5, limit: 100, limit_enabled: true, remaining: 95, limited: false},
    {uid: 'u3', nickname: '未启用', realm: 'cn', used: 7, limit: 0, limit_enabled: false, remaining: null, limited: false},
    {uid: 'u4', nickname: '无快照', realm: '', used: null, limit: 100, limit_enabled: true, remaining: null, limited: false},
  ],
  models: [
    {model: 'glm-5.3', uid: 'u1', nickname: '甲', realm: 'cn', used: 80, limit: 50, limit_enabled: true, remaining: 0, limited: true},
    {model: 'kimi-k3', uid: 'u2', nickname: '乙', realm: 'global', used: 20, limit: 50, limit_enabled: true, remaining: 30, limited: false},
  ],
  summary: {accounts: 4, limited_accounts: 1, limited_models: 1},
};
ctx.renderRemaining(data);
const acctHtml = sinks.remBody.innerHTML;
const acct = {
  escaped: acctHtml.includes('&lt;b&gt;甲&lt;/b&gt;') && !acctHtml.includes('<b>甲</b>'),
  limitedFirst: acctHtml.indexOf('u1') < acctHtml.indexOf('u2'),
  hitRow: /<tr class="hit">[\s\S]*?u1/.test(acctHtml),
  disabled: acctHtml.includes('未启用'),
  unknown: acctHtml.includes('用量未知'),
  summary: sinks.remSummary.innerHTML.includes('账号达限'),
  tabs: sinks.remTabs.innerHTML.includes('data-rem-dim="model"'),
};
ctx.remDim = 'model';
ctx.renderRemaining(data);
const modelHtml = sinks.remBody.innerHTML;
const model = {
  header: sinks.remHead.innerHTML.includes('模型 / 账号'),
  rows: (modelHtml.match(/<tr/g) || []).length,
  limited: /<tr class="hit">[\s\S]*?glm-5.3/.test(modelHtml),
  filtered: ctx.remFilteredRows(data, 'model', 'kimi global').length === 1,
};
ctx.remDim = 'account';
ctx.renderRemaining({data_available: false, reason: '<offline>', accounts: [], models: [], limits: {}, summary: {}});
const err = {
  text: sinks.remEmpty.textContent.includes('剩余额度不可用') && sinks.remEmpty.textContent.includes('<offline>'),
  hiddenTable: sinks.remTable.hidden === true,
  summary: sinks.remSummary.innerHTML.includes('用量快照不可用'),
};
process.stdout.write(JSON.stringify({acct, model, err}));`
	out := runNodeScript(t, "remaining-render", script, appJSPath(t))
	const want = `{"acct":{"escaped":true,"limitedFirst":true,"hitRow":true,"disabled":true,"unknown":true,"summary":true,"tabs":true},"model":{"header":true,"rows":2,"limited":true,"filtered":true},"err":{"text":true,"hiddenTable":true,"summary":true}}`
	if out != want {
		t.Fatalf("remaining render=%s\nwant %s", out, want)
	}
}

// 接入页复制载荷必须来自渲染后的文本节点，且不含任何真实密钥；接口即使返回
// 了带密钥的示例（回归模拟），复制按钮也只能逐字复制该文本——服务端已保证
// 示例只有占位符，本测试同时钉住占位符与复制内容。
func TestAppJSConnectCopyNeverLeaksSecrets(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function connectCopyButtons');
const end = src.indexOf('async function loadConnect', start);
if (start < 0 || end < 0) throw new Error('connect render region not found');
const sinks = {};
const copied = [];
const buttons = [];
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', title: '', hidden: false });
const root = { querySelectorAll: () => buttons };
const ctx = {
  cxSeq: 0,
  esc: s => String(s == null ? '' : s),
  $: id => sinks[id] || mk(id),
  copyText: text => { copied.push(text); return Promise.resolve(); },
  toast: () => {},
  setTimeout: () => 0,
  document: { getElementById: id => id === 'view-connect' ? root : (sinks[id] || mk(id)) },
  Array, String, Object, Promise,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderConnect=renderConnect;this.connectCopyButtons=connectCopyButtons;', ctx);
for (const id of ['cxV1','cxBase','cxAnthropic','cxOpenAI','cxCurl','cxAnthropicEnv','cxAnthropicMessages','cxResponses']) {
  const b = { dataset: { copyEl: id }, textContent: '复制', onclick: null };
  buttons.push(b);
}
ctx.renderConnect({
  v1_url: 'https://gw.example/v1', base_url: 'https://gw.example', anthropic_url: 'https://gw.example',
  auth_required: true, panel_auth_required: true,
  models: [{id: 'cn:glm-5.3'}, {id: 'global:kimi-k3'}],
  models_source: 'observed',
  examples: {
    openai_sdk: 'api_key="$WB_API_KEY"', curl: 'Bearer $WB_API_KEY',
    anthropic_env: 'ANTHROPIC_AUTH_TOKEN="$WB_API_KEY"',
    anthropic_messages: 'curl https://gw.example/v1/messages -H "x-api-key: $WB_API_KEY"',
    responses_api: 'Bearer $WB_API_KEY',
  },
  notes: ['示例不含真实密钥'],
});
(async () => {
  await buttons[1].onclick();
  await buttons[5].onclick();
  const all = copied.join('\n');
  process.stdout.write(JSON.stringify({
    copied: copied.length,
    placeholder: all.includes('$WB_API_KEY'),
    noSecret: !all.includes('admin-super-secret') && !all.includes('client-secret'),
    note: sinks.cxNote.textContent.includes('客户端需要 API Key'),
    models: sinks.cxModels.value.includes('cn:glm-5.3') && sinks.cxModels.value.includes('global:kimi-k3'),
    messages: sinks.cxAnthropicMessages.textContent.includes('/v1/messages'),
  }));
})();`
	out := runNodeScript(t, "connect-copy", script, appJSPath(t))
	const want = `{"copied":2,"placeholder":true,"noSecret":true,"note":true,"models":true,"messages":true}`
	if out != want {
		t.Fatalf("connect copy=%s\nwant %s", out, want)
	}
}

// 升级后的旧 index.html 可能没有用量页节点：loadUsage 的入口与失败分支都必须
// 静默退化，不能对 null 写 innerHTML（线上 app.js:2706 的未处理异常）。
// 这里用最小 DOM 桩覆盖“api 拒绝”的最坏路径，断言不抛异常且只提示一次版本错配。
func TestAppJSLoadUsageMissingDOMDoesNotThrow(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function domVersionTip()');
const end = src.indexOf('/* ── 活动记录', start);
if (start < 0 || end < 0) throw new Error('usage region not found');
let tip = '', toasts = 0, aborts = 0;
const ctx = {
  $: id => null,
  document: { querySelector: () => null, createElement: () => ({ setAttribute(){}, className:'' }) },
  toast: () => { toasts++; },
  api: async () => { throw new Error('boom'); },
  usageSeq: 0, usageCtrl: null, perfSeq: 0, creditHistSeq: 0,
  perfCtrl: null, creditCtrl: null, creditGrantSeq: 0, creditGrantCtrl: null,
  view: 'usage', usRange: null, usDim: 'account', usSort: 'requests', usDimPage: 1,
  usDimPageSize: 20, usCreditDim: 'account', usCreditKind: 'spend',
  usQTimer: null, usageData: null, US_DIMS: { account: { key: 'by_account', span: 7 } },
  setHTML(id, html) { tip += '|set:' + id; return false; },
  setText() { return false; },
  noteDOM(id) { tip += '|dom:' + id; },
  noteUsageDomMismatch(id) { tip += '|usage:' + id; },
  isAbortError: () => false,
  trangeQuery: () => ({ toString: () => 'range=7d' }),
  trangeLabel: () => '近 7 天',
  esc: s => String(s == null ? '' : s), Array, String, Number, Math, Object, Promise,
};
vm.createContext(ctx);
const exposed = '\nthis.loadUsage=loadUsage;this.noteDOM=noteDOM;this.noteUsageDomMismatch=noteUsageDomMismatch;this.renderUsage=renderUsage;';
vm.runInContext(src.slice(start, end) + exposed, ctx);
(async () => {
  // 无 #view-usage/#usChart/#usError/#usNote：入口只需提示一次并返回，不发请求。
  await ctx.loadUsage();
  ctx.$ = () => null;
  ctx.document.querySelector = () => null;
  let threw = false;
  try { ctx.renderUsage({ totals: { requests: 1 } }); } catch (e) { threw = true; }
  process.stdout.write(JSON.stringify({ tip: ctx.noteDOM.shown === true, threw, toasts }));
})().catch(e => { console.error(e && e.stack || e); process.exit(1); });`
	out := runNodeScript(t, "usage-missing-dom", script, appJSPath(t))
	if out != `{"tip":true,"threw":false,"toasts":0}` {
		t.Fatalf("loadUsage missing dom=%s", out)
	}
}

// 页内导航只对长页生成；短页隐藏；标签优先取 h3，点击跳转不改全局视图。
func TestAppJSSectionNavSwitch(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf("let secNavView = '', secNavTimer = null;");
const end = src.indexOf('/* ── 接入页', start);
if (start < 0 || end < 0) throw new Error('section nav region not found');
const mkEl = (id, label, top) => ({
  id, hidden: false,
  querySelector: sel => sel === 'header h3' ? { textContent: label } : null,
  getBoundingClientRect: () => ({ top }),
});
const boxes = [mkEl('b1', ' 第一块 ', 10), mkEl('b2', '第二块', 210), mkEl('', '第三块', 410)];
const links = [];
const nav = {
  hidden: true, innerHTML: '',
  querySelectorAll: sel => sel === 'a[data-sec]' ? links : [],
};
let scrolled = null;
const ctx = {
  esc: s => String(s == null ? '' : s),
  $: id => id === 'secNav' ? nav : null,
  document: {
    getElementById: id => id === 'view-usage' ? { querySelectorAll: () => boxes } : (id === 'b1' ? boxes[0] : id === 'b2' ? boxes[1] : null),
  },
  window: { scrollY: 0, pageYOffset: 0, innerWidth: 1200, scrollTo: o => { scrolled = o.top; } },
  history: { replaceState() {} },
  secNavView: '', secNavTimer: null, secNavFrame: 0, secNavActive: '',
  addEventListener: () => {}, removeEventListener: () => {},
  requestAnimationFrame: fn => { fn(); return 1; },
  SECNAV_LABELS: {}, Array, String, Math,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.setupSectionNav=setupSectionNav;', ctx);
ctx.setupSectionNav('usage');
const long = {
  visible: nav.hidden === false,
  labels: nav.innerHTML.includes('第一块') && nav.innerHTML.includes('第二块'),
  generatedId: boxes[2].id === 'sec-usage-2',
  links: nav.innerHTML.includes('data-sec="b1"'),
};
const usageDoc = ctx.document;
ctx.document = { getElementById: () => null };
ctx.setupSectionNav('config');
const short = nav.hidden === true;
const hiddenBoxes = [mkEl('b1', 'A', 10)];
const oldDoc = ctx.document;
ctx.document = { getElementById: id => id === 'view-config' ? { querySelectorAll: () => hiddenBoxes } : null };
ctx.setupSectionNav('config');
const oneSection = nav.hidden === true;
ctx.document = usageDoc;
ctx.setupSectionNav('usage');
links.length = 0;
for (const id of ['b1', 'b2']) {
  const a = {
    dataset: { sec: id }, _on: false, _cur: '',
    classList: { toggle(_, on) { a._on = on; } },
    setAttribute(k, v) { if (k === 'aria-current') this._cur = v; },
    removeAttribute(k) { if (k === 'aria-current') this._cur = ''; },
  };
  links.push(a);
}

ctx.setupSectionNav('usage');
let prevented = false;
if (typeof links[1].onclick !== 'function') throw new Error(JSON.stringify({hidden: nav.hidden, html: nav.innerHTML, len: links.length}));
links[1].onclick({ preventDefault() { prevented = true; } });
// scrollTo is captured before the deferred scroll-spy run replaces the state.
const scrolledOnClick = scrolled;
ctx.requestAnimationFrame(() => {});
const click = {
  prevented,
  scrolled: scrolledOnClick,
  on: links[1]._on && !links[0]._on,
  aria: links[1]._cur === 'location' && links[0]._cur === '',
};
process.stdout.write(JSON.stringify({long, short, oneSection, click}));`
	out := runNodeScript(t, "section-nav", script, appJSPath(t))
	const want = `{"long":{"visible":true,"labels":true,"generatedId":true,"links":true},"short":true,"oneSection":true,"click":{"prevented":true,"scrolled":88,"on":true,"aria":true}}`
	if out != want {
		t.Fatalf("section nav=%s\nwant %s", out, want)
	}
}

// 配置页查找：关键词按空格 AND 匹配字段文本，命中项加高亮并滚动到第一个；
// 清空后不残留高亮。配置页很长，这个入口是替代「一直往下滚」的主要路径。
func TestAppJSConfigFind(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function cfgFindNode');
const end = src.indexOf('if ($(\'btnEye\'))', start);
if (start < 0 || end < 0) throw new Error('config find region not found');
const mk = text => ({
  textContent: text, classes: new Set(),
  classList: { add(c) { this.__n = true; }, remove(c) { this.__n = false; } },
  scrollIntoView() { this.__scrolled = true; },
});
const nodes = [mk('监听地址 改动需重启'), mk('短请求超时 120 秒'), mk('积分保底')];
const input = { value: '', listeners: {}, addEventListener(k, fn) { this.listeners[k] = fn; } };
const note = { textContent: '' };
const ctx = {
  $: id => id === 'cfgFind' ? input : id === 'cfgFindNote' ? note : id === 'cfgForm' ? { querySelectorAll: () => nodes, addEventListener() {} } : null,
  Array, String, Math,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end), ctx);
input.value = '超时'; input.listeners.input();
const one = { note: note.textContent, hit: nodes[1].classList.__n === true, scrolled: nodes[1].__scrolled === true, others: !!nodes[0].classList.__n };
input.value = '超时 120'; input.listeners.input();
const and = nodes[1].classList.__n === true && note.textContent.includes('1');
input.value = ''; input.listeners.input();
const cleared = nodes.every(n => n.classList.__n !== true) && note.textContent === '';
process.stdout.write(JSON.stringify({ one, and, cleared }));`
	out := runNodeScript(t, "config-find", script, appJSPath(t))
	const want = `{"one":{"note":"命中 1 项","hit":true,"scrolled":true,"others":false},"and":true,"cleared":true}`
	if out != want {
		t.Fatalf("config find=%s want %s", out, want)
	}
}

// 接入/剩余额度必须有稳定的视图与占位节点；CSP 仍是 script-src 'self'，
// 不允许任何内联脚本。导航项 data-view 与 view id 必须一一对应。
func TestPanelConnectAndRemainingMarkup(t *testing.T) {
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, needle := range []string{
		`<aside class="page-sidebar" id="pageSidebar" hidden>`,
		`<nav class="secnav" id="secNav" aria-label="页内区块导航" hidden></nav>`,
		`<a href="#connect" data-view="connect">`,
		`<section class="view" id="view-connect" hidden>`,
		`id="cxV1"`, `id="cxBase"`, `id="cxAnthropic"`, `id="cxAnthropicEnv"`,
		`id="cxAnthropicMessages"`, `id="cxResponses"`, `id="cxModels"`,
		`data-copy-el="cxV1"`, `data-copy-el="cxCurl"`, `data-copy-el="cxResponses"`,
		`id="remBox"`, `id="remTabs"`, `id="remQ"`, `id="remSummary"`,
		`id="remEmpty"`, `id="remTable"`, `id="remBody"`,
		`.page-sidebar { position: fixed;`,
		`@media (max-width: 860px)`,
		`限额为 0 表示该维度未启用`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("index.html missing %q", needle)
		}
	}
	for _, view := range []string{"accounts", "taskscenter", "models", "activity", "packages", "usage", "connect", "config", "logs"} {
		if !strings.Contains(html, `id="view-`+view+`"`) {
			t.Errorf("missing view %s", view)
		}
		if !strings.Contains(html, `data-view="`+view+`"`) {
			t.Errorf("missing nav item %s", view)
		}
	}
	// index.html 是 go:embed 源文件，版本查询串由 index.go 在服务端注入；
	// 源文件必须保持无内联脚本的外链形态，否则 CSP 与 ETag 用例会失效。
	if !strings.Contains(html, `<script src="app.js"></script>`) {
		// CSP 实际在 index.go；这里只防止误把脚本改成内联。
		t.Error("panel must load app.js as an external same-origin script")
	}
}

// 剩余额度请求：单飞、旧世代丢弃、离页不渲染、失败态显式。
func TestAppJSRemainingRequestGuards(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadRemaining');
const end = src.indexOf("if ($('remTabs'))", start);
if (start < 0 || end < 0) throw new Error('loadRemaining not found');
let calls = 0, releases = [], rejectNext = null;
const renders = [];
const ctx = {
  remReq: false, remSeq: 0, remCtrl: null, remData: null,
  view: 'usage',
  api: () => { calls++; return new Promise((resolve, reject) => releases.push({resolve, reject})); },
  renderRemaining: d => renders.push(d),
  AbortController: class { constructor() { this.signal = {}; } abort() {} },
  Promise, JSON, String,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadRemaining=loadRemaining;', ctx);
(async () => {
  const p1 = ctx.loadRemaining();
  const p2 = ctx.loadRemaining();
  const single = calls === 1;
  releases[0].resolve({data_available: true, accounts: [{uid: 'u1'}]});
  await Promise.all([p1, p2]);
  const after = renders.length === 1 && renders[0].accounts.length === 1;

  const off = ctx.loadRemaining();
  ctx.view = 'accounts';
  releases[1].resolve({data_available: true, accounts: [{uid: 'u2'}]});
  await off;
  const offView = renders.length === 1;

  ctx.view = 'usage';
  const old = ctx.loadRemaining();
  const newer = ctx.loadRemaining(); // remReq 仍为 true，第二次被单飞拦下
  releases[2].resolve({data_available: true, accounts: [{uid: 'old'}]});
  await Promise.all([old, newer]);
  const last = renders[renders.length - 1];
  const latestOnly = last.accounts[0].uid === 'old';

  ctx.remReq = false;
  const fail = ctx.loadRemaining();
  releases[3].reject(Object.assign(new Error('boom'), {name: 'Error'}));
  await fail;
  const err = renders[renders.length - 1].data_available === false && renders[renders.length - 1].reason === 'boom';
  process.stdout.write(JSON.stringify({calls, single, after, offView, latestOnly, err}));
})();`
	out := runNodeScript(t, "remaining-guards", script, appJSPath(t))
	const want = `{"calls":4,"single":true,"after":true,"offView":true,"latestOnly":true,"err":true}`
	if out != want {
		t.Fatalf("remaining guards=%s\nwant %s", out, want)
	}
}

// 导航接入页：切到 connect 必须触发 /panel/api/connect；切到其它页不重复触发
// 且不出现内联脚本/事件处理器（CSP 回归由 TestPanelStrictCSPAndUniqueIDs 兜底）。
func TestAppJSConnectViewSwitch(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function go(v)');
const end = src.indexOf("/* 首次进入延到本轮脚本求值之后再 go()", start);
if (start < 0 || end < 0) throw new Error('go() not found');
let connectCalls = 0, usageCalls = 0, setup = [];
const ctx = {
  TITLES: { accounts: '账号池', usage: '用量与积分', connect: '接入' },
  view: 'accounts',
  $: id => id === 'navSub' || id === 'ttl' ? { textContent: '' } : null,
  document: {
    querySelectorAll: sel => sel === '.view' ? [] : sel === '.nav a' ? [] : [],
    getElementById: () => null,
  },
  stopScanPolling: () => {}, setupSectionNav: v => setup.push(v),
  loadConnect: () => { connectCalls++; }, loadUsage: () => { usageCalls++; },
  history: { replaceState() {} }, location: { hash: '#accounts' },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.go=go;', ctx);
ctx.go('connect');
ctx.go('connect');
ctx.go('usage');
process.stdout.write(JSON.stringify({connectCalls, usageCalls, setup}));`
	out := runNodeScript(t, "connect-view", script, appJSPath(t))
	const want = `{"connectCalls":2,"usageCalls":1,"setup":["connect","connect","usage"]}`
	if out != want {
		t.Fatalf("connect view switch=%s\nwant %s", out, want)
	}
}

// overview 条件请求：首轮 200 缓存 ETag，次轮必须带 If-None-Match；命中 304
// 时不能把账号表重渲染成空，也不能丢弃上一份 overviewData。
func TestAppJSOverviewConditionalRequest(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadOverview');
const end = src.indexOf('/* checkAuthGate', start);
if (start < 0 || end < 0) throw new Error('overview region not found');
const els = {};
const sink = id => (els[id] || (els[id] = { textContent: '', className: '', innerHTML: '', title: '',
  classList: { toggle() {} }, setAttribute(k, v) { this[k] = v; } }));
let requests = [], renders = 0;
const first = { total: 1, healthy: 1, cooling: 0, disabled: 0, paused: 0, accounts: [{ uid: 'u1' }], model_locks: [], sticky_sessions: 0, version: 't', redis_mode: 'noop', uptime_sec: 120 };
const responses = [
  { status: 200, ok: true, headers: { get: n => n === 'ETag' ? '"v1"' : '' }, json: () => Promise.resolve(first) },
  { status: 304, ok: false, headers: { get: () => '' }, json: () => Promise.reject(new Error('304 must not parse body')) },
];
const ctx = {
  Date, Number, Math, String, JSON, Promise,
  $: id => sink(id),
  setText: (id, v) => { sink(id).textContent = v; },
  authHeaders: () => ({ Authorization: 'Bearer k' }),
  openKey: () => { throw new Error('unexpected 401'); },
  toast: () => {},
  renderAccounts: rows => { renders++; ctx.rendered = rows.length; },
  renderModelLocks: () => {},
  renderExpirySnapshot: () => {},
  renderUptime: () => {},
  noteDOM: () => {},
  overviewData: null, overviewETag: '', overviewFetchedAt: 0, lastUptimeSec: 0,
  overviewInFlight: false, overviewPending: false, overviewSeq: 0,
  fetch: (url, opts) => {
    requests.push({ url: String(url), etag: (opts.headers || {})['If-None-Match'] || '' });
    return Promise.resolve(responses.shift());
  },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadOverview=loadOverview;', ctx);
(async () => {
  await ctx.loadOverview(true);
  await ctx.loadOverview(true);
  process.stdout.write(JSON.stringify({ requests, renders, rows: ctx.rendered, data: ctx.overviewData && ctx.overviewData.total }));
})();`
	out := runNodeScript(t, "overview-conditional", script, appJSPath(t))
	want := "{\"requests\":[{\"url\":\"/panel/api/overview\",\"etag\":\"\"},{\"url\":\"/panel/api/overview\",\"etag\":\"\\\"v1\\\"\"}],\"renders\":1,\"rows\":1,\"data\":1}"
	if out != want {
		t.Fatalf("overview conditional=%s want %s", out, want)
	}
}

// TestAppJSLogsSkipUnchangedRequestTable 归档窗口未变（unchanged=true）时，
// loadLogs 不应重建请求表；数据变化时必须重建。避免 5 秒轮询对最多 1000 行
// 做无意义 innerHTML 重绘并丢滚动位置。
func TestAppJSLogsSkipUnchangedRequestTable(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadLogs');
const end = src.indexOf('function renderRequestMetrics', start);
if (start < 0 || end < 0) throw new Error('loadLogs not found');
const box = {scrollTop: 0, clientHeight: 0, scrollHeight: 0, innerHTML: ''};
const sink = () => ({textContent: '', className: '', value: '100', hidden: false});
const metrics = {archive: {enabled: true}, recent: []};
const calls = [];
const archiveRounds = [];
let archiveCall = 0;
const ctx = {
  logsReq: false, logSig: '', logPin: true, logCh: 'all',
  api: path => {
    const p = String(path);
    if (p !== 'request_logs?limit=100') return Promise.resolve(p === 'logs' ? {entries: []} : metrics);
    const round = archiveCall++;
    return new Promise(resolve => { archiveRounds.push(resolve); });
  },
  $: id => id === 'logBox' ? box : sink(),
  setText: () => {}, noteDOM: () => {},
  trangeQuery: () => ({set() {}, toString: () => 'limit=100'}),
  renderRequestMetrics: (m, entries, rowsChanged) => { if (rowsChanged !== undefined) calls.push(rowsChanged); },
  lastLogMetrics: null, get lastLogRows() { return ctx.lastRows || {entries: []}; }, set lastLogRows(v) { ctx.lastRows = v; }, lastLogErrors: {},
  esc: s => String(s), fmtLogTime: s => String(s), Promise, Array, Object, String, Number,
};
vm.createContext(ctx);
ctx.reqEntries = [];
ctx.renderRequestMetrics = (m, entries, rowsChanged) => {
  ctx.reqEntries = entries || [];
  if (rowsChanged !== undefined) calls.push(rowsChanged);
};
vm.runInContext(src.slice(start, end) + '\nthis.loadLogs=loadLogs;', ctx);
(async () => {
  const first = ctx.loadLogs();
  await new Promise(r => setImmediate(r));
  // 首轮有数据；第二轮 unchanged=true 且服务端省略 entries，表格必须保留
  // 上一轮的行，不能被空数组覆盖。
  archiveRounds[0]({entries: [{request_id: 'r1'}], unchanged: false});
  await first;
  const second = ctx.loadLogs();
  await new Promise(r => setImmediate(r));
  archiveRounds[1]({entries: [], unchanged: true});
  await second;
  const third = ctx.loadLogs();
  await new Promise(r => setImmediate(r));
  archiveRounds[2]({entries: [], unchanged: true});
  await third;
  process.stdout.write(JSON.stringify({flags: calls, rows: ctx.reqEntries.length}));
})();`
	out := runNodeScript(t, "logs-skip-unchanged", script, appJSPath(t))
	const want = `{"flags":[true,false,false],"rows":1}`
	if out != want {
		t.Fatalf("logs unchanged result=%s want %s", out, want)
	}
}

// 账号页状态筛选/版本筛选与行密度：筛选必须真的收窄列表，紧凑开关必须落到
// 表格 class 与 aria-pressed 上。三者都只影响展示，不改变 /overview 请求。
func TestAppJSAccountFiltersAndDensity(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function accSearchText');
const end = src.indexOf('/* accountRowSig', start);
if (start < 0 || end < 0 || end < start) throw new Error('account filters not found');
const ctx = { Date, Math, Number, String, Array, Object };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.accMatch=accMatch;', ctx);
const now = new Date().toISOString();
const soon = new Date(Date.now() + 60 * 60 * 1000).toISOString();
const accounts = [
  { uid: 'u1', nickname: '可用', realm: 'cn', enterprise: false },
  { uid: 'u2', nickname: '冷却', realm: 'global', cooling: true, cool_remaining_sec: 300 },
  { uid: 'u3', nickname: '暂停', realm: 'cn', paused: true },
  { uid: 'u4', nickname: '禁用', realm: 'global', disabled: true },
  { uid: 'u5', nickname: '熔断', realm: 'cn', breaker_until: soon },
  { uid: 'u6', nickname: '企业', realm: 'global', enterprise: true },
];
const pick = f => accounts.filter(a => ctx.accMatch(a, { q: '', sort: 'default', state: '', realm: '', ...f })).map(a => a.uid);
const out = {
  healthy: pick({ state: 'healthy' }),
  cooling: pick({ state: 'cooling' }),
  paused: pick({ state: 'paused' }),
  disabled: pick({ state: 'disabled' }),
  global: pick({ realm: 'global' }),
  enterprise: pick({ realm: 'enterprise' }),
  combined: pick({ state: 'healthy', realm: 'global' }),
};
process.stdout.write(JSON.stringify(out));`
	out := runNodeScript(t, "account-filters", script, appJSPath(t))
	const want = `{"healthy":["u1","u6"],"cooling":["u2","u5"],"paused":["u3"],"disabled":["u4"],"global":["u2","u4","u6"],"enterprise":["u6"],"combined":["u6"]}`
	if out != want {
		t.Fatalf("account filters=%s want %s", out, want)
	}
}

// TestAppJSQueuePreScanProgressVisible 队列预扫阶段 items 为空，但必须显示
// pre_done/pre_total 进度与进行中文案，不能落进“没有待办”的空态。
func TestAppJSQueuePreScanProgressVisible(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {get() { return inert; }, set() { return true; }, apply() { return inert; }});
const out = {};
const el = id => (out[id] = out[id] || {hidden: true, textContent: '', style: {}, innerHTML: ''});
const empty = {style: {}};
const ctx = {
  esc: s => String(s == null ? '' : s),
  $: id => id === 'tcEmpty' ? empty : el(id),
  view: 'taskscenter', lastQueueSeq: 0, queuePollSeq: 1, queueReqInFlight: false,
  queueTimer: null, setQueueRunBusy: () => {},
  api: () => Promise.resolve({running: true, phase: 'scan', pre_done: 3, pre_total: 10, items: []}),
  setInterval: () => 1, clearInterval: () => {},
  Promise, Array, Number, String, Object, Math, JSON,
};
vm.createContext(ctx);
const start = src.indexOf('function updateProgress');
const end = src.indexOf('function groupsFromQueue', start);
const rq = src.indexOf('function renderQueue');
const rqe = src.indexOf('// setQueueRunBusy', rq);
if (start < 0 || end < 0 || rq < 0 || rqe < 0) throw new Error('queue progress helpers not found');
vm.runInContext(src.slice(rq, rqe) + src.slice(start, end) + '\nthis.updateProgress=updateProgress;', ctx);
ctx.updateProgress({phase: 'scan', pre_done: 3, pre_total: 10, items: []});
process.stdout.write(JSON.stringify({hidden: el('qProg').hidden, width: el('qBarFill').style.width, text: el('qProgText').textContent}));`
	out := runNodeScript(t, "queue-prescan", script, appJSPath(t))
	if !strings.Contains(out, `"hidden":false`) || !strings.Contains(out, `"width":"30%"`) || !strings.Contains(out, "正在扫描账号 3 / 10") {
		t.Fatalf("pre-scan progress=%s", out)
	}
}

// TestAppJSQueuePreScanWarnVisible 部分账号预扫失败但队列仍启动时，进度文案
// 必须持续带出 pre_warn，不能只显示“执行中”让人以为全部账号都扫到了。
func TestAppJSQueuePreScanWarnVisible(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const out = {};
const el = id => (out[id] = out[id] || {hidden: true, textContent: '', style: {}, innerHTML: '', dataset: {}});
const empty = {style: {}};
const ctx = {
  esc: s => String(s == null ? '' : s),
  $: id => id === 'tcEmpty' ? empty : el(id),
  Promise, Array, Number, String, Object, Math, JSON,
};
vm.createContext(ctx);
const start = src.indexOf('function updateProgress');
const end = src.indexOf('function groupsFromQueue', start);
const rq = src.indexOf('function renderQueue');
const rqe = src.indexOf('// setQueueRunBusy', rq);
if (start < 0 || end < 0 || rq < 0 || rqe < 0) throw new Error('queue helpers not found');
vm.runInContext(src.slice(rq, rqe) + src.slice(start, end) + '\nthis.updateProgress=updateProgress;', ctx);
ctx.updateProgress({running: true, pre_warn: '2 个账号任务查询失败，本轮可能漏扫；可稍后重试', items: [{status: 'done'}, {status: 'pending'}]});
process.stdout.write(JSON.stringify({text: el('qProgText').textContent, warn: el('qProgText').dataset.warn}));`
	out := runNodeScript(t, "queue-prescan-warn", script, appJSPath(t))
	if !strings.Contains(out, `"warn":"1"`) || !strings.Contains(out, "2 个账号任务查询失败") || !strings.Contains(out, "执行中 1 / 2") {
		t.Fatalf("pre-scan warn=%s", out)
	}
}

// TestAppJSVisibilityStopsTaskPolling 隐藏页必须停掉队列/扫描定时器；回页
// 若队列仍是本页发起的，则立即接回，不能等下一个 3 秒节拍。
func TestAppJSVisibilityStopsTaskPolling(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let hiddenPaused = false;');
const end = src.indexOf('document.addEventListener(\'visibilitychange\'', start);
if (start < 0 || end < 0) throw new Error('visibility handler not found');
const calls = [];
const ctx = {
  document: {visibilityState: 'visible'},
  loginTimer: null, loginState: null,
  queueTimer: 7, scanTimer: 9,
  refreshVisible: () => calls.push('refresh'),
  stopQueuePolling: v => { calls.push('stopQueue:' + String(v)); ctx.queueTimer = null; },
  stopScanPolling: () => { calls.push('stopScan'); ctx.scanTimer = null; },
  startQueuePolling: () => calls.push('startQueue'),
  setInterval: () => 11, clearInterval: () => {},
  view: 'taskscenter', lastQueueSeq: 3,
  document_addEventListener: () => {},
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.onVisibilityChange=onVisibilityChange;', ctx);
ctx.document.visibilityState = 'hidden';
ctx.onVisibilityChange();
ctx.document.visibilityState = 'visible';
ctx.onVisibilityChange();
process.stdout.write(JSON.stringify({calls, hiddenPaused: ctx.hiddenPaused, scanTimer: ctx.scanTimer}));`
	out := runNodeScript(t, "visibility-polling", script, appJSPath(t))
	var got struct {
		Calls []string `json:"calls"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("visibility output=%s err=%v", out, err)
	}
	want := []string{"stopQueue:true", "stopScan", "refresh", "startQueue"}
	if strings.Join(got.Calls, ",") != strings.Join(want, ",") {
		t.Fatalf("visibility calls=%v want %v", got.Calls, want)
	}
}

// TestAppJSSetAllSendsExplicitUIDs 批量启停必须把当前快照里的 UID 显式发给
// accounts/set_all；后端已拒绝空 uids（避免隐式整池操作），旧前端只发
// {enabled} 会让按钮必然 400。
func TestAppJSSetAllSendsExplicitUIDs(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function bindSetAll');
const end = src.indexOf("bindSetAll('btnAllDisable'", start);
if (start < 0 || end < 0) throw new Error('bindSetAll not found');
let body = null;
const el = {disabled: false, textContent: '全部启用', onclick: null};
const ctx = {
  overviewData: {accounts: [{uid: 'u1'}, {uid: 'u2'}]},
  $: () => el,
  confirm: () => true,
  api: (path, opt) => { body = JSON.parse(opt.body); return Promise.resolve({changed: 2}); },
  toast: () => {}, invalidatePackagesCache: () => {}, loadOverview: () => {},
  JSON, Promise,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.bindSetAll=bindSetAll;', ctx);
ctx.bindSetAll('btnAllEnable', true, 'confirm');
el.onclick();
process.stdout.write(JSON.stringify(body));`
	out := runNodeScript(t, "set-all-uids", script, appJSPath(t))
	if out != `{"enabled":true,"uids":["u1","u2"]}` {
		t.Fatalf("set_all body=%s want explicit uids", out)
	}
}

// TestIndexMobileNavBeforeMain 375px 下导航必须在正文之前，否则首屏无法切页。
func TestIndexMobileNavBeforeMain(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	headerAt := strings.Index(html, `<header class="topbar"`)
	navAt := strings.Index(html, `<nav class="nav"`)
	mainAt := strings.Index(html, `<div class="shell">`)
	if headerAt < 0 || navAt < 0 || mainAt < 0 {
		t.Fatalf("missing topbar/nav/main skeleton: header=%d nav=%d main=%d", headerAt, navAt, mainAt)
	}
	if !(headerAt < navAt && navAt < mainAt) {
		t.Fatal("navigation must live in the topbar before the main shell")
	}
	if !strings.Contains(html, `.topbar > .nav { order:3;`) {
		t.Fatal("mobile topbar must give the nav its own full-width row")
	}
}

// TestIndexResponsiveHeaderContract 钉住 Hub 风格的响应式契约：中间宽度导航
// 独占一行并可横向滑动，避免 9 个入口的 min-content 把页面撑出横向滚动；
// 超窄屏把“添加账号”收成图标，积分卡保留完整值而不是省略号。
func TestIndexResponsiveHeaderContract(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, want := range []string{
		`@media (max-width: 1180px)`,
		`.topbar > .nav { order:3; flex:1 1 100%; width:100%; min-width:0; max-width:100%; margin:0; overflow:hidden; }`,
		`@media (max-width: 700px)`,
		`@media (max-width: 430px)`,
		`#btnAdd::before`,
		`.page-sidebar { position: relative; top: auto;`,
		`display:flex; flex-direction:row; flex-wrap:nowrap; align-items:center; gap:6px;`,
		`.box > header h3 { white-space: normal; min-width: 0; overflow-wrap: anywhere; }`,
		`.box > header h3 .hint { white-space: normal; }`,
		`.tbl-wrap { overflow-x: auto; max-width: 100%; min-width: 0; }`,
		`#sCredits { white-space:normal;`,
		`id="sCredits" title=`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("responsive header contract missing %q", want)
		}
	}
	// 431–700px 区间也要把“添加账号”收成图标，否则品牌行会被文字按钮挤满。
	phoneAt := strings.Index(html, `@media (max-width: 700px)`)
	addAt := strings.Index(html, `#btnAdd { width:34px; height:34px;`)
	narrowAt := strings.Index(html, `@media (max-width: 430px)`)
	if phoneAt < 0 || addAt < phoneAt || addAt > narrowAt || narrowAt < 0 {
		t.Errorf("btnAdd must collapse during the 700px phone breakpoint, not only at 430px")
	}
}

// TestAppJSOpenTasksMissingSkeleton 旧 index.html 没有任务弹窗时，点“任务”
// 不能继续 loadTasks 后对空表格写 innerHTML；应只提示页面版本不一致。
func TestAppJSOpenTasksMissingSkeleton(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let taskUID = null;');
const end = src.indexOf('// 账号设置弹窗', start);
if (start < 0 || end < start) throw new Error('openTasks region not found');
let domTips = 0, loaded = 0;
const ctx = {
  $: () => null,
  setText: () => false,
  showModal: () => { throw new Error('must not open a missing modal'); },
  hideModal: () => {},
  noteDOM: id => { domTips++; ctx.missing = id; },
  loadTasks: () => { loaded++; },
  console,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.openTasks = openTasks;', ctx);
ctx.openTasks('u1', {enterprise: false});
process.stdout.write(JSON.stringify({tips: domTips, loaded, missing: ctx.missing || ''}));`
	out := runNodeScript(t, "open-tasks-missing", script, appJSPath(t))
	if out != `{"tips":1,"loaded":0,"missing":"taskVeil"}` {
		t.Fatalf("openTasks with missing skeleton=%s", out)
	}
}

// TestAppJSUsageOptionalBlocksMissing 线上报错 "Cannot set properties of null
// (setting 'innerHTML') at loadUsage" 的直接回归：升级后 index.html 已经不再
// 渲染旧版用量页的部分区块（usStats / usDimHead / usCreditStats 等），但同版本
// 的 app.js 仍按固定契约无条件写入。这里用「所有用量 DOM 都不存在」的最坏桩跑
// 完整 loadUsage，要求它既不抛 TypeError，也能把失败状态落在仍存在的 #usChart。
func TestAppJSUsageOptionalBlocksMissing(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function setHTML');
const loadStart = src.indexOf('async function loadUsage');
const end = src.indexOf('/* ── 活动记录', loadStart);
if (start < 0 || loadStart < 0 || end < loadStart) throw new Error('usage region not found');
const writes = {};
const els = {};
const mk = id => (els[id] || (els[id] = {innerHTML: '', textContent: '', hidden: false}));
const $ = id => {
  // Reproduce the upgraded page with a global error-returning /usage and no
  // legacy usage blocks at all, except the chart the current page still has.
  if (id === 'usChart') return mk(id);
  return null;
};
const ctx = {
  usageSeq: 0, usageCtrl: null, perfSeq: 0, creditHistSeq: 0,
  perfCtrl: null, creditCtrl: null, creditGrantCtrl: null,
  view: 'usage', usCreditKind: 'spend', usDim: 'account', usCreditDim: 'account',
  $, esc: s => String(s == null ? '' : s),
  trangeQuery: () => ({toString: () => 'range=usage'}),
  api: () => Promise.reject(new Error('usage unavailable')),
  renderUsage: () => { throw new Error('renderUsage must not run on failure'); },
  loadRemaining: () => {}, loadPerf: () => {}, loadCreditHistory: () => {},
  warmUsageModelRates: () => {}, AbortController: undefined,
  US_DIM_TABS: [['account', '按账号']],
  usTabsHtml: () => '',
};
vm.createContext(ctx);
const abortStart = src.indexOf('function isAbortError');
const abortEnd = src.indexOf('function toast', abortStart);
if (abortStart < 0 || abortEnd < abortStart) throw new Error('isAbortError not found');
vm.runInContext(src.slice(abortStart, abortEnd) + src.slice(start, src.indexOf('const US_DIM_TABS')) + src.slice(loadStart, end) +
  '\nthis.loadUsage = loadUsage;', ctx);
(async () => {
  let threw = '';
  try { await ctx.loadUsage(); } catch (e) { threw = String(e && e.message); }
  process.stdout.write(JSON.stringify({threw, chart: els.usChart ? els.usChart.innerHTML : ''}));
})();`
	out := runNodeScript(t, "usage-missing-blocks", script, appJSPath(t))
	const want = `{"threw":"","chart":"<div class=\"us-empty\">读取用量失败：usage unavailable</div>"}`
	if out != want {
		t.Fatalf("usage with missing blocks=%s\nwant %s", out, want)
	}
}

// TestAppJSUsageButtonWithoutBlocks 线上更常见的错配不是“缺图表”，而是旧页面
// 仍给了入口按钮、但用量区块已经被新版脚本改名/移除。此时 loadUsage 必须：
//  1. 被调用时不抛 TypeError（按钮仍可点）；
//  2. 不因缺少 usChart/usError 而静默丢失错误；
//  3. 落一条“页面版本与脚本不一致”的可操作提示。
func TestAppJSUsageButtonWithoutBlocks(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function setHTML');
const loadStart = src.indexOf('async function loadUsage');
const end = src.indexOf('/* ── 活动记录', loadStart);
if (start < 0 || loadStart < 0 || end < loadStart) throw new Error('usage region not found');
const writes = [];
const tipHost = { parentNode: { insertBefore: (el) => writes.push({cls: el.className, text: el.textContent}) }, textContent: '' };
const els = {viewusage: tipHost};
const $ = id => {
  if (id === 'view-usage') return tipHost;
  return null;
};
const ctx = {
  usageSeq: 0, usageCtrl: null, perfSeq: 0, creditHistSeq: 0,
  perfCtrl: null, creditCtrl: null, creditGrantCtrl: null,
  view: 'usage', usCreditKind: 'spend', usDim: 'account', usCreditDim: 'account',
  $, esc: s => String(s == null ? '' : s),
  trangeQuery: () => ({toString: () => 'range=usage'}),
  api: () => Promise.reject(new Error('usage unavailable')),
  renderUsage: () => { throw new Error('renderUsage must not run on failure'); },
  loadRemaining: () => {}, loadPerf: () => {}, loadCreditHistory: () => {}, loadCreditGrants: () => {},
  warmUsageModelRates: () => {}, AbortController: undefined,
  document: {createElement: () => ({className: '', textContent: '', setAttribute(k,v){this[k]=v;}})},
  US_DIM_TABS: [['account', '按账号']],
  usTabsHtml: () => '',
};
vm.createContext(ctx);
const abortStart = src.indexOf('function isAbortError');
const abortEnd = src.indexOf('function toast', abortStart);
if (abortStart < 0 || abortEnd < abortStart) throw new Error('isAbortError not found');
vm.runInContext(src.slice(abortStart, abortEnd) + src.slice(start, src.indexOf('const US_DIM_TABS')) + src.slice(loadStart, end) +
  '\nthis.loadUsage = loadUsage;', ctx);
(async () => {
  let threw = '';
  try { await ctx.loadUsage(); } catch (e) { threw = String(e && e.message); }
  process.stdout.write(JSON.stringify({threw, writes}));
})();`
	out := runNodeScript(t, "usage-button-missing-blocks", script, appJSPath(t))
	if !strings.Contains(out, `"threw":""`) {
		t.Fatalf("loadUsage must not throw when optional blocks are missing: %s", out)
	}
	if !strings.Contains(out, "页面版本与脚本不一致") {
		t.Fatalf("missing DOM must surface a version-mismatch hint, got %s", out)
	}
}

// TestAppJSMissingDOMNoticeSurvivesOptionalBlock 旧骨架常同时缺多个节点：
// 入口（view-usage）先提示一次版本错配，随后可选块探测不能把这条可操作
// 提示覆盖成第二条同义告警，也不能让它变成“只剩第一条可选块”。此前
// noteDOM.shown 是全局一次标记，后到的关键缺块会被永久吞掉。
func TestAppJSMissingDOMNoticeSurvivesOptionalBlock(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function domVersionTip()');
const end = src.indexOf('const US_DIM_TABS', start);
if (start < 0 || end < 0) throw new Error('dom guard region not found');
const inserted = [];
const setAttr = (el, k, v) => { el[k] = v; };
const view = {
  id: 'view-usage', hidden: false,
  prepend(el) { inserted.push(String(el.textContent)); },
};
const ctx = {
  $: id => id === 'view-usage' ? view : null,
  toast: msg => inserted.push('toast:' + msg),
  document: {
    querySelector: sel => sel === '.view:not([hidden])' ? view : null,
    createElement: () => ({ className: '', textContent: '', setAttribute(k, v) { setAttr(this, k, v); } }),
  },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.noteDOM=noteDOM;this.noteUsageDomMismatch=noteUsageDomMismatch;', ctx);
// 真实入口先由 loadUsage 的 noteDOM 报版本错配；随后用量页可选块探测
// 不能再把这条提示覆盖掉或吞掉。
ctx.noteDOM('view-usage');
ctx.noteUsageDomMismatch('usNote');
ctx.noteDOM('accBody');
process.stdout.write(JSON.stringify({inserted, shown: ctx.noteDOM.shown === true}));`
	out := runNodeScript(t, "dom-notice-multiple", script, appJSPath(t))
	var got struct {
		Inserted []string `json:"inserted"`
		Shown    bool     `json:"shown"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad output: %v\n%s", err, out)
	}
	if !got.Shown {
		t.Fatalf("noteDOM.shown must be set: %s", out)
	}
	joined := strings.Join(got.Inserted, "\n")
	if !strings.Contains(joined, "view-usage") || len(got.Inserted) != 1 {
		t.Fatalf("entry notice must survive optional-block probe, got %s", out)
	}
}

// TestAppJSUsageAbortDoesNotClearTables 快速切时间范围时旧请求会被取消；取消
// 是“结果过期”，不是“读取失败”。若把 AbortError 当失败，loadUsage 会先清空
// 刚渲染的新表，再写一条失败横幅——用户看到的就是“刷新把数据刷没了”。
func TestAppJSUsageAbortDoesNotClearTables(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function setHTML');
const loadStart = src.indexOf('async function loadUsage');
const end = src.indexOf('/* ── 活动记录', loadStart);
if (start < 0 || loadStart < 0 || end < loadStart) throw new Error('usage region not found');
const writes = [];
const els = {};
const mk = id => (els[id] || (els[id] = {id, innerHTML: 'keep', textContent: 'keep', hidden: false,
  set innerHTML(v) { this._html = v; writes.push([id, 'innerHTML', v]); }, get innerHTML() { return this._html || 'keep'; }}));
['usError','usChart','usStats','usDimTabs','usDimHead','usDimBody','usCreditTabs','usCreditHead','usCreditBody','usCreditStats','usNote','usCreditNote','usDimNote','usChartNote'].forEach(mk);
const ctx = {
  usageSeq: 0, usageCtrl: null, perfSeq: 0, creditHistSeq: 0,
  perfCtrl: null, creditCtrl: null, creditGrantCtrl: null,
  view: 'usage', usCreditKind: 'spend', usDim: 'account', usCreditDim: 'account',
  $: id => ['usError','usChart','usStats','usDimTabs','usDimHead','usDimBody','usCreditTabs','usCreditHead','usCreditBody','usCreditStats','usNote','usCreditNote','usDimNote','usChartNote'].includes(id) ? mk(id) : null,
  esc: s => String(s == null ? '' : s),
  trangeQuery: () => ({toString: () => 'range=usage'}),
  api: () => Promise.reject(Object.assign(new Error('aborted'), {name: 'AbortError'})),
  renderUsage: () => { throw new Error('renderUsage must not run on abort'); },
  loadRemaining: () => {}, loadPerf: () => {}, loadCreditHistory: () => {},
  warmUsageModelRates: () => {}, AbortController: undefined,
  US_DIM_TABS: [['account', '按账号']],
  usTabsHtml: () => '<button>tab</button>',
  usageData: {totals: {requests: 1}},
};
vm.createContext(ctx);
const abortStart = src.indexOf('function isAbortError');
const abortEnd = src.indexOf('function toast', abortStart);
if (abortStart < 0 || abortEnd < abortStart) throw new Error('isAbortError not found');
vm.runInContext(src.slice(abortStart, abortEnd) + src.slice(start, src.indexOf('const US_DIM_TABS')) + src.slice(loadStart, end) +
  '\nthis.loadUsage = loadUsage;', ctx);
(async () => {
  let threw = '';
  try { await ctx.loadUsage(); } catch (e) { threw = String(e && e.message); }
  process.stdout.write(JSON.stringify({threw, writes, kept: els.usDimBody.innerHTML}));
})();`
	out := runNodeScript(t, "usage-abort-no-clear", script, appJSPath(t))
	if !strings.Contains(out, `"threw":""`) {
		t.Fatalf("aborted usage request must not throw: %s", out)
	}
	if !strings.Contains(out, `"kept":"keep"`) || !strings.Contains(out, `"writes":[]`) {
		t.Fatalf("aborted usage request must not clear rendered tables: %s", out)
	}
}

// TestIndexHasHubCapsuleNav 导航可以换外观，但必须仍是 .nav a[data-view]；
// 移动端导航顺序与「nav 在前」的红线也由同一条测试守住。
func TestIndexHasHubCapsuleNav(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, needle := range []string{
		`<header class="topbar"`,
		`class="header-left"`,
		`class="header-right"`,
		`<ul class="main-nav">`,
		`.main-nav { display:inline-flex`,
		`.main-nav a.on, .main-nav a[aria-current="page"] { background:var(--surface)`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("index.html missing Hub nav rule %q", needle)
		}
	}
	for _, view := range []string{"accounts", "taskscenter", "models", "activity", "packages", "usage", "connect", "config", "logs"} {
		if !strings.Contains(html, `data-view="`+view+`"`) {
			t.Errorf("missing nav item %s", view)
		}
	}
}

// TestIndexHasHubViewHeads 每个主视图都应有 Hub 风格标题 + 说明；
// 只换色板而正文直接进入表格，会让长页面失去定位。
func TestIndexHasHubViewHeads(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, view := range []string{"accounts", "taskscenter", "activity", "models", "packages", "usage", "connect", "config", "logs"} {
		section := `<section class="view" id="view-` + view + `"`
		i := strings.Index(html, section)
		if i < 0 {
			t.Errorf("missing view %s", view)
			continue
		}
		next := strings.Index(html[i+len(section):], `<section class="view"`)
		chunk := html[i:]
		if next >= 0 {
			chunk = html[i : i+len(section)+next]
		}
		if !strings.Contains(chunk, `class="view-head"`) || !strings.Contains(chunk, `<h2>`) {
			t.Errorf("view %s missing Hub-style view-head h2", view)
		}
	}
}

// TestAppJSUsagePartialDomDoesNotThrow 升级时最常见的错配是“旧骨架还留着部分
// 用量节点”：入口存在、图表/统计/明细/流水其中一块缺失。loadUsage 成功后
// 会一路调用 renderUsage → renderUsageDim/renderCreditDim/renderUsageChart，
// 再并发 loadPerf/loadCreditHistory/loadCreditGrants/loadRemaining；任何一处
// 的 `$('id').innerHTML` 都不能把整条链炸成未处理 Promise 拒绝。
//
// 这里用最小 DOM 桩遍历四种缺块形态，断言：
//  1. loadUsage 不抛；
//  2. 缺失块只提示一次可操作版本信息；
//  3. 其余仍存在的块照常渲染，不因一个可选块缺失而整页空白。
func TestAppJSUsagePartialDomDoesNotThrow(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function perfPct');
const end = src.indexOf('/* ── 活动记录', start);
if (start < 0 || end < 0) throw new Error('usage render region not found');
const cases = [
  {name: 'all-missing', drop: ['view-usage','usChart','usError','usNote','usStats','usDimHead','usDimBody','usCreditHead','usCreditBody','usCreditStats','usCreditTabs']},
  {name: 'chart-missing', drop: ['usChart'], keepView: true},
  {name: 'dim-head-missing', drop: ['usDimHead'], keepView: true},
  {name: 'dim-body-missing', drop: ['usDimBody'], keepView: true},
  {name: 'credit-head-missing', drop: ['usCreditHead'], keepView: true},
  {name: 'credit-body-missing', drop: ['usCreditBody'], keepView: true},
  {name: 'credit-stats-missing', drop: ['usCreditStats'], keepView: true},
  {name: 'failure-chart-missing', drop: ['usChart','usError'], keepView: true, fail: true},
];
function runCase(c) {
  const drop = new Set(c.drop);
  const els = {};
  const mk = id => (els[id] || (els[id] = {id, innerHTML: '', textContent: '', title: '', hidden: false, disabled: false, value: id === 'usDimSize' ? '20' : '',
    getAttribute(){ return null; }, attrs: {},
    addEventListener(){}, setAttribute(k,v){this.attrs[k]=v;}, removeAttribute(k){delete this.attrs[k];}}));
  const $ = id => drop.has(id) ? null : mk(id);
  const tips = [];
  const ctx = {
    usageSeq: 0, usageCtrl: null, perfSeq: 0, creditHistSeq: 0,
    perfCtrl: null, creditCtrl: null, creditGrantSeq: 0, creditGrantCtrl: null,
    view: 'usage', usCreditKind: 'spend', usDim: 'account', usCreditDim: 'account',
    usQTimer: null, usageData: null, US_DIMS: {account: {key:'by_account', title:'账号', withRealm:true, withPerf:true, span:10}},
    usDimPage: 1, usDimPageSize: 20, usSort: 'total',
    $,
    document: {
      querySelector: sel => sel === '#view-usage' && !drop.has('view-usage') ? mk('view-usage') : null,
      querySelectorAll: () => [],
      createElement: () => ({className:'', textContent:'', setAttribute(){}}),
    },
    esc: s => String(s == null ? '' : s),
    fmtTok: v => String(v == null ? 0 : v),
    fmtMs: v => String(v == null ? 0 : v),
    fmtRate: v => String(v == null ? 0 : v),
    fmtCredit: v => String(v == null ? 0 : v),
    fmtCreditRatio: () => '—',
    fmtModelRate: () => '—',
    cacheRateCell: () => '—',
    cacheRateText: () => '—',
    usPct: () => '—',
    usKpi: (v,k) => '<div>' + k + ':' + v + '</div>',
    usMixBar: () => '',
    usRow: () => '<tr><td>row</td></tr>',
    usTabsHtml: () => '<button>tab</button>',
    usDimHead: () => '<tr><th>head</th></tr>',
    usCreditHead: () => '<tr><th>head</th></tr>',
    noteUsageDomMismatch(id) { tips.push('usage:' + id); },
    perfPct: () => ({p50:'—',p90:'—',p99:'—',max:'—'}),
    trangeLabel: () => '近 7 天',
    trangeQuery: () => ({toString: () => 'hours=72'}),
    trangeBind: () => {},
    perfQueryFromUsage: () => ({toString: () => 'hours=72'}),
    setHTML(id, html) { const el = $(id); if (!el) { tips.push(id); return false; } el.innerHTML = html; return true; },
    setText(id, text) { const el = $(id); if (!el) { tips.push(id); return false; } el.textContent = text; return true; },
    noteDOM(id) { tips.push('dom:' + id); },
    // 真实实现由 noteDOM.shown 收敛成一条提示；测试桩不能覆盖 stub 的
    // setHTML 缺块探测，否则“只缺一块”会被误判为静默。
    isAbortError: () => false,
    api: async path => c.fail ? Promise.reject(new Error('usage unavailable')) :
      (path.indexOf('usage?') === 0 ? {totals:{requests:2,total_tokens:10,prompt_tokens:6,completion_tokens:4}, series:[{t:'2026-10-19T10',scope:'hour',total_tokens:10,prompt_tokens:6,completion_tokens:4,requests:2}], by_account:[{key:'u1',realm:'cn',requests:2,total_tokens:10,prompt_tokens:6,completion_tokens:4,errors:0}], credit_by_account:[], credit_by_model:[]} : {}),
    loadRemaining: async () => {}, loadPerf: async () => {}, loadCreditHistory: async () => {}, loadCreditGrants: async () => {},
    warmUsageModelRates: () => {}, AbortController: undefined,
    Array, String, Number, Math, Object, Promise,
  };
  vm.createContext(ctx);
  vm.runInContext(src.slice(start, end) + '\nthis.loadUsage=loadUsage;', ctx);
  // 编译切片的函数声明会覆盖 ctx 上同名占位，故在脚本求值后再挂观测钩子。
  // 这里只验证“是否给出提示”，不复制真实 noteUsageDomMismatch 的去重逻辑。
  const origNote = ctx.noteUsageDomMismatch;
  ctx.noteUsageDomMismatch = id => { tips.push('usage:' + id); origNote(id); };
  return ctx.loadUsage().then(() => ({
    threw: false,
    tips: tips.slice(0, 3),
    allTips: tips.slice(),
    view: !!els['view-usage'],
    chart: els['usChart'] ? String(els['usChart'].innerHTML).length > 0 : false,
    stats: els['usStats'] ? String(els['usStats'].innerHTML).length > 0 : false,
  }), e => ({threw: String(e && e.message)}));
}
(async () => {
  const out = [];
  for (const c of cases) out.push(Object.assign({name:c.name}, await runCase(c)));
  process.stdout.write(JSON.stringify(out));
})().catch(e => { console.error(e && e.stack || e); process.exit(1); });`
	out := runNodeScript(t, "usage-partial-dom", script, appJSPath(t))
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad output: %v\n%s", err, out)
	}
	if len(got) != 8 {
		t.Fatalf("cases=%d, want 8: %s", len(got), out)
	}
	for _, c := range got {
		if c["threw"] != nil && c["threw"] != false {
			t.Errorf("%s threw %v", c["name"], c["threw"])
		}
	}
	if got[0]["tips"] == nil {
		t.Errorf("全部缺块时必须给出可操作提示：%v", got[0])
	}
	if got[1]["chart"] != false {
		t.Errorf("缺 #usChart 时不应伪造成已渲染：%v", got[1])
	}
	// 从第 2 个 case 起都是“只缺一块”：统计卡必须照常渲染，且必须留下
	// 一次版本错配提示（不能静默吞掉）。
	for _, c := range got[2:7] {
		if c["stats"] != true {
			t.Errorf("%s 应保留统计卡渲染：%v", c["name"], c)
		}
		tips, _ := c["allTips"].([]any)
		if len(tips) == 0 {
			t.Errorf("%s 缺少版本错配提示：%v", c["name"], c)
		}
	}
	// 失败分支同样不能因缺 usChart/usError 写 null：应留下错误提示并正常结束。
	if got[7]["threw"] != nil && got[7]["threw"] != false {
		t.Errorf("failure-chart-missing threw %v", got[7]["threw"])
	}
	if tips, _ := got[7]["allTips"].([]any); len(tips) == 0 {
		t.Errorf("失败分支缺块也必须提示：%v", got[7])
	}
}

// TestAppJSBenefitsVirtualWindow 福利账号表必须和账号池一样只渲染窗口行：
// 200 个账号时 DOM 不能出现 200 行，上下占位行要维持完整滚动高度，刷新后
// 滚动位置不能回到顶部；同时福利总积分与发放摘要应来自同一份 benefits 数据。
func TestAppJSBenefitsVirtualWindow(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function benefitRealmLabel');
const end = src.indexOf('function renderBenefitFocus', start);
if (start < 0 || end < 0 || end < start) throw new Error('benefits virtual region not found');
const sink = id => ({ id, innerHTML: '', textContent: '', scrollTop: 0, clientHeight: 420, addEventListener() {}, querySelector() { return null; } });
const nodes = {};
const $ = id => nodes[id] || (nodes[id] = sink(id));
const ctx = {
  $, esc: s => String(s == null ? '' : s), fmtTok: v => String(v == null ? 0 : v),
  setText: (id, v) => { $(id).textContent = v; },
  Math, Number, String, Array, Object, Date, isNaN,
  benefitsBusy: new Set(), benefitsRows: [], benefitsRowH: 56, benefitsVirtualBound: false,
  window: { matchMedia: () => ({ matches: false }) },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.renderBenefitsAccounts=renderBenefitsAccounts;this.benefitsVirtualWindow=benefitsVirtualWindow;', ctx);
const rows = [];
for (let i = 0; i < 200; i++) rows.push({uid: 'u' + String(i).padStart(3, '0'), nickname: '账号' + i, realm: i % 2 ? 'global' : 'cn', ref: 'r' + i, checkin_done: i % 3 === 0});
$('bfViewport').scrollTop = 1120;
ctx.renderBenefitsAccounts(rows);
const html = $('bfAccountsBody').innerHTML;
const rendered = (html.match(/data-bf-ref=/g) || []).length;
const pads = (html.match(/class="virtual-pad"/g) || []).length;
const keptScroll = $('bfViewport').scrollTop;
process.stdout.write(JSON.stringify({rendered, pads, keptScroll, hasTopPad: html.indexOf('height:840px') >= 0}));`
	out := runNodeScript(t, "benefits-virtual", script, appJSPath(t))
	var got struct {
		Rendered   int  `json:"rendered"`
		Pads       int  `json:"pads"`
		KeptScroll int  `json:"keptScroll"`
		HasTopPad  bool `json:"hasTopPad"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode benefits virtual: %v (%s)", err, out)
	}
	// 视口 420px / 行高 56px ≈ 8 行，加上上下缓冲区仍应远小于 200；
	// 具体缓冲区大小可在不改变虚拟化契约的前提下调整。
	if got.Rendered < 16 || got.Rendered > 80 || got.Pads != 2 || got.KeptScroll != 1120 || !got.HasTopPad {
		t.Fatalf("benefits virtual=%+v", got)
	}
}

// TestAppJSBenefitsFocusContract 选择器必须默认「全部国内账号」，单号模式只显示
// 该号任务；任务表是 Hub 的 5 列（无行内操作），完整 UID 不出现在 DOM。
func TestAppJSBenefitsFocusContract(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function benefitTaskStatus');
const end = src.indexOf('async function runTaskAction', start);
if (start < 0 || end < 0 || end < start) throw new Error('task workbench region not found');
const nodes = {};
const mk = id => (nodes[id] = { id, innerHTML: '', textContent: '', value: '', disabled: false, hidden: false, addEventListener() {}, querySelector() { return null; } });
const $ = id => nodes[id] || mk(id);
const ctx = {
  $, esc: s => String(s == null ? '' : s), fmtTok: v => String(v == null ? 0 : v),
  benefitsFocusRef: 'all', benefitsTaskRows: [], benefitsTaskData: null,
  benefitTaskStatus: () => '', AUTO_TASKS: {},
};
const make = focus => {
  const nodes = {};
  const mk = id => (nodes[id] = { id, innerHTML: '', textContent: '', value: '', disabled: false, hidden: false, addEventListener() {}, querySelector() { return null; } });
  const $ = id => nodes[id] || mk(id);
  const c = {
    $, esc: s => String(s == null ? '' : s), fmtTok: v => String(v == null ? 0 : v),
    benefitsFocusRef: focus, benefitsTaskRows: [], benefitsTaskData: null, benefitsLogFilter: {task: '', result: '', ref: ''},
    benefitTaskStatus: () => '', AUTO_TASKS: {},
  };
  vm.createContext(c);
  vm.runInContext(src.slice(start, end) + '\nthis.renderTaskWorkbench=renderTaskWorkbench;this.renderBenefitTaskRows=renderBenefitTaskRows;', c);
  return { c, $ };
};
const allCtx = make('all');
allCtx.c.renderTaskWorkbench({
  accounts: [
    {ref: 'ref-cn', uid: '13800138', nickname: '国内', realm: 'cn'},
    {ref: 'ref-cn2', uid: '13900139', nickname: '国内二', realm: 'cn'}
  ],
  summary: {accounts: 2, checkin_done: 1, checkin_pending: 1},
  tasks: []
});
const allSel = allCtx.$('bfGrowthAcct'), allBody = allCtx.$('bfTaskBody').innerHTML;
const oneCtx = make('ref-cn');
oneCtx.c.renderTaskWorkbench({
  accounts: [{ref: 'ref-cn', uid: '13800138', nickname: '国内', realm: 'cn'}],
  summary: {accounts: 1, streak_days: 7, makeup_cards: 2, lottery_chances: 3, energy: 99, cat_reward: 8},
  tasks: [
    {task_code: 'AUTO_TASK', title: '自动任务', task_desc: '说明', current: 1, target: 3, credit: 10, energy: 2, accept_status: 'accepted'},
    {task_code: 'MANUAL_TASK', title: '手动任务', current: 0, target: 1, accept_status: 'not_accepted'}
  ]
});
const oneSel = oneCtx.$('bfGrowthAcct'), oneBody = oneCtx.$('bfTaskBody').innerHTML;
process.stdout.write(JSON.stringify({
  allOptions: (allSel.innerHTML.match(/<option/g) || []).length,
  allSelected: allSel.value,
  allHint: allBody.includes('选择单个账号'),
  oneOptions: (oneSel.innerHTML.match(/<option/g) || []).length,
  oneSelected: oneSel.value,
  oneRows: (oneBody.match(/<tr/g) || []).length,
  oneActions: oneBody.includes('data-bf-task='),
  oneHasReward: oneBody.includes('+10 分'),
  leakedFullUID: allSel.innerHTML.includes('13800138000') || oneBody.includes('13800138000')
}));`
	out := runNodeScript(t, "benefits-focus", script, appJSPath(t))
	const want = `{"allOptions":3,"allSelected":"all","allHint":true,"oneOptions":2,"oneSelected":"ref-cn","oneRows":2,"oneActions":false,"oneHasReward":true,"leakedFullUID":false}`
	if out != want {
		t.Fatalf("focus workbench=%s\nwant %s", out, want)
	}
}

// TestAppJSBenefitLogContract 任务页的最近执行流水必须来自 benefits/log，
// 按时间/账号/任务/结果展示；完整 UID 不应出现在 DOM。
func TestAppJSBenefitLogContract(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function benefitTaskLabel');
const end = src.indexOf('async function loadBenefitLog', start);
if (start < 0 || end < 0 || end < start) throw new Error('benefit log region not found');
const nodes = {};
const mk = id => (nodes[id] = { id, innerHTML: '', textContent: '', value: '', disabled: false, hidden: false, addEventListener() {}, querySelector() { return null; } });
const $ = id => nodes[id] || mk(id);
const ctx = {
  $, esc: s => String(s == null ? '' : s),
  benefitsTime: v => String(v || '').slice(0, 16),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderBenefitLog=renderBenefitLog;', ctx);
ctx.renderBenefitLog([
  {ts: '2026-10-19T09:12:00Z', nickname: '甲', realm: 'cn', task: 'checkin', trigger: 'manual', ok: true, message: '签到成功'},
  {ts: '2026-10-19T09:11:00Z', nickname: '乙', realm: 'cn', task: 'travel', trigger: 'scheduler', ok: false, message: '旅行失败'}
]);
const html = $('bfLogList').innerHTML;
process.stdout.write(JSON.stringify({
  rows: (html.match(/class="bf-log-row"/g) || []).length,
  success: html.includes('签到成功'),
  failure: html.includes('旅行失败'),
  manual: html.includes('手动'),
  schedule: html.includes('排程'),
  note: $('bfLogNote').textContent,
  leaked: html.includes('13800138000')
}));`
	out := runNodeScript(t, "benefit-log", script, appJSPath(t))
	var got struct {
		Rows     int    `json:"rows"`
		Success  bool   `json:"success"`
		Failure  bool   `json:"failure"`
		Manual   bool   `json:"manual"`
		Schedule bool   `json:"schedule"`
		Note     string `json:"note"`
		Leaked   bool   `json:"leaked"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode benefit log: %v (%s)", err, out)
	}
	if got.Rows != 2 || !got.Success || !got.Failure || !got.Manual || !got.Schedule || got.Note != "最近 2 条" || got.Leaked {
		t.Fatalf("benefit log=%+v", got)
	}
}

// TestAppJSBenefitsDoesNotProbeRemovedSummaryCards 新版 Hub 式任务页已经删掉
// 旧统计卡。renderBenefits 不能在正确的新页面上探测这些 id，否则会误报
// “页面版本与脚本不一致”；旧骨架存在时仍应兼容更新。
func TestAppJSBenefitsDoesNotProbeRemovedSummaryCards(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function benefitsVirtualWindow');
const end = src.indexOf('async function loadBenefits', start);
if (start < 0 || end < 0 || end < start) throw new Error('benefits render region not found');
const nodes = {};
const mk = id => (nodes[id] = { id, innerHTML: '', textContent: '', hidden: false, scrollTop: 0, clientHeight: 420, addEventListener() {}, querySelector() { return null; } });
const missing = [];
const ctx = {
  $: id => nodes[id] || null,
  esc: s => String(s == null ? '' : s), fmtTok: v => String(v == null ? 0 : v),
  setText: (id, v) => { if (!nodes[id]) { missing.push(id); return false; } nodes[id].textContent = v; return true; },
  noteUsageDomMismatch: id => missing.push(id),
  benefitsBusy: new Set(), benefitsRows: [], benefitsRowH: 56,
  benefitsVirtualBound: true, benefitsBatchSeq: 0, benefitsBatchTimer: null,
  api: () => Promise.resolve({runs: []}), view: 'taskscenter', setTimeout,
  benefitsFocusRef: '', benefitsTaskRows: [], benefitsFocusLast: null,
  benefitsBatchDoneSig: '', AUTO_TASKS: {},
  window: { matchMedia: () => ({ matches: false }) },
  Math, Number, String, Array, Object, Date, isNaN,
};
mk('bfActionNote'); mk('bfGrantsBody');
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderBenefits=renderBenefits;', ctx);
ctx.renderBenefits({
  summary: {accounts: 3, checkin_done: 2, checkin_pending: 1, welfare_grants_available: true, welfare_credits_total: 80, welfare_grant_count: 2},
  accounts: [], recent_grants: [],
});
const current = { note: nodes.bfActionNote.textContent, missing: missing.slice() };
mk('bfAccounts'); mk('bfWelfareCredits'); mk('bfWelfareSub');
ctx.renderBenefits({summary: {accounts: 1, welfare_grants_available: true, welfare_credits_total: 80, welfare_grant_count: 2}, accounts: [], recent_grants: []});
process.stdout.write(JSON.stringify({
  current,
  legacy: { total: nodes.bfWelfareCredits.textContent, sub: nodes.bfWelfareSub.textContent },
}));`
	out := runNodeScript(t, "benefits-removed-cards", script, appJSPath(t))
	var got struct {
		Current struct {
			Note    string   `json:"note"`
			Missing []string `json:"missing"`
		} `json:"current"`
		Legacy struct {
			Total string `json:"total"`
			Sub   string `json:"sub"`
		} `json:"legacy"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if len(got.Current.Missing) != 0 {
		t.Fatalf("new task page must not probe removed cards: %v", got.Current.Missing)
	}
	if !strings.Contains(got.Current.Note, "福利积分 +80") {
		t.Fatalf("current page summary=%q", got.Current.Note)
	}
	if got.Legacy.Total != "+80" || got.Legacy.Sub != "2 笔福利发放" {
		t.Fatalf("legacy card fallback=%+v", got.Legacy)
	}
}

// TestAppJSBenefitsWelfareSummary 福利总积分/发放记录必须用 summary 的可用标记
// 区分“尚未刷新快照”和“确实为 0”，并把最近记录按时间展示。
func TestAppJSBenefitsWelfareSummary(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function benefitsVirtualWindow');
const end = src.indexOf('async function loadBenefits', start);
if (start < 0 || end < 0 || end < start) throw new Error('benefits render region not found');
const nodes = {};
const mk = id => (nodes[id] = { id, innerHTML: '', textContent: '', hidden: false, scrollTop: 0, clientHeight: 420, addEventListener() {}, querySelector() { return null; } });
const $ = id => nodes[id] || mk(id);
const ctx = {
  $, esc: s => String(s == null ? '' : s), fmtTok: v => String(v == null ? 0 : v),
  setText: (id, v) => { $(id).textContent = v; }, benefitsBusy: new Set(),
  benefitsRows: [], benefitsRowH: 56, benefitsVirtualBound: false,
  benefitsBatchSeq: 0, benefitsBatchTimer: null,
  api: () => Promise.resolve({runs: []}), view: 'taskscenter', setTimeout,
  benefitsFocusRef: '', benefitsTaskRows: [], benefitsFocusLast: null,
  benefitsBatchDoneSig: '', AUTO_TASKS: {},
  window: { matchMedia: () => ({ matches: false }) }, Math, Number, String, Array, Object, Date, isNaN,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderBenefits=renderBenefits;', ctx);
ctx.renderBenefits({
  summary: {accounts: 1, welfare_credits_total: 80, welfare_grant_count: 2, welfare_grants_available: true, welfare_snapshot_at: '2026-10-19T09:00:00Z'},
  accounts: [], recent_grants: [
    {uid: '13800138', nickname: '秋叶', name: 'Buddy 加油站签到奖励', size: 30, remain: 30, created_at: '2026-10-19T08:30:00Z', status: 'active'},
    {uid: '13800138', nickname: '秋叶', name: '成长计划奖励', size: 50, remain: 20, created_at: '2026-10-19T07:30:00Z', status: 'available'}
  ]
});
const grants = $('bfGrantsBody').innerHTML;
process.stdout.write(JSON.stringify({
  total: $('bfWelfareCredits').textContent,
  sub: $('bfWelfareSub').textContent,
  rows: (grants.match(/<tr>/g) || []).length,
  signin: grants.includes('Buddy 加油站签到奖励'),
  growth: grants.includes('成长计划奖励')
}));`
	out := runNodeScript(t, "benefits-summary", script, appJSPath(t))
	const want = `{"total":"+80","sub":"2 笔福利发放","rows":2,"signin":true,"growth":true}`
	if out != want {
		t.Fatalf("benefits summary=%s\nwant %s", out, want)
	}
}

// TestAppJSBenefitsRefreshForcesUpstream 刷新概览必须显式带 refresh=1；
// 仅清前端请求去重仍会命中服务端 30 秒快照，按钮看起来“刷新了”但数字没变。
func TestAppJSBenefitsRefreshForcesUpstream(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadBenefits');
const end = src.indexOf('\n}\n', start) + 3;
if (start < 0 || end <= start) throw new Error('loadBenefits not found');
const calls = [];
const ctx = {
  benefitsReq: null, benefitsSeq: 0, benefitsFocusRef: 'ref-1',
  view: 'taskscenter', renderBenefits() {}, renderTaskWorkbench() {},
  api: path => { calls.push(path); return Promise.resolve({}); },
};
vm.createContext(ctx);
vm.runInContext('let view="taskscenter"; let renderedTasks=false;\n' + src.slice(start, end) + '\nthis.loadBenefits=loadBenefits; this.readRendered=()=>renderedTasks;', ctx);
ctx.loadBenefits(false, false)
  .then(() => ctx.loadBenefits(true, true))
  .then(() => process.stdout.write(JSON.stringify(calls)));
`
	out := runNodeScript(t, "benefits-refresh", script, appJSPath(t))
	if out != `["benefits?focus=ref-1","tasks?focus=ref-1","benefits?focus=ref-1&refresh=1","tasks?focus=ref-1&refresh=1"]` {
		t.Fatalf("benefits requests=%s", out)
	}
}

// TestAppJSBenefitsRequestsAreIndependent 任务页两个快照请求必须分别处理：
// /benefits 超时只能清空账号福利表，不能连执行日志/任务选择器一起丢掉；
// 同时两种请求都应使用任务页专用的更宽时限，而不是通用 15 秒。
func TestAppJSBenefitsRequestsAreIndependent(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadBenefits');
const end = src.indexOf('\n}\n', start) + 3;
if (start < 0 || end <= start) throw new Error('loadBenefits not found');
const timeouts = [];
const nodes = {};
const mk = id => nodes[id] || (nodes[id] = {id, innerHTML: '', textContent: ''});
const ctx = {
  benefitsReq: null, benefitsSeq: 0, benefitsFocusRef: 'all',
  view: 'taskscenter',
  renderBenefits() {},
  renderTaskWorkbench() {},
  api: (path, opts) => {
    timeouts.push(opts && opts.timeoutMs);
    return path.startsWith('benefits') ? Promise.reject(new Error('部分账号查询超时')) : Promise.resolve({accounts: []});
  },
  $: mk, esc: s => String(s == null ? '' : s),
  Math, Number, String, Promise,
};
vm.createContext(ctx);
vm.runInContext('let view="taskscenter";\n' + src.slice(start, end) + '\nthis.loadBenefits=loadBenefits;', ctx);
ctx.loadBenefits(false, false).then(() => process.stdout.write(JSON.stringify({
  tasksRendered: !(nodes.bfFocusSummary && nodes.bfFocusSummary.innerHTML.includes('任务查询失败')),
  grants: nodes.bfGrantsBody ? nodes.bfGrantsBody.innerHTML : '',
  timeouts,
})));`
	out := runNodeScript(t, "benefits-independent", script, appJSPath(t))
	if !strings.Contains(out, `"tasksRendered":true`) || !strings.Contains(out, "读取积分发放记录失败") {
		t.Fatalf("tasks page must keep the successful half after /benefits timeout: %s", out)
	}
	if !strings.Contains(out, `"timeouts":[45000,45000]`) {
		t.Fatalf("task page requests must use the extended deadline: %s", out)
	}
}

// TestTasksCenterParityDom 任务页必须同时具备 Hub 的账号选择、批量动作、
// 执行结果与最近流水；账号表视口必须是固定高度虚拟窗口，不能随行数撑满整页。
func TestTasksCenterParityDom(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, id := range []string{
		"bfGrowthAcct", "bfBtnFocusCheckin", "bfBtnFocusRun", "bfBtnFocusTravel", "bfBtnFocusStreak",
		"bfTaskViewport", "bfTaskBody", "bfBatchList", "bfLogList", "bfBtnLogRefresh",
		"accViewport", "accBody",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("missing #%s in task/account UI", id)
		}
	}
	if !strings.Contains(html, ".acc-viewport { position: relative; height:") {
		t.Errorf("account viewport must use a bounded fixed height")
	}
	if !strings.Contains(html, ".benefits-tasks { height:") {
		t.Errorf("task detail must use a bounded viewport too")
	}
	if strings.Contains(html, "<th>任务名称 / 代码</th><th>说明</th><th>进度</th><th>奖励</th><th>状态</th><th>操作</th>") {
		t.Errorf("task table must use Hub's 5 columns; actions belong in the header")
	}
	if !strings.Contains(html, `id="bfAdvanced"`) {
		t.Errorf("legacy queue/voucher entry must be folded into advanced details")
	}
}

// TestAppJSBenefitBatchResults 批量动作结果区必须展示运行中/完成统计，
// 并只渲染服务端给出的脱敏字段，不拼接任何 UID 或凭证。
func TestAppJSBenefitBatchResults(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderBenefitBatches');
const end = src.indexOf('async function loadBenefitBatches', start);
if (start < 0 || end < 0 || end < start) throw new Error('benefit batch region not found');
const nodes = {};
const mk = id => (nodes[id] = { id, innerHTML: '', textContent: '' });
const $ = id => nodes[id] || mk(id);
const ctx = {
  $, esc: s => String(s == null ? '' : s), benefitsTime: v => String(v == null ? '' : v),
  Math, Number, String, Array, Object, BENEFIT_BATCH_LABELS: {checkin: '全部签到', streak: '连登与抽奖'},
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderBenefitBatches=renderBenefitBatches;', ctx);
ctx.renderBenefitBatches([
  {kind: 'checkin', started_at: '2026-10-19T09:00:00Z', running: false, done: true, total: 5, ok: 4, failed: 1},
  {kind: 'streak', started_at: '2026-10-19T08:00:00Z', running: true}
]);
const html = $('bfBatchList').innerHTML;
process.stdout.write(JSON.stringify({
  note: $('bfBatchNote').textContent,
  checkin: html.includes('全部签到'), streak: html.includes('连登与抽奖'),
  done: html.includes('成功 4 · 失败 1 · 共 5'), running: html.includes('执行中'),
  failedTag: html.includes('1 失败')
}));`
	out := runNodeScript(t, "benefit-batches", script, appJSPath(t))
	const want = `{"note":"最近 2 轮","checkin":true,"streak":true,"done":true,"running":true,"failedTag":true}`
	if out != want {
		t.Fatalf("benefit batches=%s\nwant %s", out, want)
	}
}
