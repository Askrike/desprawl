package generate

import (
	"encoding/json"
	"testing"

	"github.com/askrike/desprawl/internal/tf"
)

// Terraform rejects an ignore_changes path that indexes a set.
func TestIndexesSet(t *testing.T) {
	var s tf.SchemaBlock
	if err := json.Unmarshal([]byte(`{
		"attributes": {
			"ingress": {"type": ["set", ["object", {"description": "string"}]]},
			"rules": {"type": ["list", ["object", {"ports": ["set", "number"], "name": "string"}]]},
			"tags": {"type": ["map", "string"]}
		},
		"block_types": {
			"origin": {"nesting_mode": "set", "block": {"attributes": {"id": {"type": "string"}}}},
			"setting": {"nesting_mode": "list", "block": {
				"attributes": {"value": {"type": "string"}},
				"block_types": {"inner": {"nesting_mode": "set", "block": {}}}
			}},
			"config": {"nesting_mode": "single", "block": {"attributes": {"x": {"type": "string"}}}}
		}
	}`), &s); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"ingress[0].description": true,
		"ingress":                false,
		"rules[0].name":          false,
		"rules[1].ports[0]":      true,
		"tags.team":              false,
		"origin[0].id":           true,
		"setting[0].value":       false,
		"setting[0].inner[0]":    true,
		"config.x":               false,
		"unknown[0]":             false,
	} {
		if got := indexesSet(&s, path); got != want {
			t.Errorf("indexesSet(%s) = %v, want %v", path, got, want)
		}
	}
}
