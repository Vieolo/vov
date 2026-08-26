package vov

import "net/http"

// Endpoints is every method of one URL, declared together.
//
// It is separate from [Route] so that it can live beside the handlers that
// implement it — in the same file, next to the code it dispatches to — while the
// [AppConfig.Routes] slice stays a thin mapping from URL to endpoint group. The
// grouping is then visible in both places: the route list says which URLs exist,
// and the handler file says what one URL answers.
//
// Methods are fields rather than map keys, so they cannot be misspelled, an
// editor completes them, and a single go-to-definition shows a URL's whole
// surface. Each carries its own [Endpoint], so — unlike a Django class view — a
// URL's methods need not share configuration: one may take a different
// middleware stack or opt out of auth without affecting its siblings.
//
// A method left empty is simply not registered, and requests using it get the
// 405 and Allow header that net/http derives from the methods that are.
type Endpoints struct {
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

// Route binds one URL to the [Endpoints] that serve it.
type Route struct {
	// Path is the URL path pattern, using net/http ServeMux syntax, e.g.
	// "/projects/{projectId}/rounds". It must be non-empty and begin with "/".
	Path string

	// Endpoints are the methods this URL answers. Declaring none is a
	// construction error: a Route that serves nothing is a mistake, not a
	// placeholder.
	Endpoints Endpoints
}

// methodEndpoint pairs a declared endpoint with the method it answers. An empty
// Method means the endpoint was declared under [Endpoints.Any].
type methodEndpoint struct {
	Method   string
	Endpoint Endpoint
}

// declared returns the group's endpoints in a fixed order, skipping the method
// fields that were left empty. The order is stable so that registration — and
// the route manifest — does not depend on map iteration.
func (e Endpoints) declared() []methodEndpoint {
	all := []methodEndpoint{
		{"", e.Any},
		{http.MethodGet, e.GET},
		{http.MethodHead, e.HEAD},
		{http.MethodPost, e.POST},
		{http.MethodPut, e.PUT},
		{http.MethodPatch, e.PATCH},
		{http.MethodDelete, e.DELETE},
		{http.MethodOptions, e.OPTIONS},
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
