package vov

import (
	"context"
	"net/http"
)

// User is the principal a request acts as. It is behavioral and deliberately
// tiny: vov asks it questions and never reads its storage. The app's own user
// model implements it, and capability lookups (roles, permissions) are added as
// methods here as policy grows — so that capability lives on the model while
// policy lives on the endpoint declaration.
type User interface {
	// IsAuthenticated reports whether the request carries a valid principal.
	IsAuthenticated() bool
}

// Authenticator resolves the [User] a request acts as. The app supplies it,
// because only the app knows where credentials live — a session cookie, a bearer
// token, a signed header — and how to look them up.
//
// Return values are distinguished on purpose:
//
//	(user, nil) → the request is authenticated as user
//	(nil, nil)  → no credentials were presented; vov answers 401
//	(nil, err)  → the lookup itself failed (database down, and so on); vov
//	              answers 500, because a broken dependency is not a bad password
//
// Return an untyped nil User for "no user". A typed nil pointer is a non-nil
// interface, and vov would call IsAuthenticated on it.
type Authenticator func(*http.Request) (User, error)

// AuthModMode says whether an endpoint requires an authenticated user. Its zero
// value requires one, so an [Endpoint] that says nothing about auth is protected
// — forgetting to declare auth fails closed rather than exposing the route.
type AuthModMode uint8

const (
	AuthModModeRequired AuthModMode = iota // an authenticated user is required
	AuthModModeNone                        // no user is required; the route is open
)

// AuthMod is an [Endpoint]'s auth declaration. The zero value requires an
// authenticated user; [NoAuth] opts out. It is a struct rather than a bool so
// that role and permission requirements can be added to it without changing
// every declaration that already exists.
type AuthMod struct {
	mode AuthModMode
}

// NoAuth exempts an endpoint from the app's authentication requirement: no user
// is resolved and none is required. Reach for it deliberately — a route that
// says nothing is protected by default, and this is the visible, greppable token
// that says otherwise.
func NoAuth() AuthMod {
	return AuthMod{mode: AuthModModeNone}
}

// required reports whether the endpoint needs an authenticated user.
func (a AuthMod) required() bool {
	return a.mode == AuthModModeRequired
}

// userContextKey types the request-context slot holding the resolved user.
type userContextKey struct{}

// ContextWithUser returns a copy of ctx carrying u. vov calls this for every
// endpoint that requires auth; apps rarely need it outside tests.
func ContextWithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userContextKey{}, u)
}

// UserFrom returns the [User] resolved for the request, and whether there was
// one. Handlers on endpoints that require auth can rely on ok being true.
//
// vov intentionally returns the interface rather than a generic concrete type:
// the guard only ever asks yes/no questions, so only the interface belongs here.
// Apps that want their own type back wrap this in a one-line typed accessor.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userContextKey{}).(User)
	return u, ok
}

// authGuard wraps next so that it runs only for an authenticated request, and
// stashes the resolved user in the request context.
//
// It is applied inside the endpoint's middleware chain and outside the handler.
// Inside, so the middleware still sees rejected requests — a 401 is logged and
// carries a request id like any other response, and a rate limiter still shields
// the credential lookup. Outside the handler, so a handler is never reached
// without a user. It is deliberately not part of [MiddlewareMod]: no endpoint
// can drop its authentication by overriding its middleware.
func authGuard(next http.Handler, auth Authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := auth(r)
		if err != nil {
			// The lookup broke. Do not report this as a credential problem.
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		if u == nil || !u.IsAuthenticated() {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(ContextWithUser(r.Context(), u)))
	})
}
