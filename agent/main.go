// Command agent runs vov's development checks deterministically.
//
//	go -C agent run . verify              build, vet, test and gofmt every module
//	go -C agent run . manifest [--write]  check (or regenerate) the route manifest
//	go -C agent run . smoke               run the example end to end
//	go -C agent run . versions            check every recorded version agrees
//	go -C agent run . all                 all four, in that order
//	go -C agent run . bump                0.2.1 -> 0.2.2, everywhere it is recorded
//	go -C agent run . bump-minor          0.2.1 -> 0.3.0, everywhere it is recorded
//
// It is its own module so that what it needs to check vov never becomes
// something vov's users have to download — the ephemeral database a future
// cross-tenant runner wants is the obvious example.
//
// Run this rather than the underlying go commands by hand: each check is meant
// to be identical every time, and several of them are easy to run subtly wrong.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// modules are the Go modules that make up the repository, in a fixed order.
// Each is checked on its own, because `go ... ./...` never crosses a module
// boundary and a break in one would otherwise stay invisible to the others.
var modules = []struct{ label, dir string }{
	{"vov", "."},
	{"agent", "agent"},
	{"examples/tasks", "examples/tasks"},
}

func main() {
	args := os.Args[1:]
	cmd := "verify"
	if len(args) > 0 {
		cmd = args[0]
	}

	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent:", err)
		os.Exit(2)
	}

	var code int
	switch cmd {
	case "verify":
		code = verify(root)
	case "manifest":
		code = manifest(root, hasFlag(args[1:], "--write"))
	case "smoke":
		code = smoke(root)
	case "versions":
		code = versions(root)
	case "bump":
		code = bump(root, false)
	case "bump-minor":
		code = bump(root, true)
	case "all":
		for _, run := range []func() int{
			func() int { return verify(root) },
			func() int { return manifest(root, false) },
			func() int { return smoke(root) },
			// Last: release hygiene, and legitimately red between noticing a
			// packaging bug and cutting the release that fixes it. It must not
			// stand in front of the checks above — see versions.
			func() int { return versions(root) },
		} {
			if code = run(); code != 0 {
				break
			}
		}
	default:
		fmt.Fprintf(os.Stderr, "agent: unknown command %q; supported: verify, manifest, smoke, versions, all, bump, bump-minor\n", cmd)
		code = 2
	}
	os.Exit(code)
}

func hasFlag(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// repoRoot returns the repository root, derived from this source file's own
// location so that the command works from any working directory.
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate the agent source")
	}
	root := filepath.Dir(filepath.Dir(file)) // agent/main.go -> agent -> repo root
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", fmt.Errorf("no go.mod at %s: %w", root, err)
	}
	return root, nil
}

// --- shared reporting -------------------------------------------------------

// report accumulates the failures of one command so that a run reports every
// problem rather than stopping at the first.
type report struct{ failures []string }

func (r *report) okf(ok bool, format string, args ...any) bool {
	mark := "ok "
	if !ok {
		mark = "BAD"
	}
	fmt.Printf("   %s %s\n", mark, fmt.Sprintf(format, args...))
	return ok
}

// check records a comparison, printing it either way.
func (r *report) check(label string, got, want any) {
	ok := fmt.Sprint(got) == fmt.Sprint(want)
	r.okf(ok, "%s: %v (want %v)", label, got, want)
	if !ok {
		r.failures = append(r.failures, fmt.Sprintf("%s: got %v, want %v", label, got, want))
	}
}

// fail records a failure that is not a comparison.
func (r *report) fail(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// done prints the verdict and returns the process exit code.
func (r *report) done(name string) int {
	fmt.Println()
	if len(r.failures) == 0 {
		fmt.Printf("%s PASSED\n", name)
		return 0
	}
	fmt.Printf("%s FAILED (%d):\n", name, len(r.failures))
	for _, f := range r.failures {
		fmt.Printf("  - %s\n", f)
	}
	return 1
}
