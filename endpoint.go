package vov

import "net/http"

// Endpoint is the declarative unit of the framework: a handler with the standard
// net/http signature, plus the method, path, and middleware that describe how it
// is served — all as plain data. It is deliberately a value type with no methods
// exported and no hidden state, so it can be read, listed, and (later) checked by
// tools without constructing an [App].
type Endpoint struct {
	// Method is the HTTP method, e.g. "GET" or "POST". An empty Method matches
	// any method: the route is registered without a method restriction.
	Method string

	// Path is the URL path pattern, using net/http ServeMux syntax, e.g.
	// "/projects/{id}". It must be non-empty and begin with "/".
	Path string

	// Handler serves the request. For now it is a plain http.HandlerFunc; a
	// later phase introduces an error-returning handler type.
	Handler http.HandlerFunc

	// Stack names the [MiddlewareStack] from [AppConfig.Stacks] that wraps this
	// endpoint. Empty selects [DefaultStackName], so the common case says
	// nothing. Naming a stack replaces the default outright rather than adding
	// to it; a name that was never declared is a construction error.
	MiddlewareStack string

	// AuthMod declares this endpoint's authentication requirement. The zero
	// value requires an authenticated user, so a route that says nothing is
	// protected; [NoAuth] opts out.
	AuthMod AuthMod
}

// pattern returns the ServeMux pattern for the endpoint: "METHOD /path" when a
// method is set, or just "/path" when it is not.
func (e Endpoint) pattern() string {
	if e.Method == "" {
		return e.Path
	}
	return e.Method + " " + e.Path
}

// wrapped builds the endpoint's request chain from its stack, outside in:
//
//	Pre → [auth guard] → Post → Handler
//
// The guard is the seam between the stack's two halves rather than a member of
// either, so choosing a different stack can never switch authentication off —
// only [AuthMod] does that. See [authGuard]. An endpoint declaring [NoAuth] has
// no guard and no user, so its stack's Post half is skipped.
func (e Endpoint) wrapped(s MiddlewareStack, auth Authenticator) http.Handler {
	var h http.Handler = e.Handler
	if e.AuthMod.required() {
		h = apply(h, s.Post)
		h = authGuard(h, auth)
	}
	return apply(h, s.Pre)
}
