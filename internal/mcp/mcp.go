// Package mcp adapts a set of already-described tools onto the Model Context
// Protocol. It is the protocol half of vov's MCP support, and nothing else.
//
// It knows no vov types, and deliberately: it is handed plain descriptions and a
// function to call, so the framework can keep its dispatch private and still let
// a tool call reach an endpoint.
//
// The division is narrower than it first looks. Everything with a decision in it
// — who the caller is, which arguments belong in the path, whether the endpoint
// answers — happens inside the function the framework supplies, so that a tool
// call has exactly one place it can be refused and exactly one place its outcome
// is recorded. What is left here is the protocol surface: describing tools to a
// client, and rendering an answer a model can act on.
//
// It is internal because an application has no reason to name it. vov mounts the
// handler itself, or hands it over through App.MCPHandler.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Call is one tool invocation, exactly as it arrived.
//
// The arguments are still raw: decoding them is the framework's, because the
// order in which a call is authenticated and its arguments are read is a policy
// decision and this package should not be the one making it.
type Call struct {
	// Tool is the name the client asked for.
	Tool string

	// RawArgs is the arguments object the client sent, undecoded. It may be
	// empty for a tool that takes none.
	RawArgs json.RawMessage

	// Header is the transport header of the HTTP request the call arrived on,
	// which is where the caller's credentials are.
	Header http.Header

	// RespHeader is the header of the response that request will produce, so an
	// authenticator can stamp one on its way past.
	RespHeader http.Header
}

// Result is the answer to a Call.
type Result struct {
	// Status and Body are what the endpoint answered, when one was reached.
	Status int
	Body   []byte

	// Reject, when non-empty, means the call never reached an endpoint and this
	// is what the assistant should be told — a missing argument, an unusable
	// value. It is reported as a tool error rather than a protocol error,
	// because an assistant can act on it by calling again correctly.
	Reject string
}

// Tool is one callable tool, as a client sees it.
type Tool struct {
	Name        string
	Description string
	ReadOnly    bool

	// InputSchema is the JSON Schema of the tool's flat argument object.
	InputSchema map[string]any

	// Call performs the invocation. It is a closure the framework builds over
	// its own unexported dispatch, which is what lets this package reach an
	// endpoint while nothing outside the framework can.
	//
	// A returned error means the call could not be dispatched at all — an
	// unresolvable caller, a broken credential store — and becomes a protocol
	// error, because rephrasing will not fix it. Everything an assistant could
	// act on comes back in the Result.
	Call func(context.Context, Call) (Result, error)
}

// Config describes the server to build.
type Config struct {
	Name         string
	Version      string
	Title        string
	Instructions string
	Logger       *slog.Logger
	Tools        []Tool
}

// NewHandler builds the http.Handler serving cfg's tools.
func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.Name == "" || cfg.Version == "" {
		return nil, fmt.Errorf("mcp: Name and Version are required")
	}
	server := sdk.NewServer(
		&sdk.Implementation{Name: cfg.Name, Title: cfg.Title, Version: cfg.Version},
		&sdk.ServerOptions{Instructions: cfg.Instructions, Logger: cfg.Logger},
	)
	for _, t := range cfg.Tools {
		if t.Call == nil {
			return nil, fmt.Errorf("mcp: tool %q has no Call", t.Name)
		}
		server.AddTool(&sdk.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: t.ReadOnly},
		}, t.handler())
	}

	// Stateless: a tool server sends nothing the client must answer, so there is
	// no session to keep. It matches the sessionless direction of the spec and
	// means the handler holds no per-client state.
	inner := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)

	// Carry the response header down to the tool handlers, so an authenticator
	// that stamps one — clearing a revoked cookie, say — reaches a real response
	// rather than writing into the void.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), responseHeaderKey{}, w.Header())
		inner.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

// responseHeaderKey types the context slot carrying the HTTP response header of
// the request a tool call arrived on.
type responseHeaderKey struct{}

// handler returns the SDK handler for this tool. It hands the call straight over
// and renders whatever comes back: every exit a caller can influence is decided
// inside Call, so that one place decides and one place records.
func (t Tool) handler() sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		header := http.Header{}
		if req.Extra != nil && req.Extra.Header != nil {
			header = req.Extra.Header
		}
		respHeader, _ := ctx.Value(responseHeaderKey{}).(http.Header)

		res, err := t.Call(ctx, Call{
			Tool:       t.Name,
			RawArgs:    req.Params.Arguments,
			Header:     header,
			RespHeader: respHeader,
		})
		if err != nil {
			return nil, fmt.Errorf("dispatching %s: %w", t.Name, err)
		}
		return toolResult(res), nil
	}
}

// toolResult turns a Result into what the client receives.
//
// A refusal is reported as a tool error rather than a protocol error, so the
// assistant is told what happened and can act on it — asking the user to
// subscribe, or giving up on a record it may not read — instead of seeing an
// opaque failure it will simply retry.
func toolResult(res Result) *sdk.CallToolResult {
	if res.Reject != "" {
		return toolErrorResult(res.Reject)
	}
	if res.Status >= 200 && res.Status < 300 {
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: string(res.Body)}},
		}
	}
	return toolErrorResult(refusalMessage(res))
}

// refusalMessage explains a non-2xx status in terms an assistant can act on.
//
// The status is used rather than the body because vov writes its own refusals as
// bare status text: relaying "Forbidden" tells a model nothing it can do
// something about.
func refusalMessage(res Result) string {
	switch res.Status {
	case http.StatusUnauthorized:
		return "Not signed in: this call was not authenticated."
	case http.StatusPaymentRequired:
		return "This requires a paid subscription the account does not currently have."
	case http.StatusForbidden:
		return "Not permitted: the account may not perform this action."
	case http.StatusNotFound:
		return "Not found: no such record, or it is not visible to this account."
	}
	body := strings.TrimSpace(string(res.Body))
	if res.Status >= 500 {
		// Never relay a server error body: it is written for an operator.
		return fmt.Sprintf("The server failed to handle this request (%d).", res.Status)
	}
	if body == "" {
		return fmt.Sprintf("The request was rejected (%d).", res.Status)
	}
	return fmt.Sprintf("The request was rejected (%d): %s", res.Status, body)
}

func toolErrorResult(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: msg}},
	}
}
