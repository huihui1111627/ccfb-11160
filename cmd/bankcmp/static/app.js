'use strict';

// ---- tiny helpers -------------------------------------------------------
const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: opts.body ? { 'Content-Type': 'application/json' } : {},
    ...opts,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) {
    const msg = data && data.error ? data.error : `${res.status} ${res.statusText}`;
    throw new Error(msg);
  }
  return data;
}

const fmt = (n) => (n == null ? '-' : Number(n).toLocaleString('en-US'));
const dur = (ms) => {
  if (ms == null) return '-';
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`;
  const m = Math.floor(ms / 60000), s = Math.round((ms % 60000) / 1000);
  return `${m}m${s}s`;
};
const esc = (s) => String(s == null ? '' : s);

const KIND_LABEL = { missing: '多出', extra: '缺失', modified: '内容不同' };
const KIND_HINT = { missing: '源库有，目标库没有', extra: '目标库多出，源库没有', modified: '两库都有，内容不同' };
const STAB_LABEL = { stable: '稳定', transient: '期间变动', pending: '待复核' };
const STATUS_LABEL = {
  queued: '排队中', running: '运行中', succeeded: '已完成', failed: '失败', canceled: '已取消',
  pending: '等待', equal: '一致', diff: '有差异', skipped: '跳过',
};

function statusPill(status) {
  let cls = '';
  if (status === 'succeeded' || status === 'equal') cls = 'ok';
  else if (status === 'diff') cls = 'bad';
  else if (status === 'failed' || status === 'canceled') cls = 'bad';
  else if (status === 'running') cls = 'run';
  else if (status === 'queued') cls = 'warn';
  return el('span', { class: `pill ${cls}`, text: STATUS_LABEL[status] || status });
}

function msg(node, text, kind) {
  node.textContent = text || '';
  node.className = 'msg' + (kind ? ' ' + kind : '');
}

// ---- state --------------------------------------------------------------
const state = {
  connections: [],
  tasks: [],
  selectedTask: null,
  taskPoll: null,
  diffPage: 0,
  diffPageSize: 50,
  diffTotal: 0,
  taskTables: [],
  previewRepair: null,
};

// ---- tabs ---------------------------------------------------------------
function activateTab(name) {
  $$('.tab').forEach((t) => t.classList.toggle('active', t.dataset.tab === name));
  $$('.panel').forEach((p) => p.classList.toggle('active', p.id === `panel-${name}`));
  location.hash = name;
  if (name === 'connections') loadConnections();
  if (name === 'tasks') loadTasks();
  if (name === 'diffs') initDiffTab();
  if (name === 'repairs') initRepairTab();
  if (name === 'report') initReportTab();
}
$$('.tab').forEach((t) => t.addEventListener('click', () => activateTab(t.dataset.tab)));

// ---- health -------------------------------------------------------------
async function pollHealth() {
  const node = $('#health');
  try {
    const h = await api('/api/health');
    node.textContent = `服务正常 · 运行中任务 ${h.running}`;
    node.className = 'pill ok';
  } catch {
    node.textContent = '服务不可用';
    node.className = 'pill bad';
  }
}

// ---- connections --------------------------------------------------------
async function loadConnections() {
  state.connections = (await api('/api/connections')) || [];
  const list = $('#conn-list');
  list.innerHTML = '';
  if (!state.connections.length) {
    list.append(el('p', { class: 'empty', text: '还没有连接，先在左侧添加。' }));
  }
  for (const c of state.connections) {
    list.append(el('div', { class: 'item' },
      el('div', { class: 'main' },
        el('div', { class: 'title' }, c.name, ' ', el('span', { class: 'pill', text: c.driver })),
        el('div', { class: 'sub mono', title: c.dsn, text: c.dsn }),
        c.charset ? el('div', { class: 'sub', text: `默认字符集 ${c.charset}` }) : null,
      ),
      el('div', { class: 'row' },
        el('button', { class: 'ghost', onclick: () => editConnection(c), text: '编辑' }),
        el('button', { class: 'ghost', onclick: () => makeTaskWithConnection(c), text: '用于比对' }),
        el('button', { class: 'ghost', onclick: () => deleteConnection(c), text: '删除' }),
      ),
    ));
  }
  refreshConnectionSelects();
}

function editConnection(c) {
  $('#conn-id').value = c.id;
  $('#conn-name').value = c.name;
  $('#conn-driver').value = c.driver;
  $('#conn-dsn').value = c.dsn;
  $('#conn-charset').value = c.charset || '';
  $('#conn-note').value = c.note || '';
  msg($('#conn-msg'), '');
  window.scrollTo({ top: 0, behavior: 'smooth' });
}

async function deleteConnection(c) {
  if (!confirm(`删除连接“${c.name}”？`)) return;
  await api(`/api/connections/${c.id}`, { method: 'DELETE' });
  await loadConnections();
}

function connFormPayload() {
  return {
    name: $('#conn-name').value.trim(),
    driver: $('#conn-driver').value,
    dsn: $('#conn-dsn').value.trim(),
    charset: $('#conn-charset').value,
    note: $('#conn-note').value.trim(),
  };
}

$('#conn-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const id = $('#conn-id').value;
  const payload = connFormPayload();
  try {
    if (id) await api(`/api/connections/${id}`, { method: 'PUT', body: payload });
    else await api('/api/connections', { method: 'POST', body: payload });
    msg($('#conn-msg'), '已保存', 'ok');
    resetConnForm();
    await loadConnections();
  } catch (err) {
    msg($('#conn-msg'), err.message, 'error');
  }
});

$('#conn-test').addEventListener('click', async () => {
  const payload = connFormPayload();
  msg($('#conn-msg'), '连接中…');
  try {
    const r = await api('/api/connections/test', { method: 'POST', body: payload });
    if (r.ok) {
      msg($('#conn-msg'), `连接成功：${r.version}，可见 ${r.tables.length} 张表`, 'ok');
    } else {
      msg($('#conn-msg'), `连接失败：${r.error}`, 'error');
    }
  } catch (err) {
    msg($('#conn-msg'), err.message, 'error');
  }
});

function resetConnForm() {
  ['#conn-id', '#conn-name', '#conn-dsn', '#conn-note'].forEach((s) => ($(s).value = ''));
  $('#conn-charset').value = '';
  msg($('#conn-msg'), '');
}
$('#conn-reset').addEventListener('click', resetConnForm);

function refreshConnectionSelects() {
  for (const selId of ['#task-source', '#task-target']) {
    const sel = $(selId);
    const prev = sel.value;
    sel.innerHTML = '';
    sel.append(el('option', { value: '', text: '选择连接…' }));
    for (const c of state.connections) {
      sel.append(el('option', { value: String(c.id), text: `${c.name} (${c.driver})` }));
    }
    if (prev) sel.value = prev;
  }
}

function makeTaskWithConnection(c) {
  activateTab('tasks');
  if (state.connections.length < 2) {
    msg($('#task-msg'), '还需要另一个连接才能做比对。', 'error');
    return;
  }
  const sel = $('#task-source');
  if (!sel.value) sel.value = String(c.id);
  else $('#task-target').value = String(c.id);
}

// ---- tasks --------------------------------------------------------------
const SAMPLE_CONFIG = `# 每张表可单独配置列类型、字符集与摘要比对
# 不写 columns 时默认比较两侧都有的全部列
tables:
  - name: accounts
    columns:
      - name: balance
        type: decimal      # 源库文本金额 vs 目标库 numeric
      - name: open_date
        type: date
  - name: transactions
    columns:
      - name: amount
        type: decimal
      - name: txn_time
        type: datetime
      - name: memo
        type: string
        digest: true       # 大字段只比服务端摘要
      - name: receipt
        type: bytes
        digest: true
`;

$('#task-sample').addEventListener('click', () => { $('#task-config').value = SAMPLE_CONFIG; });

async function submitTask(start) {
  const src = $('#task-source').value;
  const tgt = $('#task-target').value;
  if (!src || !tgt) { msg($('#task-msg'), '请选择源库和目标库', 'error'); return; }
  const payload = {
    name: $('#task-name').value.trim() || '未命名任务',
    source_id: Number(src),
    target_id: Number(tgt),
    config: $('#task-config').value,
    start,
    options: {
      workers: Number($('#opt-workers').value) || 2,
      chunk_workers: Number($('#opt-chunk-workers').value) || 4,
      chunk_rows: Number($('#opt-chunk-rows').value) || 20000,
      diff_max_rows: 50000,
      rows_per_second: Number($('#opt-rps').value) || 0,
      queries_per_second: Number($('#opt-qps').value) || 0,
      settle_ms: Number($('#opt-settle').value) || 300,
      recheck: true,
    },
  };
  try {
    const task = await api('/api/tasks', { method: 'POST', body: payload });
    msg($('#task-msg'), start ? `任务 #${task.id} 已启动` : `任务 #${task.id} 已创建`, 'ok');
    await loadTasks();
    selectTask(task.id);
  } catch (err) {
    msg($('#task-msg'), err.message, 'error');
  }
}
$('#task-form').addEventListener('submit', (e) => { e.preventDefault(); submitTask(true); });
$('#task-dry').addEventListener('click', () => submitTask(false));

async function loadTasks() {
  state.tasks = (await api('/api/tasks')) || [];
  const list = $('#task-list');
  list.innerHTML = '';
  if (!state.tasks.length) {
    list.append(el('p', { class: 'empty', text: '还没有任务。' }));
    return;
  }
  for (const t of state.tasks) {
    list.append(el('div', { class: 'item' },
      el('div', { class: 'main' },
        el('div', { class: 'title' }, `#${t.id} ${t.name}`),
        el('div', { class: 'sub', text: `${t.source_driver} → ${t.target_driver} · 创建于 ${new Date(t.created_at).toLocaleString()}${t.duration_ms ? ' · 耗时 ' + dur(t.duration_ms) : ''}` }),
      ),
      el('div', { class: 'row' },
        statusPill(t.status),
        el('button', { class: 'ghost', onclick: () => selectTask(t.id), text: '详情' }),
        el('button', { class: 'ghost', onclick: () => startTask(t.id), text: '启动' }),
        el('button', { class: 'ghost', onclick: () => cancelTask(t.id), text: '取消' }),
      ),
    ));
  }
  refreshConnectionSelects();
}

async function startTask(id) {
  try {
    await api(`/api/tasks/${id}/start`, { method: 'POST' });
  } catch (err) {
    alert(err.message);
  }
  await loadTasks();
  selectTask(id);
}

async function cancelTask(id) {
  if (!confirm(`取消任务 #${id}？`)) return;
  await api(`/api/tasks/${id}/cancel`, { method: 'POST' });
  await loadTasks();
}

async function selectTask(id) {
  state.selectedTask = id;
  $('#task-detail').classList.remove('hidden');
  $('#task-detail-title').textContent = `任务 #${id} 详情`;
  await refreshTaskDetail();
  if (state.taskPoll) clearInterval(state.taskPoll);
  state.taskPoll = setInterval(async () => {
    if (state.selectedTask !== id) { clearInterval(state.taskPoll); return; }
    try {
      const d = await api(`/api/tasks/${id}`);
      if (d.running || d.task.status === 'running' || d.task.status === 'queued') {
        await refreshTaskDetail();
        await loadTasks();
      }
    } catch { /* ignore transient polling errors */ }
  }, 2000);
}

async function refreshTaskDetail() {
  const id = state.selectedTask;
  if (!id) return;
  let d;
  try {
    d = await api(`/api/tasks/${id}`);
  } catch (err) { msg($('#task-summary'), err.message, 'error'); return; }
  state.taskTables = d.tables || [];
  const task = d.task;

  const summary = $('#task-summary');
  summary.innerHTML = '';
  const stat = (k, v, cls) => el('div', { class: `stat ${cls || ''}` }, el('div', { class: 'v', text: v }), el('div', { class: 'k', text: k }));
  const counts = d.diff_counts || {};
  const totalDiffs = Object.values(counts).reduce((a, b) => a + b, 0);
  summary.append(
    stat('状态', STATUS_LABEL[task.status] || task.status, task.status === 'succeeded' ? 'ok' : task.status === 'failed' ? 'bad' : ''),
    stat('表数量', fmt(state.taskTables.length)),
    stat('差异总数', fmt(totalDiffs), totalDiffs ? 'bad' : 'ok'),
    stat('多出', fmt(counts.missing || 0)),
    stat('缺失', fmt(counts.extra || 0)),
    stat('内容不同', fmt(counts.modified || 0)),
    stat('耗时', dur(task.duration_ms)),
  );
  if (task.error) summary.append(el('div', { class: 'msg error', text: task.error }));
  const written = state.taskTables.filter((t) => t.source_changed || t.target_changed);
  if (written.length) {
    summary.append(el('div', { class: 'msg error',
      text: `${written.length} 张表在比对期间被写入（${written.map((t) => t.table_name).join(', ')}）。标记为“期间变动”的差异行在修复时默认跳过。` }));
  }

  const table = $('#table-runs');
  table.innerHTML = '';
  table.append(el('thead', {}, el('tr', {},
    ['表', '状态', '源行数', '目标行数', '差异', '多出', '缺失', '内容不同', '稳定', '期间变动', '区间', '耗时', ''].map((h) => el('th', { text: h })),
  )));
  const tbody = el('tbody');
  if (!state.taskTables.length) {
    tbody.append(el('tr', {}, el('td', { colspan: 13, class: 'empty', text: '尚无表级结果' })));
  }
  for (const t of state.taskTables) {
    tbody.append(el('tr', { class: 'clickable', onclick: () => openTableDiffs(t.table_name) },
      el('td', { class: 'mono', text: t.table_name }),
      el('td', {}, statusPill(t.status)),
      el('td', { class: 'num', text: fmt(t.source_rows) }),
      el('td', { class: 'num', text: fmt(t.target_rows) }),
      el('td', { class: 'num', text: fmt(t.diffs) }),
      el('td', { class: 'num', text: fmt(t.missing) }),
      el('td', { class: 'num', text: fmt(t.extra) }),
      el('td', { class: 'num', text: fmt(t.modified) }),
      el('td', { class: 'num', text: fmt(t.stable) }),
      el('td', { class: 'num', text: fmt(t.transient) }),
      el('td', { class: 'num', text: fmt(t.chunks_total) }),
      el('td', { class: 'num', text: dur(t.duration_ms) }),
      el('td', {}, el('button', { class: 'ghost', onclick: (e) => { e.stopPropagation(); openTableDiffs(t.table_name); }, text: '差异' })),
    ));
  }
  table.append(tbody);

  const events = (await api(`/api/tasks/${id}/events?after=0`)) || [];
  $('#task-events').textContent = events.map((e) => `${e.ts.slice(11, 19)} [${e.level}] ${e.msg}`).join('\n') || '（暂无日志）';
}

function openTableDiffs(tableName) {
  activateTab('diffs');
  $('#diff-task').value = String(state.selectedTask);
  onDiffTaskChange(tableName);
}

$('#task-refresh').addEventListener('click', refreshTaskDetail);

// ---- diffs --------------------------------------------------------------
async function initDiffTab() {
  const sel = $('#diff-task');
  const prev = sel.value || (state.selectedTask ? String(state.selectedTask) : '');
  sel.innerHTML = '';
  for (const t of state.tasks.length ? state.tasks : await api('/api/tasks')) {
    sel.append(el('option', { value: String(t.id), text: `#${t.id} ${t.name}` }));
  }
  if (prev) sel.value = prev;
  await onDiffTaskChange();
}

async function onDiffTaskChange(preselectTable) {
  const taskId = $('#diff-task').value;
  state.diffPage = 0;
  const tableSel = $('#diff-table');
  tableSel.innerHTML = '<option value="">全部表</option>';
  if (taskId) {
    const tables = await api(`/api/tasks/${taskId}/tables`);
    for (const t of tables) {
      tableSel.append(el('option', { value: t.table_name, text: `${t.table_name} (${t.diffs})` }));
    }
  }
  if (preselectTable) tableSel.value = preselectTable;
  await loadDiffs();
}
$('#diff-task').addEventListener('change', () => onDiffTaskChange());
$('#diff-table').addEventListener('change', () => { state.diffPage = 0; loadDiffs(); });
$('#diff-kind').addEventListener('change', () => { state.diffPage = 0; loadDiffs(); });
$('#diff-stability').addEventListener('change', () => { state.diffPage = 0; loadDiffs(); });
let searchTimer = null;
$('#diff-search').addEventListener('input', () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => { state.diffPage = 0; loadDiffs(); }, 250);
});
$('#diff-refresh').addEventListener('click', () => loadDiffs());
$('#diff-prev').addEventListener('click', () => { if (state.diffPage > 0) { state.diffPage--; loadDiffs(); } });
$('#diff-next').addEventListener('click', () => {
  if ((state.diffPage + 1) * state.diffPageSize < state.diffTotal) { state.diffPage++; loadDiffs(); }
});

async function loadDiffs() {
  const taskId = $('#diff-task').value;
  if (!taskId) { msg($('#diff-meta'), '请选择任务'); return; }
  const params = new URLSearchParams({
    limit: String(state.diffPageSize),
    offset: String(state.diffPage * state.diffPageSize),
  });
  const table = $('#diff-table').value;
  const kind = $('#diff-kind').value;
  const stab = $('#diff-stability').value;
  const q = $('#diff-search').value.trim();
  if (table) params.set('table', table);
  if (kind) params.set('kind', kind);
  if (stab) params.set('stability', stab);
  if (q) params.set('q', q);
  const data = await api(`/api/tasks/${taskId}/diffs?${params}`);
  state.diffTotal = data.total;
  const pages = Math.max(1, Math.ceil(data.total / state.diffPageSize));
  $('#diff-page').textContent = `第 ${state.diffPage + 1} / ${pages} 页 · 共 ${fmt(data.total)} 条`;
  msg($('#diff-meta'), data.total === 0 ? '没有符合条件的差异。' : '');

  const tableEl = $('#diff-table-view');
  tableEl.innerHTML = '';
  tableEl.append(el('thead', {}, el('tr', {},
    ['表', '类型', '主键', '不同列', '源值 → 目标值', '稳定性', '说明'].map((h) => el('th', { text: h })),
  )));
  const tbody = el('tbody');
  for (const d of data.diffs || []) {
    const cols = (d.columns || []).map((c) =>
      el('div', {}, el('span', { class: 'mono', text: c.column }), '：',
        el('span', { class: 'mono diff-old', text: c.source }), ' → ',
        el('span', { class: 'mono diff-new', text: c.target })));
    tbody.append(el('tr', {},
      el('td', { class: 'mono', text: d.table_name }),
      el('td', {}, el('span', { class: `pill ${d.kind === 'modified' ? 'warn' : 'bad'}`, text: KIND_LABEL[d.kind] || d.kind }),
        el('div', { class: 'sub', text: KIND_HINT[d.kind] || '' })),
      el('td', { class: 'mono', text: d.pk }),
      el('td', { class: 'mono', text: (d.columns || []).map((c) => c.column).join(', ') || (d.kind === 'modified' ? '' : '整行') }),
      el('td', {}, cols.length ? cols : el('span', { class: 'sub',
        text: d.kind === 'missing' ? '目标库缺少此整行' : d.kind === 'extra' ? '目标库多出此整行' : '—' })),
      el('td', {}, el('span', { class: `pill ${d.stability === 'stable' ? 'ok' : d.stability === 'transient' ? 'warn' : ''}`, text: STAB_LABEL[d.stability] || d.stability })),
      el('td', { class: 'sub', text: d.note || '' }),
    ));
  }
  if (!(data.diffs || []).length) tbody.append(el('tr', {}, el('td', { colspan: 7, class: 'empty', text: '无数据' })));
  tableEl.append(tbody);
}

// ---- repairs ------------------------------------------------------------
async function initRepairTab() {
  const taskSel = $('#repair-task');
  const prev = taskSel.value || (state.selectedTask ? String(state.selectedTask) : '');
  taskSel.innerHTML = '';
  for (const t of state.tasks.length ? state.tasks : await api('/api/tasks')) {
    taskSel.append(el('option', { value: String(t.id), text: `#${t.id} ${t.name}` }));
  }
  if (prev) taskSel.value = prev;
  await loadRepairTables();
  await loadRepairHistory();
}
async function loadRepairTables() {
  const taskId = $('#repair-task').value;
  const sel = $('#repair-table');
  sel.innerHTML = '';
  if (!taskId) return;
  const tables = (await api(`/api/tasks/${taskId}/tables`)) || [];
  for (const t of tables) {
    if (t.diffs > 0) sel.append(el('option', { value: t.table_name, text: `${t.table_name} (${t.diffs})` }));
  }
}
$('#repair-task').addEventListener('change', async () => { await loadRepairTables(); await loadRepairHistory(); });

$('#repair-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const taskId = $('#repair-task').value;
  const table = $('#repair-table').value;
  if (!taskId || !table) { alert('请选择任务与表'); return; }
  const kinds = $('#repair-kinds').value.split(',').filter(Boolean);
  try {
    const r = await api(`/api/tasks/${taskId}/repairs`, {
      method: 'POST',
      body: { table, kinds, include_transient: $('#repair-transient').checked },
    });
    const rep = r.repair;
    state.previewRepair = rep;
    $('#repair-preview').classList.remove('hidden');
    $('#repair-count').textContent = `${rep.statements} 条语句 · ${rep.kinds || '无差异'}`;
    $('#repair-sql').textContent = rep.sql || '（没有需要修复的差异）';
    $('#repair-undo').textContent = rep.undo_sql || '（无可回滚语句）';
    const warnings = r.warnings || [];
    msg($('#repair-msg'), warnings.join('；'), warnings.length ? 'error' : 'ok');
    $('#repair-execute').disabled = rep.statements === 0;
    await loadRepairHistory();
  } catch (err) {
    msg($('#repair-msg'), err.message, 'error');
  }
});

$('#repair-execute').addEventListener('click', async () => {
  const rep = state.previewRepair;
  if (!rep || !rep.id) { msg($('#repair-msg'), '先预览并保存批次。', 'error'); return; }
  if (!confirm(`将在目标库执行 ${rep.statements} 条语句，且不可自动撤销（可手工执行回滚 SQL）。继续？`)) return;
  try {
    const r = await api(`/api/repairs/${rep.id}/execute`, { method: 'POST', body: { confirm: 'apply' } });
    msg($('#repair-msg'), `执行完成：${r.statements_applied} 条语句已提交`, 'ok');
    await loadRepairHistory();
  } catch (err) {
    msg($('#repair-msg'), err.message, 'error');
  }
});

$('#repair-download').addEventListener('click', () => {
  const rep = state.previewRepair;
  if (!rep) return;
  const blob = new Blob([rep.sql + (rep.undo_sql ? `\n\n-- ---- undo ----\n${rep.undo_sql}` : '')], { type: 'text/plain' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = `repair-${rep.table_name || 'table'}-${rep.id || 'preview'}.sql`;
  a.click();
  URL.revokeObjectURL(a.href);
});

async function loadRepairHistory() {
  const taskId = $('#repair-task').value;
  const list = $('#repair-list');
  list.innerHTML = '';
  if (!taskId) return;
  const batches = (await api(`/api/tasks/${taskId}/repairs`)) || [];
  if (!(batches || []).length) { list.append(el('p', { class: 'empty', text: '还没有生成的修复批次。' })); return; }
  for (const b of batches) {
    list.append(el('div', { class: 'item' },
      el('div', { class: 'main' },
        el('div', { class: 'title' }, `#${b.id} ${b.table_name}`),
        el('div', { class: 'sub', text: `${b.statements} 条 · ${b.kinds || '-'} · ${b.status} · ${new Date(b.created_at).toLocaleString()}` }),
      ),
      el('div', { class: 'row' },
        el('button', { class: 'ghost', onclick: () => viewRepair(b.id), text: '查看 SQL' }),
        el('button', { class: 'ghost', disabled: b.status !== 'preview', onclick: () => executeRepair(b.id), text: '执行' }),
      ),
    ));
  }
}

async function viewRepair(id) {
  const rep = await api(`/api/repairs/${id}`);
  state.previewRepair = rep;
  $('#repair-preview').classList.remove('hidden');
  $('#repair-count').textContent = `${rep.statements} 条语句 · ${rep.kinds || '无差异'}`;
  $('#repair-sql').textContent = rep.sql;
  $('#repair-undo').textContent = rep.undo_sql || '（无）';
  $('#repair-execute').disabled = rep.statements === 0 || rep.status !== 'preview';
}

async function executeRepair(id) {
  if (!confirm(`执行修复批次 #${id}？`)) return;
  try {
    const r = await api(`/api/repairs/${id}/execute`, { method: 'POST', body: { confirm: 'apply' } });
    alert(`执行完成：${r.statements_applied} 条语句已提交`);
    await loadRepairHistory();
  } catch (err) {
    alert(err.message);
  }
}

// ---- report -------------------------------------------------------------
async function initReportTab() {
  const sel = $('#report-task');
  const prev = sel.value || (state.selectedTask ? String(state.selectedTask) : '');
  sel.innerHTML = '';
  for (const t of state.tasks.length ? state.tasks : await api('/api/tasks')) {
    sel.append(el('option', { value: String(t.id), text: `#${t.id} ${t.name}` }));
  }
  if (prev) sel.value = prev;
  await loadReport();
}
$('#report-task').addEventListener('change', loadReport);
$('#report-refresh').addEventListener('click', loadReport);

async function loadReport() {
  const id = $('#report-task').value;
  if (!id) return;
  $('#report-csv').href = `/api/tasks/${id}/report.csv`;
  const rep = await api(`/api/tasks/${id}/report`);
  const summary = $('#report-summary');
  summary.innerHTML = '';
  const stat = (k, v, cls) => el('div', { class: `stat ${cls || ''}` }, el('div', { class: 'v', text: v }), el('div', { class: 'k', text: k }));
  summary.append(
    stat('表总数', fmt(rep.total_tables)),
    stat('一致', fmt(rep.equal_tables), 'ok'),
    stat('有差异', fmt(rep.diff_tables), rep.diff_tables ? 'bad' : 'ok'),
    stat('失败', fmt(rep.failed_tables), rep.failed_tables ? 'bad' : ''),
    stat('源行数', fmt(rep.source_rows)),
    stat('目标行数', fmt(rep.target_rows)),
    stat('差异总数', fmt(rep.total_diffs), rep.total_diffs ? 'bad' : 'ok'),
    stat('耗时', dur(rep.duration_ms)),
    stat('期间有写入的表', fmt(rep.tables_with_source_writes), rep.tables_with_source_writes ? 'warn' : ''),
  );

  const table = $('#report-table');
  table.innerHTML = '';
  table.append(el('thead', {}, el('tr', {},
    ['表', '状态', '源行数', '目标行数', '差异', '多出', '缺失', '内容不同', '稳定', '期间变动', '区间数', '耗时', '错误'].map((h) => el('th', { text: h })),
  )));
  const tbody = el('tbody');
  for (const t of rep.tables) {
    tbody.append(el('tr', {},
      el('td', { class: 'mono', text: t.table_name }),
      el('td', {}, statusPill(t.status)),
      el('td', { class: 'num', text: fmt(t.source_rows) }),
      el('td', { class: 'num', text: fmt(t.target_rows) }),
      el('td', { class: 'num', text: fmt(t.diffs) }),
      el('td', { class: 'num', text: fmt(t.missing) }),
      el('td', { class: 'num', text: fmt(t.extra) }),
      el('td', { class: 'num', text: fmt(t.modified) }),
      el('td', { class: 'num', text: fmt(t.stable) }),
      el('td', { class: 'num', text: fmt(t.transient) }),
      el('td', { class: 'num', text: fmt(t.chunks_total) }),
      el('td', { class: 'num', text: dur(t.duration_ms) }),
      el('td', { class: 'sub', text: t.error || '' }),
    ));
  }
  table.append(tbody);
}

// ---- boot ---------------------------------------------------------------
(async function boot() {
  await pollHealth();
  setInterval(pollHealth, 5000);
  const hash = (location.hash || '').replace('#', '');
  activateTab(['connections', 'tasks', 'diffs', 'repairs', 'report'].includes(hash) ? hash : 'connections');
})();
