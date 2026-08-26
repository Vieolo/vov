package vov

import (
	"context"
	"net/http"
	"slices"
)

// User is the principal a request acts as. It is behavioral and deliberately
// tiny: vov asks it yes-or-no questions and never reads its storage. Capability
// lives on the model — the app decides what a role or a permission means — while
// policy lives on the endpoint declaration, where it can be read and reviewed.
//
// A model that does not use roles or permissions answers false, which is both
// honest and fail-closed: an endpoint that later requires one is refused rather
// than accidentally opened.
//
// Every method is called on the user its [Authenticator] returned, once per
// request. A model whose answers need I/O — a paid-until check, a permission
// table — should resolve them while it is being built and answer from memory, so
// a route requiring two permissions does not become two queries.
type User interface {
	// IsAuthenticated reports whether the request carries a valid principal.
	IsAuthenticated() bool

	// HasRole reports whether the user holds the named role.
	HasRole(role string) bool

	// HasPermission reports whether the user holds the named permission.
	HasPermission(permission string) bool
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

// AuthMod is an [Endpoint]'s auth declaration: whether a user is required, and
// what that user must hold. The zero value requires an authenticated user and
// nothing more; [NoAuth] opts out; [AuthRoles] and [AuthPermissions] add
// requirements on top.
//
// Roles and permissions combine differently, and the asymmetry is deliberate:
//
//	roles       — any-of. "admin or owner may do this."
//	permissions — all-of, every one checked. "this needs both of these."
//
// Build one with the constructors; the fields are unexported so that there is a
// single way to express a requirement, and so the zero value stays meaningful.
type AuthMod struct {
	mode        AuthModMode
	roles       []string
	permissions []string
}

// NoAuth exempts an endpoint from the app's authentication requirement: no user
// is resolved and none is required. Reach for it deliberately — a route that
// says nothing is protected by default, and this is the visible, greppable token
// that says otherwise.
func NoAuth() AuthMod {
	return AuthMod{mode: AuthModModeNone}
}

// AuthRoles requires an authenticated user holding at least one of the named
// roles. With no arguments it is the same as the zero value: authentication and
// nothing more.
func AuthRoles(roles ...string) AuthMod {
	return AuthMod{roles: roles}
}

// AuthPermissions requires an authenticated user holding every one of the named
// permissions. With no arguments it is the same as the zero value.
func AuthPermissions(permissions ...string) AuthMod {
	return AuthMod{permissions: permissions}
}

// WithRoles adds an any-of role requirement to an existing declaration, so that
// role and permission requirements can be combined:
//
//	vov.AuthPermissions("billing.active").WithRoles("admin", "owner")
func (a AuthMod) WithRoles(roles ...string) AuthMod {
	a.roles = append(append([]string(nil), a.roles...), roles...)
	return a
}

// WithPermissions adds an all-of permission requirement to an existing
// declaration:
//
//	vov.AuthRoles("admin").WithPermissions("billing.active")
func (a AuthMod) WithPermissions(permissions ...string) AuthMod {
	a.permissions = append(append([]string(nil), a.permissions...), permissions...)
	return a
}

// required reports whether the endpoint needs an authenticated user.
func (a AuthMod) required() bool {
	return a.mode == AuthModModeRequired
}

// isZero reports whether the declaration says nothing at all — no opt-out and no
// requirements. AuthMod holds slices and so cannot be compared with ==.
func (a AuthMod) isZero() bool {
	return a.mode == AuthModModeRequired && !a.constrained()
}

// constrained reports whether the endpoint demands anything of the user beyond
// being authenticated.
func (a AuthMod) constrained() bool {
	return len(a.roles) > 0 || len(a.permissions) > 0
}

// satisfiedBy reports whether u meets the endpoint's role and permission
// requirements: any of the roles, and all of the permissions.
func (a AuthMod) satisfiedBy(u User) bool {
	if len(a.roles) > 0 && !slices.ContainsFunc(a.roles, u.HasRole) {
		return false
	}
	for _, p := range a.permissions {
		if !u.HasPermission(p) {
			return false
		}
	}
	return true
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

// authGuard wraps next so that it runs only for a request whose user is
// authenticated and satisfies the endpoint's [AuthMod], and stashes that user in
// the request context.
//
// It is applied inside the endpoint's middleware chain and outside the handler.
// Inside, so the middleware still sees rejected requests — a 401 is logged and
// carries a request id like any other response, and a rate limiter still shields
// the credential lookup. Outside the handler, so a handler is never reached
// without a user that already met every requirement. It is deliberately not part
// of a [MiddlewareStack]: no endpoint can drop its authentication by choosing a
// different stack.
//
// The two refusals are different facts and get different codes: 401 means vov
// does not know who you are, 403 means it does and the answer is still no.
// Neither says which requirement failed — that would tell an attacker what to
// look for.
func authGuard(next http.Handler, auth Authenticator, mod AuthMod) http.Handler {
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
		if !mod.satisfiedBy(u) {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(ContextWithUser(r.Context(), u)))
	})
}
