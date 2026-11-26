package suggest

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/askrike/desprawl/internal/inventory"
	"github.com/askrike/desprawl/internal/tf"
)

// A listing data source may hold its entries in a list block rather than
// an attribute, and number IDs; both are inventory sources, and those
// named after a type come first.
func TestInventorySourcesBlocksAndNumbers(t *testing.T) {
	computed := func(typ string) *tf.SchemaAttr { return &tf.SchemaAttr{Type: json.RawMessage(typ), Computed: true} }
	schemas := &tf.Schemas{
		Resources: map[string]*tf.SchemaBlock{"x_team": {}, "x_alert": {}, "x_policy": {}},
		DataSources: map[string]*tf.SchemaBlock{
			// entries in a list block
			"x_teams": {BlockTypes: map[string]*tf.BlockType{"teams": {NestingMode: "list", Block: &tf.SchemaBlock{
				Attributes: map[string]*tf.SchemaAttr{"id": computed(`"string"`), "handle": computed(`"string"`)},
			}}}},
			// a number ID in a list attribute
			"x_alerts": {Attributes: map[string]*tf.SchemaAttr{
				"alerts": computed(`["list",["object",{"id":"number","name":"string"}]]`),
			}},
			"x_policies": {Attributes: map[string]*tf.SchemaAttr{
				"policies": computed(`["list",["object",{"id":"string"}]]`),
			}},
			"x_hosts": {Attributes: map[string]*tf.SchemaAttr{
				"hosts": computed(`["list",["object",{"id":"string"}]]`),
			}},
			// a filter block people set is not a list of entries
			"x_filtered": {BlockTypes: map[string]*tf.BlockType{"filter": {NestingMode: "list", Block: &tf.SchemaBlock{
				Attributes: map[string]*tf.SchemaAttr{"name": {Type: json.RawMessage(`"string"`), Required: true}},
			}}}},
		},
	}
	got := InventorySources(schemas, []string{"x_team", "x_alert", "x_policy"})
	want := []string{"x_alerts", "x_policies", "x_teams", "x_hosts"}
	if !slices.Equal(got, want) {
		t.Errorf("sources = %v, want %v", got, want)
	}
	for _, s := range []inventory.Source{
		{DataSource: "x_teams", List: "teams", ID: "id", Name: "handle"},
		{DataSource: "x_alerts", List: "alerts", ID: "id", Name: "name"},
	} {
		if err := CheckSource(schemas, s); err != nil {
			t.Errorf("%s: %v", s.DataSource, err)
		}
	}
	if err := CheckSource(schemas, inventory.Source{DataSource: "x_teams", List: "teams", ID: "uuid"}); err == nil {
		t.Error("x_teams: a missing field passed")
	}
}

// A listing data source gets a complete source when its schema settles it:
// one entry list with an id field, no required arguments.
func TestInventorySource(t *testing.T) {
	computed := func(typ string) *tf.SchemaAttr { return &tf.SchemaAttr{Type: json.RawMessage(typ), Computed: true} }
	schemas := &tf.Schemas{
		DataSources: map[string]*tf.SchemaBlock{
			"x_teams": {
				Attributes: map[string]*tf.SchemaAttr{"filter": {Type: json.RawMessage(`"string"`), Optional: true}},
				BlockTypes: map[string]*tf.BlockType{"teams": {NestingMode: "list", Block: &tf.SchemaBlock{
					Attributes: map[string]*tf.SchemaAttr{"id": computed(`"string"`), "handle": computed(`"string"`)},
				}}},
			},
			"x_alerts": {Attributes: map[string]*tf.SchemaAttr{
				"alerts": computed(`["list",["object",{"id":"number","display_name":"string"}]]`),
			}},
			"x_keyed": {Attributes: map[string]*tf.SchemaAttr{
				"keys": computed(`["list",["object",{"key":"string"}]]`),
			}},
			"x_two": {Attributes: map[string]*tf.SchemaAttr{
				"a": computed(`["list",["object",{"id":"string"}]]`),
				"b": computed(`["list",["object",{"id":"string"}]]`),
			}},
			"x_scoped": {Attributes: map[string]*tf.SchemaAttr{
				"parent": {Type: json.RawMessage(`"string"`), Required: true},
				"items":  computed(`["list",["object",{"id":"string"}]]`),
			}},
		},
	}
	for ds, want := range map[string]inventory.Source{
		"x_teams":  {DataSource: "x_teams", List: "teams", ID: "id", Name: "handle"},
		"x_alerts": {DataSource: "x_alerts", List: "alerts", ID: "id", Name: "display_name"},
	} {
		got, ok := InventorySource(schemas, ds)
		if !ok || got.DataSource != want.DataSource || got.List != want.List || got.ID != want.ID || got.Name != want.Name {
			t.Errorf("%s: got %+v %v, want %+v", ds, got, ok, want)
		}
	}
	for _, ds := range []string{"x_keyed", "x_two", "x_scoped", "x_none"} {
		if s, ok := InventorySource(schemas, ds); ok {
			t.Errorf("%s: suggested %+v", ds, s)
		}
	}
}

func TestFieldNumber(t *testing.T) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(`{"id": 130749611, "meta": {"n": 1.5}}`), &obj); err != nil {
		t.Fatal(err)
	}
	if got := inventory.Field(obj, "id"); got != "130749611" {
		t.Errorf("id = %q", got)
	}
	if got := inventory.Field(obj, "meta.n"); got != "1.5" {
		t.Errorf("meta.n = %q", got)
	}
}
