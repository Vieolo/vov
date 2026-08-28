// Package mcp exposes an application's declared vov endpoints as Model Context
// Protocol tools.
//
// A tool is not a second implementation of an endpoint. It names one, and the
// call is dispatched to it in process through [vov.App.Invoke], so the endpoint's
// middleware runs and its declared policy is enforced — an assistant calling a
// tool meets the same roles, permissions and paid tier a browser would. What the
// declaration cannot supply is the prose an assistant reads and the identity a
// token stands for; those are what this package asks the application for.
//
// It is a separate module, so an application that does not serve MCP never
// resolves or downloads it, nor the protocol SDK underneath.
//
// Nothing is declared twice. An endpoint becomes a tool by carrying a
// [vov.MCPTool], and the server itself is described by [vov.MCPConfig] on the app —
// this package restates no path, no method and no policy, it reads them. Building
// a handler therefore takes only the app:
//
//	h, err := mcp.NewHandler(app)
//	app.Mux().Handle("/mcp", h)
//
// What stays with the application, because vov cannot know it: which bearer
// tokens to honour, any business gate on connecting, audit trails and metering,
// and the OAuth authorization server if there is one. This package handles the
// wiring between a tool call and the endpoint that answers it.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vieolo/vov"
)

// NewHandler builds the http.Handler serving this app's tool-declaring endpoints
// over MCP.
//
// Everything it needs is already declared: [vov.AppConfig.MCP] names the server
// and says how a caller is identified, and each endpoint carrying a [vov.MCPTool]
// becomes a tool.
//
// Most applications never call this. Setting [vov.MCPConfig.Path] and pointing
// [vov.MCPConfig.BuildHandler] at this function is enough — vov builds and mounts
// the handler itself:
//
//	MCP: &vov.MCPConfig{Path: "/mcp", BuildHandler: mcp.NewHandler, ...}
//
// Call it directly only to mount the result somewhere vov would not — inside an
// app's own OAuth gate, for instance, since that gate is not vov's
// Authenticator. Leave Path empty in that case.
func NewHandler(app *vov.App) (http.Handler, error) {
	if app == nil {
		return nil, fmt.Errorf("mcp: app is nil")
	}
	cfg := app.MCP()
	if cfg == nil {
		return nil, fmt.Errorf("mcp: this app declares no AppConfig.MCP")
	}
	server := sdk.NewServer(
		&sdk.Implementation{Name: cfg.Name, Title: cfg.Title, Version: cfg.Version},
		&sdk.ServerOptions{Instructions: cfg.Instructions, Logger: cfg.Logger},
	)

	// The tools are the endpoints that say they are tools. NewApp has already
	// rejected a duplicate name or a tool with no server to serve it, so what is
	// left here is deriving each one's schema from what it declares.
	for _, r := range app.Routes() {
		for _, me := range declaredEndpoints(r) {
			if me.endpoint.MCPTool == nil {
				continue
			}
			bound, err := bindTool(r.Path, me.method, me.endpoint)
			if err != nil {
				return nil, fmt.Errorf("mcp: tool %q (%s %s): %w", me.endpoint.MCPTool.Name, me.method, r.Path, err)
			}
			server.AddTool(bound.tool, bound.toolHandler(app, cfg))
		}
	}

	// Stateless: a tool server sends nothing the client must answer, so there is
	// no session to keep. It matches the sessionless direction of the spec and
	// means the handler holds no per-client state.
	inner := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)

	// Carry the response header down to the tool handlers, so an Authenticator
	// that stamps one — clearing a revoked cookie, say — reaches a real response
	// rather than writing into the void.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), responseHeaderKey{}, w.Header())))
	}), nil
}

// responseHeaderKey types the context slot carrying the HTTP response header of
// the request a tool call arrived on.
type responseHeaderKey struct{}

// boundTool is a tool resolved against the declaration it calls.
type boundTool struct {
	tool       *sdk.Tool
	method     string
	path       string
	pathParams []string
	queryNames []string
}

// bindTool derives a tool's input schema from the endpoint it is declared on.
func bindTool(path, method string, ep vov.Endpoint) (*boundTool, error) {
	t := ep.MCPTool
	b := &boundTool{
		method:     method,
		path:       path,
		pathParams: pathParams(path),
	}

	// A tool takes one flat object of arguments: the path parameters, the
	// declared query fields, and the declared body fields together. Flat is what
	// a model calls well — nesting them under "path"/"query"/"body" would be
	// tidier for us and worse for the thing actually using it.
	props := map[string]any{}
	var required []string

	for _, p := range b.pathParams {
		props[p] = map[string]any{"type": "string"}
		required = append(required, p) // a path parameter is never optional
	}
	if q := ep.Query; q != nil {
		for _, f := range q.Fields {
			if _, clash := props[f.Name]; clash {
				return nil, fmt.Errorf("query field %q collides with a path parameter of the same name", f.Name)
			}
			props[f.Name] = f.Schema.JSONSchema()
			b.queryNames = append(b.queryNames, f.Name)
			if f.Required {
				required = append(required, f.Name)
			}
		}
	}
	if body := ep.Body; body != nil {
		if body.Kind != vov.KindObject {
			return nil, fmt.Errorf("body is %s; a tool's arguments must be an object", body.Kind)
		}
		for _, f := range body.Fields {
			if _, clash := props[f.Name]; clash {
				return nil, fmt.Errorf("body field %q collides with a path parameter or query field of the same name", f.Name)
			}
			props[f.Name] = f.Schema.JSONSchema()
			if f.Required {
				required = append(required, f.Name)
			}
		}
	}

	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}

	readOnly := b.method == http.MethodGet || b.method == http.MethodHead
	if t.ReadOnly != nil {
		readOnly = *t.ReadOnly
	}
	b.tool = &sdk.Tool{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: schema,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: readOnly},
	}
	return b, nil
}

// toolHandler returns the SDK handler that dispatches this tool's call.
func (b *boundTool) toolHandler(app *vov.App, cfg *vov.MCPConfig) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		// The application resolves the caller from the transport's headers. A
		// failure here is a protocol-level error: the assistant cannot fix a
		// broken token store by rephrasing.
		hr := &http.Request{Header: http.Header{}}
		if req.Extra != nil && req.Extra.Header != nil {
			hr.Header = req.Extra.Header
		}
		hr = hr.WithContext(ctx)

		respHeader, _ := ctx.Value(responseHeaderKey{}).(http.Header)
		user, err := cfg.Authenticate(vov.NewAuthResponse(respHeader), hr)
		if err != nil {
			return nil, fmt.Errorf("resolving the caller: %w", err)
		}

		args := map[string]json.RawMessage{}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return toolErrorResult("arguments must be a JSON object"), nil
			}
		}

		path, query, body, err := b.splitArgs(args)
		if err != nil {
			return toolErrorResult(err.Error()), nil
		}

		res, err := app.Invoke(ctx, vov.InvokeRequest{
			Method: b.method,
			Path:   path,
			Query:  query,
			Body:   body,
			User:   user,
			// This package is the MCP channel, so it is the thing that knows to
			// say so. Invoke requires it: dispatching in process is how a tool
			// call reaches its endpoint, not what it is.
			Mode: vov.RequestModeMCP,
		})
		if err != nil {
			return nil, fmt.Errorf("dispatching %s %s: %w", b.method, b.path, err)
		}
		return toolResult(res), nil
	}
}

// splitArgs routes each tool argument to where it belongs: into the path, the
// query string, or the request body.
func (b *boundTool) splitArgs(args map[string]json.RawMessage) (path string, query url.Values, body []byte, err error) {
	path = b.path
	for _, p := range b.pathParams {
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

	if len(b.queryNames) > 0 {
		query = url.Values{}
		for _, n := range b.queryNames {
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
func toolResult(res vov.InvokeResult) *sdk.CallToolResult {
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
func refusalMessage(res vov.InvokeResult) string {
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

// methodEndpoint pairs a route's endpoint with the method it answers.
type methodEndpoint struct {
	method   string
	endpoint vov.Endpoint
}

// declaredEndpoints walks a route's methods in a fixed order, skipping the ones
// it does not answer. vov keeps the same order internally, so a tool list and the
// manifest agree.
func declaredEndpoints(r vov.Route) []methodEndpoint {
	all := []methodEndpoint{
		{http.MethodGet, r.Endpoints.GET},
		{http.MethodHead, r.Endpoints.HEAD},
		{http.MethodPost, r.Endpoints.POST},
		{http.MethodPut, r.Endpoints.PUT},
		{http.MethodPatch, r.Endpoints.PATCH},
		{http.MethodDelete, r.Endpoints.DELETE},
		{http.MethodOptions, r.Endpoints.OPTIONS},
		{"", r.Endpoints.Any},
	}
	out := make([]methodEndpoint, 0, len(all))
	for _, me := range all {
		if me.endpoint.Handler != nil {
			out = append(out, me)
		}
	}
	return out
}

// pathParams returns the wildcard names in a ServeMux pattern, in order.
func pathParams(path string) []string {
	var out []string
	for rest := path; ; {
		open := strings.Index(rest, "{")
		if open < 0 {
			break
		}
		rest = rest[open+1:]
		close := strings.Index(rest, "}")
		if close < 0 {
			break
		}
		name := strings.TrimSuffix(rest[:close], "...")
		if name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
		rest = rest[close+1:]
	}
	return out
}
