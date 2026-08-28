# vov

A declarative, batteries-included Go backend framework, using standard `net/http` under the hood. `vov` holds all the configuration of the server and the handlers via declarations instead of closures.

## Features
- The App object, holding all the config of the application
    - Named middleware stack, reusable on the endpoint level


## Example 

```go
app, err := vov.NewApp(vov.AppConfig{
    Address: ":8080",

    // Everything belonging to the HTTP channel. ServerWrappers cover every
    // request the server receives, including the 404s and 405s the mux answers
    // itself — and the POST carrying a tool call, but not the call inside it.
    API: vov.APIConfig{
        Authenticator:  authenticate,
        ServerWrappers: []vov.Middleware{recoverPanic(log), cors(origin)},
    },

    // Named middleware combinations, split by the auth seam.
    MiddlewareStacks: map[string]vov.MiddlewareStack{
        vov.DefaultStackName: {
            Pre:  []vov.Middleware{requestID},
            Post: []vov.Middleware{auditLog},
        },
    },

    Routes: []vov.Route{{
        Path: "/investors",
        Endpoints: vov.Endpoints{
            // Says nothing about auth, and is protected anyway.
            GET:  vov.Endpoint{Handler: listInvestors, MinTier: 1},
            POST: vov.Endpoint{Handler: createInvestor, RolesAnyOf: []string{"admin"}},
        },
    }},
})
if err != nil {
    log.Fatal(err) // a bad declaration fails here, not in production
}
log.Fatal(app.Run()) // serves until SIGINT/SIGTERM, then drains
```

## Why declarations

A route's policy written as `mux.HandleFunc("GET /investors", needPaid(list))`
lives in a closure, and closures do not diff. Written as data it can be rendered:

```
GET     /investors     auth:required  stack:default  min-tier:1
POST    /investors     auth:required  stack:default  scopes-all:write@mcp  roles-any:admin
```

Check that file in and a loosened policy becomes a changed line in a pull
request. No test tier catches that on its own — generated tests assert against
the declaration, so they move with it and stay green.

## What's in the box

| | |
|---|---|
| **Routing** | One `Route` per URL, methods as named fields — `GET`, `POST`, … A URL declared twice is a construction error. |
| **Auth** | Authenticated by default; `AuthModeNone` opts out. `RolesAnyOf` (any-of), `PermissionsAllOf` (all-of), `MinTier` (paid, refuses **402** not 403). |
| **Scopes** | `ScopeAllOf` gates on what the *credential* was issued for, not what its owner may do — the OAuth axis. Declared per channel, so tokens can govern your MCP tools while browser sessions stay untouched, and the two can differ. Defaults key on method, so a new endpoint is governed the moment it exists. |
| **Middleware** | Named stacks per endpoint, split `Pre`/`Post` around the auth seam; `API.ServerWrappers` for every HTTP request the server receives. |
| **Config** | `LoadEnv` binds env vars onto your struct — required fields, defaults, every problem reported at once, and never a value in an error message. |
| **Shared objects** | `Global[T]`, a typed write-once holder, so handlers keep the plain `net/http` signature. |
| **Lifecycle** | `Run` handles binding, SIGINT/SIGTERM, draining, and `OnShutdown` hooks. |
| **Manifest** | `App.Manifest()` renders every endpoint and its policy for review. |

## Fails closed

A declaration that cannot mean what it says is rejected by `NewApp`, before the
server listens — a route requiring auth with no `Authenticator`, a middleware
stack that was never declared, a URL declared twice, roles on an open endpoint.
An endpoint that says nothing about auth requires it.

## Adopting it

`App.Mux()` returns the underlying `*http.ServeMux`, so declared routes and
plain `mux.HandleFunc` calls coexist. A live app can migrate one group at a time
and stop wherever it likes.

## Example

[`examples/tasks`](examples/tasks) is a working service exercising all of the
above from the outside, as its own module.

## Status

Pre-1.0, and the API still moves between minor versions. `MIT`.
