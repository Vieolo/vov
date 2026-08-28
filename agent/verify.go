package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// stages are run against every module, in order: a module that does not build
// cannot usefully be vetted, and one that does not vet cannot usefully be tested.
var stages = []struct {
	name string
	args []string
}{
	{"build", []string{"go", "build", "./..."}},
	{"vet", []string{"go", "vet", "./..."}},
	{"test", []string{"go", "test", "./..."}},
	{"fmt", []string{"gofmt", "-l", "."}},
}

// verify builds, vets, tests and format-checks every module.
func verify(root string) int {
	var r report
	for _, m := range modules {
		for _, st := range stages {
			dir := filepath.Join(root, m.dir)
			fmt.Printf("-> %s :: %s\n", m.label, strings.Join(st.args, " "))

			ok, out := runStage(dir, st.name, st.args)
			if out != "" {
				fmt.Println(out)
			}
			verdict := "PASS"
			if !ok {
				verdict = "FAIL"
				r.fail("%s :: %s", m.label, st.name)
			}
			fmt.Printf("   %s: %s :: %s\n", verdict, m.label, st.name)
		}
	}
	return r.done("VERIFY")
}

// runStage runs one stage and reports whether it passed.
//
// gofmt needs its own reading: it exits 0 even when it lists files that are not
// formatted, so its output is the result rather than its status.
func runStage(dir, name string, args []string) (ok bool, output string) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir

	if name == "fmt" {
		out, err := cmd.Output()
		listed := strings.TrimSpace(string(out))
		return err == nil && listed == "", listed
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run() == nil, ""
}
