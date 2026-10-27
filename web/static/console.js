/* Puppetron console: thin wrapper around noVNC's RFB class.
   noVNC itself is served from /novnc/ (see README for installing it). */

const $ = (s) => document.querySelector(s);
const params = new URLSearchParams(location.search);
const vm = params.get('vm');

const ui = {
  status: $('#c-status'), title: $('#c-title'), screen: $('#screen'),
  overlay: $('#overlay'), oTitle: $('#o-title'), oMsg: $('#o-msg'), oRetry: $('#o-retry'), oStart: $('#o-start'),
};

let rfb = null;
let RFB = null;
let keysyms = null;
let KeyTable = null;
let scaling = true;
let lastPassword = '';
let retryTimer = null;
const heldKeys = new Set();

function toast(msg, kind = '') {
  const el = document.createElement('div');
  el.className = 'toast ' + kind;
  el.textContent = msg;
  $('#toasts').appendChild(el);
  setTimeout(() => el.remove(), kind === 'err' ? 8000 : 3500);
}

async function api(path, opts = {}) {
  const headers = Object.assign({ 'X-Requested-With': 'puppetron' }, opts.headers || {});
  if (opts.body !== undefined && typeof opts.body !== 'string') {
    headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(opts.body);
  }
  const res = await fetch(path, Object.assign({}, opts, { headers, credentials: 'same-origin' }));
  if (res.status === 401) {
    location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search);
    throw new Error('not authenticated');
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw Object.assign(new Error(data.error || ('HTTP ' + res.status)), { status: res.status, data });
  return data;
}

function setStatus(text, cls) {
  ui.status.textContent = text;
  ui.status.className = 'badge ' + (cls || '');
}

function showOverlay(title, msg, { canStart = false } = {}) {
  ui.oTitle.textContent = title;
  ui.oMsg.textContent = msg || '';
  ui.oStart.hidden = !canStart;
  ui.overlay.hidden = false;
}
function hideOverlay() { ui.overlay.hidden = true; }

async function loadNoVNC() {
  if (RFB) return;
  try {
    const mod = await import('/novnc/core/rfb.js');
    RFB = mod.default;
    keysyms = (await import('/novnc/core/input/keysymdef.js')).default;
    KeyTable = (await import('/novnc/core/input/keysym.js')).default;
  } catch (e) {
    throw new Error('noVNC could not be loaded from /novnc/. Install it on the server (apt install novnc, or git clone https://github.com/novnc/noVNC into ./novnc). ' + e.message);
  }
}

async function connect() {
  if (!vm) { showOverlay('No VM specified', 'Open this page as /console?vm=<name>'); return; }
  clearTimeout(retryTimer);
  disconnect();
  setStatus('connecting…', '');
  showOverlay('Connecting…', `Requesting console for ${vm}`);
  let info;
  try {
    await loadNoVNC();
    info = await api(`/api/vms/${encodeURIComponent(vm)}/console`, { method: 'POST' });
  } catch (e) {
    const off = e.status === 409 && /running|allocated/i.test(e.message);
    showOverlay(off ? 'VM is not running' : 'Console unavailable', e.message, { canStart: off });
    setStatus('disconnected', 'disconnected');
    return;
  }

  let url;
  if (info.external) {
    url = info.ws_url;
  } else {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    url = `${proto}://${location.host}/${info.ws_path}`;
  }
  lastPassword = info.password || '';

  rfb = new RFB(ui.screen, url, { credentials: { password: lastPassword }, wsProtocols: ['binary'] });
  rfb.scaleViewport = scaling;
  rfb.resizeSession = false;
  rfb.background = '#000';
  rfb.focusOnClick = true;

  rfb.addEventListener('connect', () => {
    setStatus('connected', 'connected');
    hideOverlay();
    rfb.focus();
  });
  rfb.addEventListener('disconnect', (e) => {
    setStatus('disconnected', 'disconnected');
    const clean = e.detail && e.detail.clean;
    showOverlay(clean ? 'Disconnected' : 'Connection lost', clean ? 'The VNC session ended.' : 'The connection to the VM console was lost (VM stopped, or network problem).');
    rfb = null;
  });
  rfb.addEventListener('credentialsrequired', () => {
    const pw = prompt('This VM requires a VNC password:');
    if (pw !== null) rfb.sendCredentials({ password: pw });
  });
  rfb.addEventListener('securityfailure', (e) => {
    toast('Security failure: ' + (e.detail && e.detail.reason), 'err');
  });
  rfb.addEventListener('desktopname', (e) => {
    ui.title.textContent = `${vm} – ${e.detail.name}`;
  });
  rfb.addEventListener('clipboard', (e) => {
    $('#clip-text').value = e.detail.text;
  });
}

function disconnect() {
  if (rfb) {
    try { rfb.disconnect(); } catch (e) { /* ignore */ }
    rfb = null;
  }
}

async function startAndRetry() {
  ui.oStart.disabled = true;
  try {
    await api(`/api/vms/${encodeURIComponent(vm)}/action`, { method: 'POST', body: { action: 'start' } });
    toast('VM starting…', 'ok');
    showOverlay('Starting VM…', 'Waiting for the VNC server to come up.');
    let attempts = 0;
    const tryConnect = async () => {
      attempts++;
      try {
        await api(`/api/vms/${encodeURIComponent(vm)}/console`, { method: 'POST' });
        connect();
      } catch (e) {
        if (attempts < 20) retryTimer = setTimeout(tryConnect, 1500);
        else showOverlay('Console unavailable', e.message, { canStart: false });
      }
    };
    retryTimer = setTimeout(tryConnect, 1500);
  } catch (e) {
    toast('Start failed: ' + e.message, 'err');
  } finally {
    ui.oStart.disabled = false;
  }
}

// ------------------------------------------------------------ toolbar
ui.title.textContent = vm || 'Console';
document.title = `${vm || 'Console'} – Puppetron`;

ui.oRetry.addEventListener('click', connect);
ui.oStart.addEventListener('click', startAndRetry);
$('#c-reconnect').addEventListener('click', connect);

$('#c-cad').addEventListener('click', () => rfb && rfb.sendCtrlAltDel());

$('#c-scale').addEventListener('click', (e) => {
  scaling = !scaling;
  e.target.textContent = 'Scale: ' + (scaling ? 'on' : 'off');
  if (rfb) rfb.scaleViewport = scaling;
});

$('#c-fullscreen').addEventListener('click', () => {
  if (document.fullscreenElement) document.exitFullscreen();
  else document.body.requestFullscreen().catch(() => {});
});

// extra keys panel
$('#c-keys').addEventListener('click', () => { const p = $('#keys-panel'); p.hidden = !p.hidden; });
$('#keys-panel').addEventListener('click', (e) => {
  const b = e.target.closest('button');
  if (!b || !rfb || !KeyTable) return;
  if (b.dataset.key) {
    rfb.sendKey(KeyTable[b.dataset.key], b.dataset.code);
    // release any held modifiers after a key press (like a sticky-keys UX)
    releaseHeld();
  } else if (b.dataset.toggle) {
    const sym = KeyTable[b.dataset.toggle];
    if (heldKeys.has(sym)) {
      rfb.sendKey(sym, b.dataset.code, false);
      heldKeys.delete(sym); b.classList.remove('on');
    } else {
      rfb.sendKey(sym, b.dataset.code, true);
      heldKeys.add(sym); b.classList.add('on');
    }
  }
  rfb.focus();
});
function releaseHeld() {
  if (!rfb) return;
  for (const sym of heldKeys) rfb.sendKey(sym, null, false);
  heldKeys.clear();
  document.querySelectorAll('#keys-panel button.on').forEach(b => b.classList.remove('on'));
}

// clipboard panel
$('#c-clip').addEventListener('click', () => { const p = $('#clip-panel'); p.hidden = !p.hidden; });
$('#clip-close').addEventListener('click', () => { $('#clip-panel').hidden = true; });
$('#clip-send').addEventListener('click', () => {
  if (!rfb) return;
  rfb.clipboardPasteFrom($('#clip-text').value);
  toast('Sent to VM clipboard', 'ok');
  rfb.focus();
});
$('#clip-type').addEventListener('click', async () => {
  if (!rfb || !keysyms) return;
  const text = $('#clip-text').value;
  for (const ch of text) {
    let sym;
    if (ch === '\n') sym = KeyTable.XK_Return;
    else if (ch === '\t') sym = KeyTable.XK_Tab;
    else sym = keysyms.lookup(ch.codePointAt(0));
    if (!sym) continue;
    rfb.sendKey(sym, null, true);
    rfb.sendKey(sym, null, false);
    await new Promise(r => setTimeout(r, 8));
  }
  rfb.focus();
});

// power menu
const powerMenu = $('#c-power-menu');
$('#c-power').addEventListener('click', (e) => { e.stopPropagation(); powerMenu.hidden = !powerMenu.hidden; });
document.addEventListener('click', () => { powerMenu.hidden = true; });
powerMenu.addEventListener('click', async (e) => {
  const b = e.target.closest('button[data-action]');
  if (!b) return;
  const action = b.dataset.action;
  if ((action === 'destroy' || action === 'reset') && !confirm(`${b.textContent} "${vm}"?`)) return;
  try {
    const r = await api(`/api/vms/${encodeURIComponent(vm)}/action`, { method: 'POST', body: { action } });
    toast(r.message || `${action} sent`, 'ok');
    if (action === 'start') startAndRetry();
  } catch (ex) {
    toast(`${action} failed: ${ex.message}`, 'err');
  }
});

// full noVNC client link (fresh token on click)
$('#c-full-novnc').addEventListener('click', async (e) => {
  e.preventDefault();
  try {
    const info = await api(`/api/vms/${encodeURIComponent(vm)}/console`, { method: 'POST' });
    if (info.external) {
      const u = new URL(info.ws_url);
      const q = new URLSearchParams({ autoconnect: '1', host: u.hostname, port: u.port || (u.protocol === 'wss:' ? '443' : '80'), encrypt: u.protocol === 'wss:' ? '1' : '0', path: u.pathname.replace(/^\//, '') + u.search });
      window.open('/novnc/vnc.html?' + q.toString(), '_blank', 'noopener');
    } else {
      const q = new URLSearchParams({ autoconnect: '1', path: info.ws_path, resize: 'scale', reconnect: '0' });
      window.open('/novnc/vnc.html?' + q.toString(), '_blank', 'noopener');
    }
  } catch (ex) { toast(ex.message, 'err'); }
});

window.addEventListener('beforeunload', () => disconnect());
connect();
