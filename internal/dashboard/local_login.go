package dashboard

import (
	"net/http"
)

const localAdminUsername = "admin"

// localLogin authenticates the built-in administrator with the password
// generated at install time. The bcrypt hash is read lazily from the mounted
// Secret so the pod can start before the bootstrap Job that creates it.
func (a *authHandler) localLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
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
		writeError(w, http.StatusBadRequest, "Password is required")
		return
	}
	hash, err := readLocalAdminHash(a.localHashPath)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "Local administrator credentials are not available")
		return
	}
	if !verifyLocalAdminPassword(hash, password) {
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
	a.mu.Lock()
	a.pruneSessionsLocked(now)
	if len(a.sessions) >= maxSessions {
		a.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "Session capacity reached")
		return
	}
	a.sessions[sessionID] = session
	a.mu.Unlock()
	a.setSessionCookie(w, sessionID, session)
	writeJSON(w, http.StatusOK, map[string]any{"csrfToken": csrfToken})
}
