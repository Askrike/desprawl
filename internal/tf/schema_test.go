package tf

import (
	"encoding/json"
	"testing"
)

func TestMapKey(t *testing.T) {
	s := &Schemas{Resources: map[string]*SchemaBlock{"x_function": {
		Attributes: map[string]*SchemaAttr{
			"policy": {Type: json.RawMessage(`"string"`)},
			"tags":   {Type: json.RawMessage(`["map","string"]`)},
		},
		BlockTypes: map[string]*BlockType{"environment": {Block: &SchemaBlock{Attributes: map[string]*SchemaAttr{
			"variables": {Type: json.RawMessage(`["map","string"]`)},
		}}}},
	}}}
	for path, want := range map[string]string{
		"environment.variables.API_URL": "API_URL",
		"tags.team":                     "team",
		"policy.Statement.Resource":     "",
		"environment.variables":         "",
	} {
		if got, _ := s.MapKey("x_function", path); got != want {
			t.Errorf("MapKey(%q) = %q, want %q", path, got, want)
		}
	}
}
