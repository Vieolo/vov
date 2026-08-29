package vov

import (
	"encoding/json"
	"net/http"
	"testing"
)

func paramApp(params map[string]PathParam) (*App, error) {
	return NewApp(AppConfig{
		Routes: []Route{{
			Path: "/projects/{id}/rounds/{roundId}",
			Endpoints: Endpoints{GET: Endpoint{
				AuthMode:   AuthModeNone,
				PathParams: params,
				Handler:    func(http.ResponseWriter, *http.Request) {},
			}},
		}},
	})
}

// TestPathParamsRejectAnUnknownWildcard is the mistake worth catching at
// construction: a typo would otherwise document nothing at all, and the symptom
// would be an argument that silently kept its unhelpful name.
func TestPathParamsRejectAnUnknownWildcard(t *testing.T) {
	if _, err := paramApp(map[string]PathParam{"projectId": {Name: "p"}}); err == nil {
		t.Fatal("NewApp accepted PathParams naming a wildcard the route does not declare")
	}
	// The real wildcard is accepted, so the check is not simply refusing.
	if _, err := paramApp(map[string]PathParam{"id": {Name: "projectId"}}); err != nil {
		t.Fatalf("NewApp rejected a valid declaration: %v", err)
	}
}

// TestPathParamsRejectCollidingNames: two wildcards resolving to one argument
// name would make the second silently unreachable.
func TestPathParamsRejectCollidingNames(t *testing.T) {
	_, err := paramApp(map[string]PathParam{
		"id":      {Name: "roundId"},
		"roundId": {},
	})
	if err == nil {
		t.Fatal("NewApp accepted two path parameters resolving to the same name")
	}
}

// TestPathArgsAliasOnlyWhatIsDeclared: an undeclared wildcard keeps its own
// name, so declaring one parameter does not disturb its siblings.
func TestPathArgsAliasOnlyWhatIsDeclared(t *testing.T) {
	e := Endpoint{PathParams: map[string]PathParam{"id": {Name: "projectId", Description: "d"}}}
	got := resolvePathArgs("/projects/{id}/rounds/{roundId}", e)

	if len(got) != 2 {
		t.Fatalf("got %d args, want 2", len(got))
	}
	if got[0].wildcard != "id" || got[0].name != "projectId" || got[0].description != "d" {
		t.Errorf("declared param resolved to %+v", got[0])
	}
	if got[1].wildcard != "roundId" || got[1].name != "roundId" || got[1].description != "" {
		t.Errorf("undeclared param resolved to %+v", got[1])
	}
}

// --- descriptions on typed fields -------------------------------------------

type described struct {
	// The two tags do different jobs: one is a list of options, the other is
	// free text that contains the commas and colons worth writing.
	Stage string `json:"stage" vov:"required" jsonschema:"one funding stage: Pre-seed, Seed, Series A or Growth"`
	Plain string `json:"plain"`
}

// TestFieldDescriptionsSurviveTheirPunctuation is why a description does not go
// in vov's own comma-separated options tag.
func TestFieldDescriptionsSurviveTheirPunctuation(t *testing.T) {
	s := BodyOf[described]()
	if err := s.Err(); err != nil {
		t.Fatalf("BodyOf: %v", err)
	}

	want := "one funding stage: Pre-seed, Seed, Series A or Growth"
	if s.Fields[0].Description != want {
		t.Errorf("description %q, want %q", s.Fields[0].Description, want)
	}
	if !s.Fields[0].Required {
		t.Error("the vov tag stopped being read once a second tag was present")
	}
	if s.Fields[1].Description != "" {
		t.Errorf("an untagged field gained a description: %q", s.Fields[1].Description)
	}

	props := s.JSONSchema()["properties"].(map[string]any)
	if got := props["stage"].(map[string]any)["description"]; got != want {
		t.Errorf("rendered description %q, want %q", got, want)
	}
	if _, ok := props["plain"].(map[string]any)["description"]; ok {
		t.Error("an untagged field rendered a description key")
	}
}

// TestEmptyDescriptionTagIsRejected: a tag that says nothing while looking like
// it says something is a mistake, not a default.
func TestEmptyDescriptionTagIsRejected(t *testing.T) {
	type empty struct {
		A string `json:"a" jsonschema:""`
	}
	if err := BodyOf[empty]().Err(); err == nil {
		t.Fatal("an empty jsonschema tag was accepted")
	}
}

// TestReservedDescriptionPrefixIsRejected honours the ecosystem's reservation of
// "WORD=" for future syntax, so a type's tags keep meaning the same thing to vov
// and to any other reader of them.
func TestReservedDescriptionPrefixIsRejected(t *testing.T) {
	type reserved struct {
		A string `json:"a" jsonschema:"enum=a|b"`
	}
	if err := BodyOf[reserved]().Err(); err == nil {
		t.Fatal("a jsonschema tag beginning with WORD= was accepted")
	}
	// A description that merely contains "=" later on is fine.
	type fine struct {
		A string `json:"a" jsonschema:"a filter, written as key=value"`
	}
	if err := BodyOf[fine]().Err(); err != nil {
		t.Fatalf("a description containing = was rejected: %v", err)
	}
}

// --- repeated query parameters ----------------------------------------------

type listQuery struct {
	Tag  []string `json:"tag" jsonschema:"repeat to filter by several tags"`
	View string   `json:"view"`
}

// TestQueryOfAndDispatchAgreeOnLists is the halves agreeing again. QueryOf has
// always accepted a list of scalars; dispatch used to refuse it, so a
// declaration vov validated at construction could still fail at call time — the
// one kind of failure the rest of vov is arranged to make impossible.
func TestQueryOfAndDispatchAgreeOnLists(t *testing.T) {
	if err := QueryOf[listQuery]().Err(); err != nil {
		t.Fatalf("QueryOf rejected a list of scalars: %v", err)
	}

	got, err := queryArg(json.RawMessage(`["a","b","c"]`))
	if err != nil {
		t.Fatalf("dispatch rejected what QueryOf accepted: %v", err)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("got %v, want [a b c]", got)
	}
}

// TestQueryArgRepeatsRatherThanOverwrites: the values become a repeated
// parameter, not the last one winning.
func TestQueryArgRepeatsRatherThanOverwrites(t *testing.T) {
	tool := boundTool{path: "/x", queryNames: []string{"tag"}}
	_, query, _, err := tool.splitArgs(map[string]json.RawMessage{"tag": json.RawMessage(`["a","b"]`)})
	if err != nil {
		t.Fatalf("splitArgs: %v", err)
	}
	if got := query["tag"]; len(got) != 2 {
		t.Fatalf("query carried %v, want two values", got)
	}
	if query.Encode() != "tag=a&tag=b" {
		t.Errorf("encoded as %q, want %q", query.Encode(), "tag=a&tag=b")
	}
}

// TestQueryArgScalarsAreUnchanged: the list support did not alter what a plain
// value does, including an empty one meaning "omitted".
func TestQueryArgScalarsAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{`"fit"`, []string{"fit"}},
		{`7`, []string{"7"}},
		{`true`, []string{"true"}},
		{`""`, nil},
		{`null`, nil},
	} {
		got, err := queryArg(json.RawMessage(tc.raw))
		if err != nil {
			t.Errorf("%s: %v", tc.raw, err)
			continue
		}
		if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
			t.Errorf("%s: got %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestQueryArgRejectsNestedLists: a list of objects is what QueryOf refuses at
// construction, and dispatch refuses the same thing rather than encoding it.
func TestQueryArgRejectsNestedLists(t *testing.T) {
	if _, err := queryArg(json.RawMessage(`[{"a":1}]`)); err == nil {
		t.Fatal("a list of objects was accepted as a query value")
	}
	type nested struct {
		A []struct{ B string } `json:"a"`
	}
	if err := QueryOf[nested]().Err(); err == nil {
		t.Fatal("QueryOf accepted a list of objects")
	}
}
