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

// AppConfig is the declarative input to [NewApp].
type AppConfig struct {
	// Metadata describes the project. Optional but recommended.
	Metadata Metadata

	// Handlers are the endpoints to register, in order.
	Handlers []Endpoint

	// Address is the TCP listen address, e.g. ":8080". Defaults to
	// [DefaultAddress] when empty.
	Address string

	// ShutdownTimeout bounds graceful shutdown. Defaults to
	// [DefaultShutdownTimeout] when zero.
	ShutdownTimeout time.Duration
}

// App assembles a set of [Endpoint] declarations onto a standard http.ServeMux
// and owns the server lifecycle. Construct it with [NewApp].
type App struct {
	meta            Metadata
	mux             *http.ServeMux
	endpoints       []Endpoint
	addr            string
	shutdownTimeout time.Duration

	mu         sync.Mutex
	onShutdown []func(context.Context) error
}

// NewApp validates the configuration, builds a mux, and registers every handler
// on it. It fails closed: an endpoint with an empty path, a nil handler, or a
// route that duplicates an earlier one is a construction error, not a boot-time
// surprise. (Genuinely conflicting — but non-identical — mux patterns are still
// reported by the underlying http.ServeMux.)
func NewApp(cfg AppConfig) (*App, error) {
	app := &App{
		meta:            cfg.Metadata,
		mux:             http.NewServeMux(),
		addr:            cfg.Address,
		shutdownTimeout: cfg.ShutdownTimeout,
	}
	if app.addr == "" {
		app.addr = DefaultAddress
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
	srv := &http.Server{
		Addr:    a.addr,
		Handler: a.mux,
	}

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
