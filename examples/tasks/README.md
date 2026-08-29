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

  The authenticator also receives a `vov.AuthResponse`, which can set response
  headers but not write a response. `t-ramtin-revoked` shows why: the account is
  gone but its signed cookie stays valid for 30 days, so the request is refused
  **and** the cookie is cleared on the way out. The same call on the success
  branch is how you rotate a session for sliding expiry.
- **One endpoint, two renderings** — `listTasks` writes the full record for a
  browser and a compact row for an assistant, keyed on `vov.ModeFrom`. This is
  the permitted side of `RequestMode`'s line and the example exists to show
  where that line is: what varies is how much of each row is written, not which
  rows are returned or who may see them. Filtering rows by channel would be a
  policy no reviewer can see, and belongs in the declaration instead.

- **A seam around the whole handler** — `APIConfig.ServerWrappers` wrap the
  assembled mux, so it also sees the requests the mux answers *itself*:

  | Request | Without the seam | With it |
  |---|---|---|
  | `OPTIONS /tasks/{id}` (CORS preflight) | 405, browser blocks the real call | 204 + CORS headers |
  | `GET /nope` | 404 with no CORS headers | 404 the browser can actually read |
  | panic on any path | dropped connection | 500 + a log line |

  `http.ServeMux` decides 404 and 405 before dispatching, so no endpoint
  middleware ever runs for them — which is why this cannot be a `MiddlewareStack`.
  Recovery is listed first so it wraps CORS too.
- **The escape hatch** — `GET /version` is registered straight on the underlying
  mux via `app.Mux()`, bypassing vov's endpoint management: no stack, no auth.
  `GET /boom` is registered the same way and panics, showing that
  `ServerWrappers` still cover it — unavoidably, since the mux's own refusals
  can only be seen from outside everything it serves.
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

  `go -C agent run . manifest` fails if the code and the file disagree. That is
  the whole point: make `GET /tasks/{id}` public and `verify` and `smoke` both
  stay green, because no test asserts a property nobody thought to assert — but
  the manifest shows a changed line.

- **The same endpoints as MCP tools** — an endpoint becomes callable by an AI
  assistant by carrying an `MCPTool`, right where it is already declared:

  ```go
  DELETE: vov.Endpoint{
      Handler:          deleteTask,
      RolesAnyOf:       []string{"admin", "owner"},
      PermissionsAllOf: []string{"tasks.write"},
      MCPTool:          &vov.MCPTool{Name: "delete_task", Description: deleteTaskDoc},
  },
  ```

  Nothing is restated: the method, path, arguments and policy are the ones on
  that line. `AppConfig.MCP` supplies the app-level half — including where to
  serve it — and there is nothing to do after `NewApp`:

  ```go
  MCP: &vov.MCPConfig{
      Name: "tasks", Version: "0.1.0",
      Instructions: "…",
      Authenticate: makeAuthenticator(cfg.Token),
      Path: "/mcp",
  },
  ```

  ```bash
  curl -s localhost:8080/mcp -H 'Authorization: Bearer t-ramtin' \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
  ```

  `delete_task` refuses a caller without the role, and `get_reports` tells an
  unpaid caller to subscribe — both from the endpoint declaration. The manifest
  shows which endpoints are exposed, since a second audience for the same policy
  is worth a reviewer seeing.

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
#   t-ramtin-revoked    a dead credential: 401, and the cookie is cleared
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
