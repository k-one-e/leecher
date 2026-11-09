// Leecher main page. External file so the CSP can forbid inline scripts.
'use strict';

const $ = (id) => document.getElementById(id);
const FLAGS = document.body.dataset;
const CSRF_TOKEN = FLAGS.csrf || '';
const VIDEO_HOSTS = (FLAGS.videoHosts || '').split(' ').filter(Boolean);
const ME = (document.querySelector('.menu-head strong')?.textContent || '').trim().toLowerCase();

// ---------------------------------------------------------------- helpers

function apiFetch(url, opts = {}) {
    const headers = Object.assign({}, opts.headers || {});
    if (opts.method && opts.method !== 'GET') headers['X-CSRF-Token'] = CSRF_TOKEN;
    return fetch(url, Object.assign({}, opts, { headers })).then(r => {
        if (r.status === 401) {
            location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search);
            throw new Error('unauthorized');
        }
        return r;
    });
}

function el(tag, attrs, ...children) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
        if (v === undefined || v === null || v === false) continue;
        if (k === 'class') e.className = v;
        else if (k === 'text') e.textContent = v;
        else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
        else if (k === 'dataset') Object.assign(e.dataset, v);
        else e.setAttribute(k, v === true ? '' : v);
    }
    for (const c of children.flat()) if (c != null && c !== false) e.append(c);
    return e;
}

// Static icon markup only — never interpolated with user data.
const ICONS = {
    link: '<path d="M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7-7l-1.7 1.7"/><path d="M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7 7l1.7-1.7"/>',
    video: '<rect x="3" y="5" width="18" height="14" rx="3"/><path d="m10 9 5 3-5 3z"/>',
    file: '<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/>',
    torrent: '<path d="M6 3v7a6 6 0 0 0 12 0V3"/><path d="M6 7h4M14 7h4"/><path d="M12 16v5"/>',
    invalid: '<circle cx="12" cy="12" r="9"/><path d="M12 8v4.5M12 16h.01"/>',
    check: '<path d="M20 6 9 17l-5-5"/>',
    x: '<path d="M18 6 6 18M6 6l12 12"/>',
    stop: '<rect x="6" y="6" width="12" height="12" rx="2"/>',
    pause: '<rect x="6" y="5" width="4" height="14" rx="1"/><rect x="14" y="5" width="4" height="14" rx="1"/>',
    play: '<path d="M7 4.5v15l12-7.5z"/>',
    retry: '<path d="M3 11a9 9 0 1 1 2.6 7M3 4v7h7"/>',
    trash: '<path d="M4 7h16M10 11v6M14 11v6M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12M9 7V4h6v3"/>',
    download: '<path d="M12 3v12M7 10l5 5 5-5M5 21h14"/>',
    server: '<rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01"/>',
    shield: '<path d="M12 3 5 6v5c0 4.4 3 8.3 7 10 4-1.7 7-5.6 7-10V6z"/>',
    disk: '<ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>',
};
function icon(name, cls) {
    const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    s.setAttribute('viewBox', '0 0 24 24');
    s.setAttribute('aria-hidden', 'true');
    if (cls) s.setAttribute('class', cls);
    s.innerHTML = ICONS[name] || '';
    return s;
}

function fmtBytes(n) {
    if (!(n > 0)) return '';
    const u = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
    return (n / 1024 ** i).toFixed(i >= 3 ? 2 : i >= 2 ? 1 : 0) + ' ' + u[i];
}
function fmtDuration(sec) {
    sec = Math.round(Number(sec) || 0);
    if (sec <= 0) return '';
    const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
    const p = (n) => String(n).padStart(2, '0');
    return h ? `${h}:${p(m)}:${p(s)}` : `${m}:${p(s)}`;
}
function fmtEta(sec) {
    if (!(sec > 0)) return '';
    const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60);
    return h ? `${h}h ${m}m left` : m ? `${m}m left` : `${sec}s left`;
}
// yt-dlp writes ASCII-safe names; make them readable again.
function prettyName(name) {
    if (!name) return '';
    const dot = name.lastIndexOf('.');
    return dot > 0 ? name.slice(0, dot).replace(/_/g, ' ').trim() + name.slice(dot) : name;
}
function shortPath(p) {
    const parts = (p || '').split('/').filter(Boolean);
    return p && p.length > 40 && parts.length > 2 ? '…/' + parts.slice(-2).join('/') : p;
}
function expiresIn(iso) {
    const s = Math.round((Date.parse(iso) - Date.now()) / 1000);
    if (s <= 60) return 'Expires soon';
    if (s < 3600) return `Expires in ${Math.round(s / 60)} min`;
    if (s < 172800) return `Expires in ${Math.round(s / 3600)} h`;
    return `Expires in ${Math.round(s / 86400)} days`;
}
function timeAgo(ms) {
    const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
    if (s < 45) return 'just now';
    if (s < 3600) return `${Math.round(s / 60)} min ago`;
    if (s < 86400) return `${Math.round(s / 3600)} h ago`;
    return new Date(ms).toLocaleDateString();
}

function toast(msg, isError) {
    const t = el('div', { class: 'toast' + (isError ? ' is-error' : ''), role: 'status' }, icon(isError ? 'invalid' : 'check'), el('span', { text: msg }));
    $('toasts').append(t);
    setTimeout(() => { t.classList.add('is-leaving'); setTimeout(() => t.remove(), 250); }, isError ? 6000 : 3500);
}

function setLoading(btn, on) {
    btn.classList.toggle('is-loading', on);
    btn.disabled = on;
}

// ---------------------------------------------------------------- composer

const input = $('composer-input');
const fetchBtn = $('fetch-btn');
const KIND_LABELS = { none: 'Link', video: 'Video', file: 'File', torrent: 'Torrent', invalid: 'Not supported' };

function detectKind(raw) {
    const s = raw.trim();
    if (!s) return 'none';
    if (/^magnet:/i.test(s)) return FLAGS.torrents ? 'torrent' : 'invalid';
    let u;
    try { u = new URL(s); } catch (_) { return 'invalid'; }
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return 'invalid';
    const host = u.hostname.toLowerCase();
    if (VIDEO_HOSTS.some(h => host === h || host.endsWith('.' + h))) return 'video';
    if (/\.torrent$/i.test(u.pathname) && FLAGS.torrents) return 'torrent';
    return FLAGS.direct ? 'file' : 'invalid';
}

function updateComposer() {
    const kind = detectKind(input.value);
    const badge = $('kind-badge');
    badge.dataset.kind = kind;
    badge.querySelector('.kind-icon').innerHTML = ICONS[kind === 'none' ? 'link' : kind];
    $('kind-label').textContent = KIND_LABELS[kind];
    const empty = !input.value.trim();
    $('paste-btn').hidden = !empty || !navigator.clipboard || !navigator.clipboard.readText;
    $('clear-btn').hidden = empty;
    return kind;
}

function showAlert(msg) {
    $('alert-text').textContent = msg;
    $('alert').hidden = false;
}
function hideAlert() { $('alert').hidden = true; }

let fetchAbort = null;

function fetchFormats() {
    const url = input.value.trim();
    if (!url) { input.focus(); return; }
    if (detectKind(url) === 'invalid') {
        showAlert(/^magnet:/i.test(url) ? 'Torrents are not enabled on this server.' : 'That doesn’t look like a link leecher can handle. Paste an http(s) URL or a magnet link.');
        return;
    }
    hideAlert();
    closePreview();
    if (fetchAbort) fetchAbort.abort();
    fetchAbort = new AbortController();
    setLoading(fetchBtn, true);
    fetchBtn.querySelector('.btn-label').textContent = 'Fetch';

    const st = { url, resolvedUrl: '', media: null, formats: null };
    apiFetch('/api/formats', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url }),
        signal: fetchAbort.signal,
    }).then(async r => {
        const ct = (r.headers.get('Content-Type') || '').toLowerCase();
        if (!r.ok || !ct.includes('text/event-stream')) {
            const data = await r.json().catch(() => ({}));
            throw new Error(data.error || `Request failed (HTTP ${r.status}).`);
        }
        const reader = r.body.getReader();
        const decoder = new TextDecoder();
        let buf = '', event = '';
        for (;;) {
            const { done, value } = await reader.read();
            buf += done ? '\n' : decoder.decode(value, { stream: true });
            const lines = buf.split('\n');
            buf = lines.pop();
            for (const line of lines) {
                if (line.startsWith('event: ')) { event = line.slice(7).trim(); continue; }
                if (!line.startsWith('data: ')) continue;
                const raw = line.slice(6);
                if (event === 'queue') {
                    const pos = JSON.parse(raw).position;
                    fetchBtn.classList.remove('is-loading');
                    fetchBtn.querySelector('.btn-label').textContent = `#${pos} in line`;
                } else if (event === 'log') {
                    fetchBtn.classList.add('is-loading');
                } else if (event === 'media') {
                    st.media = JSON.parse(raw);
                } else if (event === 'resolved_url') {
                    st.resolvedUrl = JSON.parse(raw).url || '';
                } else if (event === 'formats') {
                    st.formats = JSON.parse(raw).formats || [];
                } else if (event === 'error') {
                    throw new Error(JSON.parse(raw).error || 'Could not read this link.');
                }
                event = '';
            }
            if (done) break;
        }
        if (!st.formats || !st.formats.length) throw new Error('No downloadable formats were found for this link.');
        openPreview(st);
    }).catch(err => {
        if (err.name === 'AbortError' || err.message === 'unauthorized') return;
        showAlert(err.message || 'Could not reach the server.');
    }).finally(() => {
        setLoading(fetchBtn, false);
        fetchBtn.querySelector('.btn-label').textContent = 'Fetch';
    });
}

$('composer').addEventListener('submit', e => { e.preventDefault(); fetchFormats(); });
input.addEventListener('input', () => { updateComposer(); hideAlert(); if (!$('preview').hidden) closePreview(); });
input.addEventListener('paste', () => {
    // Pasting into an empty box is a strong "go" signal — fetch right away.
    const wasEmpty = !input.value.trim();
    setTimeout(() => {
        const k = updateComposer();
        if (wasEmpty && (k === 'video' || k === 'file' || k === 'torrent')) fetchFormats();
    }, 0);
});
$('clear-btn').addEventListener('click', () => { input.value = ''; updateComposer(); hideAlert(); closePreview(); input.focus(); });
$('paste-btn').addEventListener('click', async () => {
    try {
        const text = (await navigator.clipboard.readText()).trim();
        if (!text) return;
        input.value = text;
        if (updateComposer() !== 'invalid') fetchFormats();
    } catch (_) { input.focus(); }
});
$('alert-close').addEventListener('click', hideAlert);

// ---------------------------------------------------------------- preview

const PRESET_LABELS = {
    'bv*+ba/b': ['Best quality', 'max resolution'],
    'bv*[height<=1080]+ba/b[height<=1080]/bv*+ba/b': ['Up to 1080p', 'recommended'],
    'ba': ['Audio only', 'MP3'],
};
let preview = null;

function previewKind(formats) {
    const id = formats[0].format_id;
    if (id === 'torrent') return 'torrent';
    if (id === 'direct') return 'file';
    if (id.startsWith('playlist:')) return 'playlist';
    return 'video';
}

function selectFormat(id) {
    preview.formatId = id;
    for (const c of $('quality-chips').children) c.setAttribute('aria-checked', String(c.dataset.id === id));
    const sel = $('more-formats');
    const inSelect = [...sel.options].some(o => o.value === id && o.value);
    sel.value = inSelect ? id : '';
    sel.classList.toggle('is-picked', inSelect);
}

function openPreview(st) {
    const kind = previewKind(st.formats);
    preview = { url: st.resolvedUrl || st.url, kind, formatId: st.formats[0].format_id, title: '' };
    const media = st.media || {};
    preview.title = media.title || '';

    // Thumbnail / type tile
    const thumb = $('thumb'), img = $('thumb-img'), ext = $('thumb-ext'), dur = $('thumb-duration');
    thumb.dataset.kind = kind === 'playlist' ? 'video' : kind;
    img.hidden = true; ext.hidden = true; dur.hidden = true;
    if (media.thumbnail) {
        img.src = media.thumbnail;
        img.alt = media.title ? `Thumbnail: ${media.title}` : '';
        img.hidden = false;
        img.onerror = () => { img.hidden = true; };
    } else {
        ext.textContent = kind === 'torrent' ? 'Torrent' : (st.formats[0].ext || 'File');
        ext.hidden = false;
    }
    const d = fmtDuration(media.duration);
    if (d) { dur.textContent = d; dur.hidden = false; }

    $('preview-title').textContent = media.title || st.url;
    const sub = [];
    if (kind === 'file') {
        const f = st.formats[0];
        sub.push(media.uploader ? media.uploader.replace(' · direct download', '') : '');
        if (f.filesize > 0) sub.push(fmtBytes(f.filesize));
        if (f.ext) sub.push('.' + f.ext);
    } else if (kind === 'torrent') {
        sub.push(media.uploader || 'Torrent');
    } else if (media.uploader) {
        sub.push(media.uploader);
    }
    $('preview-sub').textContent = sub.filter(Boolean).join(' · ');

    // Quality
    const chips = $('quality-chips'), sel = $('more-formats');
    chips.replaceChildren(); sel.replaceChildren();
    const showQuality = kind === 'video' || kind === 'playlist';
    $('quality-field').hidden = !showQuality;
    if (showQuality) {
        const presets = st.formats.filter(f => PRESET_LABELS[f.format_id] || f.format_id.startsWith('playlist:'));
        const rest = st.formats.filter(f => !presets.includes(f));
        presets.forEach(f => {
            const [label, hint] = PRESET_LABELS[f.format_id] || ['Whole playlist', 'ZIP of MP3s'];
            chips.append(el('button', { type: 'button', class: 'chip', role: 'radio', 'aria-checked': 'false', dataset: { id: f.format_id }, onclick: () => selectFormat(f.format_id) },
                label, el('small', { text: hint })));
        });
        $('more-formats-wrap').hidden = !rest.length;
        if (rest.length) {
            sel.append(el('option', { value: '', text: `More formats (${rest.length})…` }));
            for (const [label, match] of [['Video', f => f.has_video], ['Audio only', f => !f.has_video]]) {
                const items = rest.filter(match);
                if (!items.length) continue;
                const g = el('optgroup', { label });
                items.forEach(f => g.append(el('option', { value: f.format_id, text: f.label })));
                sel.append(g);
            }
        }
        const def = presets.find(f => f.format_id.includes('1080')) || presets[0] || rest[0];
        selectFormat(def.format_id);
    }

    const label = { video: 'Download', playlist: 'Download ZIP', torrent: FLAGS.transmission ? 'Add to Transmission' : 'Download torrent', file: 'Download file' }[kind];
    $('start-label').textContent = label;
    $('start-btn').querySelector('svg').innerHTML = ICONS[kind === 'torrent' ? 'torrent' : 'download'];

    $('preview').hidden = false;
    $('start-btn').focus({ preventScroll: true });
    $('preview').scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}

function closePreview() {
    $('preview').hidden = true;
    preview = null;
}

$('more-formats').addEventListener('change', e => { if (e.target.value) selectFormat(e.target.value); });
$('cancel-preview').addEventListener('click', () => { closePreview(); input.focus(); });

$('start-btn').addEventListener('click', () => {
    if (!preview) return;
    const btn = $('start-btn');
    setLoading(btn, true);
    apiFetch('/api/download', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            url: preview.url,
            format_id: preview.formatId,
            title: preview.title,
        }),
    }).then(r => r.json()).then(data => {
        if (data.error) { showAlert(data.error); return; }
        if (data.torrent) {
            highlight.add('t:' + data.torrent.hash);
            toast(data.torrent.duplicate ? 'This torrent is already in Transmission' : 'Torrent added to Transmission');
        } else {
            highlight.add('j:' + data.job_id);
            toast(preview.kind === 'torrent' ? 'Torrent started — you can close this page' : 'Download started — you can close this page');
        }
        closePreview();
        input.value = '';
        updateComposer();
        setFilter('all');
        refreshActivity(true);
    }).catch(err => {
        if (err.message !== 'unauthorized') showAlert('Could not start the download. Is the server reachable?');
    }).finally(() => setLoading(btn, false));
});

// ---------------------------------------------------------------- activity

const list = $('activity-list');
const highlight = new Set();
const openLogs = new Set();
const confirming = new Map(); // item id -> confirm kind
const retrying = new Set();
let activity = { jobs: [], torrents: [], allJobs: false, allTorrents: false };
let filter = 'all';
let pollTimer = null;

const JOB_STATES = {
    queued: ['Queued', 'muted'], downloading: ['Downloading', 'active'], done: ['Done', 'done'],
    error: ['Failed', 'error'], cancelled: ['Stopped', 'muted'], paused: ['Paused', 'muted'],
};
const TORRENT_STATES = {
    stopped: ['Paused', 'muted', 'paused'], 'check-wait': ['Waiting to check', 'muted', 'queued'], checking: ['Checking', 'active', 'active'],
    'download-wait': ['Queued', 'muted', 'queued'], downloading: ['Downloading', 'active', 'active'],
    'seed-wait': ['Done', 'done', 'done'], seeding: ['Seeding', 'seed', 'done'],
};

function jobToItem(j) {
    const [label, tone] = j.file_removed ? ['Removed', 'muted'] : j.seeding ? ['Seeding', 'seed'] : j.status === 'done' ? ['Ready', 'done'] : JOB_STATES[j.status] || [j.status, 'muted'];
    const active = j.status === 'queued' || j.status === 'downloading';
    const meta = [el('span', { class: 'status', dataset: { tone }, text: j.status === 'queued' ? (j.kind === 'torrent' ? 'Finding peers' : `Queued #${j.queue_position || 1}`) : label })];
    if (j.seeding) meta.push(el('span', { text: `↑ ${fmtBytes(j.rate_up) || '0 B'}/s · ratio ${(j.ratio || 0).toFixed(2)}`, title: 'Sharing with other peers. Keep or Delete stops seeding.' }));
    if (j.status === 'downloading') meta.push(el('span', { text: j.message && j.message !== 'Starting download...' ? j.message.replace(/^Downloading\.\.\.\s*/, '') : `${Math.round(j.progress || 0)}%` }));
    if (j.status === 'paused' && j.message) meta.push(el('span', { text: j.message }));
    if (j.status === 'done' && j.warning) meta.push(el('span', { class: 'warn', text: j.warning, title: j.warning }));
    if (j.status === 'done' && j.file_removed) meta.push(el('span', { text: 'File no longer available' }));
    else if (j.status === 'done' && j.saved_to === 'library') meta.push(el('span', { class: 'kept', text: 'In library' }), el('span', { class: 'mono', text: shortPath(j.library_path), title: j.library_path }));
    else if (j.status === 'done' && j.expires_at) meta.push(el('span', { text: expiresIn(j.expires_at), title: 'Keep it to store it permanently in the library' }));
    if (j.owner && j.owner.toLowerCase() !== ME) meta.push(el('span', { text: `by ${j.owner}` }));
    meta.push(el('span', { text: timeAgo(Date.parse(j.created_at)) }));
    return {
        id: 'j:' + j.id, raw: j, kind: j.kind === 'direct' ? 'direct' : j.kind === 'torrent' ? 'torrent' : 'video', title: j.title || prettyName(j.display_name) || j.url,
        tooltip: [j.display_name, j.url].filter(Boolean).join('\n'), active, state: j.status === 'queued' ? 'queued' : j.status, progress: active || j.status === 'paused' ? j.progress || 0 : null,
        meta, error: j.status === 'error' || j.status === 'cancelled' ? j.error || '' : '', logs: j.logs, time: Date.parse(j.created_at) || 0,
        dead: j.status === 'error' || j.status === 'cancelled' || (j.status === 'done' && !!j.file_removed),
    };
}

function torrentToItem(t) {
    const [label, tone, state] = TORRENT_STATES[t.status] || [t.status, 'muted', 'paused'];
    const meta = [el('span', { class: 'status', dataset: { tone }, text: label })];
    const pct = `${(t.percent || 0).toFixed(t.percent >= 100 ? 0 : 1)}%`;
    meta.push(el('span', { text: t.size ? `${pct} of ${fmtBytes(t.size)}` : pct }));
    if (t.status === 'downloading') {
        if (t.rate_down) meta.push(el('span', { text: `↓ ${fmtBytes(t.rate_down)}/s` }));
        meta.push(el('span', { text: `${t.peers} peer${t.peers === 1 ? '' : 's'}` }));
        const eta = fmtEta(t.eta);
        if (eta) meta.push(el('span', { text: eta }));
    }
    if (t.rate_up) meta.push(el('span', { text: `↑ ${fmtBytes(t.rate_up)}/s` }));
    if (t.owner && t.owner.toLowerCase() !== ME) meta.push(el('span', { text: `by ${t.owner}` }));
    return {
        id: 't:' + t.hash, raw: t, kind: 'torrent', title: t.name || t.hash, tooltip: t.dir ? `${t.name}\n${t.dir}` : t.name,
        active: state === 'active' || state === 'queued', state, progress: t.percent < 100 ? t.percent : null,
        meta, error: t.error || '', time: (t.added || 0) * 1000,
    };
}

function actionButton(label, iconName, cls, onclick, ariaLabel) {
    return el('button', { type: 'button', class: `btn btn-sm ${cls}`, onclick, 'aria-label': ariaLabel || label }, iconName ? icon(iconName) : null, el('span', { text: label }));
}

function itemActions(item) {
    const pending = confirming.get(item.id);
    const cancel = () => { confirming.delete(item.id); renderActivity(); };
    if (pending === 'stop') {
        return [el('span', { class: 'item-meta', text: 'Stop and delete the partial file?' }),
            actionButton('Keep going', null, 'btn-ghost', cancel),
            actionButton('Stop', 'stop', 'btn-danger', () => stopJob(item))];
    }
    if (pending === 'delete') {
        return [el('span', { class: 'item-meta', text: 'Delete this file?' }),
            actionButton('Cancel', null, 'btn-ghost', cancel),
            actionButton('Delete', 'trash', 'btn-danger', () => jobFileAction(item, 'delete'))];
    }
    if (pending === 'remove') {
        return [actionButton('Cancel', null, 'btn-ghost', cancel),
            actionButton('Keep files', 'trash', 'btn-ghost', () => torrentAction(item, 'remove'), 'Remove torrent, keep files'),
            actionButton('Delete files', 'trash', 'btn-danger', () => torrentAction(item, 'remove-data'), 'Remove torrent and delete files')];
    }
    const out = [];
    // Transmission torrents have their own controls; built-in torrents are
    // ordinary jobs and fall through to the job actions below.
    if (item.id.startsWith('t:')) {
        const paused = item.raw.status === 'stopped';
        out.push(actionButton(paused ? 'Resume' : 'Pause', paused ? 'play' : 'pause', 'btn-ghost', () => torrentAction(item, paused ? 'start' : 'stop')));
        out.push(el('button', { type: 'button', class: 'icon-btn', title: 'Remove', 'aria-label': 'Remove torrent', onclick: () => { confirming.set(item.id, 'remove'); renderActivity(); } }, icon('trash')));
    } else if (item.dead) {
        if (item.raw.can_retry) {
            const busy = retrying.has(item.id);
            const btn = actionButton(busy ? 'Retrying…' : 'Retry', 'retry', 'btn-ghost', () => retryJob(item));
            btn.disabled = busy;
            out.push(btn);
        }
        out.push(el('button', { type: 'button', class: 'icon-btn', title: 'Remove from list', 'aria-label': 'Remove from list', onclick: () => dismissJob(item) },
            icon('x')));
    } else if (item.active || item.raw.status === 'paused') {
        if (item.id.startsWith('j:') && item.kind === 'torrent' && !item.raw.seeding) {
            if (item.raw.status !== 'paused') out.push(actionButton('Pause', 'pause', 'btn-ghost', () => jobTorrentAction(item, 'pause')));
            else if (item.raw.user_paused) out.push(actionButton('Resume', 'play', 'btn-ghost', () => jobTorrentAction(item, 'resume')));
        }
        out.push(actionButton('Stop', 'stop', 'btn-ghost', () => { confirming.set(item.id, 'stop'); renderActivity(); }));
    } else if (item.raw.status === 'done' && item.raw.download_url) {
        if (FLAGS.library && item.raw.saved_to !== 'library') {
            out.push(actionButton('Keep', 'server', 'btn-ghost', () => jobFileAction(item, 'keep'), 'Keep in the server library'));
        }
        out.push(el('a', { class: 'btn btn-sm btn-primary', href: item.raw.download_url, title: 'Download to this device' }, icon('download'), el('span', { text: 'Save' })));
        out.push(el('button', { type: 'button', class: 'icon-btn', title: 'Delete file', 'aria-label': 'Delete file', onclick: () => { confirming.set(item.id, 'delete'); renderActivity(); } }, icon('trash')));
    }
    return out;
}

function renderItem(item) {
    const li = el('li', { class: 'item', dataset: { id: item.id, kind: item.kind, state: item.state } },
        el('span', { class: 'item-icon', dataset: { kind: item.kind } }, icon(item.kind === 'torrent' ? 'torrent' : item.kind === 'direct' ? 'file' : 'video')),
        el('div', { class: 'item-main' },
            el('div', { class: 'item-title', text: item.title, title: item.tooltip || item.title }),
            el('div', { class: 'item-meta' }, item.meta)),
        el('div', { class: 'item-actions' }, itemActions(item)));
    if (item.progress !== null) {
        const bar = el('span');
        bar.style.width = Math.min(100, Math.max(item.state === 'downloading' || item.state === 'active' ? 2 : 0, item.progress)).toFixed(1) + '%';
        li.append(el('div', { class: 'item-progress', role: 'progressbar', 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(item.progress)), 'aria-label': `${item.title} progress` }, bar));
    }
    if (item.error) li.append(el('div', { class: 'item-error', text: item.error }));
    if (item.logs && item.logs.length) {
        const d = el('details', { class: 'item-log' }, el('summary', { text: 'Log' }), el('pre', { text: item.logs.join('\n') }));
        d.open = openLogs.has(item.id);
        d.addEventListener('toggle', () => { d.open ? openLogs.add(item.id) : openLogs.delete(item.id); });
        li.append(d);
    }
    if (highlight.has(item.id)) {
        li.classList.add('is-new');
        highlight.delete(item.id);
    }
    return li;
}

function renderActivity() {
    const items = [...activity.jobs.map(jobToItem), ...activity.torrents.map(torrentToItem)]
        .sort((a, b) => (b.active - a.active) || (b.time - a.time));
    const counts = { all: items.length, active: items.filter(i => i.active).length, torrent: items.filter(i => i.kind === 'torrent').length };
    for (const k of Object.keys(counts)) $('count-' + k).textContent = counts[k] || '';
    const dead = items.filter(i => i.dead).length;
    $('clear-dead-btn').hidden = dead === 0;
    $('dead-count').textContent = dead ? `(${dead})` : '';
    const shown = items.filter(i => filter === 'all' || (filter === 'active' ? i.active : i.kind === 'torrent'));
    // Keep focus on an action button across re-renders.
    const focusedId = document.activeElement && document.activeElement.closest('.item') ? document.activeElement.closest('.item').dataset.id : null;
    list.replaceChildren(...shown.map(renderItem));
    if (focusedId) {
        const b = list.querySelector(`.item[data-id="${CSS.escape(focusedId)}"] .item-actions button, .item[data-id="${CSS.escape(focusedId)}"] .item-actions a`);
        if (b) b.focus({ preventScroll: true });
    }
    $('activity-empty').hidden = shown.length > 0;
    const owner = activity.allJobs ? 'All users' : '';
    $('activity-title').textContent = owner ? 'Activity · all users' : 'Activity';
    return counts.active > 0;
}

function setFilter(f) {
    filter = f;
    document.querySelectorAll('#tabs [role="tab"]').forEach(t => t.setAttribute('aria-selected', String(t.dataset.filter === f)));
    renderActivity();
}
document.querySelectorAll('#tabs [role="tab"]').forEach(t => t.addEventListener('click', () => setFilter(t.dataset.filter)));

// Jobs and torrents refresh independently: a slow or unreachable
// Transmission must never freeze the download list.
function fetchJSON(url, ms) {
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), ms);
    return apiFetch(url, { signal: ctrl.signal }).then(r => r.json()).finally(() => clearTimeout(t));
}

let torrentsInFlight = false;

function refreshActivity(soon) {
    clearTimeout(pollTimer);
    if (FLAGS.transmission && !torrentsInFlight) {
        torrentsInFlight = true;
        fetchJSON('/api/torrents', 10000)
            .then(t => { activity.torrents = t.torrents || []; renderActivity(); })
            .catch(() => {})
            .finally(() => { torrentsInFlight = false; });
    }
    return fetchJSON('/api/jobs', 10000).then(j => {
        activity.jobs = j.jobs || [];
        activity.allJobs = !!j.all;
        const busy = renderActivity();
        schedule(busy || soon ? 1500 : 8000);
    }).catch(err => { if (err.message !== 'unauthorized') schedule(5000); });
}

// Background tabs keep polling, just slowly; some embedded browsers report
// every tab as hidden.
function schedule(ms) {
    clearTimeout(pollTimer);
    pollTimer = setTimeout(refreshActivity, document.hidden ? Math.max(ms, 30000) : ms);
}
document.addEventListener('visibilitychange', () => { if (!document.hidden) refreshActivity(); });

function stopJob(item) {
    confirming.delete(item.id);
    apiFetch(`/api/cancel/${encodeURIComponent(item.raw.id)}`, { method: 'POST' })
        .then(() => { toast('Download stopped'); refreshActivity(); })
        .catch(() => {});
}

function jobTorrentAction(item, action) {
    apiFetch(`/api/jobs/${encodeURIComponent(item.raw.id)}/${action}`, { method: 'POST' })
        .then(r => r.json())
        .then(d => { if (d.error) toast(d.error, true); else toast(action === 'pause' ? 'Torrent paused' : 'Torrent resumed'); refreshActivity(true); })
        .catch(() => {});
}

function dismissJob(item) {
    activity.jobs = activity.jobs.filter(j => j.id !== item.raw.id);
    renderActivity();
    apiFetch(`/api/jobs/${encodeURIComponent(item.raw.id)}`, { method: 'DELETE' })
        .then(r => r.json()).then(d => { if (d.error) toast(d.error, true); refreshActivity(); })
        .catch(() => {});
}

function retryJob(item) {
    if (retrying.has(item.id)) return;
    retrying.add(item.id);
    renderActivity();
    apiFetch(`/api/jobs/${encodeURIComponent(item.raw.id)}/retry`, { method: 'POST' })
        .then(async r => {
            const d = await r.json();
            if (!r.ok || d.error) throw new Error(d.error || 'Could not retry the download.');
            activity.jobs = activity.jobs.filter(j => j.id !== item.raw.id);
            if (d.torrent) highlight.add('t:' + d.torrent.hash);
            else highlight.add('j:' + d.job_id);
            toast('Download retried — a new attempt has started');
            setFilter('all');
            refreshActivity(true);
        })
        .catch(err => {
            if (err.message !== 'unauthorized') toast(err.message || 'Could not retry the download.', true);
        })
        .finally(() => { retrying.delete(item.id); renderActivity(); });
}

$('clear-dead-btn').addEventListener('click', () => {
    apiFetch('/api/jobs/clear', { method: 'POST' })
        .then(r => r.json())
        .then(d => { toast(d.cleared ? `Cleared ${d.cleared} item${d.cleared === 1 ? '' : 's'}` : 'Nothing to clear'); refreshActivity(); })
        .catch(() => {});
});

function jobFileAction(item, action) {
    confirming.delete(item.id);
    const id = encodeURIComponent(item.raw.id);
    const req = action === 'keep'
        ? apiFetch(`/api/jobs/${id}/keep`, { method: 'POST' })
        : apiFetch(`/api/jobs/${id}/file`, { method: 'DELETE' });
    req.then(r => r.json())
        .then(d => {
            if (d.error) toast(d.error, true);
            else toast(action === 'keep' ? 'Kept in the library' : 'File deleted');
            refreshActivity();
        })
        .catch(() => {});
}

function torrentAction(item, action) {
    confirming.delete(item.id);
    apiFetch(`/api/torrents/${encodeURIComponent(item.raw.hash)}/${action}`, { method: 'POST' })
        .then(r => r.json())
        .then(d => {
            if (d.error) toast(d.error, true);
            else if (action.startsWith('remove')) toast(action === 'remove-data' ? 'Torrent and files deleted' : 'Torrent removed, files kept');
            refreshActivity(true);
        })
        .catch(() => {});
}

// ---------------------------------------------------------------- torrent upload

if (FLAGS.torrents) {
    $('torrent-upload-btn').hidden = false;
    $('tab-torrents').hidden = false;
    $('torrent-upload-btn').addEventListener('click', () => $('torrent-file').click());
    $('torrent-file').addEventListener('change', () => {
        const f = $('torrent-file').files[0];
        $('torrent-file').value = '';
        if (!f) return;
        const fd = new FormData();
        fd.append('file', f);
        apiFetch('/api/torrents/upload', { method: 'POST', body: fd })
            .then(r => r.json())
            .then(d => {
                if (d.error) { showAlert(d.error); return; }
                if (d.job_id) {
                    highlight.add('j:' + d.job_id);
                    toast('Torrent started — you can close this page');
                } else {
                    highlight.add('t:' + d.torrent.hash);
                    toast(d.torrent.duplicate ? 'Already in Transmission' : `Added: ${d.torrent.name}`);
                }
                refreshActivity(true);
            })
            .catch(() => {});
    });
}

// ---------------------------------------------------------------- status
// One pill in the top bar sums up where downloads leave from and how much disk
// is left; clicking it opens a panel with the details.

const statusWrap = $('status');
const statusPill = $('status-pill');
const statusPanel = $('status-panel');
const status = { egress: null, disks: null, net: null };

// The live speed chip appears above LIVE_SHOW and only goes away after the
// rate has stayed under LIVE_HIDE for LIVE_HOLD_MS, so it does not flicker
// between polls or during short stalls.
const LIVE_SHOW = 512 * 1024;
const LIVE_HIDE = 128 * 1024;
const LIVE_HOLD_MS = 12000;
const live = { shown: false, lastActive: 0 };

function flagEmoji(cc) {
    return /^[A-Z]{2}$/.test(cc || '') ? String.fromCodePoint(...[...cc].map(c => 0x1F1A5 + c.charCodeAt(0))) : '';
}
function countryName(cc) {
    try { return new Intl.DisplayNames(['en'], { type: 'region' }).of(cc) || cc; } catch (_) { return cc; }
}
function fmtRate(bps) {
    return bps >= 1024 ? fmtBytes(bps) + '/s' : '0 B/s';
}
// Short sizes for the pill: "170 GB", "67.1 GB", "1.2 TB".
function fmtCompact(n) {
    if (!(n > 0)) return '0 B';
    const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
    const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
    const v = n / 1024 ** i;
    return (v >= 100 ? v.toFixed(0) : v.toFixed(1)) + ' ' + u[i];
}
function ispName(org) {
    return (org || '').replace(/^AS\d+\s+/, '');
}
function usedPct(d) {
    return Math.min(100, d.used / d.total * 100);
}
function meter(pct, cls) {
    const fill = el('span');
    fill.style.width = pct.toFixed(1) + '%';
    return el('span', { class: cls, 'aria-hidden': 'true' }, fill);
}
// The fullest disk is the one that will stop downloads first.
function worstDisk() {
    const ds = (status.disks || []).filter(x => x.total > 0);
    return ds.length ? ds.reduce((a, b) => (b.free < a.free ? b : a)) : null;
}
// Download speed: leecher's network receive rate, or disk writes where the
// server cannot read network counters.
function liveRate() {
    if (status.net) return status.net.rx_bps;
    return (status.disks || []).reduce((sum, d) => sum + (d.io ? d.io.write_bps : 0), 0);
}
function updateLive() {
    const r = liveRate(), now = Date.now();
    if (r >= LIVE_SHOW || (live.shown && r >= LIVE_HIDE)) { live.shown = true; live.lastActive = now; }
    else if (live.shown && now - live.lastActive > LIVE_HOLD_MS) live.shown = false;
    return r;
}

function renderPill() {
    const e = status.egress, ks = e && e.kill_switch, disk = worstDisk();
    const tripped = !!(ks && ks.tripped);
    const netBad = !!e && (!e.ip || !!e.error || tripped);
    const low = (status.disks || []).some(x => x.low);
    const parts = [];
    const aria = [];

    if (e) {
        parts.push(el('span', { class: 'dot', 'aria-hidden': 'true' }));
        if (tripped) {
            parts.push(el('span', { class: 'pill-alert', text: 'Kill switch tripped' }));
            aria.push('Kill switch tripped, downloads stopped');
        } else if (!e.ip) {
            parts.push(el('span', { class: 'pill-muted', text: 'IP unknown' }));
            aria.push('Public IP unknown');
        } else {
            if (ks) parts.push(el('span', { class: 'pill-ks' }, icon('shield')));
            const flag = flagEmoji(e.country);
            if (flag) parts.push(el('span', { class: 'pill-flag', text: flag }));
            parts.push(el('span', { class: 'pill-ip', text: e.ip }));
            aria.push(`Downloading via ${e.ip}${e.city ? ' in ' + e.city : ''}${ks ? ', kill switch armed' : ''}`);
        }
    }
    if (disk) {
        if (parts.length) parts.push(el('span', { class: 'pill-sep', 'aria-hidden': 'true' }));
        parts.push(icon('disk', 'pill-disk-icon'), el('span', { class: 'pill-free', text: fmtCompact(disk.free) }), meter(usedPct(disk), 'pill-meter'));
        aria.push(`${fmtBytes(disk.free)} free${low ? ', running low' : ''}`);
        const w = liveRate();
        if (live.shown) {
            parts.push(el('span', { class: 'pill-live', title: status.net ? 'Download speed' : 'Writing to disk' }, el('span', { class: 'pill-arrow', 'aria-hidden': 'true', text: '↓' }), fmtCompact(w) + '/s'));
            aria.push(`downloading at ${fmtRate(w)}`);
        }
    }
    if (!parts.length) return;
    statusPill.replaceChildren(...parts);
    statusPill.classList.toggle('is-bad', netBad);
    statusPill.classList.toggle('is-low', low);
    statusPill.setAttribute('aria-label', aria.join(' · ') + '. Show details.');
    statusWrap.hidden = false;
}

function infoRow(label, value, cls) {
    return value ? el('div', { class: 'info-row' + (cls ? ' ' + cls : '') }, el('span', { class: 'info-label', text: label }), el('span', { class: 'info-value', text: value })) : null;
}

function renderNetCard() {
    const e = status.egress;
    const body = $('net-body');
    if (!e) { body.replaceChildren(el('p', { class: 'status-empty', text: 'Checking…' })); return; }
    const ks = e.kill_switch;
    const nodes = [];
    if (ks && ks.tripped) {
        nodes.push(el('div', { class: 'status-alert' }, icon('shield'), el('div', {},
            el('strong', { text: 'Kill switch tripped — downloads stopped' }),
            ks.reason ? el('p', { text: ks.reason }) : null)));
    }
    const flag = flagEmoji(e.country);
    nodes.push(el('div', { class: 'net-hero' },
        flag ? el('span', { class: 'net-flag', 'aria-hidden': 'true', text: flag }) : null,
        el('div', {},
            el('div', { class: 'net-ip', text: e.ip || 'Unknown IP' }),
            el('div', { class: 'net-sub', text: [e.city, e.country ? countryName(e.country) : ''].filter(Boolean).join(', ') || 'Location unknown' }))));
    const [ksText, ksCls] = !ks ? ['Off', 'is-off'] : ks.tripped ? ['Tripped', 'is-bad'] : ['Armed', 'is-ok'];
    nodes.push(el('div', { class: 'info-grid' },
        infoRow('Provider', ispName(e.org)),
        infoRow('Network', (e.org || '').match(/^AS\d+/)?.[0] || ''),
        infoRow('Kill switch', ksText, ksCls),
        status.net ? infoRow('Traffic', `↓ ${fmtRate(status.net.rx_bps)}  ↑ ${fmtRate(status.net.tx_bps)}`) : null,
        infoRow('Checked', e.checked_at ? new Date(e.checked_at).toLocaleTimeString() : '')));
    if (e.error) nodes.push(el('p', { class: 'status-error', text: e.error }));
    body.replaceChildren(...nodes);
}

function renderDiskCard() {
    const ds = (status.disks || []).filter(x => x.total > 0);
    $('disk-section').hidden = !ds.length;
    $('disk-admin').hidden = !FLAGS.admin;
    $('disk-body').replaceChildren(...ds.map(d => {
        const pct = usedPct(d);
        return el('div', { class: 'disk-item' + (d.low ? ' is-low' : '') },
            el('div', { class: 'disk-item-head' },
                el('span', { class: 'disk-item-name', text: d.label }),
                el('span', { class: 'disk-item-free' }, el('strong', { text: fmtBytes(d.free) }), ` free of ${fmtBytes(d.total)}`)),
            meter(pct, 'disk-bar'),
            el('div', { class: 'disk-item-meta' },
                el('span', { text: `${Math.round(pct)}% used` }),
                d.io ? el('span', { class: 'disk-io' },
                    el('span', { class: 'io-w' + (d.io.write_bps < 1024 ? ' is-idle' : ''), title: 'Write' }, '↓ ', fmtRate(d.io.write_bps)),
                    el('span', { class: 'io-r' + (d.io.read_bps < 1024 ? ' is-idle' : ''), title: 'Read' }, '↑ ', fmtRate(d.io.read_bps))) : null,
                d.low ? el('span', { class: 'disk-low', text: 'Running low' }) : null));
    }));
}

function renderStatus() {
    renderPill();
    if (!statusPanel.hidden) { renderNetCard(); renderDiskCard(); }
}

let egressTimer = null;
function loadEgress(force) {
    statusPill.classList.add('is-loading');
    return apiFetch('/api/egress' + (force ? '?refresh=1' : '')).then(r => r.json()).then(d => {
        status.egress = d;
        renderStatus();
        // Follow the kill switch closely while it is armed.
        clearTimeout(egressTimer);
        if (d.kill_switch) egressTimer = setTimeout(() => loadEgress(false), 5000);
    }).catch(() => {}).finally(() => statusPill.classList.remove('is-loading'));
}

let diskTimer = null;
function loadDisk() {
    clearTimeout(diskTimer);
    return apiFetch('/api/disk').then(r => r.ok ? r.json() : Promise.reject()).then(d => {
        status.disks = d.disks || [];
        status.net = d.net || null;
        updateLive();
        renderStatus();
    }).catch(() => {}).finally(() => {
        // Faster while the panel is open so the speeds feel live.
        if (!document.hidden) diskTimer = setTimeout(loadDisk, statusPanel.hidden ? 5000 : 2000);
    });
}

function setStatusOpen(open) {
    statusPanel.hidden = !open;
    statusPill.setAttribute('aria-expanded', String(open));
    statusWrap.classList.toggle('is-open', open);
    if (open) { renderNetCard(); renderDiskCard(); loadDisk(); }
}
statusPill.addEventListener('click', () => setStatusOpen(statusPanel.hidden));
$('net-recheck').addEventListener('click', () => loadEgress(true));
document.addEventListener('click', e => { if (!statusPanel.hidden && !statusWrap.contains(e.target)) setStatusOpen(false); });
document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && !statusPanel.hidden) { setStatusOpen(false); statusPill.focus(); e.stopImmediatePropagation(); }
});
document.addEventListener('visibilitychange', () => { if (!document.hidden) loadDisk(); });

// ---------------------------------------------------------------- shell

(function initShell() {
    const name = document.querySelector('.menu-head strong').textContent.trim();
    $('avatar-initial').textContent = name ? name[0] : '?';
    if (FLAGS.admin) $('menu-admin').hidden = false;

    const menu = document.querySelector('.menu');
    document.addEventListener('click', e => { if (menu.open && !menu.contains(e.target)) menu.open = false; });
    document.addEventListener('keydown', e => {
        if (e.key !== 'Escape') return;
        if (menu.open) { menu.open = false; menu.querySelector('summary').focus(); }
        else if (!$('preview').hidden) { closePreview(); input.focus(); }
    });

    const extras = [];
    if (FLAGS.direct) extras.push('direct file links');
    if (FLAGS.torrents) extras.push('torrents');
    if (extras.length) $('sources').append(` · plus ${extras.join(' and ')}`);

})();

updateComposer();
refreshActivity();
loadEgress(false);
setInterval(() => { if (!document.hidden) loadEgress(false); }, 10 * 60 * 1000);
loadDisk();

// Bookmarklets and share links open /?url=…
{
    const shared = new URLSearchParams(location.search).get('url');
    if (shared) {
        input.value = shared;
        history.replaceState(null, '', location.pathname);
        updateComposer();
        fetchFormats();
    } else {
        input.focus();
    }
}
