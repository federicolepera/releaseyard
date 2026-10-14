try {
  if (localStorage.getItem('idp-theme') === 'dark') {
    document.documentElement.dataset.theme = 'dark';
  }
} catch (_) {}
