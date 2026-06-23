package repo

import (
	"testing"

	"github.com/taozhang/llmrelay/internal/schema"
)

func TestTargetsToJSON(t *testing.T) {
	// Empty → empty string (DB column default); ParseTargets falls back.
	if s := targetsToJSON(nil); s != "" {
		t.Errorf("empty targets should be empty string, got %q", s)
	}
	targets := []AliasTarget{{Provider: "p", Model: "m"}}
	s := targetsToJSON(targets)
	if s != `[{"provider":"p","model":"m"}]` {
		t.Errorf("got %q", s)
	}
}

func TestParseTargets_Fallback(t *testing.T) {
	// Empty targets_json → single {provider, model} from the row.
	a := schema.ModelAlias{Provider: "p", Model: "m", TargetsJSON: ""}
	got, err := ParseTargets(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Provider != "p" || got[0].Model != "m" {
		t.Errorf("fallback parse: %+v", got)
	}
}

func TestParseTargets_JSON(t *testing.T) {
	a := schema.ModelAlias{TargetsJSON: `[{"provider":"a","model":"1"},{"provider":"b","model":"2"}]`}
	got, err := ParseTargets(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Provider != "b" {
		t.Errorf("json parse: %+v", got)
	}
}

func TestParseTargets_Invalid(t *testing.T) {
	a := schema.ModelAlias{Alias: "x", TargetsJSON: "not json"}
	if _, err := ParseTargets(a); err == nil {
		t.Error("expected error for invalid json")
	}
}
