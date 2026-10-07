package kernel

// Values of the virtual `@request.auth.kind` rule field.
const (
	AuthKindField     = "kind"
	AuthKindGuest     = "guest"
	AuthKindUser      = "user"
	AuthKindSuperuser = "superuser"
	AuthKindAgent     = "agent"

	// CollectionNameAgents is the system collection of MCP agent identities
	// (modules/mcp). A record of it as RequestInfo.Auth is an agent.
	CollectionNameAgents = "_agents"
)

// AuthKindOf returns the value of `@request.auth.kind` for an auth record
// (nil = guest). A real field called "kind" on the auth collection takes
// precedence in rules, for backward compatibility.
func AuthKindOf(auth *Record) string {
	if auth == nil || auth.Collection() == nil {
		return AuthKindGuest
	}
	switch auth.Collection().Name {
	case CollectionNameSuperusers:
		return AuthKindSuperuser
	case CollectionNameAgents:
		return AuthKindAgent
	}
	return AuthKindUser
}
