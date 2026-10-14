"use strict";

(function () {
  const form = document.querySelector('#login-form');
  if (!form) return;

  const password = document.querySelector('#login-password');
  const error = document.querySelector('#login-error');
  const submit = form.querySelector('button[type="submit"]');
  const oidcBlock = document.querySelector('#login-oidc');
  let submitting = false;

  // Reveal the identity-provider option when OIDC is configured.
  fetch('/api/auth-methods', { headers: { Accept: 'application/json' }, credentials: 'same-origin' })
    .then((response) => (response.ok ? response.json() : null))
    .then((methods) => { if (methods && methods.oidc && oidcBlock) oidcBlock.hidden = false; })
    .catch(() => {});

  function showError(message) {
    if (!error) return;
    error.textContent = message;
    error.hidden = false;
  }

  function hideError() {
    if (error) error.hidden = true;
  }

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    if (submitting) return;
    if (!password || password.value === '') {
      showError('Enter the administrator password.');
      return;
    }
    submitting = true;
    if (submit) submit.disabled = true;
    hideError();
    try {
      const response = await fetch('/auth/local', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ password: password.value }).toString(),
        credentials: 'same-origin',
      });
      if (response.ok) {
        window.location.assign('/');
        return;
      }
      let message = 'Sign in failed. Check the password and try again.';
      try {
        const body = await response.json();
        if (body && typeof body.error === 'string' && body.error) message = body.error;
      } catch (_) {
        // Keep the generic message if the body is not JSON.
      }
      showError(message);
    } catch (_) {
      showError('Could not reach the dashboard. Try again.');
    } finally {
      submitting = false;
      if (submit) submit.disabled = false;
    }
  });
})();
