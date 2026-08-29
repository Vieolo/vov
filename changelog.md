# Changelog

## v0.5.0 (2026-08-29)

#### Breaking changes
- `User.HasRole` and `User.HasPermission` are replaced by `User.Roles()` and `User.Permissions()`. `vov` now fetches each set once and does the matching itself, so an endpoint listing three permissions costs one lookup instead of three. Return the effective set, expanding whatever your model implies
- `Endpoint.ScopeAllOf` is now a `map[RequestMode][]string` and `ScopeRule` is gone. A channel you name overrides the app-level scope; one you leave out inherits it; an empty list requires none. `ScopeNone()` returns the map that requires none anywhere

## v0.4.1 (2026-08-29)
- Fixed `ToolCall.Arguments` arriving empty. It is now copied before routing, which also keeps a rejected call's record whole
- A tool argument the endpoint does not declare is now refused, naming it and listing what the tool does accept, instead of being marshalled into the request body

## v0.4.0 (2026-08-29)
- Query fields declared as a list of scalars are now dispatched as repeated parameters (`?tag=a&tag=b`). `QueryOf` always accepted them and the dispatcher rejected them, so a declaration `vov` validated at construction could still fail at call time
- `BodyOf` now advises declaring the type describing the contract, which may be narrower than the one the handler decodes into. Declaring a domain model offers an assistant every field on it, including the ones the server owns

#### Breaking changes
- `OnToolCall` now receives the call's `context.Context`, with the cancellation stripped, so a sink can reach a trace id without losing a row when the client disconnects

## v0.3.1 (2026-08-29)
- Body and query fields can now carry a description
- Path parameters gained `Endpoint.PathParams`, giving a route wildcard a caller-facing name and a description
- Documenting a wildcard the route does not declare, or giving two wildcards the same caller-facing name, is now a construction error
- The route manifest gained a `param:` column

## v0.3.0 (2026-08-29)
- Introduced the scopes, allowing a user having different types of authentications, each with a different scope. You can define the scope as a general rule in the app level with the ability to overwrite them on the endpoint level
- Pre-authentication middlewares now have access to the headers
- The `Invoke` function is no longer available. This function was never meant to be public and is considered a bug fix
- Added `OnToolCall` hook for the MCP, which observes every tool call after it has finished, including the ones rejected before they reached an endpoint, which no middleware can see. It records the outcome, status and duration, cannot fail the call, and recovers a panicking tool handler (which `ServerWrappers` cannot: the protocol SDK dispatches each call on its own goroutine)

#### Breaking changes
- `RequestMode` values are renamed to `RequestModeAPI` and `RequestModeMCP` to better reflect the source
- Removed the `BuildHandler` from the app-level MCP config
- `ServerWrappers` and root-level `Authenticator` are moved to the `API` field instead of being on the root `AppConfig` to clear any confusion about their role

## v0.2.1 (2026-08-28)
- Added support for built-in MCP, re-using the API endpoints as MCP tools with automated redirection
- The request's context now have a `RequestMode`, allowing you to distinguish between the network (REST) vs. invoked (MCP) calls
- Endpoints now support basic request body/query schema definitions. These definitions are currently used for MCP and are not enforced
- The route manifest gained `query:`, `body:` and `mcp-tool:` columns. Which endpoints an AI assistant can reach is a second audience for the same policy, and worth a reviewer seeing.

## v0.2.0 (2026-08-26)
- Added `ServerWrappers` which are middlewares that wrap the mux directly, applying on all handlers AND those requests return 404 and 405. These wrappers are specifically used to handle CORS preflight and/or panic recovery.

#### Breaking changes
- Added `Tier` function to the User interface
- Changed the signature of the authentication middleware to have access to the cookies and headers

## v0.1.0 (2026-08-26)
- initial release