(() => {
  'use strict';

  const $ = (selector) => document.querySelector(selector);
  const themeButton = $('.theme-toggle');
  function setTheme(dark, persist = true) {
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
    if (themeButton) {
      themeButton.setAttribute('aria-pressed', String(dark));
      themeButton.setAttribute('aria-label', `Switch to ${dark ? 'light' : 'dark'} theme`);
      themeButton.querySelector('.theme-label').textContent = dark ? 'Dark' : 'Light';
    }
    if (persist) { try { localStorage.setItem('idp-theme', dark ? 'dark' : 'light'); } catch (_) { /* Theme still works for this page. */ } }
  }
  if (themeButton) {
    let savedTheme = '';
    try { savedTheme = localStorage.getItem('idp-theme') || ''; } catch (_) { /* Storage can be unavailable. */ }
    setTheme(savedTheme === 'dark', false);
    themeButton.addEventListener('click', () => setTheme(document.documentElement.dataset.theme !== 'dark'));
  }
  const appShell = $('.app');
  const sidebarToggle = $('#sidebar-toggle');
  if (appShell && sidebarToggle) {
    const applySidebar = (collapsed) => {
      appShell.classList.toggle('is-collapsed', collapsed);
      sidebarToggle.setAttribute('aria-expanded', String(!collapsed));
      sidebarToggle.setAttribute('aria-label', collapsed ? 'Show navigation' : 'Hide navigation');
    };
    let collapsed = false;
    try { collapsed = localStorage.getItem('idp-sidebar') === 'collapsed'; } catch (_) { /* Storage can be unavailable. */ }
    applySidebar(collapsed);
    sidebarToggle.addEventListener('click', () => {
      const next = !appShell.classList.contains('is-collapsed');
      applySidebar(next);
      try { localStorage.setItem('idp-sidebar', next ? 'collapsed' : 'open'); } catch (_) { /* Storage can be unavailable. */ }
    });
  }
  const state = { definitions: [], applications: [], clusters: [], clusterQuery: '', clusterStatus: '', selectedApplication: null, selectedCluster: null, selectedInstanceUID: '', detailTab: 'overview', deleting: false, deletionPending: new Set(), editingValues: false, valuesDraft: Object.create(null), savingValues: false, definitionQuery: '', definitionSort: 'name', definitionType: '', definitionView: 'grid', destinationMode: 'cluster', preview: null, previewController: null, previewSequence: 0, previewLoading: false, savingEnvironment: false, creating: false, teams: [], team: '', resourcesQuery: '', resourcesKind: '', resourcesStatus: '', resourcesExpanded: new Set(), resourcesLive: null, resourcesLiveKey: '', selectedResourceUID: '', resourcePanelTab: 'overview', activityRange: '24h', activityType: '', activitySource: '', activityQuery: '', applicationQuery: '', applicationCluster: '', applicationNamespace: '', applicationStatus: '', applicationSort: { key: 'name', direction: 'asc' }, cloudTargets: [], cloudDefinitions: [], cloudResources: [], selectedCloudResource: null, cloudDetailTab: 'overview', cloudCreating: false, cloudSaving: false, cloudDeleting: false, cloudResourcesError: '', cloudSearch: '', cloudStatusFilter: '', cloudTargetFilter: '', cloudDefinitionFilter: '', cloudSort: { key: 'name', direction: 'asc' }, cloudDefinitionsExpanded: false, cloudTargetsExpanded: false, cloudActivityExpanded: false };
  state.selectedDefinition = null;
  state.canCreateApps = false;
  state.canDeleteApps = false;
  state.canUpdateApps = false;
  state.canChangeClusterEnvironment = false;
  state.canCreateResources = false;
  state.canDeleteResources = false;
  state.cloudProvisioning = false;
  const form = $('#create-form');
  const notice = $('#notice') || $('#applications-notice') || $('#clusters-notice') || $('#cloud-notice');
  const safe = (value) => String(value ?? '—');
  const initials = (value) => {
    const parts = String(value ?? '').trim().split(/[\s._@-]+/).filter(Boolean);
    if (!parts.length) return '–';
    if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
    return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
  };
  let csrfToken = '';
  let sessionPromise = null;
  let redirectingToLogin = false;
  let signedOutLocally = false;

  class ApiAuthError extends Error {}

  function redirectToLogin() {
    if (redirectingToLogin || signedOutLocally) return;
    redirectingToLogin = true;
    window.location.assign('/login');
  }

  async function ensureSession() {
    if (csrfToken) return;
    if (!sessionPromise) {
      sessionPromise = (async () => {
        const response = await fetch('/api/session', { headers: { Accept: 'application/json' } });
        if (response.status === 401) {
          redirectToLogin();
          throw new ApiAuthError('Sign in is required.');
        }
        if (!response.ok) throw new Error(`Could not establish a dashboard session (${response.status}).`);
        const session = await response.json();
        if (typeof session?.csrfToken !== 'string' || !session.csrfToken) throw new Error('The server returned an invalid session.');
        csrfToken = session.csrfToken;
        state.canCreateApps = session.canCreateApps === true;
        state.canDeleteApps = session.canDeleteApps === true;
        state.canUpdateApps = session.canUpdateApps === true;
        state.canChangeClusterEnvironment = session.isAdmin === true;
        state.canCreateResources = session.canCreateResources === true;
        state.canDeleteResources = session.canDeleteResources === true;
        state.cloudProvisioning = session.cloudProvisioning === true;
        const cloudNav = $('#cloud-nav');
        if (cloudNav && state.cloudProvisioning) cloudNav.removeAttribute('hidden');
        if(form) updateSubmit()
        if (state.selectedApplication) renderApplicationDetail();
        if (state.selectedCluster) renderClusterDetail();
        const adminNav = $('#admin-nav');
        if (adminNav && session.canAdminister === true) adminNav.removeAttribute('hidden');
        const userChip = $('#user-chip');
        if (userChip && typeof session.username === 'string' && session.username) {
          const name = $('#user-name');
          const avatar = $('#user-avatar');
          if (name) name.textContent = session.username;
          if (avatar) avatar.textContent = initials(session.username);
          userChip.removeAttribute('hidden');
        }
        state.teams = Array.isArray(session.teams) ? session.teams : [];
        const teamWrap = $('#team-select-wrap');
        const teamSelect = $('#team-select');
        if (teamWrap && teamSelect && state.teams.length) {
          teamSelect.replaceChildren(...state.teams.map((team) => {
            const option = document.createElement('option');
            option.value = team.namespace;
            option.textContent = team.name || team.namespace;
            return option;
          }));
          state.team = state.teams[0].namespace;
          teamSelect.value = state.team;
          teamSelect.addEventListener('change', () => { state.team = teamSelect.value; refreshForTeam(); });
          teamWrap.removeAttribute('hidden');
        }
      })();
    }
    await sessionPromise;
  }

  async function apiFetch(path, options = {}) {
    if (signedOutLocally) throw new ApiAuthError('You are signed out of this dashboard.');
    await ensureSession();
    const method = (options.method || 'GET').toUpperCase();
    const headers = new Headers(options.headers || {});
    if (!headers.has('Accept')) headers.set('Accept', 'application/json');
    if (!['GET', 'HEAD', 'OPTIONS'].includes(method)) headers.set('X-CSRF-Token', csrfToken);
    const response = await fetch(path, { ...options, method, headers });
    if (response.status === 401 && path.startsWith('/api/')) {
      redirectToLogin();
      throw new ApiAuthError('Your dashboard session has expired.');
    }
    return response;
  }

  async function getList(path) {
    const response = await apiFetch(path, { headers: { Accept: 'application/json' } });
    if (!response.ok) {
      const body = await response.json().catch(() => ({}));
      throw new Error(body.error || `Request failed (${response.status})`);
    }
    const data = await response.json();
    if (!data || !Array.isArray(data.items)) throw new Error('The server returned an invalid response');
    return data.items;
  }

  function teamQuery() {
    return state.team ? `namespace=${encodeURIComponent(state.team)}` : '';
  }

  function withTeam(path) {
    const query = teamQuery();
    if (!query) return path;
    return `${path}${path.includes('?') ? '&' : '?'}${query}`;
  }

  function refreshForTeam() {
    if ($('#applications')) refreshApplications();
    if ($('#cloud')) refreshCloud();
  }

  function element(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  function condition(obj, type) {
    return (obj?.status?.conditions || []).find((item) => item.type === type);
  }

  function clusterConnection(cluster) {
    const connected = condition(cluster, 'Connected');
    if (!connected) return { label: 'Connection status unknown', tone: 'neutral' };
    return connected.status === 'True'
      ? { label: 'Connected', tone: 'good' }
      : connected.status === 'False'
        ? { label: 'Not connected', tone: 'muted' }
        : { label: 'Connection status unknown', tone: 'neutral' };
  }

  function clusterStatusKey(cluster) {
    const label = clusterConnection(cluster).label;
    if (label === 'Connected') return 'connected';
    if (label === 'Not connected') return 'disconnected';
    return 'unknown';
  }

  function renderClusterKpis() {
    const total = state.clusters.length;
    const connected = state.clusters.filter((cluster) => clusterStatusKey(cluster) === 'connected').length;
    const disconnected = state.clusters.filter((cluster) => clusterStatusKey(cluster) === 'disconnected').length;
    const nodes = state.clusters.reduce((sum, cluster) => sum + (Array.isArray(cluster.status?.nodes) ? cluster.status.nodes.length : 0), 0);
    const values = { total, connected, disconnected, nodes };
    Object.entries(values).forEach(([key, value]) => { const target = $(`#cluster-kpi-${key}`); if (target) target.textContent = String(value); });
  }

  function renderClustersPage() {
    const root = $('#clusters');
    if (!root) return;
    renderClusterKpis();
    const query = state.clusterQuery.trim().toLocaleLowerCase('en');
    const clusters = state.clusters.filter((cluster) => {
      const name = String(cluster.metadata?.name || '').toLocaleLowerCase('en');
      return (!query || name.includes(query)) && (!state.clusterStatus || clusterStatusKey(cluster) === state.clusterStatus);
    });
    root.replaceChildren();
    if (!state.clusters.length) {
      const row = document.createElement('tr');
      const cell = element('td', 'apps-empty', 'No clusters are enrolled yet.');
      cell.colSpan = 2; row.append(cell); root.append(row);
      if (state.selectedCluster) closeCluster();
    } else if (!clusters.length) {
      const row = document.createElement('tr');
      const cell = element('td', 'apps-empty', 'No clusters match these filters.');
      cell.colSpan = 2; row.append(cell); root.append(row);
    } else {
      clusters.forEach((cluster) => {
        const row = document.createElement('tr');
        const name = cluster.metadata?.name || 'Unnamed cluster';
        const nameCell = document.createElement('td');
        const open = element('button', 'application-link clusters-name-link', safe(name));
        open.type = 'button'; open.setAttribute('aria-label', `Open cluster ${name} details`);
        open.addEventListener('click', () => openCluster(cluster));
        nameCell.append(open);
        const nodes = Array.isArray(cluster.status?.nodes) ? cluster.status.nodes.length : null;
        const nodesCell = element('td', 'clusters-node-cell', nodes === null ? '—' : String(nodes));
        if (nodes !== null) nodesCell.title = `${nodes} ${nodes === 1 ? 'node' : 'nodes'}`;
        row.append(nameCell, nodesCell); root.append(row);
      });
    }
    if (state.selectedCluster) {
      const current = state.clusters.find((item) => item.metadata?.uid === state.selectedCluster.metadata?.uid || item.metadata?.name === state.selectedCluster.metadata?.name);
      if (current) { state.selectedCluster = current; renderClusterDetail(); }
      else closeCluster();
    }
  }

  function openCluster(cluster) {
    state.selectedCluster = cluster;
    $('#clusters-list-view').hidden = true;
    document.querySelector('.page-clusters')?.classList.add('is-detail-open');
    renderClusterDetail();
    $('#cluster-detail .back-button').focus();
  }

  function closeCluster() {
    const name = state.selectedCluster?.metadata?.name;
    state.selectedCluster = null;
    $('#cluster-detail').hidden = true; $('#clusters-list-view').hidden = false;
    document.querySelector('.page-clusters')?.classList.remove('is-detail-open');
    const links = [...$('#clusters').querySelectorAll('.application-link')];
    (links.find((item) => item.textContent === name) || links[0] || $('#refresh-clusters'))?.focus();
  }

  function renderClusterDetail() {
    const cluster = state.selectedCluster;
    if (!cluster) return;
    const root = $('#cluster-detail'); root.replaceChildren(); root.hidden = false;
    const back = element('button', 'back-button', '← Back to clusters');
    back.type = 'button'; back.addEventListener('click', closeCluster);
    root.append(back, element('p', 'eyebrow', 'Cluster details'));
    const title = element('h2', '', safe(cluster.metadata?.name)); title.id = 'cluster-detail-title'; root.append(title);
    const connection = clusterConnection(cluster);
    const summary = element('div', 'cluster-summary');
    summary.append(element('span', 'cluster-summary-label', 'Connection'), badge(connection.label, connection.tone));
    root.append(summary);
    const envLabel = cluster.metadata?.labels?.['platform.releaseyard.io/environment'] || '';
    const envForm = element('form', 'cluster-environment-form');
    envForm.append(element('h3', 'values-title', 'Environment'));
    const envLabelInput = element('label', 'field');
    envLabelInput.append(element('span', '', 'Environment label'));
    const envInput = document.createElement('input'); envInput.name = 'environment'; envInput.value = envLabel; envInput.maxLength = 63; envInput.pattern = '[a-z0-9]([-a-z0-9]*[a-z0-9])?'; envInput.placeholder = 'e.g. production';
    envInput.setAttribute('aria-describedby', 'cluster-environment-hint');
    envLabelInput.append(envInput, element('small', '', 'Lowercase letters, numbers, and hyphens. Leave blank to remove.'));
    const envActions = element('div', 'form-actions');
    const envHint = element('p', `helper${state.canChangeClusterEnvironment ? '' : ' access-required'}`, state.canChangeClusterEnvironment ? 'Set one environment for this cluster.' : 'Administrator access is required to change a cluster environment.'); envHint.id = 'cluster-environment-hint';
    const envSave = element('button', 'button button-primary', state.savingEnvironment ? 'Saving…' : 'Save environment'); envSave.type = 'submit'; envSave.disabled = !state.canChangeClusterEnvironment || state.savingEnvironment || envInput.value === envLabel;
    envSave.setAttribute('aria-describedby', 'cluster-environment-hint');
    envInput.addEventListener('input', () => { envSave.disabled = !state.canChangeClusterEnvironment || state.savingEnvironment || !envInput.checkValidity() || envInput.value === envLabel; });
    envActions.append(envHint, envSave); envForm.append(envLabelInput, envActions);
    envForm.addEventListener('submit', async (event) => {
      event.preventDefault(); if (!state.canChangeClusterEnvironment || !envInput.reportValidity() || state.savingEnvironment) return;
      state.savingEnvironment = true; envSave.disabled = true; envSave.textContent = 'Saving…';
      try {
        const response = await apiFetch(`/api/clusters/${encodeURIComponent(cluster.metadata.name)}/environment`, { method: 'PATCH', headers: { 'Content-Type': 'application/json', Accept: 'application/json' }, body: JSON.stringify({ uid: cluster.metadata.uid, resourceVersion: cluster.metadata.resourceVersion, environment: envInput.value.trim() || null }) });
        const body = await response.json().catch(() => ({}));
        if (response.status === 409) throw new Error('Cluster changed while saving. Refresh and try again.');
        if (!response.ok) throw new Error(body.error || `Could not save environment (${response.status})`);
        state.selectedCluster = body; state.clusters = state.clusters.map((item) => item.metadata?.uid === body.metadata?.uid ? body : item);
        renderClustersPage(); showNotice('Cluster environment saved.', 'success');
      } catch (error) { showNotice(error.message || 'Could not save cluster environment.', 'error'); }
      finally { state.savingEnvironment = false; renderClusterDetail(); }
    });
    root.append(envForm);
    root.append(element('p', 'helper cluster-connection-note', 'Connection status reflects authentication and connectivity, not overall cluster health.'));
    const nodesReady = condition(cluster, 'NodesReady');
    const reportedNodes = Array.isArray(cluster.status?.nodes) ? cluster.status.nodes : null;
    const nodes = reportedNodes || (nodesReady?.status === 'True' ? [] : null);
    if (connection.label === 'Not connected' || nodesReady?.status === 'False') {
      root.append(element('p', 'warning', 'Node inventory may be unavailable, incomplete, or outdated. Previously reported nodes are shown when available.'));
    }
    root.append(element('h3', 'values-title', 'Nodes'));
    if (nodes === null) {
      root.append(element('p', 'empty', 'Node inventory has not been collected yet.'));
    } else {
      const count = element('p', 'cluster-node-count', `${nodes.length} ${nodes.length === 1 ? 'node' : 'nodes'}`);
      root.append(count);
      if (!nodes.length) root.append(element('p', 'empty', 'No nodes were reported.'));
      else {
        const list = element('ul', 'cluster-node-list');
        nodes.forEach((node) => {
          const item = element('li', 'cluster-node');
          item.append(element('span', 'cluster-node-name', safe(node?.name || 'Unnamed node')));
          const ready = node?.ready === 'True' ? { label: 'Ready', tone: 'good' } : node?.ready === 'False' ? { label: 'Not ready', tone: 'muted' } : { label: 'Unknown', tone: 'neutral' };
          item.append(badge(ready.label, ready.tone)); list.append(item);
        });
        root.append(list);
      }
    }
    if (cluster.status?.nodesObservedAt) {
      const observed = new Date(cluster.status.nodesObservedAt);
      const time = Number.isNaN(observed.getTime()) ? safe(cluster.status.nodesObservedAt) : new Intl.DateTimeFormat('en', { dateStyle: 'medium', timeStyle: 'short' }).format(observed);
      root.append(element('p', 'resource-meta cluster-observed', `Last observed: ${time}`));
    }
  }

  async function refreshClusterPage() {
    const button = $('#refresh-clusters'); button.disabled = true;
    try {
      const selectedName = state.selectedCluster?.metadata?.name;
      state.clusters = await getList('/api/clusters');
      if (selectedName) state.selectedCluster = state.clusters.find((item) => item.metadata?.name === selectedName) || null;
      if (selectedName && !state.selectedCluster) { $('#cluster-detail').hidden = true; $('#clusters-list-view').hidden = false; document.querySelector('.page-clusters')?.classList.remove('is-detail-open'); }
      renderClustersPage();
      notice.hidden = true;
      return true;
    } catch (error) {
      showNotice(error.message || 'Could not load clusters. Try again.', 'error');
      if (!state.clusters.length) {
        const root = $('#clusters');
        if (root) {
          const row = document.createElement('tr');
          const cell = element('td', 'apps-empty', 'Cluster list unavailable. Try again.');
          cell.colSpan = 2; row.append(cell); root.replaceChildren(row);
        }
      }
      return false;
    } finally { button.disabled = false; }
  }

  function definitionReadiness(def) {
    const validation = condition(def, 'Validated');
    const generation = def?.metadata?.generation;
    if (!validation) return { ready: false, reason: 'Validation is pending.' };
    if (validation.status !== 'True') return { ready: false, reason: validation.message || validation.reason || 'The definition has not been validated.' };
    if (validation.observedGeneration !== generation || def?.status?.observedGeneration !== generation) return { ready: false, reason: 'Validation is not current for this generation.' };
    return { ready: true, reason: '' };
  }

  function badge(label, tone = '') {
    return element('span', `badge${tone ? ` badge-${tone}` : ''}`, label);
  }

  function workloadHealth(app) {
    const health = condition(app, 'WorkloadHealthy');
    const inventory = condition(app, 'InventoryReady');
    const observedAt = app.status?.workloadObservedAt;
    const validTimestamp = typeof observedAt === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(observedAt);
    const observedTime = validTimestamp ? Date.parse(observedAt) : NaN;
    const age = Date.now() - observedTime;
    const fresh = inventory?.status === 'True'
      && Number.isInteger(app.metadata?.generation) && app.metadata.generation === app.status?.observedGeneration
      && health?.observedGeneration === app.metadata.generation
      && Number.isFinite(observedTime) && age >= 0 && age <= 5 * 60 * 1000;
    if (!fresh || !health || health.status === 'Unknown') {
      if (fresh && health?.status === 'Unknown' && health.reason === 'NoWorkloads') return { label: 'N/A', tone: 'neutral', fresh, reason: health.reason, observedAt };
      return { label: 'Unknown', tone: 'neutral', fresh: false, reason: !fresh ? 'stale' : health?.reason || 'unknown', observedAt };
    }
    if (health.status === 'True') return { label: 'Healthy', tone: 'good', fresh: true, reason: health.reason, observedAt };
    if (health.status === 'False') {
      const label = health.reason === 'Degraded' ? 'Degraded' : health.reason === 'Progressing' ? 'Progressing' : 'Unknown';
      return { label, tone: label === 'Degraded' ? 'error' : label === 'Progressing' ? 'muted' : 'neutral', fresh: true, reason: health.reason, observedAt };
    }
    return { label: 'Unknown', tone: 'neutral', fresh: false, reason: health.reason || 'unknown', observedAt };
  }

  function applicationTargets(app) {
    if (!app.spec?.environment) return null;
    if (Array.isArray(app.spec.environment.targets) && app.spec.environment.targets.length) return app.spec.environment.targets;
    return Array.isArray(app.status?.instances) ? app.status.instances.map((instance) => ({ name: instance.clusterName, uid: instance.clusterUID })) : [];
  }

  function instanceFreshness(app, instance) {
    const generation = app.metadata?.generation;
    const conditions = instance?.conditions || [];
    const ready = conditions.find((item) => item.type === 'Ready');
    const inventory = conditions.find((item) => item.type === 'InventoryReady');
    const health = conditions.find((item) => item.type === 'WorkloadHealthy');
    const target = applicationTargets(app)?.find((item) => item.uid === instance?.clusterUID);
    const definition = applicationDefinition(app);
    const validated = definition && condition(definition, 'Validated');
    const definitionGeneration = definition?.metadata?.generation;
    const environmentBinding = app.status?.environmentBinding;
    const binding = instance?.binding;
    const applied = instance?.applied;
    const chart = definition?.spec?.chart || {};
    const appliedChart = applied?.chart;
    const chartPin = (app.status?.chartPins || []).find((item) => item.repository === chart.repository && item.name === chart.name && item.version === chart.version);
    const rootReady = condition(app, 'Ready');
    const sharedFailureReasons = ['DependencyNotReady', 'InvalidApplication', 'BindingConflict', 'ChartUnavailable', 'ChartVerificationFailed', 'ChartDigestChanged', 'TargetIdentityConflict'];
    const sharedInputFailure = rootReady?.status === 'False' && sharedFailureReasons.includes(rootReady.reason);
    const definitionCurrent = Boolean(definition?.metadata?.uid && environmentBinding?.applicationDefinitionUID === definition.metadata.uid
      && validated?.status === 'True' && validated.observedGeneration === definitionGeneration
      && definition.status?.observedGeneration === definitionGeneration && definition.status?.digest
      && chart.repository && chart.name && chart.version
      && appliedChart?.repository === chart.repository && appliedChart?.name === chart.name && appliedChart?.version === chart.version
      && appliedChart?.digest === definition.status.digest && chartPin?.digest === definition.status.digest);
    const bindingCurrent = Boolean(target && binding && binding.clusterUID === target.uid && binding.remoteClaimUID
      && binding.applicationDefinitionUID === definition?.metadata?.uid
      && binding.releaseName === app.metadata?.name && binding.releaseNamespace === environmentBinding?.releaseNamespace
      && environmentBinding?.releaseNamespace === targetNamespace(app));
    const observedAt = instance?.workloadObservedAt;
    const timestamp = typeof observedAt === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(observedAt) ? Date.parse(observedAt) : NaN;
    const workloadObserved = health?.status === 'True' || health?.status === 'False' || health?.reason === 'NoWorkloads';
    const fresh = Boolean(target && target.uid === instance?.clusterUID && definitionCurrent && bindingCurrent && !sharedInputFailure)
      && validated?.status === 'True' && validated.observedGeneration === definitionGeneration
      && definition.status?.observedGeneration === definitionGeneration
      && inventory?.status === 'True' && ready?.status === 'True' && Number.isInteger(generation)
      && instance?.observedGeneration === generation
      && inventory?.observedGeneration === generation
      && health?.observedGeneration === generation
      && ready?.observedGeneration === generation && workloadObserved
      && applied?.applicationGeneration === generation && applied?.applicationDefinitionGeneration === definitionGeneration
      && applied?.desiredInputIdentity && applied.desiredInputIdentity === instance?.lastAttemptedIdentity
      && Number.isFinite(timestamp) && Date.now() >= timestamp && Date.now() - timestamp <= 5 * 60 * 1000;
    const workload = !fresh
      ? { label: 'Unknown', tone: 'neutral', fresh: false }
      : health.reason === 'NoWorkloads' ? { label: 'N/A', tone: 'neutral', fresh: true }
      : health.status === 'True' ? { label: 'Healthy', tone: 'good', fresh: true }
        : health.reason === 'Degraded' ? { label: 'Degraded', tone: 'error', fresh: true }
          : health.reason === 'Progressing' ? { label: 'Progressing', tone: 'muted', fresh: true }
            : { label: 'Unknown', tone: 'neutral', fresh: false };
    workload.observedAt = observedAt;
    return { ready: fresh ? { label: 'Ready', tone: 'good' } : { label: 'Unknown', tone: 'neutral' }, workload, fresh };
  }

  function aggregateEnvironmentHealth(app) {
    const targets = applicationTargets(app);
    if (!targets?.length) return { label: 'Unknown', tone: 'neutral' };
    const instances = Array.isArray(app.status?.instances) ? app.status.instances : [];
    const found = targets.map((target) => instances.find((item) => item.clusterUID === target.uid));
    if (found.some((item) => !item)) return { label: 'Unknown', tone: 'neutral' };
    const health = found.map((item) => instanceFreshness(app, item).workload);
    if (health.some((item) => item.label === 'Degraded')) return { label: 'Degraded', tone: 'error' };
    const generation = app.metadata?.generation;
    const rootHealth = condition(app, 'WorkloadHealthy');
    const rootInventory = condition(app, 'InventoryReady');
    const rootCurrent = Number.isInteger(generation)
      && app.status?.observedGeneration === generation
      && rootHealth?.observedGeneration === generation
      && rootHealth.status === 'True'
      && rootInventory?.observedGeneration === generation
      && rootInventory.status === 'True';
    if (!rootCurrent || health.some((item) => item.label === 'Unknown')) return { label: 'Unknown', tone: 'neutral' };
    if (health.some((item) => item.label === 'Progressing')) return { label: 'Progressing', tone: 'muted' };
    if (health.every((item) => item.label === 'Healthy')) return { label: 'Healthy', tone: 'good' };
    return { label: 'Unknown', tone: 'neutral' };
  }

  function selectedApplicationInstance(app) {
    const targets = applicationTargets(app);
    if (!targets) return null;
    const instances = Array.isArray(app.status?.instances) ? app.status.instances : [];
    const target = targets.find((item) => item.uid === state.selectedInstanceUID) || targets[0];
    if (!target) return null;
    state.selectedInstanceUID = target.uid;
    return { target, instance: instances.find((item) => item.clusterUID === target.uid) || null };
  }

  function formatObservedTime(value) {
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? safe(value) : new Intl.DateTimeFormat('en', { dateStyle: 'medium', timeStyle: 'short' }).format(date);
  }

  function completedHealthyJobAncestor(podResource, resources) {
    const byUID = new Map(resources.filter((item) => item?.uid).map((item) => [item.uid, item]));
    const seen = new Set();
    let parent = podResource?.parentUID ? byUID.get(podResource.parentUID) : null;
    while (parent && !seen.has(parent)) {
      seen.add(parent);
      if (parent.kind === 'Job' && parent.job) {
        const job = parent.job;
        if (job.state === 'Healthy') return true;
      }
      parent = parent.parentUID ? byUID.get(parent.parentUID) : null;
    }
    return false;
  }

  function workloadCardStatus(state) {
    const known = ['Healthy', 'Progressing', 'Degraded', 'Unknown'].includes(state) ? state : 'Unknown';
    return { label: known, tone: known === 'Healthy' ? 'good' : known === 'Degraded' ? 'error' : known === 'Progressing' ? 'muted' : 'neutral' };
  }

  function appendResourceWorkloadStatus(card, resource, fresh) {
    const configs = {
      Deployment: { data: resource.deployment, count: (value) => value.readyReplicas != null && value.desiredReplicas != null ? `${value.readyReplicas}/${value.desiredReplicas} ready` : 'Replica counts unavailable' },
      StatefulSet: { data: resource.statefulSet, count: (value) => value.readyReplicas != null && value.desiredReplicas != null ? `${value.readyReplicas}/${value.desiredReplicas} ready` : 'Replica counts unavailable' },
      DaemonSet: { data: resource.daemonSet, count: (value) => value.numberReady != null && value.desiredNumberScheduled != null ? `${value.numberReady}/${value.desiredNumberScheduled} ready` : 'Node counts unavailable' },
      Job: { data: resource.job, count: (value) => value.completions != null ? (value.succeeded != null ? `${value.succeeded}/${value.completions} completed` : `Completed count unavailable (of ${value.completions})`) : value.succeeded != null ? `${value.succeeded} completed` : 'Completed count unavailable' }
    };
    const config = configs[resource.kind];
    if (!config?.data) return;
    const details = config.data;
    const row = element('div', 'resource-health');
    if (!fresh) {
      row.append(badge('Status unknown', 'neutral'));
      card.append(row);
      return;
    }
    const status = workloadCardStatus(details.state);
    const noEligibleNodes = resource.kind === 'DaemonSet' && details.reason === 'NoEligibleNodes';
    row.append(badge(noEligibleNodes ? 'No eligible nodes' : status.label, noEligibleNodes ? 'neutral' : status.tone));
    row.append(element('span', 'resource-replicas', config.count(details)));
    if (resource.kind === 'Job') {
      const activity = [];
      if (details.active != null) activity.push(`Active: ${details.active}`);
      if (details.failed != null && details.failed > 0) activity.push(`Failed attempts: ${details.failed}`);
      if (activity.length) row.append(element('span', 'resource-workload-note', activity.join(' · ')));
    }
    if (details.reason) {
      let reason = details.reason;
      if (resource.kind === 'Job' && ['Suspended', 'Paused'].includes(reason)) reason = reason === 'Suspended' ? 'Job is suspended' : 'Job is paused';
      else if (resource.kind === 'Job' && ['Failed', 'BackoffLimitExceeded', 'DeadlineExceeded', 'FailureTarget'].includes(reason)) reason = `Job failure: ${reason}`;
      if (!(noEligibleNodes && details.reason === 'NoEligibleNodes')) row.append(element('span', 'resource-workload-note', safe(reason)));
    }
    card.append(row);
  }

  function appendApplicationHealth(root, app, authoritativeHealth = null) {
    const health = authoritativeHealth || workloadHealth(app);
    const section = element('section', 'application-health');
    section.setAttribute('aria-labelledby', 'application-health-title');
    const heading = element('h3', 'values-title', 'Workload health');
    heading.id = 'application-health-title';
    section.append(heading);
    const summary = element('div', 'health-summary');
    summary.append(badge(health.label, health.tone));
    if (health.observedAt && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(health.observedAt)) {
      summary.append(element('span', 'resource-meta', `Last observed${health.fresh ? '' : ' (stale)'}: ${formatObservedTime(health.observedAt)}`));
    }
    section.append(summary);
    if (!health.fresh && health.label !== 'N/A') {
      section.append(element('p', 'helper health-note', authoritativeHealth ? 'This instance is not current with the validated definition or application inputs. Retained resources below are last-known only.' : 'Workload health is unknown because inventory is unavailable, outdated, or not current for this application. Refresh to check again.'));
      root.append(section); return;
    }
    if (health.label === 'N/A') {
      section.append(element('p', 'helper health-note', 'No workloads are defined for this application.'));
      root.append(section); return;
    }

    const resources = Array.isArray(app.status?.resources) ? app.status.resources : [];
    const pods = resources.filter((item) => item?.kind === 'Pod' && item.pod);
    const hasWorkload = resources.some((item) => (item?.kind === 'Deployment' && item.deployment)
      || (item?.kind === 'StatefulSet' && item.statefulSet)
      || (item?.kind === 'DaemonSet' && item.daemonSet)
      || (item?.kind === 'Job' && item.job));
    if (!pods.length && !hasWorkload) {
      section.append(element('p', 'helper health-note', 'No workload resources were reported.'));
      root.append(section); return;
    }
    const problematicPods = pods.filter((item) => item.pod.state !== 'Healthy' && !completedHealthyJobAncestor(item, resources));
    if (problematicPods.length) {
      section.append(element('h4', 'health-subheading', 'Pods to review'));
      const podList = element('ul', 'health-pod-list');
      problematicPods.forEach((item) => {
        const pod = item.pod;
        const row = element('li', 'health-pod');
        const title = element('span', 'health-resource-name', safe(item.name || 'Unnamed pod'));
        const stateLabel = pod.state === 'Degraded' ? { label: 'Degraded', tone: 'error' } : pod.state === 'Progressing' ? { label: 'Progressing', tone: 'muted' } : pod.ready === 'False' ? { label: 'Not ready', tone: 'muted' } : { label: 'Unknown', tone: 'neutral' };
        row.append(title, badge(stateLabel.label, stateLabel.tone));
        const message = [pod.reason, pod.message].filter(Boolean).join(' · ');
        if (message) row.append(element('span', 'health-reason', safe(message)));
        podList.append(row);
      });
      section.append(podList);
    }
    root.append(section);
  }

  function applicationClusterLabel(app) {
    const targets = applicationTargets(app);
    if (app.spec?.environment) {
      if (targets?.length === 1) return targets[0].name || app.spec.environment.name;
      if (targets?.length > 1) return `${app.spec.environment.name} · ${targets.length} clusters`;
      return app.spec.environment.name;
    }
    return app.spec?.clusterRef?.name || '—';
  }

  function applicationNamespaceLabel(app) {
    return targetNamespace(app) || '—';
  }

  function applicationLastUpdated(app) {
    const values = [app.status?.workloadObservedAt, ...(Array.isArray(app.status?.instances) ? app.status.instances.map((instance) => instance.workloadObservedAt) : [])]
      .filter(Boolean);
    values.sort();
    return values.length ? values[values.length - 1] : app.metadata?.creationTimestamp || '';
  }

  function relativeTimeAgo(value) {
    const date = Date.parse(value || '');
    if (!Number.isFinite(date)) return '—';
    const seconds = Math.max(0, Math.round((Date.now() - date) / 1000));
    if (seconds < 60) return `${seconds} second${seconds === 1 ? '' : 's'} ago`;
    const minutes = Math.round(seconds / 60);
    if (minutes < 60) return `${minutes} minute${minutes === 1 ? '' : 's'} ago`;
    const hours = Math.round(minutes / 60);
    if (hours < 24) return `${hours} hour${hours === 1 ? '' : 's'} ago`;
    const days = Math.round(hours / 24);
    return `${days} day${days === 1 ? '' : 's'} ago`;
  }

  function applicationTableStatus(app) {
    const name = app.metadata?.name;
    if (state.deletionPending.has(name)) return { key: 'unavailable', label: 'Deleting', tone: 'muted' };
    const targets = applicationTargets(app);
    const health = targets ? aggregateEnvironmentHealth(app) : workloadHealth(app);
    const ready = condition(app, 'Ready');
    if (health.label === 'Degraded') return { key: 'degraded', label: 'Degraded', tone: 'muted' };
    if (targets) {
      if (health.label === 'Healthy' && ready?.status === 'True') return { key: 'running', label: 'Running', tone: 'good' };
      return { key: 'progressing', label: 'Progressing', tone: 'info' };
    }
    if (ready?.status === 'True') return { key: 'running', label: 'Running', tone: 'good' };
    if (ready?.status === 'False' && health.label !== 'Progressing') return { key: 'unavailable', label: 'Unavailable', tone: 'error' };
    if (!ready || ready.status === 'Unknown' || health.label === 'Progressing') {
      return { key: 'progressing', label: 'Progressing', tone: 'info' };
    }
    return { key: 'unavailable', label: 'Unavailable', tone: 'error' };
  }

  function applicationResourceGroup(kind) {
    if (/^(Deployment|StatefulSet|DaemonSet|ReplicaSet|Job|CronJob|Pod)$/.test(kind)) return 'workload';
    if (/^(Service|Ingress|EndpointSlice|NetworkPolicy)$/.test(kind)) return 'network';
    if (/^(ConfigMap|Secret|ServiceAccount|Role|RoleBinding|PersistentVolumeClaim)$/.test(kind)) return 'config';
    return 'other';
  }

  function applicationResourceIcon(group) {
    const paths = {
      workload: 'M12 3 21 8v8l-9 5-9-5V8l9-5Zm0 0v9m9-4-9 5-9-5',
      network: 'M4 7h16v10H4zM8 11h.01M12 11h.01M16 11h.01M8 15h8',
      config: 'M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8zM14 3v5h5M8 13h8M8 17h5',
      other: 'M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4zM13 13h7v7h-7z',
    };
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('aria-hidden', 'true'); svg.setAttribute('class', 'apps-resource-icon');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', paths[group] || paths.other);
    svg.append(path);
    return svg;
  }

  function applicationResourceSummary(app) {
    const resources = [];
    if (Array.isArray(app.status?.instances) && app.status.instances.length) {
      app.status.instances.forEach((instance) => { if (Array.isArray(instance.resources)) resources.push(...instance.resources); });
    } else if (Array.isArray(app.status?.resources)) {
      resources.push(...app.status.resources);
    }
    const groups = new Map();
    resources.forEach((resource) => {
      if (!resource) return;
      const group = applicationResourceGroup(String(resource.kind || ''));
      groups.set(group, (groups.get(group) || 0) + 1);
    });
    const order = ['workload', 'network', 'config', 'other'];
    return [...groups.entries()]
      .sort((a, b) => order.indexOf(a[0]) - order.indexOf(b[0]))
      .map(([group, count]) => ({ group, count }));
  }

  function fillApplicationFilter(select, allLabel, values) {
    if (!select) return;
    const current = select.value;
    const unique = [...new Set(values.filter((value) => value && value !== '—'))].sort((a, b) => a.localeCompare(b, 'en', { sensitivity: 'base' }));
    select.replaceChildren(new Option(allLabel, ''), ...unique.map((value) => new Option(value, value)));
    select.value = unique.includes(current) ? current : '';
  }

  function renderApplicationKpis() {
    const total = state.applications.length;
    const counts = { running: 0, degraded: 0, progressing: 0, unavailable: 0 };
    state.applications.forEach((app) => { counts[applicationTableStatus(app).key] += 1; });
    const degraded = counts.degraded + counts.progressing;
    const percent = (value) => total ? `${Math.round((value / total) * 100)}% of total` : '0% of total';
    $('#apps-kpi-total').textContent = String(total);
    $('#apps-kpi-total-note').textContent = state.team ? 'Team scope' : 'All teams';
    $('#apps-kpi-healthy').textContent = String(counts.running);
    $('#apps-kpi-healthy-note').textContent = percent(counts.running);
    $('#apps-kpi-degraded').textContent = String(degraded);
    $('#apps-kpi-degraded-note').textContent = percent(degraded);
    $('#apps-kpi-unavailable').textContent = String(counts.unavailable);
    $('#apps-kpi-unavailable-note').textContent = percent(counts.unavailable);
  }

  function renderApplicationSort() {
    document.querySelectorAll('.apps-sort').forEach((button) => {
      const active = button.dataset.sort === state.applicationSort.key;
      const arrow = button.querySelector('.apps-sort-arrow');
      if (arrow) arrow.textContent = active ? (state.applicationSort.direction === 'asc' ? '↑' : '↓') : '↕';
      button.setAttribute('aria-pressed', String(active));
      const cell = button.closest('th');
      if (cell) cell.setAttribute('aria-sort', active ? (state.applicationSort.direction === 'asc' ? 'ascending' : 'descending') : 'none');
    });
  }

  function applicationRowMenu(app) {
    const menu = document.createElement('details');
    menu.className = 'detail-menu apps-row-menu';
    const toggle = element('summary', 'detail-menu-toggle apps-row-toggle');
    toggle.setAttribute('aria-label', 'More actions'); toggle.title = 'More actions';
    toggle.append(dotsIcon());
    const list = element('div', 'detail-menu-list');
    const open = element('button', 'detail-menu-item', 'Open details'); open.type = 'button';
    open.addEventListener('click', () => { menu.open = false; openApplication(app); });
    const pending = state.deletionPending.has(app.metadata?.name);
    const remove = element('button', 'detail-menu-item detail-menu-danger', pending ? 'Deletion in progress' : 'Delete application');
    remove.type = 'button';
    remove.disabled = !state.canDeleteApps || state.deleting || pending;
    if (!state.canDeleteApps) remove.title = 'Developer or administrator access is required to delete an application.';
    remove.addEventListener('click', () => { menu.open = false; deleteApplication(app); });
    list.append(open, remove);
    menu.append(toggle, list);
    return menu;
  }

  function renderApplications() {
    const root = $('#applications');
    if (!root) return;
    renderApplicationKpis();
    fillApplicationFilter($('#application-cluster-filter'), 'All clusters', state.applications.map(applicationClusterLabel));
    fillApplicationFilter($('#application-namespace-filter'), 'All namespaces', state.applications.map(applicationNamespaceLabel));
    state.applicationCluster = $('#application-cluster-filter')?.value || '';
    state.applicationNamespace = $('#application-namespace-filter')?.value || '';
    state.applicationStatus = $('#application-status-filter')?.value || '';
    const query = state.applicationQuery.trim().toLocaleLowerCase('en');
    const applications = state.applications.filter((app) => {
      const status = applicationTableStatus(app);
      const searchable = [app.metadata?.name, app.spec?.applicationDefinitionRef?.name, applicationClusterLabel(app), applicationNamespaceLabel(app)]
        .filter(Boolean).join(' ').toLocaleLowerCase('en');
      return (!query || searchable.includes(query))
        && (!state.applicationCluster || applicationClusterLabel(app) === state.applicationCluster)
        && (!state.applicationNamespace || applicationNamespaceLabel(app) === state.applicationNamespace)
        && (!state.applicationStatus || status.key === state.applicationStatus);
    }).sort((a, b) => {
      const direction = state.applicationSort.direction === 'desc' ? -1 : 1;
      if (state.applicationSort.key === 'updated') {
        return ((Date.parse(applicationLastUpdated(a)) || 0) - (Date.parse(applicationLastUpdated(b)) || 0)) * direction;
      }
      return String(a.metadata?.name || '').localeCompare(String(b.metadata?.name || ''), 'en', { sensitivity: 'base' }) * direction;
    });

    root.replaceChildren();
    if (!state.applications.length) {
      const row = document.createElement('tr');
      const cell = element('td', 'apps-empty', 'No applications yet.');
      const link = element('a', 'inline-link', 'Browse the Catalog');
      link.href = './';
      cell.append(' ', link); cell.colSpan = 8; row.append(cell); root.append(row);
      if (state.selectedApplication) closeApplication();
    } else if (!applications.length) {
      const row = document.createElement('tr');
      const cell = element('td', 'apps-empty', 'No applications match these filters.');
      cell.colSpan = 8; row.append(cell); root.append(row);
    } else {
      applications.forEach((app) => {
        const row = document.createElement('tr');
        row.className = 'apps-row';
        const name = app.metadata?.name || 'Unnamed application';
        const status = applicationTableStatus(app);

        const nameCell = document.createElement('td');
        const nameWrap = element('div', 'apps-name-cell');
        const icon = element('span', 'apps-icon'); icon.setAttribute('aria-hidden', 'true');
        const iconSvg = document.createElementNS('http://www.w3.org/2000/svg', 'svg'); iconSvg.setAttribute('viewBox', '0 0 40 40');
        const cube = document.createElementNS('http://www.w3.org/2000/svg', 'path');
        cube.setAttribute('d', 'M20 4 35 12.5v15L20 36 5 27.5v-15L20 4Zm0 0v16m15-7.5-15 8-15-8m15 8v15');
        iconSvg.append(cube); icon.append(iconSvg);
        const nameText = element('div', 'apps-name-text');
        const open = element('button', 'application-link apps-name-button', safe(name));
        open.type = 'button'; open.setAttribute('aria-label', `Open ${safe(name)} details`);
        open.addEventListener('click', () => openApplication(app));
        nameText.append(open, element('span', 'apps-def', app.spec?.applicationDefinitionRef?.name || 'Definition not specified'));
        nameWrap.append(icon, nameText); nameCell.append(nameWrap);

        const namespaceCell = document.createElement('td');
        namespaceCell.append(element('span', 'apps-namespace-pill', applicationNamespaceLabel(app)));

        const clusterCell = document.createElement('td');
        const clusterWrap = element('div', 'apps-cluster-cell');
        const clusterIcon = element('span', 'apps-cluster-icon');
        clusterIcon.append(metaIcon('cluster'));
        clusterWrap.append(clusterIcon, element('span', '', applicationClusterLabel(app)));
        clusterCell.append(clusterWrap);

        const statusCell = document.createElement('td');
        const statusBadge = badge(status.label, status.tone);
        statusBadge.classList.add('apps-status-badge');
        statusCell.append(statusBadge);

        const revision = app.status?.applied?.helmRevision;
        const revisionCell = element('td', 'apps-revision', revision === undefined ? '—' : String(revision));

        const resourcesCell = document.createElement('td');
        const summary = applicationResourceSummary(app);
        if (!summary.length) {
          resourcesCell.append(element('span', 'apps-cell-muted', '—'));
        } else {
          const resources = element('div', 'apps-resources');
          summary.slice(0, 3).forEach(({ group, count }) => {
            const item = element('span', 'apps-resource');
            item.title = `${count} ${group} resource${count === 1 ? '' : 's'}`;
            item.append(applicationResourceIcon(group), element('span', 'apps-resource-count', String(count)));
            resources.append(item);
          });
          if (summary.length > 3) resources.append(element('span', 'apps-resource-more', `+${summary.length - 3}`));
          resourcesCell.append(resources);
        }

        const updatedCell = element('td', 'apps-cell-muted', relativeTimeAgo(applicationLastUpdated(app)));
        const actionsCell = element('td', 'apps-actions-cell');
        actionsCell.append(applicationRowMenu(app));

        row.append(nameCell, namespaceCell, clusterCell, statusCell, revisionCell, resourcesCell, updatedCell, actionsCell);
        row.addEventListener('click', (event) => {
          if (event.target.closest('button, summary, a, details, input, select, label')) return;
          openApplication(app);
        });
        root.append(row);
      });
    }
    renderApplicationSort();
    if (state.selectedApplication) {
      const latest = state.applications.find((item) => item.metadata?.name === state.selectedApplication.metadata?.name);
      if (latest) { state.selectedApplication = latest; renderApplicationDetail(); }
      else closeApplication();
    }
  }

  function targetNamespace(app) {
    if (app.status?.binding?.releaseNamespace) return app.status.binding.releaseNamespace;
    if (app.spec?.releaseNamespace) return app.spec.releaseNamespace;
    const ref = app.spec?.applicationDefinitionRef?.name;
    return state.definitions.find((def) => def.metadata?.name === ref)?.spec?.release?.namespace || '';
  }

  function setDetailChrome(open) {
    const page = $('#application-detail')?.closest('.page');
    if (page) page.classList.toggle('is-detail-open', open);
  }
  function openApplication(app) { state.selectedApplication = app; state.selectedInstanceUID = ''; state.detailTab = 'overview'; state.editingValues = false; state.valuesDraft = Object.create(null); state.resourcesQuery = ''; state.resourcesKind = ''; state.resourcesStatus = ''; state.resourcesExpanded = new Set(); state.resourcesLive = null; state.resourcesLiveKey = ''; state.selectedResourceUID = ''; state.resourcePanelTab = 'overview'; state.activityRange = '24h'; state.activityType = ''; state.activitySource = ''; state.activityQuery = ''; $('#applications-list-view').hidden = true; setDetailChrome(true); renderApplicationDetail(); $('#application-detail .back-button').focus(); }
  function closeApplication() {
    const name = state.selectedApplication?.metadata?.name;
    state.selectedApplication = null; state.editingValues = false; state.valuesDraft = Object.create(null); $('#application-detail').hidden = true; $('#applications-list-view').hidden = false; setDetailChrome(false);
    const links = [...$('#applications').querySelectorAll('.application-link')];
    (links.find((button) => button.textContent === name) || links[0] || $('#refresh-applications'))?.focus();
  }
  const APPLICATION_TABS = [
    { id: 'overview', label: 'Overview' },
    { id: 'resources', label: 'Resources' },
    { id: 'configuration', label: 'Configuration' },
    { id: 'observability', label: 'Observability' },
    { id: 'activity', label: 'Activity' },
  ];

  function applicationReadyBadge(app) {
    const ready = (app?.status?.conditions || []).find((item) => item.type === 'Ready');
    if (ready?.status === 'True') return badge('Healthy', 'good');
    if (ready?.status === 'False') return badge(ready.reason || 'Unhealthy', 'error');
    return badge('Progressing', 'muted');
  }

  const META_ICON_PATHS = {
    namespace: 'M12 3 3 7l9 4 9-4-9-4zM3 12l9 4 9-4M3 17l9 4 9-4',
    cluster: 'M4 4h16v6H4zM4 14h16v6H4zM7.5 7h.01M7.5 17h.01',
    definition: 'M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8zM14 3v5h5',
    cloud: 'M6 15h8a3 3 0 0 0 .4-6A4.5 4.5 0 0 0 6 10a2.5 2.5 0 0 0 0 5z',
  };

  function metaIcon(kind) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', META_ICON_PATHS[kind] || META_ICON_PATHS.definition);
    path.setAttribute('fill', 'none'); path.setAttribute('stroke', 'currentColor');
    path.setAttribute('stroke-width', '1.6'); path.setAttribute('stroke-linecap', 'round'); path.setAttribute('stroke-linejoin', 'round');
    svg.append(path);
    return svg;
  }

  function renderApplicationDetail() {
    const app = state.selectedApplication; if (!app) return;
    const root = $('#application-detail'); root.replaceChildren(); root.hidden = false;
    const selection = selectedApplicationInstance(app);
    const instanceView = selection?.instance ? {
      ...app,
      status: { ...app.status, conditions: selection.instance.conditions || [], observedGeneration: selection.instance.observedGeneration, workloadObservedAt: selection.instance.workloadObservedAt, resources: selection.instance.resources || [], binding: selection.instance.binding, applied: selection.instance.applied }
    } : null;
    const selectedHealth = selection?.instance ? instanceFreshness(app, selection.instance) : null;
    const view = instanceView || app;

    const header = element('div', 'detail-header');
    const topbar = element('div', 'detail-topbar');
    const back = element('button', 'back-button', '← Back to applications'); back.type = 'button'; back.addEventListener('click', closeApplication);
    const tools = element('div', 'detail-tools');
    const refresh = element('button', 'button button-secondary', 'Refresh'); refresh.type = 'button';
    refresh.addEventListener('click', async () => { refresh.disabled = true; try { await refreshDetail(); } finally { refresh.disabled = false; } });
    tools.append(refresh, detailMenu(app));
    topbar.append(back, tools);
    const titleRow = element('div', 'detail-title-row');
    const title = element('h1', 'detail-title', safe(app.metadata?.name)); title.id = 'application-detail-title';
    titleRow.append(title, applicationReadyBadge(view));
    header.append(topbar, titleRow);
    if (app.spec?.description) header.append(element('p', 'detail-description', safe(app.spec.description)));
    root.append(header);

    const meta = element('div', 'detail-meta');
    const metaItem = (iconKind, label, value) => {
      const item = element('span', 'detail-meta-item');
      const icon = element('span', 'detail-meta-icon'); icon.append(metaIcon(iconKind));
      const text = element('span', 'detail-meta-text');
      text.append(element('span', 'detail-meta-label', label), element('span', 'detail-meta-value', safe(value)));
      item.append(icon, text);
      return item;
    };
    [
      ['namespace', 'Namespace', app.metadata?.namespace],
      ['cluster', 'Cluster', app.spec?.clusterRef?.name || app.spec?.environment?.name],
      ['definition', 'Definition', app.spec?.applicationDefinitionRef?.name],
    ].filter((entry) => entry[2]).forEach((entry, index) => {
      if (index > 0) meta.append(element('span', 'detail-meta-divider'));
      meta.append(metaItem(entry[0], entry[1], entry[2]));
    });
    root.append(meta);

    const tabs = element('nav', 'detail-tabs'); tabs.setAttribute('aria-label', 'Application sections');
    APPLICATION_TABS.forEach((tab) => {
      const button = element('button', `detail-tab${state.detailTab === tab.id ? ' is-active' : ''}`, tab.label);
      button.type = 'button'; button.setAttribute('role', 'tab'); button.setAttribute('aria-selected', state.detailTab === tab.id ? 'true' : 'false');
      button.addEventListener('click', () => { state.detailTab = tab.id; renderApplicationDetail(); });
      tabs.append(button);
    });
    const panel = element('div', 'detail-panel');
    root.append(tabs, panel);
    renderApplicationTab(panel, state.detailTab, { app, view, selection, instanceView, selectedHealth });
  }

  function renderApplicationTab(panel, tab, ctx) {
    const { app, view, selection, instanceView, selectedHealth } = ctx;
    if (tab === 'overview') { renderOverview(panel, ctx); return; }
    if (tab === 'resources') {
      if (!selection || instanceView) appendResourcesTab(panel, { app, view, selectedHealth });
      else panel.append(element('p', 'helper', 'No instance selected.'));
      return;
    }
    if (tab === 'configuration') { renderConfigurationTab(panel, ctx); return; }
    if (tab === 'observability') { renderObservabilityTab(panel, ctx); return; }
    if (tab === 'activity') { renderActivityTab(panel, ctx); return; }
  }

  function dotsIcon() {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 20 20'); svg.setAttribute('aria-hidden', 'true');
    [5, 10, 15].forEach((cx) => {
      const dot = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
      dot.setAttribute('cx', String(cx)); dot.setAttribute('cy', '10'); dot.setAttribute('r', '1.7');
      svg.append(dot);
    });
    return svg;
  }

  function detailMenu(app) {
    const menu = document.createElement('details'); menu.className = 'detail-menu';
    const toggle = element('summary', 'detail-menu-toggle');
    toggle.setAttribute('aria-label', 'More actions'); toggle.title = 'More actions';
    toggle.append(dotsIcon());
    const list = element('div', 'detail-menu-list');
    const pending = state.deletionPending.has(app.metadata?.name);
    const label = pending ? 'Deletion in progress' : state.deleting ? 'Deleting…' : 'Delete application';
    const remove = element('button', 'detail-menu-item detail-menu-danger', label);
    remove.type = 'button';
    remove.disabled = !state.canDeleteApps || state.deleting || pending;
    if (!state.canDeleteApps) remove.title = 'Developer or administrator access is required to delete an application.';
    remove.addEventListener('click', () => { menu.open = false; deleteApplication(app); });
    list.append(remove);
    menu.append(toggle, list);
    return menu;
  }

  async function refreshDetail() {
    try {
      const apps = await getList(withTeam('/api/applications'));
      state.applications = apps;
      renderApplications();
      return true;
    } catch (error) {
      showNotice(error.message || 'Could not refresh this application.', 'error');
      return false;
    }
  }

  const OV_ICON_PATHS = {
    health: 'M12 20.5S3.5 15.5 3.5 9.8A4.3 4.3 0 0 1 12 7a4.3 4.3 0 0 1 8.5 2.8c0 5.7-8.5 10.7-8.5 10.7z',
    resources: 'M12 3 3 7l9 4 9-4-9-4zM3 12l9 4 9-4M3 17l9 4 9-4',
    usage: 'M4 20V10M10 20V4M16 20v-7M2 20h20',
  };

  function ovIcon(kind) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', OV_ICON_PATHS[kind] || OV_ICON_PATHS.resources);
    path.setAttribute('fill', 'none'); path.setAttribute('stroke', 'currentColor');
    path.setAttribute('stroke-width', '1.6'); path.setAttribute('stroke-linecap', 'round'); path.setAttribute('stroke-linejoin', 'round');
    svg.append(path);
    return svg;
  }

  function ovCard(iconKind, title) {
    const card = element('section', 'ov-card');
    const icon = element('span', 'ov-card-icon'); icon.append(ovIcon(iconKind));
    const body = element('div', 'ov-card-body');
    body.append(element('h4', 'ov-card-title', title));
    card.append(icon, body);
    return { card, body };
  }

  function renderOverview(panel, ctx) {
    const { app, view, selection, instanceView, selectedHealth } = ctx;
    if (selection) {
      const targets = applicationTargets(app) || [];
      if (targets.length > 1) {
        const picker = element('label', 'field instance-picker'); picker.append(element('span', '', 'Cluster instance'));
        const select = document.createElement('select'); select.setAttribute('aria-label', 'Select cluster instance');
        targets.forEach((target) => select.add(new Option(target.name, target.uid)));
        select.value = selection.target.uid;
        select.addEventListener('change', () => { state.selectedInstanceUID = select.value; renderApplicationDetail(); });
        picker.append(select); panel.append(picker);
      }
      if (!instanceView) panel.append(element('p', 'warning', `Waiting for ${safe(selection.target.name)} to report this application instance.`));
    }

    const health = selectedHealth?.workload || workloadHealth(view);
    const grid = element('div', 'overview-grid');
    const main = element('div', 'overview-main');
    const side = element('div', 'overview-side');

    const cards = element('div', 'ov-cards');
    cards.append(overviewHealthCard(health), overviewResourcesCard(view), overviewUsageCard(app));
    main.append(cards);
    main.append(overviewWorkloadsCard(app, view, health));
    main.append(overviewActivityCard(app));

    side.append(overviewQuickInfo(app, view));
    side.append(overviewValueOverrides(app));

    grid.append(main, side);
    panel.append(grid);
  }

  function overviewHealthCard(health) {
    const { card, body } = ovCard('health', 'Workload health');
    body.append(badge(health.label, health.tone));
    const valid = typeof health.observedAt === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(health.observedAt);
    if (valid) body.append(element('p', 'ov-card-note', `Last observed${health.fresh ? '' : ' (stale)'}: ${formatObservedTime(health.observedAt)}`));
    else body.append(element('p', 'ov-card-note', health.fresh ? 'Current' : 'Not current'));
    return card;
  }

  function resourceKindLabel(kind, count) {
    const irregular = { Ingress: 'Ingresses', NetworkPolicy: 'NetworkPolicies' };
    return count === 1 ? kind : (irregular[kind] || `${kind}s`);
  }

  function overviewResourcesCard(view) {
    const { card, body } = ovCard('resources', 'Resources');
    const resources = Array.isArray(view.status?.resources) ? view.status.resources : [];
    const counts = new Map();
    resources.forEach((resource) => counts.set(resource.kind, (counts.get(resource.kind) || 0) + 1));
    const total = element('div', 'ov-total');
    total.append(element('strong', '', String(resources.length)), element('span', '', 'total'));
    body.append(total);
    if (!resources.length) { body.append(element('p', 'ov-card-note', 'No resources reported')); return card; }
    const kinds = [...counts.keys()].sort((a, b) => (counts.get(b) - counts.get(a)) || a.localeCompare(b));
    const maxCells = 6;
    let cells = kinds;
    let other = 0;
    if (kinds.length > maxCells) { cells = kinds.slice(0, maxCells - 1); other = kinds.length - cells.length; }
    const grid = element('div', 'ov-counts');
    cells.forEach((kind) => {
      const count = counts.get(kind);
      const item = element('span', 'ov-count');
      item.append(element('strong', '', String(count)), document.createTextNode(` ${resourceKindLabel(kind, count)}`));
      grid.append(item);
    });
    if (other > 0) grid.append(element('span', 'ov-count ov-count-more', `+${other} other type${other === 1 ? '' : 's'}`));
    body.append(grid);
    return card;
  }

  function overviewUsageCard(app) {
    const { card, body } = ovCard('usage', 'Resource usage');
    body.append(element('p', 'ov-card-note', 'Loading…'));
    loadOverviewUsage(app.metadata?.name, body);
    return card;
  }

  async function loadOverviewUsage(name, body) {
    try {
      const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(name)}/metrics`));
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
      body.replaceChildren();
      if (!data.available) { body.append(element('p', 'ov-card-note', safe(data.reason || 'Metrics are not available.'))); return; }
      const stats = element('div', 'ov-stats');
      const cpu = element('div', 'ov-stat');
      cpu.append(element('strong', '', formatCPUmilli(data.totalCpuMilli)), element('span', '', data.requestedCpuMilli ? `of ${formatCPUmilli(data.requestedCpuMilli)} requested` : 'CPU in use'));
      const memory = element('div', 'ov-stat');
      memory.append(element('strong', '', formatBytes(data.totalMemoryBytes)), element('span', '', 'Memory in use'));
      stats.append(cpu, memory); body.append(stats);
    } catch (error) {
      body.replaceChildren(element('p', 'ov-card-note', error.message || 'Could not load usage.'));
    }
  }

  function ovSection(title, subtitle) {
    const section = element('section', 'ov-section');
    const head = element('div', 'ov-section-head');
    const titles = element('div', '');
    titles.append(element('h4', 'ov-section-title', title), element('p', 'ov-section-sub', subtitle));
    head.append(titles);
    section.append(head);
    return { section, head };
  }

  function resourceRollout(resource) {
    return resource.deployment || resource.statefulSet || resource.daemonSet || resource.job || null;
  }

  function resourceReadyText(resource) {
    if (resource.deployment) return resource.deployment.readyReplicas != null && resource.deployment.desiredReplicas != null ? `${resource.deployment.readyReplicas}/${resource.deployment.desiredReplicas}` : '—';
    if (resource.statefulSet) return resource.statefulSet.readyReplicas != null && resource.statefulSet.desiredReplicas != null ? `${resource.statefulSet.readyReplicas}/${resource.statefulSet.desiredReplicas}` : '—';
    if (resource.daemonSet) return resource.daemonSet.numberReady != null && resource.daemonSet.desiredNumberScheduled != null ? `${resource.daemonSet.numberReady}/${resource.daemonSet.desiredNumberScheduled}` : '—';
    if (resource.job) return resource.job.completions != null ? `${resource.job.succeeded ?? 0}/${resource.job.completions}` : '—';
    return '—';
  }

  function overviewWorkloadsCard(app, view, health) {
    const { section, head } = ovSection('Workloads', 'Top-level resources in this application.');
    const link = element('button', 'ov-link', 'View all resources →'); link.type = 'button';
    link.addEventListener('click', () => { state.detailTab = 'resources'; renderApplicationDetail(); });
    head.append(link);
    const resources = (Array.isArray(view.status?.resources) ? view.status.resources : []).filter((resource) => !resource.parentUID);
    if (!resources.length) { section.append(element('p', 'helper', 'No resources reported.')); return section; }
    const table = element('table', 'ov-table');
    const thead = document.createElement('thead'); const headRow = document.createElement('tr');
    ['Kind', 'Name', 'Status', 'Ready'].forEach((label) => headRow.append(element('th', '', label)));
    thead.append(headRow); table.append(thead);
    const tbody = document.createElement('tbody');
    resources.slice().sort((a, b) => (a.kind === b.kind ? String(a.name).localeCompare(String(b.name)) : String(a.kind).localeCompare(String(b.kind)))).forEach((resource) => {
      const row = document.createElement('tr');
      row.append(element('td', '', safe(resource.kind)), element('td', '', safe(resource.name)));
      const statusCell = element('td', '');
      const rollout = resourceRollout(resource);
      if (!rollout) statusCell.append(element('span', 'ov-dash', '—'));
      else if (!health.fresh) statusCell.append(badge('Unknown', 'neutral'));
      else { const state = workloadCardStatus(rollout.state); statusCell.append(badge(state.label, state.tone)); }
      row.append(statusCell, element('td', '', resourceReadyText(resource)));
      tbody.append(row);
    });
    table.append(tbody); section.append(table);
    return section;
  }

  function overviewActivityCard(app) {
    const { section, head } = ovSection('Recent activity', 'Recent events recorded for this application.');
    const link = element('button', 'ov-link', 'View all activity →'); link.type = 'button';
    link.addEventListener('click', () => { state.detailTab = 'activity'; renderApplicationDetail(); });
    head.append(link);
    const body = element('div', 'ov-activity'); body.append(element('p', 'helper', 'Loading…'));
    section.append(body);
    loadOverviewActivity(app.metadata?.name, body);
    return section;
  }

  async function loadOverviewActivity(name, body) {
    try {
      const response = await apiFetch(withTeam(`/api/activity?kind=Application&name=${encodeURIComponent(name)}`));
      if (!response.ok) throw new Error(`Request failed (${response.status})`);
      const data = await response.json();
      const items = (Array.isArray(data.items) ? data.items : []).slice(0, 5);
      if (!items.length) { body.replaceChildren(element('p', 'helper', 'No recorded activity yet.')); return; }
      const table = element('table', 'ov-table');
      const thead = document.createElement('thead'); const headRow = document.createElement('tr');
      ['Time', 'Type', 'Message'].forEach((label) => headRow.append(element('th', '', label)));
      thead.append(headRow); table.append(thead);
      const tbody = document.createElement('tbody');
      items.forEach((entry) => {
        const row = document.createElement('tr');
        row.append(element('td', '', formatObservedTime(entry.lastSeen)), element('td', '', safe(entry.type)), element('td', '', safe(entry.message || entry.reason || '')));
        tbody.append(row);
      });
      table.append(tbody); body.replaceChildren(table);
    } catch (error) {
      body.replaceChildren(element('p', 'helper', error.message || 'Could not load activity.'));
    }
  }

  function overviewQuickInfo(app, view) {
    const card = element('section', 'ov-card ov-card-block');
    card.append(element('h4', 'ov-card-title', 'Quick info'));
    const list = element('dl', 'ov-info');
    const infoRow = (label, value) => { const row = element('div', 'ov-info-row'); row.append(element('dt', '', label), element('dd', '', safe(value))); return row; };
    list.append(
      infoRow('Application', app.metadata?.name),
      infoRow('Target namespace', targetNamespace(app) || view.status?.binding?.releaseNamespace),
      infoRow('Cluster', app.spec?.clusterRef?.name || app.spec?.environment?.name),
      infoRow('Definition', app.spec?.applicationDefinitionRef?.name),
      infoRow('Created', app.metadata?.creationTimestamp ? formatObservedTime(app.metadata.creationTimestamp) : '—'),
      infoRow('Last updated', view.status?.workloadObservedAt ? formatObservedTime(view.status.workloadObservedAt) : '—'),
    );
    card.append(list);
    const labels = app.metadata?.labels || {};
    const keys = Object.keys(labels);
    if (keys.length) {
      const row = element('div', 'ov-info-row');
      row.append(element('dt', '', 'Labels'));
      const chips = element('dd', 'ov-chips');
      keys.forEach((key) => chips.append(element('span', 'ov-chip', `${key}=${labels[key]}`)));
      row.append(chips); card.append(row);
    }
    return card;
  }

  function overviewValueOverrides(app) {
    const card = element('section', 'ov-card ov-card-block');
    card.append(element('h4', 'ov-card-title', 'Value overrides'));
    const values = app.spec?.values;
    const has = values && typeof values === 'object' && Object.keys(values).length > 0;
    card.append(element('p', 'ov-card-note', has ? `${Object.keys(values).length} override${Object.keys(values).length === 1 ? '' : 's'}` : 'No value overrides'));
    const def = applicationDefinition(app);
    const available = Boolean(def && definitionReadiness(def).ready && Array.isArray(def.spec?.configurableValues) && def.spec.configurableValues.length);
    const button = element('button', 'button button-secondary', 'Edit values'); button.type = 'button';
    button.disabled = !state.canUpdateApps || !available;
    button.addEventListener('click', () => {
      if (button.disabled) return;
      state.detailTab = 'configuration'; state.editingValues = true; state.valuesDraft = Object.create(null); renderApplicationDetail();
    });
    card.append(button);
    if (!available) card.append(element('p', 'helper', 'The Application Definition is unavailable, so values cannot be edited.'));
    return card;
  }

  function selectionTargets(app) {
    return applicationTargets(app) || [];
  }

  function applicationDefinition(app) {
    const ref = app.spec?.applicationDefinitionRef?.name;
    if (!ref) return null;
    // ApplicationDefinitions are cluster-scoped, so names are unique.
    return state.definitions.find((definition) => definition.metadata?.name === ref) || null;
  }

  const CFG_ICON_PATHS = {
    document: 'M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8zM14 3v5h5',
    sliders: 'M4 6h10M18 6h2M4 12h4M12 12h8M4 18h12M20 18h0M16 4v4M8 10v4M18 16v4',
    code: 'M9 8l-4 4 4 4M15 8l4 4-4 4',
    info: 'M12 8h.01M11 12h1v4h1M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18',
    clock: 'M12 7v5l3 2M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18',
    copy: 'M9 9V5a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-4M5 9h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-8a2 2 0 0 1 2-2z',
    pencil: 'M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z',
  };

  function cfgIcon(kind) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', CFG_ICON_PATHS[kind] || CFG_ICON_PATHS.document);
    path.setAttribute('fill', 'none'); path.setAttribute('stroke', 'currentColor');
    path.setAttribute('stroke-width', '1.6'); path.setAttribute('stroke-linecap', 'round'); path.setAttribute('stroke-linejoin', 'round');
    svg.append(path);
    return svg;
  }

  function cfgCard(iconKind, title, subtitle) {
    const card = element('section', 'cfg-card');
    const head = element('div', 'cfg-card-head');
    const icon = element('span', 'cfg-card-icon'); icon.append(cfgIcon(iconKind));
    const titles = element('div', 'cfg-card-titles');
    titles.append(element('h4', 'cfg-card-title', title));
    if (subtitle) titles.append(element('p', 'cfg-card-sub', subtitle));
    head.append(icon, titles);
    card.append(head);
    return { card, head, titles };
  }

  function decodeHistory(app) {
    try {
      const raw = app.metadata?.annotations?.['platform.releaseyard.io/change-history'];
      const parsed = raw ? JSON.parse(raw) : [];
      return Array.isArray(parsed) ? parsed : [];
    } catch (error) { return []; }
  }

  function deepMerge(base, extra) {
    const result = { ...(base || {}) };
    Object.keys(extra || {}).forEach((key) => {
      const value = extra[key];
      if (value && typeof value === 'object' && !Array.isArray(value) && base?.[key] && typeof base[key] === 'object' && !Array.isArray(base[key])) {
        result[key] = deepMerge(base[key], value);
      } else {
        result[key] = value;
      }
    });
    return result;
  }

  function toYAML(value, indent = 0) {
    const pad = '  '.repeat(indent);
    if (value === null || value === undefined) return 'null';
    if (Array.isArray(value)) {
      if (!value.length) return '[]';
      return value.map((item) => {
        const rendered = toYAML(item, indent + 1);
        return typeof item === 'object' && item !== null ? `${pad}- ${rendered.trimStart()}` : `${pad}- ${rendered}`;
      }).join('\n');
    }
    if (typeof value === 'object') {
      const keys = Object.keys(value);
      if (!keys.length) return '{}';
      return keys.map((key) => {
        const item = value[key];
        if (item && typeof item === 'object') return `${pad}${key}:\n${toYAML(item, indent + 1)}`;
        if (typeof item === 'string' && /[:#]|^\s|\s$/.test(item)) return `${pad}${key}: ${JSON.stringify(item)}`;
        return `${pad}${key}: ${String(item)}`;
      }).join('\n');
    }
    if (typeof value === 'string' && /[:#]|^\s|\s$/.test(value)) return JSON.stringify(value);
    return String(value);
  }

  function renderConfigurationTab(panel, ctx) {
    const { app } = ctx;
    const def = applicationDefinition(app);
    if (state.editingValues && !state.canUpdateApps) { state.editingValues = false; state.valuesDraft = Object.create(null); }
    const layout = element('div', 'cfg-layout');
    const main = element('div', 'cfg-main');
    const side = element('div', 'cfg-side');
    main.append(cfgDefinitionCard(app, def));
    main.append(cfgOverridesCard(app, def));
    main.append(cfgYamlCard(app, def));
    side.append(cfgSummaryCard(app, def));
    side.append(cfgHistoryCard(app));
    layout.append(main, side);
    panel.append(layout);
  }

  function cfgDefinitionCard(app, def) {
    const { card } = cfgCard('document', 'Application definition', 'Source Helm chart and version used for this application.');
    const bar = element('div', 'cfg-def-bar');
    const item = (label, value) => { const node = element('div', 'cfg-def-item'); node.append(element('span', 'cfg-def-label', label), element('span', 'cfg-def-value', safe(value))); return node; };
    bar.append(item('Definition', app.spec?.applicationDefinitionRef?.name), item('Version', def?.spec?.chart?.version));
    card.append(bar);
    return card;
  }

  function cfgValueControl(config, path, current, def, editable) {
    const overrideValue = path.split('.').reduce((value, key) => value && typeof value === 'object' ? value[key] : undefined, current);
    const defaultValue = path.split('.').reduce((value, key) => value && typeof value === 'object' ? value[key] : undefined, def?.spec?.defaults);
    const effective = overrideValue !== undefined ? overrideValue : defaultValue;
    const draft = Object.prototype.hasOwnProperty.call(state.valuesDraft, path) ? state.valuesDraft[path] : (overrideValue !== undefined ? String(overrideValue) : '');
    const value = editable ? draft : (effective !== undefined ? String(effective) : '');
    let input;
    if (config.type === 'boolean') {
      input = document.createElement('select');
      input.add(new Option('Not set', '')); input.add(new Option('true', 'true')); input.add(new Option('false', 'false'));
      input.value = value;
    } else {
      input = document.createElement('input');
      input.type = config.type === 'integer' ? 'number' : 'text';
      if (config.type === 'integer') input.step = '1';
      if (config.minimum !== undefined) input.min = config.minimum;
      if (config.maximum !== undefined) input.max = config.maximum;
      input.value = value;
      input.placeholder = config.required ? 'Required' : (defaultValue !== undefined ? `Default: ${defaultValue}` : 'Optional');
    }
    input.dataset.path = path; input.dataset.type = config.type || 'string'; input.dataset.required = config.required ? 'true' : 'false';
    input.disabled = !editable;
    if (editable) {
      if (!Object.prototype.hasOwnProperty.call(state.valuesDraft, path) && overrideValue !== undefined) state.valuesDraft[path] = String(overrideValue);
      input.addEventListener('input', () => { state.valuesDraft[path] = input.value; input.setCustomValidity(''); });
      input.addEventListener('change', () => { state.valuesDraft[path] = input.value; input.setCustomValidity(''); });
      if (config.required) input.required = true;
    }
    return input;
  }

  function cfgOverridesCard(app, def) {
    const configs = Array.isArray(def?.spec?.configurableValues) ? def.spec.configurableValues : [];
    const available = Boolean(def && definitionReadiness(def).ready && configs.length);
    const { card, head } = cfgCard('sliders', 'Value overrides', 'Custom Helm values for this application. These values override the defaults from the application definition.');
    const editable = state.editingValues && state.canUpdateApps && available;
    const edit = element('button', 'button button-secondary cfg-btn'); edit.type = 'button';
    edit.append(cfgIcon('pencil'), document.createTextNode(' Edit values'));
    edit.disabled = !state.canUpdateApps || !available || state.deletionPending.has(app.metadata?.name);
    if (!editable) edit.addEventListener('click', () => {
      if (!state.canUpdateApps || !available || state.deletionPending.has(app.metadata?.name)) return;
      state.valuesDraft = Object.create(null); state.editingValues = true; renderApplicationDetail();
      $('#application-values-form input, #application-values-form select')?.focus();
    });
    head.append(edit);

    if (!configs.length) { card.append(element('p', 'helper', 'The Application Definition has no configurable values.')); return card; }

    const current = app.spec?.values || {};
    const form = element('form', 'cfg-values-form'); form.id = 'application-values-form';
    const table = element('table', 'cfg-table');
    const thead = document.createElement('thead'); const headRow = document.createElement('tr');
    ['Key', 'Value', ''].forEach((label) => headRow.append(element('th', '', label)));
    thead.append(headRow); table.append(thead);
    const tbody = document.createElement('tbody');
    configs.forEach((config, index) => {
      const path = String(config.path || `value-${index + 1}`);
      const row = document.createElement('tr'); row.className = 'cfg-row';
      const key = element('td', 'cfg-key'); key.append(element('span', '', path));
      if (config.required) key.append(element('span', 'cfg-required', 'required'));
      const valueCell = element('td', 'cfg-value'); valueCell.append(cfgValueControl(config, path, current, def, editable));
      const actions = element('td', 'cfg-row-actions'); actions.append(cfgRowMenu(path, config, def, app, editable));
      row.append(key, valueCell, actions); tbody.append(row);
    });
    table.append(tbody); form.append(table);
    if (editable) {
      const actions = element('div', 'cfg-values-actions');
      const hint = element('p', 'helper', 'Saving updates the configuration; the Helm release may take time to apply it.');
      const buttons = element('div', 'cfg-values-buttons');
      const cancel = element('button', 'button button-secondary', 'Cancel'); cancel.type = 'button';
      cancel.addEventListener('click', () => { state.editingValues = false; state.valuesDraft = Object.create(null); renderApplicationDetail(); });
      const save = element('button', 'button button-primary', state.savingValues ? 'Saving…' : 'Save values');
      save.type = 'submit'; save.disabled = state.savingValues;
      buttons.append(cancel, save); actions.append(hint, buttons); form.append(actions);
      form.addEventListener('submit', (event) => { event.preventDefault(); saveApplicationValues(app, form); });
    }
    card.append(form);
    return card;
  }

  function cfgRowMenu(path, config, def, app, editable) {
    const menu = document.createElement('details'); menu.className = 'detail-menu res-row-menu';
    const toggle = element('summary', 'detail-menu-toggle res-row-toggle');
    toggle.setAttribute('aria-label', 'Value actions'); toggle.title = 'Value actions';
    toggle.append(dotsIcon());
    const list = element('div', 'detail-menu-list');
    const copyKey = element('button', 'detail-menu-item', 'Copy key'); copyKey.type = 'button';
    copyKey.addEventListener('click', () => { menu.open = false; copyText(path); });
    list.append(copyKey);
    const defaultValue = path.split('.').reduce((value, key) => value && typeof value === 'object' ? value[key] : undefined, def?.spec?.defaults);
    if (editable) {
      const reset = element('button', 'detail-menu-item', 'Reset to default'); reset.type = 'button';
      reset.disabled = defaultValue === undefined;
      reset.addEventListener('click', () => { menu.open = false; state.valuesDraft[path] = defaultValue !== undefined ? String(defaultValue) : ''; renderApplicationDetail(); });
      list.append(reset);
    }
    menu.append(toggle, list);
    return menu;
  }

  function cfgYamlCard(app, def) {
    const { card, head } = cfgCard('code', 'YAML preview', 'Rendered values.yaml with your overrides. This is a preview of the values applied to the application.');
    const merged = deepMerge(def?.spec?.defaults || {}, app.spec?.values || {});
    const text = Object.keys(merged).length ? toYAML(merged) : '# No values apply to this application.';
    const copy = element('button', 'button button-secondary cfg-btn'); copy.type = 'button';
    copy.append(cfgIcon('copy'), document.createTextNode(' Copy'));
    copy.addEventListener('click', () => copyText(text));
    head.append(copy);
    const pre = element('div', 'cfg-yaml');
    text.split('\n').forEach((line, index) => {
      const row = element('div', 'cfg-yaml-line');
      row.append(element('span', 'cfg-yaml-ln', String(index + 1)), element('span', 'cfg-yaml-code', line));
      pre.append(row);
    });
    card.append(pre);
    return card;
  }

  function cfgSummaryCard(app, def) {
    const { card } = cfgCard('info', 'Configuration summary', null);
    const latest = decodeHistory(app)[0];
    const chart = def?.spec?.chart || {};
    const list = element('dl', 'cfg-info');
    const row = (label, valueNode) => { const item = element('div', 'cfg-info-row'); item.append(element('dt', '', label)); const dd = element('dd', ''); dd.append(valueNode); item.append(dd); list.append(item); };
    row('Source', element('span', '', 'Helm chart'));
    row('Repository', element('span', '', chart.repository || '—'));
    row('Chart', element('span', '', chart.name || '—'));
    row('Chart version', element('span', '', chart.version || '—'));
    row('Last updated', element('span', '', latest?.at ? formatObservedTime(latest.at) : (app.status?.workloadObservedAt ? formatObservedTime(app.status.workloadObservedAt) : '—')));
    row('Updated by', element('span', '', latest?.by || '—'));
    const validity = def ? definitionReadiness(def) : { ready: false, reason: 'The Application Definition is unavailable.' };
    const status = element('div', 'cfg-validity');
    status.append(badge(validity.ready ? 'Valid' : 'Not valid', validity.ready ? 'good' : 'muted'));
    status.append(element('p', 'cfg-validity-note', validity.ready ? 'Values are valid and can be applied.' : (validity.reason || 'Values cannot be applied.')));
    row('Validation status', status);
    card.append(list);
    return card;
  }

  function cfgHistoryCard(app) {
    const { card } = cfgCard('clock', 'Change history', 'Recent configuration changes for this application.');
    const entries = decodeHistory(app);
    if (!entries.length) { card.append(element('p', 'helper', 'No configuration changes recorded yet.')); return card; }
    const list = element('ol', 'cfg-history');
    entries.forEach((entry) => {
      const item = element('li', 'cfg-history-item');
      item.append(element('span', 'cfg-history-dot'));
      const body = element('div', 'cfg-history-body');
      const top = element('div', 'cfg-history-top');
      top.append(element('span', 'cfg-history-version', `v${entry.version}`));
      top.append(element('span', 'cfg-history-time', entry.at ? formatObservedTime(entry.at) : ''));
      body.append(top);
      body.append(element('p', 'cfg-history-summary', safe(entry.summary || 'Updated configuration')));
      if (entry.by) body.append(element('p', 'cfg-history-by', safe(entry.by)));
      item.append(body); list.append(item);
    });
    card.append(list);
    return card;
  }

  async function saveApplicationValues(app, formNode) {
    if (!state.canUpdateApps) {
      state.editingValues = false;
      state.valuesDraft = Object.create(null);
      renderApplicationDetail();
      return;
    }
    if (!formNode.reportValidity() || state.savingValues) return;
    const def = applicationDefinition(app);
    if (!def || !definitionReadiness(def).ready) { showNotice('The Application Definition is no longer validated. Refresh the list and try again.', 'error'); return; }
    const values = Object.create(null);
    for (const input of formNode.querySelectorAll('[data-path]')) {
      const value = input.value;
      if (value === '') {
        if (input.dataset.required === 'true') { input.focus(); showNotice(`Enter a value for “${input.dataset.path}”.`, 'error'); return; }
        continue;
      }
      let converted = value;
      if (input.dataset.type === 'boolean') converted = value === 'true';
      if (input.dataset.type === 'integer') {
        if (!/^-?\d+$/.test(value) || !Number.isSafeInteger(Number(value))) {
          input.setCustomValidity('Enter a valid, safe whole number.'); input.reportValidity();
          showNotice(`“${input.dataset.path}” must be a valid whole number.`, 'error'); return;
        }
        input.setCustomValidity(''); converted = Number(value);
      }
      setNested(values, input.dataset.path, converted);
    }
    state.savingValues = true; renderApplicationDetail();
    try {
      const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(app.metadata.name)}`), {
        method: 'PATCH', headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: JSON.stringify({ generation: app.metadata.generation, values })
      });
      const body = await response.json().catch(() => ({}));
      if (response.status === 409) throw new Error('This resource has changed. Refresh the list and try again.');
      if (!response.ok) throw new Error(body.error || `Save failed (${response.status})`);
      if (body?.metadata?.name) {
        state.selectedApplication = body;
        state.applications = state.applications.map((item) => item.metadata?.name === body.metadata.name ? body : item);
      }
      state.editingValues = false; state.valuesDraft = Object.create(null);
      const refreshed = await refreshApplications();
      if (refreshed) showNotice('Changes saved. Application update is in progress.', 'success');
    } catch (error) {
      showNotice(error.message || 'Could not save values. Try again.', 'error');
    } finally {
      state.savingValues = false;
      if (state.selectedApplication) renderApplicationDetail();
    }
  }

  function formatDateParts(value) {
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return { date: safe(value), time: '' };
    return {
      date: new Intl.DateTimeFormat('en', { dateStyle: 'medium' }).format(date),
      time: new Intl.DateTimeFormat('en', { timeStyle: 'short' }).format(date),
    };
  }

  function activityTypeLabel(kind) {
    const map = { ConfigMap: 'Config', PersistentVolumeClaim: 'PVC', ReplicaSet: 'ReplicaSet' };
    return map[kind] || kind || 'Event';
  }

  function activityTitle(entry) {
    const map = {
      ScalingReplicaSet: 'Deployment applied', SuccessfulCreate: 'Pod created', Started: 'Pod started',
      Pulled: 'Image pulled', Created: 'Application created', Killing: 'Pod stopped',
      ConfigMapUpdated: 'Config updated', SecretUpdated: 'Secret updated', FailedToRetrieveImagePullSecret: 'Image pull secret failed',
    };
    if (map[entry.reason]) return map[entry.reason];
    const humanized = String(entry.reason || '').replace(/([a-z0-9])([A-Z])/g, '$1 $2').replace(/[-_]+/g, ' ').trim();
    return humanized ? humanized.charAt(0).toUpperCase() + humanized.slice(1) : 'Event';
  }

  function activityIcon(kind) {
    if (['Deployment', 'ReplicaSet', 'StatefulSet', 'DaemonSet', 'Pod', 'Job'].includes(kind)) return resourceKindIcon();
    return cfgIcon('document');
  }

  function activityStatusBadge(entry) {
    if (entry.type === 'Warning') return badge('Warning', 'error');
    if (/health|ready|healthy|completed|scaled|success/i.test(entry.reason || '') || entry.objectKind === 'Application') return badge('Healthy', 'good');
    return badge('Info', 'neutral');
  }

  function actKpi(iconKind, title, opts = {}) {
    const card = element('section', 'act-kpi');
    const head = element('div', 'act-kpi-head');
    const icon = element('span', 'act-kpi-icon'); icon.append(obsIcon(iconKind));
    head.append(icon, element('span', 'act-kpi-title', title));
    const value = element('div', 'act-kpi-value', '—');
    const sub = element('div', 'act-kpi-sub', '');
    card.append(head, value, sub);
    if (opts.chevron) card.append(element('span', 'act-kpi-chevron', '›'));
    return { card, value, sub };
  }

  function actSelect(label, options, value, onChange) {
    const field = element('label', 'act-field');
    field.append(element('span', 'act-field-label', label));
    const select = document.createElement('select'); select.className = 'act-select';
    options.forEach(([val, text]) => select.add(new Option(text, val)));
    select.value = value;
    select.addEventListener('change', () => onChange(select.value));
    field.append(select);
    return field;
  }

  function renderActivityTab(panel, ctx) {
    const { app } = ctx;
    const name = app.metadata?.name;
    if (!name) return;
    const section = element('section', 'act-section');

    const kpis = element('div', 'act-kpis');
    const eventsKpi = actKpi('document', '24h events');
    const warningsKpi = actKpi('events', 'Warnings');
    const deployKpi = actKpi('restart', 'Last deployment', { chevron: true });
    kpis.append(eventsKpi.card, warningsKpi.card, deployKpi.card);
    section.append(kpis);

    const filters = element('div', 'act-filters');
    const rangeField = actSelect('Time range', [['1h', 'Last 1 hour'], ['6h', 'Last 6 hours'], ['24h', 'Last 24 hours'], ['7d', 'Last 7 days'], ['all', 'All time']], state.activityRange, (value) => { state.activityRange = value; redraw(); });
    const typeField = actSelect('Event type', [['', 'All types'], ['Normal', 'Normal'], ['Warning', 'Warning']], state.activityType, (value) => { state.activityType = value; redraw(); });
    const sourceSelect = actSelect('Source', [['', 'All sources']], state.activitySource, (value) => { state.activitySource = value; redraw(); });
    const sourceField = sourceSelect;
    const search = element('input', 'act-search'); search.type = 'search'; search.placeholder = 'Search events...'; search.setAttribute('aria-label', 'Search events');
    search.value = state.activityQuery;
    search.addEventListener('input', () => { state.activityQuery = search.value; redraw(); });
    filters.append(rangeField, typeField, sourceField, search);
    section.append(filters);

    const grid = element('div', 'act-grid');
    const timelineCard = element('section', 'act-card');
    const timelineHead = element('div', 'act-card-head');
    const timelineIcon = element('span', 'act-card-icon'); timelineIcon.append(cfgIcon('clock'));
    timelineHead.append(timelineIcon, element('h4', 'act-card-title', 'Activity timeline'));
    const timeline = element('ul', 'act-timeline'); timeline.append(element('li', 'act-empty', 'Loading…'));
    timelineCard.append(timelineHead, timeline);

    const eventsCard = element('section', 'act-card');
    const eventsHead = element('div', 'act-card-head');
    const eventsIcon = element('span', 'act-card-icon'); eventsIcon.append(obsIcon('events'));
    const eventsTitle = element('h4', 'act-card-title', 'Events (0)');
    eventsHead.append(eventsIcon, eventsTitle);
    const eventsBody = element('div', 'act-events'); eventsBody.append(element('p', 'helper', 'Loading…'));
    eventsCard.append(eventsHead, eventsBody);
    grid.append(timelineCard, eventsCard);
    section.append(grid);
    panel.append(section);

    let items = [];

    function cutoff() {
      const now = Date.now();
      const map = { '1h': 3600e3, '6h': 6 * 3600e3, '24h': 24 * 3600e3, '7d': 7 * 24 * 3600e3 };
      return map[state.activityRange] ? now - map[state.activityRange] : 0;
    }

    function filtered() {
      const query = state.activityQuery.trim().toLowerCase();
      const since = cutoff();
      return items.filter((entry) => {
        if (since && (!entry.lastSeen || Date.parse(entry.lastSeen) < since)) return false;
        if (state.activityType && entry.type !== state.activityType) return false;
        if (state.activitySource && (entry.source || '') !== state.activitySource) return false;
        if (query) {
          const hay = `${entry.reason || ''} ${entry.message || ''} ${entry.objectName || ''} ${entry.objectKind || ''}`.toLowerCase();
          if (!hay.includes(query)) return false;
        }
        return true;
      });
    }

    function redraw() {
      const shown = filtered();
      eventsTitle.textContent = `Events (${shown.length})`;
      if (!shown.length) {
        timeline.replaceChildren(element('li', 'act-empty', items.length ? 'No events match these filters.' : 'No events recorded yet.'));
        eventsBody.replaceChildren(element('p', 'helper', items.length ? 'No events match these filters.' : 'No events recorded yet.'));
        return;
      }
      timeline.replaceChildren(...shown.map(activityTimelineItem));
      eventsBody.replaceChildren(activityEventsTable(shown));
    }

    loadActivityEvents(app, { eventsKpi, warningsKpi, deployKpi, sourceSelect: sourceSelect.querySelector('select'), onLoaded: (loaded) => { items = loaded; redraw(); } });
  }

  function activityTimelineItem(entry) {
    const item = element('li', 'act-tl-item');
    const parts = formatDateParts(entry.lastSeen);
    const time = element('div', 'act-tl-time');
    time.append(element('span', '', parts.date), element('span', '', parts.time));
    const dot = element('span', `act-tl-dot${entry.type === 'Warning' ? ' is-warning' : ''}`);
    dot.append(activityIcon(entry.objectKind));
    const body = element('div', 'act-tl-body');
    const title = element('h5', 'act-tl-title', activityTitle(entry));
    if (entry.count > 1) title.append(element('span', 'act-tl-count', `×${entry.count}`));
    body.append(title, element('p', 'act-tl-desc', safe(entry.message || entry.objectName || '')));
    item.append(time, dot, body);
    return item;
  }

  function activityEventsTable(items) {
    const table = element('table', 'act-table');
    const thead = document.createElement('thead'); const headRow = document.createElement('tr');
    ['Time ↓', 'Type', 'Source', 'Message', 'Status'].forEach((label) => headRow.append(element('th', '', label)));
    thead.append(headRow); table.append(thead);
    const tbody = document.createElement('tbody');
    items.forEach((entry) => {
      const row = document.createElement('tr');
      const timeCell = element('td', 'act-time', safe(formatObservedTime(entry.lastSeen)));
      const typeCell = element('td', '');
      const typeWrap = element('span', 'act-type'); typeWrap.append(element('span', 'act-type-icon'), document.createTextNode(activityTypeLabel(entry.objectKind)));
      typeWrap.firstChild.append(activityIcon(entry.objectKind));
      typeCell.append(typeWrap);
      const statusCell = element('td', ''); statusCell.append(activityStatusBadge(entry));
      row.append(timeCell, typeCell, element('td', '', safe(entry.source || '—')), element('td', 'act-msg', safe(entry.message || '')), statusCell);
      tbody.append(row);
    });
    table.append(tbody);
    return table;
  }

  async function loadActivityEvents(app, refs) {
    const name = app.metadata?.name;
    let items = [];
    try {
      const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(name)}/events`));
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
      items = Array.isArray(data.items) ? data.items : [];
    } catch (error) {
      refs.eventsKpi.sub.textContent = error.message || 'Could not load events.';
      refs.onLoaded([]);
      return;
    }
    const now = Date.now();
    const within24 = items.filter((entry) => entry.lastSeen && now - Date.parse(entry.lastSeen) <= 24 * 3600e3);
    refs.eventsKpi.value.textContent = String(within24.length);
    refs.eventsKpi.sub.textContent = 'in last 24h';
    refs.warningsKpi.value.textContent = String(within24.filter((entry) => entry.type === 'Warning').length);
    refs.warningsKpi.sub.textContent = 'in last 24h';
    const deployments = items.filter((entry) => entry.objectKind === 'Deployment').sort((a, b) => String(b.lastSeen || '').localeCompare(String(a.lastSeen || '')));
    if (deployments.length) {
      const parts = formatDateParts(deployments[0].lastSeen);
      refs.deployKpi.value.textContent = `${parts.date}, ${parts.time}`;
      refs.deployKpi.sub.textContent = deployments[0].objectName || '';
    } else {
      refs.deployKpi.value.textContent = '—';
      refs.deployKpi.sub.textContent = 'No deployment events';
    }
    const sources = [...new Set(items.map((entry) => entry.source).filter(Boolean))].sort();
    const sourceSelect = refs.sourceSelect;
    sourceSelect.replaceChildren(new Option('All sources', ''), ...sources.map((source) => new Option(source, source)));
    sourceSelect.value = state.activitySource;
    refs.onLoaded(items);
  }

  const OBS_ICON_PATHS = {
    cpu: 'M9 9h6v6H9zM4 9h2M4 15h2M18 9h2M18 15h2M9 4v2M15 4v2M9 18v2M15 18v2M7 7h10v10H7z',
    memory: 'M12 4c4 0 7 1.3 7 3s-3 3-7 3-7-1.3-7-3 3-3 7-3zM5 7v10c0 1.7 3 3 7 3s7-1.3 7-3V7M5 12c0 1.7 3 3 7 3s7-1.3 7-3',
    restart: 'M21 12a9 9 0 1 1-3-6.7M21 4v5h-5',
    pod: 'M12 3 21 8v8l-9 5-9-5V8l9-5Zm0 0v10m9-5-9 5-9-5',
    logs: 'M8 6h9M8 10h9M8 14h6M5 4h14v16H5z',
    events: 'M12 3a6 6 0 0 0-6 6c0 4-1.5 5-2 5h16c-.5 0-2-1-2-5a6 6 0 0 0-6-6zM10 18a2 2 0 0 0 4 0',
  };

  function obsIcon(kind) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', OBS_ICON_PATHS[kind] || OBS_ICON_PATHS.pod);
    path.setAttribute('fill', 'none'); path.setAttribute('stroke', 'currentColor');
    path.setAttribute('stroke-width', '1.6'); path.setAttribute('stroke-linecap', 'round'); path.setAttribute('stroke-linejoin', 'round');
    svg.append(path);
    return svg;
  }

  function formatCPUmilli(milli) {
    const value = Number(milli) || 0;
    return value >= 1000 ? `${(value / 1000).toFixed(2)} cores` : `${value}m`;
  }

  function formatBytes(bytes) {
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    let value = Number(bytes) || 0; let index = 0;
    while (value >= 1024 && index < units.length - 1) { value /= 1024; index += 1; }
    return `${value.toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
  }

  function percentOf(used, total) {
    return total > 0 ? `${Math.round((used / total) * 100)}%` : '';
  }

  function obsCard(iconKind, title, bodyNode, opts = {}) {
    const card = element('section', 'obs-card');
    const head = element('div', 'obs-card-head');
    const icon = element('span', 'obs-card-icon'); icon.append(obsIcon(iconKind));
    const titles = element('div', 'obs-card-titles');
    titles.append(element('h4', 'obs-card-title', title));
    head.append(icon, titles);
    if (opts.link) {
      const link = element('button', 'obs-link', `${opts.link} →`); link.type = 'button';
      link.addEventListener('click', opts.onClick);
      head.append(link);
    }
    card.append(head, bodyNode);
    return card;
  }

  function obsKpi(iconKind, title) {
    const card = element('section', 'obs-kpi');
    const head = element('div', 'obs-kpi-head');
    const icon = element('span', 'obs-kpi-icon'); icon.append(obsIcon(iconKind));
    head.append(icon, element('span', 'obs-kpi-title', title));
    const value = element('div', 'obs-kpi-value', '—');
    const sub = element('div', 'obs-kpi-sub', '');
    card.append(head, value, sub);
    return { card, value, sub };
  }

  function renderObservabilityTab(panel, ctx) {
    const { app } = ctx;
    const name = app.metadata?.name;
    if (!name) return;
    const section = element('section', 'obs-section');

    const kpis = element('div', 'obs-kpis');
    const cpu = obsKpi('cpu', 'CPU');
    const memory = obsKpi('memory', 'Memory');
    const restarts = obsKpi('restart', 'Restarts');
    kpis.append(cpu.card, memory.card, restarts.card);
    section.append(kpis);

    const usageBody = element('div', 'obs-card-body');
    section.append(obsCard('pod', 'Pod usage', usageBody, { link: 'View all pods', onClick: () => { state.detailTab = 'resources'; renderApplicationDetail(); } }));

    const logs = obsLogsCard(app);
    const events = obsEventsCard(app);
    const grid = element('div', 'obs-grid');
    grid.append(logs.card, events.card);
    section.append(grid);

    panel.append(section);
    loadObservability(app, { cpu, memory, restarts, usageBody, logs });
  }

  async function loadObservability(app, refs) {
    const name = app.metadata?.name;
    let metrics = null;
    try {
      const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(name)}/metrics`));
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
      metrics = data;
      if (data.available) {
        refs.cpu.value.textContent = formatCPUmilli(data.totalCpuMilli);
        refs.cpu.sub.textContent = data.requestedCpuMilli ? `${percentOf(data.totalCpuMilli, data.requestedCpuMilli)} of ${formatCPUmilli(data.requestedCpuMilli)} requested` : 'CPU in use';
        refs.memory.value.textContent = formatBytes(data.totalMemoryBytes);
        refs.memory.sub.textContent = data.requestedMemoryBytes ? `${percentOf(data.totalMemoryBytes, data.requestedMemoryBytes)} of ${formatBytes(data.requestedMemoryBytes)} requested` : 'Memory in use';
      } else {
        refs.cpu.value.textContent = '—'; refs.cpu.sub.textContent = data.reason || 'Metrics are not available.';
        refs.memory.value.textContent = '—'; refs.memory.sub.textContent = data.reason || 'Metrics are not available.';
      }
    } catch (error) {
      refs.cpu.value.textContent = '—'; refs.cpu.sub.textContent = error.message || 'Could not load metrics.';
      refs.memory.value.textContent = '—'; refs.memory.sub.textContent = error.message || 'Could not load metrics.';
    }

    let pods = [];
    try {
      const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(name)}/resources`));
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
      pods = (Array.isArray(data.items) ? data.items : []).filter((item) => item.kind === 'Pod');
    } catch (error) {
      refs.usageBody.replaceChildren(element('p', 'helper', error.message || 'Could not load Pods.'));
      refs.logs.setPods([]);
      return;
    }
    const totalRestarts = pods.reduce((sum, pod) => sum + (pod.restarts || 0), 0);
    refs.restarts.value.textContent = String(totalRestarts);
    refs.restarts.sub.textContent = `${pods.length} Pod${pods.length === 1 ? '' : 's'} in this release`;
    renderPodUsage(refs.usageBody, pods, metrics);
    refs.logs.setPods(pods);
  }

  function renderPodUsage(body, pods, metrics) {
    body.replaceChildren();
    if (!pods.length) { body.append(element('p', 'helper', 'No Pods reported for this release.')); return; }
    const usage = new Map((metrics?.pods || []).map((entry) => [entry.pod, entry]));
    const table = element('table', 'obs-table');
    const thead = document.createElement('thead'); const headRow = document.createElement('tr');
    ['Pod', 'Status', 'CPU', 'Memory', 'Restarts', 'Age', ''].forEach((label) => headRow.append(element('th', '', label)));
    thead.append(headRow); table.append(thead);
    const tbody = document.createElement('tbody');
    pods.slice().sort((a, b) => String(a.name).localeCompare(String(b.name))).forEach((pod) => {
      const row = document.createElement('tr');
      const nameCell = element('td', 'obs-pod-cell');
      nameCell.append(element('span', 'obs-kind-icon'), safe(pod.name));  // placeholder icon added below
      nameCell.firstChild.append(resourceKindIcon());
      const statusCell = element('td', ''); statusCell.append(badge(pod.status || '—', pod.statusTone || 'neutral'));
      const entry = usage.get(pod.name);
      row.append(
        nameCell, statusCell,
        element('td', '', entry ? formatCPUmilli(entry.cpuMilli) : '—'),
        element('td', '', entry ? formatBytes(entry.memoryBytes) : '—'),
        element('td', '', String(pod.restarts || 0)),
        element('td', '', pod.createdAt ? ageText(pod.createdAt) : '—'),
      );
      const actions = element('td', 'obs-row-actions'); actions.append(resourceRowMenu(pod)); row.append(actions);
      tbody.append(row);
    });
    table.append(tbody); body.append(table);
    if (!metrics?.available) body.append(element('p', 'helper', safe(metrics?.reason || 'Pod CPU and memory are unavailable.')));
  }

  function obsLogsCard(app) {
    const card = element('section', 'obs-card');
    const head = element('div', 'obs-card-head');
    const icon = element('span', 'obs-card-icon'); icon.append(obsIcon('logs'));
    const titles = element('div', 'obs-card-titles'); titles.append(element('h4', 'obs-card-title', 'Logs'));
    head.append(icon, titles);
    card.append(head);

    const podSelect = document.createElement('select'); podSelect.className = 'obs-select'; podSelect.setAttribute('aria-label', 'Pod');
    const containerSelect = document.createElement('select'); containerSelect.className = 'obs-select'; containerSelect.setAttribute('aria-label', 'Container');
    const search = element('input', 'obs-search'); search.type = 'search'; search.placeholder = 'Search logs...'; search.setAttribute('aria-label', 'Search logs');
    const controls = element('div', 'obs-controls');
    const field = (label, control) => { const wrap = element('label', 'obs-field'); wrap.append(element('span', 'obs-field-label', label), control); return wrap; };
    controls.append(field('Pod', podSelect), field('Container', containerSelect), search);
    card.append(controls);

    const output = element('div', 'obs-logs');
    output.append(element('p', 'helper', 'Select a Pod to load logs.'));
    card.append(output);

    const podState = { pods: [], containers: new Map(), lines: [], loadedPod: '' };

    const redraw = () => redrawLogs(output, podState.lines, search.value);

    const load = async () => {
      const pod = podSelect.value;
      if (!pod) { output.replaceChildren(element('p', 'helper', 'Select a Pod to load logs.')); return; }
      podState.loadedPod = pod;
      output.replaceChildren(element('p', 'helper', 'Loading…'));
      try {
        const container = containerSelect.value;
        const params = container ? `?container=${encodeURIComponent(container)}` : '';
        const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(app.metadata?.name)}/pods/${encodeURIComponent(pod)}/logs${params}`));
        const data = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
        podState.lines = (data.logs ? String(data.logs).replace(/\n$/, '') : '').split('\n').filter((line, index, all) => !(all.length === 1 && line === ''));
        redraw();
      } catch (error) {
        output.replaceChildren(element('p', 'helper', error.message || 'Could not load logs.'));
      }
    };

    podSelect.addEventListener('change', () => {
      const containers = podState.containers.get(podSelect.value) || [];
      containerSelect.replaceChildren(new Option('All containers', ''), ...containers.map((name) => new Option(name, name)));
      load();
    });
    containerSelect.addEventListener('change', load);
    search.addEventListener('input', redraw);

    return {
      card, output,
      setPods(pods) {
        podState.pods = pods;
        podState.containers = new Map(pods.map((pod) => [pod.name, Array.isArray(pod.containers) ? pod.containers : []]));
        podSelect.replaceChildren(...pods.map((pod) => new Option(pod.name, pod.name)));
        if (!pods.length) { podSelect.replaceChildren(new Option('No Pods', '')); containerSelect.replaceChildren(new Option('All containers', '')); output.replaceChildren(element('p', 'helper', 'No Pods reported for this release.')); return; }
        const containers = podState.containers.get(podSelect.value) || [];
        containerSelect.replaceChildren(new Option('All containers', ''), ...containers.map((name) => new Option(name, name)));
        load();
      },
    };
  }

  function redrawLogs(container, lines, filter) {
    const needle = String(filter || '').trim().toLowerCase();
    const shown = lines.filter((line) => !needle || line.toLowerCase().includes(needle));
    container.replaceChildren();
    if (!shown.length) { container.append(element('p', 'helper', lines.length ? 'No log lines match the search.' : 'No log output.')); return; }
    shown.forEach((line, index) => {
      const row = element('div', 'obs-log-line');
      row.append(element('span', 'obs-log-num', String(index + 1)));
      const code = element('span', 'obs-log-code');
      const match = line.match(/\b(INFO|WARN|WARNING|ERROR|ERRO|DEBUG|TRACE|FATAL|PANIC)\b/i);
      if (match) {
        const at = match.index;
        code.append(document.createTextNode(line.slice(0, at)));
        code.append(element('span', `obs-log-level lvl-${match[1].toLowerCase()}`, match[1]));
        code.append(document.createTextNode(line.slice(at + match[1].length)));
      } else {
        code.textContent = line;
      }
      row.append(code); container.append(row);
    });
  }

  function obsEventsCard(app) {
    const body = element('div', 'obs-card-body');
    const card = obsCard('events', 'Events', body, { link: 'View all activity', onClick: () => { state.detailTab = 'activity'; renderApplicationDetail(); } });
    const list = element('div', 'obs-events'); list.append(element('p', 'helper', 'Loading…'));
    body.append(list);
    loadObservabilityEvents(app.metadata?.name, list);
    return { card };
  }

  async function loadObservabilityEvents(name, container) {
    try {
      const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(name)}/events`));
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
      const items = Array.isArray(data.items) ? data.items : [];
      if (!container.isConnected) return;
      if (!items.length) { container.replaceChildren(element('p', 'helper', 'No events recorded for this release.')); return; }
      const table = element('table', 'obs-table obs-events-table');
      const thead = document.createElement('thead'); const headRow = document.createElement('tr');
      ['Time', 'Type', 'Reason', 'Message'].forEach((label) => headRow.append(element('th', '', label)));
      thead.append(headRow); table.append(thead);
      const tbody = document.createElement('tbody');
      items.slice(0, 40).forEach((entry) => {
        const row = document.createElement('tr');
        row.append(
          element('td', 'obs-event-time', entry.lastSeen ? ageText(entry.lastSeen) + ' ago' : '—'),
          element('td', '', safe(entry.type || 'Normal')),
          element('td', '', safe(entry.reason || '')),
          element('td', 'obs-event-msg', safe(entry.message || '')),
        );
        tbody.append(row);
      });
      table.append(tbody); container.replaceChildren(table);
    } catch (error) {
      if (container.isConnected) container.replaceChildren(element('p', 'helper', error.message || 'Could not load events.'));
    }
  }

  function resourceNodes(view) {
    const resources = Array.isArray(view.status?.resources) ? view.status.resources : [];
    const nodes = resources.map((resource) => ({ resource, children: [], parent: null }));
    const byUID = new Map();
    nodes.forEach((node) => {
      const uid = node.resource?.uid;
      if (uid && !byUID.has(uid)) byUID.set(uid, node);
    });
    nodes.forEach((node) => {
      const parent = node.resource?.parentUID ? byUID.get(node.resource.parentUID) : null;
      if (!parent || parent === node) return;
      node.parent = parent;
      parent.children.push(node);
    });
    // Detach any cycle edges before rendering; malformed input stays visible.
    nodes.forEach((node) => {
      const seen = new Set([node]);
      let cursor = node.parent;
      while (cursor && !seen.has(cursor)) { seen.add(cursor); cursor = cursor.parent; }
      if (cursor) {
        const oldParent = node.parent;
        oldParent.children = oldParent.children.filter((child) => child !== node);
        node.parent = null;
      }
    });
    return nodes;
  }

  const RES_KIND_ICON = 'M12 3 21 8v8l-9 5-9-5V8l9-5Zm0 0v10m9-5-9 5-9-5';

  function resourceKindIcon() {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', RES_KIND_ICON);
    path.setAttribute('fill', 'none'); path.setAttribute('stroke', 'currentColor');
    path.setAttribute('stroke-width', '1.5'); path.setAttribute('stroke-linecap', 'round'); path.setAttribute('stroke-linejoin', 'round');
    svg.append(path);
    return svg;
  }

  function resourceChevron(hasChildren, open) {
    const span = element('span', `res-chevron${open ? ' is-open' : ''}${hasChildren ? '' : ' is-leaf'}`);
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', 'M9 6l6 6-6 6');
    path.setAttribute('fill', 'none'); path.setAttribute('stroke', 'currentColor');
    path.setAttribute('stroke-width', '1.8'); path.setAttribute('stroke-linecap', 'round'); path.setAttribute('stroke-linejoin', 'round');
    svg.append(path); span.append(svg);
    return span;
  }

  function ageText(value) {
    const timestamp = Date.parse(value);
    if (!Number.isFinite(timestamp)) return '—';
    const seconds = Math.max(0, Math.floor((Date.now() - timestamp) / 1000));
    if (seconds < 60) return `${seconds}s`;
    const minutes = Math.floor(seconds / 60); if (minutes < 60) return `${minutes}m`;
    const hours = Math.floor(minutes / 60); if (hours < 24) return `${hours}h`;
    const days = Math.floor(hours / 24); if (days < 30) return `${days}d`;
    const months = Math.floor(days / 30); if (months < 12) return `${months}mo`;
    return `${Math.floor(months / 12)}y`;
  }

  function inventoryResourceBadge(resource, fresh) {
    const rollout = resourceRollout(resource);
    if (rollout) { if (!fresh) return badge('Unknown', 'neutral'); const state = workloadCardStatus(rollout.state); return badge(state.label, state.tone); }
    if (resource.pod) {
      if (!fresh) return badge('Unknown', 'neutral');
      if (resource.pod.ready === 'True') return badge('Running', 'good');
      const state = workloadCardStatus(resource.pod.state); return badge(state.label, state.tone);
    }
    return badge('—', 'neutral');
  }

  async function copyText(value) {
    const text = String(value ?? '');
    if (!text) return;
    try { await navigator.clipboard.writeText(text); showNotice('Copied to clipboard', 'success'); }
    catch { showNotice('Could not copy to the clipboard.', 'error'); }
  }

  function resourceRowMenu(resource) {
    const menu = document.createElement('details'); menu.className = 'detail-menu res-row-menu';
    const toggle = element('summary', 'detail-menu-toggle res-row-toggle');
    toggle.setAttribute('aria-label', 'Resource actions'); toggle.title = 'Resource actions';
    toggle.append(dotsIcon());
    const list = element('div', 'detail-menu-list');
    const copyName = element('button', 'detail-menu-item', 'Copy name'); copyName.type = 'button';
    copyName.addEventListener('click', () => { menu.open = false; copyText(resource.name || ''); });
    const copyPath = element('button', 'detail-menu-item', 'Copy kind/name'); copyPath.type = 'button';
    copyPath.addEventListener('click', () => { menu.open = false; copyText(`${resource.kind || ''}/${resource.name || ''}`); });
    list.append(copyName, copyPath);
    menu.append(toggle, list);
    return menu;
  }

  function appendResourcesTab(panel, ctx) {
    const { app, view, selectedHealth } = ctx;
    const nodes = resourceNodes(view);
    const fresh = Boolean(selectedHealth?.workload?.fresh ?? workloadHealth(view).fresh);
    const layout = element('div', 'res-layout');
    const section = element('section', 'res-section');

    const head = element('div', 'res-head');
    const titles = element('div', 'res-head-titles');
    const title = element('h3', 'res-title');
    title.append(document.createTextNode('Resources '), element('span', 'res-total', `(${nodes.length} total)`));
    titles.append(title, element('p', 'res-sub', 'Browse and explore all Kubernetes resources for this application.'));
    head.append(titles);

    const toolbar = element('div', 'res-toolbar');
    const search = element('input', 'res-search'); search.type = 'search'; search.placeholder = 'Search resources...';
    search.setAttribute('aria-label', 'Search resources'); search.value = state.resourcesQuery;
    const kindSelect = document.createElement('select'); kindSelect.className = 'res-select'; kindSelect.setAttribute('aria-label', 'Filter by kind');
    const statusSelect = document.createElement('select'); statusSelect.className = 'res-select'; statusSelect.setAttribute('aria-label', 'Filter by status');
    const refresh = element('button', 'button button-secondary res-refresh', 'Refresh'); refresh.type = 'button';
    toolbar.append(search, kindSelect, statusSelect, refresh);
    head.append(toolbar);
    section.append(head);

    if (selectedHealth && !selectedHealth.fresh) section.append(element('p', 'warning', 'Showing last-known resources. Instance health and replica counts are not current.'));
    if (condition(app, 'InventoryReady')?.status === 'False') section.append(element('p', 'warning', 'Resource inventory is not current and may be incomplete or outdated.'));
    if (!nodes.length) {
      section.append(element('p', 'empty', 'No resources recorded.'));
      layout.append(section); panel.append(layout); return;
    }

    const selectedNode = nodes.find((node) => node.resource?.uid && node.resource.uid === state.selectedResourceUID) || null;
    if (!selectedNode) state.selectedResourceUID = '';

    kindSelect.append(new Option('All kinds', ''));
    [...new Set(nodes.map((node) => node.resource.kind).filter(Boolean))].sort().forEach((kind) => kindSelect.add(new Option(kind, kind)));
    kindSelect.value = state.resourcesKind;
    statusSelect.append(new Option('All statuses', '')); statusSelect.value = state.resourcesStatus;

    const table = element('table', 'res-table');
    const thead = document.createElement('thead'); const headRow = document.createElement('tr');
    ['Kind', 'Name', 'Status', 'Ready', 'Age', ''].forEach((label, index) => headRow.append(element('th', index === 0 ? 'res-th-kind' : index === 5 ? 'res-th-actions' : '', label)));
    thead.append(headRow); table.append(thead);
    const tbody = document.createElement('tbody'); table.append(tbody);
    section.append(table);

    const rows = [];
    const parentOf = new Map();
    nodes.forEach((node) => { if (node.resource.uid) parentOf.set(node.resource.uid, node.parent?.resource.uid || ''); });

    const renderRows = (list, depth) => list.forEach((node) => {
      const resource = node.resource || {};
      const row = document.createElement('tr');
      row.className = `res-row${resource.uid && resource.uid === state.selectedResourceUID ? ' is-selected' : ''}`;
      if (resource.uid) row.dataset.uid = resource.uid;

      const kindCell = element('td', 'res-td-kind');
      const kindWrap = element('span', 'res-kind-cell'); kindWrap.style.paddingLeft = `${depth * 20}px`;
      const hasChildren = node.children.length > 0;
      const chevron = resourceChevron(hasChildren, state.resourcesExpanded.has(resource.uid));
      if (hasChildren) {
        chevron.setAttribute('role', 'button'); chevron.tabIndex = 0;
        chevron.setAttribute('aria-label', `Toggle ${resource.name || 'resource'}`);
        chevron.addEventListener('click', () => {
          if (state.resourcesExpanded.has(resource.uid)) state.resourcesExpanded.delete(resource.uid); else state.resourcesExpanded.add(resource.uid);
          applyVisibility();
        });
      }
      const icon = element('span', 'res-kind-icon'); icon.append(resourceKindIcon());
      kindWrap.append(chevron, icon, element('span', 'res-kind', safe(resource.kind || 'Resource')));
      kindCell.append(kindWrap); row.append(kindCell);

      row.append(element('td', 'res-td-name', safe(resource.name || '—')));

      const live = state.resourcesLive?.get(resource.uid);
      const statusCell = element('td', 'res-td-status');
      const statusBadge = live ? badge(live.status || '—', live.statusTone || 'neutral') : inventoryResourceBadge(resource, fresh);
      statusCell.append(statusBadge); row.dataset.status = statusBadge.textContent || '';
      row.append(statusCell);

      row.append(element('td', 'res-td-ready', live ? (live.ready || '—') : resourceReadyText(resource)));
      row.append(element('td', 'res-td-age', live?.createdAt ? ageText(live.createdAt) : '—'));

      const actionsCell = element('td', 'res-td-actions'); actionsCell.append(resourceRowMenu(resource)); row.append(actionsCell);

      row.addEventListener('click', (event) => {
        if (!resource.uid || event.target.closest('.res-chevron, .detail-menu')) return;
        state.selectedResourceUID = state.selectedResourceUID === resource.uid ? '' : resource.uid;
        state.resourcePanelTab = 'overview';
        renderApplicationDetail();
      });

      tbody.append(row);
      rows.push({ row, node, parentUid: node.parent?.resource.uid || '' });
      renderRows(node.children, depth + 1);
    });

    const roots = nodes.filter((node) => !node.parent);
    renderRows(roots, 0);

    const statuses = [...new Set(rows.map((item) => item.row.dataset.status).filter((value) => value && value !== '—'))].sort();
    if (state.resourcesStatus && !statuses.includes(state.resourcesStatus)) state.resourcesStatus = '';
    statuses.forEach((status) => statusSelect.add(new Option(status, status)));
    statusSelect.value = state.resourcesStatus;

    function applyVisibility() {
      const query = state.resourcesQuery.trim().toLowerCase();
      const filtering = Boolean(query || state.resourcesKind || state.resourcesStatus);
      rows.forEach(({ row, node, parentUid }) => {
        const resource = node.resource || {};
        const matches = (!state.resourcesKind || resource.kind === state.resourcesKind)
          && (!state.resourcesStatus || row.dataset.status === state.resourcesStatus)
          && (!query || `${resource.kind} ${resource.name}`.toLowerCase().includes(query));
        let visible = matches;
        if (visible && !filtering) {
          let cursor = parentUid;
          while (cursor) { if (!state.resourcesExpanded.has(cursor)) { visible = false; break; } cursor = parentOf.get(cursor); }
        }
        row.hidden = !visible;
        const chevron = row.querySelector('.res-chevron');
        if (chevron && node.children.length) chevron.classList.toggle('is-open', state.resourcesExpanded.has(resource.uid));
      });
    }

    search.addEventListener('input', () => { state.resourcesQuery = search.value; applyVisibility(); });
    kindSelect.addEventListener('change', () => { state.resourcesKind = kindSelect.value; applyVisibility(); });
    statusSelect.addEventListener('change', () => { state.resourcesStatus = statusSelect.value; applyVisibility(); });
    refresh.addEventListener('click', () => { state.resourcesLive = null; state.resourcesLiveKey = ''; renderApplicationDetail(); });

    applyVisibility();
    loadResourceAges(app);
    layout.append(section);
    if (selectedNode) { layout.classList.add('is-open'); layout.append(resourcePanel(app, selectedNode, fresh)); }
    panel.append(layout);
  }

  function resourcePanel(app, node, fresh) {
    const resource = node.resource || {};
    const live = state.resourcesLive?.get(resource.uid) || null;
    const clusterName = app.spec?.clusterRef?.name || app.spec?.environment?.name || '';
    const aside = element('aside', 'res-panel');
    aside.setAttribute('aria-label', 'Resource details');

    const header = element('div', 'res-panel-head');
    const icon = element('span', 'res-panel-icon'); icon.append(resourceKindIcon());
    const titles = element('div', 'res-panel-titles');
    titles.append(element('span', 'res-panel-kind', safe(resource.kind || 'Resource')));
    titles.append(element('strong', 'res-panel-name', safe(resource.name || '—')));
    titles.append(live ? badge(live.status || '—', live.statusTone || 'neutral') : inventoryResourceBadge(resource, fresh));
    const close = element('button', 'res-panel-close', '×'); close.type = 'button'; close.setAttribute('aria-label', 'Close details');
    close.addEventListener('click', () => { state.selectedResourceUID = ''; renderApplicationDetail(); });
    header.append(icon, titles, close);
    aside.append(header);

    const tabLabels = { overview: 'Overview', yaml: 'YAML', events: 'Events', logs: 'Logs' };
    const available = resource.kind === 'Pod' ? ['overview', 'yaml', 'events', 'logs'] : ['overview', 'yaml', 'events'];
    if (!available.includes(state.resourcePanelTab)) state.resourcePanelTab = 'overview';
    const nav = element('nav', 'res-panel-tabs'); nav.setAttribute('role', 'tablist');
    available.forEach((id) => {
      const button = element('button', `res-panel-tab${state.resourcePanelTab === id ? ' is-active' : ''}`, tabLabels[id]);
      button.type = 'button'; button.setAttribute('role', 'tab'); button.setAttribute('aria-selected', state.resourcePanelTab === id ? 'true' : 'false');
      button.addEventListener('click', () => { state.resourcePanelTab = id; renderApplicationDetail(); });
      nav.append(button);
    });
    aside.append(nav);

    const body = element('div', 'res-panel-body');
    aside.append(body);
    if (state.resourcePanelTab === 'yaml') resourcePanelYAML(body, live);
    else if (state.resourcePanelTab === 'events') resourcePanelEvents(body, app, resource);
    else if (state.resourcePanelTab === 'logs') resourcePanelLogs(body, app, resource);
    else resourcePanelOverview(body, resource, live, clusterName);
    return aside;
  }

  function panelInfoRow(label, value, copyable) {
    if (value == null || value === '') return null;
    const row = element('div', 'res-panel-info-row');
    row.append(element('dt', '', label));
    const dd = element('dd', '');
    dd.append(element('span', 'res-panel-value', String(value)));
    if (copyable) dd.append(copyIconButton(String(value)));
    row.append(dd);
    return row;
  }

  function panelValueRow(label, node) {
    const row = element('div', 'res-panel-info-row');
    row.append(element('dt', '', label));
    const dd = element('dd', ''); dd.append(node); row.append(dd);
    return row;
  }

  function resourcePanelOverview(body, resource, live, clusterName) {
    const details = element('section', 'res-panel-card');
    details.append(element('h4', 'res-panel-card-title', 'Details'));
    const list = element('dl', 'res-panel-info');
    if (!live) list.append(element('p', 'helper', 'Live details are unavailable for this resource.'));
    else {
      [
        panelInfoRow('Name', live.name || resource.name, true),
        panelInfoRow('Namespace', live.namespace || resource.namespace, true),
        panelInfoRow('Cluster', clusterName, false),
        panelInfoRow('Controlled by', live.owner, true),
        panelInfoRow('Node', live.node, true),
        panelInfoRow('IP address', live.podIP, true),
        panelInfoRow('Created', live.createdAt ? formatObservedTime(live.createdAt) : '', false),
        panelInfoRow('Age', live.createdAt ? ageText(live.createdAt) : '', false),
      ].forEach((row) => { if (row) list.append(row); });
    }
    details.append(list); body.append(details);

    const statusCard = element('section', 'res-panel-card');
    statusCard.append(element('h4', 'res-panel-card-title', 'Status'));
    const statusList = element('dl', 'res-panel-info');
    if (live) {
      if (live.status) statusList.append(panelValueRow('Phase', badge(live.status, live.statusTone || 'neutral')));
      if (live.ready && live.ready !== '—') statusList.append(panelValueRow('Ready', element('span', 'res-panel-value', live.ready)));
      if (resource.kind === 'Pod') statusList.append(panelValueRow('Restarts', element('span', 'res-panel-value', String(live.restarts || 0))));
      if (live.qosClass) statusList.append(panelValueRow('QoS Class', element('span', 'res-panel-value', live.qosClass)));
      if (Array.isArray(live.conditions) && live.conditions.length) {
        const conditions = element('div', 'res-conditions');
        live.conditions.forEach((condition) => {
          const line = element('div', 'res-condition');
          line.append(element('span', `res-cond-dot ${condition.status === 'True' ? 'is-true' : condition.status === 'False' ? 'is-false' : ''}`));
          line.append(element('span', 'res-cond-type', safe(condition.type)));
          line.append(element('span', 'res-cond-status', safe(condition.status || 'Unknown')));
          conditions.append(line);
        });
        statusList.append(panelValueRow('Conditions', conditions));
      }
    } else {
      statusList.append(element('p', 'helper', 'Live status is unavailable for this resource.'));
    }
    statusCard.append(statusList); body.append(statusCard);
  }

  function resourcePanelYAML(body, live) {
    const card = element('section', 'res-panel-card');
    const head = element('div', 'res-panel-card-head');
    head.append(element('h4', 'res-panel-card-title', 'YAML'));
    if (live?.yaml) {
      const copy = element('button', 'button button-secondary res-mini-btn', 'Copy'); copy.type = 'button';
      copy.addEventListener('click', () => copyText(live.yaml));
      head.append(copy);
    }
    card.append(head);
    card.append(element('pre', 'res-yaml', live?.yaml || 'No YAML available.'));
    body.append(card);
  }

  function resourcePanelEvents(body, app, resource) {
    const card = element('section', 'res-panel-card');
    const head = element('div', 'res-panel-card-head');
    head.append(element('h4', 'res-panel-card-title', 'Events'));
    const list = element('div', 'res-events'); list.append(element('p', 'helper', 'Loading…'));
    const refresh = element('button', 'button button-secondary res-mini-btn', 'Refresh'); refresh.type = 'button';
    refresh.addEventListener('click', () => { list.replaceChildren(element('p', 'helper', 'Loading…')); loadResourceEvents(app, resource, list); });
    head.append(refresh);
    card.append(head, list); body.append(card);
    loadResourceEvents(app, resource, list);
  }

  async function loadResourceEvents(app, resource, container) {
    try {
      const params = new URLSearchParams({ uid: resource.uid || '' });
      if (resource.namespace) params.set('namespace', resource.namespace);
      const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(app.metadata?.name)}/events?${params.toString()}`));
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
      const items = Array.isArray(data.items) ? data.items : [];
      if (!container.isConnected) return;
      if (!items.length) { container.replaceChildren(element('p', 'helper', 'No events recorded for this resource.')); return; }
      container.replaceChildren();
      items.forEach((entry) => {
        const row = element('div', 'res-event');
        const top = element('div', 'res-event-top');
        top.append(badge(entry.type || 'Normal', (entry.type || '').toLowerCase() === 'warning' ? 'error' : 'neutral'));
        top.append(element('span', 'res-event-reason', safe(entry.reason || '')));
        top.append(element('span', 'res-event-time', entry.lastSeen ? `${ageText(entry.lastSeen)} ago` : ''));
        row.append(top, element('p', 'res-event-message', safe(entry.message || '')));
        if (entry.count && entry.count > 1) row.append(element('span', 'res-event-count', `${entry.count}×`));
        container.append(row);
      });
    } catch (error) {
      if (container.isConnected) container.replaceChildren(element('p', 'helper', error.message || 'Could not load events.'));
    }
  }

  function resourcePanelLogs(body, app, resource) {
    const card = element('section', 'res-panel-card');
    const head = element('div', 'res-panel-card-head');
    head.append(element('h4', 'res-panel-card-title', 'Logs'));
    const output = element('pre', 'res-logs'); output.textContent = 'Loading…';
    const refresh = element('button', 'button button-secondary res-mini-btn', 'Refresh'); refresh.type = 'button';
    const load = () => { output.textContent = 'Loading…'; loadResourceLogs(app, resource.name, output); };
    refresh.addEventListener('click', load);
    head.append(refresh);
    card.append(head, output); body.append(card);
    loadResourceLogs(app, resource.name, output);
  }

  async function loadResourceLogs(app, pod, output) {
    try {
      const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(app.metadata?.name)}/pods/${encodeURIComponent(pod)}/logs`));
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
      if (!output.isConnected) return;
      output.textContent = (data.logs && data.logs.length) ? data.logs : 'No log output.';
      if (data.truncated) output.textContent += '\n… (truncated)';
    } catch (error) {
      if (output.isConnected) output.textContent = error.message || 'Could not load logs.';
    }
  }

  function copyIconButton(value) {
    const button = element('button', 'res-copy'); button.type = 'button'; button.setAttribute('aria-label', 'Copy to clipboard');
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', 'M9 9V5a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-4M5 9h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-8a2 2 0 0 1 2-2z');
    path.setAttribute('fill', 'none'); path.setAttribute('stroke', 'currentColor');
    path.setAttribute('stroke-width', '1.6'); path.setAttribute('stroke-linecap', 'round'); path.setAttribute('stroke-linejoin', 'round');
    svg.append(path); button.append(svg);
    button.addEventListener('click', () => copyText(value));
    return button;
  }

  async function loadResourceAges(app) {
    const key = app.metadata?.name || '';
    if (state.resourcesLive || state.resourcesLiveKey === key) return;
    state.resourcesLiveKey = key;
    try {
      const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(key)}/resources`));
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
      const map = new Map();
      (Array.isArray(data.items) ? data.items : []).forEach((item) => { if (item.uid) map.set(item.uid, item); });
      state.resourcesLive = map;
      if (state.selectedApplication?.metadata?.name === key && state.detailTab === 'resources') renderApplicationDetail();
    } catch (error) {
      state.resourcesLive = new Map();
    }
  }

  async function deleteApplication(app) {
    const name = app.metadata?.name;
    if (!state.canDeleteApps || !name || state.deleting || state.deletionPending.has(name) || !window.confirm(`Delete application “${name}”? This action cannot be undone.`)) return;
    state.deleting = true; renderApplicationDetail();
    try {
      const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(name)}`), { method: 'DELETE', headers: { Accept: 'application/json' } });
      if (response.status === 404) {
        state.selectedApplication = null; $('#application-detail').hidden = true; $('#applications-list-view').hidden = false;
        const loaded = await refreshApplications();
        if (loaded) showNotice(`Application “${name}” was already absent. List refreshed.`);
        return;
      }
      if (!response.ok) { const body = await response.json().catch(() => ({})); throw new Error(body.error || `Delete failed (${response.status})`); }
      state.deletionPending.add(name);
      const loaded = await refreshApplications();
      if (loaded) {
        if (state.applications.some((item) => item.metadata?.name === name)) {
          showNotice(`Delete accepted for “${name}”. Removal is in progress and may take some time.`, 'info');
        } else {
          state.deletionPending.delete(name);
          showNotice(`Application “${name}” is no longer in the list.`, 'success');
        }
      }
    } catch (error) { showNotice(error.message || 'Could not delete the application. Try again.', 'error'); }
    finally { state.deleting = false; if (state.selectedApplication) renderApplicationDetail(); }
  }

  const UNCATEGORIZED_TYPE = '__uncategorized__';
  function definitionType(definition) {
    const type = definition?.metadata?.labels?.['platform.releaseyard.io/type'];
    return typeof type === 'string' && type.length ? type : UNCATEGORIZED_TYPE;
  }

  function definitionTypeLabel(type) {
    if (type === UNCATEGORIZED_TYPE) return 'Uncategorized';
    return type.replace(/[-_.]+/g, ' ').replace(/(^|\s)([a-z])/g, (_, space, letter) => `${space}${letter.toUpperCase()}`);
  }

  function definitionMatchesFilters(definition, query, typeFilter) {
    const chart = definition.spec?.chart || {};
    const type = definitionType(definition);
    const searchable = [definition.metadata?.name, chart.name, chart.version, chart.repository, definitionTypeLabel(type)]
      .filter(Boolean).join(' ').toLocaleLowerCase('en');
    return searchable.includes(query.trim().toLocaleLowerCase('en')) && (!typeFilter || type === typeFilter);
  }

  function definitionTypeTone(type) {
    const value = `${type || ''} ${definitionTypeLabel(type)}`.toLocaleLowerCase('en');
    if (/front/.test(value)) return 'blue';
    if (/back|api/.test(value)) return 'purple';
    if (/data|sql|postgres|redis|mongo/.test(value)) return 'orange';
    if (/infra|gateway|network|proxy/.test(value)) return 'blue';
    if (/security|auth|keycloak|identity/.test(value)) return 'red';
    if (/monitor|observ|prometheus|grafana/.test(value)) return 'amber';
    if (/ci|cd|runner|argo|gitops|deploy/.test(value)) return 'purple';
    return 'neutral';
  }

  function definitionConfigIcon() {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 20 20');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('class', 'card-config-icon');
    const line = (d) => { const path = document.createElementNS('http://www.w3.org/2000/svg', 'path'); path.setAttribute('d', d); svg.append(path); };
    line('M3 6h14M3 14h14');
    [ [8, 6], [13, 14] ].forEach(([cx, cy]) => {
      const circle = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
      circle.setAttribute('cx', String(cx)); circle.setAttribute('cy', String(cy)); circle.setAttribute('r', '2.1');
      svg.append(circle);
    });
    return svg;
  }

  function renderDefinitionChips(types) {
    const root = $('#definition-chips');
    if (!root) return;
    root.replaceChildren();
    const all = element('button', 'catalog-chip', 'All templates');
    all.type = 'button';
    all.setAttribute('aria-pressed', String(!state.definitionType));
    all.addEventListener('click', () => selectDefinitionType(''));
    root.append(all);
    types.forEach((type) => {
      const chip = element('button', 'catalog-chip', definitionTypeLabel(type));
      chip.type = 'button';
      chip.setAttribute('aria-pressed', String(state.definitionType === type));
      chip.addEventListener('click', () => selectDefinitionType(type));
      root.append(chip);
    });
  }

  function selectDefinitionType(type) {
    state.definitionType = type || '';
    renderDefinitions();
  }

  function renderDefinitionTypeFilter() {
    const select = $('#definition-type');
    const category = $('#definition-category');
    if (!select) return;
    const types = [...new Set(state.definitions.map(definitionType))]
      .sort((a, b) => definitionTypeLabel(a).localeCompare(definitionTypeLabel(b), 'en', { sensitivity: 'base' }) || a.localeCompare(b, 'en'));
    if (state.definitionType && !types.includes(state.definitionType)) state.definitionType = '';
    select.replaceChildren(new Option('All types', ''));
    if (category) category.replaceChildren(new Option('All categories', ''));
    types.forEach((type) => {
      const option = new Option(definitionTypeLabel(type), type);
      if (type !== UNCATEGORIZED_TYPE) option.title = type;
      select.add(option);
      if (category) category.add(new Option(definitionTypeLabel(type), type));
    });
    select.value = state.definitionType;
    if (category) category.value = state.definitionType;
    renderDefinitionChips(types);
  }

  function setDefinitionView(view) {
    state.definitionView = view === 'list' ? 'list' : 'grid';
    const grid = $('#view-grid');
    const list = $('#view-list');
    if (grid) grid.setAttribute('aria-pressed', String(state.definitionView === 'grid'));
    if (list) list.setAttribute('aria-pressed', String(state.definitionView === 'list'));
    const root = $('#definitions');
    if (root) root.classList.toggle('list-view', state.definitionView === 'list');
  }

  function renderDefinitions() {
    renderDefinitionTypeFilter();
    const query = state.definitionQuery.trim().toLocaleLowerCase('en');
    const definitions = state.definitions.filter((def) => definitionMatchesFilters(def, query, state.definitionType)).sort((a, b) => {
      const chartA = a.spec?.chart || {}; const chartB = b.spec?.chart || {};
      const valueA = state.definitionSort === 'chart' ? [chartA.name, chartA.version].filter(Boolean).join(' ') : a.metadata?.name || '';
      const valueB = state.definitionSort === 'chart' ? [chartB.name, chartB.version].filter(Boolean).join(' ') : b.metadata?.name || '';
      return valueA.localeCompare(valueB, 'en', { sensitivity: 'base' });
    });
    $('#definition-count').textContent = `${definitions.length} ${definitions.length === 1 ? 'template' : 'templates'}`;
    const root = $('#definitions');
    root.classList.toggle('list-view', state.definitionView === 'list');
    root.replaceChildren();
    if (!state.definitions.length) root.append(element('p', 'empty', 'No application templates available.'));
    else if (!definitions.length) root.append(element('p', 'empty no-results', 'No templates match these filters.'));
    else {
      definitions.forEach((def) => {
        const readiness = definitionReadiness(def);
        const card = element('button', `definition-card${readiness.ready ? '' : ' unavailable'}`);
        card.type = 'button';
        card.disabled = !readiness.ready;
        const type = definitionType(def);
        const chart = def.spec?.chart || {};
        const configs = Array.isArray(def.spec?.configurableValues) ? def.spec.configurableValues : [];
        const required = configs.filter((item) => item.required).length;
        const head = element('span', 'card-head');
        const icon = element('span', 'card-icon'); icon.setAttribute('aria-hidden', 'true');
        const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg'); svg.setAttribute('viewBox', '0 0 40 40');
        const cube = document.createElementNS('http://www.w3.org/2000/svg', 'path'); cube.setAttribute('d', 'M20 4 35 12.5v15L20 36 5 27.5v-15L20 4Zm0 0v16m15-7.5-15 8-15-8m15 8v15'); svg.append(cube); icon.append(svg);
        head.append(icon, element('span', 'card-name', safe(def.metadata?.name)), element('span', `card-type tone-${definitionTypeTone(type)}`, definitionTypeLabel(type)));
        const description = def.spec?.description || 'No description provided.';
        const desc = element('span', 'card-desc', safe(description));
        const config = element('span', 'card-config');
        config.append(definitionConfigIcon(), element('span', '', `${configs.length} configurable ${configs.length === 1 ? 'value' : 'values'} · ${required} required`));
        const version = chart.version ? (String(chart.version).startsWith('v') ? String(chart.version) : `v${chart.version}`) : '—';
        const footer = element('span', 'card-footer');
        footer.append(element('span', 'card-version', version));
        const status = element('span', 'card-status');
        status.append(badge(readiness.ready ? 'Validated' : readiness.reason, readiness.ready ? 'good' : 'muted'), element('span', 'card-arrow', '↗'));
        footer.append(status);
        card.append(head, desc, config, footer);
        card.setAttribute('aria-label', readiness.ready ? `Open ${safe(def.metadata?.name)}, ${definitionTypeLabel(type)}, ${safe(chart.name)} ${version}, ${configs.length} configurable values` : `${safe(def.metadata?.name)}, ${definitionTypeLabel(type)}, unavailable: ${readiness.reason}`);
        card.dataset.definitionName = def.metadata?.name || '';
        card.addEventListener('click', () => openDefinition(def));
        root.append(card);
      });
    }
    if (state.selectedDefinition) {
      const updated = state.definitions.find((item) => item.metadata?.name === state.selectedDefinition.metadata?.name);
      if (!updated || !definitionReadiness(updated).ready) showCatalog();
      else { state.selectedDefinition = updated; renderSelectedDefinition(); }
    }
  }

  function openDefinition(def) {
    if (!definitionReadiness(def).ready) return;
    state.selectedDefinition = def;
    $('#catalog-view').hidden = true;
    $('#create-view').hidden = false;
    renderCreateBreadcrumb(def);
    renderSelectedDefinition();
    $('#app-name').focus();
  }

  function showCatalog() {
    const selectedName = state.selectedDefinition?.metadata?.name;
    state.selectedDefinition = null;
    renderCreateBreadcrumb(null);
    $('#create-form').reset();
    state.destinationMode = 'cluster'; state.preview = null; state.previewLoading = false;
    if (state.previewController) state.previewController.abort();
    const clusterMode = document.querySelector('input[name="destination-mode"][value="cluster"]');
    if (clusterMode) clusterMode.checked = true;
    if ($('#environment-select')) $('#environment-select').value = '';
    updateDestinationControls();
    $('#catalog-view').hidden = false;
    $('#create-view').hidden = true;
    $('#dynamic-fields').replaceChildren();
    $('#values-fields').hidden = true;
    const cards = [...$('#definitions').querySelectorAll('button:not(:disabled)')];
    (cards.find((card) => card.dataset.definitionName === selectedName) || cards[0])?.focus();
  }

  function renderClusters() {
    const select = $('#cluster');
    const prior = select.value;
    select.replaceChildren(new Option('Select a cluster', ''));
    state.clusters.forEach((cluster) => select.add(new Option(`${safe(cluster.metadata?.name)} · ${condition(cluster, 'Connected')?.status === 'True' ? 'connected' : 'status unknown'}`, cluster.metadata?.name || '')));
    if (state.clusters.some((cluster) => cluster.metadata?.name === prior)) select.value = prior;
    const environmentSelect = $('#environment-select');
    if (environmentSelect) {
      const oldEnvironment = environmentSelect.value;
      const environments = [...new Set(state.clusters.map((cluster) => cluster.metadata?.labels?.['platform.releaseyard.io/environment']).filter(Boolean))].sort((a, b) => a.localeCompare(b, 'en'));
      environmentSelect.replaceChildren(new Option('Select an environment', ''));
      environments.forEach((name) => environmentSelect.add(new Option(name, name)));
      if (environments.includes(oldEnvironment)) environmentSelect.value = oldEnvironment;
      if (environmentSelect.value) fetchEnvironmentPreview(environmentSelect.value);
    }
    updateDestinationControls();
  }

  function updateDestinationControls() {
    const clusterField = $('#cluster-destination');
    const environmentField = $('#environment-destination');
    if (!clusterField || !environmentField) return;
    const envMode = state.destinationMode === 'environment';
    clusterField.hidden = envMode; environmentField.hidden = !envMode;
    $('#cluster').required = !envMode; $('#environment-select').required = envMode;
    if (!envMode) {
      state.preview = null;
      if (state.previewController) state.previewController.abort();
      $('#environment-preview').hidden = true;
    }
    updateSubmit();
  }

  async function fetchEnvironmentPreview(name) {
    const previewNode = $('#environment-preview');
    if (!previewNode) return null;
    if (!name) {
      if (state.previewController) state.previewController.abort();
      state.previewSequence++; state.preview = null; state.previewLoading = false;
      previewNode.hidden = true; previewNode.replaceChildren(); updateSubmit(); return null;
    }
    if (state.previewController) state.previewController.abort();
    const controller = new AbortController(); state.previewController = controller;
    const sequence = ++state.previewSequence; state.previewLoading = true; state.preview = null;
    previewNode.hidden = false; previewNode.className = 'environment-preview is-loading'; previewNode.textContent = 'Checking matching clusters…'; updateSubmit();
    try {
      const response = await apiFetch(`/api/environments/${encodeURIComponent(name)}/targets`, { headers: { Accept: 'application/json' }, signal: controller.signal });
      const body = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(body.error || `Could not check environment (${response.status})`);
      if (sequence !== state.previewSequence || $('#environment-select').value !== name) return null;
      if (body.environment !== name || typeof body.digest !== 'string' || !Array.isArray(body.targets)) throw new Error('The environment preview response was incomplete. Refresh the cluster list and try again.');
      state.preview = body;
      const targets = Array.isArray(body.targets) ? body.targets : [];
      const list = element('ul', 'preview-targets');
      targets.forEach((target) => {
        const item = element('li', 'preview-target');
        item.append(element('span', '', safe(target.name)), badge(target.deleting ? 'Deleting' : target.connected ? 'Connected' : 'Not connected', target.deleting ? 'error' : target.connected ? 'good' : 'muted'));
        list.append(item);
      });
      previewNode.replaceChildren(element('p', 'preview-summary', `${targets.length} matching cluster${targets.length === 1 ? '' : 's'}`), list);
      if (!targets.length) previewNode.append(element('p', 'warning', 'No clusters currently match this environment. The Application will follow matching clusters when they are added.'));
      else if (targets.some((item) => item.deleting)) previewNode.append(element('p', 'warning', 'A matching cluster is being deleted. Choose another environment or wait until deletion finishes.'));
      else if (targets.some((item) => !item.connected)) previewNode.append(element('p', 'warning', 'One or more matching clusters are not connected. The application can still be created, but rollout may be delayed.'));
      previewNode.className = 'environment-preview';
      updateReview();
      return body;
    } catch (error) {
      if (error.name === 'AbortError' || sequence !== state.previewSequence) return null;
      state.preview = null; previewNode.className = 'environment-preview notice-error'; previewNode.textContent = error.message || 'Could not check matching clusters.';
      return null;
    } finally {
      if (sequence === state.previewSequence) { state.previewLoading = false; updateSubmit(); }
    }
  }

  function setNested(target, path, value) {
    const parts = String(path).split('.').filter(Boolean);
    if (!parts.length) return;
    let cursor = target;
    parts.slice(0, -1).forEach((part) => {
      if (!Object.prototype.hasOwnProperty.call(cursor, part) || cursor[part] === null || typeof cursor[part] !== 'object') {
        Object.defineProperty(cursor, part, { value: Object.create(null), enumerable: true, writable: true, configurable: true });
      }
      cursor = cursor[part];
    });
    Object.defineProperty(cursor, parts[parts.length - 1], { value, enumerable: true, writable: true, configurable: true });
  }

  function renderCreateBreadcrumb(def) {
    const nav = $('#breadcrumb');
    if (!nav) return;
    nav.replaceChildren();
    const catalog = element('a', 'breadcrumb-link', 'Catalog');
    catalog.href = './';
    nav.append(catalog);
    if (!def) return;
    nav.append(element('span', 'breadcrumb-separator', '/'), element('span', 'breadcrumb-text', def.metadata?.name || 'Template'));
    nav.append(element('span', 'breadcrumb-separator', '/'), element('strong', 'breadcrumb-current', 'Create'));
  }

  function infoRow(label, value, options = {}) {
    const row = element('div', 'info-row');
    row.append(element('dt', '', label));
    const cell = document.createElement('dd');
    if (options.badge) cell.append(badge(String(value || '—'), 'good'));
    else cell.textContent = value === undefined || value === null || value === '' ? '—' : String(value);
    row.append(cell);
    return row;
  }

  function renderTemplateInfo(def) {
    const root = $('#template-info');
    if (!root) return;
    root.replaceChildren();
    const chart = def.spec?.chart || {};
    root.append(
      infoRow('Name', def.metadata?.name),
      infoRow('Version', chart.version),
      infoRow('Type', definitionTypeLabel(definitionType(def)), { badge: true }),
      infoRow('Description', def.spec?.description || 'No description provided.'),
      infoRow('Chart', chart.name),
      infoRow('Maintainer', def.metadata?.annotations?.['platform.releaseyard.io/maintainer']),
      infoRow('Home', def.metadata?.annotations?.['platform.releaseyard.io/home'] || chart.repository)
    );
  }

  function renderValuesSummary(def) {
    const root = $('#values-summary');
    const badgeNode = $('#values-count');
    if (!root) return;
    const fields = Array.isArray(def.spec?.configurableValues) ? def.spec.configurableValues : [];
    const required = fields.filter((field) => field.required === true).length;
    const optional = fields.length - required;
    root.replaceChildren();
    [
      [fields.length, 'configurable values'],
      [required, 'required values'],
      [optional, 'optional values']
    ].forEach(([count, label]) => {
      const row = element('div', 'values-summary-row');
      row.append(element('strong', '', String(count)), element('span', '', label));
      root.append(row);
    });
    if (badgeNode) badgeNode.textContent = `${fields.length} value${fields.length === 1 ? '' : 's'}`;
  }

  function renderSelectedDefinition() {
    const def = state.selectedDefinition;
    const root = $('#dynamic-fields');
    const valuesFields = $('#values-fields');
    const valuesEmpty = $('#values-empty');
    const namespaceInput = $('#release-namespace');
    const previousNamespace = namespaceInput.value;
    root.replaceChildren();
    if (!def) {
      valuesFields.hidden = true;
      valuesEmpty.hidden = false;
      $('#form-hint').textContent = 'Choose a template to get started.';
      updateSubmit();
      return;
    }
    const chart = def.spec?.chart || {};
    $('#create-definition-name').textContent = def.metadata?.name || 'Application template';
    $('#create-chart-version').textContent = chart.version || '—';
    $('#create-definition-description').textContent = def.spec?.description || 'Configure the parameters below to create a new application from this template.';
    renderTemplateInfo(def);
    renderValuesSummary(def);

    const release = def.spec?.release || {};
    const namespaceInfo = $('#release-namespace-info');
    const namespaceField = $('#release-namespace-field');
    if (release.allowNamespaceOverride === true) {
      namespaceInfo.hidden = true;
      namespaceField.hidden = false;
      namespaceInput.value = previousNamespace;
      $('#release-namespace-hint').textContent = release.namespace ? `Optional. If blank, ${release.namespace} is used. The namespace must already exist.` : 'Optional. The namespace must already exist.';
    } else {
      namespaceField.hidden = true;
      namespaceInput.value = '';
      namespaceInfo.hidden = false;
      namespaceInfo.textContent = `Target namespace: ${release.namespace || 'not specified in the definition'}`;
    }

    const fields = Array.isArray(def.spec?.configurableValues) ? def.spec.configurableValues : [];
    valuesFields.hidden = !fields.length;
    valuesEmpty.hidden = fields.length > 0;
    fields.forEach((config, index) => {
      const path = String(config.path || `value-${index + 1}`);
      const label = element('label', 'field');
      const title = element('span', '');
      title.append(document.createTextNode(path));
      if (config.required) title.append(element('em', 'required-star', '*'));
      label.append(title);
      const type = config.type;
      let input;
      if (type === 'boolean') {
        input = document.createElement('select');
        input.add(new Option('Use default', ''));
        input.add(new Option('Yes', 'true'));
        input.add(new Option('No', 'false'));
      } else {
        input = document.createElement('input');
        input.type = type === 'integer' ? 'number' : 'text';
        if (type === 'integer') input.step = '1';
        if (config.minimum !== undefined) input.min = config.minimum;
        if (config.maximum !== undefined) input.max = config.maximum;
        input.placeholder = config.required ? 'Required' : 'Optional';
      }
      input.dataset.path = path;
      input.dataset.type = type || 'string';
      input.dataset.required = config.required ? 'true' : 'false';
      if (config.required) input.required = true;
      const defaultValue = path.split('.').reduce((value, key) => value && typeof value === 'object' ? value[key] : undefined, def.spec?.defaults);
      if (type !== 'boolean' && defaultValue !== undefined && typeof defaultValue !== 'object') input.placeholder = String(defaultValue);
      const hints = [];
      if (config.required) hints.push('Required');
      if (defaultValue !== undefined) hints.push(`Default: ${typeof defaultValue === 'object' ? JSON.stringify(defaultValue) : String(defaultValue)}`);
      if (config.minimum !== undefined || config.maximum !== undefined) hints.push(`Min: ${config.minimum ?? '—'} · Max: ${config.maximum ?? '—'}`);
      if (type) hints.push(`${type[0].toUpperCase()}${type.slice(1)}`);
      label.append(input);
      if (hints.length) label.append(element('small', '', hints.join(' · ')));
      input.addEventListener('input', updateReview);
      root.append(label);
    });
    const readiness = definitionReadiness(def);
    const blocked = !readiness.ready;
    $('#form-hint').textContent = blocked ? `Creation is disabled. ${readiness.reason}` : 'Review the application details and create it when ready.';
    updateSubmit();
  }

  function updateReview() {
    const form = $('#create-form');
    if (!form) return;
    const name = form.elements.name?.value.trim() || '—';
    let destination = '—';
    if (state.destinationMode === 'cluster') {
      const selected = $('#cluster')?.selectedOptions?.[0];
      destination = selected && selected.value ? selected.value : '—';
    } else {
      const environment = $('#environment-select')?.value;
      const targets = state.preview?.environment === environment ? state.preview.targets : null;
      destination = environment ? `${environment}${targets ? ` · ${targets.length} cluster${targets.length === 1 ? '' : 's'}` : ''}` : '—';
    }
    const configured = [...$('#dynamic-fields').querySelectorAll('input, select')].filter((input) => input.value !== '').length;
    $('#review-name').textContent = name;
    $('#review-destination').textContent = destination;
    $('#review-values').textContent = configured ? `${configured} configured value${configured === 1 ? '' : 's'}` : 'Template defaults';
  }

  function updateSubmit() {
    const def = state.selectedDefinition;
    const blocked = !definitionReadiness(def).ready;
    const destinationReady = state.destinationMode === 'cluster' ? Boolean($('#cluster').value) : Boolean($('#environment-select').value && !state.previewLoading && state.preview && state.preview.environment === $('#environment-select').value);
    $('#submit').disabled =
    !state.canCreateApps ||
    state.creating ||
    !def ||
    !destinationReady ||
    !form.elements.name.value.trim() ||
    blocked;
    updateReview();
  }

  function showNotice(message, kind = 'info') {
    if (redirectingToLogin || signedOutLocally) return;
    notice.textContent = message;
    notice.className = `notice notice-${kind}`;
    notice.hidden = false;
  }

  async function initIdentityProviderAdmin() {
    const form = $('#identity-provider-form');
    const banner = $('#idp-notice');
    const issuer = $('#idp-issuer-url');
    const clientId = $('#idp-client-id');
    const clientSecret = $('#idp-client-secret');
    const redirect = $('#idp-redirect-url');
    const save = $('#idp-save');
    const remove = $('#idp-remove');
    const stateBadge = $('#idp-state');

    const showMessage = (message, kind) => {
      if (!banner) return;
      banner.textContent = message;
      banner.className = `notice notice-${kind}`;
      banner.hidden = false;
    };
    const clearMessage = () => { if (banner) banner.hidden = true; };

    function render(status) {
      if (redirect) redirect.value = status.redirectURL || '';
      const quickRedirect = $('#quick-redirect');
      const quickClientId = $('#quick-client-id');
      if (quickRedirect) quickRedirect.textContent = status.redirectURL || '—';
      if (quickClientId) quickClientId.textContent = status.clientID || '—';
      if (stateBadge) stateBadge.className = `badge${status.enabled ? ' badge-good' : ''}`;
      if (status.enabled) {
        if (issuer) issuer.value = status.issuerURL || '';
        if (clientId) clientId.value = status.clientID || '';
        if (clientSecret) { clientSecret.value = ''; clientSecret.placeholder = 'Leave blank to keep the stored secret'; }
        if (stateBadge) stateBadge.textContent = 'Enabled';
        if (remove) remove.hidden = false;
      } else {
        if (stateBadge) stateBadge.textContent = 'Local only';
        if (remove) remove.hidden = true;
      }
    }

    try {
      const response = await apiFetch('/api/administration/identity-provider', { headers: { Accept: 'application/json' } });
      if (!response.ok) throw new Error(`Load failed (${response.status})`);
      render(await response.json());
    } catch (error) {
      showMessage(error.message || 'Could not load the identity provider configuration.', 'error');
    }

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearMessage();
      const payload = { issuerURL: issuer.value.trim(), clientID: clientId.value.trim(), clientSecret: clientSecret.value };
      if (!payload.issuerURL || !payload.clientID) { showMessage('Issuer URL and client ID are required.', 'error'); return; }
      save.disabled = true; save.textContent = 'Saving…';
      try {
        const response = await apiFetch('/api/administration/identity-provider', {
          method: 'PUT', headers: { 'Content-Type': 'application/json', Accept: 'application/json' }, body: JSON.stringify(payload)
        });
        const body = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(body.error || `Save failed (${response.status})`);
        render(body);
        showMessage('Identity provider configuration saved and active.', 'success');
      } catch (error) {
        showMessage(error.message || 'Could not save the configuration.', 'error');
      } finally { save.disabled = false; save.textContent = 'Save configuration'; }
    });

    if (remove) remove.addEventListener('click', async () => {
      if (!window.confirm('Remove the OIDC configuration? Login will fall back to the local administrator.')) return;
      clearMessage();
      remove.disabled = true;
      try {
        const response = await apiFetch('/api/administration/identity-provider', { method: 'DELETE', headers: { Accept: 'application/json' } });
        if (!response.ok && response.status !== 204) throw new Error(`Remove failed (${response.status})`);
        issuer.value = ''; clientId.value = ''; clientSecret.value = '';
        render({ enabled: false, redirectURL: redirect ? redirect.value : '' });
        showMessage('OIDC configuration removed. Only the local administrator remains.', 'info');
      } catch (error) {
        showMessage(error.message || 'Could not remove the configuration.', 'error');
      } finally { remove.disabled = false; }
    });
  }

  async function initDefinitionAccessAdmin() {
    const form = $('#definition-access-form');
    if (!form) return;
    const groupInput = $('#defaccess-group');
    const search = $('#defaccess-search');
    const rowsBox = $('#defaccess-rows');
    const selectedBox = $('#defaccess-selected');
    const selectedCount = $('#defaccess-selected-count');
    const list = $('#defaccess-list');
    const save = $('#defaccess-save');
    const cancel = $('#defaccess-cancel');
    const banner = $('#defaccess-notice');
    const sortButton = $('#defaccess-sort-name');
    let definitions = [];
    let rules = [];
    let selected = new Set();
    let filter = '';
    let sortDir = 'asc';
    let loadedGroup = '';

    const showMessage = (message, kind) => { banner.textContent = message; banner.className = `notice notice-${kind} ad-notice`; banner.hidden = false; };
    const clearMessage = () => { banner.hidden = true; };
    const descriptionFor = (name) => (definitions.find((definition) => definition.name === name) || {}).description || '';
    const ruleFor = (group) => { const key = group.trim().toLowerCase(); return key ? rules.find((rule) => (rule.group || '').trim().toLowerCase() === key) || null : null; };

    function renderRows() {
      rowsBox.replaceChildren();
      const term = filter.trim().toLowerCase();
      const items = definitions
        .filter((definition) => !term || definition.name.toLowerCase().includes(term) || (definition.description || '').toLowerCase().includes(term))
        .sort((a, b) => { const order = a.name.localeCompare(b.name); return sortDir === 'asc' ? order : -order; });
      if (!items.length) { rowsBox.append(element('p', 'helper ad-empty', definitions.length ? 'No definitions match your search.' : 'No definitions exist yet.')); return; }
      for (const definition of items) {
        const row = element('label', `ad-row${selected.has(definition.name) ? ' is-selected' : ''}`);
        const check = element('span', 'ad-checkcol');
        const box = document.createElement('input');
        box.type = 'checkbox'; box.value = definition.name; box.checked = selected.has(definition.name);
        box.addEventListener('change', () => { if (box.checked) selected.add(definition.name); else selected.delete(definition.name); sync(); });
        check.append(box);
        row.append(check, element('span', 'ad-row-name', definition.name), element('span', 'ad-row-desc', definition.description || '—'));
        rowsBox.append(row);
      }
    }

    function renderSelected() {
      selectedBox.replaceChildren();
      const names = [...selected].sort((a, b) => a.localeCompare(b));
      selectedCount.textContent = `${names.length} selected`;
      if (!names.length) { selectedBox.append(element('p', 'helper', 'No definitions selected yet.')); return; }
      for (const name of names) {
        const item = element('div', 'ad-sel-item');
        const body = element('div', 'ad-sel-body');
        body.append(element('strong', 'ad-sel-name', name));
        body.append(element('small', 'ad-sel-desc', descriptionFor(name) || 'No description'));
        const remove = element('button', 'ad-sel-remove', '×');
        remove.type = 'button'; remove.setAttribute('aria-label', `Remove ${name}`);
        remove.addEventListener('click', () => { selected.delete(name); sync(); });
        item.append(element('span', 'ad-sel-icon'), body, remove);
        selectedBox.append(item);
      }
    }

    function sync() {
      rowsBox.querySelectorAll('.ad-row').forEach((row) => {
        const box = row.querySelector('input');
        if (box) row.classList.toggle('is-selected', selected.has(box.value));
      });
      renderSelected();
    }

    function renderRules() {
      list.replaceChildren();
      if (!rules.length) { list.append(element('p', 'helper', 'No rules yet.')); return; }
      for (const rule of rules) {
        const card = element('div', 'ad-rule');
        const head = element('div', 'ad-rule-head');
        head.append(element('span', 'ad-rule-group', rule.group));
        const actions = element('div', 'ad-rule-actions');
        const edit = element('button', 'button button-secondary', 'Edit'); edit.type = 'button';
        edit.addEventListener('click', () => { groupInput.value = rule.group; loadedGroup = rule.group; selected = new Set(rule.definitions || []); clearMessage(); renderRows(); renderSelected(); groupInput.focus(); });
        const remove = element('button', 'button button-danger', 'Remove'); remove.type = 'button';
        remove.addEventListener('click', async () => {
          if (!window.confirm(`Remove definition access for group “${rule.group}”?`)) return;
          try {
            const response = await apiFetch(`/api/administration/definition-access?group=${encodeURIComponent(rule.group)}`, { method: 'DELETE' });
            if (!response.ok && response.status !== 204) throw new Error(`Remove failed (${response.status})`);
            await refreshRules();
            if ((groupInput.value || '').trim().toLowerCase() === (rule.group || '').trim().toLowerCase()) { selected = new Set(); loadedGroup = ''; renderRows(); renderSelected(); }
            showMessage(`Removed access for ${rule.group}.`, 'info');
          } catch (error) { showMessage(error.message || 'Could not remove the rule.', 'error'); }
        });
        actions.append(edit, remove);
        head.append(actions);
        const defs = element('div', 'ad-rule-defs');
        const names = rule.definitions || [];
        if (!names.length) defs.append(element('span', 'helper', 'no definitions'));
        else for (const name of names) defs.append(element('span', 'ad-chip', name));
        card.append(head, defs);
        list.append(card);
      }
    }

    async function refreshRules() {
      const response = await apiFetch('/api/administration/definition-access', { headers: { Accept: 'application/json' } });
      if (!response.ok) throw new Error(`Load failed (${response.status})`);
      const body = await response.json();
      rules = Array.isArray(body.rules) ? body.rules : [];
      renderRules();
    }

    function applyGroupInput() {
      const group = groupInput.value.trim();
      if (group.toLowerCase() === loadedGroup.toLowerCase()) return;
      const rule = ruleFor(group);
      if (rule) { loadedGroup = rule.group; selected = new Set(rule.definitions || []); renderRows(); renderSelected(); }
      else { loadedGroup = ''; }
    }

    try {
      const definitionsResponse = await apiFetch('/api/applicationdefinitions', { headers: { Accept: 'application/json' } });
      if (definitionsResponse.ok) {
        const data = await definitionsResponse.json();
        definitions = (data.items || [])
          .map((item) => ({ name: item.metadata?.name, description: item.spec?.description || '' }))
          .filter((item) => item.name)
          .sort((a, b) => a.name.localeCompare(b.name));
      }
      renderRows();
      renderSelected();
      await refreshRules();
    } catch (error) {
      showMessage(error.message || 'Could not load definition access.', 'error');
    }

    groupInput.addEventListener('input', applyGroupInput);
    search.addEventListener('input', () => { filter = search.value; renderRows(); });
    sortButton.addEventListener('click', () => { sortDir = sortDir === 'asc' ? 'desc' : 'asc'; sortButton.setAttribute('aria-sort', sortDir === 'asc' ? 'ascending' : 'descending'); renderRows(); });
    cancel.addEventListener('click', () => { groupInput.value = ''; selected = new Set(); filter = ''; search.value = ''; loadedGroup = ''; clearMessage(); renderRows(); renderSelected(); });

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearMessage();
      const group = groupInput.value.trim();
      if (!group) { showMessage('A group name is required.', 'error'); return; }
      save.disabled = true; save.textContent = 'Saving…';
      try {
        const response = await apiFetch('/api/administration/definition-access', {
          method: 'PUT', headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
          body: JSON.stringify({ group, definitions: [...selected].sort() })
        });
        const body = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(body.error || `Save failed (${response.status})`);
        rules = Array.isArray(body.rules) ? body.rules : rules;
        loadedGroup = group;
        renderRules();
        showMessage(`Saved definition access for ${group}.`, 'success');
      } catch (error) {
        showMessage(error.message || 'Could not save the rule.', 'error');
      } finally {
        save.disabled = false; save.textContent = 'Save rule';
      }
    });
  }

  async function initClusterAccessAdmin() {
    const form = $('#cluster-access-form');
    if (!form) return;
    const groupInput = $('#clusaccess-group');
    const search = $('#clusaccess-search');
    const rowsBox = $('#clusaccess-rows');
    const selectedBox = $('#clusaccess-selected');
    const selectedCount = $('#clusaccess-selected-count');
    const list = $('#clusaccess-list');
    const save = $('#clusaccess-save');
    const cancel = $('#clusaccess-cancel');
    const banner = $('#clusaccess-notice');
    const sortButton = $('#clusaccess-sort-name');
    let clusters = [];
    let rules = [];
    let selected = new Set();
    let filter = '';
    let sortDir = 'asc';
    let loadedGroup = '';

    const showMessage = (message, kind) => { banner.textContent = message; banner.className = `notice notice-${kind} ad-notice`; banner.hidden = false; };
    const clearMessage = () => { banner.hidden = true; };
    const environmentFor = (name) => (clusters.find((cluster) => cluster.name === name) || {}).environment || '';
    const ruleFor = (group) => { const key = group.trim().toLowerCase(); return key ? rules.find((rule) => (rule.group || '').trim().toLowerCase() === key) || null : null; };

    function renderRows() {
      rowsBox.replaceChildren();
      const term = filter.trim().toLowerCase();
      const items = clusters
        .filter((cluster) => !term || cluster.name.toLowerCase().includes(term) || (cluster.environment || '').toLowerCase().includes(term))
        .sort((a, b) => { const order = a.name.localeCompare(b.name); return sortDir === 'asc' ? order : -order; });
      if (!items.length) { rowsBox.append(element('p', 'helper ad-empty', clusters.length ? 'No clusters match your search.' : 'No clusters exist yet.')); return; }
      for (const cluster of items) {
        const row = element('label', `ad-row${selected.has(cluster.name) ? ' is-selected' : ''}`);
        const check = element('span', 'ad-checkcol');
        const box = document.createElement('input');
        box.type = 'checkbox'; box.value = cluster.name; box.checked = selected.has(cluster.name);
        box.addEventListener('change', () => { if (box.checked) selected.add(cluster.name); else selected.delete(cluster.name); sync(); });
        check.append(box);
        row.append(check, element('span', 'ad-row-name', cluster.name), element('span', 'ad-row-desc', cluster.environment || '—'));
        rowsBox.append(row);
      }
    }

    function renderSelected() {
      selectedBox.replaceChildren();
      const names = [...selected].sort((a, b) => a.localeCompare(b));
      selectedCount.textContent = `${names.length} selected`;
      if (!names.length) { selectedBox.append(element('p', 'helper', 'No clusters selected yet.')); return; }
      for (const name of names) {
        const item = element('div', 'ad-sel-item');
        const body = element('div', 'ad-sel-body');
        body.append(element('strong', 'ad-sel-name', name));
        body.append(element('small', 'ad-sel-desc', environmentFor(name) || 'No environment'));
        const remove = element('button', 'ad-sel-remove', '×');
        remove.type = 'button'; remove.setAttribute('aria-label', `Remove ${name}`);
        remove.addEventListener('click', () => { selected.delete(name); sync(); });
        item.append(element('span', 'ad-sel-icon'), body, remove);
        selectedBox.append(item);
      }
    }

    function sync() {
      rowsBox.querySelectorAll('.ad-row').forEach((row) => {
        const box = row.querySelector('input');
        if (box) row.classList.toggle('is-selected', selected.has(box.value));
      });
      renderSelected();
    }

    function renderRules() {
      list.replaceChildren();
      if (!rules.length) { list.append(element('p', 'helper', 'No rules yet.')); return; }
      for (const rule of rules) {
        const card = element('div', 'ad-rule');
        const head = element('div', 'ad-rule-head');
        head.append(element('span', 'ad-rule-group', rule.group));
        const actions = element('div', 'ad-rule-actions');
        const edit = element('button', 'button button-secondary', 'Edit'); edit.type = 'button';
        edit.addEventListener('click', () => { groupInput.value = rule.group; loadedGroup = rule.group; selected = new Set(rule.clusters || []); clearMessage(); renderRows(); renderSelected(); groupInput.focus(); });
        const remove = element('button', 'button button-danger', 'Remove'); remove.type = 'button';
        remove.addEventListener('click', async () => {
          if (!window.confirm(`Remove cluster access for group “${rule.group}”?`)) return;
          try {
            const response = await apiFetch(`/api/administration/cluster-access?group=${encodeURIComponent(rule.group)}`, { method: 'DELETE' });
            if (!response.ok && response.status !== 204) throw new Error(`Remove failed (${response.status})`);
            await refreshRules();
            if ((groupInput.value || '').trim().toLowerCase() === (rule.group || '').trim().toLowerCase()) { selected = new Set(); loadedGroup = ''; renderRows(); renderSelected(); }
            showMessage(`Removed cluster access for ${rule.group}.`, 'info');
          } catch (error) { showMessage(error.message || 'Could not remove the rule.', 'error'); }
        });
        actions.append(edit, remove);
        head.append(actions);
        const defs = element('div', 'ad-rule-defs');
        const names = rule.clusters || [];
        if (!names.length) defs.append(element('span', 'helper', 'no clusters'));
        else for (const name of names) defs.append(element('span', 'ad-chip', name));
        card.append(head, defs);
        list.append(card);
      }
    }

    async function refreshRules() {
      const response = await apiFetch('/api/administration/cluster-access', { headers: { Accept: 'application/json' } });
      if (!response.ok) throw new Error(`Load failed (${response.status})`);
      const body = await response.json();
      rules = Array.isArray(body.rules) ? body.rules : [];
      renderRules();
    }

    function applyGroupInput() {
      const group = groupInput.value.trim();
      if (group.toLowerCase() === loadedGroup.toLowerCase()) return;
      const rule = ruleFor(group);
      if (rule) { loadedGroup = rule.group; selected = new Set(rule.clusters || []); renderRows(); renderSelected(); }
      else { loadedGroup = ''; }
    }

    try {
      const response = await apiFetch('/api/clusters', { headers: { Accept: 'application/json' } });
      if (response.ok) {
        const data = await response.json();
        clusters = (data.items || [])
          .map((item) => ({ name: item.metadata?.name, environment: item.metadata?.labels?.['platform.releaseyard.io/environment'] || '' }))
          .filter((item) => item.name)
          .sort((a, b) => a.name.localeCompare(b.name));
      }
      renderRows();
      renderSelected();
      await refreshRules();
    } catch (error) {
      showMessage(error.message || 'Could not load cluster access.', 'error');
    }

    groupInput.addEventListener('input', applyGroupInput);
    search.addEventListener('input', () => { filter = search.value; renderRows(); });
    sortButton.addEventListener('click', () => { sortDir = sortDir === 'asc' ? 'desc' : 'asc'; sortButton.setAttribute('aria-sort', sortDir === 'asc' ? 'ascending' : 'descending'); renderRows(); });
    cancel.addEventListener('click', () => { groupInput.value = ''; selected = new Set(); filter = ''; search.value = ''; loadedGroup = ''; clearMessage(); renderRows(); renderSelected(); });

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearMessage();
      const group = groupInput.value.trim();
      if (!group) { showMessage('A group name is required.', 'error'); return; }
      save.disabled = true; save.textContent = 'Saving…';
      try {
        const response = await apiFetch('/api/administration/cluster-access', {
          method: 'PUT', headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
          body: JSON.stringify({ group, clusters: [...selected].sort() })
        });
        const body = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(body.error || `Save failed (${response.status})`);
        rules = Array.isArray(body.rules) ? body.rules : rules;
        loadedGroup = group;
        renderRules();
        showMessage(`Saved cluster access for ${group}.`, 'success');
      } catch (error) {
        showMessage(error.message || 'Could not save the rule.', 'error');
      } finally {
        save.disabled = false; save.textContent = 'Save rule';
      }
    });
  }

  async function initApplicationAccessAdmin() {
    const form = $('#application-access-form');
    if (!form) return;
    const groupInput = $('#appaccess-group');
    const list = $('#appaccess-list');
    const save = $('#appaccess-save');
    const cancel = $('#appaccess-cancel');
    const banner = $('#appaccess-notice');
    let rules = [];

    const showMessage = (message, kind) => { banner.textContent = message; banner.className = `notice notice-${kind} ad-notice`; banner.hidden = false; };
    const clearMessage = () => { banner.hidden = true; };

    function renderRules() {
      list.replaceChildren();
      if (!rules.length) { list.append(element('p', 'helper', 'No rules yet.')); return; }
      for (const rule of rules) {
        const card = element('div', 'ad-rule');
        const head = element('div', 'ad-rule-head');
        head.append(element('span', 'ad-rule-group', rule.group));
        const actions = element('div', 'ad-rule-actions');
        const edit = element('button', 'button button-secondary', 'Edit'); edit.type = 'button';
        edit.addEventListener('click', () => { groupInput.value = rule.group; groupInput.focus(); clearMessage(); });
        const remove = element('button', 'button button-danger', 'Remove'); remove.type = 'button';
        remove.addEventListener('click', async () => {
          if (!window.confirm(`Revoke application access for group \u201c${rule.group}\u201d? The namespace and its applications are kept.`)) return;
          try {
            const response = await apiFetch(`/api/administration/application-access?group=${encodeURIComponent(rule.group)}`, { method: 'DELETE' });
            if (!response.ok && response.status !== 204) throw new Error(`Remove failed (${response.status})`);
            await refresh();
            showMessage(`Revoked application access for ${rule.group}.`, 'info');
          } catch (error) { showMessage(error.message || 'Could not revoke the rule.', 'error'); }
        });
        actions.append(edit, remove);
        head.append(actions);
        const defs = element('div', 'ad-rule-defs');
        defs.append(element('span', 'ad-chip', rule.namespace));
        card.append(head, defs);
        list.append(card);
      }
    }

    async function refresh() {
      const response = await apiFetch('/api/administration/application-access', { headers: { Accept: 'application/json' } });
      if (!response.ok) throw new Error(`Load failed (${response.status})`);
      const body = await response.json();
      rules = Array.isArray(body.rules) ? body.rules : [];
      renderRules();
    }

    try { await refresh(); } catch (error) { showMessage(error.message || 'Could not load application access.', 'error'); }

    cancel.addEventListener('click', () => { groupInput.value = ''; clearMessage(); });

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearMessage();
      const group = groupInput.value.trim();
      if (!group) { showMessage('A group name is required.', 'error'); return; }
      save.disabled = true; save.textContent = 'Saving\u2026';
      try {
        const response = await apiFetch('/api/administration/application-access', {
          method: 'PUT', headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
          body: JSON.stringify({ group })
        });
        const body = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(body.error || `Save failed (${response.status})`);
        rules = Array.isArray(body.rules) ? body.rules : rules;
        renderRules();
        showMessage(`Granted application access to ${group}.`, 'success');
      } catch (error) {
        showMessage(error.message || 'Could not grant access.', 'error');
      } finally {
        save.disabled = false; save.textContent = 'Grant access';
      }
    });
  }

  async function signOutLocally() {
    const button = $('#auth-logout');
    const status = $('#auth-status');
    const login = $('#auth-login');
    if (!button) return;
    button.disabled = true;
    button.textContent = 'Signing out…';
    if (status) {
      status.textContent = 'Signing out…';
      status.hidden = false;
    }
    try {
      const response = await apiFetch('/auth/logout', {
        method: 'POST',
        headers: { Accept: 'application/json' }
      });
      if (!response.ok) throw new Error(`Sign out failed (${response.status}).`);
      const body = await response.json().catch(() => ({}));
      signedOutLocally = true;

      // Hide the local UI while we leave the page.
      const page = $('.page');
      if (page) { page.inert = true; page.hidden = true; }
      const signedOutMessage = $('#auth-signed-out');
      if (signedOutMessage) signedOutMessage.hidden = false;
      button.hidden = true;

      // Navigate the browser to the sign-in page. For OIDC this is the IdP
      // logout endpoint; for a local installation it is the dashboard login.
      if (typeof body?.logoutUrl === 'string' && body.logoutUrl) {
        if (status) status.textContent = 'Redirecting to sign in…';
        window.location.assign(body.logoutUrl);
        return;
      }

      // Fallback: no logout URL available, stay on the dashboard's login view.
      if (status) status.textContent = 'Dashboard session ended.';
      if (login) {
        login.hidden = false;
        login.focus();
      }
    } catch (_) {
      if (redirectingToLogin) return;
      if (status) status.textContent = 'Could not sign out. Try again.';
      button.textContent = 'Sign out';
      button.disabled = false;
    }
  }

  async function refreshCatalog(options = {}) {
    const refreshButtons = [$('#refresh')].filter(Boolean);
    refreshButtons.forEach((button) => { button.disabled = true; });
    const results = await Promise.allSettled([
      getList('/api/applicationdefinitions'), getList('/api/clusters')
    ]);
    let failures = 0;
    results.forEach((result, index) => {
      if (result.status === 'fulfilled') state[['definitions', 'clusters'][index]] = result.value;
      else failures++;
    });
    renderDefinitions(); renderClusters();
    refreshButtons.forEach((button) => { button.disabled = false; });
    if (failures) showNotice(`Could not load ${failures === 1 ? 'one list' : `${failures} lists`}. Try “Refresh data” again.`, 'error');
    else if (!options.preserveNotice) { notice.hidden = true; }
    return failures === 0;
  }

  async function refreshApplications() {
    const button = $('#refresh-applications');
    button.disabled = true;
    try {
      const [appsResult, definitionsResult] = await Promise.allSettled([getList(withTeam('/api/applications')), getList('/api/applicationdefinitions')]);
      if (appsResult.status !== 'fulfilled') throw appsResult.reason;
      state.applications = appsResult.value;
      if (definitionsResult.status === 'fulfilled') state.definitions = definitionsResult.value;
      const present = new Set(state.applications.map((app) => app.metadata?.name));
      state.deletionPending.forEach((name) => { if (!present.has(name)) state.deletionPending.delete(name); });
      renderApplications();
      if (!state.deletionPending.size) notice.hidden = true;
      return true;
    } catch (error) {
      showNotice(error.message || 'Could not load applications. Try again.', 'error');
      if (!state.applications.length) {
        const root = $('#applications');
        if (root) {
          const row = document.createElement('tr');
          const cell = element('td', 'apps-empty', 'Application list unavailable. Try again.');
          cell.colSpan = 8; row.append(cell); root.replaceChildren(row);
        }
      }
      return false;
    } finally { button.disabled = false; }
  }

  document.addEventListener('click', (event) => {
    document.querySelectorAll('.detail-menu[open]').forEach((menu) => {
      if (!menu.contains(event.target)) menu.open = false;
    });
  });

  const logoutButton = $('#auth-logout');
  if (logoutButton) logoutButton.addEventListener('click', signOutLocally);

  function cloudStatus(resource) {
    const ready = condition(resource, 'Ready');
    if (!ready) return { key: 'pending', label: 'Pending', tone: 'neutral', dot: '' };
    if (ready.status === 'True') return { key: 'ready', label: 'Ready', tone: 'good', dot: 'good' };
    if (ready.reason === 'Creating') return { key: 'provisioning', label: 'Provisioning', tone: 'info', dot: 'info' };
    if (ready.status === 'False') return { key: 'failed', label: 'Failed', tone: 'error', dot: 'error' };
    return { key: 'pending', label: ready.reason || 'Pending', tone: 'neutral', dot: '' };
  }

  function relativeAge(value) {
    const date = Date.parse(value || '');
    if (!Number.isFinite(date)) return '—';
    const seconds = Math.max(0, Math.round((Date.now() - date) / 1000));
    if (seconds < 60) return `${seconds}s`;
    const minutes = Math.round(seconds / 60);
    if (minutes < 60) return `${minutes}m`;
    const hours = Math.round(minutes / 60);
    if (hours < 24) return `${hours}h`;
    return `${Math.round(hours / 24)}d`;
  }

  function formatCloudTime(value) {
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return '—';
    return new Intl.DateTimeFormat('en', { dateStyle: 'medium', timeStyle: 'short' }).format(date);
  }

  function cloudTargetInfo(name) {
    const target = state.cloudTargets.find((item) => item.metadata?.name === name);
    if (!target) return { name: name || '—', provider: '', region: '' };
    return { name: target.metadata?.name || '—', provider: target.spec?.provider || '', region: target.spec?.region || '' };
  }

  function cloudIcon(kind, className = '') {
    const span = element('span', className || 'cloud-row-icon');
    span.setAttribute('aria-hidden', 'true');
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 20 20');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    const paths = {
      box: 'M10 3 3 7l7 4 7-4zM3 10l7 4 7-4M3 13l7 4 7-4',
      document: 'M5 3h7l3 3v11H5zM12 3v3h3M7.5 9h5M7.5 12h5',
      target: 'M10 3a7 7 0 1 0 0 14 7 7 0 0 0 0-14zM10 7a3 3 0 1 0 0 6 3 3 0 0 0 0-6z',
      cloud: 'M6 15h8a3 3 0 0 0 .4-6A4.5 4.5 0 0 0 6 10a2.5 2.5 0 0 0 0 5z',
      clock: 'M10 3a7 7 0 1 0 0 14 7 7 0 0 0 0-14zM10 6v4l2.5 1.5'
    };
    path.setAttribute('d', paths[kind] || paths.cloud);
    svg.append(path);
    span.append(svg);
    return span;
  }

  function sortByName(items) {
    return [...items].sort((a, b) => String(a.metadata?.name || '').localeCompare(String(b.metadata?.name || '')));
  }

  function cloudOption(label, value) {
    const option = document.createElement('option');
    option.value = value;
    option.textContent = label;
    return option;
  }

  function cloudActivityEntries() {
    const entries = [];
    state.cloudResources.forEach((resource) => {
      const name = resource.metadata?.name;
      if (!name) return;
      if (resource.metadata?.creationTimestamp) entries.push({ at: resource.metadata.creationTimestamp, text: `Created ${name}`, tone: '' });
      const ready = condition(resource, 'Ready');
      if (ready?.lastTransitionTime) {
        const status = cloudStatus(resource);
        const text = status.key === 'ready' ? `${name} is ready` : status.key === 'failed' ? `${name} provisioning failed` : `${name} provisioning started`;
        entries.push({ at: ready.lastTransitionTime, text, tone: status.dot });
      }
    });
    return entries.sort((a, b) => Date.parse(b.at) - Date.parse(a.at));
  }

  function sortCloudResources(items) {
    const { key, direction } = state.cloudSort;
    const factor = direction === 'desc' ? -1 : 1;
    return [...items].sort((a, b) => {
      let left;
      let right;
      if (key === 'age') {
        left = Date.parse(a.metadata?.creationTimestamp || '');
        right = Date.parse(b.metadata?.creationTimestamp || '');
      } else if (key === 'status') {
        left = cloudStatus(a).label;
        right = cloudStatus(b).label;
      } else if (key === 'definition') {
        left = a.spec?.definitionRef?.name || '';
        right = b.spec?.definitionRef?.name || '';
      } else if (key === 'target') {
        left = a.spec?.targetRef?.name || '';
        right = b.spec?.targetRef?.name || '';
      } else {
        left = a.metadata?.name || '';
        right = b.metadata?.name || '';
      }
      if (typeof left === 'number' && typeof right === 'number') return (left - right) * factor;
      return String(left).localeCompare(String(right)) * factor;
    });
  }

  function filteredCloudResources() {
    const query = state.cloudSearch.trim().toLowerCase();
    const resources = state.cloudResources.filter((resource) => {
      if (query) {
        const haystack = [resource.metadata?.name, resource.spec?.definitionRef?.name, resource.spec?.targetRef?.name].filter(Boolean).join(' ').toLowerCase();
        if (!haystack.includes(query)) return false;
      }
      if (state.cloudStatusFilter && cloudStatus(resource).key !== state.cloudStatusFilter) return false;
      if (state.cloudTargetFilter && resource.spec?.targetRef?.name !== state.cloudTargetFilter) return false;
      if (state.cloudDefinitionFilter && resource.spec?.definitionRef?.name !== state.cloudDefinitionFilter) return false;
      return true;
    });
    return sortCloudResources(resources);
  }

  function renderCloudKpis() {
    $('#cloud-kpi-resources').textContent = String(state.cloudResources.length);
    $('#cloud-kpi-definitions').textContent = String(state.cloudDefinitions.length);
    $('#cloud-kpi-targets').textContent = String(state.cloudTargets.length);
    const pairs = new Set(state.cloudTargets.map((target) => `${target.spec?.provider || ''}/${target.spec?.region || ''}`).filter((pair) => pair !== '/'));
    $('#cloud-kpi-providers').textContent = String(pairs.size);
    const names = state.cloudTargets.map((target) => target.metadata?.name).filter(Boolean);
    $('#cloud-kpi-providers-sub').textContent = names.length ? names.join(', ') : 'No targets configured';
  }

  function fillCloudFilter(select, allLabel, values) {
    if (!select) return;
    const current = select.value;
    select.replaceChildren(cloudOption(allLabel, ''), ...values.map((value) => cloudOption(value, value)));
    select.value = values.includes(current) ? current : '';
  }

  function renderCloudFilters() {
    fillCloudFilter($('#cloud-target-filter'), 'All targets', state.cloudTargets.map((target) => target.metadata?.name).filter(Boolean));
    fillCloudFilter($('#cloud-definition-filter'), 'All definitions', state.cloudDefinitions.map((definition) => definition.metadata?.name).filter(Boolean));
    state.cloudTargetFilter = $('#cloud-target-filter')?.value || '';
    state.cloudDefinitionFilter = $('#cloud-definition-filter')?.value || '';
  }

  function updateCloudSortHeaders() {
    document.querySelectorAll('.cloud-sort').forEach((button) => {
      const active = button.dataset.sort === state.cloudSort.key;
      const arrow = button.querySelector('.cloud-sort-arrow');
      if (arrow) arrow.textContent = active ? (state.cloudSort.direction === 'asc' ? '↑' : '↓') : '↕';
      button.setAttribute('aria-sort', active ? (state.cloudSort.direction === 'asc' ? 'ascending' : 'descending') : 'none');
    });
  }

  function cloudRowMenu(resource) {
    const menu = document.createElement('details');
    menu.className = 'detail-menu';
    const toggle = element('summary', 'detail-menu-toggle res-row-toggle');
    toggle.setAttribute('aria-label', `Actions for ${resource.metadata?.name || 'resource'}`);
    toggle.title = 'Resource actions';
    toggle.append(dotsIcon());
    const list = element('div', 'detail-menu-list');
    const open = element('button', 'detail-menu-item', 'Open details');
    open.type = 'button';
    open.addEventListener('click', () => { menu.open = false; openCloudResource(resource); });
    const remove = element('button', 'detail-menu-item detail-menu-danger', state.cloudDeleting ? 'Deleting…' : 'Delete resource');
    remove.type = 'button';
    remove.disabled = !state.canDeleteResources || state.cloudDeleting;
    if (!state.canDeleteResources) remove.title = 'You do not have permission to delete cloud resources.';
    remove.addEventListener('click', () => { menu.open = false; deleteCloudResource(resource); });
    list.append(open, remove);
    menu.append(toggle, list);
    return menu;
  }

  function renderCloudTable() {
    const root = $('#cloud-resources');
    if (!root) return;
    root.replaceChildren();
    const total = state.cloudResources.length;
    $('#cloud-resource-count').textContent = String(total);
    const resources = filteredCloudResources();
    if (!total) {
      const row = document.createElement('tr');
      const cell = element('td', 'cloud-empty', state.cloudResourcesError || 'No cloud resources yet. Create one from a definition to get started.');
      cell.colSpan = 6;
      row.append(cell);
      root.append(row);
      return;
    }
    if (!resources.length) {
      const row = document.createElement('tr');
      const cell = element('td', 'cloud-empty', 'No resources match these filters.');
      cell.colSpan = 6;
      row.append(cell);
      root.append(row);
      return;
    }
    resources.forEach((resource) => {
      const row = document.createElement('tr');
      row.className = 'cloud-row';
      const name = resource.metadata?.name || 'Unnamed resource';
      const status = cloudStatus(resource);
      const target = cloudTargetInfo(resource.spec?.targetRef?.name);

      const nameCell = element('td', 'cloud-cell-name');
      const nameWrap = element('div', 'cloud-name-cell');
      nameWrap.append(cloudIcon('box'));
      const open = element('button', 'cloud-name-button', name);
      open.type = 'button';
      open.addEventListener('click', () => openCloudResource(resource));
      nameWrap.append(open);
      nameCell.append(nameWrap);

      const definitionCell = element('td', 'cloud-cell-muted', resource.spec?.definitionRef?.name || '—');
      const targetCell = element('td');
      const targetWrap = element('div', 'cloud-target-cell');
      targetWrap.append(element('span', 'cloud-target-name', target.name));
      const targetMeta = [target.provider, target.region].filter(Boolean).join(' · ');
      if (targetMeta) targetWrap.append(element('span', 'cloud-target-meta', targetMeta));
      targetCell.append(targetWrap);

      const statusCell = element('td', 'cloud-cell-status');
      statusCell.append(badge(status.label, status.tone));
      const ageCell = element('td', 'cloud-cell-muted', relativeAge(resource.metadata?.creationTimestamp));
      const actionsCell = element('td', 'cloud-cell-actions');
      actionsCell.append(cloudRowMenu(resource));

      row.append(nameCell, definitionCell, targetCell, statusCell, ageCell, actionsCell);
      row.addEventListener('click', (event) => {
        if (event.target.closest('button, summary, a, details, input, select')) return;
        openCloudResource(resource);
      });
      root.append(row);
    });
  }

  function renderCloudDefinitions() {
    const root = $('#cloud-definitions');
    if (!root) return;
    root.replaceChildren();
    $('#cloud-definition-count').textContent = String(state.cloudDefinitions.length);
    const expanded = state.cloudDefinitionsExpanded;
    const items = expanded ? state.cloudDefinitions : state.cloudDefinitions.slice(0, 3);
    if (!state.cloudDefinitions.length) {
      root.append(element('p', 'empty', 'No resource definitions available.'));
    } else {
      items.forEach((definition) => {
        const item = element('article', 'cloud-catalog-item');
        const text = element('div', 'cloud-catalog-text');
        text.append(element('h3', 'cloud-catalog-name', definition.metadata?.name || 'Unnamed definition'));
        text.append(element('p', 'cloud-catalog-desc', definition.spec?.description || 'No description provided.'));
        const create = element('button', 'button button-secondary cloud-catalog-action', 'Create resource');
        create.type = 'button';
        create.disabled = !state.canCreateResources || !state.cloudProvisioning;
        create.addEventListener('click', () => openCloudCreate(definition.metadata?.name));
        item.append(cloudIcon('document', 'cloud-catalog-icon'), text, create);
        root.append(item);
      });
    }
    const more = state.cloudDefinitions.length > 3;
    const moreButton = $('#cloud-definitions-more');
    if (moreButton) {
      moreButton.hidden = !more;
      moreButton.parentElement.hidden = !more;
      moreButton.textContent = expanded ? 'Show less' : 'View all definitions →';
    }
    const allButton = $('#cloud-definitions-all');
    if (allButton) allButton.textContent = expanded ? 'Show less' : 'View catalog →';
  }

  function renderCloudTargets() {
    const root = $('#cloud-targets');
    if (!root) return;
    root.replaceChildren();
    $('#cloud-target-count').textContent = String(state.cloudTargets.length);
    const expanded = state.cloudTargetsExpanded;
    const items = expanded ? state.cloudTargets : state.cloudTargets.slice(0, 3);
    if (!state.cloudTargets.length) {
      root.append(element('p', 'empty', 'No cloud targets available.'));
    } else {
      items.forEach((target) => {
        const item = element('article', 'cloud-target-item');
        const text = element('div', 'cloud-target-text');
        const name = target.metadata?.name || 'Unnamed target';
        const local = /localstack/i.test(name) || /localstack/i.test(target.spec?.providerConfigRef?.name || '');
        const label = local ? 'Local' : String(target.spec?.provider || 'Cloud').toUpperCase();
        const titleRow = element('div', 'cloud-target-title');
        titleRow.append(element('h3', 'cloud-target-name', name), badge(label, local ? 'good' : 'neutral'));
        text.append(titleRow);
        text.append(element('p', 'cloud-target-desc', `${target.spec?.provider || 'Cloud'} · ${target.spec?.region || 'No region'}`));
        item.append(cloudIcon('cloud', 'cloud-target-icon'), text);
        root.append(item);
      });
    }
    const more = state.cloudTargets.length > 3;
    const moreButton = $('#cloud-targets-more');
    if (moreButton) {
      moreButton.hidden = !more;
      moreButton.parentElement.hidden = !more;
      moreButton.textContent = expanded ? 'Show less' : 'View all targets →';
    }
    const allButton = $('#cloud-targets-all');
    if (allButton) allButton.textContent = expanded ? 'Show less' : 'View all targets →';
  }

  function renderCloudActivity() {
    const root = $('#cloud-activity');
    if (!root) return;
    root.replaceChildren();
    const entries = cloudActivityEntries();
    const expanded = state.cloudActivityExpanded;
    const items = expanded ? entries : entries.slice(0, 4);
    if (!items.length) {
      root.append(element('p', 'empty', 'No activity yet.'));
    } else {
      items.forEach((entry) => {
        const item = element('article', 'cloud-activity-item');
        item.append(element('span', `cloud-activity-dot${entry.tone ? ` ${entry.tone}` : ''}`));
        const body = element('div', 'cloud-activity-body');
        body.append(element('p', 'cloud-activity-text', entry.text), element('p', 'cloud-activity-time', formatCloudTime(entry.at)));
        item.append(body);
        root.append(item);
      });
    }
    const allButton = $('#cloud-activity-all');
    if (allButton) {
      allButton.hidden = entries.length <= 4;
      allButton.textContent = expanded ? 'Show less' : 'View all activity →';
    }
  }

  function renderCloudCreateOptions() {
    const definitionSelect = $('#cloud-definition');
    const targetSelect = $('#cloud-target');
    if (!definitionSelect || !targetSelect) return;
    const currentDefinition = definitionSelect.value;
    const currentTarget = targetSelect.value;
    definitionSelect.replaceChildren(cloudOption('Select a definition', ''), ...state.cloudDefinitions.map((definition) => cloudOption(definition.metadata?.name || 'Unnamed definition', definition.metadata?.name || '')));
    targetSelect.replaceChildren(cloudOption('Select a target', ''), ...state.cloudTargets.map((target) => cloudOption(target.metadata?.name || 'Unnamed target', target.metadata?.name || '')));
    if (state.cloudDefinitions.some((definition) => definition.metadata?.name === currentDefinition)) definitionSelect.value = currentDefinition;
    if (state.cloudTargets.some((target) => target.metadata?.name === currentTarget)) targetSelect.value = currentTarget;
    updateCloudSubmit();
  }

  function renderCloud() {
    if (!$('#cloud-resources')) return;
    renderCloudKpis();
    renderCloudFilters();
    renderCloudTable();
    renderCloudDefinitions();
    renderCloudTargets();
    renderCloudActivity();
    renderCloudCreateOptions();
    updateCloudSortHeaders();
    if (state.selectedCloudResource) renderCloudResourceDetail();
  }

  async function refreshCloud() {
    const button = $('#refresh-cloud');
    if (button) button.disabled = true;
    const results = await Promise.allSettled([
      getList('/api/cloudtargets'),
      getList('/api/resourcedefinitions'),
      getList(withTeam('/api/resources'))
    ]);
    const [targets, definitions, resources] = results;
    if (targets.status === 'fulfilled') state.cloudTargets = sortByName(targets.value);
    if (definitions.status === 'fulfilled') state.cloudDefinitions = sortByName(definitions.value);
    if (resources.status === 'fulfilled') {
      state.cloudResources = sortByName(resources.value);
      state.cloudResourcesError = '';
    } else {
      state.cloudResourcesError = resources.reason?.message || 'Cloud resources are unavailable for your account.';
    }
    if (state.selectedCloudResource) {
      const latest = state.cloudResources.find((item) => item.metadata?.uid === state.selectedCloudResource.metadata?.uid || item.metadata?.name === state.selectedCloudResource.metadata?.name);
      state.selectedCloudResource = latest || null;
      if (!latest) { $('#cloud-detail').hidden = true; $('#cloud-dashboard').hidden = false; document.querySelector('.page-cloud')?.classList.remove('is-detail-open'); }
    }
    renderCloud();
    const failure = results.find((result) => result.status === 'rejected');
    if (failure) showNotice(failure.reason?.message || 'Could not load cloud data. Try again.', 'error');
    else notice.hidden = true;
    if (button) button.disabled = false;
    return !failure;
  }

  function selectedCloudDefinition() {
    const select = $('#cloud-definition');
    if (!select) return null;
    return state.cloudDefinitions.find((definition) => definition.metadata?.name === select.value) || null;
  }

  function renderCloudValueFields() {
    const root = $('#cloud-dynamic-fields');
    const fields = $('#cloud-values-fields');
    if (!root || !fields) return;
    root.replaceChildren();
    const definition = selectedCloudDefinition();
    const values = definition && Array.isArray(definition.spec?.configurableValues) ? definition.spec.configurableValues : [];
    fields.hidden = values.length === 0;
    values.forEach((config) => {
      const label = element('label', 'field');
      label.append(element('span', '', config.path));
      let input;
      if (config.type === 'boolean') {
        input = document.createElement('select');
        input.append(cloudOption('Not set', ''), cloudOption('True', 'true'), cloudOption('False', 'false'));
      } else {
        input = document.createElement('input');
        input.type = config.type === 'integer' ? 'number' : 'text';
        if (config.type === 'integer') {
          input.step = '1';
          if (config.minimum !== undefined) input.min = String(config.minimum);
          if (config.maximum !== undefined) input.max = String(config.maximum);
        }
        input.autocomplete = 'off';
      }
      input.dataset.path = config.path;
      input.dataset.type = config.type || 'string';
      input.dataset.required = config.required === true ? 'true' : 'false';
      if (config.required === true) input.required = true;
      input.id = `cloud-value-${config.path.replace(/[^a-zA-Z0-9_-]/g, '-')}`;
      input.addEventListener('input', updateCloudSubmit);
      input.addEventListener('change', updateCloudSubmit);
      label.append(input);
      if (config.required) label.append(element('small', '', 'Required'));
      else if (config.minimum !== undefined || config.maximum !== undefined) label.append(element('small', '', `Range: ${config.minimum ?? 'no minimum'} – ${config.maximum ?? 'no maximum'}`));
      root.append(label);
    });
    updateCloudSubmit();
  }

  function updateCloudSubmit() {
    const submit = $('#cloud-submit');
    const name = $('#cloud-name');
    const hint = $('#cloud-form-hint');
    if (!submit || !name || !hint) return;
    const definition = $('#cloud-definition')?.value || '';
    const target = $('#cloud-target')?.value || '';
    if (!state.canCreateResources) hint.textContent = 'You do not have permission to create cloud resources.';
    else if (!definition || !target) hint.textContent = 'Choose a definition and a target to get started.';
    else hint.textContent = 'Only values declared by the definition are sent.';
    submit.disabled = !state.canCreateResources || state.cloudSaving || !name.value.trim() || !name.checkValidity() || !definition || !target;
  }

  function openCloudCreate(definitionName = '') {
    if (!state.canCreateResources) {
      showNotice('You do not have permission to create cloud resources.', 'error');
      return;
    }
    state.cloudCreating = true;
    state.selectedCloudResource = null;
    $('#cloud-dashboard').hidden = true;
    $('#cloud-detail').hidden = true;
    $('#cloud-create-view').hidden = false;
    $('#cloud-name').value = '';
    $('#cloud-definition').value = definitionName || '';
    $('#cloud-target').value = '';
    $('#cloud-dynamic-fields').replaceChildren();
    $('#cloud-values-fields').hidden = true;
    if (definitionName) renderCloudValueFields();
    updateCloudSubmit();
    $('#cloud-name').focus();
  }

  function closeCloudCreate() {
    state.cloudCreating = false;
    $('#cloud-create-view').hidden = true;
    $('#cloud-detail').hidden = true;
    $('#cloud-dashboard').hidden = false;
  }

  function setNestedValue(target, path, value) {
    const parts = String(path).split('.');
    let cursor = target;
    parts.forEach((part, index) => {
      if (index === parts.length - 1) {
        cursor[part] = value;
        return;
      }
      if (!cursor[part] || typeof cursor[part] !== 'object') cursor[part] = {};
      cursor = cursor[part];
    });
  }

  async function submitCloudResource(event) {
    event.preventDefault();
    if (state.cloudSaving) return;
    const form = $('#cloud-create-form');
    if (!form.reportValidity()) return;
    const values = Object.create(null);
    for (const input of $('#cloud-dynamic-fields').querySelectorAll('[data-path]')) {
      if (input.value === '') {
        if (input.dataset.required === 'true') {
          input.focus();
          showNotice(`Enter a value for “${input.dataset.path}”.`, 'error');
          return;
        }
        continue;
      }
      let value = input.value;
      if (input.dataset.type === 'boolean') value = value === 'true';
      else if (input.dataset.type === 'integer') {
        if (!/^-?\d+$/.test(value) || !Number.isSafeInteger(Number(value))) {
          input.setCustomValidity('Enter a valid whole number.');
          input.reportValidity();
          showNotice(`“${input.dataset.path}” must be a valid whole number.`, 'error');
          return;
        }
        input.setCustomValidity('');
        value = Number(value);
      }
      setNestedValue(values, input.dataset.path, value);
    }
    const payload = { name: $('#cloud-name').value.trim(), definitionName: $('#cloud-definition').value, targetName: $('#cloud-target').value, values };
    if (state.team) payload.namespace = state.team;
    state.cloudSaving = true;
    const button = $('#cloud-submit');
    button.disabled = true;
    button.textContent = 'Creating…';
    try {
      const response = await apiFetch('/api/resources', {
        method: 'POST', headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: JSON.stringify(payload)
      });
      const body = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(body.error || `Creation failed (${response.status})`);
      const createdName = body.metadata?.name || payload.name;
      closeCloudCreate();
      await refreshCloud();
      showNotice(`Cloud resource “${createdName}” created.`, 'success');
    } catch (error) {
      showNotice(error.message || 'An error occurred while creating the cloud resource.', 'error');
    } finally {
      state.cloudSaving = false;
      button.disabled = false;
      button.textContent = 'Create resource';
      updateCloudSubmit();
    }
  }

  function openCloudResource(resource) {
    state.selectedCloudResource = resource;
    state.cloudDetailTab = 'overview';
    $('#cloud-dashboard').hidden = true;
    $('#cloud-create-view').hidden = true;
    $('#cloud-detail').hidden = false;
    document.querySelector('.page-cloud')?.classList.add('is-detail-open');
    renderCloudResourceDetail();
    $('#cloud-detail .back-button')?.focus();
  }

  function closeCloudResource() {
    state.selectedCloudResource = null;
    $('#cloud-detail').hidden = true;
    $('#cloud-create-view').hidden = true;
    $('#cloud-dashboard').hidden = false;
    document.querySelector('.page-cloud')?.classList.remove('is-detail-open');
  }

  async function deleteCloudResource(resource) {
    if (state.cloudDeleting) return;
    if (!window.confirm(`Delete cloud resource “${resource.metadata?.name}”? The composed cloud objects will be deleted too.`)) return;
    state.cloudDeleting = true;
    try {
      const response = await apiFetch(`/api/resources/${encodeURIComponent(resource.metadata.name)}`, { method: 'DELETE' });
      if (!response.ok) {
        const body = await response.json().catch(() => ({}));
        throw new Error(body.error || `Delete failed (${response.status})`);
      }
      closeCloudResource();
      await refreshCloud();
      showNotice(`Cloud resource “${resource.metadata.name}” deleted.`, 'success');
    } catch (error) {
      showNotice(error.message || 'Could not delete the cloud resource.', 'error');
    } finally {
      state.cloudDeleting = false;
      renderCloudTable();
    }
  }

  const CLOUD_DETAIL_TABS = [
    { id: 'overview', label: 'Overview' },
    { id: 'configuration', label: 'Configuration' },
    { id: 'conditions', label: 'Conditions' },
    { id: 'outputs', label: 'Outputs' },
    { id: 'composed', label: 'Composed resources' },
    { id: 'events', label: 'Events' },
  ];

  function cloudResourceDefinition(resource) {
    const ref = resource.spec?.definitionRef?.name;
    if (!ref) return null;
    return state.cloudDefinitions.find((definition) => definition.metadata?.name === ref) || null;
  }

  function cloudResourceKind(resource) {
    const composed = Array.isArray(resource.status?.composed) ? resource.status.composed[0] : null;
    if (composed) return { apiVersion: composed.apiVersion || '', kind: composed.kind || '' };
    const template = cloudResourceDefinition(resource)?.spec?.resources?.[0];
    return { apiVersion: template?.apiVersion || '', kind: template?.kind || '' };
  }

  function cloudResourceProvider(resource) {
    return cloudTargetInfo(resource.spec?.targetRef?.name).provider || '—';
  }

  function cloudResourceLastUpdated(resource) {
    const conditions = Array.isArray(resource.status?.conditions) ? resource.status.conditions : [];
    let latest = '';
    conditions.forEach((condition) => {
      if (String(condition.lastTransitionTime || '') > latest) latest = String(condition.lastTransitionTime || '');
    });
    return latest || resource.metadata?.creationTimestamp || '';
  }

  function cloudDetailMetaItem(iconKind, label, value) {
    const item = element('span', 'detail-meta-item');
    const icon = element('span', 'detail-meta-icon'); icon.append(metaIcon(iconKind));
    const text = element('span', 'detail-meta-text');
    text.append(element('span', 'detail-meta-label', label), element('span', 'detail-meta-value', safe(value)));
    item.append(icon, text);
    return item;
  }

  function cloudDetailMenu(resource) {
    const menu = document.createElement('details'); menu.className = 'detail-menu';
    const toggle = element('summary', 'detail-menu-toggle');
    toggle.setAttribute('aria-label', 'More actions'); toggle.title = 'More actions';
    toggle.append(dotsIcon());
    const list = element('div', 'detail-menu-list');
    const remove = element('button', 'detail-menu-item detail-menu-danger', state.cloudDeleting ? 'Deleting…' : 'Delete resource');
    remove.type = 'button';
    remove.disabled = !state.canDeleteResources || state.cloudDeleting;
    if (!state.canDeleteResources) remove.title = 'You do not have permission to delete cloud resources.';
    remove.addEventListener('click', () => { menu.open = false; deleteCloudResource(resource); });
    list.append(remove); menu.append(toggle, list);
    return menu;
  }

  function cloudConfigurationCard(resource) {
    const { section, head } = ovSection('Configuration', 'Current spec values for this resource.');
    const manifest = element('pre', 'res-yaml cloud-manifest', JSON.stringify(resource, null, 2));
    manifest.hidden = true;
    const toggle = element('button', 'button button-secondary', 'View manifest');
    toggle.type = 'button';
    toggle.addEventListener('click', () => {
      manifest.hidden = !manifest.hidden;
      toggle.textContent = manifest.hidden ? 'View manifest' : 'Hide manifest';
    });
    head.append(toggle);
    const values = resource.spec?.values;
    section.append(element('pre', 'res-yaml cloud-config-values', values && typeof values === 'object' && Object.keys(values).length ? JSON.stringify(values, null, 2) : '{}'));
    section.append(manifest);
    return section;
  }

  function cloudConditionsCard(resource) {
    const { section } = ovSection('Conditions', 'Current status conditions for this resource.');
    const conditions = Array.isArray(resource.status?.conditions) ? resource.status.conditions : [];
    if (!conditions.length) { section.append(element('p', 'helper', 'No conditions reported yet.')); return section; }
    const table = element('table', 'ov-table');
    const thead = document.createElement('thead'); const headRow = document.createElement('tr');
    ['Type', 'Status', 'Reason', 'Message', 'Last transition'].forEach((label) => headRow.append(element('th', '', label)));
    thead.append(headRow); table.append(thead);
    const tbody = document.createElement('tbody');
    conditions.forEach((item) => {
      const row = document.createElement('tr');
      row.append(element('td', '', safe(item.type || '—')));
      const statusCell = element('td', '');
      statusCell.append(badge(item.status || 'Unknown', item.status === 'True' ? 'good' : item.status === 'False' ? 'error' : 'neutral'));
      row.append(statusCell, element('td', '', safe(item.reason || '—')), element('td', '', safe(item.message || '—')), element('td', '', relativeTimeAgo(item.lastTransitionTime)));
      tbody.append(row);
    });
    table.append(tbody); section.append(table);
    return section;
  }

  function cloudOutputsCard(resource) {
    const { section } = ovSection('Outputs', 'Connection details and generated values.');
    const outputs = Object.entries(resource.status?.outputs || {}).sort(([a], [b]) => a.localeCompare(b));
    if (!outputs.length) { section.append(element('p', 'helper', 'No outputs reported yet.')); return section; }
    const table = element('table', 'ov-table');
    const thead = document.createElement('thead'); const headRow = document.createElement('tr');
    ['Name', 'Value'].forEach((label) => headRow.append(element('th', '', label)));
    thead.append(headRow); table.append(thead);
    const tbody = document.createElement('tbody');
    outputs.forEach(([key, value]) => {
      const row = document.createElement('tr');
      const valueCell = element('td', '');
      const wrap = element('span', 'cloud-output-value');
      wrap.append(element('code', '', String(value)), copyIconButton(String(value)));
      valueCell.append(wrap);
      row.append(element('td', '', key), valueCell);
      tbody.append(row);
    });
    table.append(tbody); section.append(table);
    return section;
  }

  function cloudComposedCard(resource, status) {
    const { section, head } = ovSection('Composed resources', 'Kubernetes resources managed by this composite resource.');
    const composed = Array.isArray(resource.status?.composed) ? resource.status.composed : [];
    head.append(element('span', 'cloud-count-pill', `${composed.length} ${composed.length === 1 ? 'resource' : 'resources'}`));
    if (!composed.length) { section.append(element('p', 'helper', 'No managed resources reported yet.')); return section; }
    const table = element('table', 'ov-table');
    const thead = document.createElement('thead'); const headRow = document.createElement('tr');
    ['Name', 'Kind', 'Namespace', 'Status'].forEach((label) => headRow.append(element('th', '', label)));
    thead.append(headRow); table.append(thead);
    const tbody = document.createElement('tbody');
    composed.forEach((item) => {
      const row = document.createElement('tr');
      row.append(element('td', '', safe(item.name || '—')), element('td', '', safe(item.kind || '—')), element('td', '', safe(item.namespace || '—')));
      const statusCell = element('td', '');
      if (status.label === 'Ready') statusCell.append(badge('Ready', 'good'));
      else statusCell.append(element('span', 'ov-dash', '—'));
      row.append(statusCell); tbody.append(row);
    });
    table.append(tbody); section.append(table);
    return section;
  }

  function cloudDetailsCard(resource, ctx) {
    const { section } = ovSection('Resource details', 'Metadata and current state.');
    const list = element('dl', 'ov-info');
    const infoRow = (label, value) => { const row = element('div', 'ov-info-row'); row.append(element('dt', '', label), element('dd', '', safe(value))); return row; };
    const kindValue = [ctx.kind.apiVersion, ctx.kind.kind].filter(Boolean).join(' ') || '—';
    list.append(
      infoRow('Name', resource.metadata?.name || '—'),
      infoRow('Namespace', resource.metadata?.namespace || '—'),
      infoRow('Provider', ctx.provider),
      infoRow('Kind', kindValue),
      infoRow('Created', relativeTimeAgo(resource.metadata?.creationTimestamp)),
      infoRow('Last updated', relativeTimeAgo(cloudResourceLastUpdated(resource))),
    );
    const statusRow = element('div', 'ov-info-row');
    statusRow.append(element('dt', '', 'Status'));
    const statusValue = element('dd', '');
    statusValue.append(badge(ctx.status.label, ctx.status.tone));
    statusRow.append(statusValue); list.append(statusRow);
    section.append(list);
    return section;
  }

  function cloudCrossplaneCard(resource, ctx) {
    const { section } = ovSection('Crossplane reference', 'Managed resource identity.');
    const list = element('dl', 'ov-info');
    const infoRow = (label, value) => { const row = element('div', 'ov-info-row'); row.append(element('dt', '', label), element('dd', '', safe(value))); return row; };
    list.append(
      infoRow('API Version', ctx.kind.apiVersion || '—'),
      infoRow('Kind', ctx.kind.kind || '—'),
      infoRow('Metadata name', resource.metadata?.name || '—'),
    );
    section.append(list);
    return section;
  }

  function renderCloudOverview(panel, resource, ctx) {
    const grid = element('div', 'overview-grid');
    const main = element('div', 'overview-main');
    const side = element('div', 'overview-side');
    main.append(cloudConfigurationCard(resource), cloudConditionsCard(resource), cloudOutputsCard(resource), cloudComposedCard(resource, ctx.status));
    side.append(cloudDetailsCard(resource, ctx), cloudCrossplaneCard(resource, ctx));
    grid.append(main, side);
    panel.append(grid);
  }

  function renderCloudResourceDetail() {
    const resource = state.selectedCloudResource;
    if (!resource) return;
    const root = $('#cloud-detail');
    root.replaceChildren();
    root.hidden = false;
    const definition = cloudResourceDefinition(resource);
    const kind = cloudResourceKind(resource);
    const provider = cloudResourceProvider(resource);
    const status = cloudStatus(resource);

    const header = element('div', 'detail-header');
    const topbar = element('div', 'detail-topbar');
    const back = element('button', 'back-button', '← Back to cloud resources');
    back.type = 'button'; back.addEventListener('click', closeCloudResource);
    const tools = element('div', 'detail-tools');
    const refresh = element('button', 'button button-secondary', 'Refresh');
    refresh.type = 'button';
    refresh.addEventListener('click', async () => { refresh.disabled = true; try { await refreshCloud(); } finally { refresh.disabled = false; } });
    tools.append(refresh, cloudDetailMenu(resource));
    topbar.append(back, tools);

    const titleRow = element('div', 'detail-title-row');
    const title = element('h1', 'detail-title', safe(resource.metadata?.name || 'Unnamed resource'));
    title.id = 'cloud-detail-title';
    titleRow.append(title, badge(status.label, status.tone));

    const meta = element('div', 'detail-meta');
    const metaEntries = [
      ['namespace', 'Namespace', resource.metadata?.namespace],
      ['cloud', 'Provider', provider],
      ['definition', 'Kind', [kind.apiVersion, kind.kind].filter(Boolean).join(' ')],
    ].filter((entry) => entry[2]);
    metaEntries.forEach((entry, index) => {
      if (index > 0) meta.append(element('span', 'detail-meta-divider'));
      meta.append(cloudDetailMetaItem(entry[0], entry[1], entry[2]));
    });

    header.append(topbar, titleRow);
    header.append(element('p', 'cloud-detail-description', definition?.spec?.description || 'Cloud resource managed by Crossplane.'));
    header.append(meta);
    root.append(header);

    const tabs = element('nav', 'detail-tabs');
    tabs.setAttribute('aria-label', 'Cloud resource sections');
    CLOUD_DETAIL_TABS.forEach((tab) => {
      const button = element('button', `detail-tab${state.cloudDetailTab === tab.id ? ' is-active' : ''}`, tab.label);
      button.type = 'button'; button.setAttribute('role', 'tab');
      button.setAttribute('aria-selected', state.cloudDetailTab === tab.id ? 'true' : 'false');
      button.addEventListener('click', () => { state.cloudDetailTab = tab.id; renderCloudResourceDetail(); });
      tabs.append(button);
    });
    const panel = element('div', 'detail-panel');
    root.append(tabs, panel);

    if (state.cloudDetailTab === 'overview') {
      renderCloudOverview(panel, resource, { definition, kind, provider, status });
    } else {
      const label = CLOUD_DETAIL_TABS.find((tab) => tab.id === state.cloudDetailTab)?.label || 'This';
      const placeholder = element('section', 'ov-section');
      placeholder.append(element('h4', 'ov-section-title', label), element('p', 'helper', `${label} view will be available next.`));
      panel.append(placeholder);
    }
  }

  function toggleCloudSection(section) {
    if (section === 'definitions') {
      state.cloudDefinitionsExpanded = !state.cloudDefinitionsExpanded;
      renderCloudDefinitions();
    } else if (section === 'targets') {
      state.cloudTargetsExpanded = !state.cloudTargetsExpanded;
      renderCloudTargets();
    } else if (section === 'activity') {
      state.cloudActivityExpanded = !state.cloudActivityExpanded;
      renderCloudActivity();
    }
  }

  async function initCloud() {
    const create = $('#new-cloud-resource');
    $('#refresh-cloud').addEventListener('click', refreshCloud);
    create.addEventListener('click', () => openCloudCreate());
    $('#cancel-cloud-create').addEventListener('click', closeCloudCreate);
    $('#cloud-name').addEventListener('input', updateCloudSubmit);
    $('#cloud-definition').addEventListener('change', () => { renderCloudValueFields(); updateCloudSubmit(); });
    $('#cloud-target').addEventListener('change', updateCloudSubmit);
    $('#cloud-create-form').addEventListener('submit', submitCloudResource);
    $('#cloud-search').addEventListener('input', (event) => { state.cloudSearch = event.currentTarget.value; renderCloudTable(); });
    $('#cloud-status-filter').addEventListener('change', (event) => { state.cloudStatusFilter = event.currentTarget.value; renderCloudTable(); });
    $('#cloud-target-filter').addEventListener('change', (event) => { state.cloudTargetFilter = event.currentTarget.value; renderCloudTable(); });
    $('#cloud-definition-filter').addEventListener('change', (event) => { state.cloudDefinitionFilter = event.currentTarget.value; renderCloudTable(); });
    $('#cloud-reset-filters').addEventListener('click', () => {
      state.cloudSearch = '';
      state.cloudStatusFilter = '';
      state.cloudTargetFilter = '';
      state.cloudDefinitionFilter = '';
      $('#cloud-search').value = '';
      $('#cloud-status-filter').value = '';
      $('#cloud-target-filter').value = '';
      $('#cloud-definition-filter').value = '';
      renderCloudTable();
    });
    document.querySelectorAll('.cloud-sort').forEach((button) => {
      button.addEventListener('click', () => {
        const key = button.dataset.sort;
        state.cloudSort = state.cloudSort.key === key
          ? { key, direction: state.cloudSort.direction === 'asc' ? 'desc' : 'asc' }
          : { key, direction: key === 'age' ? 'desc' : 'asc' };
        renderCloudTable();
        updateCloudSortHeaders();
      });
    });
    $('#cloud-definitions-all').addEventListener('click', () => toggleCloudSection('definitions'));
    $('#cloud-definitions-more').addEventListener('click', () => toggleCloudSection('definitions'));
    $('#cloud-targets-all').addEventListener('click', () => toggleCloudSection('targets'));
    $('#cloud-targets-more').addEventListener('click', () => toggleCloudSection('targets'));
    $('#cloud-activity-all').addEventListener('click', () => toggleCloudSection('activity'));
    await refreshCloud();
    if (!state.cloudProvisioning) {
      $('#cloud-no-crossplane').hidden = false;
      create.disabled = true;
      create.title = 'Crossplane is not installed';
    } else if (!state.canCreateResources) {
      create.disabled = true;
      create.title = 'You do not have permission to create cloud resources';
    }
  }

  if (form) {
  $('#back-to-catalog').addEventListener('click', showCatalog);
  $('#cluster').addEventListener('change', updateSubmit);
  document.querySelectorAll('input[name="destination-mode"]').forEach((radio) => radio.addEventListener('change', () => {
    if (!radio.checked) return;
    state.destinationMode = radio.value;
    updateDestinationControls();
    if (state.destinationMode === 'environment' && $('#environment-select').value) fetchEnvironmentPreview($('#environment-select').value);
  }));
  $('#environment-select').addEventListener('change', () => { state.preview = null; fetchEnvironmentPreview($('#environment-select').value); updateSubmit(); });
  form.elements.name.addEventListener('input', updateSubmit);
  $('#app-description')?.addEventListener('input', updateSubmit);
  $('#refresh').addEventListener('click', refreshCatalog);
  $('#definition-search').addEventListener('input', (event) => { state.definitionQuery = event.currentTarget.value; renderDefinitions(); });
  $('#definition-type').addEventListener('change', (event) => { state.definitionType = event.currentTarget.value; renderDefinitions(); });
  $('#definition-category').addEventListener('change', (event) => { state.definitionType = event.currentTarget.value; renderDefinitions(); });
  $('#definition-sort').addEventListener('change', (event) => { state.definitionSort = event.currentTarget.value; renderDefinitions(); });
  $('#view-grid').addEventListener('click', () => setDefinitionView('grid'));
  $('#view-list').addEventListener('click', () => setDefinitionView('list'));
  setDefinitionView(state.definitionView);
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    if (state.creating) return;
    if (!form.reportValidity()) return;
    const def = state.selectedDefinition;
    if (!def || !definitionReadiness(def).ready) return;
    const submittedMode = state.destinationMode;
    const submittedEnvironment = submittedMode === 'environment' ? $('#environment-select').value : '';
    const submittedCluster = submittedMode === 'cluster' ? $('#cluster').value : '';
    const reviewedPreview = submittedMode === 'environment' ? state.preview : null;
    if (submittedMode === 'environment' && (!submittedEnvironment || state.previewLoading || !reviewedPreview || reviewedPreview.environment !== submittedEnvironment)) {
      showNotice('Select an environment before creating.', 'error');
      return;
    }
    const values = Object.create(null);
    for (const input of $('#dynamic-fields').querySelectorAll('[data-path]')) {
      if (input.value === '') {
        if (input.dataset.required === 'true') { input.focus(); showNotice(`Enter a value for “${input.dataset.path}”.`, 'error'); return; }
        continue;
      }
      let value = input.value;
      if (input.dataset.type === 'boolean') value = value === 'true';
      if (input.dataset.type === 'integer') {
        if (!/^-?\d+$/.test(value) || !Number.isSafeInteger(Number(value))) {
          input.setCustomValidity('Enter a valid, safe whole number.');
          input.reportValidity();
          showNotice(`“${input.dataset.path}” must be a valid whole number.`, 'error');
          return;
        }
        input.setCustomValidity('');
        value = Number(value);
      }
      setNested(values, input.dataset.path, value);
    }
    const button = $('#submit');
    const submittedName = form.elements.name.value.trim();
    const submittedDefinition = def.metadata.name;
    const submittedReleaseNamespace = def.spec?.release?.allowNamespaceOverride === true ? $('#release-namespace').value.trim() : '';
    const payload = {
      name: submittedName, description: $('#app-description')?.value.trim() || '', definitionName: submittedDefinition,
      ...(submittedMode === 'environment' ? { environmentName: submittedEnvironment } : { clusterName: submittedCluster }), values,
      ...(state.team ? { namespace: state.team } : {}),
      ...(submittedReleaseNamespace ? { releaseNamespace: submittedReleaseNamespace } : {})
    };
    state.creating = true;
    button.disabled = true;
    button.textContent = 'Creating…';
    document.querySelectorAll('input[name="destination-mode"], #cluster, #environment-select').forEach((control) => { control.disabled = true; });
    try {
      const response = await apiFetch('/api/applications', {
        method: 'POST', headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: JSON.stringify(payload)
      });
      const body = await response.json().catch(() => ({}));
      if (response.status === 409 && submittedMode === 'environment') {
        state.preview = null;
        await fetchEnvironmentPreview(submittedEnvironment);
        throw new Error('Environment targets changed. Review the refreshed cluster preview, then create again to confirm the new target set.');
      }
      if (!response.ok) throw new Error(body.error || `Creation failed (${response.status})`);
      const createdName = body.metadata?.name || submittedName;
      form.reset();
      showCatalog();
      const listsLoaded = await refreshCatalog({ preserveNotice: true });
      if (listsLoaded) showNotice(`Application “${createdName}” created. Open Applications to check its status.`, 'success');
    } catch (error) {
      showNotice(error.message || 'An error occurred while creating the application.', 'error');
    } finally {
      state.creating = false;
      document.querySelectorAll('input[name="destination-mode"], #cluster, #environment-select').forEach((control) => { control.disabled = false; });
      button.textContent = 'Create application';
      updateSubmit();
    }
  });

  refreshCatalog();
  } else if ($('#applications')) {
    $('#refresh-applications').addEventListener('click', refreshApplications);
    $('#application-search').addEventListener('input', (event) => { state.applicationQuery = event.currentTarget.value; renderApplications(); });
    $('#application-cluster-filter').addEventListener('change', (event) => { state.applicationCluster = event.currentTarget.value; renderApplications(); });
    $('#application-namespace-filter').addEventListener('change', (event) => { state.applicationNamespace = event.currentTarget.value; renderApplications(); });
    $('#application-status-filter').addEventListener('change', (event) => { state.applicationStatus = event.currentTarget.value; renderApplications(); });
    document.querySelectorAll('.apps-sort').forEach((button) => button.addEventListener('click', () => {
      const key = button.dataset.sort;
      state.applicationSort = state.applicationSort.key === key
        ? { key, direction: state.applicationSort.direction === 'asc' ? 'desc' : 'asc' }
        : { key, direction: 'asc' };
      renderApplications();
    }));
    refreshApplications();
  } else if ($('#clusters')) {
    $('#refresh-clusters').addEventListener('click', refreshClusterPage);
    $('#cluster-search').addEventListener('input', (event) => { state.clusterQuery = event.currentTarget.value; renderClustersPage(); });
    $('#cluster-status-filter').addEventListener('change', (event) => { state.clusterStatus = event.currentTarget.value; renderClustersPage(); });
    refreshClusterPage();
  } else if ($('#cloud')) {
    initCloud();
  } else if ($('#identity-provider-form')) {
    initIdentityProviderAdmin();
  }
  if ($('#definition-access-form')) {
    initDefinitionAccessAdmin();
  }
  if ($('#cluster-access-form')) {
    initClusterAccessAdmin();
  }
  if ($('#application-access-form')) {
    initApplicationAccessAdmin();
  }
})();
