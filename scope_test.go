package vov

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// scopedApp builds an app whose one protected route requires the given rule, and
// whose authenticator grants the given scopes.
func scopedApp(t *testing.T, rule *ScopeRule, policy *ScopePolicy, granted []string) *App {
	t.Helper()
	app, err := NewApp(AppConfig{
		Scopes: policy,
		Authenticator: func(resp AuthResponse, r *http.Request) (User, error) {
			if granted != nil {
				resp.SetScopes(granted)
			}
			return scopeUser{}, nil
		},
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

func (scopeUser) IsAuthenticated() bool     { return true }
func (scopeUser) HasRole(string) bool       { return false }
func (scopeUser) HasPermission(string) bool { return false }
func (scopeUser) Tier() int                 { return 0 }

func getStatus(t *testing.T, app *App) int {
	t.Helper()
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))
	return rec.Code
}

// TestScopeAllOfRequiresEveryScope pins the all-of semantics: a partial grant is
// not a partial pass.
func TestScopeAllOfRequiresEveryScope(t *testing.T) {
	rule := &ScopeRule{AllOf: []string{"read", "write"}}

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
	app := scopedApp(t, &ScopeRule{AllOf: []string{"read"}}, nil, nil)
	if got := getStatus(t, app); got != http.StatusForbidden {
		t.Errorf("credential with no scopes: got %d, want 403", got)
	}
}

// TestScopeModesLimitEnforcement is the reason ScopeRule is a struct rather than
// a string slice. The same declaration governs one channel and exempts the other,
// so an app whose tokens exist only for its tools does not refuse every browser.
func TestScopeModesLimitEnforcement(t *testing.T) {
	rule := &ScopeRule{AllOf: []string{"write"}, Modes: []RequestMode{RequestModeMCP}}
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
// first — and the check is the cheap one, before any HasRole that may do I/O.
func TestScopesAreCheckedBeforeRoles(t *testing.T) {
	askedRole := false
	app, err := NewApp(AppConfig{
		Authenticator: func(resp AuthResponse, r *http.Request) (User, error) {
			return roleProbeUser{asked: &askedRole}, nil
		},
		Routes: []Route{{
			Path: "/probe",
			Endpoints: Endpoints{GET: Endpoint{
				ScopeAllOf: &ScopeRule{AllOf: []string{"write"}},
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
		t.Error("the role was checked after the scope had already refused the request")
	}
}

type roleProbeUser struct{ asked *bool }

func (roleProbeUser) IsAuthenticated() bool     { return true }
func (u roleProbeUser) HasRole(string) bool     { *u.asked = true; return true }
func (roleProbeUser) HasPermission(string) bool { return false }
func (roleProbeUser) Tier() int                 { return 0 }

// TestPolicyDefaultGovernsAnUndeclaredEndpoint is the property the whole
// ByMethod mechanism exists for: an endpoint that says nothing is still governed,
// so a new mutating route cannot ship unscoped because someone forgot a line.
func TestPolicyDefaultGovernsAnUndeclaredEndpoint(t *testing.T) {
	policy := &ScopePolicy{ByMethod: map[string][]string{
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

// TestPolicyRejectsAnUngovernedEndpoint is the construction error that makes the
// policy worth setting. The omission it catches is invisible in review, because
// what is wrong is the absence of a line.
func TestPolicyRejectsAnUngovernedEndpoint(t *testing.T) {
	_, err := NewApp(AppConfig{
		Scopes:        &ScopePolicy{ByMethod: map[string][]string{http.MethodGet: {"read"}}},
		Authenticator: func(AuthResponse, *http.Request) (User, error) { return scopeUser{}, nil },
		Routes: []Route{{
			Path: "/probe",
			// POST is covered by nothing: not by the endpoint, not by ByMethod.
			Endpoints: Endpoints{POST: Endpoint{Handler: func(http.ResponseWriter, *http.Request) {}}},
		}},
	})
	if err == nil {
		t.Fatal("NewApp accepted an endpoint the policy governs but nothing scopes")
	}
}

// TestScopeNoneOptsOut: the opt-out has to be greppable, like AuthModeNone. An
// endpoint that means "no scope needed" says so instead of being silent.
func TestScopeNoneOptsOut(t *testing.T) {
	app, err := NewApp(AppConfig{
		Scopes:        &ScopePolicy{ByMethod: map[string][]string{http.MethodGet: {"read"}}},
		Authenticator: func(AuthResponse, *http.Request) (User, error) { return scopeUser{}, nil },
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
				ScopeAllOf: &ScopeRule{AllOf: []string{"read"}},
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
		Authenticator: func(resp AuthResponse, r *http.Request) (User, error) {
			resp.SetScopes([]string{"read", "write"})
			return scopeUser{}, nil
		},
		Routes: []Route{{
			Path: "/probe",
			Endpoints: Endpoints{GET: Endpoint{
				ScopeAllOf: &ScopeRule{AllOf: []string{"read"}},
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
