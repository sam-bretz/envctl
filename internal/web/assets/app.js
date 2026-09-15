(() => {
  'use strict';

  const token = document.querySelector('meta[name="envctl-token"]').content;
  const $ = (id) => document.getElementById(id);
  const TABS = ['Chat', 'Result', 'Changes', 'Log'];

  const ui = {
    state: null,
    runId: null,
    stage: null,
    stagePicked: false,
    tab: 'Chat',
    revision: null, // a historical revision being viewed; null is current
    diff: { key: null, data: null, error: null, loading: false, from: '' },
    reader: null,
    link: 'connecting',
    pending: false,
    menu: false,
  };

  // ---------- DOM helpers ----------

  function h(tag, props, ...children) {
    const el = document.createElement(tag);
    for (const [key, value] of Object.entries(props || {})) {
      if (value === undefined || value === null || value === false) continue;
      if (key === 'class') el.className = value;
      else if (key === 'text') el.textContent = value;
      else if (key.startsWith('on')) el.addEventListener(key.slice(2), value);
      else if (key in el && !key.includes('-')) el[key] = value;
      else el.setAttribute(key, value === true ? '' : value);
    }
    for (const child of children.flat(Infinity)) {
      if (child === null || child === undefined || child === false) continue;
      el.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return el;
  }

  // ---------- formatting ----------

  const zeroTime = (t) => !t || t.startsWith('0001-');
  function ago(t) {
    if (zeroTime(t)) return '';
    const s = Math.round((Date.now() - new Date(t).getTime()) / 1000);
    const abs = Math.abs(s);
    if (abs < 45) return s >= 0 ? 'just now' : 'in a moment';
    const span = abs < 3600 ? `${Math.round(abs / 60)} min` : abs < 86400 ? `${Math.round(abs / 3600)} h` : `${Math.round(abs / 86400)} d`;
    return s >= 0 ? `${span} ago` : `in ${span}`;
  }
  const when = (t) => (zeroTime(t) ? '' : new Date(t).toLocaleString());
  const timeEl = (t) => h('time', { datetime: t, title: when(t), text: ago(t) });
  const duration = (sec) => (sec >= 3600 && sec % 3600 === 0 ? `${sec / 3600} h` : sec >= 60 ? `${Math.round(sec / 60)} min` : `${sec} s`);
  const shortId = (id) => (id ? id.replace(/^([a-z]+_[0-9a-f]{6})[0-9a-f]+$/, '$1') : '');
  const bytes = (n) => (n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : n >= 1024 ? `${Math.round(n / 1024)} KB` : `${n} bytes`);
  const sha = (s) => (s ? s.slice(0, 7) : '');
  const model = (m) => m || 'harness default';
  const tokens = (n) => (n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${Math.round(n / 1e3)}K` : String(n));
  const usageTokens = (u) => (u.input || 0) + (u.cache_write || 0) + (u.cache_read || 0) + (u.output || 0);
  const plural = (n, word) => `${n} ${n === 1 ? word : word.endsWith('y') ? `${word.slice(0, -1)}ies` : `${word}s`}`;

  const STATUS = {
    'done': ['done', 'Done'],
    'checkpointed': ['done', 'Accepted'],
    'historical': ['', 'Historical'],
    'archived': ['', 'Archived'],
    'pending': ['', 'Not started'],
    'preparing': ['run', 'Preparing'],
    'running': ['run', 'Running'],
    'verifying': ['run', 'Checking the result'],
    'awaiting-approval': ['needs', 'Needs your approval'],
    'approved, publishing': ['run', 'Publishing'],
    'approved, not published': ['bad', 'Not published'],
    'recovering': ['needs', 'Recovering'],
    'failed': ['bad', 'Failed'],
    'cancelled': ['bad', 'Cancelled'],
  };
  const tone = (s) => (STATUS[s] || [''])[0];
  const label = (s) => (STATUS[s] || [null, s])[1];

  // ---------- data access ----------

  async function api(path, body) {
    const init = { headers: {} };
    if (body !== undefined) {
      init.method = 'POST';
      init.headers['Content-Type'] = 'application/json';
      init.headers['X-Envctl-Token'] = token;
      init.body = JSON.stringify(body);
    }
    const res = await fetch(path, init);
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
    return data;
  }

  const runs = () => (ui.state ? ui.state.runs : []);
  const GROUPS = [['attention', 'Needs you'], ['running', 'Running'], ['finished', 'Finished']];
  function ordered() {
    const out = [];
    for (const [lane] of GROUPS) out.push(...runs().filter((r) => r.lane === lane).sort((a, b) => b.updated_at.localeCompare(a.updated_at)));
    return out;
  }
  const currentRun = () => runs().find((r) => r.id === ui.runId) || null;
  const currentRevision = (run) => run && run.revisions.find((v) => v.id === run.current_revision);
  const viewRevision = (run) => (run && ui.revision && run.revisions.find((v) => v.id === ui.revision)) || currentRevision(run);
  const viewingHistory = (run) => !!(run && ui.revision && ui.revision !== run.current_revision);
  const currentStage = (rev) => (rev && (rev.stages.find((s) => s.id === ui.stage) || rev.stages[0])) || null;
  const finished = (run) => ['completed', 'cancelled', 'superseded'].includes(run.state);
  const resultOf = (stage) => (stage.checkpoint ? stage.checkpoint.result : stage.awaiting ? stage.awaiting.result : null);

  function defaultStage(rev) {
    if (!rev || !rev.stages.length) return null;
    const pick = (fn) => rev.stages.find(fn);
    const active = ['running', 'preparing', 'verifying', 'approved, publishing', 'approved, not published', 'failed', 'recovering'];
    return (pick((s) => s.awaiting) || pick((s) => active.includes(s.status)) || [...rev.stages].reverse().find((s) => s.status === 'done') || rev.stages[0]).id;
  }

  function ensureSelection() {
    if (!ui.state) return;
    if (!currentRun()) {
      const all = ordered();
      ui.runId = all.length ? all[0].id : null;
      ui.revision = null;
      ui.stagePicked = false;
    }
    const run = currentRun();
    if (run && ui.revision && !run.revisions.some((v) => v.id === ui.revision)) ui.revision = null;
    const rev = viewRevision(run);
    if (rev && (!ui.stagePicked || !rev.stages.some((s) => s.id === ui.stage))) ui.stage = defaultStage(rev);
  }

  function writeHash() {
    if (!ui.state) return;
    const p = new URLSearchParams();
    if (ui.runId) p.set('run', ui.runId);
    if (ui.stage && ui.stagePicked) p.set('stage', ui.stage);
    if (ui.tab !== 'Chat') p.set('view', ui.tab.toLowerCase());
    if (ui.revision) p.set('revision', ui.revision);
    history.replaceState(null, '', p.toString() ? `#${p}` : location.pathname);
  }
  function readHash() {
    const p = new URLSearchParams(location.hash.slice(1));
    ui.runId = p.get('run');
    ui.stage = p.get('stage');
    ui.stagePicked = !!ui.stage;
    ui.revision = p.get('revision');
    const view = TABS.find((t) => t.toLowerCase() === p.get('view'));
    ui.tab = view || 'Chat';
  }

  function select(changes) {
    if ('runId' in changes && changes.runId !== ui.runId) {
      ui.revision = null;
      ui.stagePicked = false;
    }
    if ('stage' in changes && !('stagePicked' in changes)) ui.stagePicked = true;
    Object.assign(ui, changes);
    ui.menu = false;
    closeSide();
    render();
  }

  // ---------- feedback ----------

  function say(text, error) {
    const el = h('div', { class: `toast${error ? ' err' : ''}`, text });
    $('toasts').append(el);
    setTimeout(() => el.remove(), error ? 9000 : 3500);
  }

  // ---------- actions ----------

  async function act(run, body, done) {
    if (viewingHistory(run)) {
      say('This is an older revision. Return to the current one to make changes.', true);
      return false;
    }
    try {
      const updated = await api(`/api/runs/${encodeURIComponent(run.id)}/actions`, {
        operation_id: crypto.randomUUID(), expected_version: run.version, revision: run.current_revision, ...body,
      });
      if (updated && updated.id) replaceRun(updated);
      if (done) say(done);
      return true;
    } catch (err) {
      say(err.message, true);
      return false;
    }
  }
  function replaceRun(run) {
    if (!ui.state) return;
    const i = ui.state.runs.findIndex((r) => r.id === run.id);
    if (i >= 0) ui.state.runs[i] = run;
    else ui.state.runs.push(run);
    render();
  }

  function openPullRequests(run) {
    if (!run || !run.pull_requests.length) return say('No pull request yet. It opens after you approve the change.', true);
    for (const pr of run.pull_requests) window.open(pr.url, '_blank', 'noopener');
  }

  function approveKey(run) {
    const rev = currentRevision(run);
    if (!rev) return;
    const selected = rev.stages.find((s) => s.id === ui.stage);
    if (selected && selected.awaiting && !viewingHistory(run)) return approveDialog(run, selected);
    const waiting = rev.stages.find((s) => s.awaiting);
    if (waiting) {
      select({ revision: null, stage: waiting.id, tab: 'Result' });
      return say(`Review ${waiting.id}, then approve it.`);
    }
    if (rev.blocked) return say(`${rev.blocked.node} is already approved, but the pull request was not opened.`, true);
    say('Nothing in this run needs approval.', true);
  }

  // What the run needs, as one sentence and at most one action.
  function runStatus(run) {
    const rev = currentRevision(run);
    const waiting = rev.stages.find((s) => s.awaiting);
    const plan = rev.stages.find((s) => s.kind === 'plan');
    if (waiting) {
      return { tone: 'needs', pill: 'Needs you', text: `${waiting.id} is waiting for your approval.`,
        action: { label: 'Review and approve', cls: 'btn-needs', fn: () => { select({ revision: null, stage: waiting.id, tab: 'Result' }); } } };
    }
    if (rev.blocked) {
      return { tone: 'bad', pill: 'Blocked', text: `Approved, but the pull request was not opened: ${rev.blocked.detail}`,
        action: { label: `Rewind ${rev.blocked.node}`, cls: 'btn-primary', fn: () => rewindDialog(run, rev.blocked.node) } };
    }
    if (rev.stalled) {
      return { tone: 'bad', pill: 'Blocked', text: `${rev.stalled.replace(/^[a-z.]+: /, '')}. No stage can start until this passes.`,
        action: plan ? { label: `Rewind to ${plan.id}`, cls: 'btn-primary', fn: () => rewindDialog(run, plan.id) } : null };
    }
    if (rev.state === 'needs-attention') {
      return { tone: 'bad', pill: 'Needs attention', text: rev.recovery ? rev.recovery.detail : 'A stage used all its attempts or the run reached its token limit.',
        action: { label: 'Rewind', cls: 'btn-primary', fn: () => rewindDialog(run) } };
    }
    if (rev.recovery) {
      return { tone: 'needs', pill: 'Recovering', text: `${rev.recovery.detail}. Retrying ${ago(rev.recovery.retry_at)}.`, action: null };
    }
    if (run.state === 'completed') {
      return { tone: 'ok', pill: 'Completed', text: run.pull_requests.length ? 'The change was approved and its pull request is open.' : 'Every stage is done.',
        action: run.pull_requests.length ? { label: run.pull_requests.length > 1 ? 'Open pull requests' : 'Open pull request', cls: 'btn-primary', fn: () => openPullRequests(run) } : null };
    }
    if (run.state === 'cancelled' || run.state === 'superseded') return { tone: '', pill: run.state === 'cancelled' ? 'Cancelled' : 'Superseded', text: 'Nothing is running. Checkpoints and artifacts stay available.', action: null };
    if (run.state === 'queued' || run.state === 'preparing') return { tone: 'run', pill: 'Starting', text: 'Preparing the run’s VM and cloning the repository.', action: null };
    const active = rev.stages.find((s) => ['running', 'preparing', 'verifying', 'approved, publishing'].includes(s.status));
    if (active) {
      const attempt = active.attempts[active.attempts.length - 1];
      const phase = attempt && attempt.progress ? attempt.progress.phase : '';
      const who = { worker: 'The worker is on it', supervisor: 'The supervisor is reviewing', checks: 'Checks are running', stack: 'Starting services' }[phase] || label(active.status);
      return { tone: 'run', pill: 'Running', text: `${active.id}: ${who}.`, action: null };
    }
    return { tone: 'run', pill: 'Running', text: 'Waiting for the next stage to start.', action: null };
  }

  // ---------- dialogs ----------

  function dialog(title, lede, build, submitLabel, onSubmit, opts = {}) {
    const dlg = $('dialog');
    const form = $('dialog-form');
    const error = h('p', { class: 'error-text', role: 'alert' });
    const submit = h('button', { type: 'submit', class: `btn ${opts.cls || 'btn-primary'}`, text: submitLabel });
    form.replaceChildren(
      h('h2', { text: title }),
      lede ? h('p', { class: 'lede', text: lede }) : null,
      build(submit),
      error,
      h('div', { class: 'buttons' }, h('button', { type: 'button', class: 'btn btn-quiet', text: opts.cancelLabel || 'Cancel', onclick: () => dlg.close() }), submit),
    );
    form.onsubmit = async (e) => {
      e.preventDefault();
      error.textContent = '';
      submit.disabled = true;
      try {
        if ((await onSubmit()) !== false) dlg.close();
      } catch (err) {
        error.textContent = err.message;
      } finally {
        submit.disabled = false;
      }
    };
    dlg.showModal();
    (form.querySelector('textarea, input, select') || submit).focus();
  }
  const field = (name, control, hint) => h('label', { class: 'field' }, h('span', { text: name }), control, hint ? h('span', { class: 'hint', text: hint }) : null);

  async function newRunDialog() {
    let choices;
    try {
      choices = await api('/api/workflows');
    } catch (err) {
      return say(err.message, true);
    }
    const task = h('textarea', { rows: 6, placeholder: 'What should the agents do? Include requirements and how to check the work.' });
    const name = h('input', { type: 'text', placeholder: 'Uses the first line of the task' });
    const ref = h('input', { type: 'text', placeholder: 'https://github.com/owner/repo/issues/123' });
    const flows = choices.workflows;
    const pick = h('select', {}, flows.map((w) => h('option', { value: w.name, text: w.name === 'default' ? 'Default' : w.name })));
    const stages = h('span', { class: 'hint' });
    const show = () => { stages.textContent = (flows.find((w) => w.name === pick.value) || { stages: [] }).stages.join(' → '); };
    pick.addEventListener('change', show);
    show();
    dialog('Start a run', `Agents work from the envctl.yaml in ${choices.root}, in their own VM, and ask you before anything is published.`,
      () => h('div', { class: 'fields' }, field('Task', task), h('label', { class: 'field' }, h('span', { text: 'Workflow' }), pick, stages), field('Name', name), field('Issue link', ref, 'Optional')),
      'Start run', async () => {
        if (!task.value.trim()) throw new Error('Describe the task first.');
        const run = await api('/api/runs', { operation_id: crypto.randomUUID(), task: task.value, name: name.value.trim(), task_ref: ref.value.trim(), workflow: pick.value });
        replaceRun(run);
        select({ runId: run.id, tab: 'Chat' });
        say(`Started ${run.name}`);
      });
  }

  function rewindDialog(run, target) {
    if (viewingHistory(run)) return say('This is an older revision. Return to the current one to rewind.', true);
    const rev = currentRevision(run);
    const stage = h('select', {}, rev.stages.map((s) => h('option', { value: s.id, text: s.id, selected: s.id === (target || ui.stage) })));
    const objective = h('textarea', { rows: 7, value: rev.objective });
    let submitButton;
    const relabel = () => { if (submitButton) submitButton.textContent = `Rewind to ${stage.value}`; };
    stage.addEventListener('change', relabel);
    dialog('Rewind this run', 'Starts a new revision from the stage you pick. Earlier checkpoints are kept; that stage and the ones after it run again. Changing the objective runs every stage again.', (submit) => {
      submitButton = submit;
      relabel();
      return h('div', { class: 'fields' }, field('From stage', stage), field('Objective', objective));
    }, 'Rewind', async () => {
      const ok = await act(run, { action: 'rewind', node: stage.value, task: objective.value }, `Rewound to ${stage.value}`);
      if (ok) select({ revision: null, stage: stage.value, tab: 'Chat' });
      return ok;
    });
  }

  function approveDialog(run, stage) {
    const a = stage.awaiting;
    const attempt = stage.attempts.find((x) => x.id === a.attempt);
    const publishes = stage.kind === 'change';
    dialog(publishes ? 'Approve and publish?' : `Approve ${stage.id}?`,
      `This approves attempt ${attempt ? attempt.number : ''} of ${stage.id} exactly as shown. ${publishes ? 'envctl then opens the pull request.' : 'The next stages can start after it.'}`,
      () => h('p', { class: 'hint faint' }, 'Result ', h('code', { text: a.work_digest.slice(0, 16) })),
      publishes ? 'Approve and publish' : 'Approve',
      () => act(run, { action: 'approve', node: stage.id, attempt: a.attempt, work_digest: a.work_digest }, publishes ? `Approved ${stage.id}. Opening the pull request.` : `Approved ${stage.id}`),
      { cls: 'btn-needs' });
  }

  function cancelDialog(run) {
    dialog('Cancel this run?', 'Running agents stop and the VM is released. Checkpoints, artifacts and pull requests stay available.',
      () => h('span'), 'Cancel run', () => act(run, { action: 'cancel' }, `Cancelled ${run.name}`), { cls: 'btn-danger', cancelLabel: 'Keep running' });
  }

  function priorityDialog(run) {
    const input = h('input', { type: 'number', value: run.priority, step: 1 });
    dialog('Set priority', 'Higher priority runs get VMs and parallel slots first.', () => field('Priority', input),
      'Save', () => act(run, { action: 'priority', priority: Number(input.value) || 0 }, 'Priority saved'));
  }

  function helpDialog() {
    const keys = [
      ['j  k', 'Next or previous run'], ['h  l', 'Previous or next stage'], ['1 – 4', 'Chat, Result, Changes, Log'],
      ['[  ]', 'Older or newer revision'], ['i', 'Write a message'], ['s', 'Switch between supervisor and worker'],
      ['n', 'Start a run'], ['a', 'Approve'], ['r', 'Rewind'], ['x', 'Cancel the run'], ['g', 'Open pull requests'],
      ['o', 'Open the stage’s first document'], ['d', 'Show changes'], ['Esc', 'Close'],
    ];
    dialog('Keyboard shortcuts', null, () => h('div', { class: 'keys' }, keys.map(([k, d]) => [h('kbd', { text: k }), h('span', { text: d })])), 'Done', () => true, { cancelLabel: 'Close' });
  }

  // ---------- artifact reader ----------

  async function openReader(files, index, context) {
    ui.reader = { files, index, context };
    const file = files[index];
    $('reader').hidden = false;
    $('reader-title').textContent = file.name;
    $('reader-meta').textContent = [context, file.size ? bytes(file.size) : null].filter(Boolean).join(', ');
    $('reader-files').hidden = files.length < 2;
    $('reader-files').replaceChildren(...files.map((f, i) => h('button', { type: 'button', 'aria-current': i === index ? 'true' : 'false', text: f.name, onclick: () => openReader(files, i, context) })));
    const body = $('reader-body');
    body.replaceChildren(h('p', { class: 'empty', text: 'Loading…' }));
    try {
      const art = await api(`/api/artifacts/${encodeURIComponent(file.digest)}?media_type=${encodeURIComponent(file.media_type || '')}`);
      if (!ui.reader || ui.reader.files[ui.reader.index] !== file) return;
      const parts = [];
      if (art.truncated) parts.push(h('p', { class: 'empty', text: `Showing the first 2 MB of ${bytes(art.size)}. Export it with envctl run artifact ${file.digest} --output <file>.` }));
      if (art.images && art.images.length) parts.push(h('div', { class: 'shots' }, art.images.map((src) => h('img', { src, alt: 'Browser screenshot recorded by a check' }))));
      if (art.kind === 'markdown') parts.push(h('article', { class: 'doc' }, sanitized(art.html)));
      else if (art.kind === 'binary') parts.push(h('p', { class: 'empty', text: `This artifact is binary. Export it with envctl run artifact ${file.digest} --output <file>.` }));
      else parts.push(h('pre', { class: 'plain', text: art.text }));
      body.replaceChildren(...parts);
      body.scrollTop = 0;
    } catch (err) {
      body.replaceChildren(h('p', { class: 'error-text', text: err.message }));
    }
  }
  function closeReader() {
    ui.reader = null;
    $('reader').hidden = true;
  }

  // The server renders Markdown without raw HTML; this allowlist is a second
  // layer, rebuilt from an inert document so nothing in it can run.
  const ALLOWED = new Set(['A', 'P', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6', 'UL', 'OL', 'LI', 'PRE', 'CODE', 'BLOCKQUOTE', 'EM', 'STRONG', 'DEL', 'HR', 'BR', 'TABLE', 'THEAD', 'TBODY', 'TR', 'TH', 'TD', 'INPUT', 'IMG', 'SPAN']);
  function sanitized(html) {
    const doc = new DOMParser().parseFromString(html, 'text/html');
    const copy = (node) => {
      if (node.nodeType === Node.TEXT_NODE) return document.createTextNode(node.nodeValue);
      if (node.nodeType !== Node.ELEMENT_NODE) return null;
      if (!ALLOWED.has(node.tagName)) {
        const frag = document.createDocumentFragment();
        for (const child of node.childNodes) { const c = copy(child); if (c) frag.append(c); }
        return frag;
      }
      const el = document.createElement(node.tagName.toLowerCase());
      if (node.tagName === 'A') {
        const href = node.getAttribute('href') || '';
        if (/^(https?:|mailto:|#)/i.test(href)) { el.href = href; el.target = '_blank'; el.rel = 'noopener noreferrer'; }
      } else if (node.tagName === 'IMG') {
        const src = node.getAttribute('src') || '';
        if (!/^data:image\/(png|jpeg|gif|webp);/i.test(src)) return document.createTextNode(node.getAttribute('alt') || '');
        el.src = src; el.alt = node.getAttribute('alt') || '';
      } else if (node.tagName === 'INPUT') {
        if (node.getAttribute('type') !== 'checkbox') return null;
        el.type = 'checkbox'; el.disabled = true; el.checked = node.hasAttribute('checked');
      }
      if (node.hasAttribute('align')) el.setAttribute('align', node.getAttribute('align'));
      for (const child of node.childNodes) { const c = copy(child); if (c) el.append(c); }
      return el;
    };
    const frag = document.createDocumentFragment();
    for (const child of doc.body.childNodes) { const c = copy(child); if (c) frag.append(c); }
    return frag;
  }

  // ---------- rendering ----------

  function render() {
    // Live updates wait while text is selected, so copying is not interrupted.
    const sel = window.getSelection();
    if (sel && !sel.isCollapsed && $('page').contains(sel.anchorNode)) {
      ui.pending = true;
      return;
    }
    ui.pending = false;
    ensureSelection();
    writeHash();
    renderSide();
    renderPage();
  }

  function renderSide() {
    const st = ui.state;
    $('root').textContent = st ? st.root.replace(/^\/Users\/[^/]+/, '~') : '';
    const link = $('link');
    link.className = 'link';
    if (ui.link === 'ok' && st && !st.error) link.textContent = 'Live';
    else if (st && st.error) { link.textContent = 'Coordinator offline'; link.classList.add('lost'); }
    else { link.textContent = ui.link === 'connecting' ? 'Connecting' : 'Reconnecting'; link.classList.add('lost'); }

    const groups = [];
    for (const [lane, title] of GROUPS) {
      const items = ordered().filter((r) => r.lane === lane);
      if (!items.length) continue;
      groups.push(h('section', { class: 'group' }, h('h2', {}, title, h('span', { text: String(items.length) })), items.map(runRow)));
    }
    if (!groups.length) groups.push(h('p', { class: 'empty-list', text: st ? 'No runs yet.' : 'Loading…' }));
    $('runs').replaceChildren(...groups);
  }

  function runRow(run) {
    const status = runStatus(run);
    const rev = currentRevision(run);
    let sub = run.lane === 'attention' ? status.text.replace(/\.$/, '') : null;
    if (!sub && run.lane === 'running') {
      const active = rev.stages.find((s) => ['running', 'preparing', 'verifying', 'approved, publishing'].includes(s.status));
      sub = active ? `${active.id}, ${label(active.status).toLowerCase()}` : status.pill;
    }
    if (!sub) sub = `${status.pill} ${ago(run.updated_at)}`;
    const dot = run.lane === 'attention' ? (status.tone === 'bad' ? 'bad' : 'needs') : run.lane === 'running' ? 'run' : run.state === 'completed' ? 'ok' : 'idle';
    return h('button', { type: 'button', class: `run-row ${run.lane === 'attention' ? 'needs' : ''}`, 'aria-current': run.id === ui.runId ? 'true' : 'false', onclick: () => select({ runId: run.id }) },
      h('span', { class: `dot ${dot}` }), h('span', { class: 'run-name', text: run.name }), h('span', { class: 'run-sub', text: sub }));
  }

  function renderPage() {
    const run = currentRun();
    const regions = ['history-note', 'head', 'pipeline', 'stage-head', 'tabs', 'panel', 'rail'];
    $('topbar-title').textContent = run ? run.name : 'envctl';
    if (!run) {
      for (const id of regions) $(id).replaceChildren();
      $('composer').hidden = true;
      $('head').append(welcome());
      return;
    }
    const rev = viewRevision(run);
    const stage = currentStage(rev);
    renderHistoryNote(run, rev);
    renderHead(run, rev);
    renderPipeline(rev, stage);
    renderStageHead(rev, stage);
    renderTabs(run, rev, stage);
    renderPanel(run, rev, stage);
    renderRail(run, rev, stage);
    $('composer').hidden = ui.tab !== 'Chat' || viewingHistory(run) || finished(run);
    $('message').placeholder = `Message the ${$('recipient').value} of ${stage ? stage.id : 'this run'}`;
  }

  function welcome() {
    const st = ui.state;
    if (st && st.error) return h('div', { class: 'welcome' }, h('h1', { text: 'The coordinator is offline' }), h('p', { text: 'Start it with envctl daemon serve, or run any envctl run command. This page reconnects on its own.' }));
    if (!st) return h('div', { class: 'welcome' }, h('p', { text: 'Connecting to envctl…' }));
    return h('div', { class: 'welcome' },
      h('h1', { text: 'Start your first run' }),
      h('p', { text: 'Describe a change. Agents plan, build and check it in their own VM, and ask you before anything is published.' }),
      h('button', { type: 'button', class: 'btn btn-primary', text: 'New run', onclick: newRunDialog }));
  }

  function renderHistoryNote(run, rev) {
    const note = $('history-note');
    if (!viewingHistory(run)) return note.replaceChildren();
    note.replaceChildren(h('div', { class: 'history-note' },
      h('span', {}, 'You are looking at revision ', h('code', { text: shortId(rev.id) }), ` from ${ago(rev.created_at)}. It can’t be changed.`),
      h('button', { type: 'button', class: 'btn btn-small', text: 'Back to current', onclick: () => select({ revision: null, stagePicked: false }) })));
  }

  function renderHead(run, rev) {
    const status = runStatus(run);
    const actions = [];
    if (status.action && !viewingHistory(run)) actions.push(h('button', { type: 'button', class: `btn ${status.action.cls}`, text: status.action.label, onclick: status.action.fn }));
    actions.push(moreMenu(run));
    const meta = [
      h('span', { text: `${rev.workflow === 'default' ? 'Default' : rev.workflow} workflow` }),
      h('span', {}, 'Started ', timeEl(run.created_at)),
      h('span', { text: `${tokens(run.usage.tokens)} tokens` }),
    ];
    if (run.revisions.length > 1) meta.push(h('span', { text: plural(run.revisions.length, 'revision') }));
    if (run.task_ref) meta.push(h('a', { href: run.task_ref, target: '_blank', rel: 'noopener noreferrer', text: 'Issue' }));
    for (const pr of run.pull_requests) meta.push(h('a', { href: pr.url, target: '_blank', rel: 'noopener noreferrer', text: `Pull request ${pr.url.split('/').pop()}` }));
    const open = $('head').querySelector('details.objective');
    $('head').replaceChildren(
      h('h1', { text: run.name }),
      h('div', { class: 'head-actions' }, actions),
      h('div', { class: 'head-status' }, h('span', { class: `pill ${status.tone}`, text: status.pill }), h('p', { text: status.text })),
      h('div', { class: 'head-meta' }, meta),
      h('details', { class: 'objective', open: open ? open.open : false }, h('summary', { text: 'Objective' }), h('p', { text: rev.objective })));
  }

  function moreMenu(run) {
    const items = [
      ['Rewind…', () => rewindDialog(run)],
      ['Set priority…', () => priorityDialog(run)],
    ];
    if (run.pull_requests.length) items.unshift(['Open pull request', () => openPullRequests(run)]);
    if (!finished(run)) items.push(['Cancel run…', () => cancelDialog(run), 'danger']);
    const wrap = h('div', { class: 'menu' },
      h('button', { type: 'button', class: 'btn btn-quiet', 'aria-haspopup': 'true', 'aria-expanded': String(ui.menu), text: 'More', onclick: (e) => { e.stopPropagation(); ui.menu = !ui.menu; renderHead(run, viewRevision(run)); } }));
    if (ui.menu) {
      wrap.append(h('div', { class: 'menu-list', role: 'menu' }, items.map(([text, fn, cls]) => h('button', { type: 'button', role: 'menuitem', class: cls || '', text, onclick: () => { ui.menu = false; render(); fn(); } }))));
    }
    return wrap;
  }

  function renderPipeline(rev, stage) {
    $('pipeline').replaceChildren(...rev.stages.map((s) => h('button', {
      type: 'button', class: `step ${tone(s.status)}`, 'aria-current': stage && s.id === stage.id ? 'true' : 'false', onclick: () => select({ stage: s.id }),
    }, h('span', { class: 'node', 'aria-hidden': 'true' }), h('span', { class: 'step-name', text: s.id }), h('span', { class: 'step-status', text: label(s.status) }))));
  }

  function renderStageHead(rev, stage) {
    if (!stage) return $('stage-head').replaceChildren();
    const l = stage.limits;
    const bits = [`${stage.kind} stage`];
    if (l.attempts) bits.push(`attempt ${l.attempts} of ${l.max_attempts}`);
    if (stage.gate === 'human' && !stage.checkpoint) bits.push('you approve the result');
    $('stage-head').replaceChildren(h('h2', { text: stage.id }), h('p', { text: bits.join(', ') }));
  }

  function renderTabs(run, rev, stage) {
    const res = stage ? resultOf(stage) : null;
    const counts = { Result: res && res.artifacts ? res.artifacts.length || '' : '' };
    $('tabs').replaceChildren(...TABS.map((name, i) => h('button', {
      type: 'button', role: 'tab', class: 'tab', 'aria-selected': ui.tab === name ? 'true' : 'false', title: `${name} (${i + 1})`,
      onclick: () => select({ tab: name }),
    }, name, counts[name] ? h('span', { class: 'count', text: String(counts[name]) }) : null)));
  }

  function renderPanel(run, rev, stage) {
    const panel = $('panel');
    const pinned = [...panel.querySelectorAll('.activity')].map((el) => el.scrollHeight - el.scrollTop - el.clientHeight < 8);
    const content = !stage ? h('p', { class: 'empty', text: 'This workflow has no stages.' })
      : { Chat: chat, Result: result, Changes: changes, Log: log }[ui.tab](run, rev, stage);
    panel.replaceChildren(content);
    panel.querySelectorAll('.activity').forEach((el, i) => { if (pinned[i] !== false) el.scrollTop = el.scrollHeight; });
  }

  // Chat: the stage's attempts, results and reviews, with your messages.
  function chat(run, rev, stage) {
    const items = [];
    const push = (at, el) => items.push({ at: at || '', el });
    for (const a of stage.attempts) {
      const models = a.models || {};
      const modelText = models.worker || models.supervisor ? ` with ${model(models.worker)}` : '';
      push(a.started_at, h('div', { class: 'event' }, h('span', {}, `Attempt ${a.number} started${modelText} `, timeEl(a.started_at))));
      const p = a.progress;
      if (a.state === 'running') {
        const phase = p ? { worker: 'The worker is working', supervisor: 'The supervisor is reviewing', checks: 'Running checks', stack: 'Starting services' }[p.phase] || p.phase : 'Starting';
        push(p ? p.updated_at : a.started_at, h('div', { class: 'working' },
          h('div', { class: 'working-head' }, h('span', { class: 'spinner', 'aria-hidden': 'true' }), h('strong', { text: phase }), p && p.detail ? h('span', { class: 'muted', text: p.detail }) : null,
            p && p.generation ? h('span', { class: 'muted', text: `resumed ${plural(p.generation, 'time')}` }) : null, p ? h('span', { class: 'faint' }, 'updated ', timeEl(p.updated_at)) : null),
          p && p.activity && p.activity.length ? h('div', { class: 'activity', text: p.activity.join('\n') }) : null));
      }
      if (a.result) {
        push(a.updated_at, message('worker', 'Worker', a.updated_at, a.result.summary, a.usage ? `${tokens(usageTokens(a.usage))} tokens` : null));
        const r = a.result.review;
        if (r && r.summary) push(a.updated_at, message('supervisor', 'Supervisor', a.updated_at, r.summary, null, r.accepted));
      }
      if (a.error) push(a.updated_at, h('div', {}, h('div', { class: 'event bad' }, h('span', { text: `Attempt ${a.number} failed` })), h('p', { class: 'failure', text: a.error })));
      if (a.approval) push(a.approval.at, h('div', { class: 'event' }, h('span', {}, `${a.approval.actor === 'local' ? 'You' : a.approval.actor} approved this result `, timeEl(a.approval.at))));
    }
    for (const m of rev.messages) {
      if (m.node && m.node !== stage.id) continue;
      push(m.created_at, h('div', { class: 'msg mine' },
        h('div', {}, h('div', { class: 'bubble', text: m.body }), h('div', { class: 'msg-foot' }, `To the ${m.recipient}${m.node ? '' : ' of every stage'}, ${m.status}`))));
    }
    if (!items.length) {
      return h('p', { class: 'empty', text: finished(run) ? `${stage.id} didn’t run in this revision.` : `${stage.id} hasn’t started yet. A message you send now reaches its agents when it does.` });
    }
    items.sort((x, y) => x.at.localeCompare(y.at));
    return h('div', { class: 'chat' }, items.map((i) => i.el));
  }

  function message(role, name, at, text, note, accepted) {
    const verdict = accepted === undefined ? null : h('span', { class: `verdict ${accepted ? 'ok' : 'bad'}`, text: accepted ? 'Accepted' : 'Asked for changes' });
    return h('div', { class: 'msg' },
      h('span', { class: `avatar ${role}${accepted === false ? ' rejected' : ''}`, 'aria-hidden': 'true', text: name[0] }),
      h('div', {}, h('div', { class: 'msg-head' }, h('strong', { text: name }), verdict, timeEl(at), note ? h('span', { text: note }) : null), h('p', { class: 'msg-body', text })));
  }

  function checksList(stage, res) {
    const results = (res && res.checks) || [];
    const names = new Set(results.map((c) => c.name));
    const pending = (stage.checks || []).filter((n) => !names.has(n));
    if (!results.length && !pending.length) return null;
    return h('ul', { class: 'checks' },
      results.map((c) => h('li', {},
        h('span', { class: `dot ${c.passed ? 'ok' : 'bad'}` }),
        h('span', { class: 'grow', text: c.name }),
        h('span', { class: `check-mark ${c.passed ? 'ok' : 'bad'}`, text: c.passed ? 'Passed' : `Failed, exit ${c.exit_code}` }),
        c.evidence_digest ? h('button', { type: 'button', class: 'btn btn-quiet btn-small', text: 'Output', onclick: () => openReader([{ name: `${c.name} output`, digest: c.evidence_digest, media_type: '', size: 0 }], 0, stage.id) }) : null)),
      pending.map((n) => h('li', {}, h('span', { class: 'dot idle' }), h('span', { class: 'grow', text: n }), h('span', { class: 'check-mark idle', text: 'Not run yet' }))));
  }

  function filesGrid(files, context) {
    if (!files || !files.length) return null;
    return h('div', { class: 'files' }, files.map((f, i) => h('button', { type: 'button', class: 'file', onclick: () => openReader(files, i, context) },
      h('strong', { text: f.name }), h('span', { text: `${/markdown/.test(f.media_type) ? 'Document' : f.media_type}, ${bytes(f.size)}` }))));
  }

  function commitFacts(res) {
    const keys = Object.keys(res.commits || {}).sort();
    if (!keys.length) return null;
    return h('dl', { class: 'facts' }, keys.map((k) => [
      h('dt', { text: k }),
      h('dd', {}, h('code', { title: res.commits[k], text: sha(res.commits[k]) }), res.prs && res.prs[k] ? [' ', h('a', { href: res.prs[k], target: '_blank', rel: 'noopener noreferrer', text: 'pull request' })] : null),
    ]));
  }
  const block = (title, content) => (content ? h('div', { class: 'block' }, title ? h('h3', { text: title }) : null, content) : null);

  function result(run, rev, stage) {
    const context = `${run.name}, ${stage.id}`;
    if (stage.awaiting) {
      const r = stage.awaiting.result;
      const publishes = stage.kind === 'change';
      return h('div', {},
        h('div', { class: 'approval' },
          h('h3', { text: publishes ? 'Ready to publish' : 'Ready for your approval' }),
          h('p', { text: publishes ? 'Read the summary, checks and changes. Approving opens the pull request with exactly this result.' : 'Read the result below. Approving lets the next stages start from it.' }),
          h('div', { class: 'row' },
            h('button', { type: 'button', class: 'btn btn-needs', disabled: viewingHistory(run), text: publishes ? 'Approve and publish' : 'Approve', onclick: () => approveDialog(run, stage) }),
            h('button', { type: 'button', class: 'btn btn-quiet', text: 'See changes', onclick: () => select({ tab: 'Changes' }) }),
            h('span', { class: 'faint' }, 'Result ', h('code', { text: stage.awaiting.work_digest.slice(0, 12) })))),
        block('Summary', h('p', { class: 'prose-sm', text: r.summary })),
        r.review && r.review.summary ? block('Supervisor', h('p', { class: 'prose-sm', text: r.review.summary })) : null,
        block('Checks', checksList(stage, r)),
        block('Documents', filesGrid(r.artifacts, context)),
        block('Commits', commitFacts(r)));
    }
    const cp = stage.checkpoint;
    if (!cp) {
      const latest = [...stage.attempts].reverse().find((a) => a.result);
      if (latest) {
        return h('div', {}, h('p', { class: 'empty', text: `Attempt ${latest.number}’s result is ${label(stage.status).toLowerCase()}. It isn’t accepted yet.` }),
          block('Summary', h('p', { class: 'prose-sm', text: latest.result.summary })), block('Documents', filesGrid(latest.result.artifacts, context)));
      }
      return h('p', { class: 'empty', text: `${stage.id} has no result yet.` });
    }
    const r = cp.result;
    return h('div', {},
      block('Summary', h('p', { class: 'prose-sm', text: r.summary })),
      r.review && r.review.summary ? block('Supervisor', h('p', { class: 'prose-sm', text: r.review.summary })) : null,
      cp.approval ? block('Approval', h('p', {}, `Approved by ${cp.approval.actor === 'local' ? 'you' : cp.approval.actor} `, timeEl(cp.approval.at))) : null,
      cp.historical_only ? block(null, h('p', { class: 'empty', text: 'Kept for history only; it can’t feed a new stage.' })) : null,
      block('Checks', checksList(stage, r)),
      block('Documents', filesGrid(r.artifacts, context)),
      (r.requirements || []).length ? block('Found during planning', h('dl', { class: 'facts' }, r.requirements.map((q) => [h('dt', { text: q.capability }), h('dd', { text: q.reason })]))) : null,
      block('Commits', commitFacts(r)));
  }

  function changes(run, rev, stage) {
    const bases = [h('option', { value: '', text: 'Where this revision started' })];
    for (const v of run.revisions) {
      for (const s of v.stages) {
        if (s.checkpoint && !(v.id === rev.id && s.id === stage.id)) bases.push(h('option', { value: s.checkpoint.id, selected: ui.diff.from === s.checkpoint.id, text: `${s.id}${v.id === rev.id ? '' : ` in revision ${shortId(v.id)}`}` }));
      }
    }
    const from = h('select', { onchange: () => { ui.diff.from = from.value; } }, bases);
    const key = `${run.id}/${rev.id}/${stage.id}/${ui.diff.from}`;
    const target = stage.checkpoint || stage.awaiting;
    const controls = h('div', { class: 'compare' },
      h('label', {}, 'Compare with', from),
      h('button', { type: 'button', class: 'btn', disabled: !stage.checkpoint || ui.diff.loading, text: ui.diff.loading ? 'Loading…' : 'Show changes', onclick: () => loadDiff(run, rev, stage) }));
    let body = null;
    if (!stage.checkpoint) body = h('p', { class: 'empty', text: target ? 'Changes can be compared once this result is accepted.' : `Changes appear once ${stage.id} has a result.` });
    else if (ui.diff.key === key && ui.diff.error) body = h('p', { class: 'error-text', text: ui.diff.error });
    else if (ui.diff.key === key && ui.diff.data) body = diffView(ui.diff.data);
    const repos = h('dl', { class: 'facts' }, rev.repositories.map((repo) => [
      h('dt', { text: repo.id }),
      h('dd', {}, `${repo.url} at ${repo.ref || 'HEAD'}, from `, repo.source_pin ? h('code', { title: repo.source_pin, text: sha(repo.source_pin) }) : 'an unpinned commit', ', into ', h('code', { text: repo.output_branch })),
    ]));
    return h('div', {}, controls, body, block('Repositories', repos));
  }

  async function loadDiff(run, rev, stage) {
    const key = `${run.id}/${rev.id}/${stage.id}/${ui.diff.from}`;
    ui.diff = { key, data: null, error: null, loading: true, from: ui.diff.from };
    render();
    try {
      const q = new URLSearchParams({ revision: rev.id, node: stage.id });
      if (ui.diff.from) q.set('from', ui.diff.from);
      ui.diff.data = await api(`/api/runs/${encodeURIComponent(run.id)}/diff?${q}`);
    } catch (err) {
      ui.diff.error = err.message;
    }
    ui.diff.loading = false;
    render();
  }

  function diffView(c) {
    const parts = [];
    let adds = 0, dels = 0, count = 0;
    const repos = (c.repositories || []).map((repo) => {
      const files = repo.unavailable ? [] : parsePatch(repo.patch || '');
      for (const f of files) { adds += f.adds; dels += f.dels; count++; }
      return { repo, files };
    });
    parts.push(h('p', { class: 'diff-summary', text: `${plural(count, 'file')} changed, ${adds} added, ${dels} removed${c.from.objective !== c.to.objective ? '. The objective changed between these points.' : ''}` }));
    for (const { repo, files } of repos) {
      if (repos.length > 1) parts.push(h('h3', { class: 'diff-repo', text: repo.repository }));
      if (repo.unavailable) { parts.push(h('p', { class: 'empty', text: repo.unavailable })); continue; }
      if (repo.truncated) parts.push(h('p', { class: 'error-text', text: 'This patch is over 1 MB and was cut short.' }));
      if (!files.length) parts.push(h('p', { class: 'empty', text: 'No source changes.' }));
      for (const f of files) {
        parts.push(h('details', { class: 'diff-file', open: files.length <= 8 },
          h('summary', {}, h('span', { class: 'path', text: f.path }), h('span', { class: 'adds', text: `+${f.adds}` }), h('span', { class: 'dels', text: `−${f.dels}` })),
          h('div', { class: 'diff-lines' }, f.lines.map(([k, t]) => h('div', { class: k, text: t })))));
      }
    }
    return h('div', {}, parts);
  }

  function parsePatch(text) {
    const files = [];
    let f = null;
    for (const line of text.split('\n')) {
      if (line.startsWith('diff --git ')) {
        const m = line.match(/ b\/(.*)$/);
        f = { path: m ? m[1] : line.slice(11), adds: 0, dels: 0, lines: [] };
        files.push(f);
        continue;
      }
      if (!f || line.startsWith('index ') || line.startsWith('--- ') || line.startsWith('+++ ') || line.startsWith('new file mode') || line.startsWith('deleted file mode')) continue;
      if (line.startsWith('@@')) f.lines.push(['h', line]);
      else if (line.startsWith('+')) { f.adds++; f.lines.push(['a', line]); }
      else if (line.startsWith('-')) { f.dels++; f.lines.push(['d', line]); }
      else f.lines.push(['', line]);
    }
    return files;
  }

  // Log: the decisions that shaped the run, newest first, across revisions.
  function log(run) {
    const events = [];
    const add = (at, dot, title, detail, rev) => { if (!zeroTime(at)) events.push({ at, dot, title, detail, rev }); };
    const now = new Date().toISOString();
    run.revisions.forEach((v, i) => {
      const parent = run.revisions.find((p) => p.id === v.parent);
      if (i === 0) add(v.created_at, 'idle', 'Run started', v.objective.split('\n')[0], v);
      else {
        const kept = v.stages.filter((s) => s.checkpoint && !s.attempts.length).map((s) => s.id);
        const detail = parent && parent.objective !== v.objective ? 'The objective changed, so every stage runs again.' : kept.length ? `Kept ${kept.join(', ')}.` : '';
        const first = v.stages.find((s) => !(s.checkpoint && !s.attempts.length));
        add(v.created_at, 'idle', `Rewound${first ? ` to ${first.id}` : ''}`, detail, v);
      }
      for (const s of v.stages) {
        for (const a of s.attempts) {
          if (a.error) add(a.updated_at, 'bad', `${s.id} attempt ${a.number} failed`, a.error, v);
          if (a.result && a.result.review && a.result.review.summary) {
            add(a.updated_at, a.result.review.accepted ? 'ok' : 'bad', a.result.review.accepted ? `Supervisor accepted ${s.id}` : `Supervisor asked for changes to ${s.id}`, a.result.review.summary, v);
          }
          if (a.approval) add(a.approval.at, 'needs', `${a.approval.actor === 'local' ? 'You' : a.approval.actor} approved ${s.id}`, '', v);
          if (a.result && a.result.prs) for (const url of Object.values(a.result.prs)) add(a.updated_at, 'ok', 'Pull request opened', url, v);
        }
      }
      for (const m of v.messages) add(m.created_at, 'run', `You messaged the ${m.recipient}${m.node ? ` of ${m.node}` : ''}`, m.body, v);
      if (v.current && v.recovery) add(now, 'needs', `Recovering from a ${v.recovery.phase} problem`, v.recovery.detail, v);
      if (v.current && v.stalled) add(now, 'bad', 'Blocked', v.stalled, v);
    });
    if (!events.length) return h('p', { class: 'empty', text: 'Nothing has happened yet.' });
    events.sort((a, b) => b.at.localeCompare(a.at));
    return h('ol', { class: 'log' }, events.map((e) => h('li', {},
      h('span', { class: `dot ${e.dot}` }),
      h('div', {},
        h('p', { class: 'log-title', text: e.title }),
        h('div', { class: 'log-meta' }, timeEl(e.at), run.revisions.length > 1
          ? h('button', { type: 'button', class: 'rev-link', title: 'View this revision', onclick: () => select({ revision: e.rev.current ? null : e.rev.id, stagePicked: false, tab: 'Chat' }) }, h('span', { class: 'rev-chip', text: shortId(e.rev.id) }), e.rev.current ? ' current' : '')
          : null),
        e.detail ? h('p', { class: 'log-detail clamp', text: e.detail, title: e.detail }) : null))));
  }

  // Rail: the run's facts, environment and readiness, out of the way.
  function renderRail(run, rev, stage) {
    const u = run.usage;
    const meterClass = u.fraction >= 1 ? 'meter over' : u.fraction >= 0.8 ? 'meter warn' : 'meter';
    const usage = h('section', {}, h('h3', { text: 'Tokens' }),
      h('div', {}, h('strong', { text: tokens(u.tokens) }), u.limit > 0 ? h('span', { class: 'faint', text: ` of ${tokens(u.limit)}` }) : h('span', { class: 'faint', text: ' (no limit)' })),
      u.limit > 0 ? h('div', { class: meterClass }, h('i')) : null,
      h('p', { class: 'faint', text: [u.cost_usd ? `$${u.cost_usd.toFixed(2)} at API prices` : null, u.estimated ? 'running jobs estimated' : null].filter(Boolean).join(', ') }));

    const rt = rev.runtime;
    const env = h('section', {}, h('h3', { text: 'Environment' }),
      h('ul', { class: 'svc' },
        h('li', {}, h('span', { class: `dot ${rt.ready ? 'ok' : rt.state === 'stopped' ? 'idle' : 'needs'}` }), h('span', { class: 'grow', text: rt.id ? `VM ${rt.state || 'unknown'}` : 'No VM yet' })),
        (rt.services || []).map((s) => h('li', { title: s.url || '' }, h('span', { class: `dot ${/healthy|running/.test(s.state) ? 'ok' : 'needs'}` }), h('span', { class: 'grow', text: s.name }), h('span', { class: 'faint', text: s.state }))),
        (rev.children || []).map((c) => h('li', {}, h('span', { class: `dot ${c.runtime.ready ? 'ok' : 'needs'}` }), h('span', { class: 'grow', text: `${c.node} branch VM` }), h('span', { class: 'faint', text: c.runtime.state })))),
      rt.preview_url ? h('p', {}, h('a', { href: rt.preview_url, target: '_blank', rel: 'noopener noreferrer', text: 'Open preview' })) : null);

    const r = rev.readiness;
    const failing = r.probes.filter((p) => !p.passed);
    const ready = h('section', {}, h('h3', { text: 'Readiness' }),
      failing.length
        ? h('ul', { class: 'rail-problems' }, failing.map((p) => h('li', { text: `${p.capability}: ${p.detail}` })))
        : h('p', { class: 'muted', text: r.probes.length ? `${plural(r.probes.length, 'capability')} checked` : 'Checked during plan' }),
      r.probes.length ? h('details', {}, h('summary', { text: 'All checks' }), h('ul', { class: 'svc' }, r.probes.map((p) => h('li', { title: p.detail }, h('span', { class: `dot ${p.passed ? 'ok' : 'bad'}` }), h('span', { class: 'grow', text: p.capability }), h('span', { class: 'faint', text: ago(p.checked_at) }))))) : null);

    const path = h('input', { type: 'text', placeholder: 'Plugin file path' });
    const plugins = h('section', {}, h('h3', { text: 'Plugins' }),
      (r.plugins || []).length
        ? h('ul', { class: 'svc' }, r.plugins.map((p) => h('li', {}, h('span', { class: 'grow', text: `${p.id} ${p.version}` }), finished(run) ? null : h('button', { type: 'button', class: 'btn btn-quiet btn-small', text: 'Remove', onclick: () => act(run, { action: 'plugin-remove', plugin_id: p.id }, `Removed ${p.id}. Plan runs again.`) }))))
        : h('p', { class: 'muted', text: 'None attached' }),
      finished(run) || viewingHistory(run) ? null : h('form', { class: 'plugin-form', onsubmit: (e) => { e.preventDefault(); if (path.value.trim()) act(run, { action: 'plugin-attach', plugin_path: path.value.trim() }, 'Plugin attached. Plan runs again.'); } },
        path, h('button', { type: 'submit', class: 'btn btn-small', text: 'Attach' })));

    const stageFacts = stage ? h('section', {}, h('h3', { text: `${stage.id} settings` }), h('dl', { class: 'facts' },
      h('dt', { text: 'Worker' }), h('dd', { text: model(stage.models.worker) }),
      h('dt', { text: 'Supervisor' }), h('dd', { text: model(stage.models.supervisor) }),
      h('dt', { text: 'Attempts' }), h('dd', { text: `${stage.limits.attempts} of ${stage.limits.max_attempts}` }),
      h('dt', { text: 'Time limit' }), h('dd', { text: `${duration(stage.limits.attempt_seconds)} each` }),
      h('dt', { text: 'Nudged after' }), h('dd', { text: `${duration(stage.limits.stall_seconds)} silent` }))) : null;

    const runFacts = h('section', {}, h('h3', { text: 'Run' }), h('dl', { class: 'facts' },
      h('dt', { text: 'Revision' }), h('dd', {}, h('code', { title: rev.id, text: shortId(rev.id) }), rev.current ? '' : ' (history)'),
      h('dt', { text: 'Priority' }), h('dd', { text: String(run.priority) }),
      h('dt', { text: 'Owner' }), h('dd', { text: run.owner })));

    $('rail').replaceChildren(usage, env, ready, stageFacts, plugins, runFacts);
    const bar = $('rail').querySelector('.meter i');
    if (bar) bar.style.width = `${Math.min(100, u.fraction * 100).toFixed(1)}%`;
  }

  // ---------- composer ----------

  const composer = $('composer');
  const textarea = $('message');
  function grow() {
    textarea.style.height = 'auto';
    textarea.style.height = `${Math.min(200, textarea.scrollHeight)}px`;
  }
  textarea.addEventListener('input', grow);
  $('recipient').addEventListener('change', () => render());
  composer.addEventListener('submit', async (e) => {
    e.preventDefault();
    const run = currentRun();
    const text = textarea.value.trim();
    if (!run || !text) return;
    $('send').disabled = true;
    const to = $('recipient').value;
    const ok = await act(run, { action: 'message', node: ui.stage, recipient: to, message: text }, `Sent to the ${ui.stage} ${to}`);
    $('send').disabled = false;
    if (ok) { textarea.value = ''; grow(); }
  });
  textarea.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      composer.requestSubmit();
    }
  });

  // ---------- keyboard ----------

  document.addEventListener('keydown', (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey || $('dialog').open) return;
    if (e.key === 'Escape') {
      if (ui.reader) return closeReader();
      if (ui.menu) { ui.menu = false; return render(); }
      return closeSide();
    }
    if (e.target.closest && e.target.closest('input, textarea, select, [contenteditable]')) return;
    const run = currentRun();
    const rev = viewRevision(run);
    const stages = rev ? rev.stages : [];
    const i = stages.findIndex((s) => s.id === ui.stage);
    const list = ordered();
    const ri = list.findIndex((r) => r.id === ui.runId);
    const k = e.key;
    let handled = true;
    if (k === 'j' || k === 'k') { const next = list[Math.max(0, Math.min(list.length - 1, ri + (k === 'j' ? 1 : -1)))]; if (next) select({ runId: next.id }); }
    else if (k === 'h' || k === 'ArrowLeft') { if (i > 0) select({ stage: stages[i - 1].id }); }
    else if (k === 'l' || k === 'ArrowRight') { if (i >= 0 && i < stages.length - 1) select({ stage: stages[i + 1].id }); }
    else if (/^[1-4]$/.test(k)) select({ tab: TABS[Number(k) - 1] });
    else if (k === '[' || k === ']') moveRevision(run, k === '[' ? -1 : 1);
    else if (k === 'i') { select({ tab: 'Chat' }); if (!composer.hidden) textarea.focus(); }
    else if (k === 's') { const r = $('recipient'); r.value = r.value === 'supervisor' ? 'worker' : 'supervisor'; render(); say(`Messages go to the ${r.value}`); }
    else if (k === 'n') newRunDialog();
    else if (k === 'r' && run) rewindDialog(run);
    else if (k === 'a' && run) approveKey(run);
    else if (k === 'x' && run && !finished(run)) cancelDialog(run);
    else if (k === 'g') openPullRequests(run);
    else if (k === 'o' && run) {
      const stage = currentStage(rev);
      const files = stage && resultOf(stage) ? resultOf(stage).artifacts : [];
      if (files && files.length) openReader(files, 0, `${run.name}, ${stage.id}`);
      else say('This stage has no documents yet.', true);
    }
    else if (k === 'd' && run) { const stage = currentStage(rev); select({ tab: 'Changes' }); if (stage && stage.checkpoint) loadDiff(run, rev, stage); }
    else if (k === '?') helpDialog();
    else handled = false;
    if (handled) e.preventDefault();
  });

  function moveRevision(run, step) {
    if (!run) return;
    const ids = run.revisions.map((v) => v.id);
    const next = ids[Math.max(0, Math.min(ids.length - 1, ids.indexOf(viewRevision(run).id) + step))];
    select({ revision: next === run.current_revision ? null : next, stagePicked: false });
  }

  // ---------- themes ----------

  const THEME_KEY = 'envctl-web-theme';
  const VARS = ['--canvas', '--surface', '--sidebar', '--selected', '--ink', '--ink-2', '--ink-3', '--line', '--line-strong', '--accent', '--needs', '--needs-soft', '--run', '--run-soft', '--ok', '--ok-soft', '--bad', '--bad-soft', '--idle', '--idle-soft', '--add', '--del'];
  function applyTheme(name, palettes) {
    const root = document.documentElement;
    for (const v of VARS) root.style.removeProperty(v);
    root.removeAttribute('data-theme');
    if (name === 'light' || name === 'dark') root.dataset.theme = name;
    const p = palettes.find((x) => x.name === name);
    if (!p) return;
    root.dataset.theme = p.dark ? 'dark' : 'light';
    const mix = (a, b, pct) => `color-mix(in srgb, ${a} ${pct}%, ${b})`;
    const set = (k, v) => root.style.setProperty(k, v);
    set('--canvas', p.background);
    set('--surface', p.dark ? mix(p.background, p.text, 94) : mix(p.background, '#ffffff', 40));
    set('--sidebar', mix(p.background, p.surface, 70));
    set('--selected', mix(p.background, p.surface, 40));
    set('--ink', p.text); set('--ink-2', mix(p.text, p.muted, 60)); set('--ink-3', p.muted);
    set('--line', mix(p.line, p.background, 60)); set('--line-strong', p.line); set('--accent', p.accent);
    for (const [role, color] of [['needs', p.warn], ['run', p.accent], ['ok', p.success], ['bad', p.danger], ['idle', p.muted]]) {
      set(`--${role}`, color);
      set(`--${role}-soft`, mix(color, p.background, 16));
    }
    set('--add', mix(p.success, p.background, 14)); set('--del', mix(p.danger, p.background, 14));
  }
  async function setupThemes() {
    const pick = $('theme');
    let palettes = [];
    try { palettes = await api('/api/themes'); } catch { /* the built-in look still works */ }
    pick.replaceChildren(
      h('option', { value: 'system', text: 'System theme' }), h('option', { value: 'light', text: 'Light' }), h('option', { value: 'dark', text: 'Dark' }),
      h('optgroup', { label: 'Terminal themes' }, palettes.map((p) => h('option', { value: p.name, text: p.name }))));
    let saved = 'system';
    try { saved = localStorage.getItem(THEME_KEY) || 'system'; } catch { /* storage may be unavailable */ }
    pick.value = saved;
    applyTheme(saved, palettes);
    pick.addEventListener('change', () => {
      applyTheme(pick.value, palettes);
      try { localStorage.setItem(THEME_KEY, pick.value); } catch { /* ignore */ }
    });
  }

  // ---------- wiring ----------

  function closeSide() {
    $('side').classList.remove('open');
    $('scrim').hidden = true;
    $('menu').setAttribute('aria-expanded', 'false');
  }
  $('menu').addEventListener('click', () => {
    $('side').classList.add('open');
    $('scrim').hidden = false;
    $('menu').setAttribute('aria-expanded', 'true');
  });
  $('scrim').addEventListener('click', closeSide);
  $('new-run').addEventListener('click', newRunDialog);
  $('help-open').addEventListener('click', helpDialog);
  $('reader-close').addEventListener('click', closeReader);
  $('reader-scrim').addEventListener('click', closeReader);
  document.addEventListener('click', (e) => { if (ui.menu && !e.target.closest('.menu')) { ui.menu = false; render(); } });
  document.addEventListener('selectionchange', () => {
    const sel = window.getSelection();
    if (ui.pending && (!sel || sel.isCollapsed)) render();
  });
  window.addEventListener('hashchange', () => { readHash(); render(); });
  setInterval(() => document.querySelectorAll('time[datetime]').forEach((el) => { el.textContent = ago(el.getAttribute('datetime')); }), 20000);

  function connect() {
    const source = new EventSource('/api/stream');
    source.addEventListener('state', (e) => {
      ui.state = JSON.parse(e.data);
      ui.link = 'ok';
      render();
    });
    source.onerror = () => {
      ui.link = 'reconnecting';
      renderSide();
    };
  }

  readHash();
  setupThemes();
  render();
  connect();
})();
