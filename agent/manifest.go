package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// manifestPath is the checked-in route manifest the example's declarations must
// agree with.
const manifestPath = "examples/tasks/routes.txt"

// manifest compares the example's live route manifest with the checked-in file,
// or rewrites the file when write is set.
//
// This is the check no test tier can stand in for. Generated tests assert that
// the code does what the declaration says, so an endpoint whose policy is
// quietly loosened stays green — the tests move with it. Here it is a changed
// line in a pull request, which is where a person sees it.
func manifest(root string, write bool) int {
	var r report

	cmd := exec.Command("go", "run", ".", "-manifest")
	cmd.Dir = filepath.Join(root, "examples", "tasks")
	cmd.Env = append(os.Environ(), exampleEnv...)
	out, err := cmd.Output()
	if err != nil {
		fmt.Println("could not render the manifest:")
		if ee, ok := err.(*exec.ExitError); ok {
			fmt.Println(string(ee.Stderr))
		}
		r.fail("rendering the manifest: %v", err)
		return r.done("MANIFEST")
	}
	current := string(out)

	file := filepath.Join(root, manifestPath)
	if write {
		if err := os.WriteFile(file, out, 0o644); err != nil {
			r.fail("writing %s: %v", manifestPath, err)
			return r.done("MANIFEST")
		}
		fmt.Printf("MANIFEST WRITTEN: %s\n", manifestPath)
		return 0
	}

	checkedIn, err := os.ReadFile(file)
	if err != nil {
		fmt.Printf("%s could not be read (run: agent manifest --write)\n", manifestPath)
		r.fail("reading %s: %v", manifestPath, err)
		return r.done("MANIFEST")
	}

	if string(checkedIn) == current {
		r.okf(true, "route manifest matches %s", manifestPath)
		return r.done("MANIFEST")
	}

	fmt.Printf("routes changed but %s was not updated.\n", manifestPath)
	fmt.Println("Review this — a loosened policy looks exactly like any other line.")
	fmt.Println()
	fmt.Print(diffLines(string(checkedIn), current))
	fmt.Println("\nIf the change is intended: agent manifest --write")
	r.fail("%s is out of date", manifestPath)
	return r.done("MANIFEST")
}

// diffLines reports which lines the two versions do not share, in file order.
//
// It is a set difference rather than a true diff, which suits this artifact
// better than it sounds: every manifest line is a self-contained statement about
// one endpoint, so a changed policy shows up as the old line leaving and the new
// one arriving — which is exactly the pair a reviewer needs to compare.
func diffLines(oldText, newText string) string {
	oldLines := strings.Split(oldText, "\n")
	newLines := strings.Split(newText, "\n")

	var b strings.Builder
	for _, l := range oldLines {
		if strings.TrimSpace(l) != "" && !slices.Contains(newLines, l) {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	for _, l := range newLines {
		if strings.TrimSpace(l) != "" && !slices.Contains(oldLines, l) {
			fmt.Fprintf(&b, "+ %s\n", l)
		}
	}
	return b.String()
}
