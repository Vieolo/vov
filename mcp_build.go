package vov

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	mcpsrv "github.com/vieolo/vov/internal/mcp"
)

// MCPHandler returns the http.Handler serving this app's tool-declaring
// endpoints over MCP, for an app that mounts it itself.
//
// Setting [MCPConfig.Path] is the usual way and needs none of this: [NewApp]
// builds the handler and registers it. Reach for this only to mount the result
// somewhere vov would not — inside an application's own OAuth gate, for
// instance, since that gate is not vov's [Authenticator]. Leave Path empty in
// that case.
//
// It returns an error if the app declares no [AppConfig.MCP].
func (a *App) MCPHandler() (http.Handler, error) {
	if a.mcpHandler == nil {
		return nil, fmt.Errorf("vov: this app declares no AppConfig.MCP")
	}
	return a.mcpHandler, nil
}

// buildMCPHandler assembles the tool server from the declarations.
//
// The tools are the endpoints that say they are tools, and each is handed to the
// protocol package with a closure over [App.invoke]. That closure is the whole
// reason dispatch can stay unexported: the protocol package needs the capability,
// not the method, so it can be given one without the other being reachable by
// anything an application imports.
func (a *App) buildMCPHandler(cfg *MCPConfig) (http.Handler, error) {
	var tools []mcpsrv.Tool
	for _, r := range a.routes {
		for _, me := range r.Endpoints.declared() {
			if me.Endpoint.MCPTool == nil {
				continue
			}
			t, err := a.bindTool(r.Path, me.Method, me.Endpoint, cfg)
			if err != nil {
				return nil, fmt.Errorf("tool %q (%s %s): %w",
					me.Endpoint.MCPTool.Name, methodLabel(me.Method), r.Path, err)
			}
			tools = append(tools, t)
		}
	}
	return mcpsrv.NewHandler(mcpsrv.Config{
		Name:         cfg.Name,
		Version:      cfg.Version,
		Title:        cfg.Title,
		Instructions: cfg.Instructions,
		Logger:       cfg.Logger,
		Tools:        tools,
	})
}

// bindTool derives one tool from the endpoint it is declared on.
//
// Nothing is restated: the method, path, arguments and policy are the ones
// already declared, and the [MCPTool] adds only a name and the prose a model
// reads. A name collision between a path parameter and a declared field is a
// construction error, since the flat argument object could not carry both.
func (a *App) bindTool(path, method string, ep Endpoint, cfg *MCPConfig) (mcpsrv.Tool, error) {
	decl := ep.MCPTool
	params := pathParams(path)

	// A tool takes one flat object of arguments: the path parameters, the
	// declared query fields, and the declared body fields together. Flat is what
	// a model calls well — nesting them under "path"/"query"/"body" would be
	// tidier for us and worse for the thing actually using it.
	props := map[string]any{}
	var required, queryNames []string

	for _, p := range params {
		props[p] = map[string]any{"type": "string"}
		required = append(required, p) // a path parameter is never optional
	}
	if q := ep.Query; q != nil {
		for _, f := range q.Fields {
			if _, clash := props[f.Name]; clash {
				return mcpsrv.Tool{}, fmt.Errorf("query field %q collides with a path parameter of the same name", f.Name)
			}
			props[f.Name] = f.Schema.JSONSchema()
			queryNames = append(queryNames, f.Name)
			if f.Required {
				required = append(required, f.Name)
			}
		}
	}
	if body := ep.Body; body != nil {
		if body.Kind != KindObject {
			return mcpsrv.Tool{}, fmt.Errorf("body is %s; a tool's arguments must be an object", body.Kind)
		}
		for _, f := range body.Fields {
			if _, clash := props[f.Name]; clash {
				return mcpsrv.Tool{}, fmt.Errorf("body field %q collides with a path parameter or query field of the same name", f.Name)
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

	readOnly := method == http.MethodGet || method == http.MethodHead
	if decl.ReadOnly != nil {
		readOnly = *decl.ReadOnly
	}

	return mcpsrv.Tool{
		Name:        decl.Name,
		Description: decl.Description,
		ReadOnly:    readOnly,
		InputSchema: schema,
		Path:        path,
		PathParams:  params,
		QueryNames:  queryNames,
		Call:        a.toolDispatch(method, cfg),
	}, nil
}

// toolDispatch builds the closure a tool is called through.
//
// Authentication and dispatch both happen here rather than in the protocol
// package, which is what keeps [User] — and [App.invoke] — out of a package that
// has no business holding either.
func (a *App) toolDispatch(method string, cfg *MCPConfig) func(context.Context, mcpsrv.Request) (mcpsrv.Result, error) {
	return func(ctx context.Context, in mcpsrv.Request) (mcpsrv.Result, error) {
		// The application resolves the caller from the transport's headers. A
		// failure here is a dispatch failure, not a refusal: the assistant cannot
		// fix a broken token store by rephrasing.
		hr := (&http.Request{Header: in.Header}).WithContext(ctx)
		resp := NewAuthResponse(in.RespHeader)
		user, err := cfg.Authenticate(resp, hr)
		if err != nil {
			return mcpsrv.Result{}, fmt.Errorf("resolving the caller: %w", err)
		}

		res, err := a.invoke(ctx, invokeRequest{
			Method: method,
			Path:   in.Path,
			Query:  in.Query,
			Body:   in.Body,
			User:   user,
			// The guard skips the authenticator for a vouched call, so what it
			// reported about the credential has to travel with the identity it
			// reported it alongside.
			Scopes: resp.scopes(),
			// This is the MCP channel, so this is the thing that knows to say so.
			// Dispatching in process is how a tool call reaches its endpoint, not
			// what the call is.
			Mode: RequestModeMCP,
		})
		if err != nil {
			return mcpsrv.Result{}, err
		}
		return mcpsrv.Result{Status: res.Status, Body: res.Body}, nil
	}
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
