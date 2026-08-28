package vov

import (
	"fmt"
	"net/http"
	"sort"
)

// Middleware wraps an http.Handler, following the standard net/http idiom. It is
// declared as data — inside a [MiddlewareStack] — rather than pre-applied to the
// handler, so that what wraps a route stays readable off the declaration.
type Middleware func(http.Handler) http.Handler

// DefaultStackName is the key in [AppConfig.MiddlewareStacks] whose stack applies to every
// endpoint that does not name one.
const DefaultStackName = "default"

// MiddlewareStack is one named combination of middleware, split by the seam
// where the request's user becomes known.
//
// Stacks are declared once in [AppConfig.MiddlewareStacks] and selected by name from an
// endpoint. Real services converge on a handful of combinations — public,
// authenticated, admin, webhook — so naming them puts each combination in one
// place and turns "what wraps this route?" into a word a reviewer can read off
// the declaration.
type MiddlewareStack struct {
	// Pre runs outside the auth guard, outermost first, for every request that
	// reaches this endpoint — including the ones the guard rejects, which is what
	// keeps a 401 logged and stamped.
	//
	// "Reaches this endpoint" is the limit, and it matters. Anything that must
	// see every request the server receives — CORS, panic recovery, access
	// logging, request ids on unrouted paths — belongs in
	// [APIConfig.ServerWrappers] instead. http.ServeMux answers a request for an
	// undeclared path (404) or an undeclared method (405) itself, before any Pre
	// middleware exists to run, and a CORS preflight is exactly such a request:
	// put CORS here and a browser client silently breaks.
	//
	// What belongs here is endpoint-scoped and must precede authentication: a
	// per-route rate limit, a body-size cap, a webhook signature check. So does
	// anything an endpoint needs while declaring [AuthModeNone], since Post does
	// not run there.
	Pre []Middleware

	// Post runs inside the auth guard, outermost first, so it can read the user
	// with [UserFrom]: audit logging, tenant scoping, per-user rate limits.
	//
	// It runs only on endpoints that require auth. An endpoint declaring
	// [AuthModeNone] has no user, so Post is skipped for it — a stack shared between
	// protected and open endpoints keeps working, but middleware an open
	// endpoint depends on must live in Pre.
	Post []Middleware
}

// resolveStack returns the stack an endpoint uses: the one it names, or the
// default when it names none. A name that is not declared is an error rather
// than a silently empty stack — a route wrapped in nothing because of a typo is
// exactly the failure this design exists to prevent.
//
// A missing default stack is not an error: an app may legitimately have no
// app-wide middleware at all.
func resolveStack(stacks map[string]MiddlewareStack, name string) (MiddlewareStack, error) {
	if name == "" {
		name = DefaultStackName
		if _, ok := stacks[name]; !ok {
			return MiddlewareStack{}, nil
		}
	}
	s, ok := stacks[name]
	if !ok {
		return MiddlewareStack{}, fmt.Errorf("unknown middleware stack %q (declared: %s)", name, declaredStacks(stacks))
	}
	return s, nil
}

// declaredStacks renders the available stack names for an error message.
func declaredStacks(stacks map[string]MiddlewareStack) string {
	if len(stacks) == 0 {
		return "none"
	}
	names := make([]string, 0, len(stacks))
	for n := range stacks {
		names = append(names, fmt.Sprintf("%q", n))
	}
	sort.Strings(names)
	return fmt.Sprint(names)
}

// apply wraps h in ms, outermost first: with [A, B] the request flows
// A -> B -> h.
func apply(h http.Handler, ms []Middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}
