package vov

import (
	"fmt"
	"sync"
)

// Dependencies is a typed, write-once holder for the objects an application
// builds at boot and needs from many handlers: a database handle, a logger,
// service clients.
//
// It exists so that handlers can reach those objects while keeping the standard
// net/http signature. The alternatives both cost more than they give: threading
// a dependency type through the framework makes every declaration mention it and
// forces one shared type on every handler, and stashing values in the request
// context trades a compile error for a runtime assertion.
//
// Declare one holder per application, next to the type it holds:
//
//	type Deps struct{ DB *sql.DB; Log *slog.Logger }
//
//	var deps = vov.NewDependencies[*Deps]()
//
//	func main() {
//	    deps.Set(&Deps{DB: db, Log: log})
//	    app, err := vov.NewApp(vov.AppConfig{RequireDeps: []vov.Readiness{deps}, ...})
//	}
//
//	func listTasks(w http.ResponseWriter, r *http.Request) {
//	    d := deps.Get()
//	    d.Log.Debug("listing")
//	}
//
// Get is typed, so nothing is asserted at the call site and a wrong field is a
// compile error. What it does not do — and cannot — is put a handler's
// dependencies in its signature: reaching for a holder is invisible to a reader
// of the declaration. That is the price of the standard handler shape.
//
// A holder is safe for concurrent use. Handlers may call Get on every request;
// it takes a read lock and returns the stored value.
type Dependencies[T any] struct {
	mu  sync.RWMutex
	val T
	set bool
}

// NewDependencies returns an empty holder. Populate it with [Dependencies.Set]
// once the values exist — typically after configuration has been loaded and
// before [NewApp] is called.
func NewDependencies[T any]() *Dependencies[T] {
	return &Dependencies[T]{}
}

// Set stores v. It panics if called twice: a dependency holder swapped while
// requests are in flight would let two requests see different worlds, and every
// legitimate use sets it exactly once during start-up.
func (d *Dependencies[T]) Set(v T) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.set {
		panic("vov: Dependencies.Set called twice — dependencies are set once at start-up")
	}
	d.val, d.set = v, true
}

// Get returns the stored value. It panics if Set has not been called, rather
// than handing back a zero value that would fail later as a nil dereference
// somewhere less obvious.
//
// Pass the holder to [AppConfig.RequireDeps] to turn that panic into a
// construction error instead, caught before the server ever listens.
func (d *Dependencies[T]) Get() T {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.set {
		panic(fmt.Sprintf("vov: dependencies of type %T were never Set", d.val))
	}
	return d.val
}

// Ready reports whether Set has been called. It satisfies [Readiness].
func (d *Dependencies[T]) Ready() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.set
}

// Describe names the held type, for error messages. It satisfies [Readiness].
func (d *Dependencies[T]) Describe() string {
	return fmt.Sprintf("%T", d.val)
}

// Readiness is anything that can report whether it has been populated. A
// *[Dependencies] satisfies it, which is what lets [AppConfig.RequireDeps] check
// holders without knowing the type inside them — the reason none of vov's other
// types need a type parameter.
type Readiness interface {
	Ready() bool
	Describe() string
}
