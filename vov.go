// Package vov is a declarative, batteries-included Go backend framework.
//
// The endpoint declaration is the single source of truth. A [Route] groups every
// method of one URL, and each [Endpoint] carries its handler alongside the facts
// that govern it — which middleware stack wraps it, whether it needs an
// authenticated user, which roles or permissions or paid tier that user must
// hold — all as plain data. Everything else is a consumer of that declaration
// rather than a parallel structure that can drift from it: routing today, and
// [Manifest], generated tests, and an OpenAPI or MCP surface after.
//
// The point of keeping those facts as data is that data can be diffed. A policy
// that is quietly loosened still passes every test, because the tests assert
// against the declaration that changed; a checked-in [Manifest] turns it into a
// modified line in a pull request, which is the only place a person sees it.
//
// An [App] assembles those declarations onto a standard http.ServeMux, wraps
// that mux in [APIConfig.ServerWrappers], and serves the result — see
// [App.Handler] for what is served and [App.Mux] for the escape hatch that lets
// routes vov does not model coexist with the ones it does. It owns the server
// lifecycle too: binding, SIGINT and SIGTERM, draining, and cleanup hooks.
//
// Two things sit deliberately outside the App, so that neither depends on it:
// [LoadEnv], which binds the process environment onto a config struct before
// anything is built, and [Global], a typed write-once holder for the objects an
// app builds at boot and reaches from many handlers.
//
// Requests pass through two distinct layers, and the difference is not
// cosmetic:
//
//	API.ServerWrappers  every HTTP request the server receives, including the 404s and
//	                 405s http.ServeMux answers itself — so CORS preflight,
//	                 panic recovery, and access logging belong here
//	MiddlewareStack  only requests that reach an endpoint, split by the auth
//	                 seam into Pre (outside the guard) and Post (inside it,
//	                 where the user is known)
//
// Every piece is meant to be droppable. Handlers keep the standard net/http
// signature, middleware keeps the standard wrapper shape, and the mux underneath
// is a plain *http.ServeMux — so a codebase can adopt vov one route at a time and
// stop wherever it likes.
package vov
