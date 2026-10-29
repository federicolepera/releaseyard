package dashboard

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
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
func (a *authHandler) teamNamespacesForSession(ctx context.Context, session dashboardSession, impersonated client.Client) ([]string, error) {
	if session.authMethod != authMethodOIDC || impersonated == nil {
		return nil, nil
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
		allowed, err := selfSubjectAccessAllowedWithError(ctx, impersonated, attributes)
		if err != nil {
			return nil, err
		}
		if allowed {
			seen[namespace] = struct{}{}
			teams = append(teams, namespace)
		}
	}
	slices.Sort(teams)
	return teams, nil
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
	isAdmin                bool
	canAdminister          bool
	canManageProjectAccess bool
	canCreateApps          bool
	canDeleteApps          bool
	canUpdateApps          bool
	canCreateResources     bool
	canDeleteResources     bool
}

// administerPermission is the representative action the Administration area
// needs: managing the ClusterRoles and ClusterRoleBindings behind catalog access. Requiring it (via SelfSubjectAccessReview as the user) means an
// OIDC user is an administrator only when the management cluster's RBAC already
// lets them do what the panel does, so the panel cannot escalate privileges.
var administerPermission = authorizationv1.ResourceAttributes{Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings", Verb: verbCreate}

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
func fullCapabilities() dashboardCapabilities {
	return dashboardCapabilities{
		isAdmin:                true,
		canAdminister:          true,
		canManageProjectAccess: true,
		canCreateApps:          true,
		canDeleteApps:          true,
		canUpdateApps:          true,
		canCreateResources:     true,
		canDeleteResources:     true,
	}
}

func (a *authHandler) capabilities(ctx context.Context, session dashboardSession, impersonated client.Client) (dashboardCapabilities, error) {
	if session.authMethod == authMethodLocal || impersonated == nil {
		return fullCapabilities(), nil
	}
	namespaces := teamsFromContext(ctx)
	if len(namespaces) == 0 {
		namespaces = []string{a.namespace}
	}
	allowed := func(resource, verb string) (bool, error) {
		for _, namespace := range namespaces {
			attributes := authorizationv1.ResourceAttributes{Namespace: namespace, Group: platformAPIGroup, Resource: resource, Verb: verb}
			ok, err := selfSubjectAccessAllowedWithError(ctx, impersonated, attributes)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	clusters := authorizationv1.ResourceAttributes{Group: platformAPIGroup, Resource: resourceClusters, Verb: "patch"}
	isAdmin, err := selfSubjectAccessAllowedWithError(ctx, impersonated, clusters)
	if err != nil {
		return dashboardCapabilities{}, err
	}
	canAdminister, err := selfSubjectAccessAllowedWithError(ctx, impersonated, administerPermission)
	if err != nil {
		return dashboardCapabilities{}, err
	}
	canManageProjectAccess, err := a.canManageAnyProjectAccess(ctx, impersonated)
	if err != nil {
		return dashboardCapabilities{}, err
	}
	canCreateApps, err := allowed(resourceApplications, "create")
	if err != nil {
		return dashboardCapabilities{}, err
	}
	canDeleteApps, err := allowed(resourceApplications, "delete")
	if err != nil {
		return dashboardCapabilities{}, err
	}
	canUpdateApps, err := allowed(resourceApplications, "update")
	if err != nil {
		return dashboardCapabilities{}, err
	}
	canCreateResources, err := allowed(resourceResources, "create")
	if err != nil {
		return dashboardCapabilities{}, err
	}
	canDeleteResources, err := allowed(resourceResources, "delete")
	if err != nil {
		return dashboardCapabilities{}, err
	}
	return dashboardCapabilities{
		isAdmin:                isAdmin,
		canAdminister:          canAdminister,
		canManageProjectAccess: canManageProjectAccess,
		canCreateApps:          canCreateApps,
		canDeleteApps:          canDeleteApps,
		canUpdateApps:          canUpdateApps,
		canCreateResources:     canCreateResources,
		canDeleteResources:     canDeleteResources,
	}, nil
}

func selfSubjectAccessAllowed(ctx context.Context, c client.Client, attributes authorizationv1.ResourceAttributes) bool {
	allowed, err := selfSubjectAccessAllowedWithError(ctx, c, attributes)
	return err == nil && allowed
}

func selfSubjectAccessAllowedWithError(ctx context.Context, c client.Client, attributes authorizationv1.ResourceAttributes) (bool, error) {
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attributes},
	}
	if err := c.Create(ctx, review); err != nil {
		return false, err
	}
	return review.Status.Allowed, nil
}

const authzCacheTTL = 5 * time.Minute

type cachedAuthorization struct {
	teams        []string
	capabilities dashboardCapabilities
	expires      time.Time
}

func authorizationCacheKey(session dashboardSession) string {
	groups := append([]string(nil), session.groups...)
	slices.Sort(groups)
	return session.issuer + "\x00" + session.username + "\x00" + strings.Join(groups, "\x1f")
}

// authorization resolves the team namespaces and dashboard capabilities for a
// session. The Kubernetes authorization checks are cached briefly because they
// are the same for every API request in a page load and are expensive to repeat.
// Administrative API and page requests bypass the cache so a revoked
// administrator cannot keep using the panel until the cache expires.
func (a *authHandler) authorization(ctx context.Context, session dashboardSession, impersonated client.Client, refresh bool) ([]string, dashboardCapabilities) {
	if session.authMethod == authMethodLocal || impersonated == nil {
		return nil, fullCapabilities()
	}
	key := authorizationCacheKey(session)
	if !refresh {
		if cached, ok := a.authzCache.Load(key); ok {
			entry := cached.(cachedAuthorization)
			if a.now().Before(entry.expires) {
				return entry.teams, entry.capabilities
			}
		}
	}
	value, err, _ := a.authzGroup.Do(key, func() (any, error) {
		if !refresh {
			if cached, ok := a.authzCache.Load(key); ok {
				entry := cached.(cachedAuthorization)
				if a.now().Before(entry.expires) {
					return entry, nil
				}
			}
		}
		// Detach from the first caller's cancellation so concurrent requests do
		// not all fail when one of them is aborted.
		authCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		teams, err := a.teamNamespacesForSession(authCtx, session, impersonated)
		if err != nil {
			return nil, err
		}
		capabilities, err := a.capabilities(withTeams(authCtx, teams), session, impersonated)
		if err != nil {
			return nil, err
		}
		entry := cachedAuthorization{teams: teams, capabilities: capabilities, expires: a.now().Add(authzCacheTTL)}
		a.authzCache.Store(key, entry)
		return entry, nil
	})
	if err != nil {
		if !refresh {
			if cached, ok := a.authzCache.Load(key); ok {
				entry := cached.(cachedAuthorization)
				return entry.teams, entry.capabilities
			}
		}
		return nil, dashboardCapabilities{}
	}
	entry := value.(cachedAuthorization)
	return entry.teams, entry.capabilities
}

func (a *authHandler) canManageAnyProjectAccess(ctx context.Context, impersonated client.Client) (bool, error) {
	if impersonated == nil {
		return true, nil
	}
	projects := &platformv1alpha1.ProjectList{}
	if err := a.k8s.List(ctx, projects); err != nil {
		return false, err
	}
	for i := range projects.Items {
		namespace := projects.Items[i].Spec.Namespace
		if namespace == "" {
			continue
		}
		attributes := authorizationv1.ResourceAttributes{Namespace: namespace, Group: "rbac.authorization.k8s.io", Resource: "rolebindings", Verb: verbCreate}
		ok, err := selfSubjectAccessAllowedWithError(ctx, impersonated, attributes)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
