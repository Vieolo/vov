package main

// The task resource: the URLs it serves, its handlers, and its storage.
//
// Handlers are plain functions with the standard net/http signature. They reach
// the objects built at boot through the deps holder, so nothing in this file has
// to be wired to anything else. The vov.Endpoints groups at the top keep a URL's
// method list beside the code that implements it.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"

	"github.com/vieolo/vov"
)

// The prose an assistant reads. It lives beside the endpoints rather than inside
// the literals, because a 200-character string in a route table reads badly —
// which is a formatting problem with a formatting answer, not a reason to declare
// the route somewhere else.
const (
	listTasksDoc = "Every task on the list, with its id, title and owner. " +
		"Start here: the ids returned are what get_task and delete_task take."
	getTaskDoc = "One task in full, by id. Prefer this over scanning list_tasks " +
		"when you already know which task you mean."
	createTaskDoc = "Add a task to the list. The title is what a person will read, " +
		"so write it as an instruction rather than a summary."
	deleteTaskDoc = "Remove a task permanently. Only an admin or an owner may do " +
		"this, and it cannot be undone — confirm before calling it."
)

// collectionEndpoints serves /tasks.
func collectionEndpoints() vov.Endpoints {
	return vov.Endpoints{
		// Nothing declared, so: default stack, auth required. The majority case
		// says nothing and is protected anyway.
		GET: vov.Endpoint{
			Handler: listTasks,
			Query:   vov.QueryOf[listTasksQuery](),
			// One line makes this endpoint callable by an assistant. Its method,
			// path, arguments and policy are the ones declared right here.
			MCPTool: &vov.MCPTool{Name: "list_tasks", Description: listTasksDoc},
		},
		// Reading is open to any authenticated user; writing takes a permission.
		// Same URL, different wrapping and different authority — per method.
		POST: vov.Endpoint{
			Handler:          createTask,
			MiddlewareStack:  "json",
			PermissionsAllOf: []string{"tasks.write"},
			// The same type createTask decodes into — and the same schema the
			// assistant is given for its arguments.
			Body:    vov.BodyOf[createTaskInput](),
			MCPTool: &vov.MCPTool{Name: "create_task", Description: createTaskDoc},
		},
	}
}

// taskIDParam documents the {id} wildcard once, for both methods of the URL.
//
// The alias is the point. `{id}` is unambiguous in /tasks/{id}, where the path
// supplies the noun; offered to a model choosing between tools that each take
// the id of a different thing, it is not. The description says where the value
// comes from, which is what turns two tools into a chain the assistant can
// follow without asking the user to paste an id.
var taskIDParam = map[string]vov.PathParam{
	"id": {
		Name:        "taskId",
		Description: "the task's id, as returned by list_tasks",
	},
}

// itemEndpoints serves /tasks/{id}.
func itemEndpoints() vov.Endpoints {
	return vov.Endpoints{
		GET: vov.Endpoint{
			Handler:    getTask,
			PathParams: taskIDParam,
			MCPTool:    &vov.MCPTool{Name: "get_task", Description: getTaskDoc},
		},
		// Deleting needs both: one of the listed roles (any-of) and every listed
		// permission (all-of). Reading the same URL needs neither — which is the
		// point of configuring auth per method rather than per URL.
		DELETE: vov.Endpoint{
			Handler:          deleteTask,
			PathParams:       taskIDParam,
			RolesAnyOf:       []string{"admin", "owner"},
			PermissionsAllOf: []string{"tasks.write"},
			// The tool inherits the role and permission above: an assistant
			// acting for a member is refused exactly as a browser would be.
			MCPTool: &vov.MCPTool{Name: "delete_task", Description: deleteTaskDoc},
		},
	}
}

// --- handlers ---------------------------------------------------------------

// listTasksQuery declares what GET /tasks accepts in its query string.
type listTasksQuery struct {
	Owner string `json:"owner" jsonschema:"filter to one owner, matched exactly against the name a task was created under"`
	Limit int    `json:"limit" jsonschema:"how many tasks to return; omit for all of them"`
	// A list of scalars becomes a repeated query parameter — ?tag=a&tag=b — and
	// an array argument in the tool schema. QueryOf permits it and dispatch
	// honours it; a list of objects is refused by both.
	Tag []string `json:"tag" jsonschema:"repeat to narrow to tasks carrying every listed tag"`
}

func listTasks(w http.ResponseWriter, r *http.Request) {
	d := deps.Get()
	out := d.Store.all()
	d.Log.Debug("listed tasks", "count", len(out))
	writeJSON(w, http.StatusOK, out)
}

// createTaskInput is both what createTask decodes into and what the endpoint
// declares as its body, so the contract and the code that reads it are the same
// type and cannot drift.
//
// Note the two optional fields. Tags is a plain slice: absent and empty are the
// same thing for it. Notes is a pointer, so the handler can tell an absent field
// from an explicit null — the distinction a PATCH needs, and the reason vov
// describes the shape rather than decoding it for you.
// The two tags do different jobs and are deliberately separate: vov's own is a
// list of options, and a description is free text that would need escaping to
// live in one.
type createTaskInput struct {
	Title string   `json:"title" vov:"required" jsonschema:"a short imperative summary, e.g. \"write the tests\""`
	Tags  []string `json:"tags" jsonschema:"free-form labels; reuse ones already on other tasks where they fit"`
	Notes *string  `json:"notes" jsonschema:"longer context, if the title alone would not be enough later"`
}

func createTask(w http.ResponseWriter, r *http.Request) {
	d := deps.Get()
	var in createTaskInput
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
	t, err := d.Store.add(in.Title, currentUser(r).name)
	if err != nil {
		writeJSON(w, http.StatusInsufficientStorage, map[string]string{"error": err.Error()})
		return
	}

	// A second shared dependency, used from the same handler.
	if _, err := d.S3.PutObject(fmt.Sprintf("tasks/%d.json", t.ID), []byte(t.Title)); err != nil {
		d.Log.Error("archive failed", "id", t.ID, "err", err)
	}

	writeJSON(w, http.StatusCreated, t)
}

func getTask(w http.ResponseWriter, r *http.Request) {
	d := deps.Get()
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	t, found := d.Store.get(id)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func deleteTask(w http.ResponseWriter, r *http.Request) {
	d := deps.Get()
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	if !d.Store.remove(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	d.Log.Info("task deleted", "id", id, "by", currentUser(r).name)
	w.WriteHeader(http.StatusNoContent)
}

// taskID reads the {id} path value, answering 400 and reporting false when it is
// not a number.
func taskID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return 0, false
	}
	return id, true
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

func (s *taskStore) all() []task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]task, 0, len(s.items))
	for _, t := range s.items {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *taskStore) add(title, owner string) (task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) >= s.max {
		return task{}, fmt.Errorf("task limit reached")
	}
	s.nextID++
	t := task{ID: s.nextID, Title: title, Owner: owner}
	s.items[t.ID] = t
	return t, nil
}

func (s *taskStore) get(id int) (task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.items[id]
	return t, ok
}

func (s *taskStore) remove(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return false
	}
	delete(s.items, id)
	return true
}
