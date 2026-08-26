package vov

import "net/http"

// Middleware wraps an http.Handler, following the standard net/http idiom. It is
// declared as data — on an [Endpoint] or as an app-wide default — rather than
// pre-applied to the handler, so that what wraps a route stays readable off the
// declaration.
type Middleware func(http.Handler) http.Handler

// MiddlewareModMode says how an [Endpoint]'s middleware relates to the app-wide
// defaults. Its zero value inherits them, so an Endpoint that says nothing about
// middleware gets the defaults — the common case.
type MiddlewareModMode uint8

const (
	MiddlewareModModeInherit  MiddlewareModMode = iota // use the app default stack unchanged
	MiddlewareModModeExtend                            // app default stack, then these
	MiddlewareModModeOverride                          // exactly these, ignoring the default stack
)

// MiddlewareMod is an [Endpoint]'s middleware declaration, relative to the
// app-wide default stack ([AppConfig.Middleware]). It is deliberately not a plain
// slice: a slice cannot distinguish "inherit the default" from "override with
// nothing" except by nil-versus-empty, which is invisible at a glance and easy to
// flip by accident.
//
// Build one with [ExtendMiddleware], [OverrideMiddleware], or [NoMiddleware]. The
// zero value inherits the default stack, so most endpoints leave the field unset.
type MiddlewareMod struct {
	mode  MiddlewareModMode
	stack []Middleware
}

// ExtendMiddleware keeps the app-wide defaults and adds m inside them: the
// defaults stay outermost, and m runs closest to the handler — inside the auth
// guard, so it can read the user with [UserFrom].
func ExtendMiddleware(m ...Middleware) MiddlewareMod {
	return MiddlewareMod{mode: MiddlewareModModeExtend, stack: m}
}

// OverrideMiddleware replaces the app-wide defaults with exactly m, which runs
// closest to the handler just as with [ExtendMiddleware]. Use it for endpoints
// that need a different stack rather than an additional layer — a webhook
// verified by signature instead of by session, for example.
//
// It cannot switch off authentication: the auth guard is a seam between the
// default phases rather than a member of either, so only [AuthMod] controls it.
func OverrideMiddleware(m ...Middleware) MiddlewareMod {
	return MiddlewareMod{mode: MiddlewareModModeOverride, stack: m}
}

// NoMiddleware runs the handler bare, with neither the app-wide default stack nor
// any addition. It is [OverrideMiddleware] with no arguments, named for the
// benefit of whoever reads the route later.
func NoMiddleware() MiddlewareMod {
	return OverrideMiddleware()
}

// resolve splits the endpoint's effective middleware into the three groups the
// request chain is built from, each outermost first:
//
//	before   — outside the auth guard; runs for every request to the endpoint
//	afterAuth — inside the guard; runs only once a user has been resolved
//	own      — the endpoint's own, innermost, just outside the handler
//
// Both Extend and Override put the endpoint's own middleware in the same place;
// they differ only in whether the app-wide defaults survive around it.
func (s MiddlewareMod) resolve(beforeDefaults, afterAuthDefaults []Middleware) (before, afterAuth, own []Middleware) {
	switch s.mode {
	case MiddlewareModModeOverride:
		return nil, nil, s.stack
	case MiddlewareModModeExtend:
		return beforeDefaults, afterAuthDefaults, s.stack
	default: // MiddlewareModModeInherit
		return beforeDefaults, afterAuthDefaults, nil
	}
}

// apply wraps h in ms, outermost first: with [A, B] the request flows
// A -> B -> h.
func apply(h http.Handler, ms []Middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}
