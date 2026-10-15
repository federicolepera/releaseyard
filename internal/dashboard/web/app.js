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
  const state = { definitions: [], applications: [], clusters: [], selectedApplication: null, selectedCluster: null, selectedInstanceUID: '', detailTab: 'overview', deleting: false, deletionPending: new Set(), editingValues: false, valuesDraft: Object.create(null), savingValues: false, definitionQuery: '', definitionSort: 'name', definitionType: '', destinationMode: 'cluster', preview: null, previewController: null, previewSequence: 0, previewLoading: false, savingEnvironment: false, creating: false, teams: [], team: '', resourcesQuery: '', resourcesKind: '', resourcesStatus: '', resourcesExpanded: new Set(), resourcesLive: null, resourcesLiveKey: '' };
  state.selectedDefinition = null;
  state.canCreateApps = false;
  state.canDeleteApps = false;
  state.canUpdateApps = false;
  state.canChangeClusterEnvironment = false;
  const form = $('#create-form');
  const notice = $('#notice') || $('#applications-notice') || $('#clusters-notice');
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
    if (!response.ok) throw new Error(`Request failed (${response.status})`);
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

  function renderClustersPage() {
    const root = $('#clusters');
    if (!root) return;
    root.replaceChildren();
    $('#cluster-count').textContent = String(state.clusters.length);
    if (!state.clusters.length) {
      root.append(element('p', 'empty', 'No clusters are enrolled yet.'));
    } else {
      const list = element('div', 'resource-list');
      state.clusters.forEach((cluster) => {
        const row = element('article', 'resource-row');
        const details = element('div', 'resource-main');
        const name = cluster.metadata?.name || 'Unnamed cluster';
        const open = element('button', 'application-link', name);
        open.type = 'button'; open.setAttribute('aria-label', `Open cluster ${name} details`);
        open.addEventListener('click', () => openCluster(cluster));
        const heading = element('h3', 'resource-name'); heading.append(open); details.append(heading);
        const environment = cluster.metadata?.labels?.['platform.releaseyard.io/environment'];
        if (environment) details.append(element('p', 'resource-meta', `Environment: ${environment}`));
        const status = clusterConnection(cluster);
        const side = element('div', 'resource-side'); side.append(badge(status.label, status.tone));
        row.append(details, side); list.append(row);
      });
      root.append(list);
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
    renderClusterDetail();
    $('#cluster-detail .back-button').focus();
  }

  function closeCluster() {
    const name = state.selectedCluster?.metadata?.name;
    state.selectedCluster = null;
    $('#cluster-detail').hidden = true; $('#clusters-list-view').hidden = false;
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
      if (selectedName && !state.selectedCluster) { $('#cluster-detail').hidden = true; $('#clusters-list-view').hidden = false; }
      renderClustersPage();
      notice.hidden = true;
      return true;
    } catch (error) {
      showNotice(error.message || 'Could not load clusters. Try again.', 'error');
      if (!state.clusters.length) $('#clusters').textContent = 'Cluster list unavailable.';
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
    return app.spec?.environment && Array.isArray(app.spec.environment.targets) ? app.spec.environment.targets : null;
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

  function renderApplications() {
    const root = $('#applications');
    root.replaceChildren();
    $('#app-count').textContent = String(state.applications.length);
    if (!state.applications.length) {
      const empty = element('p', 'empty', 'No applications yet.');
      const link = element('a', 'inline-link', 'Browse the Catalog');
      link.href = './';
      empty.append(' ', link);
      root.append(empty);
      if (state.selectedApplication) closeApplication();
      return;
    }
    const list = element('div', 'resource-list');
    state.applications.forEach((app) => {
      const row = element('article', 'resource-row');
      const details = element('div', 'resource-main');
      const appName = app.metadata?.name || 'Unnamed application';
      const openButton = element('button', 'application-link', appName);
      openButton.type = 'button'; openButton.setAttribute('aria-label', `Open ${appName} details`);
      openButton.addEventListener('click', () => openApplication(app));
      const heading = element('h3', 'resource-name'); heading.append(openButton); details.append(heading);
      const definition = app.spec?.applicationDefinitionRef?.name || 'Definition not specified';
      const targets = applicationTargets(app);
      const cluster = targets ? `${app.spec.environment.name} · ${targets.length} cluster${targets.length === 1 ? '' : 's'}` : app.spec?.clusterRef?.name || 'Cluster not specified';
      details.append(element('p', 'resource-meta', `${definition} · ${cluster}`));
      details.append(element('p', 'resource-meta', `Target namespace: ${targetNamespace(app) || 'Not specified'}`));
      const status = condition(app, 'Ready');
      const instances = targets ? (Array.isArray(app.status?.instances) ? app.status.instances : []) : [];
      const generation = app.metadata?.generation;
      const rootReady = status?.status === 'True' && Number.isInteger(generation)
        && app.status?.observedGeneration === generation && status.observedGeneration === generation;
      const allInstancesReady = Boolean(rootReady && targets?.length && targets.every((target) => instances.some((instance) => {
        const ready = instance.conditions?.find((item) => item.type === 'Ready');
        return instance.clusterUID === target.uid && instance.observedGeneration === generation
          && ready?.status === 'True' && ready.observedGeneration === generation
          && instance.applied?.applicationGeneration === generation;
      })));
      const statusLabel = targets ? (allInstancesReady ? 'All releases ready' : 'Release status pending') : status ? (status.status === 'True' ? 'Release ready' : `Release ${status.reason || 'not ready'}`) : 'Release status pending';
      const side = element('div', 'resource-side');
      side.append(badge(statusLabel, targets ? (allInstancesReady ? 'good' : 'neutral') : status?.status === 'True' ? 'good' : status?.status === 'False' ? 'muted' : 'neutral'));
      const health = targets ? aggregateEnvironmentHealth(app) : workloadHealth(app);
      side.append(badge(`Workload ${health.label}`, health.tone));
      if (app.status?.applied?.helmRevision !== undefined) side.append(element('span', 'minor', `Release revision ${safe(app.status.applied.helmRevision)}`));
      if (state.deletionPending.has(appName)) side.append(badge('Deletion in progress', 'muted'));
      row.append(details, side);
      list.append(row);
    });
    root.append(list);
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
  function openApplication(app) { state.selectedApplication = app; state.selectedInstanceUID = ''; state.detailTab = 'overview'; state.editingValues = false; state.valuesDraft = Object.create(null); state.resourcesQuery = ''; state.resourcesKind = ''; state.resourcesStatus = ''; state.resourcesExpanded = new Set(); state.resourcesLive = null; state.resourcesLiveKey = ''; $('#applications-list-view').hidden = true; setDetailChrome(true); renderApplicationDetail(); $('#application-detail .back-button').focus(); }
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
    root.append(header);

    const meta = element('div', 'detail-meta');
    const metaItem = (label, value) => {
      const item = element('span', 'detail-meta-item');
      item.append(element('span', 'detail-meta-label', label), element('span', 'detail-meta-value', safe(value)));
      return item;
    };
    meta.append(
      metaItem('Namespace', app.metadata?.namespace),
      metaItem('Cluster', app.spec?.clusterRef?.name || app.spec?.environment?.name),
      metaItem('Definition', app.spec?.applicationDefinitionRef?.name),
    );
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
    if (tab === 'configuration') {
      panel.append(element('h3', 'values-title', 'Value overrides'));
      const values = app.spec?.values;
      panel.append(element('pre', 'values-json', values && typeof values === 'object' && Object.keys(values).length ? JSON.stringify(values, null, 2) : 'No value overrides'));
      appendValuesEditor(panel, app);
      return;
    }
    if (tab === 'observability') { appendObservability(panel, view); return; }
    if (tab === 'activity') { appendActivity(panel, app); return; }
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
    return state.definitions.find((definition) => definition.metadata?.name === ref && definition.metadata?.namespace === app.metadata?.namespace);
  }

  function appendValuesEditor(root, app) {
    const def = applicationDefinition(app);
    const configs = Array.isArray(def?.spec?.configurableValues) ? def.spec.configurableValues : [];
    const available = Boolean(def && definitionReadiness(def).ready && configs.length);
    const section = element('section', 'values-editor');
    if (state.editingValues && !state.canUpdateApps) {
      state.editingValues = false;
      state.valuesDraft = Object.create(null);
    }
    if (!state.editingValues) {
      const button = element('button', 'button button-secondary', 'Edit values');
      button.type = 'button'; button.disabled = !state.canUpdateApps || !available || state.deletionPending.has(app.metadata?.name);
      button.setAttribute('aria-describedby', 'values-edit-help');
      button.addEventListener('click', () => {
        if (!state.canUpdateApps || !available || state.deletionPending.has(app.metadata?.name)) return;
        state.valuesDraft = Object.create(null);
        state.editingValues = true;
        renderApplicationDetail();
        $('#application-values-form input, #application-values-form select')?.focus();
      });
      section.append(button);
      const help = !state.canUpdateApps
        ? 'Developer or administrator access is required to update application values.'
        : !def ? 'The Application Definition is unavailable, so values cannot be edited.' : !definitionReadiness(def).ready ? 'The Application Definition is not validated, so values cannot be edited.' : !configs.length ? 'The Application Definition has no configurable values.' : 'Only values exposed by the Application Definition can be edited.';
      section.append(element('p', `helper values-edit-help${state.canUpdateApps ? '' : ' access-required'}`, help));
      section.lastChild.id = 'values-edit-help';
      root.append(section); return;
    }
    const formNode = element('form', 'application-values-form'); formNode.id = 'application-values-form';
    formNode.setAttribute('aria-labelledby', 'application-values-heading');
    const heading = element('h3', 'values-title', 'Edit configurable values'); heading.id = 'application-values-heading';
    formNode.append(heading, element('p', 'helper', 'Defaults apply automatically. Leave an optional field blank to remove its override.'));
    const fields = element('div', 'form-grid values-edit-grid');
    const current = app.spec?.values || {};
    configs.forEach((config, index) => {
      const path = String(config.path || `value-${index + 1}`);
      const label = element('label', 'field');
      const labelText = element('span', '', path);
      if (config.required) labelText.append(element('span', 'required-mark', ' · required'));
      label.append(labelText);
      const input = config.type === 'boolean' ? document.createElement('select') : document.createElement('input');
      if (config.type === 'boolean') {
        input.add(new Option('Not set', ''));
        input.add(new Option('Yes', 'true'));
        input.add(new Option('No', 'false'));
      } else {
        input.type = config.type === 'integer' ? 'number' : 'text';
        if (config.type === 'integer') input.step = '1';
        if (config.minimum !== undefined) input.min = config.minimum;
        if (config.maximum !== undefined) input.max = config.maximum;
        input.placeholder = config.required ? 'Enter a value' : 'Optional';
      }
      input.dataset.path = path; input.dataset.type = config.type || 'string'; input.dataset.required = config.required ? 'true' : 'false';
      const fromCurrent = path.split('.').reduce((value, key) => value && typeof value === 'object' ? value[key] : undefined, current);
      if (!Object.prototype.hasOwnProperty.call(state.valuesDraft, path) && fromCurrent !== undefined) {
        state.valuesDraft[path] = typeof fromCurrent === 'boolean' ? String(fromCurrent) : String(fromCurrent);
      }
      input.value = state.valuesDraft[path] ?? '';
      input.addEventListener('input', () => { state.valuesDraft[path] = input.value; input.setCustomValidity(''); });
      input.addEventListener('change', () => { state.valuesDraft[path] = input.value; input.setCustomValidity(''); });
      if (config.required) input.required = true;
      label.append(input);
      const defaultValue = path.split('.').reduce((value, key) => value && typeof value === 'object' ? value[key] : undefined, def.spec?.defaults);
      if (defaultValue !== undefined) label.append(element('small', '', `Default: ${typeof defaultValue === 'object' ? JSON.stringify(defaultValue) : String(defaultValue)}`));
      if (config.minimum !== undefined || config.maximum !== undefined) label.append(element('small', '', `Range: ${config.minimum ?? 'no minimum'} – ${config.maximum ?? 'no maximum'}`));
      fields.append(label);
    });
    formNode.append(fields);
    const formActions = element('div', 'form-actions values-edit-actions');
    const hint = element('p', 'helper', 'Saving updates the configuration; the Helm release may take time to apply it.');
    const buttons = element('div', 'values-edit-buttons');
    const cancel = element('button', 'button button-secondary', 'Cancel'); cancel.type = 'button';
    cancel.addEventListener('click', () => { state.editingValues = false; state.valuesDraft = Object.create(null); renderApplicationDetail(); });
    const save = element('button', 'button button-primary', state.savingValues ? 'Saving…' : 'Save values');
    save.type = 'submit'; save.disabled = state.savingValues;
    buttons.append(cancel, save); formActions.append(hint, buttons); formNode.append(formActions);
    formNode.addEventListener('submit', (event) => { event.preventDefault(); saveApplicationValues(app, formNode); });
    section.append(formNode); root.append(section);
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

  function appendActivity(root, app) {
    const name = app.metadata?.name;
    const section = element('section', 'activity-section');
    section.setAttribute('aria-labelledby', 'application-activity-title');
    const heading = element('h3', 'values-title', 'Activity'); heading.id = 'application-activity-title';
    section.append(heading, element('p', 'helper', 'Recent events recorded for this application.'));
    const list = element('ul', 'activity-list');
    list.append(element('li', 'activity-empty', 'Loading activity…'));
    section.append(list); root.append(section);
    loadActivity(name, list);
  }

  async function loadActivity(name, list) {
    try {
      const response = await apiFetch(withTeam(`/api/activity?kind=Application&name=${encodeURIComponent(name)}`));
      if (response.status === 403) { list.replaceChildren(element('li', 'activity-empty', 'You do not have permission to view this activity.')); return; }
      if (!response.ok) throw new Error(`Request failed (${response.status})`);
      const data = await response.json();
      const items = Array.isArray(data.items) ? data.items : [];
      if (!items.length) { list.replaceChildren(element('li', 'activity-empty', 'No recorded activity yet.')); return; }
      list.replaceChildren(...items.map(activityRow));
    } catch (error) {
      list.replaceChildren(element('li', 'activity-empty', error.message || 'Could not load activity.'));
    }
  }

  function activityRow(entry) {
    const warning = entry.type === 'Warning';
    const row = element('li', `activity-row${warning ? ' activity-warning' : ''}`);
    const head = element('div', 'activity-head');
    head.append(badge(warning ? 'Warning' : 'Normal', warning ? 'error' : 'good'), element('span', 'activity-reason', safe(entry.reason || 'Event')));
    if (entry.count > 1) head.append(element('span', 'activity-count', `×${entry.count}`));
    head.append(element('time', 'activity-time', formatObservedTime(entry.lastSeen)));
    row.append(head, element('p', 'activity-message', safe(entry.message || '')));
    return row;
  }

  function observabilityPods(app) {
    const resources = Array.isArray(app.status?.resources) ? app.status.resources : [];
    return resources.filter((resource) => resource.kind === 'Pod' && resource.name).map((resource) => resource.name).sort();
  }

  function appendObservability(root, app) {
    const name = app.metadata?.name;
    if (!name) return;
    const section = element('section', 'observability-section');
    section.setAttribute('aria-labelledby', 'application-observability-title');
    const heading = element('h3', 'values-title', 'Observability'); heading.id = 'application-observability-title';
    section.append(heading, element('p', 'helper', 'Live data read from the target cluster.'));
    section.append(observabilityResourcesBlock(name));
    section.append(observabilityMetricsBlock(name));
    section.append(observabilityLogsBlock(name, observabilityPods(app)));
    root.append(section);
  }

  function observabilityBlock(title, description) {
    const block = element('div', 'observability-block');
    const head = element('div', 'observability-head');
    head.append(element('strong', '', title));
    block.append(head);
    if (description) block.append(element('p', 'helper', description));
    const body = element('div', 'observability-body');
    block.append(body);
    return { block, head, body };
  }

  function observabilityResourcesBlock(name) {
    const { block, head, body } = observabilityBlock('Resources (live YAML & status)', 'Every resource of the release, read live from the target cluster. Secrets are excluded.');
    const button = element('button', 'button button-secondary', 'Load resources'); button.type = 'button';
    body.append(element('p', 'helper', 'Not loaded yet.'));
    button.addEventListener('click', async () => {
      button.disabled = true; body.replaceChildren(element('p', 'helper', 'Loading…'));
      try {
        const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(name)}/resources`));
        const data = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
        renderLiveResources(body, data);
      } catch (error) { body.replaceChildren(element('p', 'empty', error.message || 'Could not load resources.')); }
      finally { button.disabled = false; }
    });
    head.append(button);
    return block;
  }

  function renderLiveResources(body, data) {
    const items = Array.isArray(data.items) ? data.items : [];
    body.replaceChildren();
    if (data.release) body.append(element('p', 'resource-meta', `Release ${safe(data.release)} in ${safe(data.namespace)} on ${safe(data.clusterName)}`));
    if (!items.length) { body.append(element('p', 'helper', 'No resources reported yet.')); return; }
    for (const item of items) {
      const details = element('details', 'resource-yaml');
      details.append(element('summary', '', `${safe(item.kind)} / ${safe(item.name)}`));
      if (item.error) details.append(element('p', 'empty', safe(item.error)));
      else details.append(element('pre', 'values-json', safe(item.yaml || '')));
      body.append(details);
    }
    if (data.truncated) body.append(element('p', 'helper', 'Output truncated to the first resources.'));
  }

  function observabilityMetricsBlock(name) {
    const { block, head, body } = observabilityBlock('Resource usage (CPU / memory)', 'CPU and memory currently used by the release Pods, from metrics-server.');
    const button = element('button', 'button button-secondary', 'Load usage'); button.type = 'button';
    body.append(element('p', 'helper', 'Not loaded yet.'));
    button.addEventListener('click', async () => {
      button.disabled = true; body.replaceChildren(element('p', 'helper', 'Loading…'));
      try {
        const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(name)}/metrics`));
        const data = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
        renderPodMetrics(body, data);
      } catch (error) { body.replaceChildren(element('p', 'empty', error.message || 'Could not load usage.')); }
      finally { button.disabled = false; }
    });
    head.append(button);
    return block;
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

  function renderPodMetrics(body, data) {
    body.replaceChildren();
    if (!data.available) { body.append(element('p', 'helper', safe(data.reason || 'Metrics are not available.'))); return; }
    const pods = Array.isArray(data.pods) ? data.pods : [];
    if (!pods.length) { body.append(element('p', 'helper', 'No Pod metrics reported yet.')); return; }
    const table = element('table', 'metrics-table');
    const head = document.createElement('thead'); const headRow = document.createElement('tr');
    for (const label of ['Pod', 'CPU', 'Memory']) headRow.append(element('th', '', label));
    head.append(headRow); table.append(head);
    const tbody = document.createElement('tbody');
    for (const pod of pods) {
      const tr = document.createElement('tr');
      tr.append(element('td', '', safe(pod.pod)), element('td', '', formatCPUmilli(pod.cpuMilli)), element('td', '', formatBytes(pod.memoryBytes)));
      tbody.append(tr);
    }
    const total = document.createElement('tr');
    total.append(element('td', '', 'Total'), element('td', '', formatCPUmilli(data.totalCpuMilli)), element('td', '', formatBytes(data.totalMemoryBytes)));
    tbody.append(total);
    table.append(tbody); body.append(table);
  }

  function observabilityLogsBlock(name, pods) {
    const { block, head, body } = observabilityBlock('Pod logs', 'Recent log lines from a Pod of the release.');
    const select = document.createElement('select'); select.className = 'obs-pod-select';
    if (pods.length) select.append(...pods.map((pod) => new Option(pod, pod)));
    else select.append(new Option('No pods reported', ''));
    select.disabled = !pods.length;
    const button = element('button', 'button button-secondary', 'Load logs'); button.type = 'button';
    button.disabled = !pods.length;
    const controls = element('div', 'observability-controls'); controls.append(select, button);
    head.append(controls);
    const output = element('pre', 'values-json obs-logs', 'Not loaded yet.');
    body.append(output);
    button.addEventListener('click', async () => {
      const pod = select.value; if (!pod) return;
      button.disabled = true; output.textContent = 'Loading…';
      try {
        const response = await apiFetch(withTeam(`/api/applications/${encodeURIComponent(name)}/pods/${encodeURIComponent(pod)}/logs`));
        const data = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
        output.textContent = (data.logs && data.logs.length) ? data.logs : 'No log output.';
        if (data.truncated) output.textContent += '\n… (truncated)';
      } catch (error) { output.textContent = error.message || 'Could not load logs.'; }
      finally { button.disabled = !pods.length; }
    });
    return block;
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
    if (!nodes.length) { section.append(element('p', 'empty', 'No resources recorded.')); panel.append(section); return; }

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
      const row = document.createElement('tr'); row.className = 'res-row';
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
    panel.append(section);
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

  function renderDefinitionTypeFilter() {
    const select = $('#definition-type');
    if (!select) return;
    const types = [...new Set(state.definitions.map(definitionType))]
      .sort((a, b) => definitionTypeLabel(a).localeCompare(definitionTypeLabel(b), 'en', { sensitivity: 'base' }) || a.localeCompare(b, 'en'));
    if (state.definitionType && !types.includes(state.definitionType)) state.definitionType = '';
    select.replaceChildren(new Option('All types', ''));
    types.forEach((type) => {
      const option = new Option(definitionTypeLabel(type), type);
      if (type !== UNCATEGORIZED_TYPE) option.title = type;
      select.add(option);
    });
    select.value = state.definitionType;
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
    $('#definition-count').textContent = String(definitions.length);
    const root = $('#definitions');
    root.replaceChildren();
    if (!state.definitions.length) root.append(element('p', 'empty', 'No Application Definitions available.'));
    else if (!definitions.length) root.append(element('p', 'empty no-results', 'No definitions match these filters.'));
    else {
      definitions.forEach((def) => {
        const readiness = definitionReadiness(def);
        const card = element('button', `definition-card${readiness.ready ? '' : ' unavailable'}`);
        card.type = 'button';
        card.disabled = !readiness.ready;
        const main = element('span', 'card-main');
        const type = definitionType(def);
        const icon = element('span', 'card-icon'); icon.setAttribute('aria-hidden', 'true');
        const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg'); svg.setAttribute('viewBox', '0 0 40 40');
        const cube = document.createElementNS('http://www.w3.org/2000/svg', 'path'); cube.setAttribute('d', 'M20 4 35 12.5v15L20 36 5 27.5v-15L20 4Zm0 0v16m15-7.5-15 8-15-8m15 8v15'); svg.append(cube); icon.append(svg);
        main.append(icon, element('span', 'card-type', definitionTypeLabel(type)));
        main.append(element('span', 'card-name', safe(def.metadata?.name)));
        const chart = def.spec?.chart || {};
        main.append(element('span', 'card-chart', [chart.name, chart.version].filter(Boolean).join(' · ') || 'Chart details unavailable'));
        const configs = Array.isArray(def.spec?.configurableValues) ? def.spec.configurableValues : [];
        const required = configs.filter((item) => item.required).length;
        main.append(element('span', 'card-config', `${configs.length} configurable ${configs.length === 1 ? 'value' : 'values'} · ${required} required`));
        const footer = element('span', 'card-footer');
        footer.append(badge(readiness.ready ? 'Validated' : readiness.reason, readiness.ready ? 'good' : 'muted'), element('span', 'card-arrow', '↗'));
        card.append(main, footer);
        card.setAttribute('aria-label', readiness.ready ? `Open ${safe(def.metadata?.name)}, ${definitionTypeLabel(type)}, ${safe(chart.name)} ${safe(chart.version)}, ${configs.length} configurable values` : `${safe(def.metadata?.name)}, ${definitionTypeLabel(type)}, unavailable: ${readiness.reason}`);
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
    renderSelectedDefinition();
    $('#app-name').focus();
  }

  function showCatalog() {
    const selectedName = state.selectedDefinition?.metadata?.name;
    state.selectedDefinition = null;
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
      if (!targets.length) previewNode.append(element('p', 'warning', 'No clusters currently match this environment.'));
      else if (targets.some((item) => item.deleting)) previewNode.append(element('p', 'warning', 'A matching cluster is being deleted. Choose another environment or wait until deletion finishes.'));
      else if (targets.some((item) => !item.connected)) previewNode.append(element('p', 'warning', 'One or more matching clusters are not connected. The application can still be created, but rollout may be delayed.'));
      previewNode.className = 'environment-preview';
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

  function renderSelectedDefinition() {
    const def = state.selectedDefinition;
    const root = $('#dynamic-fields');
    const fieldset = $('#values-fields');
    const detail = $('#definition-detail');
    const namespaceInput = $('#release-namespace');
    const previousNamespace = namespaceInput.value;
    root.replaceChildren();
    detail.replaceChildren();
    if (!def) {
      fieldset.hidden = true;
      $('#form-hint').textContent = 'Choose a definition and cluster to get started.';
      updateSubmit();
      return;
    }
    const chart = def.spec?.chart || {};
    detail.append(element('p', 'resource-meta', [chart.name, chart.version, chart.repository].filter(Boolean).join(' · ') || 'Chart details unavailable'));
    const release = def.spec?.release || {};
    const namespaceInfo = $('#release-namespace-info');
    const namespaceField = $('#release-namespace-field');
    if (release.allowNamespaceOverride === true) {
      namespaceInfo.hidden = true;
      namespaceField.hidden = false;
      namespaceInput.value = previousNamespace;
      $('#release-namespace-hint').textContent = release.namespace ? `Optional. If blank, ${safe(release.namespace)} is used. The namespace must already exist.` : 'Optional. The namespace must already exist.';
    } else {
      namespaceField.hidden = true;
      namespaceInput.value = '';
      namespaceInfo.hidden = false;
      namespaceInfo.textContent = `Target namespace: ${safe(release.namespace || 'not specified in the definition')}`;
    }
    const readiness = definitionReadiness(def);
    if (!readiness.ready) detail.append(element('p', 'warning', `Application Definition is not ready: ${safe(readiness.reason)}`));
    const fields = Array.isArray(def.spec?.configurableValues) ? def.spec.configurableValues : [];
    fieldset.hidden = !fields.length;
    fields.forEach((config, index) => {
      const path = String(config.path || `value-${index + 1}`);
      const label = element('label', 'field');
      const title = element('span', '', path);
      if (config.required) title.append(element('span', 'required-mark', ' · required'));
      label.append(title);
      const type = config.type;
      let input;
      if (type === 'boolean') {
        input = document.createElement('select');
        input.add(new Option('Not set', ''));
        input.add(new Option('Yes', 'true'));
        input.add(new Option('No', 'false'));
      } else {
        input = document.createElement('input');
        input.type = type === 'integer' ? 'number' : 'text';
        if (type === 'integer') input.step = '1';
        if (config.minimum !== undefined) input.min = config.minimum;
        if (config.maximum !== undefined) input.max = config.maximum;
        input.placeholder = config.required ? 'Enter a value' : 'Optional';
      }
      input.dataset.path = path;
      input.dataset.type = type || 'string';
      input.dataset.required = config.required ? 'true' : 'false';
      if (config.required) input.required = true;
      label.append(input);
      const defaultValue = path.split('.').reduce((value, key) => value && typeof value === 'object' ? value[key] : undefined, def.spec?.defaults);
      if (defaultValue !== undefined) label.append(element('small', '', `Default: ${typeof defaultValue === 'object' ? JSON.stringify(defaultValue) : String(defaultValue)}`));
      if (config.minimum !== undefined || config.maximum !== undefined) label.append(element('small', '', `Range: ${config.minimum ?? 'no minimum'} – ${config.maximum ?? 'no maximum'}`));
      root.append(label);
    });
    const blocked = !readiness.ready;
    $('#form-hint').textContent = blocked ? `Creation is disabled. ${readiness.reason}` : fields.length ? 'Optional values are not sent when left blank.' : 'This definition has no additional configurable values.';
    updateSubmit();
  }

  function updateSubmit() {
    const def = state.selectedDefinition;
    const blocked = !definitionReadiness(def).ready;
    const destinationReady = state.destinationMode === 'cluster' ? Boolean($('#cluster').value) : Boolean(state.preview?.digest && state.preview.targets?.length && !state.preview.targets.some((target) => target.deleting) && !state.previewLoading && state.preview.environment === $('#environment-select').value);
    $('#submit').disabled =
    !state.canCreateApps ||
    state.creating ||
    !def ||
    !destinationReady ||
    !form.elements.name.value.trim() ||
    blocked;

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
    const refreshButtons = [$('#refresh'), $('#refresh-create')].filter(Boolean);
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
      if (!state.applications.length) $('#applications').textContent = 'List unavailable.';
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
  $('#refresh').addEventListener('click', refreshCatalog);
  $('#refresh-create').addEventListener('click', refreshCatalog);
  $('#definition-search').addEventListener('input', (event) => { state.definitionQuery = event.currentTarget.value; renderDefinitions(); });
  $('#definition-type').addEventListener('change', (event) => { state.definitionType = event.currentTarget.value; renderDefinitions(); });
  $('#definition-sort').addEventListener('change', (event) => { state.definitionSort = event.currentTarget.value; renderDefinitions(); });
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
    if (submittedMode === 'environment' && (!submittedEnvironment || state.previewLoading || !reviewedPreview
      || reviewedPreview.environment !== submittedEnvironment || typeof reviewedPreview.digest !== 'string'
      || !Array.isArray(reviewedPreview.targets) || !reviewedPreview.targets.length || reviewedPreview.targets.some((target) => target.deleting))) {
      showNotice('Review a current environment preview with matching clusters that are not being deleted before creating.', 'error');
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
      name: submittedName, definitionName: submittedDefinition,
      ...(submittedMode === 'environment' ? { environmentName: submittedEnvironment, previewDigest: reviewedPreview.digest } : { clusterName: submittedCluster }), values,
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
    refreshApplications();
  } else if ($('#clusters')) {
    $('#refresh-clusters').addEventListener('click', refreshClusterPage);
    refreshClusterPage();
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
