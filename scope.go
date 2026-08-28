package vov

import (
	"context"
	"fmt"
	"slices"
)

// ScopeRule declares the scopes a credential must carry to reach an endpoint.
//
// A scope is a property of the *credential*, not of the principal — which is what
// separates it from [Endpoint.PermissionsAllOf] and why it is a second axis
// rather than a second spelling. The same person holding a read-only token has
// identical permissions and fewer scopes: an OAuth token carries at most its
// owner's authority and usually less. Permissions answer "may this user do this";
// scopes answer "was this key cut for it".
//
// Every listed scope is required, which is what OAuth means by a scope set.
type ScopeRule struct {
	// AllOf are the scopes the credential must carry, every one of them. Empty
	// means the endpoint requires none — see [ScopeNone], which says that
	// deliberately rather than by omission.
	AllOf []string

	// Modes limits enforcement to the listed channels. Nil inherits
	// [ScopePolicy.Modes], and if there is no policy either, enforces on every
	// channel.
	//
	// It exists because a scope model usually belongs to one channel. An app
	// whose OAuth server exists only for its MCP tools has no scopes on a browser
	// session at all, so enforcing them on the HTTP API would refuse every
	// browser call; naming the channel is how that is said once instead of being
	// worked around per endpoint.
	Modes []RequestMode
}

// ScopeNone declares that an endpoint requires no scopes, on a policy that
// otherwise demands every endpoint name some. It is the greppable opt-out, in the
// spirit of [AuthModeNone]: an exception someone wrote, not one that happened.
//
// It is a distinct value from a nil [Endpoint.ScopeAllOf], which means "say
// nothing and take the policy's default".
func ScopeNone() *ScopeRule { return &ScopeRule{} }

// ScopePolicy is the app-wide half of the scope declaration: which channels
// enforce scopes at all, and what an endpoint that names none requires.
//
// It exists so that the deployment fact — "this application's scopes govern its
// MCP channel" — is written once rather than repeated on every endpoint. An
// endpoint restating it would be exactly the duplication [Route] exists to
// remove, and across sixty endpoints it is a line somebody eventually forgets.
type ScopePolicy struct {
	// ByMethod supplies the requirement for an endpoint that declares none,
	// keyed by HTTP method: {"GET": {"read"}, "POST": {"write"}}. Use the empty
	// string for endpoints declared under [Endpoints.Any].
	//
	// It is the whole of what vov can derive, and it is optional. vov does not
	// know an application's scope vocabulary — a token may carry "read", or
	// "investors:delete", or anything else — so it cannot infer a requirement
	// from the method the way [MCPTool.ReadOnly] infers read-only-ness. A method
	// mapping is the most it can honestly offer: an app whose scopes are shaped
	// that way gets the requirement for free, and an app whose scopes are not
	// leaves this empty and declares per endpoint.
	//
	// Whichever it uses, nothing is silently unscoped: see [ScopePolicy] on the
	// construction error.
	ByMethod map[string][]string

	// Modes are the channels this policy enforces on. Nil enforces on every
	// channel. An endpoint's own [ScopeRule.Modes] overrides it.
	Modes []RequestMode
}

// scopeCheck is a rule resolved against one endpoint at construction, so the
// guard does no lookup per request.
type scopeCheck struct {
	allOf []string
	modes []RequestMode // nil means every mode
}

// enforcedIn reports whether this check applies to a request arriving on m.
func (s *scopeCheck) enforcedIn(m RequestMode) bool {
	if s == nil || len(s.allOf) == 0 {
		return false
	}
	return s.modes == nil || slices.Contains(s.modes, m)
}

// satisfiedBy reports whether the granted scopes cover everything required.
//
// A credential that carried none satisfies nothing, which is the fail-closed
// reading and the only honest one: an endpoint that says it needs a scope has
// said the bare credential is not enough.
func (s *scopeCheck) satisfiedBy(granted []string) bool {
	for _, want := range s.allOf {
		if !slices.Contains(granted, want) {
			return false
		}
	}
	return true
}

// resolveScope resolves an endpoint's effective scope requirement.
//
// The endpoint's own rule wins outright when it declares one — including an empty
// [ScopeNone], which is a decision and not an absence. Otherwise the policy's
// per-method default applies. Modes fall back separately, so an endpoint can
// override *what* it requires without also having to restate *where*.
func resolveScope(e Endpoint, method string, p *ScopePolicy) *scopeCheck {
	// An open endpoint resolves no credential, so there is nothing to carry a
	// scope and no guard to check one. A default must not reach it: the manifest
	// would render a requirement that is never enforced, which is worse than
	// rendering none — a reviewer would read it as protection.
	if !e.AuthMode.required() {
		return nil
	}

	var modes []RequestMode
	if p != nil {
		modes = p.Modes
	}

	if r := e.ScopeAllOf; r != nil {
		if r.Modes != nil {
			modes = r.Modes
		}
		return &scopeCheck{allOf: r.AllOf, modes: modes}
	}
	if p != nil {
		if allOf, ok := p.ByMethod[method]; ok {
			return &scopeCheck{allOf: allOf, modes: modes}
		}
	}
	return nil
}

// policyModes reports the channels a policy enforces on, expanded.
func policyModes(p *ScopePolicy) []RequestMode {
	if p == nil {
		return nil
	}
	if p.Modes == nil {
		return []RequestMode{RequestModeAPI, RequestModeMCP}
	}
	return p.Modes
}

// validateScopes rejects a scope declaration that cannot mean what it says.
func validateScopes(e Endpoint) error {
	r := e.ScopeAllOf
	if r == nil {
		return nil
	}
	// A scope is carried by a credential, and an open endpoint resolves none —
	// so the requirement would silently not be enforced. Same reasoning as roles.
	if !e.AuthMode.required() {
		return fmt.Errorf("declares ScopeAllOf but also %s, which resolves no credential to check it against", AuthModeNone)
	}
	for _, s := range r.AllOf {
		if s == "" {
			return fmt.Errorf("declares an empty scope name")
		}
	}
	for _, m := range r.Modes {
		if !m.valid() {
			return fmt.Errorf("ScopeAllOf declares an unknown Modes entry %q (use %q or %q)", string(m), RequestModeAPI, RequestModeMCP)
		}
	}
	return nil
}

// scopeContextKey types the context slot holding the credential's scopes.
//
// There is deliberately no exported setter. The guard trusts what it reads here,
// so the only way in is [AuthResponse.SetScopes] — a seam vov owns and calls
// itself. Were this writable by application code, any middleware could grant
// itself a scope, and an [AppConfig.ServerWrappers] entry runs before routing.
type scopeContextKey struct{}

// ScopesFrom returns the scopes the request's credential carried, and whether the
// credential declared any. A handler on an endpoint behind a scope rule can rely
// on ok being true.
func ScopesFrom(ctx context.Context) ([]string, bool) {
	s, ok := ctx.Value(scopeContextKey{}).([]string)
	return s, ok
}

// contextWithScopes returns a copy of ctx carrying the granted scopes.
func contextWithScopes(ctx context.Context, scopes []string) context.Context {
	return context.WithValue(ctx, scopeContextKey{}, scopes)
}

// requireScopeDecision rejects an endpoint that a policy governs but that nothing
// gave a requirement.
//
// This is what makes [AppConfig.Scopes] worth setting rather than being one more
// optional field. Without it, adding a mutating endpoint and forgetting its scope
// leaves a read-only credential able to call it — an omission that is invisible
// in review, because the absence of a line is what is wrong. With it, the build
// stops and names the endpoint.
//
// Reachability is decided per channel: every endpoint answers the HTTP API, but
// only one carrying an [MCPTool] can be reached over MCP, so a policy scoped to
// the MCP channel says nothing about the rest of the app.
func requireScopeDecision(e Endpoint, resolved *scopeCheck, p *ScopePolicy) error {
	if p == nil || resolved != nil || !e.AuthMode.required() {
		return nil
	}
	for _, m := range policyModes(p) {
		if m == RequestModeMCP && e.MCPTool == nil {
			continue // not reachable on that channel
		}
		return fmt.Errorf("AppConfig.Scopes governs the %s channel but this endpoint declares no ScopeAllOf "+
			"and no ScopePolicy.ByMethod entry covers its method (declare vov.ScopeNone() to require none)", m)
	}
	return nil
}
