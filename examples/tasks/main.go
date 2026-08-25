// Command tasks is a small example service built with the vov framework. It
// exists to exercise vov's ergonomics from the outside: declarative endpoints, a
// default middleware stack that endpoints inherit, extend, override, or drop, the
// mux escape hatch, a cleanup hook, and a lifecycle-managed Run. It keeps its data
// in memory and depends only on the standard library plus vov.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
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
		Middleware: []vov.Middleware{requestID, logging},
		Handlers: []vov.Endpoint{
			// Bare: no default stack, no additions. A health check should not
			// depend on any of it.
			{Method: http.MethodGet, Path: "/healthz", Handler: healthz,
				Middleware: vov.NoMiddleware()},

			// Inherit the default stack — the common case, so the field is unset.
			{Method: http.MethodGet, Path: "/tasks", Handler: store.list},
			{Method: http.MethodGet, Path: "/tasks/{id}", Handler: store.get},

			// Extend: the default stack, plus one more layer inside it.
			{Method: http.MethodPost, Path: "/tasks", Handler: store.create,
				Middleware: vov.ExtendMiddleware(requireJSON)},

			// Override: a completely different stack. This route is authenticated
			// by signature, not by the session the default stack would assume.
			{Method: http.MethodPost, Path: "/webhook", Handler: webhook,
				Middleware: vov.OverrideMiddleware(verifySignature)},
		},
	})
	if err != nil {
		log.Fatalf("tasks: %v", err)
	}

	// Escape hatch: register straight on the underlying mux, bypassing vov.
	// Useful for anything vov does not model yet. Note that these routes get no
	// middleware from the framework — not even the default stack.
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

// --- domain -----------------------------------------------------------------

type task struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
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

	s.mu.Lock()
	s.nextID++
	t := task{ID: s.nextID, Title: in.Title}
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

// logging records method, path, and latency. Part of the default stack.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start))
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
