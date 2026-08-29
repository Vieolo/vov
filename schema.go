package vov

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode"
)

// Kind is the JSON type of a declared value.
type Kind string

const (
	KindString  Kind = "string"
	KindNumber  Kind = "number"
	KindInteger Kind = "integer"
	KindBoolean Kind = "boolean"
	KindArray   Kind = "array"
	KindObject  Kind = "object"

	// KindAny is a value whose shape is deliberately not described — a
	// json.RawMessage or an any field. It says "something goes here", which is
	// honest, rather than pretending to a structure that was never declared.
	KindAny Kind = ""
)

// Field is one member of an object [Schema].
type Field struct {
	// Name is the JSON name, from the json tag when there is one.
	Name string

	// Schema describes the field's value.
	Schema *Schema

	// Required reports whether the field must be present. It comes from a
	// `vov:"required"` tag and never from inference: `omitempty` is about how a
	// value is written, not whether a caller must send it, and reading a
	// contract off an encoding hint would make the declaration mean something
	// its author did not write.
	Required bool

	// Description is the prose a consumer shows for this field, from a
	// `jsonschema:"…"` tag whose whole value is the description:
	//
	//	Country string `json:"country" jsonschema:"ISO-3166 alpha-2 code, matched against the investor's home country"`
	//
	// It is a separate tag from vov's own on purpose. `vov:"…"` is a list of
	// comma-separated options, and a description is free text that routinely
	// contains commas and colons — putting prose in a flag list would need
	// escaping to say the very things worth saying. The tag is the one the
	// ecosystem already uses for exactly this, so a type carrying descriptions
	// reads the same to vov and to a generic JSON Schema inferrer.
	//
	// Shape describes what a value *is*; this describes what to put there, which
	// is a different question and the one a caller usually gets wrong. For an
	// assistant it is often the whole difference between a working call and a
	// silently empty result.
	Description string
}

// Schema describes the shape of a request body or query string.
//
// It is derived from a Go type — the same type the handler decodes into — so a
// declaration cannot drift from the code that reads it. What it is *for* is
// being consumed: an OpenAPI document, an MCP tool's input schema, and a test
// runner that has to construct a valid-but-foreign body to attempt a
// cross-tenant write all need this same description, and deriving it once is why
// it lives on the endpoint.
//
// It describes; it does not enforce. vov does not decode the body, because
// decoding is where a PATCH decides whether an absent field and an explicit null
// mean the same thing — a distinction Go's own decoder collapses for a plain
// pointer, and one only the application's types can resolve. Handlers decode as
// they always did; semantic rules stay with them.
type Schema struct {
	Kind Kind

	// Fields are the members of an object, in declaration order. Empty on an
	// object means free-form — a map, whose keys are not known ahead of time.
	Fields []Field

	// Elem is the element schema of an array.
	Elem *Schema

	// Nullable reports whether null is an accepted value. A Go pointer is both
	// nullable and, absent a required tag, optional.
	Nullable bool

	// Format carries a JSON Schema format hint, e.g. "date-time".
	Format string

	// TypeName is the Go type the schema was derived from, for the manifest.
	TypeName string

	// err records why a type could not be described. [NewApp] surfaces it, so a
	// declaration that cannot be honoured fails at construction rather than
	// producing a schema that quietly lies.
	err error
}

// BodyOf describes T as an endpoint's request body.
//
// Pass the type that describes the *contract* — what a caller may send. Often
// that is the type the handler decodes into, and keeping them the same is worth
// something: the declaration and the code that reads it cannot then drift.
//
// It is not always the same, and should not be forced to be. A handler that
// decodes into a domain model would, by declaring it, advertise every field of
// that model — including the ones the server owns and overwrites, which a caller
// must not set and an assistant should never be offered. Declaring a narrower
// type is the safer direction and the recommended one there: a field added to
// the model later is not silently published as an input.
//
// vov cannot check that the two agree — a handler is opaque to it — so this is
// convention either way. The trade is between drift vov cannot detect and a
// surface that is wrong on purpose, and the second is worse.
//
// A body type in the simple case:
//
//	type createProject struct {
//	    Name    string  `json:"name" vov:"required"`
//	    Country *string `json:"hqCountry"` // optional, and may be null
//	}
//
//	POST: vov.Endpoint{Handler: create, Body: vov.BodyOf[createProject]()}
func BodyOf[T any]() *Schema {
	return schemaOf(reflect.TypeFor[T](), 0)
}

// QueryOf describes T as an endpoint's query string. T should be a flat struct:
// a query is a list of names and values, so a nested object has no natural
// encoding and is rejected.
func QueryOf[T any]() *Schema {
	s := schemaOf(reflect.TypeFor[T](), 0)
	if s.err == nil && s.Kind != KindObject {
		s.err = fmt.Errorf("query must be described by a struct, not %s", s.TypeName)
	}
	if s.err == nil {
		for _, f := range s.Fields {
			switch f.Schema.Kind {
			case KindObject, KindAny:
				s.err = fmt.Errorf("query field %q is %s; a query carries only scalars and lists of them", f.Name, f.Schema.Kind)
			case KindArray:
				if f.Schema.Elem != nil && f.Schema.Elem.Kind == KindObject {
					s.err = fmt.Errorf("query field %q is a list of objects, which a query string cannot carry", f.Name)
				}
			}
		}
	}
	return s
}

// maxSchemaDepth bounds recursion. A self-referential type would otherwise
// describe itself forever.
const maxSchemaDepth = 20

var (
	timeType       = reflect.TypeFor[time.Time]()
	rawMessageType = reflect.TypeFor[json.RawMessage]()
)

// schemaOf reflects t into a Schema.
func schemaOf(t reflect.Type, depth int) *Schema {
	if depth > maxSchemaDepth {
		return &Schema{err: fmt.Errorf("type nests deeper than %d levels, or refers to itself", maxSchemaDepth)}
	}

	s := &Schema{TypeName: typeName(t)}

	// A pointer is nullable; describe what it points at.
	if t.Kind() == reflect.Pointer {
		inner := schemaOf(t.Elem(), depth)
		inner.Nullable = true
		inner.TypeName = s.TypeName
		return inner
	}

	// Types with their own JSON representation, before their Kind is consulted.
	switch t {
	case timeType:
		s.Kind, s.Format = KindString, "date-time"
		return s
	case rawMessageType:
		s.Kind = KindAny
		return s
	}

	switch t.Kind() {
	case reflect.String:
		s.Kind = KindString
	case reflect.Bool:
		s.Kind = KindBoolean
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		s.Kind = KindInteger
	case reflect.Float32, reflect.Float64:
		s.Kind = KindNumber
	case reflect.Interface:
		s.Kind = KindAny
	case reflect.Slice, reflect.Array:
		// []byte is base64 text in JSON, not a list of numbers.
		if t.Elem().Kind() == reflect.Uint8 && t.Kind() == reflect.Slice {
			s.Kind, s.Format = KindString, "byte"
			return s
		}
		s.Kind = KindArray
		s.Elem = schemaOf(t.Elem(), depth+1)
		s.err = s.Elem.err
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			s.err = fmt.Errorf("map key is %s; JSON object keys are strings", t.Key())
			return s
		}
		// A map is an object whose members are not known in advance, so it is
		// described as free-form rather than with a field list.
		s.Kind = KindObject
	case reflect.Struct:
		s.Kind = KindObject
		s.Fields, s.err = structFields(t, depth)
	default:
		s.err = fmt.Errorf("cannot describe %s", t.Kind())
	}
	return s
}

// structFields describes t's exported fields, following encoding/json's rules
// for names, omission, and embedding.
func structFields(t reflect.Type, depth int) ([]Field, error) {
	var fields []Field
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue // json ignores it, so the declaration must too
		}

		name, ok := jsonName(sf)
		if !ok {
			continue // `json:"-"`
		}

		// An embedded struct without a json name is flattened by encoding/json,
		// so its fields belong to this object.
		if sf.Anonymous && name == "" {
			inner := schemaOf(sf.Type, depth+1)
			if inner.err != nil {
				return nil, inner.err
			}
			fields = append(fields, inner.Fields...)
			continue
		}
		if name == "" {
			name = sf.Name
		}

		fs := schemaOf(sf.Type, depth+1)
		if fs.err != nil {
			return nil, fmt.Errorf("field %s: %w", sf.Name, fs.err)
		}
		desc, err := fieldDescription(sf)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", sf.Name, err)
		}
		fields = append(fields, Field{
			Name:        name,
			Schema:      fs,
			Required:    hasTagOption(sf.Tag.Get("vov"), "required"),
			Description: desc,
		})
	}
	return fields, nil
}

// jsonName returns the field's JSON name, and false when json skips the field.
// An empty name with ok means the field has no json tag name of its own.
func jsonName(sf reflect.StructField) (name string, ok bool) {
	tag, tagged := sf.Tag.Lookup("json")
	if !tagged {
		return "", true
	}
	name, _, _ = strings.Cut(tag, ",")
	if name == "-" {
		// `json:"-"` skips; `json:"-,"` means a field literally named "-".
		if !strings.HasPrefix(tag, "-,") {
			return "", false
		}
		return "-", true
	}
	return name, true
}

// fieldDescription reads a field's `jsonschema:"…"` tag.
//
// The whole tag value is the description; there is no key=value syntax, which is
// what lets prose contain commas and colons without escaping. Two shapes are
// rejected rather than accepted quietly, both of them the ecosystem's own rules:
// an empty tag, which says nothing while looking like it says something, and one
// beginning with "WORD=", which is reserved for future syntax. Honouring that
// reservation is what keeps a type's tags meaning the same thing to vov and to
// any other reader of them.
func fieldDescription(sf reflect.StructField) (string, error) {
	tag, ok := sf.Tag.Lookup("jsonschema")
	if !ok {
		return "", nil
	}
	if tag == "" {
		return "", fmt.Errorf(`has an empty jsonschema tag; remove it or describe the field`)
	}
	if word, _, found := strings.Cut(tag, "="); found && word != "" && !strings.ContainsFunc(word, unicode.IsSpace) {
		return "", fmt.Errorf("jsonschema tag begins with %q=, which is reserved; a description is the whole tag value", word)
	}
	return tag, nil
}

func hasTagOption(tag, want string) bool {
	for opt := range strings.SplitSeq(tag, ",") {
		if strings.TrimSpace(opt) == want {
			return true
		}
	}
	return false
}

// typeName renders a type for the manifest: the bare name for a named type,
// something readable for the rest.
func typeName(t reflect.Type) string {
	if n := t.Name(); n != "" {
		return n
	}
	switch t.Kind() {
	case reflect.Pointer:
		return typeName(t.Elem())
	case reflect.Slice, reflect.Array:
		return "[]" + typeName(t.Elem())
	case reflect.Map:
		return "map[" + typeName(t.Key()) + "]" + typeName(t.Elem())
	default:
		return t.String()
	}
}

// JSONSchema renders the schema as a JSON Schema document, which is the shape an
// MCP tool's inputSchema and an OpenAPI requestBody both want.
func (s *Schema) JSONSchema() map[string]any {
	if s == nil {
		return nil
	}
	out := map[string]any{}

	if s.Kind != KindAny {
		if s.Nullable {
			out["type"] = []any{string(s.Kind), "null"}
		} else {
			out["type"] = string(s.Kind)
		}
	}
	if s.Format != "" {
		out["format"] = s.Format
	}

	switch s.Kind {
	case KindObject:
		if len(s.Fields) > 0 {
			props := make(map[string]any, len(s.Fields))
			var required []string
			for _, f := range s.Fields {
				props[f.Name] = f.JSONSchema()
				if f.Required {
					required = append(required, f.Name)
				}
			}
			out["properties"] = props
			if len(required) > 0 {
				out["required"] = required
			}
		}
	case KindArray:
		if s.Elem != nil {
			out["items"] = s.Elem.JSONSchema()
		}
	}
	return out
}

// JSONSchema renders the field as a JSON Schema property: its value's schema,
// plus the field's own description. The description belongs to the field rather
// than to its type, which is why it is added here and not by [Schema.JSONSchema]
// — two fields of the same type describe different things.
func (f Field) JSONSchema() map[string]any {
	out := f.Schema.JSONSchema()
	if out == nil {
		out = map[string]any{}
	}
	if f.Description != "" {
		out["description"] = f.Description
	}
	return out
}

// Err reports why a declared type could not be described, if it could not be.
func (s *Schema) Err() error {
	if s == nil {
		return nil
	}
	return s.err
}
