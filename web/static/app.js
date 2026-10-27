/* Puppetron dashboard */
(() => {
  'use strict';

  // ------------------------------------------------------------ helpers
  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

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
    const ct = res.headers.get('content-type') || '';
    const data = ct.includes('json') ? await res.json().catch(() => ({})) : await res.text();
    if (!res.ok) {
      const msg = (data && data.error) || (typeof data === 'string' && data) || ('HTTP ' + res.status);
      const err = new Error(msg);
      err.data = data;
      throw err;
    }
    return data;
  }

  function toast(msg, kind = '') {
    const el = document.createElement('div');
    el.className = 'toast ' + kind;
    el.textContent = msg;
    $('#toasts').appendChild(el);
    setTimeout(() => el.remove(), kind === 'err' ? 8000 : 4000);
  }

  function esc(s) {
    return String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }

  function fmtKiB(kib) {
    if (!kib) return '0';
    return fmtBytes(kib * 1024);
  }
  function fmtBytes(b) {
    if (b == null || isNaN(b)) return '–';
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    let i = 0;
    while (b >= 1024 && i < units.length - 1) { b /= 1024; i++; }
    return (i === 0 ? b : b.toFixed(b < 10 ? 2 : 1)) + ' ' + units[i];
  }
  function fmtBps(b) {
    if (!b) return '0 B/s';
    return fmtBytes(b) + '/s';
  }
  function stateLabel(d) {
    return d.state || d.state_key;
  }
  function isActive(d) {
    return ['running', 'paused', 'idle', 'blocked', 'inshutdown'].includes(d.state_key);
  }

  // ------------------------------------------------------------ state
  const state = {
    vms: [],
    selected: null,
    tab: 'overview',
    caps: {},
    listTimer: null,
    statsTimer: null,
    hostTimer: null,
    busy: new Set(),
  };

  // ------------------------------------------------------------ host
  async function loadHost() {
    try {
      const h = await api('/api/host');
      $('#conn-status').className = 'dot ' + (h.error ? 'err' : 'ok');
      $('#conn-status').title = h.error ? h.error : (h.uri + ' · ' + (h.libvirt_version || '') + ' · ' + (h.hypervisor_version || ''));
      $('#host-name').textContent = h.hostname || '';
      if (!h.error) {
        setMeter('#host-cpu', h.cpu_usage, h.cpu_usage.toFixed(0) + '% of ' + h.cpus + ' CPUs');
        const used = h.mem_total_kib - h.mem_free_kib;
        setMeter('#host-mem', h.mem_total_kib ? used / h.mem_total_kib * 100 : 0, fmtKiB(used) + ' / ' + fmtKiB(h.mem_total_kib));
      }
    } catch (e) {
      $('#conn-status').className = 'dot err';
      $('#conn-status').title = e.message;
    }
  }
  function setMeter(sel, pct, text) {
    const m = $(sel);
    const fill = $('.fill', m);
    fill.style.width = Math.max(0, Math.min(100, pct || 0)) + '%';
    fill.className = 'fill ' + (pct > 90 ? 'err' : pct > 75 ? 'warn' : '');
    $('.meter-value', m).textContent = text;
  }

  // ------------------------------------------------------------ VM list
  async function loadVMs() {
    try {
      const vms = await api('/api/vms');
      state.vms = vms;
      $('#list-error').hidden = true;
      renderList();
      if (state.selected) {
        const d = vms.find(v => v.name === state.selected);
        if (d) renderDetailHeader(d);
      }
    } catch (e) {
      $('#list-error').textContent = 'Cannot load VMs: ' + e.message;
      $('#list-error').hidden = false;
    }
  }

  function filteredVMs() {
    const q = $('#filter').value.trim().toLowerCase();
    const st = $('#state-filter').value;
    return state.vms.filter(d => {
      if (q && !(d.name.toLowerCase().includes(q) || (d.title || '').toLowerCase().includes(q) || d.ips.some(ip => ip.address.includes(q)))) return false;
      if (st === 'running' && d.state_key !== 'running') return false;
      if (st === 'shutoff' && d.state_key !== 'shutoff') return false;
      if (st === 'paused' && !['paused', 'pmsuspended'].includes(d.state_key)) return false;
      if (st === 'other' && ['running', 'shutoff', 'paused', 'pmsuspended'].includes(d.state_key)) return false;
      return true;
    });
  }

  function renderList() {
    const list = $('#vm-list');
    const vms = filteredVMs();
    const running = state.vms.filter(v => v.state_key === 'running').length;
    const memRunning = state.vms.filter(isActive).reduce((a, v) => a + v.used_mem_kib, 0);
    const cpuRunning = state.vms.filter(isActive).reduce((a, v) => a + v.vcpus, 0);
    $('#summary').innerHTML = `<span><b>${state.vms.length}</b> VMs</span><span><b>${running}</b> running</span><span><b>${cpuRunning}</b> vCPUs allocated</span><span><b>${fmtKiB(memRunning)}</b> RAM allocated</span>`;
    $('#list-empty').hidden = vms.length > 0;

    const existing = new Map($$('.vm-card', list).map(el => [el.dataset.name, el]));
    const frag = document.createDocumentFragment();
    for (const d of vms) {
      let card = existing.get(d.name);
      if (!card) {
        card = document.createElement('article');
        card.className = 'vm-card';
        card.dataset.name = d.name;
        card.addEventListener('click', (e) => {
          if (e.target.closest('button')) return;
          select(d.name);
        });
      }
      existing.delete(d.name);
      card.classList.toggle('selected', state.selected === d.name);
      card.innerHTML = cardHTML(d);
      frag.appendChild(card);
    }
    existing.forEach(el => el.remove());
    list.appendChild(frag);
  }

  function cardHTML(d) {
    const ips = d.ips.filter(i => i.protocol === 'ipv4').map(i => i.address.split('/')[0]);
    const tags = [];
    if (d.autostart) tags.push('<span class="badge tag" title="Autostart enabled">autostart</span>');
    if (d.console_type === 'vnc') tags.push('<span class="badge tag" title="VNC console available">vnc</span>');
    else if (d.console_type !== 'none') tags.push(`<span class="badge tag" title="Graphics type">${esc(d.console_type)}</span>`);
    if (!d.persistent) tags.push('<span class="badge tag" title="Transient domain">transient</span>');
    if (d.error) tags.push(`<span class="badge tag crashed" title="${esc(d.error)}">!</span>`);
    const busy = state.busy.has(d.name) ? '<span class="spinner" title="Working…"></span>' : '';
    return `
      <div class="state-strip ${esc(d.state_key)}"></div>
      <div>
        <div class="title">${esc(d.name)} <span class="badge ${esc(d.state_key)}">${esc(stateLabel(d))}</span> ${tags.join(' ')} ${busy}</div>
        <div class="meta">
          ${d.title ? `<span>${esc(d.title)}</span>` : ''}
          <span>${d.vcpus} vCPU</span>
          <span>${fmtKiB(d.max_mem_kib)}</span>
          ${ips.length ? `<span class="ip">${esc(ips.join(', '))}</span>` : ''}
        </div>
      </div>
      <div class="card-actions">${quickActionsHTML(d)}</div>`;
  }

  function quickActionsHTML(d) {
    const b = (action, label, title, cls = '') => `<button class="btn sm ${cls}" data-action="${action}" data-vm="${esc(d.name)}" title="${title}">${label}</button>`;
    let html = '';
    if (d.state_key === 'shutoff' || d.state_key === 'crashed' || d.state_key === 'pmsuspended') html += b('start', '▶', 'Start');
    if (d.state_key === 'paused') html += b('resume', '▶', 'Resume');
    if (d.state_key === 'running') {
      html += b('shutdown', '⏻', 'Shutdown (ACPI)');
      html += b('destroy', '■', 'Force off', 'danger');
    }
    if (isActive(d) && d.console_type === 'vnc') {
      html += `<a class="btn sm primary" href="/console?vm=${encodeURIComponent(d.name)}" target="_blank" rel="noopener" title="Open console">Console</a>`;
    }
    return html;
  }

  // ------------------------------------------------------------ actions
  const confirmActions = {
    destroy: 'Force off (pull the plug on) "%s"? Unsaved guest data may be lost.',
    reset: 'Hard-reset "%s"? This is like pressing the reset button.',
  };
  const actionLabels = {
    start: 'Start', shutdown: 'Shutdown', reboot: 'Reboot', reset: 'Reset', destroy: 'Force off',
    suspend: 'Pause', resume: 'Resume', managedsave: 'Managed save', 'autostart-on': 'Enable autostart', 'autostart-off': 'Disable autostart',
  };

  async function doAction(name, action) {
    if (confirmActions[action] && !confirm(confirmActions[action].replace('%s', name))) return;
    state.busy.add(name);
    renderList();
    try {
      const r = await api(`/api/vms/${encodeURIComponent(name)}/action`, { method: 'POST', body: { action } });
      toast(r.message || `${actionLabels[action] || action}: ${name}`, 'ok');
    } catch (e) {
      toast(`${actionLabels[action] || action} failed: ${e.message}`, 'err');
    } finally {
      state.busy.delete(name);
      await loadVMs();
      if (state.selected === name) await loadDetail(name);
    }
  }

  document.addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-action]');
    if (!btn) return;
    const name = btn.dataset.vm || state.selected;
    if (!name) return;
    doAction(name, btn.dataset.action);
  });

  // ------------------------------------------------------------ detail
  async function select(name) {
    state.selected = name;
    renderList();
    $('#detail-pane').hidden = false;
    history.replaceState(null, '', '#' + encodeURIComponent(name));
    await loadDetail(name);
    showTab(state.tab);
  }

  function closeDetail() {
    state.selected = null;
    $('#detail-pane').hidden = true;
    history.replaceState(null, '', location.pathname);
    stopStats();
    renderList();
  }

  async function loadDetail(name) {
    try {
      const d = await api(`/api/vms/${encodeURIComponent(name)}`);
      renderDetailHeader(d);
      renderOverview(d);
      renderSettings(d);
      if (isActive(d)) startStats(name); else { stopStats(); clearStats(); }
      if (state.tab === 'snapshots') loadSnapshots();
    } catch (e) {
      toast('Cannot load VM: ' + e.message, 'err');
      if (/not found/i.test(e.message)) closeDetail();
    }
  }

  function renderDetailHeader(d) {
    $('#d-name').textContent = d.name;
    $('#d-state').textContent = stateLabel(d);
    $('#d-state').className = 'badge ' + d.state_key;
    $('#d-uuid').textContent = d.uuid;
    const acts = [];
    const b = (action, label, cls = '') => acts.push(`<button class="btn ${cls}" data-action="${action}">${label}</button>`);
    switch (d.state_key) {
      case 'running':
        b('shutdown', '⏻ Shutdown'); b('reboot', '↻ Reboot'); b('suspend', '⏸ Pause'); b('reset', 'Reset'); b('destroy', '■ Force off', 'danger');
        break;
      case 'paused':
        b('resume', '▶ Resume'); b('destroy', '■ Force off', 'danger');
        break;
      case 'shutoff': case 'crashed': case 'pmsuspended':
        b('start', '▶ Start', 'primary');
        break;
      default:
        b('destroy', '■ Force off', 'danger');
    }
    if (isActive(d) && d.console_type === 'vnc') {
      acts.unshift(`<a class="btn primary" href="/console?vm=${encodeURIComponent(d.name)}" target="_blank" rel="noopener">🖥 Console</a>`);
    }
    $('#d-actions').innerHTML = acts.join('');
  }

  function renderOverview(d) {
    const kv = [
      ['Title', d.title], ['Description', d.description], ['ID', d.id >= 0 ? d.id : '–'],
      ['vCPUs', d.vcpus], ['Max memory', fmtKiB(d.max_mem_kib)], ['Current memory', fmtKiB(d.used_mem_kib)],
      ['Arch / machine', [d.arch, d.machine].filter(Boolean).join(' / ')], ['Firmware', d.firmware],
      ['OS type', d.os_type], ['CPU time', d.cpu_time], ['Persistent', d.persistent ? 'yes' : 'no'],
      ['Autostart', d.autostart ? 'yes' : 'no'], ['Managed save', d.managed_save ? 'yes' : 'no'],
      ['Snapshots', d.has_snapshots ? 'yes' : 'no'],
    ].filter(([, v]) => v !== undefined && v !== '' && v !== null);
    $('#d-info').innerHTML = kv.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('');

    table('#d-ips', ['Interface', 'MAC', 'Protocol', 'Address'], d.ips.map(i => [i.iface, i.mac, i.protocol, i.address]), 'No addresses reported (VM off, no DHCP lease, or no guest agent).');
    table('#d-disks', ['Target', 'Device', 'Bus', 'Format', 'Source'], d.disks.map(x => [x.target, x.device, x.bus, x.format, x.source]), 'No disks.');
    table('#d-nics', ['MAC', 'Type', 'Source', 'Model'], d.nics.map(n => [n.mac, n.type, n.source, n.model]), 'No network interfaces.');
    table('#d-graphics', ['Type', 'Listen', 'Port', 'Socket'], d.graphics.map(g => [g.type, g.listen, g.port > 0 ? g.port : '(auto)', g.socket || '']), 'No graphics device – console unavailable.');
  }

  function table(sel, headers, rows, empty) {
    const t = $(sel);
    if (!rows.length) { t.innerHTML = `<tr><td class="muted">${esc(empty)}</td></tr>`; return; }
    t.innerHTML = `<tr>${headers.map(h => `<th>${esc(h)}</th>`).join('')}</tr>` +
      rows.map(r => `<tr>${r.map(c => `<td class="mono">${esc(c)}</td>`).join('')}</tr>`).join('');
  }

  function renderSettings(d) {
    const ab = $('#autostart-btn');
    ab.textContent = d.autostart ? 'Disable autostart' : 'Enable autostart';
    ab.dataset.action = d.autostart ? 'autostart-off' : 'autostart-on';
    $('#undefine-btn').disabled = !state.caps.undefine;
    $('#xml-save').hidden = !state.caps.xml_edit;
  }

  // ------------------------------------------------------------ stats
  function startStats(name) {
    stopStats();
    const tick = async () => {
      if (state.selected !== name) return;
      try {
        const s = await api(`/api/vms/${encodeURIComponent(name)}/stats`);
        $('#s-cpu').textContent = s.cpu_percent.toFixed(1) + '%' + (s.vcpus ? ` · ${s.vcpus} vCPU` : '');
        setFill('#s-cpu-bar', s.cpu_percent);
        const memPct = s.mem_max_kib ? s.mem_used_kib / s.mem_max_kib * 100 : 0;
        $('#s-mem').textContent = `${fmtKiB(s.mem_used_kib)} / ${fmtKiB(s.mem_max_kib)}`;
        setFill('#s-mem-bar', memPct);
        const rd = s.blocks.reduce((a, b) => a + b.rd_bps, 0), wr = s.blocks.reduce((a, b) => a + b.wr_bps, 0);
        $('#s-disk').textContent = `↓ ${fmtBps(rd)}  ↑ ${fmtBps(wr)}`;
        const rx = s.nets.reduce((a, b) => a + b.rx_bps, 0), tx = s.nets.reduce((a, b) => a + b.tx_bps, 0);
        $('#s-net').textContent = `↓ ${fmtBps(rx)}  ↑ ${fmtBps(tx)}`;
      } catch (e) { /* VM may have stopped */ }
    };
    tick();
    state.statsTimer = setInterval(tick, 3000);
  }
  function stopStats() { if (state.statsTimer) clearInterval(state.statsTimer); state.statsTimer = null; }
  function clearStats() {
    ['#s-cpu', '#s-mem', '#s-disk', '#s-net'].forEach(s => $(s).textContent = '–');
    setFill('#s-cpu-bar', 0); setFill('#s-mem-bar', 0);
  }
  function setFill(sel, pct) {
    const f = $(sel);
    f.style.width = Math.max(0, Math.min(100, pct || 0)) + '%';
    f.className = 'fill ' + (pct > 90 ? 'err' : pct > 75 ? 'warn' : '');
  }

  // ------------------------------------------------------------ tabs
  function showTab(tab) {
    state.tab = tab;
    $$('#tabs button').forEach(b => b.classList.toggle('active', b.dataset.tab === tab));
    $$('.tab').forEach(s => s.hidden = s.dataset.tab !== tab);
    if (tab === 'snapshots') loadSnapshots();
    if (tab === 'xml') loadXML();
  }
  $('#tabs').addEventListener('click', e => {
    const b = e.target.closest('button[data-tab]');
    if (b) showTab(b.dataset.tab);
  });

  // ------------------------------------------------------------ snapshots
  async function loadSnapshots() {
    const name = state.selected;
    if (!name) return;
    const t = $('#snap-table');
    try {
      const snaps = await api(`/api/vms/${encodeURIComponent(name)}/snapshots`);
      $('#snap-empty').hidden = snaps.length > 0;
      t.innerHTML = snaps.length ? `<tr><th>Name</th><th>Created</th><th>State</th><th>Parent</th><th>Description</th><th></th></tr>` +
        snaps.map(s => `<tr>
          <td class="mono">${esc(s.name)} ${s.current ? '<span class="badge tag">current</span>' : ''}</td>
          <td>${esc(s.created ? new Date(s.created).toLocaleString() : '')}</td>
          <td>${esc(s.state)}</td>
          <td class="mono">${esc(s.parent)}</td>
          <td>${esc(s.description)}</td>
          <td><div class="row-actions">
            <button class="btn sm" data-snap-revert="${esc(s.name)}">Revert</button>
            <button class="btn sm danger" data-snap-delete="${esc(s.name)}">Delete</button>
          </div></td></tr>`).join('') : '';
    } catch (e) {
      t.innerHTML = `<tr><td class="error">${esc(e.message)}</td></tr>`;
    }
  }

  $('#snap-form').addEventListener('submit', async e => {
    e.preventDefault();
    const name = state.selected;
    const body = { name: $('#snap-name').value.trim(), description: $('#snap-desc').value.trim() };
    const btn = $('button', e.target);
    btn.disabled = true;
    try {
      const r = await api(`/api/vms/${encodeURIComponent(name)}/snapshots`, { method: 'POST', body });
      toast(r.message || 'Snapshot created', 'ok');
      $('#snap-name').value = ''; $('#snap-desc').value = '';
      loadSnapshots();
    } catch (ex) {
      toast('Snapshot failed: ' + ex.message, 'err');
    } finally { btn.disabled = false; }
  });

  $('#snap-table').addEventListener('click', async e => {
    const rev = e.target.closest('[data-snap-revert]');
    const del = e.target.closest('[data-snap-delete]');
    const name = state.selected;
    if (rev) {
      const snap = rev.dataset.snapRevert;
      if (!confirm(`Revert "${name}" to snapshot "${snap}"? Current disk state will be lost.`)) return;
      try {
        const r = await api(`/api/vms/${encodeURIComponent(name)}/snapshots/${encodeURIComponent(snap)}/revert`, { method: 'POST' });
        toast(r.message || 'Reverted', 'ok');
        loadVMs(); loadDetail(name); loadSnapshots();
      } catch (ex) { toast('Revert failed: ' + ex.message, 'err'); }
    } else if (del) {
      const snap = del.dataset.snapDelete;
      if (!confirm(`Delete snapshot "${snap}"?`)) return;
      try {
        const r = await api(`/api/vms/${encodeURIComponent(name)}/snapshots/${encodeURIComponent(snap)}`, { method: 'DELETE' });
        toast(r.message || 'Snapshot deleted', 'ok');
        loadSnapshots();
      } catch (ex) { toast('Delete failed: ' + ex.message, 'err'); }
    }
  });

  // ------------------------------------------------------------ XML
  async function loadXML() {
    const name = state.selected;
    if (!name) return;
    const inactive = $('#xml-inactive').checked ? '1' : '0';
    try {
      $('#xml-text').value = await api(`/api/vms/${encodeURIComponent(name)}/xml?inactive=${inactive}`);
    } catch (e) {
      $('#xml-text').value = 'Error: ' + e.message;
    }
  }
  $('#xml-reload').addEventListener('click', loadXML);
  $('#xml-inactive').addEventListener('change', loadXML);
  $('#xml-save').addEventListener('click', async () => {
    const name = state.selected;
    if (!confirm(`Redefine "${name}" with the edited XML? Changes to a running VM apply after the next full stop/start.`)) return;
    try {
      const r = await api(`/api/vms/${encodeURIComponent(name)}/xml`, { method: 'PUT', body: $('#xml-text').value, headers: { 'Content-Type': 'application/xml' } });
      toast(r.message || 'Domain defined', 'ok');
      loadVMs(); loadDetail(name);
    } catch (e) { toast('Define failed: ' + e.message, 'err'); }
  });

  // ------------------------------------------------------------ undefine
  $('#undefine-btn').addEventListener('click', async () => {
    const name = state.selected;
    const storage = $('#undefine-storage').checked;
    const typed = prompt(`Type the VM name to confirm deletion${storage ? ' INCLUDING ALL DISK IMAGES' : ''}:\n${name}`);
    if (typed !== name) { if (typed !== null) toast('Name did not match – cancelled', 'err'); return; }
    try {
      const r = await api(`/api/vms/${encodeURIComponent(name)}?storage=${storage ? 1 : 0}`, { method: 'DELETE' });
      toast(r.message || 'VM deleted', 'ok');
      closeDetail();
      loadVMs();
    } catch (e) { toast('Delete failed: ' + e.message, 'err'); }
  });

  // ------------------------------------------------------------ new VM
  const dlg = $('#new-vm-dialog');
  $('#new-vm-btn').addEventListener('click', async () => {
    $('#new-vm-error').hidden = true;
    $('#new-vm-cmd').hidden = true;
    dlg.showModal();
    try {
      const [pools, nets, isos] = await Promise.all([api('/api/pools'), api('/api/networks'), api('/api/isos')]);
      $('#nv-pool').innerHTML = '<option value="">(default)</option>' + pools.filter(p => p.state === 'active').map(p => `<option value="${esc(p.name)}">${esc(p.name)} – ${fmtBytes(p.available_bytes)} free</option>`).join('');
      $('#nv-network').innerHTML = nets.map(n => `<option value="${esc(n.name)}" ${n.name === 'default' ? 'selected' : ''}>${esc(n.name)}${n.state !== 'active' ? ' (inactive)' : ''}</option>`).join('') || '<option value="default">default</option>';
      $('#iso-list').innerHTML = isos.map(i => `<option value="${esc(i)}">`).join('');
    } catch (e) { toast('Cannot load storage/network lists: ' + e.message, 'err'); }
  });
  $('#new-vm-cancel').addEventListener('click', () => dlg.close());
  $('#new-vm-form').addEventListener('submit', async e => {
    e.preventDefault();
    const f = new FormData(e.target);
    const body = {
      name: f.get('name').trim(),
      memory_mib: +f.get('memory_mib'),
      vcpus: +f.get('vcpus'),
      disk_gib: +f.get('disk_gib') || 0,
      pool: f.get('pool'),
      disk_path: f.get('disk_path').trim(),
      iso: f.get('iso').trim(),
      os_variant: f.get('os_variant').trim(),
      network: f.get('network'),
      bridge: f.get('bridge').trim(),
      uefi: f.get('uefi') === 'on',
      autostart: f.get('autostart') === 'on',
    };
    const btn = $('#new-vm-submit');
    btn.disabled = true; btn.textContent = 'Creating…';
    $('#new-vm-error').hidden = true;
    try {
      const r = await api('/api/vms', { method: 'POST', body });
      toast(`VM "${body.name}" created`, 'ok');
      dlg.close();
      await loadVMs();
      select(body.name);
    } catch (ex) {
      $('#new-vm-error').textContent = ex.message;
      $('#new-vm-error').hidden = false;
      if (ex.data && ex.data.command) {
        $('#new-vm-cmd').textContent = ex.data.command.join(' ') + (ex.data.output ? '\n\n' + ex.data.output : '');
        $('#new-vm-cmd').hidden = false;
      }
    } finally { btn.disabled = false; btn.textContent = 'Create'; }
  });

  // ------------------------------------------------------------ misc wiring
  $('#detail-close').addEventListener('click', closeDetail);
  $('#refresh-btn').addEventListener('click', () => { loadVMs(); loadHost(); if (state.selected) loadDetail(state.selected); });
  $('#filter').addEventListener('input', renderList);
  $('#state-filter').addEventListener('change', renderList);
  $('#logout-btn').addEventListener('click', async () => {
    try { await api('/api/logout', { method: 'POST' }); } catch (e) { /* ignore */ }
    location.href = '/login';
  });
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') { loadVMs(); loadHost(); }
  });

  async function init() {
    try {
      const me = await api('/api/me');
      $('#user-name').textContent = me.user;
      state.caps = await api('/api/capabilities');
      $('#new-vm-btn').hidden = !state.caps.create;
      if (!state.caps.novnc) toast('noVNC is not installed on the server – consoles will not work. See README.', 'err');
    } catch (e) { return; }
    await Promise.all([loadVMs(), loadHost()]);
    state.listTimer = setInterval(loadVMs, 5000);
    state.hostTimer = setInterval(loadHost, 10000);
    const hash = decodeURIComponent(location.hash.slice(1));
    if (hash && state.vms.some(v => v.name === hash)) select(hash);
  }
  init();
})();
