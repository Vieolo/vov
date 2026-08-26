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

	// Middleware declares this endpoint's middleware relative to the app-wide
	// default stack ([AppConfig.Middleware]). The zero value inherits that stack;
	// [ExtendMiddleware] adds to it, [OverrideMiddleware] replaces it, and
	// [NoMiddleware] runs the handler bare.
	MiddlewareMod MiddlewareMod

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

// wrapped builds the endpoint's request chain, from the outside in:
//
//	before → [auth guard] → afterAuth → own → Handler
//
// The guard is a seam rather than a list element, so no [MiddlewareMod] can
// remove it — see [authGuard]. When the endpoint declares [NoAuth] there is no
// guard and no user, so the app-wide after-auth middleware is skipped; the
// endpoint's own middleware still runs.
func (e Endpoint) wrapped(beforeDefaults, afterAuthDefaults []Middleware, auth Authenticator) http.Handler {
	before, afterAuth, own := e.MiddlewareMod.resolve(beforeDefaults, afterAuthDefaults)

	h := apply(e.Handler, own)
	if e.AuthMod.required() {
		h = apply(h, afterAuth)
		h = authGuard(h, auth)
	}
	return apply(h, before)
}
