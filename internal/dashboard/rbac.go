package dashboard

import (
	"context"
	"net/http"
	"slices"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ImpersonatingClientFactory builds a Kubernetes client that authenticates as
// the dashboard ServiceAccount but impersonates the given user and groups, so
// that the management cluster's RBAC decides what the user may do.
type ImpersonatingClientFactory func(userName string, groups []string) (client.Client, error)

// authenticatedGroup is added to every impersonated identity, mirroring what
// the API server does for real authenticated users. Without it the impersonated
// user could not even create a SelfSubjectAccessReview.
const authenticatedGroup = "system:authenticated"

type impersonatingClientContextKey struct{}

func withImpersonatingClient(ctx context.Context, c client.Client) context.Context {
	return context.WithValue(ctx, impersonatingClientContextKey{}, c)
}

// writeClientFromRequest returns the impersonating client attached to the
// request, when the session acts as a specific user.
func writeClientFromRequest(r *http.Request) (client.Client, bool) {
	c, ok := r.Context().Value(impersonatingClientContextKey{}).(client.Client)
	return c, ok
}

type sessionContextKey struct{}

func withSession(ctx context.Context, session dashboardSession) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, session)
}

func sessionFromContext(ctx context.Context) (dashboardSession, bool) {
	session, ok := ctx.Value(sessionContextKey{}).(dashboardSession)
	return session, ok
}

type teamsContextKey struct{}

func withTeams(ctx context.Context, teams []string) context.Context {
	return context.WithValue(ctx, teamsContextKey{}, teams)
}

func teamsFromContext(ctx context.Context) []string {
	teams, _ := ctx.Value(teamsContextKey{}).([]string)
	return teams
}

// teamNamespacesForSession resolves the team namespaces a session may use to
// create and read Applications. The local administrator uses the fixed
// management namespace instead and gets no teams. For OIDC sessions a candidate
// namespace is the configured prefix plus a group the user belongs to, kept
// only when the management cluster RBAC lets the user list Applications there.
func (a *authHandler) teamNamespacesForSession(ctx context.Context, session dashboardSession, impersonated client.Client) []string {
	if session.authMethod != authMethodOIDC || impersonated == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(session.groups))
	teams := make([]string, 0, len(session.groups))
	for _, group := range session.groups {
		namespace := namespaceForGroup(a.teamNamespacePrefix, group)
		if namespace == "" {
			continue
		}
		if _, exists := seen[namespace]; exists {
			continue
		}
		attributes := authorizationv1.ResourceAttributes{Namespace: namespace, Group: platformAPIGroup, Resource: resourceApplications, Verb: "list"}
		if selfSubjectAccessAllowed(ctx, impersonated, attributes) {
			seen[namespace] = struct{}{}
			teams = append(teams, namespace)
		}
	}
	slices.Sort(teams)
	return teams
}

// namespaceForGroup builds a namespace name from the configured prefix and an
// identity-provider group, sanitizing the group to a DNS-1123 label. It returns
// an empty string when the result cannot be a valid namespace.
func namespaceForGroup(prefix, group string) string {
	sanitized := sanitizeDNSLabel(group)
	if sanitized == "" {
		return ""
	}
	namespace := prefix + sanitized
	if len(namespace) > 63 {
		namespace = strings.TrimRight(namespace[:63], "-")
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) != 0 {
		return ""
	}
	return namespace
}

func sanitizeDNSLabel(value string) string {
	var builder strings.Builder
	pendingDash := false
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingDash && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			pendingDash = false
			builder.WriteRune(r)
			continue
		}
		pendingDash = true
	}
	return strings.Trim(builder.String(), "-")
}

type dashboardCapabilities struct {
	isAdmin            bool
	canAdminister      bool
	canCreateApps      bool
	canDeleteApps      bool
	canUpdateApps      bool
	canCreateResources bool
	canDeleteResources bool
}

// administerPermission is the representative action the Administration area
// needs: managing the ClusterRoles and ClusterRoleBindings behind definition and
// cluster access. Requiring it (via SelfSubjectAccessReview as the user) means an
// OIDC user is an administrator only when the management cluster's RBAC already
// lets them do what the panel does, so the panel cannot escalate privileges.
var administerPermission = authorizationv1.ResourceAttributes{Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings", Verb: "create"}

// impersonatingClient returns the cached impersonating client for the session.
// Local sessions and configurations without a factory return nil, meaning the
// dashboard ServiceAccount client is used directly.
func (a *authHandler) impersonatingClient(session dashboardSession) (client.Client, error) {
	if a.clientFactory == nil || session.authMethod != authMethodOIDC || session.username == "" {
		return nil, nil
	}
	groups := append([]string{authenticatedGroup}, session.groups...)
	key := session.username + "\x00" + strings.Join(groups, "\x1f")
	if cached, ok := a.clients.Load(key); ok {
		return cached.(client.Client), nil
	}
	c, err := a.clientFactory(session.username, groups)
	if err != nil {
		return nil, err
	}
	a.clients.Store(key, c)
	return c, nil
}

// capabilities reports what the session may do. It asks the management cluster
// with a SelfSubjectAccessReview performed as the impersonated user, so the
// answer always reflects the cluster's RBAC rather than any rule in this app.
func (a *authHandler) capabilities(ctx context.Context, session dashboardSession, impersonated client.Client) dashboardCapabilities {
	if session.authMethod == authMethodLocal || impersonated == nil {
		return dashboardCapabilities{
			isAdmin:            true,
			canAdminister:      true,
			canCreateApps:      true,
			canDeleteApps:      true,
			canUpdateApps:      true,
			canCreateResources: true,
			canDeleteResources: true,
		}
	}
	namespaces := teamsFromContext(ctx)
	if len(namespaces) == 0 {
		namespaces = []string{a.namespace}
	}
	allowed := func(resource, verb string) bool {
		for _, namespace := range namespaces {
			attributes := authorizationv1.ResourceAttributes{Namespace: namespace, Group: platformAPIGroup, Resource: resource, Verb: verb}
			if selfSubjectAccessAllowed(ctx, impersonated, attributes) {
				return true
			}
		}
		return false
	}
	clusters := authorizationv1.ResourceAttributes{Group: platformAPIGroup, Resource: resourceClusters, Verb: "patch"}
	return dashboardCapabilities{
		isAdmin:            selfSubjectAccessAllowed(ctx, impersonated, clusters),
		canAdminister:      selfSubjectAccessAllowed(ctx, impersonated, administerPermission),
		canCreateApps:      allowed(resourceApplications, "create"),
		canDeleteApps:      allowed(resourceApplications, "delete"),
		canUpdateApps:      allowed(resourceApplications, "update"),
		canCreateResources: allowed(resourceResources, "create"),
		canDeleteResources: allowed(resourceResources, "delete"),
	}
}

func selfSubjectAccessAllowed(ctx context.Context, c client.Client, attributes authorizationv1.ResourceAttributes) bool {
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attributes},
	}
	if err := c.Create(ctx, review); err != nil {
		return false
	}
	return review.Status.Allowed
}
