(() => {
  const BASE_PATH = window.location.pathname.startsWith('/app/fn-nspawn')
    ? '/app/fn-nspawn'
    : '';

  const state = {
    me: null,
    host: null,
    settings: null,
    initialized: false,
    containers: [],
    selectedMachine: null,
    snapshots: [],
    images: [],
    tasks: [],
    selectedTaskId: null,
    rollbackSnap: null,
    importPreview: null,
    fsBrowse: {
      currentPath: '',
      parent: '',
      mode: 'any', // 'dir' | 'config' | 'archive'
      onSelect: null,
    },
    terminals: {}, // machine -> { machine, el, term, fitAddon, ws, user, status, statusText }
    termUsers: {}, // machine -> username
    logWS: null,
    taskTimer: null,
    pollTimer: null,
    activeTab: 'overview',
    metricsPrev: {},
    liveStats: {},
  };

  const $ = (id) => document.getElementById(id);

  function apiUrl(path) {
    return `${BASE_PATH}${path}`;
  }

  function wsUrl(path) {
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    return `${proto}//${window.location.host}${BASE_PATH}${path}`;
  }

  async function apiFetch(path, opts = {}) {
    const res = await fetch(apiUrl(path), {
      headers: { 'Content-Type': 'application/json', ...(opts.headers || {}) },
      ...opts,
    });
    const text = await res.text();
    let data = {};
    try {
      data = text ? JSON.parse(text) : {};
    } catch (_) {
      data = { error: text };
    }
    if (!res.ok) {
      throw new Error(data.error || `HTTP ${res.status}`);
    }
    return data;
  }

  function toast(msg, type = 'ok') {
    const el = document.createElement('div');
    el.className = `toast ${type}`;
    el.textContent = msg;
    $('toastContainer').appendChild(el);
    setTimeout(() => el.remove(), 3800);
  }

  function esc(str) {
    return String(str ?? '').replace(/[&<>"']/g, (c) => ({
      '&': '&amp;',
      '<': '&lt;',
      '>': '&gt;',
      '"': '&quot;',
      "'": '&#39;',
    }[c]));
  }

  function formatBytes(bytes) {
    if (!bytes || bytes <= 0) return '0 B';
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    let i = 0;
    let v = bytes;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
  }

  function formatCPU(nsec) {
    if (!nsec || nsec <= 0) return '0 s';
    return `${(nsec / 1e9).toFixed(1)} s`;
  }

  function formatCompactBytes(bytes) {
    if (bytes == null || bytes < 0) return '-';
    if (bytes === 0) return '0B';
    const units = ['B', 'K', 'M', 'G', 'T'];
    let i = 0;
    let v = bytes;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return `${i === 0 ? v.toFixed(0) : (v >= 10 ? v.toFixed(1) : v.toFixed(1))}${units[i]}`;
  }

  function formatRate(bps) {
    if (bps == null || bps < 0) return '-';
    if (bps < 1) return '0B/s';
    const units = ['B/s', 'K/s', 'M/s', 'G/s'];
    let i = 0;
    let v = bps;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)}${units[i]}`;
  }

  function formatUptime(sec) {
    if (!sec || sec <= 0) return '-';
    const d = Math.floor(sec / 86400);
    const h = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60);
    if (d > 0) return `${d}d ${h}h`;
    if (h > 0) return `${h}h ${m}m`;
    if (m > 0) return `${m}m`;
    return `${sec}s`;
  }

  function renderDistroIcon(distro, osPretty) {
    const hint = `${distro || ''} ${osPretty || ''}`.toLowerCase();
    if (hint.includes('ubuntu')) {
      return '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9.5" fill="#E95420"/><circle cx="12" cy="12" r="4.2" fill="none" stroke="#fff" stroke-width="2.1"/><circle cx="12" cy="5.5" r="1.4" fill="#fff"/><circle cx="6.4" cy="15.3" r="1.4" fill="#fff"/><circle cx="17.6" cy="15.3" r="1.4" fill="#fff"/></svg>';
    }
    if (hint.includes('debian')) {
      return '<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="#D70A53" d="M12.7 3.2c-4.5-.3-8.5 3-8.8 7.5-.3 4.8 3.3 9.2 8.2 9.9.9.1 1.1-.6.4-.9-2.8-1-5.1-3.8-4.9-7.1.2-3.4 2.9-6 6.2-5.8 2.6.2 4.5 2.2 4.3 4.6-.1 2-1.8 3.4-3.7 3.3-1.4-.1-2.4-1.2-2.3-2.5.1-.9.9-1.6 1.8-1.5.5 0 .9.3 1 .8.1.4.7.4.9 0 .3-.8-.3-1.9-1.4-2.1-1.6-.3-3.2.9-3.4 2.6-.3 2.3 1.5 4.3 3.9 4.5 3 .2 5.7-2 5.9-5.1.3-4-2.7-7.9-8.1-8.3z"/></svg>';
    }
    return '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="4" fill="#334155"/><path d="M8 9.5l3 2.5-3 2.5M12.5 14.5h3.5" stroke="#38bdf8" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" fill="none"/></svg>';
  }

  function renderProgressBar(pct, subText) {
    const valid = pct != null && !Number.isNaN(pct);
    const clamped = valid ? Math.max(0, Math.min(100, pct)) : 0;
    const cls = clamped >= 85 ? 'hi' : (clamped >= 65 ? 'mid' : '');
    const outCls = clamped < 18 ? 'out' : '';
    const pctLabel = valid ? `${clamped > 0 && clamped < 10 ? clamped.toFixed(1) : Math.round(clamped)}%` : '-';
    return `
      <div class="pb">
        <i class="${cls}" style="width:${valid ? clamped : 0}%"></i>
        <b class="${outCls}">${pctLabel}</b>
      </div>
      ${subText ? `<span class="probe-sub">${esc(subText)}</span>` : ''}
    `;
  }

  function probeRow(iconId, label, valHtml) {
    return `
      <div class="probe-row">
        <label class="probe-label"><svg class="li"><use href="#${iconId}"/></svg>${esc(label)}</label>
        <div class="probe-val">${valHtml}</div>
      </div>
    `;
  }

  function openModal(id) { $(id).hidden = false; }
  function closeModal(id) { $(id).hidden = true; }

  document.querySelectorAll('[data-close]').forEach((btn) => {
    btn.addEventListener('click', () => closeModal(btn.dataset.close));
  });

  // Tabs
  $('detailTabs').addEventListener('click', (e) => {
    const btn = e.target.closest('.tab');
    if (!btn) return;
    const tab = btn.dataset.tab;
    state.activeTab = tab;
    document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('active', t === btn));
    document.querySelectorAll('.tab-pane').forEach((p) => p.classList.toggle('active', p.id === `tab-${tab}`));
    if (tab === 'terminal' && state.selectedMachine) {
      syncTerminalView(state.selectedMachine);
    }
    if (tab === 'snapshots' && state.selectedMachine) {
      loadSnapshots(state.selectedMachine);
      loadImages(state.selectedMachine);
    }
  });

  function updateThemeButton() {
    const cur = document.documentElement.dataset.theme || 'dark';
    const isDark = cur === 'dark';
    const useEl = $('themeIconUse');
    if (useEl) {
      useEl.setAttribute('href', isDark ? '#i-sun' : '#i-moon');
    }
    const btn = $('btnThemeToggle');
    btn.classList.toggle('is-dark', isDark);
    btn.classList.toggle('is-light', !isDark);
    const label = isDark ? '切换浅色外观' : '切换深色外观';
    btn.title = label;
    btn.setAttribute('aria-label', label);
  }

  function initSidebarResizer() {
    const resizer = $('sidebarResizer');
    const saved = parseInt(localStorage.getItem('fn-nspawn-sidebar-w') || '', 10);
    if (!Number.isNaN(saved) && saved >= 220 && saved <= 520) {
      document.documentElement.style.setProperty('--sidebar-w', `${saved}px`);
    }
    if (!resizer) return;

    let dragging = false;
    let startX = 0;
    let startW = 290;

    resizer.addEventListener('pointerdown', (e) => {
      if (window.innerWidth <= 900) return;
      dragging = true;
      startX = e.clientX;
      startW = $('sidebar').getBoundingClientRect().width || 290;
      resizer.classList.add('dragging');
      document.body.classList.add('resizing-sidebar');
      resizer.setPointerCapture && resizer.setPointerCapture(e.pointerId);
      e.preventDefault();
    });

    resizer.addEventListener('pointermove', (e) => {
      if (!dragging) return;
      const next = Math.max(220, Math.min(520, Math.round(startW + (e.clientX - startX))));
      document.documentElement.style.setProperty('--sidebar-w', `${next}px`);
    });

    const stopDrag = (e) => {
      if (!dragging) return;
      dragging = false;
      resizer.classList.remove('dragging');
      document.body.classList.remove('resizing-sidebar');
      const curW = Math.round($('sidebar').getBoundingClientRect().width);
      if (curW >= 220 && curW <= 520) {
        localStorage.setItem('fn-nspawn-sidebar-w', String(curW));
      }
      if (e && resizer.hasPointerCapture && resizer.hasPointerCapture(e.pointerId)) {
        resizer.releasePointerCapture(e.pointerId);
      }
    };

    resizer.addEventListener('pointerup', stopDrag);
    resizer.addEventListener('pointercancel', stopDrag);
    resizer.addEventListener('dblclick', () => {
      document.documentElement.style.setProperty('--sidebar-w', '290px');
      localStorage.setItem('fn-nspawn-sidebar-w', '290');
    });
  }

  $('btnThemeToggle').addEventListener('click', () => {
    const cur = document.documentElement.dataset.theme || 'dark';
    const next = cur === 'dark' ? 'light' : 'dark';
    localStorage.setItem('fn-nspawn-theme', next);
    window.applyTheme && window.applyTheme(next);
    updateThemeButton();
  });

  async function loadInitial() {
    initSidebarResizer();
    updateThemeButton();
    if (!state.initialized) {
      updateEmptyState('loading');
    }
    try {
      const [me, host, settings] = await Promise.all([
        apiFetch('/api/me').catch(() => ({ username: 'admin' })),
        apiFetch('/api/host'),
        apiFetch('/api/settings'),
      ]);
      state.me = me;
      state.host = host;
      state.settings = settings;
      renderHost();
      await loadContainers();
      await loadTasks();
      startAutoRefresh();
    } catch (err) {
      updateEmptyState('error', err.message);
      if (!state.containers.length) {
        $('containerList').innerHTML = `
          <div class="sidebar-empty">
            <div class="sidebar-empty-icon" aria-hidden="true"><svg viewBox="0 0 24 24"><use href="#i-cube"/></svg></div>
            <div>
              <div class="sidebar-empty-title">加载失败</div>
              <div class="sidebar-empty-sub">点击上方“刷新”重试</div>
            </div>
          </div>
        `;
      }
      toast(`初始化失败: ${err.message}`, 'err');
    }
  }

  function startAutoRefresh() {
    if (state.pollTimer) return;
    state.pollTimer = setInterval(() => {
      if (document.hidden) return;
      if (state.activeTab !== 'overview') return;
      if (document.querySelector('.modal-backdrop:not([hidden])')) return;
      loadContainers(true).catch(() => {});
    }, 4000);
  }

  function renderHost() {
    const h = state.host;
    if (!h) return;
    renderAboutModal();
    populateNICSelect($('cfgPhysicalNIC'), h.nics, undefined, $('cfgNICWarn'));
    populateNICSelect($('newPhysicalNIC'), h.nics, undefined, $('newNICWarn'));
    populateNICSelect($('impPhysicalNIC'), h.nics, undefined, $('impNICWarn'));
    populateNICSelect($('migPhysicalNIC'), h.nics, undefined, $('migNICWarn'));
    populateNICSelect($('setDefaultNIC'), h.nics, state.settings?.default_physical_nic, $('setDefaultNICWarn'));
  }

  function renderAboutModal() {
    const h = state.host || {};
    const me = state.me || {};
    const ver = h.app_version || '1.3.3';
    $('aboutVersion').textContent = `v${ver}`;
    const userDisplay = me.username || 'admin';
    const depPill = (ok) => ok
      ? '<span class="pill pill-ok">在线 / 已就绪</span>'
      : '<span class="pill pill-err">离线 / 未安装</span>';
    $('aboutInfoKV').innerHTML = kvRows([
      ['当前登录账号', `${esc(userDisplay)}（当前登录的飞牛系统管理员）`],
      ['宿主机名称', esc(h.hostname || '-')],
      ['操作系统', esc(h.os_version || '-')],
      ['内核与架构', esc(`${h.kernel || '-'} (${h.arch || '-'})`)],
      ['宿主机规格', `${h.cpu_cores || '-'} 核 CPU · ${h.total_memory_bytes ? formatBytes(h.total_memory_bytes) : '-'}`],
      ['systemd-nspawn', depPill(h.has_systemd_nspawn)],
      ['Open vSwitch (OVS)', depPill(h.has_ovs)],
      ['Btrfs', depPill(h.has_btrfs)],
      ['debootstrap', depPill(h.has_debootstrap)],
    ]);
  }

  $('btnAbout').addEventListener('click', () => {
    renderAboutModal();
    openModal('aboutModal');
  });

  function updateNICOVSWarning(selectEl, warnEl) {
    if (!selectEl || !warnEl) return;
    const h = state.host;
    const nicName = selectEl.value;
    if (!h) {
      warnEl.hidden = true;
      return;
    }
    if (!h.has_ovs) {
      warnEl.hidden = false;
      warnEl.textContent = '宿主机未检测到 Open vSwitch (OVS) 服务。本管理器依赖 OVS 网桥为容器分配独立局域网 IP，请先在飞牛系统网络设置开启 OVS。';
      return;
    }
    if (!nicName) {
      warnEl.hidden = true;
      return;
    }
    const nics = h.nics || [];
    const matched = nics.find((n) => n.name === nicName);
    if (!matched) {
      warnEl.hidden = false;
      warnEl.textContent = `当前选择的网卡 ${nicName} 在宿主机中未找到，请改选已开启 OVS 的物理网卡。`;
      return;
    }
    if (!matched.is_ovs_port) {
      warnEl.hidden = false;
      warnEl.textContent = `网卡 ${nicName} 当前未开启 OVS（未加入 OVS Bridge），容器无法桥接联网。请先到飞牛 NAS「网络设置」为该网卡开启 OVS，或改选已标注 [已开启 OVS] 的网卡。`;
      return;
    }
    warnEl.hidden = true;
    warnEl.textContent = '';
  }

  function populateNICSelect(selectEl, nics, currentVal, warnEl) {
    if (!selectEl) return;
    const prevVal = currentVal !== undefined ? currentVal : selectEl.value;
    const list = [...(nics || [])].sort((a, b) => {
      if (!!a.is_ovs_port !== !!b.is_ovs_port) return a.is_ovs_port ? -1 : 1;
      if ((a.oper_state === 'up') !== (b.oper_state === 'up')) return a.oper_state === 'up' ? -1 : 1;
      return a.name.localeCompare(b.name);
    });
    selectEl.innerHTML = list.map((n) => {
      const tag = n.is_ovs_port
        ? ` [已开启 OVS: ${n.ovs_bridge}]`
        : ` [未开启 OVS · ${n.oper_state || 'unknown'}]`;
      return `<option value="${esc(n.name)}">${esc(n.name)}${esc(tag)}</option>`;
    }).join('');
    if (prevVal && !list.some((n) => n.name === prevVal)) {
      selectEl.insertAdjacentHTML('beforeend', `<option value="${esc(prevVal)}">${esc(prevVal)} (未检测到 / 未开启 OVS)</option>`);
    }
    if (prevVal) {
      selectEl.value = prevVal;
    } else {
      const firstOVS = list.find((n) => n.is_ovs_port);
      if (firstOVS) {
        selectEl.value = firstOVS.name;
      } else if (list.length > 0) {
        selectEl.value = list[0].name;
      }
    }
    updateNICOVSWarning(selectEl, warnEl);
  }

  [
    ['cfgPhysicalNIC', 'cfgNICWarn'],
    ['newPhysicalNIC', 'newNICWarn'],
    ['impPhysicalNIC', 'impNICWarn'],
    ['migPhysicalNIC', 'migNICWarn'],
    ['setDefaultNIC', 'setDefaultNICWarn'],
  ].forEach(([selId, warnId]) => {
    const sel = $(selId);
    if (sel) {
      sel.addEventListener('change', () => updateNICOVSWarning(sel, $(warnId)));
    }
  });

  function updateLiveMetrics(list) {
    const now = Date.now();
    const nextPrev = {};
    const nextLive = {};
    (list || []).forEach((item) => {
      const m = item.config.machine;
      const rt = item.runtime;
      const running = rt.active_state === 'active';
      const prev = state.metricsPrev[m];
      let cpuPct = running ? 0 : null;
      let rxRate = running ? 0 : null;
      let txRate = running ? 0 : null;

      if (running && prev) {
        const dt = (now - prev.ts) / 1000;
        if (dt >= 0.5 && dt <= 30) {
          if (rt.cpu_usage_nsec >= prev.cpu) {
            const cores = Math.max(1, state.host?.cpu_cores || 1);
            cpuPct = Math.min(100, ((rt.cpu_usage_nsec - prev.cpu) / (dt * 1e9 * cores)) * 100);
          }
          if (rt.net_rx_bytes >= prev.rx) {
            rxRate = (rt.net_rx_bytes - prev.rx) / dt;
          }
          if (rt.net_tx_bytes >= prev.tx) {
            txRate = (rt.net_tx_bytes - prev.tx) / dt;
          }
        } else if (state.liveStats[m]) {
          cpuPct = state.liveStats[m].cpuPct;
          rxRate = state.liveStats[m].rxRate;
          txRate = state.liveStats[m].txRate;
        }
      }
      nextPrev[m] = {
        ts: now,
        cpu: rt.cpu_usage_nsec || 0,
        rx: rt.net_rx_bytes || 0,
        tx: rt.net_tx_bytes || 0,
      };
      nextLive[m] = { cpuPct, rxRate, txRate };
    });
    state.metricsPrev = nextPrev;
    state.liveStats = nextLive;
  }

  function updateEmptyState(mode, errMsg = '') {
    const pill = $('emptyStatusPill');
    const title = $('emptyTitle');
    const desc = $('emptyDesc');
    if (!pill || !title || !desc) return;

    const defaultDesc = '每个容器采用独立标准化目录（<code>rootfs/</code> + <code>config/container.json</code> + <code>images/</code>）持久化保存，支持随时快照回滚与重装系统后一键恢复。';
    if (mode === 'loading') {
      pill.hidden = false;
      pill.className = 'empty-status-pill';
      pill.innerHTML = '<span class="empty-spinner"></span><span>正在扫描宿主机容器...</span>';
      title.textContent = '欢迎使用 nspawn 容器';
      desc.innerHTML = defaultDesc;
      return;
    }
    if (mode === 'error') {
      pill.hidden = false;
      pill.className = 'empty-status-pill is-error';
      pill.innerHTML = `<span class="empty-status-dot"></span><span>初始化扫描失败：${esc(errMsg || '请检查后端服务')}</span>`;
      title.textContent = '暂时无法获取容器列表';
      desc.textContent = '请点击左侧“刷新”按钮重试，或检查 fn-nspawn 后端服务运行状态。';
      return;
    }
    if (mode === 'unselected') {
      pill.hidden = true;
      title.textContent = '未选择容器';
      desc.textContent = '请从左侧容器列表选择一个实例查看状态与控制台，或新建、恢复系统容器。';
      return;
    }
    const h = state.host;
    const allReady = !!(h && h.has_systemd_nspawn && h.has_ovs && h.has_btrfs);
    pill.hidden = false;
    pill.className = `empty-status-pill ${allReady ? 'is-ready' : ''}`;
    pill.innerHTML = `<span class="empty-status-dot"></span><span>${allReady ? '宿主机环境就绪 · 暂无容器实例' : '宿主机已连接 · 暂无容器实例'}</span>`;
    title.textContent = '开始创建你的第一个系统容器';
    desc.innerHTML = defaultDesc;
  }

  async function loadContainers(silent = false) {
    const list = await apiFetch('/api/containers');
    state.initialized = true;
    state.containers = list || [];
    updateLiveMetrics(state.containers);
    $('containerCount').textContent = String(state.containers.length);
    renderContainerList();

    if (state.selectedMachine && !state.containers.some((c) => c.config.machine === state.selectedMachine)) {
      state.selectedMachine = null;
    }
    const existingSet = new Set(state.containers.map((c) => c.config.machine));
    Object.keys(state.terminals).forEach((m) => {
      if (!existingSet.has(m)) {
        disposeTerminal(m);
      }
    });
    if (!state.selectedMachine && state.containers.length > 0) {
      selectContainer(state.containers[0].config.machine);
    } else if (state.selectedMachine) {
      renderDetail({ skipConfigForm: silent });
    } else {
      updateEmptyState(state.containers.length === 0 ? 'empty' : 'unselected');
      $('emptyState').hidden = false;
      $('detailView').hidden = true;
      syncTerminalView(null);
    }
  }

  function renderContainerList() {
    const el = $('containerList');
    if (state.containers.length === 0) {
      el.innerHTML = `
        <div class="sidebar-empty">
          <div class="sidebar-empty-icon" aria-hidden="true"><svg viewBox="0 0 24 24"><use href="#i-cube"/></svg></div>
          <div>
            <div class="sidebar-empty-title">暂无容器实例</div>
            <div class="sidebar-empty-sub">点击上方“新建容器”或“从配置恢复”</div>
          </div>
        </div>
      `;
      return;
    }
    el.innerHTML = state.containers.map((c) => {
      const { config, runtime } = c;
      const isActive = config.machine === state.selectedMachine;
      const running = runtime.active_state === 'active';
      const ip = (runtime.ipv4_addrs && runtime.ipv4_addrs[0]) || '无 IP';
      const mem = running ? formatCompactBytes(runtime.memory_current) : '已停止';
      const stateCls = running ? 'running' : (runtime.active_state === 'failed' ? 'failed' : 'stopped');
      const stateTxt = running ? 'RUN' : (runtime.active_state === 'failed' ? 'ERR' : 'OFF');
      const warn = config.layout === 'external'
        ? '<span class="pill">外部</span>'
        : (!config.link_healthy ? '<span class="pill pill-warn">未同步</span>' : '');
      const termLive = state.terminals[config.machine]?.status === 'connected'
        ? '<span class="term-live-dot" title="该容器有活跃网页终端会话">TERM</span>'
        : '';
      return `
        <div class="container-item ${isActive ? 'active' : ''}" data-machine="${esc(config.machine)}">
          <div class="ci-top">
            <div class="ci-title">
              <span class="distro-icon-sm">${renderDistroIcon(config.distro, runtime.os_pretty_name)}</span>
              <span class="ci-name">${esc(config.machine)}</span>
            </div>
            <div class="row-gap" style="gap:4px;">
              ${termLive}
              ${warn}
              <span class="status-badge ${stateCls}" style="font-size:10px;padding:1px 6px;">${stateTxt}</span>
            </div>
          </div>
          <div class="ci-meta">
            <span>${esc(ip)}</span>
            <span>${esc(mem)}</span>
          </div>
        </div>
      `;
    }).join('');

    el.querySelectorAll('.container-item').forEach((item) => {
      item.addEventListener('click', () => selectContainer(item.dataset.machine));
    });
  }

  function selectContainer(machine) {
    if (state.selectedMachine !== machine) {
      disconnectLogs();
      $('logViewer').textContent = '点击“实时追踪”查看 journalctl 日志...';
    }
    state.selectedMachine = machine;
    renderContainerList();
    renderDetail();
    syncTerminalView(machine);
    loadSnapshots(machine);
    loadImages(machine);
  }

  function getSelected() {
    return state.containers.find((c) => c.config.machine === state.selectedMachine) || null;
  }

  function renderDetail(opts = {}) {
    const item = getSelected();
    if (!item) {
      updateEmptyState(state.containers.length === 0 ? 'empty' : 'unselected');
      $('emptyState').hidden = false;
      $('detailView').hidden = true;
      return;
    }
    $('emptyState').hidden = true;
    $('detailView').hidden = false;

    const { config, runtime } = item;
    const running = runtime.active_state === 'active';
    const isStandard = config.layout === 'standard';
    const isExternal = config.layout === 'external';
    const inStdRoot = !!config.in_standard_root;
    const live = state.liveStats[config.machine] || {};

    $('detailDistroIcon').innerHTML = renderDistroIcon(config.distro, runtime.os_pretty_name);
    $('detailName').textContent = config.machine;
    const badge = $('detailStateBadge');
    badge.textContent = running ? 'RUNNING' : (runtime.active_state === 'failed' ? 'FAILED' : 'STOPPED');
    badge.className = `status-badge ${running ? 'running' : (runtime.active_state === 'failed' ? 'failed' : 'stopped')}`;

    $('detailOS').textContent = runtime.os_pretty_name || (config.distro ? `${config.distro}/${config.suite}` : 'Linux 系统容器');
    if (isExternal) {
      $('detailLayoutPill').textContent = '外部运行容器';
      $('detailLayoutPill').className = 'pill pill-warn';
    } else if (isStandard && inStdRoot) {
      $('detailLayoutPill').textContent = '标准目录';
      $('detailLayoutPill').className = 'pill pill-ok';
    } else if (isStandard) {
      $('detailLayoutPill').textContent = '非标准路径';
      $('detailLayoutPill').className = 'pill pill-warn';
    } else {
      $('detailLayoutPill').textContent = '旧版目录';
      $('detailLayoutPill').className = 'pill pill-warn';
    }
    $('detailLinkWarn').hidden = isExternal || config.link_healthy;
    $('detailMeta').textContent = `${config.service_name} · ${runtime.active_state}/${runtime.sub_state} · ${config.container_dir || config.rootfs || '-'}`;

    const isServiceUnit = (config.service_name || '').endsWith('.service');
    const canManageUnit = isExternal ? isServiceUnit : (config.link_healthy || !!config.service_path);
    const canStopUnit = running && (isServiceUnit || isExternal);
    $('btnStart').disabled = running || !canManageUnit;
    $('btnStop').disabled = !canStopUnit;
    $('btnRestart').disabled = !canManageUnit;
    $('btnRestore').disabled = isExternal;
    $('toggleAutostart').checked = !!runtime.enabled;
    $('toggleAutostart').disabled = !canManageUnit;

    const canMigrate = !!config.can_migrate_to_standard;
    $('btnMigrateStandard').hidden = !canMigrate;
    $('migrateBanner').hidden = !canMigrate;
    if (canMigrate) {
      if (isExternal) {
        $('migrateBannerTitle').textContent = '发现外部运行中的系统容器，尚未纳入标准管理目录';
        $('migrateBannerDesc').textContent = `当前位置 ${config.rootfs || '-'}（${config.service_name}）。点击右侧可安全复刻为标准容器并移除原启动项，原容器数据将完整保留。`;
      } else if (!isStandard) {
        $('migrateBannerTitle').textContent = '当前容器为旧版目录结构';
        $('migrateBannerDesc').textContent = '可一键安全迁移至标准容器数据目录，统一生成 container.json、独立快照与导出目录，原目录数据完整保留。';
      } else {
        $('migrateBannerTitle').textContent = '当前容器位于补充扫描目录，不在设定的标准容器目录下';
        $('migrateBannerDesc').textContent = `当前根目录为 ${config.base_root || '-'}，可安全复刻迁移至标准目录 ${state.settings?.standard_base_root || '/vol1/nspawn'}，原目录数据完整保留。`;
      }
    }
    $('externalReadonlyBanner').hidden = !isExternal;
    $('btnSaveConfigOnly').disabled = isExternal;
    const cfgSubmitBtn = $('configForm').querySelector('button[type="submit"]');
    if (cfgSubmitBtn) cfgSubmitBtn.disabled = isExternal;
    $('btnAddCfgBind').disabled = isExternal;
    $('btnOpenDelete').disabled = isExternal;
    $('btnOpenNewSnap').disabled = isExternal;
    $('btnOpenExportImage').disabled = isExternal;

    // CPU & Memory probe metrics
    const cores = state.host?.cpu_cores || 1;
    const cpuSub = config.cpu_quota
      ? `限额 ${config.cpu_quota}`
      : `调度 0-${Math.max(0, cores - 1)}`;
    const memLimit = runtime.memory_limit_bytes || state.host?.total_memory_bytes || 0;
    const memPct = running && memLimit > 0 ? Math.min(100, (runtime.memory_current / memLimit) * 100) : (running ? 0 : null);
    const memSub = running
      ? (memLimit > 0 ? `${formatCompactBytes(runtime.memory_current)} / ${formatCompactBytes(memLimit)}` : formatCompactBytes(runtime.memory_current))
      : `上限 ${config.memory_max || '不限'}`;

    const uptimeText = running ? (formatUptime(runtime.uptime_sec) !== '-' ? formatUptime(runtime.uptime_sec) : '运行中') : '已停止';
    const pidText = running ? `PID ${runtime.leader_pid || runtime.main_pid || '-'}` : `Unit: ${runtime.unit_file_state || '-'}`;
    const btrfsHtml = runtime.rootfs_is_btrfs
      ? '<span class="pill pill-ok">Btrfs 子卷</span>'
      : (runtime.rootfs_exists ? '<span class="pill pill-warn">普通目录</span>' : '<span class="pill pill-err">Rootfs 缺失</span>');
    const autoSyncHtml = config.autostart
      ? '<span class="pill pill-ok">配置自启: 开</span>'
      : '<span class="pill">配置自启: 关</span>';

    $('overviewStatusKV').innerHTML = [
      probeRow('i-cpu', 'CPU', renderProgressBar(live.cpuPct, `${cpuSub} · 累计 ${formatCPU(runtime.cpu_usage_nsec)}`)),
      probeRow('i-mem', '内存', renderProgressBar(memPct, memSub)),
      probeRow('i-clock', '运行', `<span>${esc(uptimeText)}</span><span class="probe-sub">${esc(pidText)}</span>`),
      probeRow('i-shield', '存储', `${btrfsHtml}${autoSyncHtml}`),
    ].join('');

    // Network probe metrics
    const speedHtml = running
      ? `<span class="net-pair"><span><i class="ar d">↓</i> ${esc(formatRate(live.rxRate))}</span><span><i class="ar u">↑</i> ${esc(formatRate(live.txRate))}</span></span>`
      : '<span class="probe-sub">-</span>';
    const trafficHtml = (runtime.net_rx_bytes > 0 || runtime.net_tx_bytes > 0 || running)
      ? `<span class="net-pair"><span><i class="ar d">↓</i> ${esc(formatCompactBytes(runtime.net_rx_bytes))}</span><span><i class="ar u">↑</i> ${esc(formatCompactBytes(runtime.net_tx_bytes))}</span></span>`
      : '<span class="probe-sub">暂无统计</span>';

    const v4List = runtime.ipv4_addrs || [];
    const v6List = runtime.ipv6_addrs || [];
    const addrItems = [
      ...v4List.map((ip) => `<span class="ip-copy" title="点击复制 IPv4" onclick="window.__copyText('${esc(ip)}')">${esc(ip)}</span>`),
      ...v6List.map((ip) => `<span class="ip-copy ip6" title="点击复制 IPv6" onclick="window.__copyText('${esc(ip)}')">${esc(ip)}</span>`),
    ];
    const addrHtml = addrItems.length
      ? `<div class="ip-stack">${addrItems.join('')}</div>`
      : '<span class="probe-sub">无 IP（待启动或未获取到 DHCP）</span>';

    const ovsHtml = runtime.veth_attached
      ? `<span class="pill pill-ok">${esc(runtime.ovs_bridge)}</span>`
      : `<span class="pill pill-warn">${esc(runtime.ovs_bridge || '未接入')}</span>`;
    const nicDetail = `${config.physical_nic || '-'} / ${config.veth || '-'}${runtime.default_gateway ? ` · GW ${runtime.default_gateway}` : ''}`;
    const macHtml = runtime.host0_mac
      ? `<span class="ip-copy" title="点击复制 MAC" onclick="window.__copyText('${esc(runtime.host0_mac)}')">${esc(runtime.host0_mac)}</span>`
      : '';

    $('overviewNetworkKV').innerHTML = [
      probeRow('i-speed', '网速', speedHtml),
      probeRow('i-traffic', '流量', trafficHtml),
      probeRow('i-net', '地址', addrHtml),
      probeRow('i-folder', 'OVS', `${ovsHtml}<span class="probe-sub">${esc(nicDetail)}</span>${macHtml}`),
    ].join('');

    const bindsHtml = (config.bind_mounts || []).length
      ? config.bind_mounts.map((b) => `<div>${esc(b.host_path)} → ${esc(b.container_path)}${b.read_only ? ' (ro)' : ''}</div>`).join('')
      : '<span class="muted">无目录映射</span>';

    $('overviewPathsKV').innerHTML = kvRows([
      ['容器独立根目录', esc(config.container_dir || '-')],
      ['Rootfs 目录', esc(config.rootfs || '-')],
      ['配置目录 (config/)', esc(config.conf_dir || '-')],
      ['格式化配置 (container.json)', config.has_config_file
        ? `<span class="pill pill-ok">已持久化</span> <span>${esc(config.config_file)}</span>`
        : `<span class="pill pill-warn">尚未生成（保存配置或恢复时自动生成）</span>`],
      ['镜像与快照目录 (images/)', esc(config.images_dir || config.snapshot_dir || '-')],
      ['Systemd 单元', isExternal
        ? `${esc(config.service_name || '-')} <span class="pill">外部托管</span>`
        : (config.link_healthy ? esc(config.systemd_link) : '<span class="pill pill-warn">未同步（请点上方“修复”）</span>')],
      ['目录映射', `<div>${bindsHtml}</div>`],
    ]);

    if (!opts.skipConfigForm) {
      populateNICSelect($('cfgPhysicalNIC'), state.host?.nics, config.physical_nic, $('cfgNICWarn'));
      $('cfgTmpSize').value = config.tmp_size || '4G';
      $('cfgMemoryMax').value = config.memory_max || '';
      $('cfgCPUQuota').value = config.cpu_quota || '';
      $('cfgDescription').value = config.description || '';
      $('cfgAutostart').checked = !!config.autostart;
      renderBindList($('cfgBindList'), config.bind_mounts || []);
    }

    $('snapDirHint').textContent = `快照路径: ${config.snapshot_dir || '-'}`;
    $('imageDirHint').textContent = `镜像归档路径: ${config.exports_dir || '-'}`;
  }

  function kvRows(pairs) {
    return pairs.map(([k, v]) => `
      <div class="kv-row">
        <span class="kv-k">${esc(k)}</span>
        <div class="kv-v">${v}</div>
      </div>
    `).join('');
  }

  window.__copyText = async (txt) => {
    try {
      await navigator.clipboard.writeText(txt);
      toast(`已复制: ${txt}`);
    } catch {
      toast('复制失败，请手动复制', 'err');
    }
  };

  function renderBindList(container, binds) {
    container.innerHTML = '';
    (binds || []).forEach((b) => appendBindRow(container, b));
  }

  function appendBindRow(container, b = { host_path: '', container_path: '', read_only: false }) {
    const row = document.createElement('div');
    row.className = 'bind-row';
    row.innerHTML = `
      <input type="text" class="bind-host" placeholder="宿主机目录，如 /vol1/data" value="${esc(b.host_path)}" />
      <input type="text" class="bind-cont" placeholder="容器目录，如 /mnt/data" value="${esc(b.container_path)}" />
      <label style="display:flex;align-items:center;gap:4px;font-size:12px;">
        <input type="checkbox" class="bind-ro" ${b.read_only ? 'checked' : ''} /> 只读
      </label>
      <button type="button" class="btn btn-sm btn-danger-outline">移除</button>
    `;
    row.querySelector('button').addEventListener('click', () => row.remove());
    container.appendChild(row);
  }

  function collectBindList(container) {
    const rows = Array.from(container.querySelectorAll('.bind-row'));
    return rows.map((r) => ({
      host_path: r.querySelector('.bind-host').value.trim(),
      container_path: r.querySelector('.bind-cont').value.trim(),
      read_only: r.querySelector('.bind-ro').checked,
    })).filter((b) => b.host_path || b.container_path);
  }

  $('btnAddCfgBind').addEventListener('click', () => appendBindRow($('cfgBindList')));
  $('btnAddNewBind').addEventListener('click', () => appendBindRow($('newBindList')));

  // View container.json
  $('btnViewSpec').addEventListener('click', async () => {
    const machine = state.selectedMachine;
    if (!machine) return;
    try {
      const res = await apiFetch(`/api/containers/${encodeURIComponent(machine)}/spec`);
      $('specFilePath').textContent = res.exists_on_disk
        ? `文件位置: ${res.config_file}`
        : `预生成预览（保存配置后将写入 ${res.config_file || 'config/container.json'}）`;
      $('specJsonViewer').textContent = res.raw_json || JSON.stringify(res.spec, null, 2);
      openModal('specModal');
    } catch (err) {
      toast(err.message, 'err');
    }
  });

  $('btnCopySpec').addEventListener('click', () => {
    window.__copyText($('specJsonViewer').textContent);
  });

  // Actions
  async function runServiceAction(action) {
    const machine = state.selectedMachine;
    if (!machine) return false;
    try {
      toast(`正在执行 ${action} ...`);
      await apiFetch(`/api/containers/${encodeURIComponent(machine)}/action`, {
        method: 'POST',
        body: JSON.stringify({ action }),
      });
      toast(`${machine} ${action} 成功`);
      await loadContainers();
      return true;
    } catch (err) {
      toast(err.message, 'err');
      return false;
    }
  }

  $('btnStart').addEventListener('click', () => runServiceAction('start'));
  $('btnStop').addEventListener('click', () => runServiceAction('stop'));
  $('btnRestart').addEventListener('click', () => runServiceAction('restart'));
  $('toggleAutostart').addEventListener('change', async (e) => {
    const nextEnabled = e.target.checked;
    const ok = await runServiceAction(nextEnabled ? 'enable' : 'disable');
    if (!ok) {
      e.target.checked = !nextEnabled;
    }
  });

  $('btnRestore').addEventListener('click', () => {
    const item = getSelected();
    if (!item || item.config.layout === 'external') return;
    $('repairMachine').textContent = item.config.machine;
    openModal('repairModal');
  });

  $('btnConfirmRepair').addEventListener('click', async () => {
    const machine = state.selectedMachine;
    if (!machine) return;
    try {
      closeModal('repairModal');
      const task = await apiFetch(`/api/containers/${encodeURIComponent(machine)}/restore`, { method: 'POST' });
      toast('已启动容器修复任务');
      openTaskMonitor(task.id);
    } catch (err) {
      toast(err.message, 'err');
    }
  });

  function updateMigratePreview() {
    const base = $('migTargetBaseRoot').value.trim() || state.settings?.standard_base_root || '/vol1/nspawn';
    const machine = $('migTargetMachine').value.trim() || '<容器名>';
    $('migVethPreview').textContent = machine ? `(veth: ve-${machine}, ${machine.length + 3}/15 字符)` : '(veth: ve-...)';
    const cdir = `${base.replace(/\/+$/, '')}/${machine}`;
    $('migDirPreview').textContent =
`目标标准容器目录预览：
${cdir}/
├── rootfs/              (无损克隆的根文件系统)
├── config/              (container.json + nspawn-${machine}.service + network-*.sh + restore.sh)
└── images/              (snapshots/ + exports/)`;
  }

  function openMigrateModal() {
    const item = getSelected();
    if (!item || !item.config.can_migrate_to_standard) return;
    const { config, runtime } = item;
    const stdRoot = state.settings?.standard_base_root || '/vol1/nspawn';
    $('migSourceKV').innerHTML = kvRows([
      ['源容器名称', `<strong>${esc(config.machine)}</strong>`],
      ['当前类型 / Unit', `${esc(config.layout)} · ${esc(config.service_name || '-')}`],
      ['源 Rootfs 路径', esc(config.rootfs || '-')],
      ['当前状态', `${esc(runtime.active_state)} / ${runtime.enabled ? '已开启自启' : '未开启自启'}`],
    ]);
    $('migTargetBaseRoot').value = stdRoot;
    $('migTargetMachine').value = config.machine;
    $('migAutostart').checked = !!(config.autostart || runtime.enabled);
    $('migStartNow').checked = true;
    const prefNIC = config.physical_nic || state.settings?.default_physical_nic;
    populateNICSelect($('migPhysicalNIC'), state.host?.nics, prefNIC, $('migNICWarn'));
    updateMigratePreview();
    openModal('migrateModal');
  }

  $('btnMigrateStandard').addEventListener('click', openMigrateModal);
  $('btnBannerMigrate').addEventListener('click', openMigrateModal);
  $('migTargetBaseRoot').addEventListener('input', updateMigratePreview);
  $('migTargetMachine').addEventListener('input', updateMigratePreview);
  $('btnBrowseMigRoot').addEventListener('click', () => {
    openFSBrowser({
      title: '选择目标标准容器根目录',
      mode: 'dir',
      initialPath: $('migTargetBaseRoot').value.trim(),
      onSelect: (p) => {
        $('migTargetBaseRoot').value = p;
        updateMigratePreview();
      },
    });
  });

  $('migrateForm').addEventListener('submit', async (e) => {
    e.preventDefault();
    const machine = state.selectedMachine;
    if (!machine) return;
    try {
      const task = await apiFetch(`/api/containers/${encodeURIComponent(machine)}/migrate`, {
        method: 'POST',
        body: JSON.stringify({
          target_base_root: $('migTargetBaseRoot').value.trim(),
          target_machine: $('migTargetMachine').value.trim(),
          physical_nic: $('migPhysicalNIC').value,
          autostart: $('migAutostart').checked,
          start_now: $('migStartNow').checked,
        }),
      });
      closeModal('migrateModal');
      toast('已启动安全迁移任务');
      openTaskMonitor(task.id);
    } catch (err) {
      toast(err.message, 'err');
    }
  });

  async function saveContainerConfig(restartNow) {
    const machine = state.selectedMachine;
    if (!machine) return;
    const payload = {
      description: $('cfgDescription').value.trim(),
      physical_nic: $('cfgPhysicalNIC').value,
      tmp_size: $('cfgTmpSize').value.trim(),
      memory_max: $('cfgMemoryMax').value.trim(),
      cpu_quota: $('cfgCPUQuota').value.trim(),
      autostart: $('cfgAutostart').checked,
      bind_mounts: collectBindList($('cfgBindList')),
      restart_now: restartNow,
    };
    try {
      await apiFetch(`/api/containers/${encodeURIComponent(machine)}/config`, {
        method: 'PUT',
        body: JSON.stringify(payload),
      });
      toast(restartNow ? '配置已持久化并重启容器' : '配置已校验并持久化保存');
      await loadContainers();
    } catch (err) {
      toast(err.message, 'err');
    }
  }

  $('btnSaveConfigOnly').addEventListener('click', () => saveContainerConfig(false));
  $('configForm').addEventListener('submit', (e) => {
    e.preventDefault();
    saveContainerConfig(true);
  });

  // Snapshots
  async function loadSnapshots(machine) {
    try {
      const snaps = await apiFetch(`/api/containers/${encodeURIComponent(machine)}/snapshots`);
      state.snapshots = snaps || [];
      renderSnapshots();
    } catch (err) {
      $('snapTableBody').innerHTML = `<tr><td colspan="5" class="muted">加载失败: ${esc(err.message)}</td></tr>`;
    }
  }

  function renderSnapshots() {
    const tbody = $('snapTableBody');
    if (!state.snapshots.length) {
      tbody.innerHTML = '<tr><td colspan="5" class="muted">暂无快照</td></tr>';
      return;
    }
    tbody.innerHTML = state.snapshots.map((s) => `
      <tr>
        <td><code>${esc(s.name)}</code></td>
        <td>${s.read_only ? '<span class="pill pill-ok">只读 (ro)</span>' : '<span class="pill">可写 (rw)</span>'}</td>
        <td class="muted">${esc(s.created_at || '-')}</td>
        <td class="muted"><code>${esc((s.uuid || '').slice(0, 13))}</code></td>
        <td style="text-align:right">
          <button class="btn btn-sm btn-warning" data-rb="${esc(s.name)}">回滚</button>
          <button class="btn btn-sm btn-danger-outline" data-del-snap="${esc(s.name)}">删除</button>
        </td>
      </tr>
    `).join('');

    tbody.querySelectorAll('[data-rb]').forEach((b) => {
      b.addEventListener('click', () => {
        state.rollbackSnap = b.dataset.rb;
        $('rbMachine').textContent = state.selectedMachine;
        $('rbSnapName').textContent = state.rollbackSnap;
        openModal('rollbackModal');
      });
    });

    tbody.querySelectorAll('[data-del-snap]').forEach((b) => {
      b.addEventListener('click', async () => {
        const snapName = b.dataset.delSnap;
        if (!confirm(`确定删除快照 ${snapName}？`)) return;
        try {
          await apiFetch(`/api/containers/${encodeURIComponent(state.selectedMachine)}/snapshots/${encodeURIComponent(snapName)}`, {
            method: 'DELETE',
          });
          toast('快照已删除');
          loadSnapshots(state.selectedMachine);
        } catch (err) {
          toast(err.message, 'err');
        }
      });
    });
  }

  $('btnRefreshSnaps').addEventListener('click', () => state.selectedMachine && loadSnapshots(state.selectedMachine));
  $('btnOpenNewSnap').addEventListener('click', () => openModal('snapModal'));

  $('snapForm').addEventListener('submit', async (e) => {
    e.preventDefault();
    const machine = state.selectedMachine;
    if (!machine) return;
    try {
      await apiFetch(`/api/containers/${encodeURIComponent(machine)}/snapshots`, {
        method: 'POST',
        body: JSON.stringify({
          tag: $('snapTag').value.trim(),
          read_only: $('snapReadOnly').checked,
          stop_first: $('snapStopFirst').checked,
        }),
      });
      closeModal('snapModal');
      toast('Btrfs 快照已创建');
      loadSnapshots(machine);
    } catch (err) {
      toast(err.message, 'err');
    }
  });

  $('btnConfirmRollback').addEventListener('click', async () => {
    const machine = state.selectedMachine;
    if (!machine || !state.rollbackSnap) return;
    try {
      const task = await apiFetch(`/api/containers/${encodeURIComponent(machine)}/rollback`, {
        method: 'POST',
        body: JSON.stringify({
          snapshot_name: state.rollbackSnap,
          safety_backup: $('rbSafetyBackup').checked,
        }),
      });
      closeModal('rollbackModal');
      toast('回滚任务已启动');
      openTaskMonitor(task.id);
    } catch (err) {
      toast(err.message, 'err');
    }
  });

  // Images (images/exports)
  async function loadImages(machine) {
    try {
      const imgs = await apiFetch(`/api/containers/${encodeURIComponent(machine)}/images`);
      state.images = imgs || [];
      renderImages();
    } catch (err) {
      $('imageTableBody').innerHTML = `<tr><td colspan="5" class="muted">加载失败: ${esc(err.message)}</td></tr>`;
    }
  }

  function renderImages() {
    const tbody = $('imageTableBody');
    if (!state.images.length) {
      tbody.innerHTML = '<tr><td colspan="5" class="muted">暂无镜像归档，可点击右上角“+ 导出 Rootfs 镜像”</td></tr>';
      return;
    }
    tbody.innerHTML = state.images.map((img) => `
      <tr>
        <td><code>${esc(img.name)}</code></td>
        <td><span class="pill">${esc(img.format)}</span></td>
        <td class="muted">${formatBytes(img.size_bytes)}</td>
        <td class="muted">${esc(img.created_at || '-')}</td>
        <td style="text-align:right">
          <button class="btn btn-sm btn-ghost" data-use-img="${esc(img.path)}">以此新建容器</button>
          <button class="btn btn-sm btn-danger-outline" data-del-img="${esc(img.name)}">删除</button>
        </td>
      </tr>
    `).join('');

    tbody.querySelectorAll('[data-use-img]').forEach((b) => {
      b.addEventListener('click', () => {
        openCreateModalWithArchive(b.dataset.useImg);
      });
    });

    tbody.querySelectorAll('[data-del-img]').forEach((b) => {
      b.addEventListener('click', async () => {
        const fname = b.dataset.delImg;
        if (!confirm(`确定删除镜像归档 ${fname}？`)) return;
        try {
          await apiFetch(`/api/containers/${encodeURIComponent(state.selectedMachine)}/images/${encodeURIComponent(fname)}`, {
            method: 'DELETE',
          });
          toast('镜像归档已删除');
          loadImages(state.selectedMachine);
        } catch (err) {
          toast(err.message, 'err');
        }
      });
    });
  }

  $('btnRefreshImages').addEventListener('click', () => state.selectedMachine && loadImages(state.selectedMachine));
  $('btnOpenExportImage').addEventListener('click', () => {
    const sel = $('expImgSource');
    sel.innerHTML = '<option value="">当前 rootfs（Btrfs 自动瞬时快照，不停机）</option>';
    (state.snapshots || []).forEach((s) => {
      sel.insertAdjacentHTML('beforeend', `<option value="${esc(s.name)}">快照: ${esc(s.name)}</option>`);
    });
    const zstdOpt = $('expImgFormat').querySelector('option[value="tar.zst"]');
    if (zstdOpt && state.host) {
      zstdOpt.disabled = !state.host.has_zstd;
      zstdOpt.textContent = state.host.has_zstd ? '.tar.zst (zstd 高速压缩)' : '.tar.zst (宿主机未安装 zstd)';
    }
    openModal('exportImageModal');
  });

  $('exportImageForm').addEventListener('submit', async (e) => {
    e.preventDefault();
    const machine = state.selectedMachine;
    if (!machine) return;
    try {
      const task = await apiFetch(`/api/containers/${encodeURIComponent(machine)}/images/export`, {
        method: 'POST',
        body: JSON.stringify({
          tag: $('expImgTag').value.trim(),
          format: $('expImgFormat').value,
          source_snapshot: $('expImgSource').value,
          stop_first: $('expImgStopFirst').checked,
        }),
      });
      closeModal('exportImageModal');
      toast('镜像导出任务已启动');
      openTaskMonitor(task.id);
    } catch (err) {
      toast(err.message, 'err');
    }
  });

  // Delete container
  $('btnOpenDelete').addEventListener('click', () => {
    if (!state.selectedMachine) return;
    $('delMachine').textContent = state.selectedMachine;
    $('delMachineHint').textContent = state.selectedMachine;
    $('delRootfs').checked = false;
    $('delSnapshots').checked = false;
    $('delConfirmInput').value = '';
    $('delMismatchHint').hidden = true;
    $('btnConfirmDelete').disabled = true;
    openModal('deleteModal');
  });

  $('delConfirmInput').addEventListener('input', () => {
    const val = $('delConfirmInput').value.trim();
    const target = state.selectedMachine || '';
    const matched = val !== '' && val === target;
    $('btnConfirmDelete').disabled = !matched;
    $('delMismatchHint').hidden = val === '' || matched;
  });

  $('btnConfirmDelete').addEventListener('click', async () => {
    const machine = state.selectedMachine;
    if (!machine) return;
    if ($('delConfirmInput').value.trim() !== machine) {
      $('delMismatchHint').hidden = false;
      $('btnConfirmDelete').disabled = true;
      toast(`请输入正确的容器名 "${machine}" 以确认删除`, 'err');
      return;
    }
    try {
      const q = `delete_rootfs=${$('delRootfs').checked}&delete_snapshots=${$('delSnapshots').checked}`;
      await apiFetch(`/api/containers/${encodeURIComponent(machine)}?${q}`, { method: 'DELETE' });
      disposeTerminal(machine);
      closeModal('deleteModal');
      toast(`已移除容器 ${machine}`);
      state.selectedMachine = null;
      await loadContainers();
    } catch (err) {
      toast(err.message, 'err');
    }
  });

  // Create container
  function updateCreateDirPreview() {
    const base = $('newBaseRoot').value.trim() || state.settings?.standard_base_root || '/vol1/nspawn';
    const machine = $('newMachine').value.trim() || '<容器名>';
    const cdir = `${base.replace(/\/+$/, '')}/${machine}`;
    $('newDirPreview').textContent =
`标准化容器目录预览：
${cdir}/
├── rootfs/              (Btrfs 子卷)
├── config/              (container.json + nspawn-${machine}.service + network-*.sh + restore.sh)
└── images/              (snapshots/ + exports/)`;
  }

  function toggleCreateSourceFields() {
    const isArchive = $('newSourceType').value === 'archive';
    $('newArchiveGroup').hidden = !isArchive;
    $('newDebootstrapGroup').hidden = isArchive;
    $('newExtrasWrap').hidden = isArchive;
    $('newPassword').required = !isArchive;
    $('newPasswordLabel').textContent = isArchive ? '重置用户密码（留空沿用镜像原密码）' : '用户密码 *';
  }

  $('newSourceType').addEventListener('change', toggleCreateSourceFields);
  $('newBaseRoot').addEventListener('input', updateCreateDirPreview);
  $('newMachine').addEventListener('input', (e) => {
    const v = e.target.value.trim();
    $('vethPreview').textContent = v ? `(veth: ve-${v}, ${v.length + 3}/15 字符)` : '(veth: ve-...)';
    updateCreateDirPreview();
  });

  $('newDistro').addEventListener('change', (e) => {
    if (e.target.value === 'ubuntu') {
      $('newSuite').value = 'noble';
      $('newMirror').value = 'https://mirrors.ustc.edu.cn/ubuntu';
      $('newSecMirror').value = 'https://mirrors.ustc.edu.cn/ubuntu';
    } else {
      $('newSuite').value = 'trixie';
      $('newMirror').value = 'https://mirrors.ustc.edu.cn/debian';
      $('newSecMirror').value = 'https://mirrors.ustc.edu.cn/debian-security';
    }
  });

  function prepareCreateModalDefaults() {
    const cfg = state.settings;
    if (cfg) {
      $('newBaseRoot').value = cfg.standard_base_root || (cfg.base_roots && cfg.base_roots[0]) || '/vol1/nspawn';
      $('newSuite').value = cfg.default_suite || 'trixie';
      $('newMirror').value = cfg.default_mirror || 'https://mirrors.ustc.edu.cn/debian';
      $('newSecMirror').value = cfg.default_security_mirror || 'https://mirrors.ustc.edu.cn/debian-security';
      $('newTmpSize').value = cfg.default_tmp_size || '4G';
      populateNICSelect($('newPhysicalNIC'), state.host?.nics, cfg.default_physical_nic, $('newNICWarn'));
    }
    toggleCreateSourceFields();
    updateCreateDirPreview();
  }

  function openCreateModalDefault() {
    $('newSourceType').value = 'debootstrap';
    $('newSourceArchive').value = '';
    prepareCreateModalDefaults();
    openModal('createModal');
  }

  function openCreateModalWithArchive(archivePath) {
    $('newSourceType').value = 'archive';
    $('newSourceArchive').value = archivePath;
    prepareCreateModalDefaults();
    openModal('createModal');
  }

  function closeCreateDropdown() {
    const dd = $('createDropdown');
    const btn = $('btnAddContainerMenu');
    if (dd) dd.hidden = true;
    if (btn) btn.setAttribute('aria-expanded', 'false');
  }

  $('btnAddContainerMenu').addEventListener('click', (e) => {
    e.stopPropagation();
    const dd = $('createDropdown');
    const nextHidden = !dd.hidden;
    dd.hidden = nextHidden;
    $('btnAddContainerMenu').setAttribute('aria-expanded', String(!nextHidden));
  });

  document.addEventListener('click', (e) => {
    if (!e.target.closest('#createMenuWrap')) {
      closeCreateDropdown();
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      closeCreateDropdown();
    }
  });

  $('btnCreateModal').addEventListener('click', () => {
    closeCreateDropdown();
    openCreateModalDefault();
  });
  $('btnEmptyCreate').addEventListener('click', openCreateModalDefault);

  $('btnBrowseBaseRoot').addEventListener('click', () => {
    openFSBrowser({
      title: '选择容器存储根目录',
      mode: 'dir',
      initialPath: $('newBaseRoot').value.trim(),
      onSelect: (p) => {
        $('newBaseRoot').value = p;
        updateCreateDirPreview();
      },
    });
  });

  $('btnBrowseArchive').addEventListener('click', () => {
    openFSBrowser({
      title: '选择 Rootfs 镜像归档 (.tar.gz / .tar.zst)',
      mode: 'archive',
      initialPath: $('newSourceArchive').value.trim() || $('newBaseRoot').value.trim(),
      onSelect: (p) => {
        $('newSourceArchive').value = p;
      },
    });
  });

  $('createForm').addEventListener('submit', async (e) => {
    e.preventDefault();
    const payload = {
      source_type: $('newSourceType').value,
      source_archive: $('newSourceArchive').value.trim(),
      machine: $('newMachine').value.trim(),
      container_user: $('newUser').value.trim(),
      password: $('newPassword').value,
      base_root: $('newBaseRoot').value.trim(),
      distro: $('newDistro').value,
      suite: $('newSuite').value.trim(),
      physical_nic: $('newPhysicalNIC').value,
      tmp_size: $('newTmpSize').value.trim(),
      memory_max: $('newMemoryMax').value.trim(),
      cpu_quota: $('newCPUQuota').value.trim(),
      mirror: $('newMirror').value.trim(),
      security_mirror: $('newSecMirror').value.trim(),
      bind_mounts: collectBindList($('newBindList')),
      install_extras: $('newInstallExtras').checked,
      autostart: $('newAutostartBoot').checked,
      auto_start: $('newAutoStart').checked,
    };
    try {
      const task = await apiFetch('/api/containers', {
        method: 'POST',
        body: JSON.stringify(payload),
      });
      closeModal('createModal');
      $('newPassword').value = '';
      toast('创建任务已提交');
      openTaskMonitor(task.id);
    } catch (err) {
      toast(err.message, 'err');
    }
  });

  // Import / Restore from config
  function openImportModal() {
    state.importPreview = null;
    $('importPreviewBox').hidden = true;
    $('btnConfirmImport').disabled = true;
    if (!$('impConfigPath').value.trim()) {
      $('impConfigPath').value = state.settings?.standard_base_root || state.settings?.base_roots?.[0] || '/vol1/nspawn';
    }
    openModal('importModal');
  }

  $('btnImportModal').addEventListener('click', () => {
    closeCreateDropdown();
    openImportModal();
  });
  $('btnEmptyImport').addEventListener('click', openImportModal);

  $('btnBrowseImport').addEventListener('click', () => {
    openFSBrowser({
      title: '选择容器目录或 config/container.json',
      mode: 'config',
      initialPath: $('impConfigPath').value.trim(),
      onSelect: async (p) => {
        $('impConfigPath').value = p;
        await previewImport();
      },
    });
  });

  async function previewImport() {
    const path = $('impConfigPath').value.trim();
    if (!path) {
      toast('请先输入或选择配置文件路径', 'err');
      return;
    }
    try {
      const res = await apiFetch('/api/containers/import/preview', {
        method: 'POST',
        body: JSON.stringify({ config_path: path }),
      });
      state.importPreview = res;
      const sp = res.spec;
      $('importPreviewKV').innerHTML = kvRows([
        ['容器名称', `<strong>${esc(sp.machine)}</strong> ${res.already_registered ? '<span class="pill pill-warn">已存在（将更新并重连）</span>' : '<span class="pill pill-ok">可导入恢复</span>'}`],
        ['配置解析来源', `${esc(res.config_type)} · ${esc(res.resolved_config_path)}`],
        ['容器目录', esc(res.container_dir)],
        ['Rootfs 状态', res.rootfs_is_btrfs
          ? `<span class="pill pill-ok">有效 Btrfs 子卷</span> (${esc(res.rootfs_path)})`
          : (res.rootfs_exists ? `<span class="pill pill-warn">普通目录</span> (${esc(res.rootfs_path)})` : `<span class="pill pill-err">缺失: ${esc(res.rootfs_path)}</span>`)],
        ['镜像来源信息', esc([sp.image?.source_type, sp.image?.distro, sp.image?.suite, sp.image?.arch].filter(Boolean).join(' / ') || '-')],
        ['配置开机自启', sp.system?.autostart ? '<span class="pill pill-ok">启用 (true)</span>' : '<span class="pill">禁用 (false)</span>'],
        ['Bind Mount 数量', `${(sp.bind_mounts || []).length} 条`],
      ]);
      populateNICSelect($('impPhysicalNIC'), state.host?.nics, sp.network?.physical_nic, $('impNICWarn'));
      $('importPreviewBox').hidden = false;
      $('btnConfirmImport').disabled = !res.rootfs_exists;
      if (!res.rootfs_exists) {
        toast('未找到 rootfs 目录，无法恢复', 'err');
      }
    } catch (err) {
      state.importPreview = null;
      $('importPreviewBox').hidden = true;
      $('btnConfirmImport').disabled = true;
      toast(err.message, 'err');
    }
  }

  $('btnPreviewImport').addEventListener('click', previewImport);

  $('importForm').addEventListener('submit', async (e) => {
    e.preventDefault();
    try {
      const task = await apiFetch('/api/containers/import', {
        method: 'POST',
        body: JSON.stringify({
          config_path: $('impConfigPath').value.trim(),
          physical_nic: $('impPhysicalNIC').value,
          apply_autostart: $('impApplyAutostart').checked,
          start_now: $('impStartNow').checked,
        }),
      });
      closeModal('importModal');
      toast('容器配置恢复任务已启动');
      openTaskMonitor(task.id);
    } catch (err) {
      toast(err.message, 'err');
    }
  });

  // Reusable Host FS Browser
  async function loadFSPath(path) {
    try {
      const q = path ? `?path=${encodeURIComponent(path)}` : '';
      const res = await apiFetch(`/api/fs/browse${q}`);
      state.fsBrowse.currentPath = res.path;
      state.fsBrowse.parent = res.parent || '';
      $('fsCurrentPath').value = res.path;

      $('fsBookmarks').innerHTML = (res.bookmarks || []).map((bm) =>
        `<button type="button" class="btn btn-sm btn-ghost" data-bm="${esc(bm)}">${esc(bm)}</button>`
      ).join('');
      $('fsBookmarks').querySelectorAll('[data-bm]').forEach((b) => {
        b.addEventListener('click', () => loadFSPath(b.dataset.bm));
      });

      const listEl = $('fsEntryList');
      const entries = res.entries || [];
      if (!entries.length) {
        listEl.innerHTML = '<div class="empty-hint">空目录（或无匹配文件）</div>';
        return;
      }
      listEl.innerHTML = entries.map((ent) => {
        const icon = ent.is_dir ? '📁' : '📄';
        const right = ent.is_dir
          ? (state.fsBrowse.mode === 'config' ? '<button type="button" class="btn btn-sm btn-ghost" data-pick-dir="' + esc(ent.path) + '">选此目录</button>' : '<span class="muted">进入</span>')
          : `<span class="muted">${formatBytes(ent.size)}</span>`;
        return `
          <div class="fs-item" data-path="${esc(ent.path)}" data-dir="${ent.is_dir ? '1' : '0'}">
            <div class="fs-item-name"><span>${icon}</span><span>${esc(ent.name)}</span></div>
            <div>${right}</div>
          </div>
        `;
      }).join('');

      listEl.querySelectorAll('[data-pick-dir]').forEach((btn) => {
        btn.addEventListener('click', (ev) => {
          ev.stopPropagation();
          const picked = btn.dataset.pickDir;
          closeModal('fsBrowseModal');
          state.fsBrowse.onSelect && state.fsBrowse.onSelect(picked);
        });
      });

      listEl.querySelectorAll('.fs-item').forEach((row) => {
        row.addEventListener('click', () => {
          const p = row.dataset.path;
          const isDir = row.dataset.dir === '1';
          if (isDir) {
            loadFSPath(p);
          } else if (state.fsBrowse.mode !== 'dir') {
            closeModal('fsBrowseModal');
            state.fsBrowse.onSelect && state.fsBrowse.onSelect(p);
          }
        });
      });
    } catch (err) {
      toast(err.message, 'err');
    }
  }

  function openFSBrowser({ title, mode, initialPath, onSelect }) {
    state.fsBrowse.mode = mode || 'any';
    state.fsBrowse.onSelect = onSelect;
    $('fsBrowseTitle').textContent = title || '选择路径';
    $('btnFsSelectDir').hidden = mode === 'archive';
    $('btnFsSelectDir').textContent = mode === 'config' ? '选择当前目录作为容器目录' : '选择当前目录';
    openModal('fsBrowseModal');
    loadFSPath(initialPath || '');
  }

  $('btnFsUp').addEventListener('click', () => {
    if (state.fsBrowse.parent) loadFSPath(state.fsBrowse.parent);
  });
  $('btnFsGo').addEventListener('click', () => loadFSPath($('fsCurrentPath').value.trim()));
  $('btnFsSelectDir').addEventListener('click', () => {
    const p = state.fsBrowse.currentPath;
    closeModal('fsBrowseModal');
    state.fsBrowse.onSelect && state.fsBrowse.onSelect(p);
  });

  // Settings
  $('btnSettings').addEventListener('click', async () => {
    const cfg = await apiFetch('/api/settings');
    state.settings = cfg;
    $('setStandardBaseRoot').value = cfg.standard_base_root || '/vol1/nspawn';
    $('setBaseRoots').value = (cfg.base_roots || []).join('\n');
    populateNICSelect($('setDefaultNIC'), state.host?.nics, cfg.default_physical_nic || '', $('setDefaultNICWarn'));
    $('setDefaultSuite').value = cfg.default_suite || 'trixie';
    $('setDefaultMirror').value = cfg.default_mirror || '';
    $('setDefaultSecMirror').value = cfg.default_security_mirror || '';
    $('setTheme').value = window.getThemePref ? window.getThemePref() : 'auto';
    openModal('settingsModal');
  });

  $('btnBrowseStdRoot').addEventListener('click', () => {
    openFSBrowser({
      title: '选择容器标准数据目录',
      mode: 'dir',
      initialPath: $('setStandardBaseRoot').value.trim(),
      onSelect: (p) => {
        $('setStandardBaseRoot').value = p;
      },
    });
  });

  $('btnAddScanRoot').addEventListener('click', () => {
    openFSBrowser({
      title: '选择并添加补充扫描目录',
      mode: 'dir',
      initialPath: $('setStandardBaseRoot').value.trim() || '/vol1',
      onSelect: (p) => {
        const list = $('setBaseRoots').value.split('\n').map((s) => s.trim()).filter(Boolean);
        if (!list.includes(p)) {
          list.push(p);
          $('setBaseRoots').value = list.join('\n');
        }
      },
    });
  });

  $('settingsForm').addEventListener('submit', async (e) => {
    e.preventDefault();
    const stdRoot = $('setStandardBaseRoot').value.trim();
    const roots = $('setBaseRoots').value.split('\n').map((s) => s.trim()).filter(Boolean);
    const next = {
      ...(state.settings || {}),
      standard_base_root: stdRoot,
      base_roots: roots,
      default_physical_nic: $('setDefaultNIC').value.trim(),
      default_suite: $('setDefaultSuite').value.trim(),
      default_mirror: $('setDefaultMirror').value.trim(),
      default_security_mirror: $('setDefaultSecMirror').value.trim(),
    };
    try {
      state.settings = await apiFetch('/api/settings', { method: 'PUT', body: JSON.stringify(next) });
      const themePref = $('setTheme').value || 'auto';
      localStorage.setItem('fn-nspawn-theme', themePref);
      window.applyTheme && window.applyTheme(themePref);
      updateThemeButton();
      closeModal('settingsModal');
      toast('设置已保存');
      await loadContainers();
    } catch (err) {
      toast(err.message, 'err');
    }
  });

  // Web Terminal (independent per-container persistent sessions)
  function getOrCreateTerminalSession(machine) {
    if (state.terminals[machine]) {
      return state.terminals[machine];
    }
    const pane = document.createElement('div');
    pane.className = 'term-pane';
    pane.hidden = state.selectedMachine !== machine;
    $('terminalContainer').appendChild(pane);

    const term = new window.Terminal({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: 'Menlo, Monaco, Consolas, "Courier New", monospace',
      theme: { background: '#0a0b0d', foreground: '#e2e8f0' },
    });
    const fitAddon = new window.FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open(pane);

    const session = {
      machine,
      el: pane,
      term,
      fitAddon,
      ws: null,
      user: state.termUsers[machine] || 'root',
      status: 'disconnected',
      statusText: '未连接',
    };

    term.onData((data) => {
      if (session.ws && session.ws.readyState === WebSocket.OPEN) {
        session.ws.send(data);
      }
    });

    state.terminals[machine] = session;
    return session;
  }

  function sendResize(session) {
    if (session && session.ws && session.ws.readyState === WebSocket.OPEN) {
      session.ws.send(JSON.stringify({
        type: 'resize',
        cols: session.term.cols,
        rows: session.term.rows,
      }));
    }
  }

  function syncTerminalView(machine) {
    Object.entries(state.terminals).forEach(([m, sess]) => {
      sess.el.hidden = m !== machine;
    });
    const session = machine ? state.terminals[machine] : null;
    const hintEl = $('termEmptyHint');
    if (hintEl) {
      hintEl.hidden = !!session;
      if (!session) {
        hintEl.textContent = machine
          ? `容器 ${machine} 尚未开启终端会话，点击上方“连接终端”建立独立连接`
          : '请先选择一个容器';
      }
    }
    $('termUser').value = (machine && state.termUsers[machine]) || session?.user || 'root';
    $('termStatus').textContent = session ? session.statusText : '未连接';
    $('btnTermDisconnect').disabled = !(session && session.ws && (session.ws.readyState === WebSocket.OPEN || session.ws.readyState === WebSocket.CONNECTING));

    if (state.activeTab === 'terminal' && session) {
      setTimeout(() => {
        if (state.selectedMachine === machine && state.activeTab === 'terminal') {
          session.fitAddon.fit();
          sendResize(session);
          session.term.focus();
        }
      }, 40);
    }
  }

  function connectTerminal(machine = state.selectedMachine) {
    if (!machine) return;
    const user = $('termUser').value.trim() || 'root';
    state.termUsers[machine] = user;

    disconnectTerminal(machine);
    const session = getOrCreateTerminalSession(machine);
    session.user = user;
    session.status = 'connecting';
    session.statusText = '连接中...';
    session.term.reset();

    if (state.selectedMachine === machine) {
      syncTerminalView(machine);
    }
    session.fitAddon.fit();

    const ws = new WebSocket(wsUrl(`/api/ws/terminal?machine=${encodeURIComponent(machine)}&user=${encodeURIComponent(user)}`));
    ws.binaryType = 'arraybuffer';
    session.ws = ws;

    ws.onopen = () => {
      if (session.ws !== ws) return;
      session.status = 'connected';
      session.statusText = `已连接 (${user}@${machine})`;
      if (state.selectedMachine === machine) {
        $('termStatus').textContent = session.statusText;
        $('btnTermDisconnect').disabled = false;
        session.fitAddon.fit();
        session.term.focus();
      }
      sendResize(session);
      renderContainerList();
    };
    ws.onmessage = (ev) => {
      if (session.ws !== ws) return;
      if (ev.data instanceof ArrayBuffer) {
        session.term.write(new Uint8Array(ev.data));
      } else {
        session.term.write(ev.data);
      }
    };
    ws.onclose = () => {
      if (session.ws !== ws) return;
      session.ws = null;
      session.status = 'disconnected';
      session.statusText = '已断开';
      if (state.selectedMachine === machine) {
        $('termStatus').textContent = session.statusText;
        $('btnTermDisconnect').disabled = true;
      }
      renderContainerList();
    };
    ws.onerror = () => {
      if (session.ws !== ws) return;
      session.status = 'error';
      session.statusText = '连接异常';
      if (state.selectedMachine === machine) {
        $('termStatus').textContent = session.statusText;
      }
      renderContainerList();
    };
  }

  function disconnectTerminal(machine = state.selectedMachine) {
    if (!machine) return;
    const session = state.terminals[machine];
    if (!session || !session.ws) return;
    const ws = session.ws;
    session.ws = null;
    session.status = 'disconnected';
    session.statusText = '已断开';
    ws.close();
    if (state.selectedMachine === machine) {
      $('termStatus').textContent = session.statusText;
      $('btnTermDisconnect').disabled = true;
    }
    renderContainerList();
  }

  function disposeTerminal(machine) {
    const session = state.terminals[machine];
    if (!session) return;
    if (session.ws) {
      const ws = session.ws;
      session.ws = null;
      ws.close();
    }
    try {
      session.term.dispose();
    } catch (_) {}
    session.el.remove();
    delete state.terminals[machine];
    delete state.termUsers[machine];
  }

  function fitActiveTerminal() {
    if (state.activeTab !== 'terminal' || !state.selectedMachine) return;
    const session = state.terminals[state.selectedMachine];
    if (session) {
      session.fitAddon.fit();
      sendResize(session);
    }
  }

  window.addEventListener('resize', fitActiveTerminal);

  if (typeof ResizeObserver !== 'undefined' && $('terminalContainer')) {
    let termRaf = 0;
    const termRO = new ResizeObserver(() => {
      if (termRaf) cancelAnimationFrame(termRaf);
      termRaf = requestAnimationFrame(fitActiveTerminal);
    });
    termRO.observe($('terminalContainer'));
  }

  $('termUser').addEventListener('input', () => {
    if (state.selectedMachine) {
      state.termUsers[state.selectedMachine] = $('termUser').value;
    }
  });

  $('btnTermConnect').addEventListener('click', () => connectTerminal(state.selectedMachine));
  $('btnTermDisconnect').addEventListener('click', () => disconnectTerminal(state.selectedMachine));
  $('btnTermClear').addEventListener('click', () => {
    const session = state.selectedMachine ? state.terminals[state.selectedMachine] : null;
    session && session.term.clear();
  });

  // Live Logs
  function connectLogs() {
    const machine = state.selectedMachine;
    if (!machine) return;
    disconnectLogs();
    const viewer = $('logViewer');
    viewer.textContent = '';
    const ws = new WebSocket(wsUrl(`/api/ws/logs?machine=${encodeURIComponent(machine)}`));
    $('logStatus').textContent = '实时追踪中...';
    ws.onmessage = (ev) => {
      viewer.textContent += (viewer.textContent ? '\n' : '') + ev.data;
      viewer.scrollTop = viewer.scrollHeight;
    };
    ws.onclose = () => { $('logStatus').textContent = '已暂停'; };
    state.logWS = ws;
  }

  function disconnectLogs() {
    if (state.logWS) {
      state.logWS.close();
      state.logWS = null;
    }
  }

  $('btnLogConnect').addEventListener('click', connectLogs);
  $('btnLogStop').addEventListener('click', disconnectLogs);
  $('btnLogClear').addEventListener('click', () => { $('logViewer').textContent = ''; });

  // Tasks
  async function loadTasks() {
    try {
      state.tasks = await apiFetch('/api/tasks');
      const runningCount = state.tasks.filter((t) => t.status === 'running').length;
      $('taskBadge').textContent = String(runningCount);
      $('taskBadge').hidden = runningCount === 0;
      renderTasks();
    } catch (_) {}
  }

  function renderTasks() {
    const listEl = $('taskList');
    if (!state.tasks.length) {
      listEl.innerHTML = '<div class="muted">暂无后台任务</div>';
      return;
    }
    listEl.innerHTML = state.tasks.map((t) => {
      const active = t.id === state.selectedTaskId;
      const cls = t.status === 'success' ? 'pill-ok' : (t.status === 'failed' ? 'pill-err' : 'pill-warn');
      return `
        <div class="task-item ${active ? 'active' : ''}" data-tid="${esc(t.id)}">
          <div class="ci-top">
            <strong>${esc(t.type)} · ${esc(t.target)}</strong>
            <span class="pill ${cls}">${esc(t.status)}</span>
          </div>
          <div class="muted" style="margin-top:4px;">${new Date(t.started_at).toLocaleTimeString()}</div>
        </div>
      `;
    }).join('');

    listEl.querySelectorAll('.task-item').forEach((el) => {
      el.addEventListener('click', () => {
        state.selectedTaskId = el.dataset.tid;
        renderTasks();
        showTaskLog();
      });
    });
    showTaskLog();
  }

  function showTaskLog() {
    const t = state.tasks.find((x) => x.id === state.selectedTaskId);
    const v = $('taskLogViewer');
    if (!t) return;
    v.textContent = (t.logs || []).join('\n') || '等待输出...';
    v.scrollTop = v.scrollHeight;
  }

  function openTaskMonitor(taskId) {
    state.selectedTaskId = taskId;
    openModal('tasksModal');
    pollTasks();
  }

  async function pollTasks() {
    await loadTasks();
    const cur = state.tasks.find((x) => x.id === state.selectedTaskId);
    if (cur && cur.status === 'running') {
      clearTimeout(state.taskTimer);
      state.taskTimer = setTimeout(pollTasks, 1200);
    } else if (cur && cur.status === 'success') {
      if (cur.target) state.selectedMachine = cur.target;
      await loadContainers();
      if (state.selectedMachine) {
        loadSnapshots(state.selectedMachine);
        loadImages(state.selectedMachine);
      }
    }
  }

  $('btnTasks').addEventListener('click', () => {
    openModal('tasksModal');
    pollTasks();
  });

  $('btnRefresh').addEventListener('click', async () => {
    await loadInitial();
    toast('已刷新状态');
  });

  loadInitial();
})();
