# tasks — a vov example

A tiny in-memory task list that shows how a project consumes `vov`. It is its own
Go module and imports `github.com/vieolo/vov` across a module boundary, exactly
as an external project would (the local source is wired in with a `replace`
directive in `go.mod`).

## What it demonstrates

- **Declarative endpoints** — method, path, handler, and middleware are given as
  data in `vov.AppConfig.Handlers`, not assembled imperatively.
- **Middleware as data** — the `logging` middleware is attached per endpoint.
- **The escape hatch** — `GET /version` is registered straight on the underlying
  mux via `app.Mux()`, bypassing vov.
- **Lifecycle** — `app.Run()` serves until SIGINT/SIGTERM, then drains and runs
  the `OnShutdown` cleanup hook.

## Run it

```bash
go run .
```

Then, in another terminal:

```bash
curl -s localhost:8080/healthz
curl -s -X POST localhost:8080/tasks -d '{"title":"write the tests"}'
curl -s localhost:8080/tasks
curl -s localhost:8080/tasks/1
curl -s localhost:8080/version   # served via the mux escape hatch
```

Press Ctrl-C to trigger graceful shutdown.
