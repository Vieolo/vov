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
	// Handlers are the endpoints to register, in order.
	Handlers []Endpoint

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
	endpoints       []Endpoint
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

	seen := make(map[string]struct{}, len(cfg.Handlers))
	for i, e := range cfg.Handlers {
		if err := validateEndpoint(e); err != nil {
			return nil, fmt.Errorf("vov: handler %d (%q %q): %w", i, e.Method, e.Path, err)
		}
		p := e.pattern()
		if _, dup := seen[p]; dup {
			return nil, fmt.Errorf("vov: handler %d: duplicate route %q", i, p)
		}
		seen[p] = struct{}{}

		app.mux.Handle(p, e.wrapped())
		app.endpoints = append(app.endpoints, e)
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

func validateEndpoint(e Endpoint) error {
	if e.Path == "" {
		return errors.New("path is empty")
	}
	if e.Path[0] != '/' {
		return errors.New(`path must begin with "/"`)
	}
	if e.Handler == nil {
		return errors.New("handler is nil")
	}
	return nil
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
