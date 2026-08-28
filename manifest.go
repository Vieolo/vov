package vov

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// manifestHeader introduces the rendered manifest. It is part of the file so
// that whoever opens it in a diff can read the format without leaving the file.
const manifestHeader = `# vov route manifest — every endpoint this app declares, and who may reach it.
# Regenerate whenever a route declaration changes; review the diff.
#
#   METHOD  PATH  auth:<mode>  stack:<name>  [roles-any:a|b]  [perms-all:x,y]
#                 [min-tier:N]  [query:Type]  [body:Type]  [mcp-tool:name]
#
# roles-any  is satisfied by ANY one of the listed roles       (| reads as "or")
# perms-all  requires EVERY one of the listed permissions      (, reads as "and")
# min-tier   requires User.Tier() >= N; refused with 402, not 403
# query/body the Go type the input shape was declared from
# mcp-tool   the endpoint is callable by an AI assistant under this name

`

// Column widths are constants rather than measured from the content: a manifest
// is read as a diff, and widths computed from the longest path would re-pad
// every line whenever one long route is added. A path wider than this overflows
// its own line and leaves the others alone.
const (
	manifestMethodWidth = 7 // "OPTIONS"
	manifestPathWidth   = 30
)

// Manifest renders routes as the text to check in beside the code.
//
// It exists because of a gap no test tier can cover. Generated tests assert that
// the code does what the declaration says, so an endpoint whose policy is
// quietly loosened stays green — the tests move with it. A checked-in manifest
// turns that same change into a modified line in a pull request, which is the
// only place a human sees it.
//
// The output is deterministic: routes are sorted by path, and each route's
// methods appear in a fixed order, so the file changes only when a declaration
// changes and not when the Routes slice is reordered.
//
// Every line carries its own method and path rather than being grouped under a
// URL heading, because a diff shows a handful of lines without their
// surroundings — a changed line has to say for itself which endpoint it is.
//
// The manifest covers declared routes only. Anything registered straight on
// [App.Mux] is outside the framework and cannot appear here.
func Manifest(routes []Route) string {
	sorted := slices.Clone(routes)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var b strings.Builder
	b.WriteString(manifestHeader)
	for _, r := range sorted {
		for _, me := range r.Endpoints.declared() {
			b.WriteString(manifestLine(r.Path, me))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// manifestLine renders one endpoint.
func manifestLine(path string, me methodEndpoint) string {
	method := me.Method
	if method == "" {
		method = "ANY" // declared under Endpoints.Any: no method restriction
	}

	stack := me.Endpoint.MiddlewareStack
	if stack == "" {
		stack = DefaultStackName
	}

	mode := me.Endpoint.AuthMode
	if mode == "" {
		mode = AuthModeRequired // the zero value; say what it means, not nothing
	}

	// Always print auth and stack, even when they are the default: a reviewer
	// should not have to know what an omitted field would have meant.
	fields := []string{
		fmt.Sprintf("auth:%s", mode),
		fmt.Sprintf("stack:%s", stack),
	}
	if roles := me.Endpoint.RolesAnyOf; len(roles) > 0 {
		fields = append(fields, "roles-any:"+strings.Join(roles, "|"))
	}
	if perms := me.Endpoint.PermissionsAllOf; len(perms) > 0 {
		fields = append(fields, "perms-all:"+strings.Join(perms, ","))
	}
	if tier := me.Endpoint.MinTier; tier > 0 {
		fields = append(fields, fmt.Sprintf("min-tier:%d", tier))
	}
	// The declared input shapes, by the Go type they came from. The name is what
	// a reviewer can act on: a route that gains or loses a body is a contract
	// change worth a line, while spelling out every field would bury the policy
	// this file exists to show.
	if q := me.Endpoint.Query; q != nil {
		fields = append(fields, "query:"+q.TypeName)
	}
	if b := me.Endpoint.Body; b != nil {
		fields = append(fields, "body:"+b.TypeName)
	}
	// Which endpoints an assistant can reach is worth a reviewer's attention in
	// its own right: it is a second audience for the same policy.
	if t := me.Endpoint.MCPTool; t != nil {
		fields = append(fields, "mcp-tool:"+t.Name)
	}

	return fmt.Sprintf("%-*s %-*s %s",
		manifestMethodWidth, method,
		manifestPathWidth, path,
		strings.Join(fields, "  "))
}

// Routes returns the route declarations the app was built from, in the order
// they were given. The slice is a copy, so a caller cannot reshape the app's
// routing by editing it.
func (a *App) Routes() []Route {
	return slices.Clone(a.routes)
}

// MCP returns the app's Model Context Protocol declaration, or nil when it has
// none. It is how the vov/mcp module reads what to serve; applications rarely
// need it.
func (a *App) MCP() *MCPConfig {
	return a.mcp
}

// Manifest renders the app's routes — see [Manifest].
func (a *App) Manifest() string {
	return Manifest(a.routes)
}
