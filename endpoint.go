package vov

import "net/http"

// Middleware wraps an http.Handler, following the standard net/http idiom. It is
// declared as data on an [Endpoint] rather than pre-applied to the handler, so
// that what wraps a route stays readable off the declaration.
type Middleware func(http.Handler) http.Handler

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

	// Middleware wraps Handler, outermost first: with [A, B] the request flows
	// A -> B -> Handler.
	Middleware []Middleware
}

// pattern returns the ServeMux pattern for the endpoint: "METHOD /path" when a
// method is set, or just "/path" when it is not.
func (e Endpoint) pattern() string {
	if e.Method == "" {
		return e.Path
	}
	return e.Method + " " + e.Path
}

// wrapped returns Handler with its middleware applied, outermost first.
func (e Endpoint) wrapped() http.Handler {
	var h http.Handler = e.Handler
	for i := len(e.Middleware) - 1; i >= 0; i-- {
		h = e.Middleware[i](h)
	}
	return h
}
