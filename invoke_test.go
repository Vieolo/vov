package vov

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// modeProbe builds a one-route app whose handler records the mode it was reached
// with. The route opts out of auth so that the test says nothing about the guard.
func modeProbe(t *testing.T, seen *RequestMode) *App {
	t.Helper()
	app, err := NewApp(AppConfig{
		Routes: []Route{{
			Path: "/probe",
			Endpoints: Endpoints{
				GET: Endpoint{
					AuthMode: AuthModeNone,
					Handler: func(w http.ResponseWriter, r *http.Request) {
						*seen = ModeFrom(r.Context())
					},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	return app
}

// TestInvokeRequiresMode pins the decision that Invoke has no default channel.
//
// Whichever default were chosen, an omitted Mode would name a channel nobody
// decided on — and the same empty value already means something else to
// [ModeFrom], where absence is what an ordinary network request looks like. One
// zero value cannot honestly mean both.
func TestInvokeRequiresMode(t *testing.T) {
	var seen RequestMode
	app := modeProbe(t, &seen)

	if _, err := app.Invoke(context.Background(), InvokeRequest{Path: "/probe"}); err == nil {
		t.Fatal("Invoke accepted an empty Mode, which is required")
	}
	if seen != "" {
		t.Fatalf("the handler ran for a rejected request, seeing mode %q", seen)
	}
}

// TestInvokeRejectsUnknownMode covers the typo. RequestMode is a string type, so
// an unknown one compiles; dispatching it would put a request on a channel
// nothing downstream recognizes, and anything keyed on the mode would quietly
// take its default branch.
func TestInvokeRejectsUnknownMode(t *testing.T) {
	var seen RequestMode
	app := modeProbe(t, &seen)

	_, err := app.Invoke(context.Background(), InvokeRequest{Path: "/probe", Mode: "network"})
	if err == nil {
		t.Fatal("Invoke accepted an unknown Mode")
	}
	if seen != "" {
		t.Fatalf("the handler ran for a rejected request, seeing mode %q", seen)
	}
}

// TestInvokeCarriesTheNamedMode is the whole point of the field: the dispatch is
// byte-for-byte the same either way, and the channel is whichever one the caller
// named. A runner standing in for the HTTP API gets RequestModeAPI through the
// same call that a tool server uses for RequestModeMCP.
func TestInvokeCarriesTheNamedMode(t *testing.T) {
	for _, want := range []RequestMode{RequestModeAPI, RequestModeMCP} {
		t.Run(string(want), func(t *testing.T) {
			var seen RequestMode
			app := modeProbe(t, &seen)

			res, err := app.Invoke(context.Background(), InvokeRequest{Path: "/probe", Mode: want})
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if res.Status != http.StatusOK {
				t.Fatalf("status %d, want %d", res.Status, http.StatusOK)
			}
			if seen != want {
				t.Errorf("handler saw mode %q, want %q", seen, want)
			}
		})
	}
}

// TestNetworkRequestIsAPIMode covers the other half of the contract. Nothing tags
// a request that arrived over HTTP, so ModeFrom answers by absence — and it must
// answer with the API channel rather than the empty mode actually stored, or
// every audit row and mode-keyed branch would see a channel that does not exist.
func TestNetworkRequestIsAPIMode(t *testing.T) {
	var seen RequestMode
	app := modeProbe(t, &seen)

	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusOK)
	}
	if seen != RequestModeAPI {
		t.Errorf("an untagged request reported mode %q, want %q", seen, RequestModeAPI)
	}
}
