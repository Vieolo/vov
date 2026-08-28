package vov

import (
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
