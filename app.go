package vov

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// DefaultAddress is the listen address used when [AppConfig.Address] is empty.
const DefaultAddress = ":8080"

// DefaultShutdownTimeout bounds how long [App.Run] waits for in-flight requests
// and cleanup hooks to finish before giving up.
const DefaultShutdownTimeout = 15 * time.Second

// AppConfig is the declarative input to [NewApp]. Apart from Handlers, its fields
// are the app-wide operational preferences; more are added here as they are
// needed rather than imagined up front.
type AppConfig struct {
	// Routes are the URLs to serve, in order. Each groups every method of one
	// URL — see [Route]. A URL may be declared only once: two Routes sharing a
	// Path is a construction error, since it would split one URL's definition
	// across two places.
	Routes []Route

	// RequireGlobals are the [Global] holders that must be populated before this
	// app may be built. Listing one turns "forgot to call Set" from a panic on
	// the first request that happens to touch it into an error at construction,
	// before the server listens.
	//
	// vov never reads what a holder contains; it only asks whether it is ready.
	RequireGlobals []Readiness

	// Stacks are the named middleware combinations endpoints can be wrapped in,
	// each split into a Pre and Post half by the auth seam — see
	// [MiddlewareStack]. The stack under [DefaultStackName] applies to every
	// endpoint that does not name another; an endpoint naming one that was never
	// declared is a construction error.
	//
	// Stacks apply to endpoints only. Routes registered straight on [App.Mux]
	// are outside the framework and get no middleware from it.
	MiddlewareStacks map[string]MiddlewareStack

	// ServerWrappers wrap the whole assembled handler, outermost first, and
	// therefore see every request the server receives — not only the ones that
	// reach an endpoint. They wrap in both directions: a wrapper observes the
	// response on the way out as well as the request on the way in, which is
	// what makes access logging and panic recovery possible here.
	//
	// That is the difference from MiddlewareStacks, and it is not a shade of
	// meaning. http.ServeMux answers two kinds of request itself, before
	// dispatching to anything registered: a path no route declares (404), and a
	// method the matched path does not declare (405). No endpoint middleware
	// runs for either. A CORS preflight is exactly the second kind — OPTIONS
	// against a path registered for GET and PATCH — so a cross-origin browser
	// app cannot work unless something outside the mux answers it.
	//
	// The same is true of the things an operator most wants to be unconditional:
	// panic recovery, request-id stamping, access logging. An unmatched path
	// that panics or goes unlogged is precisely the request worth a record.
	//
	// Because it wraps the mux, it also covers routes registered straight on
	// [App.Mux]. That is unavoidable rather than chosen: the mux decides its own
	// 404 internally, so anything that can see that decision is necessarily
	// outside everything the mux serves.
	//
	// A wrapper runs before routing, so it cannot know which endpoint matched:
	// no path parameters, no AuthMode, no declared roles. It is also invisible to
	// [Manifest], which documents endpoint policy. Keep authorization out of it —
	// a rule declared here cannot be reviewed there.
	ServerWrappers []Middleware

	// Authenticator resolves the user a request acts as. Endpoints require an
	// authenticated user unless they declare [AuthModeNone], so this is required
	// unless every endpoint opts out — [NewApp] rejects a configuration that
	// needs an authenticator and has none, rather than starting a server whose
	// every protected route answers 401.
	Authenticator Authenticator

	// Address is the TCP listen address, e.g. ":8080". Defaults to
	// [DefaultAddress] when empty.
	Address string

	// ShutdownTimeout bounds graceful shutdown. Defaults to
	// [DefaultShutdownTimeout] when zero.
	ShutdownTimeout time.Duration

	// MCP, when set, describes the Model Context Protocol server this app
	// exposes — see [MCPConfig]. The endpoints it exposes are the ones declaring
	// an [MCPTool]; nothing is restated here.
	//
	// Setting it does not serve anything by itself. The vov/mcp module builds a
	// handler from this and the declarations, which the app mounts where it
	// likes.
	MCP *MCPConfig

	// Server optionally tunes the http.Server vov serves with — timeouts, TLS,
	// connection hooks, byte limits, and so on. It mirrors http.Server but drops
	// Addr and Handler, which vov owns (one place for each, no conflicting
	// values). Its defaulted timeout fields are pointers: a nil field takes vov's
	// default, an explicit value — including 0, meaning "no timeout" — is honored.
	// Nil means all defaults. See [Server].
	Server *Server
}

// App assembles a set of [Endpoint] declarations onto a standard http.ServeMux,
// holds the http.Server it will serve with, and owns the server lifecycle.
// Construct it with [NewApp].
type App struct {
	mcp             *MCPConfig
	mux             *http.ServeMux
	handler         http.Handler // mux wrapped in ServerWrappers; what is served
	server          *http.Server
	routes          []Route
	shutdownTimeout time.Duration

	mu         sync.Mutex
	onShutdown []func(context.Context) error
}

// NewApp validates the configuration, builds a mux, registers every handler on
// it, and resolves the http.Server to serve with. It fails closed: an endpoint
// with an empty path, a nil handler, or a route that duplicates an earlier one is
// a construction error, not a boot-time surprise. (Genuinely conflicting — but
// non-identical — mux patterns are still reported by the underlying
// http.ServeMux.)
func NewApp(cfg AppConfig) (*App, error) {
	app := &App{
		mcp:             cfg.MCP,
		mux:             http.NewServeMux(),
		shutdownTimeout: cfg.ShutdownTimeout,
	}
	if app.shutdownTimeout == 0 {
		app.shutdownTimeout = DefaultShutdownTimeout
	}

	// Fail closed on an unpopulated global: a handler that reaches for one would
	// panic on the request that first needs it, in production.
	for i, g := range cfg.RequireGlobals {
		if g == nil {
			return nil, fmt.Errorf("vov: RequireGlobals[%d] is nil", i)
		}
		if !g.Ready() {
			return nil, fmt.Errorf("vov: global %s was never Set", g.Describe())
		}
	}

	seenTool := map[string]string{}
	seenPath := make(map[string]int, len(cfg.Routes))
	for i, r := range cfg.Routes {
		if err := validateRoutePath(r); err != nil {
			return nil, fmt.Errorf("vov: route %d (%q): %w", i, r.Path, err)
		}
		if prev, dup := seenPath[r.Path]; dup {
			return nil, fmt.Errorf("vov: route %d: %q is already declared by route %d — a URL's methods belong in one Route", i, r.Path, prev)
		}
		seenPath[r.Path] = i

		declared := r.Endpoints.declared()
		if len(declared) == 0 {
			return nil, fmt.Errorf("vov: route %d (%s): declares no method", i, r.Path)
		}

		for _, me := range declared {
			p := r.pattern(me.Method)
			if me.Endpoint.Handler == nil {
				return nil, fmt.Errorf("vov: route %d (%s): %s is configured but has no handler", i, r.Path, methodLabel(me.Method))
			}

			// Fail closed: an endpoint that requires auth with no way to
			// authenticate is a configuration bug, not one that should quietly
			// reject everyone.
			if me.Endpoint.AuthMode.required() && cfg.Authenticator == nil {
				return nil, fmt.Errorf("vov: route %d (%s): requires auth but AppConfig.Authenticator is nil (declare vov.AuthModeNone to make it open)", i, p)
			}
			if err := validateAuth(me.Endpoint); err != nil {
				return nil, fmt.Errorf("vov: route %d (%s): %w", i, p, err)
			}
			if err := me.Endpoint.Body.Err(); err != nil {
				return nil, fmt.Errorf("vov: route %d (%s): Body: %w", i, p, err)
			}
			if err := me.Endpoint.Query.Err(); err != nil {
				return nil, fmt.Errorf("vov: route %d (%s): Query: %w", i, p, err)
			}
			if t := me.Endpoint.MCPTool; t != nil {
				if t.Name == "" {
					return nil, fmt.Errorf("vov: route %d (%s): MCPTool has no name", i, p)
				}
				if prev, dup := seenTool[t.Name]; dup {
					return nil, fmt.Errorf("vov: route %d (%s): MCP tool %q is already declared by %s", i, p, t.Name, prev)
				}
				seenTool[t.Name] = p
				if cfg.MCP == nil {
					return nil, fmt.Errorf("vov: route %d (%s): declares MCPTool %q but AppConfig.MCP is nil, so nothing would serve it", i, p, t.Name)
				}
			}

			stack, err := resolveStack(cfg.MiddlewareStacks, me.Endpoint.MiddlewareStack)
			if err != nil {
				return nil, fmt.Errorf("vov: route %d (%s): %w", i, p, err)
			}

			app.mux.Handle(p, me.Endpoint.wrapped(stack, cfg.Authenticator))
		}
		app.routes = append(app.routes, r)
	}

	if cfg.MCP != nil {
		if cfg.MCP.Name == "" || cfg.MCP.Version == "" {
			return nil, fmt.Errorf("vov: AppConfig.MCP: Name and Version are required")
		}
		if cfg.MCP.Authenticate == nil {
			return nil, fmt.Errorf("vov: AppConfig.MCP: Authenticate is required — only the app knows which credentials its tool endpoint honours")
		}
		if len(seenTool) == 0 {
			return nil, fmt.Errorf("vov: AppConfig.MCP is set but no endpoint declares an MCPTool")
		}
		if cfg.MCP.Path != "" {
			if cfg.MCP.Path[0] != '/' {
				return nil, fmt.Errorf("vov: AppConfig.MCP.Path %q must begin with %q", cfg.MCP.Path, "/")
			}
			if cfg.MCP.BuildHandler == nil {
				return nil, fmt.Errorf("vov: AppConfig.MCP.Path is set but BuildHandler is nil (pass mcp.NewHandler)")
			}
			h, err := cfg.MCP.BuildHandler(app)
			if err != nil {
				return nil, fmt.Errorf("vov: building the MCP handler: %w", err)
			}
			if h == nil {
				return nil, fmt.Errorf("vov: AppConfig.MCP.BuildHandler returned a nil handler")
			}
			// Registered like any other route, so it sits inside ServerWrappers
			// and gets the same recovery and logging as everything else.
			app.mux.Handle(cfg.MCP.Path, h)
		}
	}

	// Wrap the finished mux. This is the only layer that sees the requests the
	// mux refuses on its own, so it is what gets served — app.mux is no longer
	// the whole story once ServerWrappers are set.
	app.handler = apply(app.mux, cfg.ServerWrappers)

	// Resolve the listen address, then materialize the http.Server vov serves
	// with. A nil cfg.Server is fine: ToNetHTTPServer treats it as all-defaults.
	addr := cfg.Address
	if addr == "" {
		addr = DefaultAddress
	}
	app.server = cfg.Server.ToNetHTTPServer(addr, app.handler)

	return app, nil
}

func validateRoutePath(r Route) error {
	if r.Path == "" {
		return errors.New("path is empty")
	}
	if r.Path[0] != '/' {
		return errors.New(`path must begin with "/"`)
	}
	return nil
}

// validateAuth rejects declarations that cannot mean what they say.
func validateAuth(e Endpoint) error {
	if !e.AuthMode.valid() {
		return fmt.Errorf("unknown AuthMode %q (use %q, %q, or leave it unset for %q)",
			string(e.AuthMode), AuthModeRequired, AuthModeNone, AuthModeRequired)
	}
	// Roles, permissions, and tier are checked against a user, and an open
	// endpoint never resolves one — so these would silently not be enforced.
	if !e.AuthMode.required() && e.constrained() {
		return errors.New("declares RolesAnyOf, PermissionsAllOf, or MinTier but also vov.AuthModeNone, which resolves no user to check them against")
	}
	// A negative MinTier admits everyone, including tier 0, which is what
	// leaving it unset already means — so it is a mistake, not a way to say
	// "open".
	if e.MinTier < 0 {
		return fmt.Errorf("declares a negative MinTier (%d); leave it unset for no paywall", e.MinTier)
	}
	for _, r := range e.RolesAnyOf {
		if r == "" {
			return errors.New("declares an empty role name")
		}
	}
	for _, p := range e.PermissionsAllOf {
		if p == "" {
			return errors.New("declares an empty permission name")
		}
	}
	return nil
}

// methodLabel names a method for an error message, including the anonymous one.
func methodLabel(method string) string {
	if method == "" {
		return "Any"
	}
	return method
}

// Mux returns the underlying *http.ServeMux, for registering routes vov does not
// model. Handlers registered on it bypass the framework's endpoint management
// and do not appear in [Manifest].
//
// It is the registration surface, not the serving one: it does not include
// [AppConfig.ServerWrappers], which wrap it. To exercise the app as it is
// actually served — in a test, or behind another server — use [App.Handler].
func (a *App) Mux() *http.ServeMux {
	return a.mux
}

// Handler returns what the app serves: the mux wrapped in
// [AppConfig.ServerWrappers]. This is the handler to hand to httptest, or to
// mount inside a larger server when [App.Run] is not being used.
func (a *App) Handler() http.Handler {
	return a.handler
}

// OnShutdown registers a cleanup function to run during graceful shutdown, after
// the HTTP server has stopped accepting connections and in-flight requests have
// drained. Hooks run in registration order, sharing the shutdown deadline. Safe
// to call concurrently.
func (a *App) OnShutdown(fn func(context.Context) error) {
	if fn == nil {
		return
	}
	a.mu.Lock()
	a.onShutdown = append(a.onShutdown, fn)
	a.mu.Unlock()
}

// Run starts the HTTP server and blocks until an interrupt (SIGINT) or
// termination (SIGTERM) signal arrives, or the server stops on its own. On
// signal it stops accepting connections, waits for in-flight requests to drain,
// and runs the registered shutdown hooks, all bounded by ShutdownTimeout.
func (a *App) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return a.run(ctx)
}

// run serves until ctx is cancelled or the server errors, then shuts down
// gracefully. It is separated from Run so the lifecycle can be driven by an
// arbitrary context in tests.
func (a *App) run(ctx context.Context) error {
	srv := a.server

	serveErr := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		// The server stopped before any signal (e.g. it failed to bind).
		return err
	case <-ctx.Done():
		// Signal received; fall through to graceful shutdown.
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer cancel()

	// Stop accepting new connections and let in-flight requests drain first,
	// then release resources via the cleanup hooks.
	shutdownErr := srv.Shutdown(shutdownCtx)
	hookErr := a.runShutdownHooks(shutdownCtx)

	// Surface a genuine serve error (e.g. a bind failure that raced the signal)
	// ahead of shutdown/hook errors.
	if err := <-serveErr; err != nil {
		return err
	}
	return errors.Join(shutdownErr, hookErr)
}

func (a *App) runShutdownHooks(ctx context.Context) error {
	a.mu.Lock()
	hooks := make([]func(context.Context) error, len(a.onShutdown))
	copy(hooks, a.onShutdown)
	a.mu.Unlock()

	var errs []error
	for _, fn := range hooks {
		if err := fn(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
