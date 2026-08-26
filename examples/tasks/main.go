// Command tasks is a small example service built with the vov framework. It
// exists to exercise vov's ergonomics from the outside: declarative endpoints, a
// default middleware stack that endpoints inherit, extend, override, or drop,
// authentication that applies unless a route opts out, the mux escape hatch, a
// cleanup hook, and a lifecycle-managed Run. It keeps its data in memory and
// depends only on the standard library plus vov.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vieolo/vov"
)

// config is both the declaration of what this service reads from the
// environment and the way it reads it, so the two cannot drift apart. Required
// variables stop the server from starting; the rest fall back to a default or a
// zero value.
type config struct {
	Addr     string        `env:"TASKS_ADDR" envDefault:":8080"`
	Token    string        `env:"TASKS_TOKEN,required"`
	Greeting string        `env:"TASKS_GREETING" envDefault:"ok"`
	MaxTasks int           `env:"TASKS_MAX" envDefault:"100"`
	Debug    bool          `env:"TASKS_DEBUG"`
	Idle     time.Duration `env:"TASKS_IDLE_TIMEOUT" envDefault:"90s"`
}

func main() {
	// Load the environment first: everything below is built from it, which is
	// why this is a plain function and not a field of vov.AppConfig.
	var cfg config
	if err := vov.LoadEnv(&cfg); err != nil {
		log.Fatalf("tasks: %v", err)
	}
	if cfg.Debug {
		log.Printf("tasks: config %+v", cfg.redacted())
	}

	store := newTaskStore(cfg.MaxTasks)

	app, err := vov.NewApp(vov.AppConfig{
		Address: cfg.Addr,
		// Tune the underlying server with a vov.Server: the http.Server knobs
		// minus Addr and Handler (vov owns those). Defaulted timeouts are
		// pointers — leave one nil to take vov's default, or set it inline with
		// vov.Ptr (even to 0, meaning "no timeout"). Other fields pass through.
		Server: &vov.Server{
			ReadHeaderTimeout: vov.Ptr(5 * time.Second),
			IdleTimeout:       vov.Ptr(cfg.Idle),
		},
		// The middleware combinations this service uses, named once here. Pre
		// runs outside the auth guard (so it covers rejected requests too); Post
		// runs inside it, where the user is known.
		MiddlewareStacks: map[string]vov.MiddlewareStack{
			// Applies to every endpoint that does not name another stack.
			vov.DefaultStackName: {
				Pre:  []vov.Middleware{requestID, logging},
				Post: []vov.Middleware{auditLog},
			},
			// The default plus a body-type check, for endpoints that take JSON.
			"json": {
				Pre:  []vov.Middleware{requestID, logging},
				Post: []vov.Middleware{auditLog, requireJSON},
			},
			// Authenticated by signature rather than by user, so the check goes
			// in Pre: this stack's endpoints declare NoAuth, and Post never runs
			// for them.
			"webhook": {
				Pre: []vov.Middleware{requestID, logging, verifySignature},
			},
			// Deliberately nothing. A health check should not depend on any of it.
			"bare": {},
		},
		// How this app resolves the user of a request. Endpoints require one
		// unless they declare vov.NoAuth(). The valid token comes from the
		// environment, which is why the authenticator is built from cfg.
		Authenticator: makeAuthenticator(cfg.Token),
		// One Route per URL: every method that URL answers is declared in one
		// place, and each method carries its own configuration.
		Routes: []vov.Route{
			{
				Path: "/healthz",
				// Open and unwrapped.
				GET: vov.Endpoint{Handler: healthz(cfg.Greeting),
					MiddlewareStack: "bare", AuthMod: vov.NoAuth()},
			},
			{
				Path: "/tasks",
				// Nothing declared, so: default stack, auth required. The
				// majority case says nothing and is protected anyway.
				GET: vov.Endpoint{Handler: store.list},
				// Same URL, same auth, different wrapping — one word says which.
				POST: vov.Endpoint{Handler: store.create, MiddlewareStack: "json"},
			},
			{
				Path:   "/tasks/{id}",
				GET:    vov.Endpoint{Handler: store.get},
				DELETE: vov.Endpoint{Handler: store.delete},
			},
			{
				Path: "/webhook",
				POST: vov.Endpoint{Handler: webhook,
					MiddlewareStack: "webhook", AuthMod: vov.NoAuth()},
			},
		},
	})
	if err != nil {
		log.Fatalf("tasks: %v", err)
	}

	// Escape hatch: register straight on the underlying mux, bypassing vov.
	// Useful for anything vov does not model yet. These routes are entirely
	// yours — the framework adds no middleware and no auth to them.
	app.Mux().HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": "0.1.0"})
	})

	// Cleanup hook, run during graceful shutdown.
	app.OnShutdown(func(ctx context.Context) error {
		log.Printf("tasks: %d task(s) in memory at exit", store.len())
		return nil
	})

	log.Printf("tasks: listening on :8080")
	if err := app.Run(); err != nil {
		log.Fatalf("tasks: %v", err)
	}
}

// redacted returns a copy safe to log: the token is a credential and must never
// reach a log line, the way vov itself never puts a value in an EnvError.
func (c config) redacted() config {
	if c.Token != "" {
		c.Token = "[redacted]"
	}
	return c
}

// --- auth -------------------------------------------------------------------

// user is this app's own model. It satisfies vov.User by answering the one
// question vov asks; everything else about it stays the app's business.
type user struct {
	name string
}

func (u *user) IsAuthenticated() bool { return u != nil && u.name != "" }

// makeAuthenticator builds the app's vov.Authenticator around the token from the
// environment. It owns the credential lookup, which in a real service would hit
// a session table or verify a signature.
func makeAuthenticator(valid string) vov.Authenticator {
	return func(r *http.Request) (vov.User, error) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case token == "":
			return nil, nil // no credentials presented -> 401
		case token == "t-boom":
			return nil, errors.New("session store unavailable") // -> 500, not 401
		case token == valid:
			return &user{name: "ramtin"}, nil
		default:
			return nil, nil
		}
	}
}

// currentUser is the typed accessor the app defines over vov's interface. vov
// hands back a vov.User; the concrete type reappears here, in one place.
func currentUser(r *http.Request) *user {
	u, ok := vov.UserFrom(r.Context())
	if !ok {
		return nil
	}
	return u.(*user)
}

// --- domain -----------------------------------------------------------------

type task struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Owner string `json:"owner"`
	Done  bool   `json:"done"`
}

type taskStore struct {
	mu     sync.RWMutex
	nextID int
	max    int
	items  map[int]task
}

func newTaskStore(max int) *taskStore {
	return &taskStore{max: max, items: make(map[int]task)}
}

func (s *taskStore) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

func (s *taskStore) list(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	out := make([]task, 0, len(s.items))
	for _, t := range s.items {
		out = append(out, t)
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, out)
}

func (s *taskStore) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if in.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
		return
	}

	// The endpoint requires auth, so the guard has already run and there is
	// always a user here.
	s.mu.Lock()
	if len(s.items) >= s.max {
		s.mu.Unlock()
		writeJSON(w, http.StatusInsufficientStorage, map[string]string{"error": "task limit reached"})
		return
	}
	s.nextID++
	t := task{ID: s.nextID, Title: in.Title, Owner: currentUser(r).name}
	s.items[t.ID] = t
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, t)
}

func (s *taskStore) get(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}

	s.mu.RLock()
	t, ok := s.items[id]
	s.mu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// healthz reports the greeting configured in the environment, which is the
// shortest way to see a value travel from an env var to a response body.
func (s *taskStore) delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}

	s.mu.Lock()
	_, ok := s.items[id]
	delete(s.items, id)
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func healthz(greeting string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": greeting})
	}
}

func webhook(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"received": "ok"})
}

// --- middleware -------------------------------------------------------------

// requestID stamps every response with an id. Part of the default stack, so its
// presence in a response is evidence that stack ran.
func requestID(next http.Handler) http.Handler {
	var n atomicCounter
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", strconv.FormatUint(n.next(), 10))
		next.ServeHTTP(w, r)
	})
}

// logging records method, path, and latency. Part of the default stack. Because
// the auth guard runs inside the middleware chain, rejected requests are logged
// here too.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start))
	})
}

// auditLog records who performed the request. It is the reason the after-auth
// phase exists: it needs the user, so it cannot run in the outer stack, which
// executes before anyone has been authenticated.
func auditLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		w.Header().Set("X-Audit-User", u.name)
		log.Printf("audit: %s %s by %s", r.Method, r.URL.Path, u.name)
		next.ServeHTTP(w, r)
	})
}

// requireJSON rejects a body that is not declared as JSON. Added to one endpoint
// on top of the default stack.
func requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "" && ct != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "expected application/json"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// verifySignature stands in for a real webhook signature check. It replaces the
// default stack rather than adding to it.
func verifySignature(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Signature") == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing signature"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- plumbing ---------------------------------------------------------------

type atomicCounter struct {
	mu sync.Mutex
	n  uint64
}

func (c *atomicCounter) next() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
