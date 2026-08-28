// Package mcp adapts a set of already-described tools onto the Model Context
// Protocol. It is the protocol half of vov's MCP support, and nothing else.
//
// It knows no vov types, and deliberately: it is handed plain descriptions and a
// function to call, so the framework can keep its dispatch private and still let
// a tool call reach an endpoint. Everything semantic — which endpoints are tools,
// what their arguments are, who the caller is, and whether they may — is decided
// by the framework before anything here runs. What is left is genuinely
// protocol-shaped: registering tools, mapping a flat argument object onto a path,
// query and body, and rendering an answer a model can act on.
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
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Request is one dispatched tool call, after its arguments have been mapped onto
// the shape an endpoint expects.
type Request struct {
	// Path is the tool's path with its parameters substituted.
	Path string

	// Query and Body carry the arguments that belong in each.
	Query url.Values
	Body  []byte

	// Header is the transport header of the HTTP request the tool call arrived
	// on, which is where a caller's credentials are.
	Header http.Header

	// RespHeader is the header of the response that request will produce, so an
	// authenticator can stamp one on its way past — clearing a revoked cookie,
	// rotating a live one.
	RespHeader http.Header
}

// Result is what the endpoint answered.
type Result struct {
	Status int
	Body   []byte
}

// Tool is one callable tool, fully described.
type Tool struct {
	Name        string
	Description string
	ReadOnly    bool

	// InputSchema is the JSON Schema of the tool's flat argument object.
	InputSchema map[string]any

	// Path is the route pattern, e.g. "/tasks/{id}". PathParams names its
	// wildcards in order, and QueryNames the arguments that belong in the query
	// string; everything else an assistant sends becomes the body.
	Path       string
	PathParams []string
	QueryNames []string

	// Call dispatches this tool's request and returns what the endpoint
	// answered. It is a closure the framework builds over its own unexported
	// dispatch, which is what lets this package reach an endpoint while nothing
	// outside the framework can.
	//
	// A returned error means the call could not be dispatched at all — an
	// unresolvable caller, a broken credential store. A refusal is not an error:
	// it comes back as a Result carrying its status.
	Call func(context.Context, Request) (Result, error)
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

// handler returns the SDK handler that dispatches this tool's call.
func (t Tool) handler() sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		args := map[string]json.RawMessage{}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return toolErrorResult("arguments must be a JSON object"), nil
			}
		}

		path, query, body, err := t.splitArgs(args)
		if err != nil {
			return toolErrorResult(err.Error()), nil
		}

		var header http.Header
		if req.Extra != nil && req.Extra.Header != nil {
			header = req.Extra.Header
		} else {
			header = http.Header{}
		}
		respHeader, _ := ctx.Value(responseHeaderKey{}).(http.Header)

		res, err := t.Call(ctx, Request{
			Path:       path,
			Query:      query,
			Body:       body,
			Header:     header,
			RespHeader: respHeader,
		})
		if err != nil {
			// A protocol-level error: the assistant cannot fix a broken token
			// store by rephrasing, so this is not a tool error it should retry.
			return nil, fmt.Errorf("dispatching %s: %w", t.Name, err)
		}
		return toolResult(res), nil
	}
}

// splitArgs routes each tool argument to where it belongs: into the path, the
// query string, or the request body.
func (t Tool) splitArgs(args map[string]json.RawMessage) (path string, query url.Values, body []byte, err error) {
	path = t.Path
	for _, p := range t.PathParams {
		raw, ok := args[p]
		if !ok {
			return "", nil, nil, fmt.Errorf("missing required argument %q", p)
		}
		v, err := scalarArg(raw)
		if err != nil {
			return "", nil, nil, fmt.Errorf("argument %q: %w", p, err)
		}
		path = strings.Replace(path, "{"+p+"}", url.PathEscape(v), 1)
		delete(args, p)
	}

	if len(t.QueryNames) > 0 {
		query = url.Values{}
		for _, n := range t.QueryNames {
			raw, ok := args[n]
			if !ok {
				continue
			}
			v, err := scalarArg(raw)
			if err != nil {
				return "", nil, nil, fmt.Errorf("argument %q: %w", n, err)
			}
			if v != "" {
				query.Set(n, v)
			}
			delete(args, n)
		}
	}

	// Whatever is left is the body. An endpoint declaring no body gets none.
	if len(args) > 0 {
		if body, err = json.Marshal(args); err != nil {
			return "", nil, nil, fmt.Errorf("encoding the request body: %w", err)
		}
	}
	return path, query, body, nil
}

// scalarArg renders a JSON tool argument as the string a path or query carries.
func scalarArg(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("is not valid JSON")
	}
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case bool, float64:
		return strings.Trim(string(raw), `"`), nil
	default:
		return "", fmt.Errorf("must be a string, number or boolean")
	}
}

// toolResult turns an endpoint's response into a tool result.
//
// A refusal is reported as a tool error rather than a protocol error, so the
// assistant is told what happened and can act on it — asking the user to
// subscribe, or giving up on a record it may not read — instead of seeing an
// opaque failure it will simply retry.
func toolResult(res Result) *sdk.CallToolResult {
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
