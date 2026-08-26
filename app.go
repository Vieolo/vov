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

	// Stacks are the named middleware combinations endpoints can be wrapped in,
	// each split into a Pre and Post half by the auth seam — see
	// [MiddlewareStack]. The stack under [DefaultStackName] applies to every
	// endpoint that does not name another; an endpoint naming one that was never
	// declared is a construction error.
	//
	// Stacks apply to endpoints only. Routes registered straight on [App.Mux]
	// are outside the framework and get no middleware from it.
	MiddlewareStacks map[string]MiddlewareStack

	// Authenticator resolves the user a request acts as. Endpoints require an
	// authenticated user unless they declare [NoAuth], so this is required
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
	mux             *http.ServeMux
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
		mux:             http.NewServeMux(),
		shutdownTimeout: cfg.ShutdownTimeout,
	}
	if app.shutdownTimeout == 0 {
		app.shutdownTimeout = DefaultShutdownTimeout
	}

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
			if me.Endpoint.AuthMod.required() && cfg.Authenticator == nil {
				return nil, fmt.Errorf("vov: route %d (%s): requires auth but AppConfig.Authenticator is nil (declare vov.NoAuth() to make it open)", i, p)
			}

			stack, err := resolveStack(cfg.MiddlewareStacks, me.Endpoint.MiddlewareStack)
			if err != nil {
				return nil, fmt.Errorf("vov: route %d (%s): %w", i, p, err)
			}

			app.mux.Handle(p, me.Endpoint.wrapped(stack, cfg.Authenticator))
		}
		app.routes = append(app.routes, r)
	}

	// Resolve the listen address, then materialize the http.Server vov serves
	// with. A nil cfg.Server is fine: ToNetHTTPServer treats it as all-defaults.
	addr := cfg.Address
	if addr == "" {
		addr = DefaultAddress
	}
	app.server = cfg.Server.ToNetHTTPServer(addr, app.mux)

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

// methodLabel names a method for an error message, including the anonymous one.
func methodLabel(method string) string {
	if method == "" {
		return "Any"
	}
	return method
}

// Mux returns the underlying *http.ServeMux. Handlers registered directly on it
// bypass the framework's endpoint management — they will not appear in the route
// manifest later — so use it as an escape hatch for routes vov does not model.
func (a *App) Mux() *http.ServeMux {
	return a.mux
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
