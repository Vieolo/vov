package vov

import (
	"log/slog"
)

// MCPTool declares that an endpoint is callable by an AI assistant over the
// Model Context Protocol.
//
// It carries only what cannot be derived from the endpoint it sits on. The
// method, the path, the arguments, who may call it and what happens when they
// may not are all already declared — a tool adds a name and the prose that tells
// a model when to reach for it, and nothing else. Restating the route here would
// be the same duplication [Route] exists to remove.
//
// Descriptions are usually long, and long string literals read badly inside a
// route table. Keep them in a const beside the handler and reference it:
//
//	const listTasksDoc = "Every task on the list, with its id, title and owner. " +
//	    "Start here: the ids returned are what get_task takes."
//
//	GET: vov.Endpoint{
//	    Handler:     listTasks,
//	    Query:       vov.QueryOf[listTasksQuery](),
//	    MCPTool:     &vov.MCPTool{Name: "list_tasks", Description: listTasksDoc},
//	}
//
// Declaring an MCPTool does not by itself serve anything. A protocol package — see
// the vov/mcp module — reads these declarations and exposes them; vov itself only
// records that the endpoint is meant to be reachable that way, which is a fact
// worth having in the manifest whether or not a server is mounted.
type MCPTool struct {
	// Name is what an assistant calls, e.g. "list_tasks". It must be unique
	// across the app.
	Name string

	// Description tells the assistant what the tool is for and when to use it.
	// It is the whole of what a model knows when choosing between tools, so it
	// is worth more care than its length suggests.
	Description string

	// ReadOnly marks the tool as making no modifications, which lets a client
	// treat it as safe to call freely. Nil derives it from the endpoint's
	// method: GET and HEAD are read-only, everything else is not.
	ReadOnly *bool
}

// MCPConfig describes the Model Context Protocol server an app exposes, when it
// exposes one. It is the app-level half of the declaration; the per-endpoint half
// is [MCPTool].
//
// Setting it does not start anything. It records what the server is called and
// how a caller is identified, so that the vov/mcp module can build a handler from
// declarations alone rather than being handed a second copy of them.
type MCPConfig struct {
	// Name and Version identify this server to clients. Both are required when
	// MCPConfig is set.
	Name    string
	Version string

	// Title is an optional human-readable name.
	Title string

	// Instructions tell an assistant how to navigate this particular product —
	// which tool to establish context with before the others make sense, for
	// instance. They reach the model once, at connection, and change how well it
	// uses everything else.
	Instructions string

	// Authenticate resolves the user a tool call acts as. It is required, and it
	// is the seam vov cannot cross: only the application knows which credentials
	// its tool endpoint honours — an OAuth access token bound to an audience is
	// a different thing from the session cookie a browser sends.
	//
	// An app whose tool endpoint honours its ordinary credentials passes the
	// same function it gave [AppConfig.Authenticator].
	Authenticate Authenticator

	// Path is the URL the tool server is served at, e.g. "/mcp". Setting it is
	// all it takes: [NewApp] builds the handler and mounts it, and there is
	// nothing to do afterwards.
	//
	// Leave it empty to mount the handler yourself — take it from
	// [App.MCPHandler] and register it wherever you like, which is what an app
	// that wraps the tool endpoint in its own OAuth gate will want. The handler
	// is built either way, so a declaration that cannot produce one fails at
	// construction whichever route is taken.
	Path string

	// Logger, if set, receives protocol-level logging from the tool server.
	Logger *slog.Logger
}
