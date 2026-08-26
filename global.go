package vov

import (
	"fmt"
	"sync"
)

// Global is a typed, write-once holder for an object an application builds at
// boot and needs from many handlers: a struct of a database handle, a logger,
// service clients.
//
// The name is literal. This is process-wide state, with the coupling and the
// test-substitution hazards that implies, and it says so rather than dressing
// itself up as dependency injection — vov injects nothing, constructs nothing,
// and never looks inside what it holds.
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
//	var deps = vov.NewGlobal[*Deps]()
//
//	func main() {
//	    deps.Set(&Deps{DB: db, Log: log})
//	    app, err := vov.NewApp(vov.AppConfig{RequireGlobals: []vov.Readiness{deps}, ...})
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
// A Global is safe for concurrent use. Handlers may call Get on every request;
// it takes a read lock and returns the stored value.
type Global[T any] struct {
	mu  sync.RWMutex
	val T
	set bool
}

// NewGlobal returns an empty holder. Populate it with [Global.Set] once the
// value exists — typically after configuration has been loaded and before
// [NewApp] is called.
func NewGlobal[T any]() *Global[T] {
	return &Global[T]{}
}

// Set stores v. It panics if called twice: a global swapped while requests are
// in flight would let two requests see different worlds, and every legitimate
// use sets it exactly once during start-up.
func (g *Global[T]) Set(v T) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.set {
		panic("vov: Global.Set called twice — a global is set once at start-up")
	}
	g.val, g.set = v, true
}

// Get returns the stored value. It panics if Set has not been called, rather
// than handing back a zero value that would fail later as a nil dereference
// somewhere less obvious.
//
// Pass the holder to [AppConfig.RequireGlobals] to turn that panic into a
// construction error instead, caught before the server ever listens.
func (g *Global[T]) Get() T {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.set {
		panic(fmt.Sprintf("vov: global of type %T was never Set", g.val))
	}
	return g.val
}

// Ready reports whether Set has been called. It satisfies [Readiness].
func (g *Global[T]) Ready() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.set
}

// Describe names the held type, for error messages. It satisfies [Readiness].
func (g *Global[T]) Describe() string {
	return fmt.Sprintf("%T", g.val)
}

// Readiness is anything that can report whether it has been populated. A
// *[Global] satisfies it, which is what lets [AppConfig.RequireGlobals] check
// holders without knowing the type inside them — the reason none of vov's other
// types need a type parameter.
type Readiness interface {
	Ready() bool
	Describe() string
}
