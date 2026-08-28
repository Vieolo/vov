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

	// Modes limits enforcement to the listed channels. Nil means every channel
	// [AppConfig.Scopes] governs, or — with no policy at all — every channel.
	//
	// Leaving it nil is the usual case, and says the honest thing: an endpoint
	// override is about *what* this endpoint requires, while which channels have
	// a scope model is a property of the application, already stated once on the
	// policy. Naming modes here overrides that, including onto a channel the
	// policy does not otherwise govern.
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
	// API and MCP are the per-channel defaults, each mapping an HTTP method to
	// the scopes an endpoint that declares none requires. Use the empty string
	// for endpoints declared under [Endpoints.Any].
	//
	//	MCP: map[string][]string{
	//	    http.MethodGet:  {"tasks:read"},
	//	    http.MethodPost: {"tasks:write"},
	//	}
	//
	// A nil map means that channel has no scope model and is not governed, which
	// is the common case for the HTTP API of an application whose OAuth server
	// exists for its assistants: a browser session carries no scopes at all, and
	// governing it would refuse every browser call. Keying by channel is what
	// makes that a fact you can see in one place rather than assemble from two,
	// and it lets the channels differ — an API that gates only deletion while
	// every tool call is scoped is a policy, not a workaround.
	//
	// A method absent from a map requires no scopes on that channel, so gating a
	// single method takes a single entry rather than an enumeration of the rest.
	//
	// Keying by method — rather than listing endpoints — is what keeps this from
	// being a thing to remember. A new mutating endpoint is governed the moment
	// it exists, because its method was already spoken for; there is no per-route
	// line for anyone to forget. That is the whole reason a default lives here at
	// all, and the reason it is the recommended way to use scopes.
	//
	// It is nonetheless all vov can derive. vov does not know an application's
	// scope vocabulary — a token may carry "read", or "investors:delete", or
	// anything else — so it cannot infer a requirement from the method the way
	// [MCPTool.ReadOnly] infers read-only-ness. An app whose scopes are
	// method-shaped gets the requirement for free; one whose scopes are not
	// leaves these empty and declares [Endpoint.ScopeAllOf] per endpoint.
	API map[string][]string
	MCP map[string][]string
}

// allModes are the channels a policy can govern, in the order the manifest
// renders them.
var allModes = []RequestMode{RequestModeAPI, RequestModeMCP}

// byMode returns the policy's method map for one channel.
func (p *ScopePolicy) byMode(m RequestMode) map[string][]string {
	if p == nil {
		return nil
	}
	switch m {
	case RequestModeAPI:
		return p.API
	case RequestModeMCP:
		return p.MCP
	}
	return nil
}

// governs reports whether the policy declares a scope model for m.
func (p *ScopePolicy) governs(m RequestMode) bool { return p.byMode(m) != nil }

// scopeCheck is a rule resolved against one endpoint at construction, so the
// guard does no lookup per request. It is per channel, because the two can
// require different things.
type scopeCheck struct {
	byMode map[RequestMode][]string
}

// requiredIn reports the scopes a request arriving on m must carry.
func (s *scopeCheck) requiredIn(m RequestMode) []string {
	if s == nil {
		return nil
	}
	return s.byMode[m]
}

// satisfiedBy reports whether the granted scopes cover everything required.
//
// A credential that carried none satisfies nothing, which is the fail-closed
// reading and the only honest one: an endpoint that says it needs a scope has
// said the bare credential is not enough.
func satisfiedBy(granted, required []string) bool {
	for _, want := range required {
		if !slices.Contains(granted, want) {
			return false
		}
	}
	return true
}

// resolveScope resolves an endpoint's effective scope requirement.
//
// The endpoint's own rule wins outright when it declares one — including an empty
// [ScopeNone], which is a decision and not an absence. Otherwise each governed
// channel supplies its own per-method default, and the two channels need not
// agree: an API that gates only deletion while every tool call is scoped resolves
// to a requirement on MCP and none on the API for the same endpoint.
func resolveScope(e Endpoint, method string, p *ScopePolicy) *scopeCheck {
	// An open endpoint resolves no credential, so there is nothing to carry a
	// scope and no guard to check one. A default must not reach it: the manifest
	// would render a requirement that is never enforced, which is worse than
	// rendering none — a reviewer would read it as protection.
	if !e.AuthMode.required() {
		return nil
	}

	byMode := map[RequestMode][]string{}

	// The endpoint's own rule wins outright, on every channel it applies to.
	if r := e.ScopeAllOf; r != nil {
		if len(r.AllOf) == 0 {
			return nil // ScopeNone: a decision, and the decision is "none"
		}
		modes := r.Modes
		if modes == nil {
			// Say what, and let the policy have said where. With no policy at
			// all there is nothing to inherit, so it governs everywhere.
			for _, m := range allModes {
				if p == nil || p.governs(m) {
					modes = append(modes, m)
				}
			}
		}
		for _, m := range modes {
			byMode[m] = r.AllOf
		}
	} else {
		// Otherwise each governed channel supplies its own per-method default,
		// and they need not agree.
		for _, m := range allModes {
			if allOf, ok := p.byMode(m)[method]; ok && len(allOf) > 0 {
				byMode[m] = allOf
			}
		}
	}

	if len(byMode) == 0 {
		return nil
	}
	return &scopeCheck{byMode: byMode}
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
// itself a scope, and an [APIConfig.ServerWrappers] entry runs before routing.
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
