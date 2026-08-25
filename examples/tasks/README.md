# tasks — a vov example

A tiny in-memory task list that shows how a project consumes `vov`. It is its own
Go module and imports `github.com/vieolo/vov` across a module boundary, exactly
as an external project would (the local source is wired in with a `replace`
directive in `go.mod`).

## What it demonstrates

- **Declarative endpoints** — method, path, handler, and middleware are given as
  data in `vov.AppConfig.Handlers`, not assembled imperatively.
- **A default middleware stack** — `AppConfig.Middleware` (`requestID`, `logging`)
  applies to every endpoint, and each route says how it relates to it:

  | Route | Declaration | Effective stack |
  |---|---|---|
  | `GET /tasks` | *(unset)* | `requestID`, `logging` |
  | `POST /tasks` | `vov.ExtendMiddleware(requireJSON)` | `requestID`, `logging`, `requireJSON` |
  | `POST /webhook` | `vov.OverrideMiddleware(verifySignature)` | `verifySignature` |
  | `GET /healthz` | `vov.NoMiddleware()` | *(none)* |

  The default stack stamps `X-Request-Id`, so which routes it reached is visible
  in the response headers.
- **The escape hatch** — `GET /version` is registered straight on the underlying
  mux via `app.Mux()`, bypassing vov (and so getting no middleware at all).
- **Server tuning** — a `vov.Server` (the `http.Server` knobs minus `Addr`/`Handler`)
  is supplied through `AppConfig.Server`; defaulted timeouts are pointers, set inline
  with `vov.Ptr`.
- **Lifecycle** — `app.Run()` serves until SIGINT/SIGTERM, then drains and runs
  the `OnShutdown` cleanup hook.

## Run it

```bash
go run .
```

Then, in another terminal:

```bash
curl -si localhost:8080/healthz | grep -i x-request-id   # bare: no id
curl -si localhost:8080/tasks | grep -i x-request-id     # inherited: id present
curl -s -X POST localhost:8080/tasks -H 'Content-Type: application/json' -d '{"title":"write the tests"}'
curl -s -X POST localhost:8080/tasks -H 'Content-Type: text/plain' -d 'nope'   # 415, added layer
curl -s -X POST localhost:8080/webhook                   # 401, overridden stack
curl -s -X POST localhost:8080/webhook -H 'X-Signature: sig'
curl -s localhost:8080/version                           # via the mux escape hatch
```

Press Ctrl-C to trigger graceful shutdown.
