// Account page: password change, API tokens and the bookmarklet.
const CSRF_TOKEN = document.body.dataset.csrf || '';

function api(url, opts = {}) {
    const headers = Object.assign({ 'X-CSRF-Token': CSRF_TOKEN }, opts.headers || {});
    return fetch(url, Object.assign({}, opts, { headers })).then(async r => {
        if (r.status === 401) { location.href = '/login?next=%2Faccount'; throw new Error('unauthorized'); }
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || `HTTP ${r.status}`);
        return data;
    });
}

function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
}

function fmtDate(iso) {
    if (!iso || iso.startsWith('0001')) return 'never';
    return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

function setStatus(node, msg, isError) {
    node.textContent = msg;
    node.classList.toggle('err', !!isError);
}

function renderTokens(tokens) {
    const list = document.getElementById('tok-list');
    list.replaceChildren();
    if (!tokens.length) {
        list.appendChild(el('p', 'acct-text', 'No tokens yet.'));
        return;
    }
    tokens.forEach(t => {
        const row = el('div', 'acct-item');
        const info = el('div', 'grow');
        info.appendChild(el('div', '', t.name));
        info.appendChild(el('div', 'meta', `…${t.hint}`));
        info.appendChild(el('div', 'meta', `Created ${fmtDate(t.created_at)} · last used ${fmtDate(t.last_used)}`));
        const revoke = el('button', 'btn btn-danger btn-sm', 'Revoke');
        revoke.type = 'button';
        revoke.addEventListener('click', () => {
            if (!confirm(`Revoke token "${t.name}"? Anything using it stops working.`)) return;
            api(`/api/account/tokens/${encodeURIComponent(t.id)}`, { method: 'DELETE' }).then(load).catch(e => alert(e.message));
        });
        row.append(info, revoke);
        list.appendChild(row);
    });
}

function load() {
    return api('/api/account', { method: 'GET' }).then(d => {
        document.getElementById('acct-who').textContent = `Signed in as ${d.username} (${d.role})`;
        renderTokens(d.tokens || []);
    });
}

document.getElementById('pw-form').addEventListener('submit', e => {
    e.preventDefault();
    const status = document.getElementById('pw-status');
    const cur = document.getElementById('pw-current').value;
    const nw = document.getElementById('pw-new').value;
    if (nw !== document.getElementById('pw-confirm').value) { setStatus(status, 'Passwords do not match', true); return; }
    api('/api/account/password', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ current_password: cur, new_password: nw }),
    }).then(() => {
        e.target.reset();
        setStatus(status, 'Password updated. Other sessions were signed out.');
    }).catch(err => setStatus(status, err.message, true));
});

document.getElementById('tok-form').addEventListener('submit', e => {
    e.preventDefault();
    const name = document.getElementById('tok-name').value.trim();
    api('/api/account/tokens', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name }),
    }).then(d => {
        document.getElementById('tok-value').textContent = d.token;
        document.getElementById('tok-new').hidden = false;
        document.getElementById('tok-name').value = '';
        load();
    }).catch(err => alert(err.message));
});

document.getElementById('tok-copy').addEventListener('click', () => {
    const v = document.getElementById('tok-value').textContent;
    navigator.clipboard.writeText(v).then(() => { document.getElementById('tok-copy').textContent = 'Copied'; });
});

document.getElementById('tok-example').textContent =
`curl -X POST ${location.origin}/api/download \\
  -H "Authorization: Bearer lch_…" \\
  -H "Content-Type: application/json" \\
  -d '{"url": "https://example.com/file.zip", "destination": "library"}'

# then follow it:
curl -H "Authorization: Bearer lch_…" ${location.origin}/api/status/<job_id>`;

document.getElementById('bookmarklet').href =
    `javascript:void(window.open('${location.origin}/?url='+encodeURIComponent(location.href)))`;
document.getElementById('bookmarklet').addEventListener('click', e => e.preventDefault());

// Register Leecher as the browser's handler for magnet: links. The browser
// substitutes the full magnet URI for %s; the main page picks up ?url=.
(function magnetHandler() {
    const btn = document.getElementById('magnet-btn');
    const status = document.getElementById('magnet-status');
    if (!('registerProtocolHandler' in navigator)) {
        btn.disabled = true;
        setStatus(status, 'Not supported in this browser.', true);
        return;
    }
    btn.addEventListener('click', () => {
        try {
            navigator.registerProtocolHandler('magnet', `${location.origin}/?url=%s`);
            setStatus(status, 'Confirm in your browser’s prompt (usually in the address bar).');
        } catch (e) {
            setStatus(status, location.protocol === 'https:' ? 'Your browser refused the request.' : 'This only works over HTTPS.', true);
        }
    });
})();

load().catch(() => {});
