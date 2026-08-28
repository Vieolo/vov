package vov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	tools, err := a.mcpTools(cfg)
	if err != nil {
		return nil, err
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

// mcpTools binds every tool-declaring endpoint. Separated from the handler so
// that a test can exercise one tool's dispatch without a protocol round trip:
// what is worth pinning is what vov decides, not what the SDK renders.
func (a *App) mcpTools(cfg *MCPConfig) ([]mcpsrv.Tool, error) {
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
	return tools, nil
}

// bindTool derives one tool from the endpoint it is declared on.
//
// Nothing is restated: the method, path, arguments and policy are the ones
// already declared, and the [MCPTool] adds only a name and the prose a model
// reads. A name collision between a path parameter and a declared field is a
// construction error, since the flat argument object could not carry both.
func (a *App) bindTool(path, method string, ep Endpoint, cfg *MCPConfig) (mcpsrv.Tool, error) {
	decl := ep.MCPTool
	params := resolvePathArgs(path, ep)

	// A tool takes one flat object of arguments: the path parameters, the
	// declared query fields, and the declared body fields together. Flat is what
	// a model calls well — nesting them under "path"/"query"/"body" would be
	// tidier for us and worse for the thing actually using it.
	props := map[string]any{}
	var required, queryNames []string

	for _, p := range params {
		prop := map[string]any{"type": "string"}
		if p.description != "" {
			prop["description"] = p.description
		}
		props[p.name] = prop
		required = append(required, p.name) // a path parameter is never optional
	}
	if q := ep.Query; q != nil {
		for _, f := range q.Fields {
			if _, clash := props[f.Name]; clash {
				return mcpsrv.Tool{}, fmt.Errorf("query field %q collides with a path parameter of the same name", f.Name)
			}
			props[f.Name] = f.JSONSchema()
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
			props[f.Name] = f.JSONSchema()
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
		Call:        a.toolDispatch(boundTool{decl.Name, method, path, params, queryNames}, cfg),
	}, nil
}

// errStack is what a panicking tool handler reports to the client. It says
// nothing: a panic's value is written for an operator, reaches the
// [MCPConfig.OnToolCall] record instead, and relaying it would hand an assistant
// internal detail it has no use for and should not have.
var errStack = errors.New("the server failed to handle this tool call")

// boundTool is a tool resolved against the declaration it calls.
type boundTool struct {
	name       string
	method     string
	path       string
	pathParams []pathArg
	queryNames []string
}

// toolDispatch builds the closure a tool is called through.
//
// Everything a caller can influence happens here, in one place and one order:
// resolve the caller, read the arguments, dispatch. That the order is visible is
// the point — authentication comes first so that a call rejected for a bad
// argument is still attributable to whoever made it, which is exactly the call an
// operator wants to see when an assistant is looping on one.
//
// The single deferred observation is deliberate too. Scattering it across the
// exits would leave "it must fire for the failures as well" one early return away
// from being untrue again, which is how it comes to be missing in the first place.
func (a *App) toolDispatch(t boundTool, cfg *MCPConfig) func(context.Context, mcpsrv.Call) (mcpsrv.Result, error) {
	return func(ctx context.Context, in mcpsrv.Call) (out mcpsrv.Result, err error) {
		started := time.Now()
		call := ToolCall{Tool: t.name, Outcome: ToolOutcomeFailed}
		defer func() {
			// Recovery has to happen here, and it is not belt-and-braces on top
			// of [APIConfig.ServerWrappers]: those cannot reach this. The
			// protocol SDK dispatches each tool call on a goroutine of its own,
			// so a wrapper's deferred recover — which sits on the goroutine
			// serving the HTTP request the call arrived on — never sees the
			// panic, and neither does net/http's. Without this, one panicking
			// handler ends the process.
			panicked := recover()
			if panicked != nil {
				out, err = mcpsrv.Result{}, errStack
			}
			call.Err, call.Status, call.Duration = err, out.Status, time.Since(started)
			if panicked != nil {
				// The client is told nothing beyond "it failed"; the detail is
				// written for whoever is on call.
				call.Err = fmt.Errorf("panic in tool %q: %v", t.name, panicked)
			}
			if err == nil && out.Reject == "" {
				call.Outcome = outcomeOf(out.Status)
			}
			observeToolCall(cfg, call)
		}()

		// The application resolves the caller from the transport's headers. A
		// failure here is a dispatch failure, not a refusal: the assistant cannot
		// fix a broken token store by rephrasing.
		hr := (&http.Request{Header: in.Header}).WithContext(ctx)
		resp := NewAuthResponse(in.RespHeader)
		user, err := cfg.Authenticate(resp, hr)
		if err != nil {
			return mcpsrv.Result{}, fmt.Errorf("resolving the caller: %w", err)
		}
		call.User, call.Scopes = user, resp.scopes()

		// A rejection is an answer, not a failure: the assistant is told what was
		// wrong so it can call again correctly. The named error result stays nil,
		// and the outcome carries the fact instead.
		args, argErr := decodeToolArgs(in.RawArgs)
		if argErr != nil {
			call.Outcome = ToolOutcomeRejected
			return mcpsrv.Result{Reject: "arguments must be a JSON object"}, nil
		}
		call.Arguments = args

		path, query, body, mapErr := t.splitArgs(args)
		if mapErr != nil {
			call.Outcome = ToolOutcomeRejected
			return mcpsrv.Result{Reject: mapErr.Error()}, nil
		}

		res, err := a.invoke(ctx, invokeRequest{
			Method: t.method,
			Path:   path,
			Query:  query,
			Body:   body,
			// The transport headers of the call, so an application's own
			// middleware sees the request it actually arrived as. It is the seam
			// an app enriches a tool call through — the same Pre middleware it
			// already writes, rather than a second, MCP-only hook.
			Header: in.Header,
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

// decodeToolArgs reads the arguments object a client sent.
func decodeToolArgs(raw json.RawMessage) (map[string]json.RawMessage, error) {
	args := map[string]json.RawMessage{}
	if len(raw) == 0 {
		return args, nil
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return args, nil
}

// splitArgs routes each tool argument to where it belongs: into the path, the
// query string, or the request body.
//
// It lives here rather than in the protocol package because what it maps onto is
// vov's route model — a path pattern's wildcards and an endpoint's declared query
// fields — and not anything MCP defines.
func (t boundTool) splitArgs(args map[string]json.RawMessage) (path string, query url.Values, body []byte, err error) {
	path = t.path
	for _, p := range t.pathParams {
		// Looked up by the name the caller uses, substituted by the name the
		// pattern uses. They differ whenever the endpoint declared an alias.
		raw, ok := args[p.name]
		if !ok {
			return "", nil, nil, fmt.Errorf("missing required argument %q", p.name)
		}
		v, err := scalarArg(raw)
		if err != nil {
			return "", nil, nil, fmt.Errorf("argument %q: %w", p.name, err)
		}
		path = strings.Replace(path, "{"+p.wildcard+"}", url.PathEscape(v), 1)
		delete(args, p.name)
	}

	if len(t.queryNames) > 0 {
		query = url.Values{}
		for _, n := range t.queryNames {
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
