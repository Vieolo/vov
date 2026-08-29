package vov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
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
	var bodyNames []string
	// A body declared as an object with no field list is a map: its keys are not
	// known ahead of time, so anything left over legitimately belongs to it.
	freeFormBody := ep.Body != nil && len(ep.Body.Fields) == 0
	if body := ep.Body; body != nil {
		if body.Kind != KindObject {
			return mcpsrv.Tool{}, fmt.Errorf("body is %s; a tool's arguments must be an object", body.Kind)
		}
		for _, f := range body.Fields {
			bodyNames = append(bodyNames, f.Name)
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
		Call: a.toolDispatch(boundTool{
			name:         decl.Name,
			method:       method,
			path:         path,
			pathParams:   params,
			queryNames:   queryNames,
			bodyNames:    bodyNames,
			freeFormBody: freeFormBody,
		}, cfg),
	}, nil
}

// errStack is what a panicking tool handler reports to the client. It says
// nothing: a panic's value is written for an operator, reaches the
// [MCPConfig.OnToolCall] record instead, and relaying it would hand an assistant
// internal detail it has no use for and should not have.
var errStack = errors.New("the server failed to handle this tool call")

// boundTool is a tool resolved against the declaration it calls.
type boundTool struct {
	name         string
	method       string
	path         string
	pathParams   []pathArg
	queryNames   []string
	bodyNames    []string
	freeFormBody bool
}

// accepts lists every argument name this tool takes, for an error message that
// tells an assistant what it could have sent instead.
func (t boundTool) accepts() []string {
	out := make([]string, 0, len(t.pathParams)+len(t.queryNames)+len(t.bodyNames))
	for _, p := range t.pathParams {
		out = append(out, p.name)
	}
	out = append(out, t.queryNames...)
	out = append(out, t.bodyNames...)
	slices.Sort(out)
	return out
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
			observeToolCall(ctx, cfg, call)
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
		// Cloned, not aliased. splitArgs consumes the map as it routes — deleting
		// each argument it places in the path or the query — so sharing it would
		// leave the record holding only what vov failed to understand, which for
		// a read tool whose inputs are all path parameters is nothing at all.
		// Copying before the split is also what keeps a rejected call's record
		// whole, since that path returns before the split finishes.
		call.Arguments = maps.Clone(args)

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
			vs, err := queryArg(raw)
			if err != nil {
				return "", nil, nil, fmt.Errorf("argument %q: %w", n, err)
			}
			for _, v := range vs {
				query.Add(n, v)
			}
			delete(args, n)
		}
	}

	// Whatever is left is the body — but only what the body actually declares.
	//
	// An argument matching nothing used to be forwarded anyway, which meant a
	// model that invented a parameter name had it silently marshalled into a
	// request the endpoint never advertised, and learned nothing. The schema
	// already names every argument this tool takes, so anything else is a
	// mistake worth saying out loud: "unknown argument" is something an
	// assistant can act on, and silence is not.
	if len(args) > 0 {
		if !t.freeFormBody {
			var unknown []string
			for name := range args {
				if !slices.Contains(t.bodyNames, name) {
					unknown = append(unknown, name)
				}
			}
			if len(unknown) > 0 {
				slices.Sort(unknown) // map order would make the message vary
				return "", nil, nil, fmt.Errorf("unknown argument(s) %s; this tool accepts %s",
					strings.Join(quoteAll(unknown), ", "), strings.Join(quoteAll(t.accepts()), ", "))
			}
		}
		if body, err = json.Marshal(args); err != nil {
			return "", nil, nil, fmt.Errorf("encoding the request body: %w", err)
		}
	}
	return path, query, body, nil
}

// queryArg renders a JSON tool argument as the values a query string carries.
//
// A list becomes a repeated parameter, which is what [QueryOf] has always
// permitted a query field to be: it accepts a scalar or a list of scalars and
// rejects only a list of objects. Dispatch used to accept the scalar and refuse
// the list, so a declaration vov validated at construction could fail at call
// time — the one kind of failure vov is arranged to make impossible.
func queryArg(raw json.RawMessage) ([]string, error) {
	// A JSON array unmarshals into this; anything else does not, which is a
	// cheaper test than inspecting the bytes and is exact.
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		out := make([]string, 0, len(list))
		for i, elem := range list {
			v, err := scalarArg(elem)
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			out = append(out, v)
		}
		return out, nil
	}

	v, err := scalarArg(raw)
	if err != nil {
		return nil, err
	}
	if v == "" {
		// An empty scalar is an omitted parameter rather than an empty one: a
		// handler reading q.Get sees the same thing either way, and sending it
		// would make "unset" and "set to nothing" indistinguishable.
		return nil, nil
	}
	return []string{v}, nil
}

// quoteAll renders names for an error message.
func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
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
