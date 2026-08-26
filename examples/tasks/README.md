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
- **Auth on by default** — `AppConfig.Authenticator` resolves the user; every
  endpoint requires one unless it declares `vov.NoAuth()`. `/tasks` says nothing
  about auth and is protected anyway; `/healthz` and `/webhook` opt out.
- **Two middleware phases around the auth seam** — `Middleware` runs outside the
  guard (so a 401 is still logged and still carries a request id);
  `AfterAuthMiddleware` runs inside it, where `vov.UserFrom` works — that is how
  `auditLog` knows who made the request. A full chain:

  ```
  requestID → logging → [auth guard] → auditLog → requireJSON → handler
  ```

  The guard is a seam between the phases, not a member of either, so no
  `MiddlewareMod` can remove it — only `AuthMod` can.

  A missing credential is a 401, but a *failing* authenticator is a 500 — a
  broken session store is not a bad password. Try `Authorization: Bearer t-boom`.
- **The escape hatch** — `GET /version` is registered straight on the underlying
  mux via `app.Mux()`, bypassing vov entirely: no middleware, no auth. Reaching
  for the mux means taking full control.
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

The demo token is `t-ramtin`:

```bash
curl -s localhost:8080/tasks                             # 401: protected by default
curl -s localhost:8080/tasks -H 'Authorization: Bearer t-ramtin'
curl -s localhost:8080/tasks -H 'Authorization: Bearer t-boom'   # 500, not 401
curl -s -X POST localhost:8080/tasks -H 'Authorization: Bearer t-ramtin' \
  -H 'Content-Type: application/json' -d '{"title":"write the tests"}'   # owner is set from the context
curl -s -X POST localhost:8080/tasks -H 'Authorization: Bearer t-ramtin' \
  -H 'Content-Type: text/plain' -d 'nope'                # 415, from the added layer

curl -si localhost:8080/healthz | grep -i x-request-id   # bare: no id, no auth
curl -s -X POST localhost:8080/webhook                   # 401, from the overridden stack
curl -s -X POST localhost:8080/webhook -H 'X-Signature: sig'
curl -s localhost:8080/version                           # via the mux escape hatch
```

Press Ctrl-C to trigger graceful shutdown.
