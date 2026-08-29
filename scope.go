package vov

import (
	"context"
	"fmt"
)

// ScopeAllOf declares the scopes a credential must carry, per channel.
//
// A scope is a property of the credential, not of the user, which is what
// separates it from [Endpoint.PermissionsAllOf]
//
// The key of the map determinses the [RequestMode] of the request. Any
// value provided for a request mode will override that of [AppConfig.Scopes] and
// if a key is absent, the app-level scope is used.
//
// The value of the map determines the scopes the credential requires to reach
// the endpoint, all of them. An empty slice means no scope is required for
// the specified key
type ScopeAllOf = map[RequestMode][]string

// ScopeNone requires no scopes on any channel, overriding whatever
// [AppConfig.Scopes] would otherwise apply.
func ScopeNone() ScopeAllOf { return ScopeAllOf{RequestModeAPI: {}, RequestModeMCP: {}} }

// ScopePolicy is the app-wide scope declaration, stating which RequestModes
// requires which scopes for different methods.
//
// A scope defined for a RequestMode and Method combo will be applied to the entire
// app, removing the need to declare the scopes in each endpoint individually.
// If, however, an endpoint requires a different set of scopes than what is described
// in the AppConfig, the endpoint can override the scope requirements.
//
// Any missing field/key or empty slice is interpreted as "no scope required".
//
// Example:
//
//	 Scopes: &vov.ScopePolicy{
//	 	API: map[string][]string{
//				http.MethodGet:    {"tasks:read"},
//				http.MethodPost:   {"tasks:write"},
//				http.MethodDelete:    {"tasks:read", "tasks:delete"},
//			},
//	 	MCP: map[string][]string{
//				http.MethodGet:    {"tasks:read"},
//				http.MethodPost:   {"tasks:write"},
//				http.MethodPut:    {"tasks:read", "tasks:write"},
//			},
//		}
//
// Have in mind that `vov` has no understanding of the vocabulary of your scopes,
// treats any given string equally, and only uses your declaration to make a distinction
type ScopePolicy struct {
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

// scopeCheck is a rule resolved against one endpoint at construction, so the
// guard does no lookup per request. It is per channel, because the two can
// require different things.
type scopeCheck map[RequestMode][]string

// requiredIn reports the scopes a request arriving on m must carry.
func (s scopeCheck) requiredIn(m RequestMode) []string { return s[m] }

// resolveScope resolves an endpoint's effective requirement, per channel.
//
// The endpoint decides a channel by naming it; otherwise the policy's per-method
// default applies. The two channels need not agree.
func resolveScope(e Endpoint, method string, p *ScopePolicy) scopeCheck {
	// An open endpoint resolves no credential, so there is nothing to check a
	// scope against. A default must not reach it: the manifest would render a
	// requirement that is never enforced, and a reviewer would read it as
	// protection.
	if !e.AuthMode.required() {
		return nil
	}

	byMode := make(scopeCheck)
	for _, m := range allModes {
		allOf, declared := e.ScopeAllOf[m]
		if !declared {
			allOf = p.byMode(m)[method]
		}
		if len(allOf) > 0 {
			byMode[m] = allOf
		}
	}

	if len(byMode) == 0 {
		return nil
	}
	return byMode
}

// validateScopes rejects a scope declaration that cannot mean what it says.
func validateScopes(e Endpoint) error {
	if len(e.ScopeAllOf) == 0 {
		return nil
	}
	// A scope is carried by a credential, and an open endpoint resolves none, so
	// the requirement would silently not be enforced. Same reasoning as roles.
	if !e.AuthMode.required() {
		return fmt.Errorf("declares ScopeAllOf but also %s, which resolves no credential to check it against", AuthModeNone)
	}
	for m, allOf := range e.ScopeAllOf {
		if !m.valid() {
			return fmt.Errorf("ScopeAllOf is keyed by an unknown mode %q (use %q or %q)", string(m), RequestModeAPI, RequestModeMCP)
		}
		for _, s := range allOf {
			if s == "" {
				return fmt.Errorf("ScopeAllOf[%s] declares an empty scope name", m)
			}
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
