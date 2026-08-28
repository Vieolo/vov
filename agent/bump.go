package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The repository releases its modules in lockstep: root vov and vov/mcp always
// carry the same version, and every intra-repo requirement records it.
//
// That is not tidiness. A `replace` directive applies only in the main module, so
// a consumer of vov/mcp never sees the one pointing at ../ — it gets whatever
// version vov/mcp's go.mod *requires*. When that requirement is stale, `go get
// github.com/vieolo/vov/mcp` selects a vov too old to compile against, and the
// break is invisible from inside this repository, where the replace hides it. The
// whole point of the commands below is to make that state unreachable.
//
// vov/mcp is tagged on every release even when none of its code changed. A
// version it does not need costs nothing; a version it needs and does not have
// costs a consumer a build failure they cannot diagnose.

// releaseModules are the modules published under their own tag. A submodule's
// tag is its directory followed by the version, which is how the go tool finds
// it — hence mcp/v0.2.2 rather than v0.2.2.
var releaseModules = []struct{ label, tagPrefix string }{
	{"vov", ""},
	{"mcp", "mcp/"},
}

// versionedRequires lists, per module, the intra-repo requirements whose
// recorded version must equal the repository's own. A slice rather than a map so
// that a run reports them in the same order every time.
var versionedRequires = []struct {
	dir   string
	paths []string
}{
	{"mcp", []string{"github.com/vieolo/vov"}},
	{"examples/tasks", []string{"github.com/vieolo/vov", "github.com/vieolo/vov/mcp"}},
}

const (
	goYAMLName    = "go.yaml"
	changelogName = "changelog.md"
)

// bump raises the repository version and rewrites every file that records it.
//
// It edits files and stops. Committing and tagging stay with the operator, which
// is why the tag commands are printed rather than run: a pushed tag is the one
// step in a release that cannot be taken back quietly.
func bump(root string, minor bool) int {
	var r report

	cur, err := readVersion(root)
	if err != nil {
		r.fail("reading the current version: %v", err)
		return r.done("BUMP")
	}
	next := cur.nextPatch()
	if minor {
		next = cur.nextMinor()
	}
	fmt.Printf("-> bump :: %s -> %s\n", cur, next)

	if err := writeVersion(root, next); err != nil {
		r.fail("writing %s: %v", goYAMLName, err)
		return r.done("BUMP")
	}
	r.okf(true, "%s: version %s", goYAMLName, next)

	for _, m := range versionedRequires {
		dir := filepath.Join(root, m.dir)
		for _, path := range m.paths {
			if err := setRequire(dir, path, next); err != nil {
				r.fail("%s: %s: %v", m.dir, path, err)
				continue
			}
			r.okf(true, "%s/go.mod: require %s %s", m.dir, path, next.tag())
		}
	}

	switch has, err := hasChangelogEntry(root, next); {
	case err != nil:
		r.fail("reading %s: %v", changelogName, err)
	case has:
		r.okf(true, "%s: already documents %s", changelogName, next.tag())
	default:
		if err := openChangelogEntry(root, next); err != nil {
			r.fail("opening the changelog entry: %v", err)
		} else {
			r.okf(true, "%s: opened a section for %s — write the notes", changelogName, next.tag())
		}
	}

	if len(r.failures) == 0 {
		printReleaseSteps(next)
	}
	return r.done("BUMP")
}

// printReleaseSteps prints the part of a release the agent deliberately does not
// perform. Tags are listed explicitly rather than pushed with --tags, so that a
// release pushes the two it just created and nothing it did not.
func printReleaseSteps(v semver) {
	fmt.Printf("\nFiles are updated. The rest is yours to review and run:\n\n")
	fmt.Printf("  go -C agent run . all\n")
	fmt.Printf("  git commit -am %q\n", "Version bump to "+v.String())
	tags := make([]string, 0, len(releaseModules))
	for _, m := range releaseModules {
		tags = append(tags, m.tagPrefix+v.tag())
		fmt.Printf("  git tag %s%s\n", m.tagPrefix, v.tag())
	}
	fmt.Printf("  git push && git push origin %s\n", strings.Join(tags, " "))
}

// versions asserts that every recorded version agrees with go.yaml's.
//
// The bump command exists to make this redundant, and it runs anyway: a version
// can still be edited by hand, and this break stays invisible from inside the
// repository — the replace directives mean everything here builds against local
// source no matter what the requirements say. The first person to see it is a
// consumer outside the repository, who cannot fix it.
//
// It is its own command, and the last thing `all` runs, because it is release
// hygiene rather than a build check. Between a bug being noticed and the release
// that carries the fix, this is legitimately red — and a red release check must
// not stand in front of the checks that would catch a behavioural regression.
func versions(root string) int {
	var r report
	checkVersions(root, &r)
	return r.done("VERSIONS")
}

func checkVersions(root string, r *report) {
	fmt.Printf("-> versions :: recorded versions agree with %s\n", goYAMLName)

	v, err := readVersion(root)
	if err != nil {
		r.fail("versions :: reading %s: %v", goYAMLName, err)
		return
	}

	for _, m := range versionedRequires {
		got, err := modRequires(filepath.Join(root, m.dir))
		if err != nil {
			r.fail("versions :: %s: %v", m.dir, err)
			continue
		}
		for _, path := range m.paths {
			r.check(fmt.Sprintf("%s/go.mod requires %s", m.dir, path), got[path], v.tag())
		}
	}

	has, err := hasChangelogEntry(root, v)
	if err != nil {
		r.fail("versions :: reading %s: %v", changelogName, err)
		return
	}
	r.check(changelogName+" documents "+v.tag(), has, true)
}

// --- the version itself -----------------------------------------------------

// semver is the repository version: major.minor.patch, with no prerelease or
// build metadata, because nothing here has ever released with them.
type semver struct{ major, minor, patch int }

func parseSemver(s string) (semver, error) {
	fields := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(fields) != 3 {
		return semver{}, fmt.Errorf("%q is not major.minor.patch", strings.TrimSpace(s))
	}
	var v semver
	for i, dst := range []*int{&v.major, &v.minor, &v.patch} {
		n, err := strconv.Atoi(fields[i])
		if err != nil || n < 0 {
			return semver{}, fmt.Errorf("%q is not major.minor.patch", strings.TrimSpace(s))
		}
		*dst = n
	}
	return v, nil
}

func (v semver) String() string { return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch) }

// tag renders the version as Go records it, which is with a leading v. go.yaml
// stores it without one, and keeping the two renderings in separate methods is
// what stops a bare "0.2.2" reaching a go.mod.
func (v semver) tag() string { return "v" + v.String() }

func (v semver) nextPatch() semver { return semver{v.major, v.minor, v.patch + 1} }

// nextMinor resets the patch, so 0.2.1 becomes 0.3.0 rather than 0.3.1.
func (v semver) nextMinor() semver { return semver{v.major, v.minor + 1, 0} }

// A major bump is deliberately not offered. Pre-1.0 there is none to make, and
// afterwards it deserves more deliberation than a command that runs in a second.

// --- go.yaml ----------------------------------------------------------------

// readVersion reports the version go.yaml declares.
//
// go.yaml is read line by line rather than through a YAML parser so that the
// agent keeps no dependencies, and rewritten the same way so that every comment
// in the file survives a bump. A "version:" prefix at column zero is unambiguous:
// the key is at the document's top level and nothing nested is unindented.
func readVersion(root string) (semver, error) {
	lines, i, err := versionLine(root)
	if err != nil {
		return semver{}, err
	}
	return parseSemver(strings.TrimPrefix(lines[i], "version:"))
}

func writeVersion(root string, v semver) error {
	lines, i, err := versionLine(root)
	if err != nil {
		return err
	}
	lines[i] = "version: " + v.String()
	return os.WriteFile(filepath.Join(root, goYAMLName), []byte(strings.Join(lines, "\n")), 0o644)
}

// versionLine returns go.yaml's lines and the index of its version declaration.
func versionLine(root string) ([]string, int, error) {
	b, err := os.ReadFile(filepath.Join(root, goYAMLName))
	if err != nil {
		return nil, 0, err
	}
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "version:") {
			return lines, i, nil
		}
	}
	return nil, 0, fmt.Errorf("%s declares no top-level version", goYAMLName)
}

// --- go.mod -----------------------------------------------------------------

// modRequires reports the version the module at dir records for each module it
// requires. It goes through `go mod edit -json` rather than parsing the file, so
// a requirement inside a block reads the same as a standalone one.
func modRequires(dir string) (map[string]string, error) {
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("reading go.mod: %w", err)
	}
	var f struct {
		Require []struct{ Path, Version string }
	}
	if err := json.Unmarshal(out, &f); err != nil {
		return nil, fmt.Errorf("parsing go.mod: %w", err)
	}
	got := make(map[string]string, len(f.Require))
	for _, req := range f.Require {
		got[req.Path] = req.Version
	}
	return got, nil
}

// setRequire records path at v in the module at dir.
func setRequire(dir, path string, v semver) error {
	cmd := exec.Command("go", "mod", "edit", "-require="+path+"@"+v.tag())
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go mod edit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- changelog --------------------------------------------------------------

// changelogHeading is the heading under which a version's notes are written.
func changelogHeading(v semver) string { return "## " + v.tag() }

func hasChangelogEntry(root string, v semver) (bool, error) {
	b, err := os.ReadFile(filepath.Join(root, changelogName))
	if err != nil {
		return false, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, changelogHeading(v)) {
			return true, nil
		}
	}
	return false, nil
}

// openChangelogEntry inserts a dated heading for v above the newest one, and
// writes nothing under it. The agent knows that a version changed; it does not
// know what changed, and a generated summary of a release is worth less than an
// empty section that is visibly waiting to be filled in.
func openChangelogEntry(root string, v semver) error {
	path := filepath.Join(root, changelogName)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "## ") })
	if at < 0 {
		return fmt.Errorf("%s has no version heading to insert above", changelogName)
	}
	entry := []string{
		fmt.Sprintf("%s (%s)", changelogHeading(v), time.Now().Format("2006-01-02")),
		"",
	}
	return os.WriteFile(path, []byte(strings.Join(slices.Insert(lines, at, entry...), "\n")), 0o644)
}
