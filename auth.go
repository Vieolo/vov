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

	// Tier reports the user's paid tier: 0 for free, and higher numbers for
	// higher subscriptions. It is ordinal, so an endpoint declaring
	// [Endpoint.MinTier] admits every tier at or above it and a subscription
	// upgrade never removes access.
	//
	// A model with nothing to sell returns 0, which is honest and fail-closed:
	// an endpoint that later gates on tier refuses rather than opening.
	Tier() int
}

// AuthResponse is the part of the response an [Authenticator] may touch: its
// headers, and nothing else. vov owns the status and the body, because vov is
// what decides whether the outcome is 401, 403, 402, or the handler's own
// response — so an authenticator can add a header without any risk of writing a
// response that collides with the one about to be sent.
//
// The motivating case is a credential that must be revoked as it is rejected. A
// long-lived signed cookie belonging to an account that no longer exists should
// be cleared while it is refused; otherwise the browser re-sends the dead
// credential on every request until it expires on its own, which for a 30-day
// session means 30 days.
//
// Rotating a session on successful authentication — sliding expiry — is the same
// mechanism, on the other branch.
type AuthResponse struct {
	header http.Header
}

// Header returns the response headers, for anything [AuthResponse.SetCookie]
// does not cover — a WWW-Authenticate challenge, for instance.
func (a AuthResponse) Header() http.Header {
	return a.header
}

// SetCookie adds a Set-Cookie header, like http.SetCookie does for a
// ResponseWriter. Clearing a cookie is the same call with MaxAge set to -1 and
// the Name and Path that were used to write it.
func (a AuthResponse) SetCookie(c *http.Cookie) {
	if c == nil {
		return
	}
	if v := c.String(); v != "" {
		a.header.Add("Set-Cookie", v)
	}
}

// Authenticator resolves the [User] a request acts as. The app supplies it,
// because only the app knows where credentials live — a session cookie, a bearer
// token, a signed header — and how to look them up.
//
// It receives an [AuthResponse] so that it can set headers on the way past —
// clearing a revoked cookie, rotating a live one. Headers set there survive
// whatever vov writes next, including a refusal.
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
type Authenticator func(resp AuthResponse, r *http.Request) (User, error)

// AuthMode says whether an [Endpoint] needs an authenticated user.
//
// The zero value — an endpoint that says nothing — means [AuthModeRequired]. An
// endpoint is opened only by naming [AuthModeNone], so forgetting to declare
// auth protects a route rather than exposing it. Everything here is arranged
// around that: see [AuthMode.required].
type AuthMode string

const (
	// AuthModeRequired demands an authenticated user. It is also what the zero
	// value means, so it rarely needs writing.
	AuthModeRequired AuthMode = "required"

	// AuthModeNone opts out: no user is resolved and none is required. Reach for
	// it deliberately — it is the visible, greppable token that says a route is
	// open.
	AuthModeNone AuthMode = "none"
)

// required reports whether the endpoint needs an authenticated user.
//
// It tests for the opt-out rather than for the opt-in, which is what makes the
// default safe: the zero value, and anything vov does not recognize, requires
// authentication. Comparing against AuthModeRequired instead would leave every
// endpoint that declared nothing wide open.
func (a AuthMode) required() bool {
	return a != AuthModeNone
}

// valid reports whether a is a mode vov knows. AuthMode is a string type, so a
// typo compiles; an unrecognized mode still fails closed above, but it is a
// mistake worth naming at construction rather than silently accepting.
func (a AuthMode) valid() bool {
	switch a {
	case "", AuthModeRequired, AuthModeNone:
		return true
	default:
		return false
	}
}

// authorized reports whether u satisfies an endpoint's declared requirements.
//
// The two combine differently, and the asymmetry follows what they are: a role
// is an identity, and holding any one of the listed identities is enough; a
// permission is a capability, and every listed one is needed.
//
//	roles       — any-of. "an admin or an owner may do this."
//	permissions — all-of, every one checked. "this needs both of these."
//
// Either list being empty means it places no requirement.
func authorized(u User, roles, permissions []string) bool {
	if len(roles) > 0 && !slices.ContainsFunc(roles, u.HasRole) {
		return false
	}
	for _, p := range permissions {
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
// The refusals are different facts and get different codes:
//
//	401  vov does not know who you are
//	403  it does, and the answer is no
//	402  it does, and the answer is yes as soon as you pay
//
// 403 never says which role or permission was missing — that would tell an
// attacker what to look for. 402 is deliberately distinguishable, because it is
// not really a denial: it is a price, the client is expected to act on it, and
// what it discloses ("this resource is paid") is on the pricing page anyway.
//
// The order is load-bearing. Roles and permissions are checked before tier, so
// 402 is returned only when payment is the last remaining barrier. Answering 402
// to someone who also lacks the required role would send them to a checkout page
// for access they still would not have.
func authGuard(next http.Handler, auth Authenticator, e Endpoint) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The authenticator gets the headers, not the writer: it may stamp a
		// Set-Cookie on its way past, and cannot write a response of its own.
		u, err := auth(AuthResponse{header: w.Header()}, r)
		if err != nil {
			// The lookup broke. Do not report this as a credential problem.
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		if u == nil || !u.IsAuthenticated() {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		if !authorized(u, e.RolesAnyOf, e.PermissionsAllOf) {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		if e.MinTier > 0 && u.Tier() < e.MinTier {
			http.Error(w, http.StatusText(http.StatusPaymentRequired), http.StatusPaymentRequired)
			return
		}
		next.ServeHTTP(w, r.WithContext(ContextWithUser(r.Context(), u)))
	})
}
