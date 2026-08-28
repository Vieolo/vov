# Changelog

## v0.3.0 (2026-08-29)
- Introduced the scopes, allowing a user having different types of authentications, each with a different scope. You can define the scope as a general rule in the app level with the ability to overwrite them on the endpoint level
- Pre-authentication middlewares now have access to the headers
- The `Invoke` function is no longer available. This function was never meant to be public and is considered a bug fix
- Added `OnToolCall` hook for the MCP, which will be run before the MCP tool calls are dispatched

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