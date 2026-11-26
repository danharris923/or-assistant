'use strict';

const $ = id => document.getElementById(id);
const el = {};
for (const id of ['model-button', 'model-name', 'model-price', 'model-panel', 'model-search', 'tools-only', 'model-count', 'model-list',
    'ask-toggle', 'cost', 'new-chat', 'settings-button', 'chat', 'empty', 'whoami', 'key-needed', 'key-needed-button', 'suggestions',
    'items', 'composer-form', 'input', 'send', 'stop', 'status', 'settings', 'key-status', 'key-input', 'key-save', 'key-remove',
    'key-msg', 'balance', 'settings-user', 'version']) el[id] = $(id);

const PREFERRED_MODELS = ['anthropic/claude-sonnet-5.5', 'openai/gpt-6.2-sol', 'anthropic/claude-haiku-5.5'];
const TOOL_ICONS = {run_command: 'i-terminal', read_file: 'i-file', write_file: 'i-file', edit_file: 'i-edit', list_dir: 'i-folder'};
const STATUS_LABELS = {pending_approval: 'Waiting for you', running: 'Running…', done: 'Done', error: 'Failed', denied: 'Denied', cancelled: 'Cancelled'};

let state = {items: [], seq: 0, running: false, total_cost: 0};
let ready = false, queued = [];
let models = [], modelById = new Map(), activeRow = -1, visibleModels = [];
const openDetails = new Set();

// ---------- helpers ----------

async function api(path, body) {
    const opts = body === undefined ? {} : {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)};
    const res = await fetch('/api/' + path, opts);
    const type = res.headers.get('content-type') || '';
    const data = type.includes('json') ? await res.json() : {error: (await res.text()).trim()};
    if (!res.ok) throw new Error(data.error || res.statusText);
    return data;
}

function svgIcon(id, cls) {
    const ns = 'http://www.w3.org/2000/svg';
    const svg = document.createElementNS(ns, 'svg'), use = document.createElementNS(ns, 'use');
    use.setAttribute('href', '#' + id);
    svg.append(use);
    if (cls) svg.setAttribute('class', cls);
    return svg;
}

function h(tag, cls, text) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text !== undefined) node.textContent = text;
    return node;
}

function fmtMoney(n) {
    if (!n) return '$0';
    if (n < 0.01) return '$' + n.toFixed(4);
    if (n < 1) return '$' + n.toFixed(3);
    return '$' + n.toFixed(2);
}

function fmtRate(n) {
    if (n >= 10) return n.toFixed(0);
    if (n >= 1) return n.toFixed(2).replace(/\.?0+$/, '');
    return n.toFixed(3).replace(/\.?0+$/, '');
}

function fmtPrice(m) {
    if (!m) return '';
    if (m.in < 0 || m.out < 0) return 'price varies';
    if (m.in === 0 && m.out === 0) return 'Free';
    return '$' + fmtRate(m.in) + ' in · $' + fmtRate(m.out) + ' out /M';
}

function fmtCtx(n) {
    if (!n) return '';
    if (n >= 1e6) return (n / 1e6).toFixed(n % 1e6 ? 1 : 0) + 'M ctx';
    return Math.round(n / 1000) + 'K ctx';
}

function setStatus(text, isError) {
    el.status.textContent = text || '';
    el.status.classList.toggle('err-text', !!isError);
}

function showError(err) { setStatus(err.message || String(err), true); }

function nearBottom() { return el.chat.scrollHeight - el.chat.scrollTop - el.chat.clientHeight < 120; }
function scrollDown() { el.chat.scrollTop = el.chat.scrollHeight; }

// ---------- live state ----------

function connect() {
    const es = new EventSource('/api/events');
    es.onopen = () => {
        ready = false;
        queued = [];
        api('state').then(s => {
            state = s;
            ready = true;
            renderAll();
            for (const ev of queued) handleEvent(ev);
            queued = [];
            setStatus('');
        }).catch(showError);
    };
    es.onmessage = e => {
        const ev = JSON.parse(e.data);
        if (!ready) queued.push(ev); else handleEvent(ev);
    };
    es.onerror = () => { if (es.readyState !== EventSource.OPEN) setStatus('Reconnecting to the assistant…', true); };
}

function handleEvent(ev) {
    if (ev.seq <= state.seq) return;
    state.seq = ev.seq;
    const stick = nearBottom();
    switch (ev.type) {
    case 'item_add':
    case 'item_update':
        state.items[ev.index] = ev.item;
        renderItem(ev.index);
        break;
    case 'delta': {
        const item = state.items[ev.index];
        if (item) { item.text = (item.text || '') + ev.text; scheduleRender(ev.index); }
        break;
    }
    case 'reset':
        state.items = [];
        openDetails.clear();
        el.items.replaceChildren();
        break;
    }
    state.running = ev.running;
    state.total_cost = ev.total_cost;
    renderChrome();
    if (stick) scrollDown();
}

const pendingRenders = new Set();
function scheduleRender(index) {
    if (!pendingRenders.size) requestAnimationFrame(() => {
        const stick = nearBottom();
        for (const i of pendingRenders) renderItem(i);
        pendingRenders.clear();
        if (stick) scrollDown();
    });
    pendingRenders.add(index);
}

// ---------- transcript rendering ----------

function renderAll() {
    el.items.replaceChildren();
    state.items.forEach((_, i) => renderItem(i));
    renderChrome();
    scrollDown();
}

function renderItem(index) {
    const item = state.items[index];
    if (!item) return;
    const node = buildItem(item, index);
    node.dataset.index = index;
    const old = el.items.querySelector(':scope > [data-index="' + index + '"]');
    if (old) old.replaceWith(node);
    else {
        const tail = el.items.querySelector(':scope > .thinking');
        el.items.insertBefore(node, tail);
    }
}

function buildItem(item, index) {
    switch (item.kind) {
    case 'user':
        return h('div', 'msg user', item.text);
    case 'assistant': {
        const node = h('div', 'msg assistant');
        node.innerHTML = window.renderMarkdown(item.text || '');
        return node;
    }
    case 'error':
        return h('div', 'notice error', item.text);
    case 'meta': {
        const m = modelById.get(item.model);
        const parts = [m ? m.name : item.model];
        if (item.tokens) parts.push(item.tokens.toLocaleString() + ' tokens');
        if (item.cost) parts.push(fmtMoney(item.cost));
        return h('div', 'meta', parts.join(' · '));
    }
    case 'tool':
        return buildTool(item, index);
    }
    return h('div');
}

function detailsBlock(index, key, summary, content, startOpen) {
    const id = index + ':' + key;
    const d = h('details');
    d.open = openDetails.has(id) || !!startOpen;
    d.addEventListener('toggle', () => { if (d.open) openDetails.add(id); else openDetails.delete(id); });
    d.append(h('summary', '', summary), content);
    return d;
}

function buildTool(item, index) {
    const args = item.args && typeof item.args === 'object' ? item.args : {};
    const status = item.status || 'running';
    const pending = status === 'pending_approval';
    const card = h('div', 'tool ' + status);
    const head = h('div', 'tool-head');
    const title = h('span', 'title');
    const code = text => h('code', '', text);
    switch (item.name) {
    case 'run_command': title.append(code((args.command || '').split('\n')[0])); break;
    case 'read_file': title.append('Read ', code(args.path || '')); break;
    case 'write_file': title.append('Write ', code(args.path || '')); break;
    case 'edit_file': title.append('Edit ', code(args.path || '')); break;
    case 'list_dir': title.append('List ', code(args.path || '~')); break;
    default: title.append(item.name || 'tool');
    }
    head.append(svgIcon(TOOL_ICONS[item.name] || 'i-terminal'), title, h('span', 'status-badge ' + status, STATUS_LABELS[status] || status));
    card.append(head);

    const body = h('div', 'tool-body');
    const pre = (text, cls) => h('pre', cls || '', text);
    if (item.name === 'run_command') {
        const cmd = args.command || '';
        if (cmd.includes('\n') || cmd.length > 70) body.append(pre(cmd));
        if (pending && args.workdir) body.append(h('div', 'small muted', 'in ' + args.workdir));
    } else if (item.name === 'write_file') {
        const content = args.content || '';
        const lines = content.split('\n').length;
        body.append(detailsBlock(index, 'content', 'Content (' + lines + ' line' + (lines === 1 ? '' : 's') + ')', pre(content), pending));
    } else if (item.name === 'edit_file') {
        const diff = h('div');
        diff.append(pre(args.old_text || '', 'del'), pre(args.new_text || '', 'add'));
        if (args.replace_all) diff.append(h('div', 'small muted', 'Replaces every occurrence'));
        body.append(pending ? diff : detailsBlock(index, 'diff', 'Change', diff, false));
    }
    if (item.output) {
        const failed = status === 'error';
        body.append(detailsBlock(index, 'out', failed ? 'Error' : 'Output', pre(item.output), failed));
    }
    if (pending) {
        const row = h('div', 'approve-row');
        const deny = h('button', 'ghost', 'Deny'), allow = h('button', 'primary', 'Allow');
        deny.type = allow.type = 'button';
        const decide = ok => {
            deny.disabled = allow.disabled = true;
            api('approve', {index, allow: ok}).catch(showError);
        };
        deny.onclick = () => decide(false);
        allow.onclick = () => decide(true);
        row.append(h('span', 'small muted', item.name === 'run_command' ? 'Run this command?' : 'Make this change?'), deny, allow);
        body.append(row);
        setTimeout(() => allow.focus(), 0);
    }
    if (body.childNodes.length) card.append(body);
    return card;
}

function renderChrome() {
    const empty = !state.items.length;
    el.empty.hidden = !empty;
    el['key-needed'].hidden = !!state.has_key;
    el.suggestions.hidden = !state.has_key;
    el.whoami.textContent = (state.user || 'user') + '@' + (state.host || 'localhost');
    el['ask-toggle'].checked = state.approval !== 'auto';
    el.cost.textContent = state.total_cost ? fmtMoney(state.total_cost) : '';
    el.send.hidden = !!state.running;
    el.stop.hidden = !state.running;
    el['new-chat'].disabled = !!state.running;

    // streaming cursor on the newest answer, bouncing dots while waiting on the model
    const last = state.items[state.items.length - 1];
    el.items.querySelectorAll('.streaming').forEach(n => n.classList.remove('streaming'));
    const waiting = state.items.some(it => it.kind === 'tool' && it.status === 'pending_approval');
    let dots = el.items.querySelector(':scope > .thinking');
    if (state.running && last && last.kind === 'assistant') {
        el.items.querySelector(':scope > [data-index="' + (state.items.length - 1) + '"]')?.classList.add('streaming');
    }
    const showDots = state.running && !waiting && !(last && last.kind === 'assistant');
    if (showDots && !dots) {
        dots = h('div', 'thinking');
        dots.append(h('span'), h('span'), h('span'));
        el.items.append(dots);
    } else if (!showDots && dots) dots.remove();

    if (el.status.classList.contains('err-text')) return;
    if (waiting) setStatus('Waiting for your approval');
    else if (state.running) setStatus('Working… press Stop to interrupt');
    else setStatus('');
}

// ---------- model picker ----------

async function loadModels() {
    try {
        const res = await api('models');
        models = res.models;
        modelById = new Map(models.map(m => [m.id, m]));
        if (!state.model) {
            const pick = PREFERRED_MODELS.map(id => modelById.get(id)).find(Boolean) || models.find(m => m.tools);
            if (pick) await chooseModel(pick.id, true);
        }
        renderModelButton();
        state.items.forEach((it, i) => { if (it.kind === 'meta') renderItem(i); });
    } catch (err) {
        el['model-name'].textContent = state.model || 'Models unavailable';
        el['model-price'].textContent = err.message;
    }
}

function renderModelButton() {
    const m = modelById.get(state.model);
    el['model-name'].textContent = m ? m.name : (state.model || 'Choose a model');
    el['model-price'].textContent = m ? [fmtPrice(m), fmtCtx(m.context), m.tools ? '' : 'no tools'].filter(Boolean).join(' · ') : '';
}

function renderModelList() {
    const terms = el['model-search'].value.toLowerCase().split(/\s+/).filter(Boolean);
    const toolsOnly = el['tools-only'].checked;
    visibleModels = models.filter(m => (!toolsOnly || m.tools) &&
        terms.every(t => (m.id + ' ' + m.name).toLowerCase().includes(t)));
    el['model-count'].textContent = visibleModels.length + ' model' + (visibleModels.length === 1 ? '' : 's');
    const frag = document.createDocumentFragment();
    visibleModels.slice(0, 250).forEach((m, i) => {
        const row = h('button', 'model-row' + (m.id === state.model ? ' selected' : '') + (i === activeRow ? ' active' : ''));
        row.type = 'button';
        row.setAttribute('role', 'option');
        const price = h('span', 'model-row-price', fmtPrice(m));
        if (price.textContent === 'Free') { price.textContent = ''; price.append(h('span', 'badge free', 'Free')); }
        const sub = h('span', 'model-row-price', [fmtCtx(m.context), m.tools ? '' : 'no tools'].filter(Boolean).join(' · '));
        row.append(h('span', 'model-row-name', m.name), price, h('span', 'model-row-id', m.id), sub);
        row.onclick = () => { chooseModel(m.id); closeModelPanel(); };
        frag.append(row);
    });
    if (!visibleModels.length) frag.append(h('div', 'small muted', 'No models match.'));
    el['model-list'].replaceChildren(frag);
}

async function chooseModel(id, quiet) {
    try {
        await api('settings', {model: id});
        state.model = id;
        renderModelButton();
        if (!quiet) setStatus('');
    } catch (err) { showError(err); }
}

function openModelPanel() {
    el['model-panel'].hidden = false;
    el['model-button'].setAttribute('aria-expanded', 'true');
    el['model-search'].value = '';
    activeRow = -1;
    renderModelList();
    el['model-search'].focus();
    el['model-list'].querySelector('.selected')?.scrollIntoView({block: 'nearest'});
}

function closeModelPanel() {
    el['model-panel'].hidden = true;
    el['model-button'].setAttribute('aria-expanded', 'false');
}

function moveActive(delta) {
    const n = Math.min(visibleModels.length, 250);
    if (!n) return;
    activeRow = (activeRow + delta + n) % n;
    el['model-list'].querySelectorAll('.model-row').forEach((r, i) => r.classList.toggle('active', i === activeRow));
    el['model-list'].children[activeRow]?.scrollIntoView({block: 'nearest'});
}

el['model-button'].onclick = () => el['model-panel'].hidden ? openModelPanel() : closeModelPanel();
el['model-search'].oninput = () => { activeRow = 0; renderModelList(); };
el['tools-only'].onchange = () => { activeRow = -1; renderModelList(); };
el['model-search'].onkeydown = e => {
    if (e.key === 'ArrowDown') { e.preventDefault(); moveActive(1); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); moveActive(-1); }
    else if (e.key === 'Enter') {
        e.preventDefault();
        const m = visibleModels[Math.max(activeRow, 0)];
        if (m) { chooseModel(m.id); closeModelPanel(); }
    } else if (e.key === 'Escape') closeModelPanel();
};
document.addEventListener('mousedown', e => {
    if (!el['model-panel'].hidden && !$('model-picker').contains(e.target)) closeModelPanel();
});

// ---------- composer ----------

function autosize() {
    el.input.style.height = 'auto';
    el.input.style.height = Math.min(el.input.scrollHeight, 220) + 'px';
}

async function send(text) {
    text = (text ?? el.input.value).trim();
    if (!text) return;
    if (state.running) { setStatus('Still working. Wait for the answer or press Stop.', true); return; }
    if (!state.has_key) { openSettings(); return; }
    setStatus('');
    try {
        await api('chat', {text});
        el.input.value = '';
        autosize();
    } catch (err) { showError(err); }
}

el['composer-form'].onsubmit = e => { e.preventDefault(); send(); };
el.input.addEventListener('input', autosize);
el.input.addEventListener('keydown', e => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
});
el.stop.onclick = () => api('stop', {}).catch(showError);
el['new-chat'].onclick = () => api('new', {}).then(() => { setStatus(''); el.input.focus(); }).catch(showError);
el.suggestions.addEventListener('click', e => { if (e.target.classList.contains('chip')) send(e.target.textContent); });
el['ask-toggle'].onchange = () => {
    const approval = el['ask-toggle'].checked ? 'ask' : 'auto';
    api('settings', {approval}).then(r => {
        state.approval = r.approval;
        setStatus(approval === 'auto' ? 'Commands and file changes now run without asking.' : 'You will approve each command and file change.');
    }).catch(err => { el['ask-toggle'].checked = !el['ask-toggle'].checked; showError(err); });
};

// ---------- settings ----------

function renderKeyStatus() {
    const s = el['key-status'];
    s.className = 'small ' + (state.has_key ? 'ok-text' : 'muted');
    s.textContent = state.has_key
        ? 'Key in use: ' + state.key_hint + (state.key_source === 'environment' ? ' (from OPENROUTER_API_KEY)' : '')
        : 'No key set yet.';
    el['key-remove'].hidden = !state.has_key || state.key_source === 'environment';
}

async function loadBalance() {
    if (!state.has_key) { el.balance.textContent = 'Add a key to see usage.'; return; }
    el.balance.textContent = 'Loading…';
    try {
        const b = await api('balance');
        const parts = [fmtMoney(b.usage || 0) + ' spent with this key'];
        if (b.limit != null) parts.push('limit ' + fmtMoney(b.limit) + (b.limit_remaining != null ? ' (' + fmtMoney(b.limit_remaining) + ' left)' : ''));
        else parts.push('no spending limit set');
        if (b.is_free_tier) parts.push('free tier');
        el.balance.textContent = parts.join(' · ');
    } catch (err) { el.balance.textContent = err.message; }
}

function openSettings() {
    renderKeyStatus();
    el['key-msg'].textContent = '';
    el['key-input'].value = '';
    el['settings-user'].textContent = state.user || 'this user';
    el.version.textContent = 'OR Assistant ' + (state.version || '') + ' · running as ' + (state.user || '?') + '@' + (state.host || '?') + (state.root ? ' (root!)' : '');
    el.settings.showModal();
    loadBalance();
    if (!state.has_key) el['key-input'].focus();
}

async function saveKey(key) {
    el['key-save'].disabled = el['key-remove'].disabled = true;
    el['key-msg'].className = 'small muted';
    el['key-msg'].textContent = key ? 'Checking key with OpenRouter…' : 'Removing…';
    try {
        const r = await api('key', {key});
        Object.assign(state, r);
        el['key-input'].value = '';
        el['key-msg'].className = 'small ok-text';
        el['key-msg'].textContent = key ? 'Key saved.' : 'Key removed.';
        renderKeyStatus();
        renderChrome();
        loadBalance();
    } catch (err) {
        el['key-msg'].className = 'small err-text';
        el['key-msg'].textContent = err.message;
    } finally {
        el['key-save'].disabled = el['key-remove'].disabled = false;
    }
}

el['settings-button'].onclick = openSettings;
el['key-needed-button'].onclick = openSettings;
el['key-save'].onclick = () => { const k = el['key-input'].value.trim(); if (k) saveKey(k); };
el['key-input'].onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); el['key-save'].click(); } };
el['key-remove'].onclick = () => saveKey('');

// ---------- start ----------

api('state').then(s => {
    state = s;
    renderAll();
    loadModels();
    connect();
    el.input.focus();
}).catch(showError);
