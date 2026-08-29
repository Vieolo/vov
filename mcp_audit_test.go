package vov

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	mcpsrv "github.com/vieolo/vov/internal/mcp"
)

// auditApp builds an app with one tool, recording every call its sink is handed.
func auditApp(t *testing.T, auth Authenticator, seen *[]ToolCall, sink func(context.Context, ToolCall)) *App {
	t.Helper()
	if sink == nil {
		sink = func(_ context.Context, c ToolCall) { *seen = append(*seen, c) }
	}
	app, err := NewApp(AppConfig{
		API: APIConfig{Authenticator: auth},
		MCP: &MCPConfig{
			Name: "probe", Version: "0",
			Authenticate: auth,
			OnToolCall:   sink,
		},
		Routes: []Route{{
			Path: "/thing/{id}",
			Endpoints: Endpoints{GET: Endpoint{
				MCPTool: &MCPTool{Name: "get_thing", Description: "d"},
				Handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) },
			}},
		}},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	return app
}

// callTool drives the dispatch closure directly, which is the seam the protocol
// package calls. Going through the SDK would test the SDK.
func callTool(t *testing.T, app *App, raw string) {
	t.Helper()
	tools, err := app.mcpTools(app.mcp)
	if err != nil {
		t.Fatalf("building tools: %v", err)
	}
	_, _ = tools[0].Call(context.Background(), mcpCall(tools[0].Name, raw))
}

func okAuth(AuthResponse, *http.Request) (User, error) { return scopeUser{}, nil }

// mcpCall builds the call the protocol package would hand to the dispatch.
func mcpCall(tool, raw string) mcpsrv.Call {
	return mcpsrv.Call{Tool: tool, RawArgs: json.RawMessage(raw), Header: http.Header{}, RespHeader: http.Header{}}
}

// TestRejectedArgumentsAreStillObserved is the whole reason this hook exists. The
// call never reaches an endpoint, so no middleware runs for it — an assistant
// looping on an argument it keeps getting wrong is visible here and nowhere else.
func TestRejectedArgumentsAreStillObserved(t *testing.T) {
	var seen []ToolCall
	app := auditApp(t, okAuth, &seen, nil)

	callTool(t, app, `{}`) // the tool's path parameter is missing

	if len(seen) != 1 {
		t.Fatalf("got %d records, want 1", len(seen))
	}
	if seen[0].Outcome != ToolOutcomeRejected {
		t.Errorf("outcome %q, want %q", seen[0].Outcome, ToolOutcomeRejected)
	}
	if seen[0].Status != 0 {
		t.Errorf("status %d, want 0: nothing was dispatched", seen[0].Status)
	}
	// The bug this pins: rendering the rejection must not read an error that was
	// cleared on the way out.
	if seen[0].Err != nil {
		t.Errorf("a rejection is an answer, not a failure: Err = %v", seen[0].Err)
	}
}

// TestAuthenticationPrecedesArgumentMapping: a call rejected for a bad argument
// must still be attributable to whoever made it, which is exactly the record an
// operator wants when an assistant is looping.
func TestAuthenticationPrecedesArgumentMapping(t *testing.T) {
	var seen []ToolCall
	auth := func(resp AuthResponse, r *http.Request) (User, error) {
		resp.SetScopes([]string{"read"})
		return scopeUser{}, nil
	}
	app := auditApp(t, auth, &seen, nil)

	callTool(t, app, `{}`) // rejected for a missing argument

	if len(seen) != 1 {
		t.Fatalf("got %d records, want 1", len(seen))
	}
	if seen[0].User == nil {
		t.Error("a rejected call was recorded with no user: authentication ran too late")
	}
	if len(seen[0].Scopes) != 1 || seen[0].Scopes[0] != "read" {
		t.Errorf("scopes %v, want [read]", seen[0].Scopes)
	}
}

// TestNonSuccessIsObservedWithItsStatus: an endpoint that refused is a different
// event from one that was never reached, and only the first has a status.
func TestNonSuccessIsObservedWithItsStatus(t *testing.T) {
	var seen []ToolCall
	app := auditApp(t, okAuth, &seen, nil)

	callTool(t, app, `{"id":"7"}`) // the handler answers 418

	if len(seen) != 1 {
		t.Fatalf("got %d records, want 1", len(seen))
	}
	if seen[0].Outcome != ToolOutcomeRefused || seen[0].Status != http.StatusTeapot {
		t.Errorf("outcome %q status %d, want %q %d",
			seen[0].Outcome, seen[0].Status, ToolOutcomeRefused, http.StatusTeapot)
	}
	if seen[0].Duration <= 0 {
		t.Error("no duration was recorded")
	}
}

// TestUnresolvableCallerIsObservedAsFailed: a broken credential store is not a
// refusal, and the record says so rather than inventing a status.
func TestUnresolvableCallerIsObservedAsFailed(t *testing.T) {
	var seen []ToolCall
	broken := func(AuthResponse, *http.Request) (User, error) {
		return nil, errors.New("token store unavailable")
	}
	app := auditApp(t, okAuth, &seen, nil)
	app.mcp.Authenticate = broken

	callTool(t, app, `{"id":"7"}`)

	if len(seen) != 1 {
		t.Fatalf("got %d records, want 1", len(seen))
	}
	if seen[0].Outcome != ToolOutcomeFailed || seen[0].Err == nil {
		t.Errorf("outcome %q err %v, want %q and an error", seen[0].Outcome, seen[0].Err, ToolOutcomeFailed)
	}
	if seen[0].User != nil {
		t.Error("a user was recorded for a call whose caller could not be resolved")
	}
}

// TestObserverCannotFailTheCall: by the time the sink runs the endpoint has
// committed, so a sink that panics must not turn a written row into a reported
// failure. Nor may a missing sink change anything.
func TestObserverCannotFailTheCall(t *testing.T) {
	app := auditApp(t, okAuth, nil, func(context.Context, ToolCall) { panic("the audit sink is down") })

	tools, err := app.mcpTools(app.mcp)
	if err != nil {
		t.Fatalf("building tools: %v", err)
	}
	res, err := tools[0].Call(context.Background(), mcpCall(tools[0].Name, `{"id":"7"}`))
	if err != nil {
		t.Fatalf("a panicking sink failed the call: %v", err)
	}
	if res.Status != http.StatusTeapot {
		t.Errorf("status %d, want %d: the sink altered the result", res.Status, http.StatusTeapot)
	}
}

// TestPanickingToolIsContained pins a hazard ServerWrappers cannot reach.
//
// The protocol SDK dispatches each tool call on a goroutine of its own, so a
// wrapper's deferred recover — which sits on the goroutine serving the HTTP
// request — never sees a panicking handler, and neither does net/http's. Before
// vov recovered here, one panicking tool ended the process.
func TestPanickingToolIsContained(t *testing.T) {
	var seen []ToolCall
	app, err := NewApp(AppConfig{
		API: APIConfig{Authenticator: okAuth},
		MCP: &MCPConfig{
			Name: "probe", Version: "0", Authenticate: okAuth,
			OnToolCall: func(_ context.Context, c ToolCall) { seen = append(seen, c) },
		},
		Routes: []Route{{
			Path: "/boom",
			Endpoints: Endpoints{GET: Endpoint{
				MCPTool: &MCPTool{Name: "boom", Description: "d"},
				Handler: func(http.ResponseWriter, *http.Request) { panic("handler exploded") },
			}},
		}},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	tools, err := app.mcpTools(app.mcp)
	if err != nil {
		t.Fatalf("building tools: %v", err)
	}

	_, err = tools[0].Call(context.Background(), mcpCall("boom", `{}`))
	if err == nil {
		t.Fatal("a panicking tool reported success")
	}
	// The client learns that it failed and nothing else: a panic value is
	// written for an operator, not for an assistant.
	if strings.Contains(err.Error(), "exploded") {
		t.Errorf("the panic value reached the client: %v", err)
	}
	if len(seen) != 1 || seen[0].Outcome != ToolOutcomeFailed {
		t.Fatalf("got %d records, outcome %v; want 1 failed", len(seen), seen)
	}
	// ...while the operator's record keeps it.
	if seen[0].Err == nil || !strings.Contains(seen[0].Err.Error(), "exploded") {
		t.Errorf("the observed record lost the panic value: %v", seen[0].Err)
	}
}

// TestObservedArgumentsSurviveRouting is a regression test for a real defect.
//
// splitArgs consumes the argument map as it routes, deleting each value it
// places in the path or the query. The record used to alias that map, so by the
// time the deferred observation ran it held only what vov had *failed* to
// route — which for a read tool whose inputs are all path parameters is an empty
// map. An audit trail exists to answer "what has this assistant been doing", and
// `get_project {}` does not answer it.
func TestObservedArgumentsSurviveRouting(t *testing.T) {
	var seen []ToolCall
	app := auditApp(t, okAuth, &seen, nil)

	callTool(t, app, `{"id":"42"}`) // "id" is the tool's path parameter

	if len(seen) != 1 {
		t.Fatalf("got %d records, want 1", len(seen))
	}
	got, ok := seen[0].Arguments["id"]
	if !ok {
		t.Fatalf("the routed path parameter was consumed from the record: %v", seen[0].Arguments)
	}
	if string(got) != `"42"` {
		t.Errorf("recorded id as %s, want \"42\"", got)
	}
	// The fixture's handler answers 418, so the outcome is a refusal — what
	// matters here is that the call was dispatched at all, and so had its
	// arguments routed and therefore consumed.
	if seen[0].Status != http.StatusTeapot {
		t.Errorf("status %d, want %d — the call did not reach the endpoint", seen[0].Status, http.StatusTeapot)
	}
}

// TestObservedArgumentsSurviveAQueryRoute covers the other consumer of the map.
// A query field is deleted by the same loop, and a tool whose inputs are all
// query parameters is the other shape that recorded nothing.
func TestObservedArgumentsSurviveAQueryRoute(t *testing.T) {
	var seen []ToolCall
	app, err := NewApp(AppConfig{
		API: APIConfig{Authenticator: okAuth},
		MCP: &MCPConfig{
			Name: "probe", Version: "0", Authenticate: okAuth,
			OnToolCall: func(_ context.Context, c ToolCall) { seen = append(seen, c) },
		},
		Routes: []Route{{
			Path: "/things",
			Endpoints: Endpoints{GET: Endpoint{
				Query: QueryOf[struct {
					Q string `json:"q"`
				}](),
				MCPTool: &MCPTool{Name: "find_things", Description: "d"},
				Handler: func(http.ResponseWriter, *http.Request) {},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	tools, err := app.mcpTools(app.mcp)
	if err != nil {
		t.Fatalf("building tools: %v", err)
	}
	if _, err := tools[0].Call(context.Background(), mcpCall("find_things", `{"q":"acme"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(seen) != 1 || string(seen[0].Arguments["q"]) != `"acme"` {
		t.Fatalf("the routed query field was consumed from the record: %v", seen[0].Arguments)
	}
}

// TestUnknownArgumentIsRefused: an argument matching nothing the tool declares
// used to be marshalled into the request body regardless, so a model that
// invented a parameter name had it forwarded and learned nothing. The schema
// already names every argument, so anything else is a mistake worth stating.
func TestUnknownArgumentIsRefused(t *testing.T) {
	var seen []ToolCall
	app := auditApp(t, okAuth, &seen, nil)

	tools, err := app.mcpTools(app.mcp)
	if err != nil {
		t.Fatalf("building tools: %v", err)
	}
	res, err := tools[0].Call(context.Background(), mcpCall("get_thing", `{"id":"42","colour":"red"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(res.Reject, `unknown argument(s) "colour"`) {
		t.Errorf("rejection was %q, want it to name the unknown argument", res.Reject)
	}
	// It says what could have been sent instead, which is the actionable half.
	if !strings.Contains(res.Reject, `accepts "id"`) {
		t.Errorf("rejection was %q, want it to list the accepted arguments", res.Reject)
	}
	// And the attempt is still observed, in full.
	if len(seen) != 1 || seen[0].Outcome != ToolOutcomeRejected {
		t.Fatalf("got %d records, outcome %v", len(seen), seen)
	}
	if _, ok := seen[0].Arguments["colour"]; !ok {
		t.Error("the record lost the argument that caused the rejection")
	}
}

// TestFreeFormBodyStillAcceptsAnything: a body declared as a map has keys that
// are not known ahead of time, so the check must not fire for it.
func TestFreeFormBodyStillAcceptsAnything(t *testing.T) {
	app, err := NewApp(AppConfig{
		API: APIConfig{Authenticator: okAuth},
		MCP: &MCPConfig{Name: "probe", Version: "0", Authenticate: okAuth},
		Routes: []Route{{
			Path: "/anything",
			Endpoints: Endpoints{POST: Endpoint{
				Body:    BodyOf[map[string]any](),
				MCPTool: &MCPTool{Name: "post_anything", Description: "d"},
				Handler: func(http.ResponseWriter, *http.Request) {},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	tools, err := app.mcpTools(app.mcp)
	if err != nil {
		t.Fatalf("building tools: %v", err)
	}
	res, err := tools[0].Call(context.Background(), mcpCall("post_anything", `{"whatever":1}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Reject != "" {
		t.Errorf("a free-form body refused an undeclared key: %q", res.Reject)
	}
}
