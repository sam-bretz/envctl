(() => {
  'use strict';

  const token = document.querySelector('meta[name="envctl-token"]').content;
  const $ = (id) => document.getElementById(id);
  const PANELS = ['Conversation', 'Checkpoint', 'Changes', 'Tests', 'Services', 'Readiness', 'Graph', 'History'];

  const ui = {
    state: null,
    runId: null,
    stage: null,
    stagePicked: false,
    panel: 'Conversation',
    revision: null, // a historical revision being viewed; null is current
    expanded: false,
    diff: { key: null, data: null, error: null, loading: false, from: '' },
    reader: null,
    link: 'connecting',
    pending: false,
  };

  // ---------- DOM helpers ----------

  function h(tag, props, ...children) {
    const el = document.createElement(tag);
    for (const [key, value] of Object.entries(props || {})) {
      if (value === undefined || value === null || value === false) continue;
      if (key === 'class') el.className = value;
      else if (key === 'text') el.textContent = value;
      else if (key.startsWith('on')) el.addEventListener(key.slice(2), value);
      else if (key in el && key !== 'list') el[key] = value;
      else el.setAttribute(key, value === true ? '' : value);
    }
    append(el, children);
    return el;
  }
  function append(el, children) {
    for (const child of children.flat(Infinity)) {
      if (child === null || child === undefined || child === false) continue;
      el.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return el;
  }
  const svg = (tag, attrs, ...children) => {
    const el = document.createElementNS('http://www.w3.org/2000/svg', tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v);
    }
    for (const c of children.flat()) if (c) el.append(c instanceof Node ? c : document.createTextNode(c));
    return el;
  };

  // ---------- formatting ----------

  const zeroTime = (t) => !t || t.startsWith('0001-');
  function ago(t) {
    if (zeroTime(t)) return '';
    const s = Math.round((Date.now() - new Date(t).getTime()) / 1000);
    const abs = Math.abs(s);
    const span = abs < 60 ? `${abs} s` : abs < 3600 ? `${Math.round(abs / 60)} min` : abs < 86400 ? `${Math.round(abs / 3600)} h` : `${Math.round(abs / 86400)} d`;
    if (abs < 10) return 'just now';
    return s >= 0 ? `${span} ago` : `in ${span}`;
  }
  const when = (t) => (zeroTime(t) ? '' : new Date(t).toLocaleString());
  const timeEl = (t) => h('time', { datetime: t, title: when(t), text: ago(t) });
  const duration = (sec) => (sec >= 3600 && sec % 3600 === 0 ? `${sec / 3600} h` : sec >= 60 ? `${Math.round(sec / 60)} min` : `${sec} s`);
  const shortId = (id) => (id ? id.replace(/^([a-z]+_[0-9a-f]{8})[0-9a-f]+$/, '$1') : '');
  const idEl = (id) => h('code', { title: id, text: shortId(id) });
  const bytes = (n) => (n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : n >= 1024 ? `${(n / 1024).toFixed(1)} KB` : `${n} bytes`);
  const sha = (s) => (s ? s.slice(0, 12) : '');
  const model = (m) => m || 'harness default';

  const STATUS = {
    'done': ['done', 'Done'],
    'historical': ['historical', 'Historical'],
    'pending': ['pending', 'Not started'],
    'preparing': ['running', 'Preparing'],
    'running': ['running', 'Running'],
    'verifying': ['publishing', 'Checking the result'],
    'awaiting-approval': ['waiting', 'Waiting for you'],
    'approved, publishing': ['publishing', 'Approved, publishing'],
    'approved, not published': ['failed', 'Approved, not published'],
    'recovering': ['waiting', 'Recovering'],
    'failed': ['failed', 'Failed'],
    'cancelled': ['failed', 'Cancelled'],
    'checkpointed': ['done', 'Accepted'],
    'archived': ['historical', 'Archived'],
  };
  const statusClass = (s) => (STATUS[s] || ['pending'])[0];
  const statusLabel = (s) => (STATUS[s] || [null, s])[1];
  const RUN_STATE = { queued: 'Queued', preparing: 'Preparing its VM', active: 'Running', recovering: 'Recovering', draining: 'Finishing old work', 'needs-attention': 'Needs attention', completed: 'Completed', cancelled: 'Cancelled', superseded: 'Superseded' };
  const runState = (s) => RUN_STATE[s] || s;

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
  const LANES = [['attention', 'Waiting on you'], ['running', 'Running'], ['finished', 'Finished']];
  function ordered() {
    const out = [];
    for (const [lane] of LANES) {
      out.push(...runs().filter((r) => r.lane === lane).sort((a, b) => b.updated_at.localeCompare(a.updated_at)));
    }
    return out;
  }
  const currentRun = () => runs().find((r) => r.id === ui.runId) || null;
  const currentRevision = (run) => run && run.revisions.find((v) => v.id === run.current_revision);
  const viewRevision = (run) => (run && ui.revision && run.revisions.find((v) => v.id === ui.revision)) || currentRevision(run);
  const viewingHistory = (run) => run && ui.revision && ui.revision !== run.current_revision;
  const currentStage = (rev) => (rev && (rev.stages.find((s) => s.id === ui.stage) || rev.stages[0])) || null;

  function defaultStage(rev) {
    if (!rev || !rev.stages.length) return null;
    const pick = (fn) => rev.stages.find(fn);
    return (pick((s) => s.awaiting) || pick((s) => ['running', 'preparing', 'verifying', 'approved, publishing', 'approved, not published', 'failed'].includes(s.status)) || [...rev.stages].reverse().find((s) => s.status === 'done') || rev.stages[0]).id;
  }

  function ensureSelection() {
    if (!ui.state) return; // keep a linked selection until runs arrive
    const all = ordered();
    if (!currentRun()) {
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
    if (ui.panel !== 'Conversation') p.set('view', ui.panel);
    if (ui.revision) p.set('revision', ui.revision);
    history.replaceState(null, '', p.toString() ? `#${p}` : location.pathname);
  }
  function readHash() {
    const p = new URLSearchParams(location.hash.slice(1));
    ui.runId = p.get('run');
    ui.stage = p.get('stage');
    ui.stagePicked = !!ui.stage;
    ui.revision = p.get('revision');
    ui.panel = PANELS.includes(p.get('view')) ? p.get('view') : 'Conversation';
  }

  function select(changes) {
    if ('runId' in changes && changes.runId !== ui.runId) {
      ui.revision = null;
      ui.stagePicked = false;
      ui.expanded = false;
    }
    if ('stage' in changes) ui.stagePicked = true;
    Object.assign(ui, changes);
    closeRunsMenu();
    render();
  }

  // ---------- status line ----------

  function say(text, error) {
    const el = h('div', { class: error ? 'err' : '', text });
    $('status').append(el);
    setTimeout(() => el.remove(), error ? 9000 : 4000);
  }

  // ---------- actions ----------

  async function act(run, body, done) {
    if (viewingHistory(run)) {
      say('History is read-only. Return to the current revision to make changes.', true);
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
    if (!run || !run.pull_requests.length) {
      say('No pull request yet. The approved change opens one after you approve it.', true);
      return;
    }
    for (const pr of run.pull_requests) window.open(pr.url, '_blank', 'noopener');
  }

  function approveFlow(run) {
    const rev = currentRevision(run);
    if (!rev) return;
    const selected = rev.stages.find((s) => s.id === ui.stage);
    if (selected && selected.awaiting && !viewingHistory(run)) return approveDialog(run, selected);
    const waiting = rev.stages.find((s) => s.awaiting);
    if (waiting) {
      select({ revision: null, stage: waiting.id, panel: 'Checkpoint' });
      say(`${waiting.id} is waiting for your approval. Review it here, then approve.`);
      return;
    }
    if (rev.blocked) say(`${rev.blocked.node} is already approved, but the pull request was not opened: ${rev.blocked.detail}`, true);
    else say('Nothing in this run is waiting for approval.', true);
  }

  // ---------- dialogs ----------

  function dialog(title, build, submitLabel, onSubmit, opts = {}) {
    const dlg = $('dialog');
    const form = $('dialog-form');
    const error = h('p', { class: 'error-text', role: 'alert' });
    const submit = h('button', { type: 'submit', class: opts.danger ? 'primary danger-fill' : opts.approve ? 'approve' : 'primary', text: submitLabel });
    form.replaceChildren(
      h('h2', { text: title }),
      build(submit),
      error,
      h('div', { class: 'buttons' }, h('button', { type: 'button', class: 'quiet', text: opts.cancelLabel || 'Cancel', onclick: () => dlg.close() }), submit),
    );
    form.onsubmit = async (e) => {
      e.preventDefault();
      error.textContent = '';
      submit.disabled = true;
      try {
        const result = await onSubmit();
        if (result !== false) dlg.close();
      } catch (err) {
        error.textContent = err.message;
      } finally {
        submit.disabled = false;
      }
    };
    dlg.showModal();
    const first = form.querySelector('textarea, input, select');
    (first || submit).focus();
  }
  const field = (label, control, hint) => h('label', { class: 'field' }, h('span', { text: label }), control, hint ? h('p', { class: 'hint', text: hint }) : null);

  async function newRunDialog() {
    let choices;
    try {
      choices = await api('/api/workflows');
    } catch (err) {
      say(err.message, true);
      return;
    }
    const task = h('textarea', { rows: 6, required: true, placeholder: 'What should the agents do? Include requirements and how to check the work.' });
    const name = h('input', { type: 'text', placeholder: 'Defaults to the first line of the task' });
    const ref = h('input', { type: 'text', placeholder: 'https://github.com/owner/repo/issues/123' });
    const flows = choices.workflows;
    const pick = h('select', {}, flows.map((w) => h('option', { value: w.name, text: w.name === 'default' ? `default (${w.template || 'custom'})` : w.name })));
    const preview = h('p', { class: 'stages-preview' });
    const showStages = () => { preview.textContent = (flows.find((w) => w.name === pick.value) || { stages: [] }).stages.join(' → '); };
    pick.addEventListener('change', showStages);
    showStages();
    dialog('Start a run', () => h('div', { class: 'fields' },
      h('p', { class: 'hint', text: `Runs start from the envctl.yaml in ${choices.root}.` }),
      field('Task', task),
      field('Workflow', pick, null), preview,
      field('Name', name),
      field('Issue link', ref, 'Optional.'),
    ), 'Start run', async () => {
      if (!task.value.trim()) throw new Error('Describe the task for the run.');
      const run = await api('/api/runs', { operation_id: crypto.randomUUID(), task: task.value, name: name.value.trim(), task_ref: ref.value.trim(), workflow: pick.value });
      replaceRun(run);
      select({ runId: run.id, panel: 'Conversation' });
      say(`Started ${run.name}`);
    });
  }

  function rewindDialog(run) {
    if (viewingHistory(run)) return say('History is read-only. Return to the current revision to rewind.', true);
    const rev = currentRevision(run);
    const stage = h('select', {}, rev.stages.map((s) => h('option', { value: s.id, text: s.id, selected: s.id === ui.stage })));
    const objective = h('textarea', { rows: 8, value: rev.objective });
    let submitButton;
    const label = () => { if (submitButton) submitButton.textContent = `Rewind to ${stage.value}`; };
    stage.addEventListener('change', label);
    dialog('Rewind this run', (submit) => {
      submitButton = submit;
      label();
      return h('div', { class: 'fields' },
        h('p', { class: 'hint', text: 'A rewind starts a new revision from the stage you choose. Checkpoints before it are kept; that stage and everything after it run again. Changing the objective runs every stage again.' }),
        field('Stage', stage),
        field('Objective', objective));
    }, 'Rewind', async () => {
      const ok = await act(run, { action: 'rewind', node: stage.value, task: objective.value }, `Rewound to ${stage.value}`);
      if (ok) select({ revision: null, stage: stage.value });
      return ok;
    });
  }

  function approveDialog(run, stage) {
    const a = stage.awaiting;
    const attempt = stage.attempts.find((x) => x.id === a.attempt);
    const publishes = stage.kind === 'change';
    dialog(`Approve ${stage.id}?`, () => h('div', { class: 'fields' },
      h('p', { text: `This approves attempt ${attempt ? attempt.number : ''} exactly as shown. ${publishes ? 'envctl then opens the pull request.' : 'The next stages can start after it.'}` }),
      h('p', { class: 'hint' }, 'Result digest ', h('code', { text: a.work_digest.slice(0, 16) })),
    ), publishes ? 'Approve and publish' : 'Approve', () => act(run, { action: 'approve', node: stage.id, attempt: a.attempt, work_digest: a.work_digest },
      publishes ? `Approved ${stage.id}; publishing it now` : `Approved ${stage.id}`), { approve: true });
  }

  function cancelDialog(run) {
    dialog('Cancel this run?', () => h('p', { text: 'Running agents stop and the run’s VM is released. Checkpoints, artifacts and pull requests stay available.' }),
      'Cancel run', () => act(run, { action: 'cancel' }, `Cancelled ${run.name}`), { danger: true, cancelLabel: 'Keep running' });
  }

  function priorityDialog(run) {
    const input = h('input', { type: 'number', value: run.priority, step: 1 });
    dialog('Set priority', () => field('Priority', input, 'Higher priority runs get VMs and parallel slots first.'),
      'Save priority', () => act(run, { action: 'priority', priority: Number(input.value) || 0 }, 'Priority saved'));
  }

  function helpDialog() {
    const keys = [
      ['j / k', 'Next or previous run'], ['h / l', 'Previous or next stage'], ['1 – 8', 'Switch view'],
      ['[ / ]', 'Older or newer revision'], ['i', 'Write a message'], ['s', 'Switch between supervisor and worker'],
      ['n', 'Start a run'], ['r', 'Rewind'], ['a', 'Approve'], ['x', 'Cancel the run'], ['g', 'Open pull requests'],
      ['o', 'Open the stage’s artifact'], ['d', 'Show changes'], ['Esc', 'Close the artifact'],
    ];
    dialog('Keyboard shortcuts', () => h('div', { class: 'keys' }, keys.map(([k, d]) => [h('kbd', { text: k }), h('span', { text: d })])), 'Done', () => true, { cancelLabel: 'Close' });
  }

  // ---------- artifact reader ----------

  async function openReader(files, index, context) {
    ui.reader = { files, index, context };
    const file = files[index];
    $('reader').hidden = false;
    $('reader-title').textContent = file.name;
    $('reader-meta').replaceChildren(`${context} · ${file.media_type || 'artifact'} · ${bytes(file.size || 0)} · `, h('code', { title: file.digest, text: sha(file.digest) }));
    $('reader-files').hidden = files.length < 2;
    $('reader-files').replaceChildren(...files.map((f, i) => h('button', { type: 'button', 'aria-current': i === index ? 'true' : 'false', text: f.name, onclick: () => openReader(files, i, context) })));
    const body = $('reader-body');
    body.replaceChildren(h('p', { class: 'panel-empty', text: 'Loading…' }));
    try {
      const art = await api(`/api/artifacts/${encodeURIComponent(file.digest)}?media_type=${encodeURIComponent(file.media_type || '')}`);
      if (!ui.reader || ui.reader.files[ui.reader.index] !== file) return;
      const parts = [];
      if (art.truncated) parts.push(h('p', { class: 'panel-empty', text: `Showing the first 2 MB of ${bytes(art.size)}. Export the whole artifact with envctl run artifact ${file.digest} --output <file>.` }));
      if (art.images && art.images.length) parts.push(h('div', { class: 'shots' }, art.images.map((src) => h('img', { src, alt: 'Browser screenshot recorded by a check' }))));
      if (art.kind === 'markdown') {
        parts.push(h('article', { class: 'prose' }, sanitized(art.html)));
      } else if (art.kind === 'binary') {
        parts.push(h('p', { class: 'panel-empty', text: `This artifact is binary. Export it with envctl run artifact ${file.digest} --output <file>.` }));
      } else {
        parts.push(h('pre', { class: 'plain', text: art.text }));
      }
      body.replaceChildren(...parts);
      body.scrollTop = 0;
    } catch (err) {
      body.replaceChildren(h('p', { class: 'error-text', text: err.message }));
    }
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

  function closeReader() {
    ui.reader = null;
    $('reader').hidden = true;
  }

  // ---------- rendering ----------

  function render() {
    // Live updates wait while text is selected, so copying is not interrupted.
    const sel = window.getSelection();
    if (sel && !sel.isCollapsed && $('run').contains(sel.anchorNode)) {
      ui.pending = true;
      return;
    }
    ui.pending = false;
    ensureSelection();
    writeHash();
    renderBar();
    renderRuns();
    renderRun();
  }

  function renderBar() {
    const st = ui.state;
    $('root').textContent = st ? st.root : '';
    const link = $('link');
    link.className = 'bar-link';
    if (ui.link === 'ok' && st && !st.error) link.textContent = 'Live';
    else if (st && st.error) { link.textContent = 'Coordinator not answering'; link.classList.add('lost'); }
    else { link.textContent = ui.link === 'connecting' ? 'Connecting…' : 'Reconnecting…'; link.classList.add('lost'); }
  }

  function renderRuns() {
    const aside = $('runs');
    const all = runs();
    const children = [];
    for (const [lane, title] of LANES) {
      const items = ordered().filter((r) => r.lane === lane);
      if (!items.length && lane !== 'running') continue;
      children.push(h('section', { class: 'lane' },
        h('h2', {}, title, h('span', { text: String(items.length) })),
        items.length ? items.map(runStrip) : h('p', { class: 'lane-empty', text: all.length ? 'Nothing running.' : 'No runs yet.' })));
    }
    aside.replaceChildren(...children);
  }

  function runStrip(run) {
    const rev = currentRevision(run);
    const failed = run.state === 'needs-attention' || run.state === 'cancelled';
    const note = run.attention || (run.lane === 'running' ? runningNote(rev) : `${runState(run.state)} ${ago(run.updated_at)}`);
    return h('button', {
      type: 'button', class: `strip ${failed && run.lane !== 'attention' ? 'failed' : run.lane}`, 'aria-current': run.id === ui.runId ? 'true' : 'false',
      onclick: () => select({ runId: run.id }),
    },
    h('span', { class: 'strip-name', text: run.name }),
    h('span', { class: 'strip-note', text: note }),
    h('span', { class: 'strip-cells', 'aria-hidden': 'true' }, (rev ? rev.stages : []).map((s) => h('i', { class: statusClass(s.status), title: `${s.id}: ${statusLabel(s.status)}` }))));
  }

  function runningNote(rev) {
    if (!rev) return '';
    const active = rev.stages.find((s) => ['running', 'preparing', 'verifying', 'approved, publishing'].includes(s.status));
    if (active) return `${active.id}: ${statusLabel(active.status).toLowerCase()}`;
    return runState(rev.state);
  }

  function renderRun() {
    const run = currentRun();
    const main = $('run');
    const parts = ['run-head', 'banner', 'track', 'stage-head', 'tabs', 'panel'];
    if (!run) {
      for (const id of parts) $(id).replaceChildren();
      $('banner').hidden = true;
      $('composer').hidden = true;
      const empty = ui.state && ui.state.error
        ? h('div', { class: 'empty-run' }, h('h1', { text: 'The coordinator is not answering' }), h('p', { text: ui.state.error }), h('p', { text: 'Start it with envctl daemon serve, or run any envctl run command.' }))
        : h('div', { class: 'empty-run' }, h('h1', { text: ui.state ? 'No runs yet' : 'Connecting to envctl…' }),
          ui.state ? h('p', { text: `Start one from the envctl.yaml in ${ui.state.root}. Agents plan, build and check the work in their own VM, and ask you before anything is published.` }) : null,
          ui.state ? h('button', { type: 'button', class: 'primary', text: 'Start a run', onclick: newRunDialog }) : null);
      $('run-head').append(empty);
      return;
    }
    main.dataset.run = run.id;
    const rev = viewRevision(run);
    const stage = currentStage(rev);
    renderHead(run, rev);
    renderBanner(run, rev);
    renderTrack(rev, stage);
    renderStageHead(stage);
    renderTabs(rev, stage);
    renderPanel(run, rev, stage);
    $('composer').hidden = ui.panel !== 'Conversation' || viewingHistory(run);
    updateSendLabel();
  }

  function renderHead(run, rev) {
    const facts = [
      h('span', {}, h('strong', { text: runState(run.state) })),
      h('span', { text: `${rev.workflow} workflow` }),
      h('span', {}, 'Revision ', idEl(rev.id), viewingHistory(run) ? ' (history)' : ''),
      h('span', {}, 'Updated ', timeEl(run.updated_at)),
    ];
    if (run.priority) facts.push(h('span', { text: `Priority ${run.priority}` }));
    if (run.task_ref) facts.push(h('a', { href: run.task_ref, target: '_blank', rel: 'noopener noreferrer', text: 'Issue' }));
    const actions = [];
    if (run.pull_requests.length) actions.push(h('button', { type: 'button', text: run.pull_requests.length > 1 ? `Open ${run.pull_requests.length} pull requests` : 'Open pull request', title: 'g', onclick: () => openPullRequests(run) }));
    const live = !['completed', 'cancelled', 'superseded'].includes(run.state);
    actions.push(h('button', { type: 'button', text: 'Rewind', title: 'r', onclick: () => rewindDialog(run) }));
    actions.push(h('button', { type: 'button', class: 'quiet', text: 'Priority', onclick: () => priorityDialog(run) }));
    if (live) actions.push(h('button', { type: 'button', class: 'quiet danger', text: 'Cancel run', title: 'x', onclick: () => cancelDialog(run) }));

    const long = rev.objective.length > 280 || rev.objective.split('\n').length > 3;
    const u = run.usage;
    const meterClass = u.fraction >= 1 ? 'meter over' : u.fraction >= 0.8 ? 'meter warn' : 'meter';
    $('run-head').replaceChildren(
      h('h1', { class: 'run-title', text: run.name }),
      h('div', { class: 'run-actions' }, actions),
      h('div', { class: 'run-facts' }, facts),
      h('p', { class: `objective${long && !ui.expanded ? ' clamped' : ''}`, text: rev.objective }),
      long ? h('button', { type: 'button', class: 'objective-toggle', text: ui.expanded ? 'Show less' : 'Show the whole objective', onclick: () => { ui.expanded = !ui.expanded; render(); } }) : null,
      h('div', { class: 'usage' },
        u.limit > 0 ? h('div', { class: meterClass, role: 'meter', 'aria-valuemin': 0, 'aria-valuemax': 100, 'aria-valuenow': Math.round(u.fraction * 100), 'aria-label': 'Token ceiling used' }, h('i')) : h('span'),
        h('span', { text: u.summary })),
    );
    const bar = $('run-head').querySelector('.meter i');
    if (bar) bar.style.width = `${Math.min(100, u.fraction * 100).toFixed(1)}%`;
  }

  function renderBanner(run, rev) {
    const banner = $('banner');
    let content = null;
    let kind = '';
    const current = currentRevision(run);
    if (viewingHistory(run)) {
      kind = 'history';
      content = [h('div', {}, h('h3', { text: 'You are looking at an older revision' }), h('p', { text: 'It is kept for review; nothing here can change it.' })),
        h('div', { class: 'actions' }, h('button', { type: 'button', text: 'Return to the current revision', onclick: () => select({ revision: null }) }))];
    } else if (current.stages.some((s) => s.awaiting)) {
      const s = current.stages.find((x) => x.awaiting);
      content = [h('div', {}, h('h3', { text: `${s.id} is waiting for your approval` }), h('p', { text: s.kind === 'change' ? 'Review the result. Approving opens the pull request.' : 'Review the result, then approve it so the run can continue.' })),
        h('div', { class: 'actions' },
          ui.stage === s.id && ui.panel === 'Checkpoint' ? null : h('button', { type: 'button', text: 'Review it', onclick: () => select({ stage: s.id, panel: 'Checkpoint' }) }),
          h('button', { type: 'button', class: 'approve', text: s.kind === 'change' ? 'Approve and publish' : 'Approve', onclick: () => { select({ stage: s.id, panel: 'Checkpoint' }); approveDialog(run, s); } }))];
    } else if (current.blocked) {
      kind = 'failed';
      const b = current.blocked;
      content = [h('div', {}, h('h3', { text: 'Approved, but the pull request was not opened' }), h('p', { text: b.detail }), h('p', { text: `Rewind ${b.node} and approve it again. envctl keeps retrying until then.` })),
        h('div', { class: 'actions' }, h('button', { type: 'button', text: `Rewind ${b.node}`, onclick: () => { ui.stage = b.node; rewindDialog(run); } }))];
    } else if (current.stalled) {
      kind = 'failed';
      const plan = current.stages.find((s) => s.kind === 'plan');
      content = [h('div', {}, h('h3', { text: 'This run is blocked' }), h('p', { text: current.stalled }), h('p', { text: 'No stage can start until readiness passes. Rewinding to plan checks it again against the current code.' })),
        h('div', { class: 'actions' },
          h('button', { type: 'button', text: 'See readiness', onclick: () => select({ panel: 'Readiness' }) }),
          plan ? h('button', { type: 'button', text: `Rewind to ${plan.id}`, onclick: () => { ui.stage = plan.id; rewindDialog(run); } }) : null)];
    } else if (current.state === 'needs-attention') {
      kind = 'failed';
      content = [h('div', {}, h('h3', { text: 'This run needs attention' }), h('p', { text: current.recovery ? current.recovery.detail : 'A stage used all its attempts or the run reached its token ceiling.' }), h('p', { text: 'Rewind with a fix or a higher limit, or cancel the run.' })),
        h('div', { class: 'actions' }, h('button', { type: 'button', text: 'Rewind', onclick: () => rewindDialog(run) }))];
    } else if (current.recovery) {
      const r = current.recovery;
      content = [h('div', {}, h('h3', { text: `Recovering from a ${r.phase} problem` }), h('p', { text: r.detail }), h('p', {}, `Attempt ${r.failures}. Retrying `, timeEl(r.retry_at), '.'))];
    }
    banner.hidden = !content;
    banner.className = `banner ${kind}`;
    banner.replaceChildren(...(content || []));
  }

  function renderTrack(rev, stage) {
    $('track').replaceChildren(...rev.stages.map((s) => {
      const tries = s.limits.attempts ? `Attempt ${s.limits.attempts} of ${s.limits.max_attempts}` : s.gate === 'human' ? 'Needs your approval' : '';
      return h('button', {
        type: 'button', class: `cell ${statusClass(s.status)}`, 'aria-current': stage && s.id === stage.id ? 'true' : 'false',
        onclick: () => select({ stage: s.id }),
      }, h('span', { class: 'cell-name', text: s.id }), h('span', { class: 'cell-status', text: statusLabel(s.status) }), h('span', { class: 'cell-count', text: tries }));
    }));
  }

  function renderStageHead(stage) {
    if (!stage) return $('stage-head').replaceChildren();
    const l = stage.limits;
    $('stage-head').replaceChildren(
      h('h2', { text: stage.id }),
      h('span', { text: `${stage.kind} stage` }),
      h('span', { text: `${l.attempts} of ${l.max_attempts} attempts` }),
      h('span', { text: `${duration(l.attempt_seconds)} per attempt` }),
      h('span', { text: `nudged after ${duration(l.stall_seconds)} silent` }),
      h('span', { text: `Worker: ${model(stage.models.worker)}` }),
      h('span', { text: `Supervisor: ${model(stage.models.supervisor)}` }),
      stage.needs.length ? h('span', { text: `After ${stage.needs.join(', ')}` }) : null,
    );
  }

  function renderTabs(rev, stage) {
    const counts = {
      Tests: stage ? (resultOf(stage)?.checks || []).length || '' : '',
      Readiness: ['completed', 'cancelled', 'superseded'].includes(rev.state) ? '' : rev.readiness.problems.length || '',
    };
    $('tabs').replaceChildren(...PANELS.map((name, i) => h('button', {
      type: 'button', role: 'tab', class: 'tab', 'aria-selected': ui.panel === name ? 'true' : 'false', title: String(i + 1),
      onclick: () => select({ panel: name }),
    }, name, counts[name] ? h('span', { class: 'count', text: String(counts[name]) }) : null)));
  }

  const resultOf = (stage) => (stage.checkpoint ? stage.checkpoint.result : stage.awaiting ? stage.awaiting.result : null);

  function renderPanel(run, rev, stage) {
    const panel = $('panel');
    const scroll = [...panel.querySelectorAll('.activity')].map((el) => el.scrollHeight - el.scrollTop - el.clientHeight < 8);
    let content;
    if (!stage) content = h('p', { class: 'panel-empty', text: 'This workflow has no stages.' });
    else {
      content = {
        Conversation: conversation, Checkpoint: checkpoint, Changes: changes, Tests: tests,
        Services: services, Readiness: readiness, Graph: graph, History: historyPanel,
      }[ui.panel](run, rev, stage);
    }
    panel.replaceChildren(content);
    panel.querySelectorAll('.activity').forEach((el, i) => { if (scroll[i] !== false) el.scrollTop = el.scrollHeight; });
  }

  // Conversation: attempts, results, reviews and your messages, in order.
  function conversation(run, rev, stage) {
    const items = [];
    for (const a of stage.attempts) {
      const models = a.models || {};
      const head = [h('strong', { text: `Attempt ${a.number}` }), timeEl(a.started_at)];
      const body = [h('span', { text: statusLabel(a.state) || a.state })];
      if (models.worker || models.supervisor) body.push(h('p', { class: 'note', text: `Worker ${model(models.worker)}, supervisor ${model(models.supervisor)}` }));
      if (a.usage) body.push(h('p', { class: 'note', text: `${fmtTokens(a.usage)} tokens${a.usage.cost_usd ? `, $${a.usage.cost_usd.toFixed(2)} API-equivalent` : ''}` }));
      const p = a.progress;
      if (a.state === 'running' && p) {
        body.push(h('p', { class: 'note' }, h('span', { class: 'live', text: `Live: ${p.phase}` }), p.generation ? ` (resume ${p.generation})` : '', p.detail ? ` — ${p.detail}` : '', ' · updated ', timeEl(p.updated_at)));
        if (p.activity && p.activity.length) body.push(h('div', { class: 'activity', text: p.activity.join('\n') }));
      }
      items.push({ at: a.started_at, el: h('li', { class: 'entry' }, h('div', { class: 'entry-who' }, head), h('div', { class: 'entry-body' }, body)) });
      if (a.error) items.push({ at: a.updated_at, el: entry('error', 'Failed', a.updated_at, a.error) });
      if (a.result) {
        items.push({ at: a.updated_at, el: entry('', 'Worker', a.updated_at, a.result.summary) });
        const r = a.result.review;
        if (r && r.summary) items.push({ at: a.updated_at, el: entry(r.accepted ? 'accepted' : 'rejected', r.accepted ? 'Supervisor accepted' : 'Supervisor rejected', a.updated_at, r.summary) });
      }
    }
    for (const m of rev.messages) {
      if (m.node && m.node !== stage.id) continue;
      const el = entry('you', `You to ${m.recipient}`, m.created_at, m.body);
      el.querySelector('.entry-body').after(h('p', { class: 'note', text: m.node ? m.status : `${m.status} (sent to every stage)` }));
      items.push({ at: m.created_at, el });
    }
    if (!items.length) return h('p', { class: 'panel-empty', text: `Nothing has happened in ${stage.id} yet. A message sent now reaches its agents when the stage starts.` });
    items.sort((x, y) => x.at.localeCompare(y.at));
    return h('ol', { class: 'timeline' }, items.map((i) => i.el));
  }
  function entry(kind, who, at, text) {
    return h('li', { class: `entry ${kind}` }, h('div', { class: 'entry-who' }, h('strong', { text: who }), timeEl(at)), h('div', { class: 'entry-body', text }));
  }
  function fmtTokens(u) {
    const n = (u.input || 0) + (u.cache_write || 0) + (u.cache_read || 0) + (u.output || 0);
    return n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${(n / 1e3).toFixed(1)}K` : String(n);
  }

  function artifactList(files, context) {
    if (!files || !files.length) return h('p', { class: 'panel-empty', text: 'No artifacts.' });
    return h('ul', { class: 'files' }, files.map((f, i) => h('li', {}, h('button', { type: 'button', onclick: () => openReader(files, i, context) }, h('span', { text: f.name }), h('span', { text: `${f.media_type} · ${bytes(f.size)}` })))));
  }
  function commitsList(commits, prs) {
    const keys = Object.keys(commits || {}).sort();
    if (!keys.length) return null;
    return h('dl', { class: 'kv' }, keys.map((k) => [h('dt', { text: k }), h('dd', {}, h('code', { title: commits[k], text: sha(commits[k]) }), prs && prs[k] ? [' ', h('a', { href: prs[k], target: '_blank', rel: 'noopener noreferrer', text: 'pull request' })] : null)]));
  }
  const section = (title, ...children) => h('div', { class: 'section' }, title ? h('h3', { text: title }) : null, children);

  function checkpoint(run, rev, stage) {
    const context = `${run.name} · ${stage.id}`;
    if (stage.awaiting) {
      const a = stage.awaiting;
      const r = a.result;
      const history = viewingHistory(run);
      return h('div', {},
        h('div', { class: 'approval' },
          h('h3', { text: 'Review this result before approving' }),
          h('p', { class: 'summary-text', text: r.summary }),
          r.review && r.review.summary ? h('p', { class: 'summary-text note', text: `Supervisor ${r.review.accepted ? 'accepted' : 'rejected'}: ${r.review.summary}` }) : null,
          h('div', { class: 'actions' },
            h('button', { type: 'button', class: 'approve', disabled: history, text: stage.kind === 'change' ? 'Approve and publish' : 'Approve', onclick: () => approveDialog(run, stage) }),
            h('span', { class: 'digest' }, 'Approves result ', h('code', { text: a.work_digest.slice(0, 16) })))),
        section('Checks', checksTable(stage, r)),
        section('Artifacts', artifactList(r.artifacts, context)),
        section('Commits', commitsList(r.commits) || h('p', { class: 'panel-empty', text: 'No commits.' })));
    }
    const cp = stage.checkpoint;
    if (!cp) {
      const latest = stage.attempts[stage.attempts.length - 1];
      if (latest && latest.result) {
        return h('div', {}, h('p', { class: 'panel-empty', text: `The latest result is ${statusLabel(stage.status).toLowerCase()}.` }), section('Worker summary', h('p', { class: 'summary-text', text: latest.result.summary })), section('Artifacts', artifactList(latest.result.artifacts, context)));
      }
      return h('p', { class: 'panel-empty', text: `${stage.id} has no accepted checkpoint yet.` });
    }
    const r = cp.result;
    return h('div', {},
      section('Summary', h('p', { class: 'summary-text', text: r.summary })),
      r.review && r.review.summary ? section('Supervisor', h('p', { class: 'summary-text', text: r.review.summary })) : null,
      cp.approval ? section('Approval', h('p', {}, `Approved by ${cp.approval.actor} `, timeEl(cp.approval.at), '. Result ', h('code', { text: sha(cp.approval.result_digest) }))) : null,
      cp.historical_only ? section('', h('p', { class: 'panel-empty', text: 'Historical only: this output cannot satisfy a new stage.' })) : null,
      (r.requirements || []).length ? section('Requirements found during planning', h('ul', { class: 'problems' }, r.requirements.map((q) => h('li', {}, h('strong', { text: q.capability }), ` for ${(q.nodes || []).join(', ')}: ${q.reason}`)))) : null,
      section('Artifacts', artifactList(r.artifacts, context)),
      section('Commits', commitsList(r.commits, r.prs) || h('p', { class: 'panel-empty', text: 'No commits.' })),
      section('Checkpoint', h('dl', { class: 'kv' }, h('dt', { text: 'ID' }), h('dd', {}, h('code', { text: cp.id })), h('dt', { text: 'Recorded' }), h('dd', {}, timeEl(cp.created_at)))));
  }

  function checksTable(stage, result) {
    const results = (result && result.checks) || [];
    const names = new Set(results.map((c) => c.name));
    const pending = (stage.checks || []).filter((n) => !names.has(n));
    if (!results.length && !pending.length) return h('p', { class: 'panel-empty', text: `${stage.id} has no checks.` });
    return h('div', { class: 'table-wrap' }, h('table', {},
      h('thead', {}, h('tr', {}, ['Check', 'Result', 'Exit code', 'Evidence'].map((t) => h('th', { text: t })))),
      h('tbody', {},
        results.map((c) => h('tr', {},
          h('td', { text: c.name }),
          h('td', { class: c.passed ? 'pass' : 'fail', text: c.passed ? 'Passed' : 'Failed' }),
          h('td', { text: String(c.exit_code) }),
          h('td', {}, c.evidence_digest ? h('button', { type: 'button', class: 'quiet', text: 'Open evidence', onclick: () => openReader([{ name: `${c.name} evidence`, digest: c.evidence_digest, media_type: '', size: 0 }], 0, stage.id) }) : ''))),
        pending.map((n) => h('tr', {}, h('td', { text: n }), h('td', { class: 'warnt', text: 'Not run yet' }), h('td'), h('td'))))));
  }

  function tests(run, rev, stage) {
    return checksTable(stage, resultOf(stage));
  }

  function changes(run, rev, stage) {
    const repos = section('Repositories', ...rev.repositories.map((repo) => {
      const commit = stage.checkpoint && stage.checkpoint.result.commits ? stage.checkpoint.result.commits[repo.id] : '';
      const pr = stage.checkpoint && stage.checkpoint.result.prs ? stage.checkpoint.result.prs[repo.id] : '';
      return h('dl', { class: 'kv' },
        h('dt', { text: 'Repository' }), h('dd', {}, h('strong', { text: repo.id }), ` ${repo.url} at ${repo.ref || 'HEAD'}`),
        h('dt', { text: 'Starts from' }), h('dd', {}, repo.source_pin ? h('code', { title: repo.source_pin, text: sha(repo.source_pin) }) : 'Not pinned yet'),
        h('dt', { text: 'Output branch' }), h('dd', {}, h('code', { text: repo.output_branch })),
        commit ? [h('dt', { text: `${stage.id} commit` }), h('dd', {}, h('code', { title: commit, text: sha(commit) }))] : null,
        pr ? [h('dt', { text: 'Pull request' }), h('dd', {}, h('a', { href: pr, target: '_blank', rel: 'noopener noreferrer', text: pr }))] : null,
        repo.destination ? [h('dt', { text: 'Publishes to' }), h('dd', { text: repo.destination })] : null);
    }));

    const bases = [h('option', { value: '', text: 'The revision’s starting source' })];
    for (const v of run.revisions) {
      for (const s of v.stages) {
        if (s.checkpoint && !(v.id === rev.id && s.id === stage.id)) bases.push(h('option', { value: s.checkpoint.id, selected: ui.diff.from === s.checkpoint.id, text: `${shortId(v.id)} · ${s.id} checkpoint` }));
      }
    }
    const from = h('select', { onchange: () => { ui.diff.from = from.value; } }, bases);
    const key = `${run.id}/${rev.id}/${stage.id}/${ui.diff.from}`;
    const hasCheckpoint = !!stage.checkpoint;
    const controls = h('div', { class: 'diff-controls' },
      h('label', {}, 'Compare against', from),
      h('button', { type: 'button', class: 'primary', disabled: !hasCheckpoint || ui.diff.loading, text: ui.diff.loading ? 'Loading…' : 'Show changes', onclick: () => loadDiff(run, rev, stage) }));
    let result = null;
    if (!hasCheckpoint) result = h('p', { class: 'panel-empty', text: `Changes appear once ${stage.id} has a checkpoint.` });
    else if (ui.diff.key === key && ui.diff.error) result = h('p', { class: 'error-text', text: ui.diff.error });
    else if (ui.diff.key === key && ui.diff.data) result = diffView(ui.diff.data);
    return h('div', {}, repos, section(`Changes in ${stage.id}`, controls, result));
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
    const label = (e) => (e.checkpoint ? `${shortId(e.revision)} · ${e.checkpoint.node} checkpoint` : `${shortId(e.revision)} starting source`);
    const parts = [h('p', { class: 'note', text: `From ${label(c.from)} to ${label(c.to)}` })];
    if (c.from.objective !== c.to.objective) parts.push(h('p', { class: 'note', text: 'The objective changed between these points.' }));
    for (const repo of c.repositories || []) {
      parts.push(h('h3', { class: 'diff-repo', text: repo.repository }));
      if (repo.unavailable) { parts.push(h('p', { class: 'panel-empty', text: repo.unavailable })); continue; }
      if (repo.truncated) parts.push(h('p', { class: 'warnt', text: 'This patch is over 1 MB and was cut short.' }));
      const files = parsePatch(repo.patch || '');
      if (!files.length) parts.push(h('p', { class: 'panel-empty', text: 'No source changes.' }));
      for (const f of files) {
        parts.push(h('details', { class: 'diff-file', open: files.length <= 12 },
          h('summary', {}, h('span', { text: f.path }), h('span', { class: 'adds', text: `+${f.adds}` }), h('span', { class: 'dels', text: `−${f.dels}` })),
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
      if (!f || line.startsWith('index ') || line.startsWith('--- ') || line.startsWith('+++ ')) continue;
      if (line.startsWith('@@')) f.lines.push(['h', line]);
      else if (line.startsWith('+')) { f.adds++; f.lines.push(['a', line]); }
      else if (line.startsWith('-')) { f.dels++; f.lines.push(['d', line]); }
      else f.lines.push(['', line]);
    }
    return files;
  }

  function services(run, rev) {
    const runtime = (rt, title) => section(title,
      h('dl', { class: 'kv' },
        h('dt', { text: 'VM' }), h('dd', {}, rt.id ? h('code', { text: rt.id }) : 'Not created yet'),
        h('dt', { text: 'State' }), h('dd', { text: `${rt.state || 'unknown'}${rt.ready ? ', ready' : ', not ready'}` }),
        rt.provider ? [h('dt', { text: 'Provider' }), h('dd', { text: `${rt.provider} (${rt.location})` })] : null,
        rt.preview_url ? [h('dt', { text: 'Preview' }), h('dd', {}, h('a', { href: rt.preview_url, target: '_blank', rel: 'noopener noreferrer', text: rt.preview_url }))] : null),
      rt.services && rt.services.length
        ? h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, ['Service', 'State', 'Address inside the VM'].map((t) => h('th', { text: t })))),
          h('tbody', {}, rt.services.map((s) => h('tr', {}, h('td', { text: s.name }), h('td', { class: /healthy|running/.test(s.state) ? 'pass' : 'warnt', text: s.state }), h('td', {}, s.url ? h('code', { text: s.url }) : ''))))))
        : h('p', { class: 'panel-empty', text: 'No services reported.' }));
    return h('div', {}, runtime(rev.runtime, 'Run VM'), (rev.children || []).map((c) => runtime(c.runtime, `${c.node} branch VM`)));
  }

  function readiness(run, rev) {
    const r = rev.readiness;
    const problems = r.problems.length
      ? section('Plan must resolve these before later stages run', h('ul', { class: 'problems' }, r.problems.map((p) => h('li', { text: p }))))
      : section('', h('p', { class: 'pass', text: 'All declared capabilities have current readiness evidence.' }));
    const probes = section('Readiness checks', r.probes.length
      ? h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, ['Capability', 'Result', 'Detail', 'Checked', 'Expires'].map((t) => h('th', { text: t })))),
        h('tbody', {}, r.probes.map((p) => h('tr', {}, h('td', { text: p.capability }), h('td', { class: p.passed ? 'pass' : 'fail', text: p.passed ? 'Ready' : 'Not ready' }), h('td', { text: p.detail }), h('td', {}, timeEl(p.checked_at)), h('td', {}, timeEl(p.expires_at)))))))
      : h('p', { class: 'panel-empty', text: 'Plan has not checked readiness yet.' }));
    const needed = section('Capabilities this run needs', h('div', { class: 'chips' }, r.requirements.map((c) => h('span', { class: `chip${r.problems.some((p) => p.startsWith(`${c}:`)) ? ' fail' : ''}`, text: c }))));
    const discovered = (r.discovered || []).length ? section('Found during planning', h('ul', { class: 'problems' }, r.discovered.map((q) => h('li', {}, h('strong', { text: q.capability }), ` for ${(q.nodes || []).join(', ')}: ${q.reason}`)))) : null;
    const path = h('input', { type: 'text', placeholder: '/path/to/plugin.yaml' });
    const plugins = section('Plugins',
      (r.plugins || []).length ? h('ul', { class: 'files' }, r.plugins.map((p) => h('li', { class: 'inline-form' },
        h('span', {}, h('strong', { text: `${p.id}@${p.version}` }), ` provides ${(p.provides || []).join(', ') || 'nothing'}`),
        h('button', { type: 'button', class: 'quiet danger', text: 'Remove', onclick: () => act(run, { action: 'plugin-remove', plugin_id: p.id }, `Removed ${p.id}; Plan runs again`) })))) : h('p', { class: 'panel-empty', text: 'No plugins attached.' }),
      h('form', { class: 'inline-form', onsubmit: (e) => { e.preventDefault(); if (path.value.trim()) act(run, { action: 'plugin-attach', plugin_path: path.value.trim() }, 'Plugin attached; Plan runs again'); } },
        path, h('button', { type: 'submit', text: 'Attach plugin' })),
      h('p', { class: 'note', text: 'Attaching or removing a plugin starts a new revision from Plan.' }));
    return h('div', {}, problems, needed, probes, discovered, plugins);
  }

  function graph(run, rev, stage) {
    const W = 150, H = 62, GX = 40, GY = 20, PAD = 4;
    const cols = {};
    for (const s of rev.stages) (cols[s.depth] = cols[s.depth] || []).push(s);
    const pos = {};
    let height = 0;
    for (const [d, list] of Object.entries(cols)) {
      list.forEach((s, i) => { pos[s.id] = { x: PAD + Number(d) * (W + GX), y: PAD + i * (H + GY) }; });
      height = Math.max(height, list.length * (H + GY));
    }
    const width = PAD * 2 + Object.keys(cols).length * (W + GX) - GX;
    const edges = [];
    for (const s of rev.stages) {
      for (const n of s.needs) {
        const a = pos[n], b = pos[s.id];
        if (!a || !b) continue;
        const x1 = a.x + W, y1 = a.y + H / 2, x2 = b.x, y2 = b.y + H / 2, mx = (x1 + x2) / 2;
        edges.push(svg('path', { d: `M${x1} ${y1} C${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}` }));
      }
    }
    const nodes = rev.stages.map((s) => {
      const p = pos[s.id];
      const cls = statusClass(s.status);
      const bar = { done: 'bar-done', running: 'bar-running', publishing: 'bar-running', waiting: 'bar-waiting', failed: 'bar-failed' }[cls] || 'bar-pending';
      return svg('g', { class: `node${s.id === stage.id ? ' current' : ''}`, tabindex: '0', role: 'button', 'aria-label': `${s.id}, ${statusLabel(s.status)}`, onclick: () => select({ stage: s.id, panel: 'Conversation' }) },
        svg('rect', { x: p.x, y: p.y, width: W, height: H, rx: 3 }),
        svg('rect', { class: bar, x: p.x, y: p.y, width: W, height: 5, rx: 1 }),
        svg('text', { x: p.x + 12, y: p.y + 28 }, s.id),
        svg('text', { class: 's', x: p.x + 12, y: p.y + 47 }, statusLabel(s.status)));
    });
    return h('div', { class: 'graph' }, svg('svg', { width, height: height - GY + PAD * 2, viewBox: `0 0 ${width} ${height - GY + PAD * 2}`, role: 'img', 'aria-label': 'Stage dependency graph' }, edges, nodes));
  }

  function historyPanel(run, rev) {
    const list = [...run.revisions].reverse();
    return h('ol', { class: 'revisions' }, list.map((v) => {
      const viewing = v.id === rev.id;
      const done = v.stages.filter((s) => s.checkpoint);
      return h('li', { class: `revision${v.current ? ' current' : ''}` },
        h('h3', {}, h('code', { title: v.id, text: shortId(v.id) }), h('span', { text: runState(v.state) }), v.current ? h('span', { text: 'current' }) : null, h('span', {}, 'started ', timeEl(v.created_at))),
        h('p', { class: 'objective clamped', text: v.objective }),
        v.parent ? h('p', { class: 'note' }, 'Rewound from ', idEl(v.parent), v.from_checkpoint ? [' keeping checkpoints up to ', idEl(v.from_checkpoint)] : null) : null,
        done.length ? h('div', { class: 'chips' }, done.map((s) => h('span', { class: 'chip', text: `${s.id}${s.checkpoint.historical_only ? ' (historical)' : ''}` }))) : h('p', { class: 'note', text: 'No checkpoints.' }),
        h('p', {}, h('button', { type: 'button', disabled: viewing, text: viewing ? 'Viewing' : v.current ? 'View the current revision' : 'View this revision', onclick: () => select({ revision: v.current ? null : v.id, stagePicked: false }) })));
    }));
  }

  // ---------- composer ----------

  function recipient() {
    return document.querySelector('input[name="recipient"]:checked').value;
  }
  function updateSendLabel() {
    $('send').textContent = `Send to ${recipient()}`;
  }
  $('composer').addEventListener('change', updateSendLabel);
  $('composer').addEventListener('submit', async (e) => {
    e.preventDefault();
    const run = currentRun();
    const text = $('message').value.trim();
    if (!run || !text) return;
    $('send').disabled = true;
    const ok = await act(run, { action: 'message', node: ui.stage, recipient: recipient(), message: text }, `Sent to the ${ui.stage} ${recipient()}`);
    $('send').disabled = false;
    if (ok) $('message').value = '';
  });
  $('message').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      $('composer').requestSubmit();
    }
  });

  // ---------- keyboard ----------

  document.addEventListener('keydown', (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if ($('dialog').open) return;
    if (e.key === 'Escape' && ui.reader) { closeReader(); return; }
    const t = e.target;
    if (t.closest && t.closest('input, textarea, select, [contenteditable]')) return;
    const run = currentRun();
    const rev = viewRevision(run);
    const stages = rev ? rev.stages : [];
    const i = stages.findIndex((s) => s.id === ui.stage);
    const list = ordered();
    const ri = list.findIndex((r) => r.id === ui.runId);
    const key = e.key;
    let handled = true;
    if (key === 'j' || key === 'k') { const next = list[Math.max(0, Math.min(list.length - 1, ri + (key === 'j' ? 1 : -1)))]; if (next) select({ runId: next.id }); }
    else if (key === 'h' || key === 'ArrowLeft') { if (i > 0) select({ stage: stages[i - 1].id }); }
    else if (key === 'l' || key === 'ArrowRight') { if (i < stages.length - 1) select({ stage: stages[i + 1].id }); }
    else if (/^[1-8]$/.test(key)) select({ panel: PANELS[Number(key) - 1] });
    else if (key === '[' || key === ']') moveRevision(run, key === '[' ? -1 : 1);
    else if (key === 'i') { select({ panel: 'Conversation' }); $('message').focus(); }
    else if (key === 's') { const other = document.querySelector(`input[name="recipient"][value="${recipient() === 'supervisor' ? 'worker' : 'supervisor'}"]`); other.checked = true; updateSendLabel(); say(`Messages go to the ${other.value}`); }
    else if (key === 'n') newRunDialog();
    else if (key === 'r' && run) rewindDialog(run);
    else if (key === 'a' && run) approveFlow(run);
    else if (key === 'x' && run) cancelDialog(run);
    else if (key === 'g') openPullRequests(run);
    else if (key === 'o' && run) {
      const stage = currentStage(rev);
      const files = stage && resultOf(stage) ? resultOf(stage).artifacts : [];
      if (files && files.length) openReader(files, 0, `${run.name} · ${stage.id}`);
      else say('This stage has no artifacts yet.', true);
    }
    else if (key === 'd' && run) { const stage = currentStage(rev); select({ panel: 'Changes' }); if (stage && stage.checkpoint) loadDiff(run, rev, stage); }
    else if (key === '?') helpDialog();
    else handled = false;
    if (handled) e.preventDefault();
  });

  function moveRevision(run, step) {
    if (!run) return;
    const ids = run.revisions.map((v) => v.id);
    const at = ids.indexOf(viewRevision(run).id);
    const next = ids[Math.max(0, Math.min(ids.length - 1, at + step))];
    select({ revision: next === run.current_revision ? null : next, stagePicked: false });
  }

  // ---------- themes ----------

  const THEME_KEY = 'envctl-web-theme';
  function applyTheme(name, palettes) {
    const root = document.documentElement;
    for (const v of ['--ground', '--strip', '--raised', '--ink', '--muted', '--rule', '--attention', '--running', '--done', '--failed', '--idle', '--focus', '--add', '--del']) root.style.removeProperty(v);
    root.removeAttribute('data-theme');
    if (name === 'light' || name === 'dark') root.dataset.theme = name;
    const p = palettes.find((x) => x.name === name);
    if (p) {
      root.dataset.theme = p.dark ? 'dark' : 'light';
      const set = (k, v) => root.style.setProperty(k, v);
      set('--ground', p.background);
      set('--strip', `color-mix(in srgb, ${p.background} 72%, ${p.surface})`);
      set('--raised', `color-mix(in srgb, ${p.background} 88%, ${p.dark ? p.text : '#ffffff'} ${p.dark ? '4%' : '12%'})`);
      set('--ink', p.text); set('--muted', p.muted); set('--rule', p.line);
      set('--attention', p.warn); set('--running', p.accent); set('--done', p.success); set('--failed', p.danger);
      set('--idle', p.muted); set('--focus', p.accent);
      set('--add', `color-mix(in srgb, ${p.success} 18%, ${p.background})`);
      set('--del', `color-mix(in srgb, ${p.danger} 18%, ${p.background})`);
    }
  }
  async function setupThemes() {
    const pick = $('theme');
    let palettes = [];
    try { palettes = await api('/api/themes'); } catch { /* the built-in look still works */ }
    pick.replaceChildren(
      h('option', { value: 'system', text: 'envctl, follow system' }),
      h('option', { value: 'light', text: 'envctl light' }),
      h('option', { value: 'dark', text: 'envctl dark' }),
      h('optgroup', { label: 'Terminal dashboard themes' }, palettes.map((p) => h('option', { value: p.name, text: p.name }))));
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

  $('new-run').addEventListener('click', newRunDialog);
  $('help-open').addEventListener('click', helpDialog);
  $('reader-close').addEventListener('click', closeReader);
  $('menu').addEventListener('click', () => {
    const open = !$('runs').classList.contains('open');
    $('runs').classList.toggle('open', open);
    $('menu').setAttribute('aria-expanded', String(open));
  });
  function closeRunsMenu() {
    $('runs').classList.remove('open');
    $('menu').setAttribute('aria-expanded', 'false');
  }
  document.addEventListener('selectionchange', () => {
    const sel = window.getSelection();
    if (ui.pending && (!sel || sel.isCollapsed)) render();
  });
  window.addEventListener('hashchange', () => { readHash(); render(); });
  setInterval(() => { if (ui.state) document.querySelectorAll('time[datetime]').forEach((el) => { el.textContent = ago(el.getAttribute('datetime')); }); }, 20000);

  function connect() {
    const source = new EventSource('/api/stream');
    source.addEventListener('state', (e) => {
      ui.state = JSON.parse(e.data);
      ui.link = 'ok';
      render();
    });
    source.onerror = () => {
      ui.link = 'reconnecting';
      renderBar();
    };
  }

  readHash();
  setupThemes();
  render();
  connect();
})();
