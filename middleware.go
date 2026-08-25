package vov

import "net/http"

// Middleware wraps an http.Handler, following the standard net/http idiom. It is
// declared as data — on an [Endpoint] or as an app-wide default — rather than
// pre-applied to the handler, so that what wraps a route stays readable off the
// declaration.
type Middleware func(http.Handler) http.Handler

// stackMode says how an [Endpoint]'s middleware relates to the app-wide default
// stack. Its zero value inherits, so an Endpoint that says nothing about
// middleware gets the default — the common case.
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

// ExtendMiddleware keeps the app-wide default stack and adds m inside it: the
// defaults stay outermost, and m runs between them and the handler.
func ExtendMiddleware(m ...Middleware) MiddlewareMod {
	return MiddlewareMod{mode: MiddlewareModModeExtend, stack: m}
}

// OverrideMiddleware replaces the app-wide default stack with exactly m. Use it
// for endpoints that need a different stack rather than an additional layer — a
// webhook verified by signature instead of by session, for example.
func OverrideMiddleware(m ...Middleware) MiddlewareMod {
	return MiddlewareMod{mode: MiddlewareModModeOverride, stack: m}
}

// NoMiddleware runs the handler bare, with neither the app-wide default stack nor
// any addition. It is [OverrideMiddleware] with no arguments, named for the
// benefit of whoever reads the route later.
func NoMiddleware() MiddlewareMod {
	return OverrideMiddleware()
}

// resolve returns the effective middleware for the endpoint, outermost first,
// given the app-wide default stack.
func (s MiddlewareMod) resolve(defaults []Middleware) []Middleware {
	switch s.mode {
	case MiddlewareModModeOverride:
		return s.stack
	case MiddlewareModModeExtend:
		out := make([]Middleware, 0, len(defaults)+len(s.stack))
		out = append(out, defaults...)
		out = append(out, s.stack...)
		return out
	default: // stackInherit
		return defaults
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
