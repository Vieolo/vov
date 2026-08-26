package vov

import "net/http"

// Route is every endpoint of a single URL, declared together.
//
// One URL is one object: the methods it answers are fields, so opening a Route
// shows the whole surface of that URL at once, and a single go-to-definition
// answers "what can be done here?". Handlers that operate on the same resource
// are neighbours in the source rather than four unrelated registrations that
// happen to repeat the same path string.
//
// Each method carries its own [Endpoint], so — unlike a Django class view — a
// URL's methods do not have to share configuration: one may take a different
// middleware stack or opt out of auth without affecting its siblings.
//
// A method whose Endpoint has no handler is simply not registered, and requests
// using it get the 405 and Allow header that net/http derives from the methods
// that are.
type Route struct {
	// Path is the URL path pattern, using net/http ServeMux syntax, e.g.
	// "/projects/{projectId}/rounds". It must be non-empty and begin with "/".
	Path string

	// Any answers every method that no specific field below claims. It is for
	// catch-alls — a static file tree, a single-page-app fallback — not for
	// ordinary endpoints, which should name their method so the declaration
	// says what the URL supports.
	Any Endpoint

	GET Endpoint

	// HEAD rarely needs declaring: net/http already answers HEAD with the GET
	// endpoint, and reports it in Allow. Declare it only to handle HEAD
	// differently from GET.
	HEAD Endpoint

	POST    Endpoint
	PUT     Endpoint
	PATCH   Endpoint
	DELETE  Endpoint
	OPTIONS Endpoint
}

// methodEndpoint pairs a declared endpoint with the method it answers. An empty
// Method means the endpoint was declared under [Route.Any].
type methodEndpoint struct {
	Method   string
	Endpoint Endpoint
}

// declared returns the route's endpoints in a fixed order, skipping the method
// fields that were left empty. The order is stable so that registration — and
// the route manifest later — does not depend on map iteration.
func (r Route) declared() []methodEndpoint {
	all := []methodEndpoint{
		{"", r.Any},
		{http.MethodGet, r.GET},
		{http.MethodHead, r.HEAD},
		{http.MethodPost, r.POST},
		{http.MethodPut, r.PUT},
		{http.MethodPatch, r.PATCH},
		{http.MethodDelete, r.DELETE},
		{http.MethodOptions, r.OPTIONS},
	}
	out := make([]methodEndpoint, 0, len(all))
	for _, me := range all {
		if me.Endpoint.declared() {
			out = append(out, me)
		}
	}
	return out
}

// pattern returns the ServeMux pattern for one of the route's methods:
// "METHOD /path", or just "/path" for an endpoint declared under Any.
func (r Route) pattern(method string) string {
	if method == "" {
		return r.Path
	}
	return method + " " + r.Path
}
