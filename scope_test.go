package vov

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// scopedApp builds an app whose one protected route requires the given rule, and
// whose authenticator grants the given scopes.
func scopedApp(t *testing.T, rule ScopeAllOf, policy *ScopePolicy, granted []string) *App {
	t.Helper()
	app, err := NewApp(AppConfig{
		Scopes: policy,
		API: APIConfig{Authenticator: func(resp AuthResponse, r *http.Request) (User, error) {
			if granted != nil {
				resp.SetScopes(granted)
			}
			return scopeUser{}, nil
		}},
		Routes: []Route{{
			Path: "/probe",
			Endpoints: Endpoints{
				GET:  Endpoint{ScopeAllOf: rule, Handler: func(http.ResponseWriter, *http.Request) {}},
				POST: Endpoint{ScopeAllOf: rule, Handler: func(http.ResponseWriter, *http.Request) {}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	return app
}

// scopeUser is authenticated and holds nothing else, so only the scope check can
// refuse it.
type scopeUser struct{}

func (scopeUser) IsAuthenticated() bool { return true }
func (scopeUser) Roles() []string       { return nil }
func (scopeUser) Permissions() []string { return nil }
func (scopeUser) Tier() int             { return 0 }

func getStatus(t *testing.T, app *App) int {
	t.Helper()
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))
	return rec.Code
}

// TestScopeAllOfRequiresEveryScope pins the all-of semantics: a partial grant is
// not a partial pass.
func TestScopeAllOfRequiresEveryScope(t *testing.T) {
	rule := ScopeAllOf{RequestModeAPI: {"read", "write"}}

	if got := getStatus(t, scopedApp(t, rule, nil, []string{"read", "write"})); got != http.StatusOK {
		t.Errorf("full grant: got %d, want 200", got)
	}
	if got := getStatus(t, scopedApp(t, rule, nil, []string{"read"})); got != http.StatusForbidden {
		t.Errorf("partial grant: got %d, want 403", got)
	}
}

// TestMissingScopesRefuse is the fail-closed case. An authenticator that never
// calls SetScopes leaves the credential with nothing, and an endpoint that
// declared a requirement has already said a bare credential is not enough.
func TestMissingScopesRefuse(t *testing.T) {
	app := scopedApp(t, ScopeAllOf{RequestModeAPI: {"read"}}, nil, nil)
	if got := getStatus(t, app); got != http.StatusForbidden {
		t.Errorf("credential with no scopes: got %d, want 403", got)
	}
}

// TestScopeModesLimitEnforcement is the reason the declaration is keyed by
// channel. One channel is governed and the other exempt, so an app whose tokens
// exist only for its tools does not refuse every browser.
func TestScopeModesLimitEnforcement(t *testing.T) {
	rule := ScopeAllOf{RequestModeMCP: {"write"}}
	app := scopedApp(t, rule, nil, nil) // no scopes granted at all

	// Over the API the rule does not apply, so a scopeless credential passes.
	if got := getStatus(t, app); got != http.StatusOK {
		t.Errorf("API request under an MCP-only rule: got %d, want 200", got)
	}

	// The very same endpoint, reached on the governed channel, refuses.
	// A vouched caller with an identity but no grant: 401 is settled before any
	// of this, so the refusal under test has to be the scope one.
	res, err := app.invoke(context.Background(), invokeRequest{
		Path: "/probe", Mode: RequestModeMCP, User: scopeUser{},
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Status != http.StatusForbidden {
		t.Errorf("MCP request under an MCP-only rule: got %d, want 403", res.Status)
	}
}

// TestScopesAreCheckedBeforeRoles pins the ordering. A credential not issued for
// the operation cannot perform it whoever holds it, so the scope is decided
// first — and the check is the cheap one, before any Roles() that may do I/O.
func TestScopesAreCheckedBeforeRoles(t *testing.T) {
	askedRole := false
	app, err := NewApp(AppConfig{
		API: APIConfig{Authenticator: func(resp AuthResponse, r *http.Request) (User, error) {
			return roleProbeUser{asked: &askedRole}, nil
		}},
		Routes: []Route{{
			Path: "/probe",
			Endpoints: Endpoints{GET: Endpoint{
				ScopeAllOf: ScopeAllOf{RequestModeAPI: {"write"}},
				RolesAnyOf: []string{"admin"},
				Handler:    func(http.ResponseWriter, *http.Request) {},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if got := getStatus(t, app); got != http.StatusForbidden {
		t.Fatalf("got %d, want 403", got)
	}
	if askedRole {
		t.Error("roles were fetched after the scope had already refused the request")
	}
}

type roleProbeUser struct{ asked *bool }

func (roleProbeUser) IsAuthenticated() bool { return true }
func (u roleProbeUser) Roles() []string     { *u.asked = true; return []string{"admin"} }
func (roleProbeUser) Permissions() []string { return nil }
func (roleProbeUser) Tier() int             { return 0 }

// TestPolicyDefaultGovernsAnUndeclaredEndpoint is the property the whole
// ByMethod mechanism exists for: an endpoint that says nothing is still governed,
// so a new mutating route cannot ship unscoped because someone forgot a line.
func TestPolicyDefaultGovernsAnUndeclaredEndpoint(t *testing.T) {
	policy := &ScopePolicy{API: map[string][]string{
		http.MethodGet:  {"read"},
		http.MethodPost: {"write"},
	}}
	app := scopedApp(t, nil, policy, []string{"read"}) // read-only credential

	if got := getStatus(t, app); got != http.StatusOK {
		t.Errorf("GET with a read grant: got %d, want 200", got)
	}

	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/probe", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST with a read-only grant: got %d, want 403", rec.Code)
	}
}

// TestUnlistedMethodIsUngoverned: a method absent from a channel's map requires
// no scopes there, so gating one method costs one entry and not an enumeration
// of every other. It is what lets a policy govern its channels differently.
func TestUnlistedMethodIsUngoverned(t *testing.T) {
	policy := &ScopePolicy{API: map[string][]string{http.MethodPost: {"write"}}}
	app := scopedApp(t, nil, policy, nil) // a credential carrying nothing

	if got := getStatus(t, app); got != http.StatusOK {
		t.Errorf("GET, unlisted: got %d, want 200", got)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/probe", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST, listed: got %d, want 403", rec.Code)
	}
}

// TestScopeNoneOptsOut: the opt-out has to be greppable, like AuthModeNone. An
// endpoint that means "no scope needed" says so instead of being silent.
func TestScopeNoneOptsOut(t *testing.T) {
	app, err := NewApp(AppConfig{
		Scopes: &ScopePolicy{API: map[string][]string{http.MethodGet: {"read"}}},
		API:    APIConfig{Authenticator: func(AuthResponse, *http.Request) (User, error) { return scopeUser{}, nil }},
		Routes: []Route{{
			Path:      "/probe",
			Endpoints: Endpoints{GET: Endpoint{ScopeAllOf: ScopeNone(), Handler: func(http.ResponseWriter, *http.Request) {}}},
		}},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if got := getStatus(t, app); got != http.StatusOK {
		t.Errorf("ScopeNone with no scopes granted: got %d, want 200", got)
	}
}

// TestScopeAllOfWithAuthModeNoneIsRejected: an open endpoint resolves no
// credential, so the requirement would silently not be enforced. Same reasoning
// that already rejects roles on an open endpoint.
func TestScopeAllOfWithAuthModeNoneIsRejected(t *testing.T) {
	_, err := NewApp(AppConfig{
		Routes: []Route{{
			Path: "/probe",
			Endpoints: Endpoints{GET: Endpoint{
				AuthMode:   AuthModeNone,
				ScopeAllOf: ScopeAllOf{RequestModeAPI: {"read"}},
				Handler:    func(http.ResponseWriter, *http.Request) {},
			}},
		}},
	})
	if err == nil {
		t.Fatal("NewApp accepted ScopeAllOf on an endpoint declaring AuthModeNone")
	}
}

// TestScopesReachTheHandler: a handler behind a rule can read what the credential
// actually carried, without any exported way to have written it.
func TestScopesReachTheHandler(t *testing.T) {
	var seen []string
	app, err := NewApp(AppConfig{
		API: APIConfig{Authenticator: func(resp AuthResponse, r *http.Request) (User, error) {
			resp.SetScopes([]string{"read", "write"})
			return scopeUser{}, nil
		}},
		Routes: []Route{{
			Path: "/probe",
			Endpoints: Endpoints{GET: Endpoint{
				ScopeAllOf: ScopeAllOf{RequestModeAPI: {"read"}},
				Handler: func(w http.ResponseWriter, r *http.Request) {
					seen, _ = ScopesFrom(r.Context())
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if got := getStatus(t, app); got != http.StatusOK {
		t.Fatalf("got %d, want 200", got)
	}
	if len(seen) != 2 || seen[0] != "read" || seen[1] != "write" {
		t.Errorf("handler saw scopes %v, want [read write]", seen)
	}
}

// TestChannelsCanBeGovernedDifferently is what keying the policy by channel buys
// that a single method map plus a mode list could not express at all: an HTTP API
// that gates only deletion, while every tool call is scoped.
func TestChannelsCanBeGovernedDifferently(t *testing.T) {
	policy := &ScopePolicy{
		API: map[string][]string{http.MethodDelete: {"tasks:write"}},
		MCP: map[string][]string{
			http.MethodGet:    {"tasks:read"},
			http.MethodDelete: {"tasks:write"},
		},
	}
	var app *App
	app, err := NewApp(AppConfig{
		Scopes: policy,
		API: APIConfig{Authenticator: func(resp AuthResponse, r *http.Request) (User, error) {
			return scopeUser{}, nil // a credential carrying nothing
		}},
		Routes: []Route{{
			Path: "/probe",
			Endpoints: Endpoints{
				GET:    Endpoint{Handler: func(http.ResponseWriter, *http.Request) {}, MCPTool: &MCPTool{Name: "get_probe"}},
				DELETE: Endpoint{Handler: func(http.ResponseWriter, *http.Request) {}, MCPTool: &MCPTool{Name: "del_probe"}},
			},
		}},
		MCP: &MCPConfig{
			Name: "probe", Version: "0", Path: "",
			Authenticate: func(AuthResponse, *http.Request) (User, error) { return scopeUser{}, nil },
		},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	// GET is ungoverned on the API and governed on MCP — the same endpoint,
	// the same scopeless credential, two answers.
	if got := getStatus(t, app); got != http.StatusOK {
		t.Errorf("API GET (ungoverned): got %d, want 200", got)
	}
	res, err := app.invoke(context.Background(), invokeRequest{Path: "/probe", Mode: RequestModeMCP, User: scopeUser{}})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Status != http.StatusForbidden {
		t.Errorf("MCP GET (governed): got %d, want 403", res.Status)
	}

	// DELETE is governed on both, so the API refuses it too.
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/probe", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("API DELETE (governed): got %d, want 403", rec.Code)
	}
}

// TestUnnamedChannelFallsBackToThePolicy: naming one channel does not decide the
// other, which is what map-key presence buys over a modes list.
func TestUnnamedChannelFallsBackToThePolicy(t *testing.T) {
	policy := &ScopePolicy{API: map[string][]string{http.MethodGet: {"read"}}}
	app := scopedApp(t, ScopeAllOf{RequestModeMCP: {"write"}}, policy, nil)

	// The endpoint said nothing about the API channel, so the policy still governs it.
	if got := getStatus(t, app); got != http.StatusForbidden {
		t.Errorf("API: got %d, want 403 from the policy default", got)
	}
}

// TestEmptyListRequiresNothing: a named channel with no scopes overrides the
// policy rather than inheriting it.
func TestEmptyListRequiresNothing(t *testing.T) {
	policy := &ScopePolicy{API: map[string][]string{http.MethodGet: {"read"}}}
	app := scopedApp(t, ScopeAllOf{RequestModeAPI: {}}, policy, nil)

	if got := getStatus(t, app); got != http.StatusOK {
		t.Errorf("got %d, want 200: an empty list requires no scopes", got)
	}
}

// TestAccessorsAreAskedOnlyWhenDeclared: the point of the slice accessors is one
// lookup instead of N, and an endpoint declaring neither must still cost neither.
func TestAccessorsAreAskedOnlyWhenDeclared(t *testing.T) {
	var roleCalls, permCalls int
	mk := func(e Endpoint) *App {
		t.Helper()
		e.Handler = func(http.ResponseWriter, *http.Request) {}
		app, err := NewApp(AppConfig{
			API: APIConfig{Authenticator: func(AuthResponse, *http.Request) (User, error) {
				return countingUser{&roleCalls, &permCalls}, nil
			}},
			Routes: []Route{{Path: "/probe", Endpoints: Endpoints{GET: e}}},
		})
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		return app
	}

	getStatus(t, mk(Endpoint{}))
	if roleCalls != 0 || permCalls != 0 {
		t.Errorf("an endpoint declaring nothing asked for roles %d times and permissions %d", roleCalls, permCalls)
	}

	roleCalls, permCalls = 0, 0
	getStatus(t, mk(Endpoint{PermissionsAllOf: []string{"a", "b", "c"}}))
	if permCalls != 1 {
		t.Errorf("three declared permissions cost %d lookups, want 1", permCalls)
	}
	if roleCalls != 0 {
		t.Errorf("roles were fetched for an endpoint declaring none")
	}
}

type countingUser struct{ roles, perms *int }

func (countingUser) IsAuthenticated() bool   { return true }
func (u countingUser) Roles() []string       { *u.roles++; return []string{"admin"} }
func (u countingUser) Permissions() []string { *u.perms++; return []string{"a", "b", "c"} }
func (countingUser) Tier() int               { return 0 }

func TestResolveScope(t *testing.T) {
	// local overrides app
	assert.Equal(t,
		resolveScope(
			Endpoint{ScopeAllOf: ScopeAllOf{RequestModeAPI: []string{"one"}}},
			http.MethodGet,
			&ScopePolicy{
				API: map[string][]string{http.MethodGet: {"two"}},
				MCP: map[string][]string{http.MethodGet: {"three"}},
			},
		),
		scopeCheck{RequestModeAPI: []string{"one"}, RequestModeMCP: []string{"three"}},
	)

	// local and app cover two different methods
	assert.Equal(t,
		resolveScope(
			Endpoint{ScopeAllOf: ScopeAllOf{RequestModeAPI: []string{"one"}}},
			http.MethodPut,
			&ScopePolicy{
				API: map[string][]string{http.MethodGet: {"two"}},
				MCP: map[string][]string{http.MethodGet: {"three"}},
			},
		),
		scopeCheck{RequestModeAPI: []string{"one"}},
	)

	// local overrides nothing
	assert.Equal(t,
		resolveScope(
			Endpoint{},
			http.MethodGet,
			&ScopePolicy{
				API: map[string][]string{http.MethodGet: {"two"}},
				MCP: map[string][]string{http.MethodGet: {"three"}},
			},
		),
		scopeCheck{RequestModeAPI: []string{"two"}, RequestModeMCP: []string{"three"}},
	)

	// an empty list overrides the app rather than inheriting it
	assert.Equal(t,
		resolveScope(
			Endpoint{ScopeAllOf: ScopeAllOf{RequestModeAPI: []string{}}},
			http.MethodGet,
			&ScopePolicy{
				API: map[string][]string{http.MethodGet: {"two"}},
				MCP: map[string][]string{http.MethodGet: {"three"}},
			},
		),
		scopeCheck{RequestModeMCP: []string{"three"}},
	)

	// endpoint does not require auth
	assert.Nil(t,
		resolveScope(
			Endpoint{AuthMode: AuthModeNone},
			http.MethodGet,
			&ScopePolicy{
				API: map[string][]string{http.MethodGet: {"two"}},
				MCP: map[string][]string{http.MethodGet: {"three"}},
			},
		),
	)
}
