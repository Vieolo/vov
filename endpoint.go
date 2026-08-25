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
	Middleware MiddlewareStack
}

// pattern returns the ServeMux pattern for the endpoint: "METHOD /path" when a
// method is set, or just "/path" when it is not.
func (e Endpoint) pattern() string {
	if e.Method == "" {
		return e.Path
	}
	return e.Method + " " + e.Path
}

// wrapped returns Handler with its effective middleware applied, outermost
// first, resolved against the app-wide default stack.
func (e Endpoint) wrapped(defaults []Middleware) http.Handler {
	return apply(e.Handler, e.Middleware.resolve(defaults))
}
