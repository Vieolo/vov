# tasks — a vov example

A tiny in-memory task list that shows how a project consumes `vov`. It is its own
Go module and imports `github.com/vieolo/vov` across a module boundary, exactly
as an external project would (the local source is wired in with a `replace`
directive in `go.mod`).

## What it demonstrates

- **Startup environment** — the `config` struct is both the declaration of what
  the service reads and the way it reads it, so the two cannot drift:

  ```go
  type config struct {
      Token string        `env:"TASKS_TOKEN,required"`
      Addr  string        `env:"TASKS_ADDR" envDefault:":8080"`
      Idle  time.Duration `env:"TASKS_IDLE_TIMEOUT" envDefault:"90s"`
  }
  vov.LoadEnv(&cfg)
  ```

  `LoadEnv` runs *before* `NewApp`, because everything below is built from it —
  the store's limit, the authenticator's token, the listen address. Every problem
  is reported at once, and errors name variables without ever printing a value.
- **One `Route` per URL** — every method a URL answers is declared together, and
  each method carries its own configuration:

  ```go
  {
      Path:   "/tasks/{id}",
      GET:    vov.Endpoint{Handler: store.get},
      DELETE: vov.Endpoint{Handler: store.delete},
  }
  ```

  Opening a `Route` shows that URL's whole surface. Declaring the same path in
  two `Route`s is a construction error, so a URL's methods cannot drift apart —
  and vov knows which methods exist, so `PUT /tasks/1` gets a 405 with
  `Allow: DELETE, GET, HEAD`.
- **Declarative endpoints** — handler, middleware stack, and auth are given as
  data on each method's `vov.Endpoint`, not assembled imperatively.
- **Named middleware stacks** — `AppConfig.Stacks` declares each combination once;
  an endpoint picks one by name, and saying nothing picks `"default"`:

  | Endpoint | `MiddlewareStack` | `Pre` (outside the guard) | `Post` (inside it) |
  |---|---|---|---|
  | `GET /tasks` | *(unset → default)* | `requestID`, `logging` | `auditLog` |
  | `POST /tasks` | `"json"` | `requestID`, `logging` | `auditLog`, `requireJSON` |
  | `POST /webhook` | `"webhook"` | `requestID`, `logging`, `verifySignature` | — |
  | `GET /healthz` | `"bare"` | — | — |

  Naming a stack that was never declared is a construction error, not a route
  that quietly ends up wrapped in nothing.
- **Auth on by default, authority per method** — `AppConfig.Authenticator`
  resolves the user; every endpoint requires one unless it declares
  `vov.NoAuth()`. `/tasks` says nothing about auth and is protected anyway;
  `/healthz` and `/webhook` opt out. On top of that, an endpoint can demand more:

  | Endpoint | Declaration | Who gets through |
  |---|---|---|
  | `GET /tasks/{id}` | *(unset)* | any authenticated user |
  | `POST /tasks` | `Permissions: []string{"tasks.write"}` | holds **every** listed permission |
  | `DELETE /tasks/{id}` | `RolesAnyOf: []string{"admin","owner"}`<br>`PermissionsAllOf: []string{"tasks.write"}` | holds **any** listed role **and every** listed permission |
  | `GET /reports` | `RolesAnyOf: []string{"member"}`<br>`MinTier: 2` | holds the role **and** has paid to tier 2+ |

  `Roles` is any-of because a role is an identity — any of the listed ones will
  do. `Permissions` is all-of because a permission is a capability and each one
  is needed. Declaring both means a user must satisfy both.

  Note that `GET` and `DELETE` on `/tasks/{id}` differ — reading needs neither,
  deleting needs both — which is why auth is configured per method, not per URL.

  The refusals mean different things: **401** is "vov does not know who you
  are", **403** is "it does, and the answer is no", **402** is "it does, and the
  answer is yes as soon as you pay". 403 never says which requirement failed;
  402 is deliberately distinguishable so a frontend can show an upgrade panel
  instead of a generic error.

  The order is load-bearing — roles and permissions are checked before tier, so
  402 only ever means payment is the *last* barrier. `/reports` shows all of it:
  the `owner` user lacks the `member` role and gets 403 even though they also
  have not paid, because paying would not let them in.
- **The auth seam splits every stack** — `Pre` runs outside the guard (so a 401
  is still logged and still carries a request id); `Post` runs inside it, where
  `vov.UserFrom` works — that is how `auditLog` knows who made the request:

  ```
  requestID → logging → [auth guard] → auditLog → requireJSON → handler
  ```

  The guard is the seam between the halves, not a member of either, so choosing a
  different stack can never switch authentication off — only `AuthMod` can. On a
  `NoAuth` endpoint there is no user, so `Post` is skipped: anything such a route
  needs goes in `Pre`, which is why `verifySignature` lives there.

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

- **A checked-in route manifest** — [`routes.txt`](routes.txt) is `app.Manifest()`
  rendered to a file, regenerated with `go run . -manifest`:

  ```
  GET     /tasks/{id}    auth:required  stack:default
  DELETE  /tasks/{id}    auth:required  stack:default  roles-any:admin|owner  perms-all:tasks.write
  ```

  `python3 agent.py manifest` fails if the code and the file disagree. That is
  the whole point: make `GET /tasks/{id}` public and `verify` and `smoke` both
  stay green, because no test asserts a property nobody thought to assert — but
  the manifest shows a changed line.

## Run it

`TASKS_TOKEN` is required — without it the server refuses to start and says so:

```bash
go run .
```

```bash
TASKS_TOKEN=t-ramtin go run .
```

Then, in another terminal (the demo token is whatever you set above):

```bash
curl -s localhost:8080/tasks                             # 401: protected by default
curl -s localhost:8080/tasks -H 'Authorization: Bearer t-ramtin'
curl -s localhost:8080/tasks -H 'Authorization: Bearer t-boom'   # 500, not 401

# Demo users, all suffixes of the configured token:
#   t-ramtin            member  + tasks.write
#   t-ramtin-admin      admin   + tasks.write
#   t-ramtin-owner      owner   + tasks.write   (the any-of role's second entry)
#   t-ramtin-halfadmin  admin   only            (role but no permission)
#   t-ramtin-pro        member  + tasks.write + paid tier 2
#   t-ramtin-reader     member  only
curl -s -X POST localhost:8080/tasks -H 'Authorization: Bearer t-ramtin-reader' \
  -H 'Content-Type: application/json' -d '{"title":"x"}'  # 403: lacks tasks.write
curl -s -X DELETE localhost:8080/tasks/1 -H 'Authorization: Bearer t-ramtin'            # 403: no role
curl -s -X DELETE localhost:8080/tasks/1 -H 'Authorization: Bearer t-ramtin-halfadmin'  # 403: no permission
curl -s -X DELETE localhost:8080/tasks/1 -H 'Authorization: Bearer t-ramtin-admin'      # 204

curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/reports -H 'Authorization: Bearer t-ramtin-owner'   # 403: wrong role
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/reports -H 'Authorization: Bearer t-ramtin-reader'  # 402: unpaid
curl -s localhost:8080/reports -H 'Authorization: Bearer t-ramtin-pro'                                      # 200
curl -s -X POST localhost:8080/tasks -H 'Authorization: Bearer t-ramtin' \
  -H 'Content-Type: application/json' -d '{"title":"write the tests"}'   # owner is set from the context
curl -s -X POST localhost:8080/tasks -H 'Authorization: Bearer t-ramtin' \
  -H 'Content-Type: text/plain' -d 'nope'                # 415, from the "json" stack

curl -si localhost:8080/healthz | grep -i x-request-id   # "bare": no id, no auth
curl -s -X POST localhost:8080/webhook                   # 401, from the "webhook" stack
curl -s -X POST localhost:8080/webhook -H 'X-Signature: sig'
curl -s localhost:8080/version                           # via the mux escape hatch
```

Press Ctrl-C to trigger graceful shutdown.
