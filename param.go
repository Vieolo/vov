package vov

import (
	"fmt"
	"slices"
	"strings"
)

// PathParam documents one wildcard in a route pattern.
//
// A path parameter is the one input with no Go type to hang anything on: it
// exists only as text in the pattern, so unlike a body or query field it has
// nowhere to carry a name of its own or a word about what it means. This is that
// somewhere.
//
// It is on [Endpoint] rather than on [MCPTool] because the facts are not
// MCP-specific. A generated OpenAPI document wants the same description in
// parameters[].description, and one declaration serving both is the same reason
// [Endpoint.Body] and [Endpoint.Query] live where they do.
type PathParam struct {
	// Name is what a caller sends this parameter as, when that should differ
	// from the wildcard. Empty keeps the wildcard's own name.
	//
	// It exists because the two audiences want different words. `{id}` is
	// idiomatic in a URL, where the surrounding path supplies the noun —
	// /projects/{id} is unambiguous to anyone reading it. It is not idiomatic as
	// an argument name offered to a model choosing between thirty tools, several
	// of which take the id of a different thing; there the noun has to be in the
	// name, because there is no path around it.
	//
	// Renaming the wildcard is the cleaner fix where an app can afford it —
	// /projects/{id} and /projects/{projectId} are the same route to
	// http.ServeMux, so it is an internal rename and not a URL change. Reach for
	// an alias when that rename would cost more than it is worth, and know that
	// it leaves two names for one value; [Manifest] renders the alias so that the
	// second one is visible rather than buried.
	Name string

	// Description tells a caller what to put here. For an assistant it is often
	// the difference between a working call and a silent empty result: which
	// format a value takes, which closed set it comes from, or — most valuable —
	// which other tool produces it ("the project's UUID, from list_projects").
	//
	// Long prose reads badly inside a route table. Keep it in a const beside the
	// handler and reference it, as [MCPTool] advises for the same reason.
	Description string
}

// pathArg is one wildcard resolved against its [PathParam] declaration: the name
// in the pattern, and the name a caller uses for it.
type pathArg struct {
	wildcard    string
	name        string
	description string
}

// resolvePathArgs returns a route's wildcards in pattern order, each carrying
// whatever the endpoint declared about it.
func resolvePathArgs(path string, e Endpoint) []pathArg {
	wildcards := pathParams(path)
	out := make([]pathArg, 0, len(wildcards))
	for _, w := range wildcards {
		arg := pathArg{wildcard: w, name: w}
		if p, ok := e.PathParams[w]; ok {
			if p.Name != "" {
				arg.name = p.Name
			}
			arg.description = p.Description
		}
		out = append(out, arg)
	}
	return out
}

// validatePathParams rejects a declaration that documents something the route
// does not have, or that gives two parameters the same name.
//
// Naming a wildcard that is not in the pattern is the mistake worth catching: a
// typo would otherwise document nothing at all, silently, and the symptom would
// be a tool argument that kept its unhelpful name for reasons nobody could see.
func validatePathParams(path string, e Endpoint) error {
	wildcards := pathParams(path)
	for name := range e.PathParams {
		if !slices.Contains(wildcards, name) {
			return fmt.Errorf("PathParams documents %q, which is not a wildcard in this route (it has %v)", name, wildcards)
		}
	}

	seen := map[string]string{}
	for _, arg := range resolvePathArgs(path, e) {
		if prev, dup := seen[arg.name]; dup {
			return fmt.Errorf("PathParams: %q and %q both resolve to the argument name %q", prev, arg.wildcard, arg.name)
		}
		seen[arg.name] = arg.wildcard
	}
	return nil
}

// pathParams returns the wildcard names in a ServeMux pattern, in order.
func pathParams(path string) []string {
	var out []string
	for rest := path; ; {
		open := strings.Index(rest, "{")
		if open < 0 {
			break
		}
		rest = rest[open+1:]
		close := strings.Index(rest, "}")
		if close < 0 {
			break
		}
		name := strings.TrimSuffix(rest[:close], "...")
		if name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
		rest = rest[close+1:]
	}
	return out
}
