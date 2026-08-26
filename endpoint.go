package vov

import "net/http"

// Endpoint is one method of one URL: the handler, plus the facts that describe
// how it is served — all as plain data. It lives inside a [Route], which
// supplies the path and the method, so an Endpoint says only what is specific to
// that method.
//
// It is deliberately a value type with no exported methods and no hidden state,
// so it can be read, listed, and (later) checked by tools without constructing
// an [App]. Its zero value declares nothing: a method field left empty on a
// Route is a method that URL does not answer.
type Endpoint struct {
	// Handler serves the request. For now it is a plain http.HandlerFunc; a
	// later phase introduces an error-returning handler type.
	Handler http.HandlerFunc

	// MiddlewareStack names the [MiddlewareStack] from
	// [AppConfig.MiddlewareStacks] that wraps this endpoint. Empty selects
	// [DefaultStackName], so the common case says nothing. Naming a stack
	// replaces the default outright rather than adding to it; a name that was
	// never declared is a construction error.
	MiddlewareStack string

	// AuthMod declares this endpoint's authentication requirement. The zero
	// value requires an authenticated user, so a method that says nothing is
	// protected; [NoAuth] opts out. It is per-method on purpose: a URL may be
	// readable by anyone and writable only by an authenticated user.
	AuthMod AuthMod

	// Roles restricts the endpoint to users holding at least one of them —
	// any-of, because a role is an identity and any of the listed identities
	// will do: "an admin or an owner may do this". Empty means no restriction.
	RolesAnyOf []string

	// Permissions restricts the endpoint to users holding every one of them —
	// all-of, because a permission is a capability and each listed one is
	// needed. Every entry is checked. Empty means no restriction.
	//
	// Roles and Permissions may be declared together; a user must then satisfy
	// both. Neither is checked on an endpoint declaring [NoAuth], which resolves
	// no user, so that combination is a construction error rather than a
	// requirement that silently does nothing.
	PermissionsAllOf []string
}

// declared reports whether the endpoint was filled in at all. Configuration
// without a handler counts as declared, so that [NewApp] can reject it rather
// than silently dropping a method someone meant to serve.
func (e Endpoint) declared() bool {
	return e.Handler != nil ||
		e.MiddlewareStack != "" ||
		e.AuthMod != (AuthMod{}) ||
		len(e.RolesAnyOf) > 0 ||
		len(e.PermissionsAllOf) > 0
}

// constrained reports whether the endpoint demands anything of the user beyond
// being authenticated.
func (e Endpoint) constrained() bool {
	return len(e.RolesAnyOf) > 0 || len(e.PermissionsAllOf) > 0
}

// wrapped builds the endpoint's request chain from its stack, outside in:
//
//	Pre → [auth guard] → Post → Handler
//
// The guard is the seam between the stack's two halves rather than a member of
// either, so choosing a different stack can never switch authentication off —
// only [AuthMod] does that. See [authGuard]. An endpoint declaring [NoAuth] has
// no guard and no user, so its stack's Post half is skipped.
func (e Endpoint) wrapped(s MiddlewareStack, auth Authenticator) http.Handler {
	var h http.Handler = e.Handler
	if e.AuthMod.required() {
		h = apply(h, s.Post)
		h = authGuard(h, auth, e.RolesAnyOf, e.PermissionsAllOf)
	}
	return apply(h, s.Pre)
}
