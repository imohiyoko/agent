package auth

import "github.com/imohiyoko/agent/internal/spec"

// Actions checked by Allowed. See ADR 0003.
const (
	ActNamespaceRead = "namespace.read"
	ActRunRead       = "run.read"
	ActRunCreate     = "run.create"
	ActRunStop       = "run.stop"
	ActAuditRead     = "audit.read"
)

var roleActions = map[string]map[string]bool{
	spec.RoleViewer: {
		ActNamespaceRead: true, ActRunRead: true,
	},
	spec.RoleOperator: {
		ActNamespaceRead: true, ActRunRead: true, ActRunCreate: true, ActRunStop: true,
	},
	spec.RoleAdmin: {
		ActNamespaceRead: true, ActRunRead: true, ActRunCreate: true, ActRunStop: true, ActAuditRead: true,
	},
}

// Authorizer decides what a principal may do. It is the only place where
// that decision is made.
type Authorizer struct {
	// Admins are admin in every namespace.
	Admins map[string]bool
}

// Role returns the principal's effective role in ns, or "".
func (a *Authorizer) Role(principal string, ns *spec.Namespace) string {
	if principal == "" || ns == nil {
		return ""
	}
	if a.Admins[principal] {
		return spec.RoleAdmin
	}
	return ns.RoleOf(principal)
}

// Allowed reports whether principal may do action in ns. Unknown principals,
// namespaces, roles and actions are all denied.
func (a *Authorizer) Allowed(principal string, ns *spec.Namespace, action string) bool {
	return roleActions[a.Role(principal, ns)][action]
}
