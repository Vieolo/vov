package main

// The task resource: its storage, its handlers, and — at the top — the URLs it
// serves. Keeping the vov.Endpoints groups here means a URL's method list lives
// beside the code that implements it, so adding a method is one edit in one
// file. main.go only has to say which path each group is mounted on.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"

	"github.com/vieolo/vov"
)

// collectionEndpoints serves /tasks.
func (s *taskStore) collectionEndpoints() vov.Endpoints {
	return vov.Endpoints{
		// Nothing declared, so: default stack, auth required. The majority case
		// says nothing and is protected anyway.
		GET: vov.Endpoint{Handler: s.list},
		// Same URL, same auth, different wrapping — one word says which.
		POST: vov.Endpoint{Handler: s.create, MiddlewareStack: "json"},
	}
}

// itemEndpoints serves /tasks/{id}.
func (s *taskStore) itemEndpoints() vov.Endpoints {
	return vov.Endpoints{
		GET:    vov.Endpoint{Handler: s.get},
		DELETE: vov.Endpoint{Handler: s.delete},
	}
}

// --- storage ----------------------------------------------------------------

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

// --- handlers ---------------------------------------------------------------

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
