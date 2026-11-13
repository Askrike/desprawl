package discover

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// A list resource that can't see every scope (one without a scope
// argument) lists fewer objects than the inventory finds. The inventory's
// finds are kept when the parts their mapping named after identity
// attributes match no listed object; without such parts listing is
// trusted.
func TestFillKeepsWhatListingMissed(t *testing.T) {
	var s tf.Schemas
	if err := json.Unmarshal([]byte(`{
		"resources": {"x_rule": {"attributes": {"name": {"type": "string"}}}, "x_key": {}, "x_service": {}},
		"lists": {"x_rule": {}, "x_key": {}, "x_service": {}},
		"identities": {
			"x_rule": {"attributes": {"name": {"type": "string"}}},
			"x_key": {"attributes": {"id": {"type": "string"}}},
			"x_service": {"attributes": {
				"cluster": {"type": "string", "required_for_import": true},
				"name": {"type": "string", "required_for_import": true}
			}}
		}
	}`), &s); err != nil {
		t.Fatal(err)
	}
	d := Discoverer{Schemas: &s}
	rep := &Report{Listed: map[string]int{"x_rule": 1, "x_key": 1, "x_service": 1}, Failed: map[string]string{}, Inventory: map[string]int{}}
	listed := []model.Target{
		{Type: "x_rule", Name: "a", Identity: map[string]any{"name": "a", "region": "r1"}},
		{Type: "x_key", Name: "k1", Identity: map[string]any{"id": "k1"}},
		{Type: "x_service", Name: "s1", Identity: map[string]any{"cluster": "arn:x:cluster/c", "name": "s1"}},
	}
	got, err := d.Fill(context.Background(), listed, rep, []model.Target{
		{Type: "x_rule", ID: "a", Origin: "inventory", Attrs: map[string]string{"name": "a"}},
		{Type: "x_rule", ID: "bus/b", Origin: "inventory", Attrs: map[string]string{"name": "bus/b"}},
		// key_id isn't an identity attribute: can't tell k1 from k2.
		{Type: "x_key", ID: "k1", Origin: "inventory", Attrs: map[string]string{"key_id": "k1"}},
		{Type: "x_key", ID: "k2", Origin: "inventory", Attrs: map[string]string{"key_id": "k2"}},
		// Only cluster of the identity's cluster and name, spelled differently
		// (name, not ARN): comparing it alone would add s1 again.
		{Type: "x_service", ID: "c/s1", Origin: "inventory", Attrs: map[string]string{"cluster": "c", "service": "s1"}},
		{Type: "x_service", ID: "c/s2", Origin: "inventory", Attrs: map[string]string{"cluster": "c", "service": "s2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[3].ID != "bus/b" {
		t.Errorf("targets = %+v, want the listed three and bus/b", got)
	}
	if rep.Inventory["x_rule"] != 1 || rep.Inventory["x_key"] != 0 || rep.Inventory["x_service"] != 0 {
		t.Errorf("inventory counts = %v", rep.Inventory)
	}
}

// A parent found by inventory may lack an attribute its children are
// listed by; it is left out instead of failing the child type.
func TestBlocksLeaveOutParentsWithoutArgs(t *testing.T) {
	d := Discoverer{Schemas: &tf.Schemas{}}
	spec := ListArg{Type: "x_target", Parent: "x_rule", Args: map[string]string{"rule": "name", "bus": "bus_name"}}
	listed := map[string][]tf.ListResult{"x_rule": {{Type: "x_rule", Identity: map[string]any{"name": "a", "bus_name": "default"}}}}
	d.Added = []model.Target{{Type: "x_rule", ID: "b"}}
	got, err := d.blocks("x_target", spec, listed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["rule"] != "a" || got[0]["bus"] != "default" {
		t.Errorf("blocks = %v", got)
	}
	if _, err := d.blocks("x_target", spec, map[string][]tf.ListResult{}, nil); err == nil {
		t.Error("no parent has the attributes: want an error")
	}
}
