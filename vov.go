// Package vov is a declarative, batteries-included Go backend framework.
//
// The endpoint declaration is the single source of truth. An [Endpoint] carries
// its method, path, handler, and middleware as plain data, and every other part
// of the framework — routing today, and policy, the route manifest, and
// generated tests later — is a consumer of that declaration rather than a
// parallel structure that can drift from it.
//
// An [App] assembles a set of endpoints onto a standard *http.ServeMux, exposes
// that mux for raw registration, and owns the server lifecycle (signal handling,
// graceful shutdown, cleanup hooks) so it does not have to be rewritten for every
// service. The App is only an assembler over the declarations; the declarations
// remain usable without it.
package vov

// Metadata describes the project an [App] serves. It holds only non-secret,
// descriptive facts and is expected to grow (build info, environment, and so on).
type Metadata struct {
	Name        string
	Description string
	Version     string
}
