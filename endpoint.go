package vov

import "net/http"

// Endpoint is one method of one URL: the handler, plus the facts that describe
// how it is served — all as plain data. It lives inside an [Endpoints] group,
// which supplies the path and the method, so an Endpoint says only what is
// specific to that method.
//
// It is deliberately a value type with no exported methods and no hidden state,
// so it can be read, listed, and (later) checked by tools without constructing
// an [App]. Its zero value declares nothing: a method field left empty on a
// group is a method that URL does not answer.
type Endpoint struct {
	// Handler serves the request. It is the standard net/http signature; the
	// objects an app builds at boot are reached through a [Global] holder rather
	// than being threaded through here.
	Handler http.HandlerFunc

	// MiddlewareStack names the [MiddlewareStack] from
	// [AppConfig.MiddlewareStacks] that wraps this endpoint. Empty selects
	// [DefaultStackName], so the common case says nothing. Naming a stack
	// replaces the default outright rather than adding to it; a name that was
	// never declared is a construction error.
	MiddlewareStack string

	// AuthMode declares this endpoint's authentication requirement. The zero
	// value requires an authenticated user, so a method that says nothing is
	// protected; [AuthModeNone] opts out. It is per-method on purpose: a URL may
	// be readable by anyone and writable only by an authenticated user.
	AuthMode AuthMode

	// RolesAnyOf restricts the endpoint to users holding at least one of them —
	// any-of, because a role is an identity and any of the listed identities
	// will do: "an admin or an owner may do this". Empty means no restriction.
	RolesAnyOf []string

	// PermissionsAllOf restricts the endpoint to users holding every one of
	// them — all-of, because a permission is a capability and each listed one is
	// needed. Every entry is checked. Empty means no restriction.
	//
	// The two may be declared together; a user must then satisfy both. Neither
	// is checked on an endpoint declaring [AuthModeNone], which resolves no
	// user, so that combination is a construction error rather than a
	// requirement that silently does nothing.
	PermissionsAllOf []string

	// ScopeAllOf restricts the endpoint to credentials carrying all of the listed
	// scopes per channel. It is a different axis from permissions as permissions
	// are applied to user, regardless of the auth method/token while a single
	// user can have multiple auth tokens, each with a different scope
	//
	//	ScopeAllOf: vov.ScopeAllOf{vov.RequestModeMCP: {"tasks:write"}}
	//
	// Providing a key for this field will override the scopes of that mode, described
	// in [AppConfig.Scopes]. For more detail, see [ScopeAllOf]
	ScopeAllOf ScopeAllOf

	// PathParams documents this route's wildcards, keyed by the wildcard name —
	// see [PathParam]. It is how a path parameter gets a caller-facing name and a
	// description, which a body or query field carries on its Go struct field and
	// a wildcard has nowhere else to put.
	//
	// Documenting a name the route does not declare is a construction error, so a
	// typo fails the build rather than quietly documenting nothing.
	PathParams map[string]PathParam

	// Body describes the shape of the request body, built with [BodyOf] from the
	// type the handler decodes into. Query does the same for the query string,
	// with [QueryOf].
	//
	// They describe; vov does not decode or enforce them. What they are for is
	// the consumers that cannot work without a machine-readable contract: an
	// OpenAPI document, an MCP tool's input schema, and a test runner that has to
	// build a valid body to attempt a request it should not be allowed to make.
	// Declaring the shape once, on the endpoint, is what keeps those three from
	// becoming three copies that drift.
	//
	// A type vov cannot describe is a construction error, so a declaration never
	// silently renders as something it is not.
	Body  *Schema
	Query *Schema

	// MCPTool, when set, declares this endpoint callable by an AI assistant over
	// the Model Context Protocol, under the given name — see [MCPTool]. It adds
	// only that name and the prose a model reads; the method, path, arguments
	// and policy are the ones already declared here, so a tool is never a second
	// description of the endpoint.
	MCPTool *MCPTool

	// MinTier restricts the endpoint to users whose [User.Tier] is at least this
	// high. Zero — the default — places no restriction, so only endpoints behind
	// a paywall mention it.
	//
	// A user who is refused here gets **402 Payment Required**, not 403, because
	// the two say different things to a client: 403 is a denial, 402 is a price.
	// A frontend can act on that — show the upgrade panel rather than a generic
	// error — which is why tier is a declaration of its own rather than a
	// permission.
	//
	// Reach for it only for access that money actually unlocks. Ordinal gating
	// that payment cannot resolve belongs in [Endpoint.PermissionsAllOf], whose
	// refusal is honest about being final.
	MinTier int
}

// declared reports whether the endpoint was filled in at all. Configuration
// without a handler counts as declared, so that [NewApp] can reject it rather
// than silently dropping a method someone meant to serve.
func (e Endpoint) declared() bool {
	return e.Handler != nil ||
		e.MiddlewareStack != "" ||
		e.AuthMode != "" ||
		len(e.RolesAnyOf) > 0 ||
		len(e.PermissionsAllOf) > 0 ||
		e.MinTier != 0 ||
		len(e.ScopeAllOf) > 0 ||
		len(e.PathParams) > 0 ||
		e.Body != nil ||
		e.Query != nil ||
		e.MCPTool != nil
}

// constrained reports whether the endpoint demands anything of the user beyond
// being authenticated.
func (e Endpoint) constrained() bool {
	return len(e.RolesAnyOf) > 0 || len(e.PermissionsAllOf) > 0 || e.MinTier != 0
}

// wrapped builds the endpoint's request chain from its stack, outside in:
//
//	Pre → [auth guard] → Post → Handler
//
// The guard is the seam between the stack's two halves rather than a member of
// either, so choosing a different stack can never switch authentication off —
// only [AuthMode] does that. See [authGuard]. An endpoint declaring
// [AuthModeNone] has no guard and no user, so its stack's Post half is skipped.
func (e Endpoint) wrapped(s MiddlewareStack, auth Authenticator, sc *scopeCheck) http.Handler {
	var h http.Handler = e.Handler
	if e.AuthMode.required() {
		h = apply(h, s.Post)
		h = authGuard(h, auth, e, sc)
	}
	return apply(h, s.Pre)
}
