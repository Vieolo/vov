package vov

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// EnvError reports everything wrong with the environment at once. Its Problems
// name variables and say what was expected; they never contain a variable's
// value, because the environment is where credentials live and an error string
// ends up in logs.
type EnvError struct {
	Problems []string
}

func (e *EnvError) Error() string {
	if len(e.Problems) == 1 {
		return "vov: environment: " + e.Problems[0]
	}
	return fmt.Sprintf("vov: environment (%d problems):\n  - %s",
		len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// LoadEnv fills dst from the process environment, where dst is a pointer to a
// struct whose fields carry `env` tags:
//
//	type Config struct {
//	    DatabaseURL string        `env:"DATABASE_URL,required"`
//	    Port        int           `env:"PORT" envDefault:"8080"`
//	    Timeout     time.Duration `env:"TIMEOUT" envDefault:"30s"`
//	    Debug       bool          `env:"DEBUG"`
//	}
//
// The struct is both the declaration and the access surface, so what a service
// requires and how it reads it cannot drift apart. Call it before [NewApp]: the
// dependencies that need configuration are built before the endpoints that use
// them.
//
// A field with no `env` tag is left alone, so a config struct may hold values
// that come from somewhere else. Nested structs are walked. A missing optional
// variable leaves the field at its zero value; declare the field as a pointer to
// tell "unset" from "set to the zero value", as [Ptr] does elsewhere in vov.
//
// An unset variable and one set to the empty string are treated the same: the
// default applies, and a required variable is reported missing.
//
// Every problem is collected and returned together as an [EnvError] — one boot
// failure listing all of them beats fixing one variable per restart.
func LoadEnv(dst any) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return errors.New("vov: LoadEnv needs a non-nil pointer to a struct")
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return errors.New("vov: LoadEnv needs a pointer to a struct")
	}

	var problems []string
	loadStruct(v, &problems)
	if len(problems) > 0 {
		return &EnvError{Problems: problems}
	}
	return nil
}

// loadStruct fills every env-tagged field of v, appending to problems rather
// than stopping at the first failure.
func loadStruct(v reflect.Value, problems *[]string) {
	t := v.Type()
	for i := range t.NumField() {
		field, value := t.Field(i), v.Field(i)

		tag, tagged := field.Tag.Lookup("env")
		if !tagged {
			// Untagged structs are groups of configuration; walk into them.
			if field.Type.Kind() == reflect.Struct && value.CanSet() {
				loadStruct(value, problems)
			}
			continue
		}

		name, required := parseEnvTag(tag)
		if name == "" {
			*problems = append(*problems, fmt.Sprintf("field %s has an env tag with no name", field.Name))
			continue
		}
		if !value.CanSet() {
			*problems = append(*problems, fmt.Sprintf("%s: field %s is unexported and cannot be set", name, field.Name))
			continue
		}

		raw := os.Getenv(name)
		if raw == "" {
			raw = field.Tag.Get("envDefault")
		}
		if raw == "" {
			if required {
				*problems = append(*problems, fmt.Sprintf("%s is required but not set", name))
			}
			continue // optional and absent: leave the zero value
		}

		if err := setEnvValue(value, raw); err != nil {
			// err never quotes raw — see EnvError.
			*problems = append(*problems, fmt.Sprintf("%s: %v", name, err))
		}
	}
}

// parseEnvTag splits an `env` tag into its variable name and whether the
// variable is required: `env:"PORT"` or `env:"DATABASE_URL,required"`.
func parseEnvTag(tag string) (name string, required bool) {
	name, rest, _ := strings.Cut(tag, ",")
	for opt := range strings.SplitSeq(rest, ",") {
		if strings.TrimSpace(opt) == "required" {
			required = true
		}
	}
	return strings.TrimSpace(name), required
}

// setEnvValue parses raw into v according to v's type.
//
// Errors returned here must describe what was expected and must never include
// raw: a value that fails to parse can just as easily be a password as a port,
// and strconv's own errors quote the input.
func setEnvValue(v reflect.Value, raw string) error {
	// A pointer field distinguishes "unset" from "set to the zero value".
	if v.Kind() == reflect.Pointer {
		p := reflect.New(v.Type().Elem())
		if err := setEnvValue(p.Elem(), raw); err != nil {
			return err
		}
		v.Set(p)
		return nil
	}

	// Duration is an int64 underneath, so it has to be handled before Kind.
	if v.Type() == reflect.TypeOf(time.Duration(0)) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return errors.New("expected a duration such as 30s or 2m")
		}
		v.SetInt(int64(d))
		return nil
	}

	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("expected a boolean such as true or false")
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("expected an integer that fits in %s", v.Type())
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("expected a non-negative integer that fits in %s", v.Type())
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, v.Type().Bits())
		if err != nil {
			return errors.New("expected a number")
		}
		v.SetFloat(f)
	case reflect.Slice:
		if v.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported field type %s", v.Type())
		}
		parts := strings.Split(raw, ",")
		out := reflect.MakeSlice(v.Type(), 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = reflect.Append(out, reflect.ValueOf(p))
			}
		}
		v.Set(out)
	default:
		return fmt.Errorf("unsupported field type %s", v.Type())
	}
	return nil
}
