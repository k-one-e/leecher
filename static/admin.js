// Admin panel behaviour. External so the CSP can forbid inline scripts.
const CSRF_TOKEN = document.body.dataset.csrf || '';
const DEFAULTS = JSON.parse(document.body.dataset.defaults || '{}');
let pendingDelete = '';
let lastFocusedBeforeConfirm = null;

// Set placeholders from embedded defaults (single source of truth)
Object.keys(DEFAULTS).forEach(k => {
  const el = document.getElementById('s_' + k);
  if (el) el.placeholder = String(DEFAULTS[k]);
});

async function loadFiles() {
  try {
    const r = await fetch('/api/admin/files');
    if (r.status === 401) { window.location.href = '/admin/login'; return; }
    const data = await r.json();
    const files = data.files || [];

    document.getElementById('fileBadge').textContent = files.length;
    document.getElementById('totalFiles').textContent = files.length;

    let totalBytes = 0;
    files.forEach(f => totalBytes += f.size);
    document.getElementById('totalSize').textContent = formatBytes(totalBytes);

    const tbody = document.getElementById('fileBody');
    if (files.length === 0) {
      tbody.innerHTML = `<tr><td colspan="4" class="empty">
        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" width="48" height="48"><path d="M22 19a2 2 0 01-2 2H4a2 2 0 01-2-2V5a2 2 0 012-2h5l2 3h9a2 2 0 012 2z"/></svg>
        <p>No downloaded files</p>
      </td></tr>`;
      return;
    }

    tbody.innerHTML = files.map(f => {
      const ext = f.name.split('.').pop().toUpperCase();
      // Strip the job ID prefix (hex_) for display
      const displayName = f.name.replace(/^[a-f0-9]+_/, '');
      return `<tr>
        <td>
          <span class="file-name" title="${esc(f.name)}">${esc(displayName)}</span>
          <span class="file-ext">${esc(ext)}</span>
        </td>
        <td>${esc(f.size_mb)}</td>
        <td class="text-muted">${esc(f.modified)}</td>
        <td style="text-align:right;white-space:nowrap">
          <div style="display:inline-flex;gap:.4rem;align-items:center">
            <a href="${esc(f.path)}" class="btn btn-ghost btn-sm" title="Download">
              <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>
            </a>
            <button class="btn btn-danger btn-sm" data-delete="${esc(f.name)}" title="Delete">
              <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 01-2 2H7a2 2 0 01-2-2V6m3 0V4a2 2 0 012-2h4a2 2 0 012 2v2"/></svg>
            </button>
          </div>
        </td>
      </tr>`;
    }).join('');
  } catch(e) {
    console.error('Load files error:', e);
  }
}

function askDelete(name) {
  pendingDelete = name;
  const display = name.replace(/^[a-f0-9]+_/, '');
  document.getElementById('confirmName').textContent = display;
  lastFocusedBeforeConfirm = document.activeElement;
  document.getElementById('confirmOverlay').classList.add('active');
  document.getElementById('confirmDeleteBtn').focus();
}

function closeConfirm() {
  document.getElementById('confirmOverlay').classList.remove('active');
  pendingDelete = '';
  if (lastFocusedBeforeConfirm && lastFocusedBeforeConfirm.focus) lastFocusedBeforeConfirm.focus();
  lastFocusedBeforeConfirm = null;
}

async function confirmDelete() {
  if (!pendingDelete) return;
  const btn = document.getElementById('confirmDeleteBtn');
  btn.textContent = 'Deleting...';
  btn.disabled = true;
  try {
    const r = await fetch('/api/admin/delete/' + encodeURIComponent(pendingDelete), {
      method: 'POST',
      headers: {'X-CSRF-Token': CSRF_TOKEN}
    });
    if (r.status === 401) { window.location.href = '/admin/login'; return; }
    closeConfirm();
    loadFiles();
  } catch(e) {
    console.error('Delete error:', e);
  } finally {
    btn.textContent = 'Delete';
    btn.disabled = false;
  }
}

function formatBytes(bytes) {
  if (bytes === 0) return '0 B';
  const units = ['B','KB','MB','GB','TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(1024));
  return (bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0) + ' ' + units[i];
}

function esc(s) {
  return String(s ?? '').replace(/[&<>"'`]/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;',
    '"': '&quot;', "'": '&#39;', '`': '&#96;'
  })[c]);
}

document.getElementById('confirmOverlay').addEventListener('click', function(e) {
  if (e.target === this) closeConfirm();
});

document.addEventListener('keydown', function(e) {
  const overlay = document.getElementById('confirmOverlay');
  if (!overlay.classList.contains('active')) return;
  if (e.key === 'Escape') { closeConfirm(); return; }
  if (e.key !== 'Tab') return;
  // Keep focus inside the dialog while it is open.
  const focusable = overlay.querySelectorAll('button');
  const first = focusable[0], last = focusable[focusable.length - 1];
  if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
  else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
});

loadFiles();
loadSettings();

const settingKeys = [
  'max_file_age_minutes',
  'download_timeout_minutes',
  'max_concurrent_downloads',
  'max_downloads_per_ip',
  'max_file_size_mb',
  'max_storage_mb',
  'min_disk_free_mb',
  'session_hours',
  'rate_limit_formats',
  'rate_limit_downloads',
  'time_zone',
  'probe_concurrency',
  'probe_queue_max',
  'probe_timeout_secs',
  'download_queue_max',
  'direct_timeout_minutes',
  'library_dir',
  'transmission_url',
  'transmission_download_dir',
  'torrent_engine',
  'seed_ratio',
  'seed_minutes',
  'max_active_torrents',
  'kill_switch_interval_secs'
];
const stringSettingKeys = ['time_zone', 'library_dir', 'transmission_url', 'transmission_download_dir', 'torrent_engine'];
const floatSettingKeys = ['seed_ratio'];

function updateDebugLabel() {
  const cb = document.getElementById('s_debug_mode');
  const lbl = document.getElementById('debug_mode_label');
  if (cb && lbl) {
    lbl.textContent = cb.checked ? 'On' : 'Off';
    lbl.style.color = cb.checked ? '#00ff87' : '#555';
  }
}
document.getElementById('s_debug_mode').addEventListener('change', updateDebugLabel);

function updateDirectLabel() {
  const cb = document.getElementById('s_direct_downloads');
  const lbl = document.getElementById('direct_downloads_label');
  if (cb && lbl) {
    lbl.textContent = cb.checked ? 'On' : 'Off';
    lbl.style.color = cb.checked ? '#00ff87' : '#555';
  }
}
document.getElementById('s_direct_downloads').addEventListener('change', updateDirectLabel);

function updateKillSwitchLabel() {
  const cb = document.getElementById('s_kill_switch');
  const lbl = document.getElementById('kill_switch_label');
  if (cb && lbl) {
    lbl.textContent = cb.checked ? 'On' : 'Off';
    lbl.style.color = cb.checked ? '#00ff87' : '#555';
  }
}
document.getElementById('s_kill_switch').addEventListener('change', updateKillSwitchLabel);

function killSwitchIPs() {
  return document.getElementById('s_kill_switch_ips').value.split(/[\s,]+/).map(s => s.trim()).filter(Boolean);
}

async function detectKillSwitchIP() {
  const btn = document.getElementById('ksDetectBtn');
  const status = document.getElementById('ksDetectStatus');
  btn.disabled = true;
  status.className = 'settings-status';
  status.textContent = 'Checking…';
  try {
    const r = await fetch('/api/admin/killswitch?detect=1');
    const data = await r.json();
    if (!r.ok || data.error) throw new Error(data.error || 'lookup failed');
    const list = killSwitchIPs();
    if (list.includes(data.ip)) {
      status.textContent = `${data.ip} is already allowed`;
    } else {
      document.getElementById('s_kill_switch_ips').value = [...list, data.ip].join('\n');
      markSettingsDirty();
      status.textContent = `Added ${data.ip} — Save to apply`;
    }
    status.className = 'settings-status ok';
  } catch (e) {
    status.textContent = e.message;
    status.className = 'settings-status err';
  } finally {
    btn.disabled = false;
  }
}

async function loadKillSwitchStatus() {
  const state = document.getElementById('ksState');
  const live = document.getElementById('ksLive');
  try {
    const r = await fetch('/api/admin/killswitch');
    if (!r.ok) return;
    const d = await r.json();
    live.className = 'setting-desc ks-live';
    if (!d.enabled) {
      state.textContent = 'off';
      live.textContent = '';
      return;
    }
    const when = d.checked_at && !d.checked_at.startsWith('0001') ? new Date(d.checked_at).toLocaleTimeString() : '';
    if (d.tripped) {
      state.textContent = 'tripped';
      live.classList.add('tripped');
      const since = d.tripped_at ? ` since ${new Date(d.tripped_at).toLocaleTimeString()}` : '';
      live.textContent = `Tripped${since}: ${d.reason}. Downloads are stopped.${when ? ` Last check ${when}.` : ''}`;
    } else if (d.verified) {
      state.textContent = 'armed';
      live.classList.add('ok');
      live.textContent = `Armed — outbound IP ${d.ip} is allowed.${when ? ` Last check ${when}.` : ''}`;
    } else {
      state.textContent = 'checking';
      live.textContent = 'Waiting for the first check…';
    }
  } catch (_) { /* transient; the next poll retries */ }
}
loadKillSwitchStatus();
setInterval(loadKillSwitchStatus, 3000);

// Settings only apply on Save, so an edited-but-unsaved panel has to announce
// itself — otherwise flipping the Debug toggle looks like it took effect.
let settingsDirty = false;

function markSettingsDirty() {
  if (settingsDirty) return;
  settingsDirty = true;
  document.getElementById('saveSettingsBtn')?.classList.add('needs-save');
  document.getElementById('saveBar')?.classList.add('visible');
}

function clearSettingsDirty() {
  settingsDirty = false;
  document.getElementById('saveSettingsBtn')?.classList.remove('needs-save');
  document.getElementById('saveBar')?.classList.remove('visible');
}

async function loadSettings() {
  try {
    const r = await fetch('/api/admin/settings');
    if (r.status === 401) return;
    const data = await r.json();
    settingKeys.forEach(k => {
      const el = document.getElementById('s_' + k);
      if (el && data[k] !== undefined) el.value = data[k];
    });
    updateCookiesStatus(data.cookies_file);
    const dbg = document.getElementById('s_debug_mode');
    if (dbg) { dbg.checked = !!data.debug_mode; updateDebugLabel(); }
    const direct = document.getElementById('s_direct_downloads');
    if (direct) { direct.checked = !!data.direct_downloads; updateDirectLabel(); }
    const ks = document.getElementById('s_kill_switch');
    if (ks) { ks.checked = !!data.kill_switch; updateKillSwitchLabel(); }
    const ksIPs = document.getElementById('s_kill_switch_ips');
    if (ksIPs) ksIPs.value = (data.kill_switch_ips || []).join('\n');
  } catch(e) {
    console.error('Load settings error:', e);
  }
}

async function saveSettings() {
  const btn = document.getElementById('saveSettingsBtn');
  const status = document.getElementById('settingsStatus');
  btn.disabled = true;
  status.textContent = '';
  status.className = 'settings-status';

  const payload = {};
  settingKeys.forEach(k => {
    const el = document.getElementById('s_' + k);
    if (el) {
      if (stringSettingKeys.includes(k)) {
        payload[k] = el.value || '';
      } else if (floatSettingKeys.includes(k)) {
        payload[k] = parseFloat(el.value) || 0;
      } else {
        payload[k] = parseInt(el.value, 10) || 0;
      }
    }
  });
  const dbg = document.getElementById('s_debug_mode');
  if (dbg) payload.debug_mode = dbg.checked;
  const direct = document.getElementById('s_direct_downloads');
  if (direct) payload.direct_downloads = direct.checked;
  const ks = document.getElementById('s_kill_switch');
  if (ks) payload.kill_switch = ks.checked;
  if (document.getElementById('s_kill_switch_ips')) payload.kill_switch_ips = killSwitchIPs();

  try {
    const r = await fetch('/api/admin/settings', {
      method: 'POST',
      headers: {'Content-Type': 'application/json', 'X-CSRF-Token': CSRF_TOKEN},
      body: JSON.stringify(payload)
    });
    if (r.status === 401) { window.location.href = '/admin/login'; return; }
    const data = await r.json();
    if (data.error) {
      status.textContent = data.error;
      status.className = 'settings-status err';
    } else {
      status.textContent = 'Saved';
      status.className = 'settings-status ok';
      clearSettingsDirty();
      setTimeout(() => { status.textContent = ''; }, 3000);
      loadSettings(); // show the server's normalised kill switch list
      setTimeout(loadKillSwitchStatus, 1500);
      // Refresh tables so timezone changes take effect
      loadFiles();
      loadHistory();
    }
  } catch(e) {
    status.textContent = 'Failed to save';
    status.className = 'settings-status err';
  } finally {
    btn.disabled = false;
  }
}

function updateCookiesStatus(path) {
  const statusEl = document.getElementById('cookiesStatus');
  const clearBtn = document.getElementById('clearCookiesBtn');
  const refreshBtn = document.getElementById('refreshCookiesBtn');
  const healthEl = document.getElementById('cookiesHealth');
  const warnEl = document.getElementById('cookiesAuthWarn');
  if (path && path.length > 0) {
    statusEl.textContent = 'active';
    statusEl.style.color = '#00ff87';
    clearBtn.style.display = '';
    refreshBtn.style.display = '';
    loadCookieStatus();
    // Re-poll hourly so the "N days left" chip stays current on long-lived tabs.
    if (!window.__cookieStatusTimer) {
      window.__cookieStatusTimer = setInterval(loadCookieStatus, 60 * 60 * 1000);
    }
  } else {
    statusEl.textContent = 'none';
    statusEl.style.color = '';
    clearBtn.style.display = 'none';
    refreshBtn.style.display = 'none';
    healthEl.style.display = 'none';
    healthEl.innerHTML = '';
    warnEl.style.display = 'none';
    warnEl.innerHTML = '';
  }
}

function formatRelativeTime(iso) {
  const then = new Date(iso).getTime();
  if (!then) return '';
  const diff = Math.max(0, Date.now() - then);
  const m = Math.floor(diff / 60000);
  if (m < 1) return 'just now';
  if (m < 60) return m + ' min ago';
  const h = Math.floor(m / 60);
  if (h < 24) return h + ' h ago';
  const d = Math.floor(h / 24);
  return d + ' d ago';
}

function escapeHTML(s) {
  return String(s || '').replace(/[&<>"']/g, c => ({
    '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
  })[c]);
}

async function loadCookieStatus() {
  const healthEl = document.getElementById('cookiesHealth');
  const warnEl = document.getElementById('cookiesAuthWarn');
  try {
    // Cache-bust defensively in case an upstream proxy ignores Cache-Control.
    const r = await fetch('/api/admin/cookies?_=' + Date.now(), { cache: 'no-store' });
    if (r.status === 401) { window.location.href = '/admin/login'; return; }
    const data = await r.json();
    // Chips per platform
    if (data.present && Array.isArray(data.platforms) && data.platforms.length > 0) {
      const chips = data.platforms.map(p => {
        let cls = 'ok', label = p.name;
        if (!p.present) {
          cls = 'err';
          label += ' · missing ' + (p.missing || []).join(', ');
        } else if (p.expired) {
          cls = 'err';
          label += ' · expired';
        } else if (p.expires_in_days > 0 && p.expires_in_days <= 7) {
          cls = 'warn';
          label += ' · expires in ' + p.expires_in_days + 'd';
        } else if (p.expires_in_days > 0) {
          label += ' · ' + p.expires_in_days + 'd left';
        }
        return '<span class="cookies-chip ' + cls + '"><span class="dot"></span>' + escapeHTML(label) + '</span>';
      }).join('');
      healthEl.innerHTML = chips;
      healthEl.style.display = '';
    } else {
      healthEl.innerHTML = '';
      healthEl.style.display = 'none';
    }
    // Last auth failure banner
    if (data.last_auth_failure) {
      const f = data.last_auth_failure;
      warnEl.innerHTML =
        '<strong>Recent auth failure</strong> &mdash; a download from <strong>' +
        escapeHTML(f.host || 'unknown host') + '</strong> failed ' +
        escapeHTML(formatRelativeTime(f.at)) +
        ' with an auth-related error. The cookies for this platform may be stale.';
      warnEl.style.display = '';
    } else {
      warnEl.innerHTML = '';
      warnEl.style.display = 'none';
    }
  } catch(e) {
    console.error('Load cookie status error:', e);
  }
}

async function uploadCookies() {
  const input = document.getElementById('cookiesFileInput');
  const status = document.getElementById('cookiesUploadStatus');
  if (!input.files || input.files.length === 0) return;
  const file = input.files[0];
  if (file.size > 1024 * 1024) {
    status.textContent = 'File too large (max 1MB)';
    status.className = 'settings-status err';
    input.value = '';
    return;
  }
  status.textContent = 'Uploading...';
  status.className = 'settings-status';
  try {
    const fd = new FormData();
    fd.append('file', file);
    const r = await fetch('/api/admin/cookies', {
      method: 'POST',
      headers: {'X-CSRF-Token': CSRF_TOKEN},
      body: fd
    });
    if (r.status === 401) { window.location.href = '/admin/login'; return; }
    const data = await r.json();
    if (data.error) {
      status.textContent = data.error;
      status.className = 'settings-status err';
    } else {
      status.textContent = 'Uploaded';
      status.className = 'settings-status ok';
      updateCookiesStatus(data.path);
      setTimeout(() => { status.textContent = ''; }, 3000);
    }
  } catch(e) {
    status.textContent = 'Upload failed';
    status.className = 'settings-status err';
  } finally {
    input.value = '';
  }
}

async function clearCookies() {
  const status = document.getElementById('cookiesUploadStatus');
  try {
    const r = await fetch('/api/admin/cookies', {
      method: 'DELETE',
      headers: {'X-CSRF-Token': CSRF_TOKEN}
    });
    if (r.status === 401) { window.location.href = '/admin/login'; return; }
    const data = await r.json();
    if (data.error) {
      status.textContent = data.error;
      status.className = 'settings-status err';
    } else {
      status.textContent = 'Cleared';
      status.className = 'settings-status ok';
      updateCookiesStatus('');
      setTimeout(() => { status.textContent = ''; }, 3000);
    }
  } catch(e) {
    status.textContent = 'Failed';
    status.className = 'settings-status err';
  }
}

// ----- Users ---------------------------------------------------------------

async function userRequest(url, method, body) {
  const status = document.getElementById('userStatus');
  status.textContent = '';
  status.className = 'settings-status';
  const opts = {method, headers: {'X-CSRF-Token': CSRF_TOKEN}};
  if (body) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(url, opts);
  if (r.status === 401) { window.location.href = '/login?next=%2Fadmin'; return null; }
  const data = await r.json().catch(() => ({}));
  if (!r.ok || data.error) {
    status.textContent = data.error || ('HTTP ' + r.status);
    status.className = 'settings-status err';
    return null;
  }
  return data;
}

function flashUserStatus(msg) {
  const status = document.getElementById('userStatus');
  status.textContent = msg;
  status.className = 'settings-status ok';
  setTimeout(() => { status.textContent = ''; }, 3000);
}

async function loadUsers() {
  const r = await fetch('/api/admin/users');
  if (!r.ok) return;
  const data = await r.json();
  const users = data.users || [];
  document.getElementById('userBadge').textContent = users.length;
  const tbody = document.getElementById('userBody');
  tbody.innerHTML = users.map(u => {
    const me = u.username === data.me;
    const created = u.created_at ? new Date(u.created_at).toLocaleDateString('sv-SE') : '—';
    const flag = u.must_change_password ? ' <span class="text-muted" title="Must change password at next sign-in">(pending)</span>' : '';
    const actions = me ? '<span class="text-muted">you</span>' : `
      <button class="btn btn-ghost btn-sm" data-role="${esc(u.username)}" data-next="${u.role === 'admin' ? 'user' : 'admin'}">Make ${u.role === 'admin' ? 'user' : 'admin'}</button>
      <button class="btn btn-ghost btn-sm" data-reset="${esc(u.username)}">Reset password</button>
      <button class="btn btn-danger btn-sm" data-remove="${esc(u.username)}">Delete</button>`;
    return `<tr>
      <td><span class="file-name">${esc(u.username)}</span>${flag}</td>
      <td class="text-muted">${esc(u.role)}</td>
      <td class="text-muted">${u.tokens}</td>
      <td class="text-muted">${esc(created)}</td>
      <td style="text-align:right;white-space:nowrap">${actions}</td>
    </tr>`;
  }).join('');
}

async function addUser() {
  const username = document.getElementById('nu_username').value.trim();
  const password = document.getElementById('nu_password').value;
  const role = document.getElementById('nu_role').value;
  const data = await userRequest('/api/admin/users', 'POST', {username, password, role});
  if (!data) return;
  document.getElementById('nu_username').value = '';
  document.getElementById('nu_password').value = '';
  flashUserStatus('User created');
  loadUsers();
}

document.getElementById('userBody').addEventListener('click', async e => {
  const roleBtn = e.target.closest('[data-role]');
  const resetBtn = e.target.closest('[data-reset]');
  const removeBtn = e.target.closest('[data-remove]');
  if (roleBtn) {
    if (await userRequest('/api/admin/users/' + encodeURIComponent(roleBtn.dataset.role), 'POST', {role: roleBtn.dataset.next})) {
      flashUserStatus('Role updated'); loadUsers();
    }
  } else if (resetBtn) {
    const pw = prompt('New temporary password for ' + resetBtn.dataset.reset + ' (min. 8 characters). They must change it at next sign-in.');
    if (!pw) return;
    if (await userRequest('/api/admin/users/' + encodeURIComponent(resetBtn.dataset.reset), 'POST', {password: pw})) {
      flashUserStatus('Password reset'); loadUsers();
    }
  } else if (removeBtn) {
    if (!confirm('Delete user ' + removeBtn.dataset.remove + '? Their sessions and API tokens stop working.')) return;
    if (await userRequest('/api/admin/users/' + encodeURIComponent(removeBtn.dataset.remove), 'DELETE')) {
      flashUserStatus('User deleted'); loadUsers();
    }
  }
});

async function testTransmission() {
  const status = document.getElementById('transmissionStatus');
  status.textContent = 'Testing...';
  status.className = 'settings-status';
  try {
    const r = await fetch('/api/admin/transmission-test');
    const data = await r.json().catch(() => ({}));
    if (!r.ok || data.error) throw new Error(data.error || ('HTTP ' + r.status));
    status.textContent = 'Connected — Transmission ' + data.version + ' (default folder ' + data.download_dir + ')';
    status.className = 'settings-status ok';
  } catch (e) {
    status.textContent = e.message;
    status.className = 'settings-status err';
  }
}

async function updateYtdlp() {
  const btn = document.getElementById('updateYtdlpBtn');
  const status = document.getElementById('updateStatus');
  const logWrap = document.getElementById('ytdlpLogWrap');
  const logPre = document.getElementById('ytdlpLog');
  btn.disabled = true;
  status.textContent = 'Updating...';
  status.className = 'settings-status';
  logPre.textContent = '';
  logWrap.style.display = 'block';

  try {
    const resp = await fetch('/api/admin/update-ytdlp', {
      method: 'POST',
      headers: {'X-CSRF-Token': CSRF_TOKEN}
    });
    if (resp.status === 401) { window.location.href = '/admin/login'; return; }
    if (!resp.ok) {
      const d = await resp.json().catch(() => ({}));
      throw new Error(d.error || 'Update failed');
    }
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';
    let finalVersion = '';
    let currentEvent = 'message';
    let hadError = false;
    while (true) {
      const {done, value} = await reader.read();
      if (done) break;
      buf += decoder.decode(value, {stream: true});
      const lines = buf.split('\n');
      buf = lines.pop();
      for (const line of lines) {
        if (line.startsWith('event: ')) {
          currentEvent = line.slice(7).trim();
          continue;
        }
        if (line.startsWith('data: ')) {
          const msg = line.slice(6);
          if (currentEvent === 'done') {
            finalVersion = msg;
            logPre.textContent += '✓ Done: ' + msg + '\n';
          } else if (currentEvent === 'error') {
            hadError = true;
            logPre.textContent += '✗ Error: ' + msg + '\n';
          } else {
            logPre.textContent += msg + '\n';
          }
          logPre.scrollTop = logPre.scrollHeight;
          currentEvent = 'message';
        }
      }
    }
    if (hadError) {
      status.textContent = 'Update failed';
      status.className = 'settings-status err';
    } else {
      if (finalVersion) {
        document.getElementById('ytdlpVersion').textContent = finalVersion;
      }
      status.textContent = 'Updated!';
      status.className = 'settings-status ok';
      setTimeout(() => { status.textContent = ''; }, 4000);
    }
  } catch(e) {
    logPre.textContent += '\nError: ' + e.message + '\n';
    status.textContent = 'Update failed';
    status.className = 'settings-status err';
  } finally {
    btn.disabled = false;
  }
}

async function loadHistory() {
  try {
    const r = await fetch('/api/admin/history');
    if (r.status === 401) return;
    const data = await r.json();
    const history = data.history || [];
    document.getElementById('historyBadge').textContent = history.length;
    const tbody = document.getElementById('historyBody');
    if (history.length === 0) {
      tbody.innerHTML = '<tr><td colspan="5" class="empty"><p>No download history</p></td></tr>';
      return;
    }
    tbody.innerHTML = history.slice(0, 50).map(h => {
      const statusClass = (h.status === 'done' || h.status === 'added') ? 'color:#00ff87' : h.status === 'error' ? 'color:#ff4444' : '';
      const displayUrl = h.url ? (h.url.length > 60 ? h.url.substring(0, 60) + '...' : h.url) : '—';
      const displayName = h.display_name || '—';
      let date = '—';
      if (h.finished_at) {
        try {
          const tz = document.getElementById('s_time_zone') ? document.getElementById('s_time_zone').value : 'UTC';
          date = new Date(h.finished_at).toLocaleString('sv-SE', {timeZone: tz, hour12: false}).replace('T',' ');
        } catch(e) {
          date = new Date(h.finished_at).toLocaleString('sv-SE', {hour12: false}).replace('T',' ');
        }
      }
      return `<tr>
        <td><span class="file-name" title="${esc(h.url || '')}" style="max-width:300px">${esc(displayUrl)}</span></td>
        <td style="${statusClass};font-weight:600;font-size:.8rem;text-transform:uppercase">${esc(h.status)}</td>
        <td class="text-muted">${esc(displayName)}${h.destination === 'library' ? ' <span title="Saved to server">⇢ server</span>' : ''}${h.kind === 'torrent' ? ' <span title="Torrent">⇢ transmission</span>' : ''}</td>
        <td class="text-muted">${esc(h.owner || '—')}</td>
        <td class="text-muted">${esc(date)}</td>
      </tr>`;
    }).join('');
  } catch(e) {
    console.error('Load history error:', e);
  }
}

loadHistory();
loadUsers();

// SSE: auto-refresh file table when files change on server
(function connectSSE() {
  const es = new EventSource('/api/admin/events');
  es.addEventListener('files-changed', () => {
    loadFiles();
    loadHistory();
  });
  es.onerror = () => {
    es.close();
    setTimeout(connectSSE, 5000);
  };
})();

// Storage: free space, live throughput and an on-demand speed test.
const diskBenchState = {}; // disk id -> {running, error}

function formatRate(bps) {
  return bps >= 1024 ? formatBytes(bps) + '/s' : '0 B/s';
}

function timeAgo(iso) {
  const s = Math.max(0, Math.round((Date.now() - new Date(iso)) / 1000));
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s / 60) + ' min ago';
  if (s < 86400) return Math.floor(s / 3600) + ' h ago';
  return new Date(iso).toLocaleDateString();
}

function renderDisk(d, benchBusy) {
  if (d.error || !d.total) {
    return `<div class="disk-row">
      <div class="disk-head"><span class="disk-name">${esc(d.label)}</span></div>
      <div class="disk-paths">${esc((d.paths || []).join('  ·  '))}</div>
      <div class="disk-err">${esc(d.error || 'Size unavailable')}</div>
    </div>`;
  }
  const pct = Math.min(100, d.used / d.total * 100);
  const st = diskBenchState[d.id] || {};
  let bench = '';
  if (st.running) bench = '<span class="disk-bench-result">Testing… (up to a minute on slow disks)</span>';
  else if (st.error) bench = `<span class="disk-bench-result err">${esc(st.error)}</span>`;
  else if (d.bench) {
    bench = `<span class="disk-bench-result" title="${esc(formatBytes(d.bench.size))} sequential, ${esc(new Date(d.bench.at).toLocaleString())}">`
      + `Write <b>${esc(formatRate(d.bench.write_bps))}</b> · Read <b>${esc(formatRate(d.bench.read_bps))}</b>`
      + `${d.bench.read_cached ? ' (read may be cached)' : ''} · ${esc(timeAgo(d.bench.at))}</span>`;
  }
  const io = d.io
    ? `<span class="io-r">Read <b>${esc(formatRate(d.io.read_bps))}</b></span><span class="io-w">Write <b>${esc(formatRate(d.io.write_bps))}</b></span>`
    : '<span>Live speed unavailable on this system</span>';
  return `<div class="disk-row${d.low ? ' low' : ''}">
    <div class="disk-head">
      <span class="disk-name">${esc(d.label)}${d.device ? ` <span class="disk-dev">${esc(d.device)}</span>` : ''}</span>
      <span class="disk-free-big"><strong>${esc(formatBytes(d.free))}</strong> free of ${esc(formatBytes(d.total))}${d.low ? ' · running low' : ''}</span>
    </div>
    <div class="disk-paths">${esc((d.paths || []).join('  ·  '))}</div>
    <div class="disk-bar" role="progressbar" aria-label="${esc(d.label)} used" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${Math.round(pct)}"><span style="width:${pct.toFixed(1)}%"></span></div>
    <div class="disk-meta">
      <span>Used <b>${esc(formatBytes(d.used))}</b> (${Math.round(pct)}%)</span>
      ${io}
      <span class="disk-bench">${bench}
        <button class="btn btn-ghost btn-sm" data-bench="${esc(d.id)}"${benchBusy ? ' disabled' : ''}>Speed test</button>
      </span>
    </div>
  </div>`;
}

async function loadDisks() {
  try {
    const r = await fetch('/api/disk');
    if (r.status === 401) { window.location.href = '/admin/login'; return; }
    const data = await r.json();
    const disks = data.disks || [];
    const busy = !!data.bench_running || Object.values(diskBenchState).some(s => s.running);
    document.getElementById('diskList').innerHTML = disks.length
      ? disks.map(d => renderDisk(d, busy)).join('')
      : '<p class="disk-empty">No disks configured</p>';
    const sized = disks.filter(d => d.total > 0);
    if (sized.length) {
      const worst = sized.reduce((a, b) => (b.free < a.free ? b : a));
      document.getElementById('diskUsage').textContent = formatBytes(worst.free);
    }
  } catch (e) {
    document.getElementById('diskList').innerHTML = `<p class="disk-empty">Could not load disk info: ${esc(e.message)}</p>`;
  }
}

async function runDiskBench(id) {
  diskBenchState[id] = {running: true};
  loadDisks();
  try {
    const r = await fetch('/api/admin/disk-bench?disk=' + encodeURIComponent(id), {
      method: 'POST',
      headers: {'X-CSRF-Token': CSRF_TOKEN}
    });
    const data = await r.json().catch(() => ({}));
    diskBenchState[id] = r.ok ? {} : {error: data.error || ('HTTP ' + r.status)};
  } catch (e) {
    diskBenchState[id] = {error: e.message};
  }
  loadDisks();
}

document.getElementById('diskList').addEventListener('click', e => {
  const btn = e.target.closest('[data-bench]');
  if (btn && !btn.disabled) runDiskBench(btn.dataset.bench);
});
loadDisks();
setInterval(() => { if (!document.hidden) loadDisks(); }, 3000);

// Event wiring — replaces the inline on* attributes so the CSP can drop
// 'unsafe-inline' for scripts.
const bind = (id, ev, fn) => { const el = document.getElementById(id); if (el) el.addEventListener(ev, fn); };
bind('refreshFilesBtn', 'click', loadFiles);
bind('refreshHistoryBtn', 'click', loadHistory);
bind('saveSettingsBtn', 'click', saveSettings);
bind('addUserBtn', 'click', addUser);
bind('testTransmissionBtn', 'click', testTransmission);
bind('updateYtdlpBtn', 'click', updateYtdlp);
bind('cookiesFileInput', 'change', uploadCookies);
bind('pickCookiesBtn', 'click', () => document.getElementById('cookiesFileInput').click());
bind('clearCookiesBtn', 'click', clearCookies);
bind('refreshCookiesBtn', 'click', loadCookieStatus);
bind('confirmCancelBtn', 'click', closeConfirm);
bind('confirmDeleteBtn', 'click', confirmDelete);
bind('saveBarBtn', 'click', saveSettings);

// Any edit to a setting arms the save bar.
settingKeys.forEach(k => {
  const el = document.getElementById('s_' + k);
  if (el) { el.addEventListener('input', markSettingsDirty); el.addEventListener('change', markSettingsDirty); }
});
bind('s_debug_mode', 'change', markSettingsDirty);
bind('s_direct_downloads', 'change', markSettingsDirty);
bind('s_kill_switch', 'change', markSettingsDirty);
bind('s_kill_switch_ips', 'input', markSettingsDirty);
bind('ksDetectBtn', 'click', detectKillSwitchIP);

// File rows are re-rendered constantly, so delegate instead of rebinding.
document.getElementById('fileBody').addEventListener('click', e => {
  const btn = e.target.closest('[data-delete]');
  if (btn) askDelete(btn.dataset.delete);
});

