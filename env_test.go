package vov

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A failing environment must report every problem at once, and must never put a
// variable's value in the message: the environment is where credentials live,
// and an error string ends up in logs. The values below are chosen so that a
// leak would be unmistakable.
func TestLoadEnvAggregatesProblemsWithoutLeakingValues(t *testing.T) {
	t.Setenv("T_PORT", "not-a-number-hunter2")
	t.Setenv("T_DUR", "not-a-duration-swordfish")
	t.Setenv("T_FLAG", "maybe-correcthorse")
	// T_SECRET deliberately unset: it is required.

	var cfg struct {
		Secret string        `env:"T_SECRET,required"`
		Port   int           `env:"T_PORT"`
		Dur    time.Duration `env:"T_DUR"`
		Flag   bool          `env:"T_FLAG"`
	}

	err := LoadEnv(&cfg)
	if err == nil {
		t.Fatal("expected an error")
	}
	var envErr *EnvError
	if !errors.As(err, &envErr) {
		t.Fatalf("expected *EnvError, got %T", err)
	}
	if len(envErr.Problems) != 4 {
		t.Fatalf("expected all 4 problems at once, got %d: %v", len(envErr.Problems), envErr.Problems)
	}

	msg := err.Error()
	for _, leak := range []string{"hunter2", "swordfish", "correcthorse"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error leaked an env value (%q):\n%s", leak, msg)
		}
	}
	for _, name := range []string{"T_SECRET", "T_PORT", "T_DUR", "T_FLAG"} {
		if !strings.Contains(msg, name) {
			t.Errorf("error does not name %s:\n%s", name, msg)
		}
	}
}

func TestLoadEnvTypesDefaultsAndNesting(t *testing.T) {
	t.Setenv("T_NAME", "tasks")
	t.Setenv("T_LIST", "a, b ,c")
	t.Setenv("T_OPT", "7")
	// T_MISSING unset -> default. T_ABSENT unset -> nil, not zero.

	var cfg struct {
		Name    string        `env:"T_NAME"`
		List    []string      `env:"T_LIST"`
		Opt     *int          `env:"T_OPT"`
		Absent  *int          `env:"T_ABSENT"`
		Missing time.Duration `env:"T_MISSING" envDefault:"90s"`
		Nested  struct {
			Inner string `env:"T_NAME"`
		}
		Untagged string
	}

	if err := LoadEnv(&cfg); err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	if cfg.Name != "tasks" {
		t.Errorf("Name = %q, want %q", cfg.Name, "tasks")
	}
	if want := []string{"a", "b", "c"}; !equalStrings(cfg.List, want) {
		t.Errorf("List = %#v, want %#v (comma-separated, trimmed)", cfg.List, want)
	}
	if cfg.Opt == nil || *cfg.Opt != 7 {
		t.Errorf("Opt = %v, want 7", cfg.Opt)
	}
	if cfg.Absent != nil {
		t.Errorf("Absent = %v, want nil so unset stays distinguishable from zero", *cfg.Absent)
	}
	if cfg.Missing != 90*time.Second {
		t.Errorf("Missing = %v, want the 90s default", cfg.Missing)
	}
	if cfg.Nested.Inner != "tasks" {
		t.Errorf("Nested.Inner = %q, want the nested struct to be walked", cfg.Nested.Inner)
	}
	if cfg.Untagged != "" {
		t.Errorf("Untagged = %q, want an untagged field left alone", cfg.Untagged)
	}
}

func TestLoadEnvRejectsBadTargets(t *testing.T) {
	var notAStruct int
	var plain struct{ A string }

	for name, target := range map[string]any{
		"nil":                nil,
		"non-struct pointer": &notAStruct,
		"non-pointer struct": plain,
	} {
		if err := LoadEnv(target); err == nil {
			t.Errorf("LoadEnv(%s): expected an error", name)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
