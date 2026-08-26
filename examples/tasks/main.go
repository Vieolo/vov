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

func main() {
	store := newTaskStore()

	app, err := vov.NewApp(vov.AppConfig{
		Address: ":8080",
		// Tune the underlying server with a vov.Server: the http.Server knobs
		// minus Addr and Handler (vov owns those). Defaulted timeouts are
		// pointers — leave one nil to take vov's default, or set it inline with
		// vov.Ptr (even to 0, meaning "no timeout"). Other fields pass through.
		Server: &vov.Server{
			ReadHeaderTimeout: vov.Ptr(5 * time.Second),
			IdleTimeout:       vov.Ptr(90 * time.Second),
		},
		// The default stack, applied to every endpoint that does not say
		// otherwise. Outermost first: requestID wraps logging wraps the handler.
		// This runs outside the auth guard, so it also covers rejected requests.
		Middleware: []vov.Middleware{requestID, logging},
		// Applied inside the auth guard, so these can read the user. Skipped on
		// endpoints that declare vov.NoAuth(), where there is no user to read.
		AfterAuthMiddleware: []vov.Middleware{auditLog},
		// How this app resolves the user of a request. Endpoints require one
		// unless they declare vov.NoAuth().
		Authenticator: authenticate,
		Handlers: []vov.Endpoint{
			// Open and bare: no auth, no middleware. A health check should not
			// depend on either.
			{Method: http.MethodGet, Path: "/healthz", Handler: healthz,
				MiddlewareMod: vov.NoMiddleware(), AuthMod: vov.NoAuth()},

			// Nothing declared, so: default middleware stack, auth required.
			// The majority case says nothing and is protected anyway.
			{Method: http.MethodGet, Path: "/tasks", Handler: store.list},
			{Method: http.MethodGet, Path: "/tasks/{id}", Handler: store.get},

			// Extend: the default stack, plus one more layer inside it. Still
			// authenticated, because nothing here says otherwise.
			{Method: http.MethodPost, Path: "/tasks", Handler: store.create,
				MiddlewareMod: vov.ExtendMiddleware(requireJSON)},

			// A webhook: authenticated by signature rather than by user, so it
			// opts out of auth and replaces the middleware stack entirely.
			{Method: http.MethodPost, Path: "/webhook", Handler: webhook,
				MiddlewareMod: vov.OverrideMiddleware(verifySignature), AuthMod: vov.NoAuth()},
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

// --- auth -------------------------------------------------------------------

// user is this app's own model. It satisfies vov.User by answering the one
// question vov asks; everything else about it stays the app's business.
type user struct {
	name string
}

func (u *user) IsAuthenticated() bool { return u != nil && u.name != "" }

// authenticate is the app's vov.Authenticator: it owns the credential lookup,
// which in a real service would hit a session table or verify a token.
func authenticate(r *http.Request) (vov.User, error) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch token {
	case "":
		return nil, nil // no credentials presented -> 401
	case "t-boom":
		return nil, errors.New("session store unavailable") // -> 500, not 401
	case "t-ramtin":
		return &user{name: "ramtin"}, nil
	default:
		return nil, nil
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
	items  map[int]task
}

func newTaskStore() *taskStore {
	return &taskStore{items: make(map[int]task)}
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

func healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
