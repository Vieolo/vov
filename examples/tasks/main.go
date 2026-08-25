// Command tasks is a small example service built with the vov framework. It
// exists to exercise vov's ergonomics from the outside: declarative endpoints,
// middleware declared as data, the mux escape hatch, a cleanup hook, and a
// lifecycle-managed Run. It keeps its data in memory and depends only on the
// standard library plus vov.
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
		Metadata: vov.Metadata{
			Name:        "tasks",
			Description: "A tiny task list, built to demo vov.",
			Version:     "0.1.0",
		},
		Address: ":8080",
		Handlers: []vov.Endpoint{
			// Public, no middleware.
			{Method: http.MethodGet, Path: "/healthz", Handler: healthz},

			// The task API. Each carries the logging middleware as data.
			{Method: http.MethodGet, Path: "/tasks", Handler: store.list, Middleware: []vov.Middleware{logging}},
			{Method: http.MethodPost, Path: "/tasks", Handler: store.create, Middleware: []vov.Middleware{logging}},
			{Method: http.MethodGet, Path: "/tasks/{id}", Handler: store.get, Middleware: []vov.Middleware{logging}},
		},
	})
	if err != nil {
		log.Fatalf("tasks: %v", err)
	}

	// Escape hatch: register straight on the underlying mux, bypassing vov.
	// Useful for anything vov does not model yet.
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

// --- plumbing ---------------------------------------------------------------

func healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// logging is an example Middleware: it records method, path, and latency.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
