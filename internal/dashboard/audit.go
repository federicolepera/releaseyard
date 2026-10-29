package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
)

const (
	auditOutcomeSuccess = "success"
	auditOutcomeDenied  = "denied"
	auditOutcomeFailure = "failure"

	auditEventLocalLogin = "auth.login.local"
	auditEventOIDCLogin  = "auth.login.oidc"
	auditEventLogout     = "auth.logout"
	auditEventAPIWrite   = "api.write"

	auditReasonSessionStoreUnavailable = "session_store_unavailable"
)

type auditRequestIDContextKey struct{}

func withAuditRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, auditRequestIDContextKey{}, id)
}

func auditRequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(auditRequestIDContextKey{}).(string)
	return id
}

func newAuditRequestID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return ""
	}
	return hex.EncodeToString(value)
}

type auditEvent struct {
	Event        string
	Outcome      string
	Actor        string
	Groups       []string
	AuthMethod   authMethod
	Impersonated bool
	Reason       string
	Status       int
}

func (a *authHandler) audit(r *http.Request, event auditEvent) {
	if a.auditLog == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("event", event.Event),
		slog.String("outcome", event.Outcome),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("request_id", auditRequestIDFromContext(r.Context())),
		slog.String("remote_addr", r.RemoteAddr),
		slog.Bool("impersonated", event.Impersonated),
	}
	if event.Actor != "" {
		attrs = append(attrs, slog.String("actor", event.Actor))
	}
	if len(event.Groups) > 0 {
		attrs = append(attrs, slog.Any("groups", event.Groups))
	}
	if event.AuthMethod != "" {
		attrs = append(attrs, slog.String("auth_method", string(event.AuthMethod)))
	}
	if event.Reason != "" {
		attrs = append(attrs, slog.String("reason", event.Reason))
	}
	if event.Status != 0 {
		attrs = append(attrs, slog.Int("status", event.Status))
	}
	a.auditLog.LogAttrs(r.Context(), slog.LevelInfo, "audit", attrs...)
}

func (a *authHandler) auditHTTP(r *http.Request, session dashboardSession, hasSession bool, status int) {
	if a.auditLog == nil || !isStateChangingMethod(r.Method) || !isAPIPath(r.URL.Path) {
		return
	}
	event := auditEvent{
		Event:   auditEventForRequest(r.Method, r.URL.Path),
		Outcome: auditOutcome(status),
		Status:  status,
	}
	if hasSession {
		event.Actor = session.username
		event.Groups = session.groups
		event.AuthMethod = session.authMethod
		_, event.Impersonated = writeClientFromRequest(r)
	}
	a.audit(r, event)
}

func auditEventForRequest(method, path string) string {
	if strings.HasPrefix(path, "/api/administration/identity-provider") {
		switch method {
		case http.MethodPut:
			return "admin.identity_provider.update"
		case http.MethodPost:
			return "admin.identity_provider.test"
		case http.MethodDelete:
			return "admin.identity_provider.delete"
		}
	}
	if strings.HasPrefix(path, "/api/administration/project-access") {
		switch method {
		case http.MethodPut:
			return "admin.project_access.update"
		case http.MethodDelete:
			return "admin.project_access.delete"
		}
	}
	if strings.HasPrefix(path, "/api/administration/platform-access") {
		switch method {
		case http.MethodPut:
			return "admin.platform_access.update"
		case http.MethodDelete:
			return "admin.platform_access.delete"
		}
	}
	return auditEventAPIWrite
}

func auditOutcome(status int) string {
	switch {
	case status >= 200 && status < 400:
		return auditOutcomeSuccess
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return auditOutcomeDenied
	default:
		return auditOutcomeFailure
	}
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}
