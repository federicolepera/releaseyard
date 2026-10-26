package dashboard

import (
	"errors"
	"net/http"
)

const localAdminUsername = "admin"

// localLogin authenticates the built-in administrator with the password
// generated at install time. The bcrypt hash is read lazily from the mounted
// Secret so the pod can start before the bootstrap Job that creates it.
func (a *authHandler) localLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		a.audit(r, auditEvent{Event: auditEventLocalLogin, Outcome: auditOutcomeDenied, Actor: localAdminUsername, AuthMethod: authMethodLocal, Reason: "origin_not_allowed", Status: http.StatusForbidden})
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "Login request is invalid")
		return
	}
	password := r.PostFormValue("password")
	if password == "" {
		a.audit(r, auditEvent{Event: auditEventLocalLogin, Outcome: auditOutcomeFailure, Actor: localAdminUsername, AuthMethod: authMethodLocal, Reason: "password_required", Status: http.StatusBadRequest})
		writeError(w, http.StatusBadRequest, "Password is required")
		return
	}
	hash, err := readLocalAdminHash(a.localHashPath)
	if err != nil {
		a.audit(r, auditEvent{Event: auditEventLocalLogin, Outcome: auditOutcomeFailure, Actor: localAdminUsername, AuthMethod: authMethodLocal, Reason: "local_admin_unavailable", Status: http.StatusServiceUnavailable})
		writeError(w, http.StatusServiceUnavailable, "Local administrator credentials are not available")
		return
	}
	if !verifyLocalAdminPassword(hash, password) {
		a.audit(r, auditEvent{Event: auditEventLocalLogin, Outcome: auditOutcomeDenied, Actor: localAdminUsername, AuthMethod: authMethodLocal, Reason: "invalid_credentials", Status: http.StatusUnauthorized})
		writeError(w, http.StatusUnauthorized, "Invalid username or password")
		return
	}
	csrfToken, err := randomOpaque()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not create session")
		return
	}
	sessionID, err := randomOpaque()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not create session")
		return
	}
	now := a.now()
	session := dashboardSession{
		subject:      localAdminUsername,
		username:     localAdminUsername,
		csrfToken:    csrfToken,
		lastActivity: now,
		expires:      now.Add(sessionAbsoluteTTL),
		authMethod:   authMethodLocal,
	}
	if err := a.store.PutSession(r.Context(), sessionID, session, sessionTTL(session, now)); err != nil {
		reason := auditReasonSessionStoreUnavailable
		if errors.Is(err, errSessionCapacity) {
			reason = "session_capacity_reached"
			writeError(w, http.StatusServiceUnavailable, "Session capacity reached")
		} else {
			writeError(w, http.StatusServiceUnavailable, "Session store is unavailable")
		}
		a.audit(r, auditEvent{Event: auditEventLocalLogin, Outcome: auditOutcomeFailure, Actor: localAdminUsername, AuthMethod: authMethodLocal, Reason: reason, Status: http.StatusServiceUnavailable})
		return
	}
	a.audit(r, auditEvent{Event: auditEventLocalLogin, Outcome: auditOutcomeSuccess, Actor: localAdminUsername, AuthMethod: authMethodLocal, Status: http.StatusOK})
	a.setSessionCookie(w, sessionID, session)
	writeJSON(w, http.StatusOK, map[string]any{"csrfToken": csrfToken})
}
