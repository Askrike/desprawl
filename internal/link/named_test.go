package link

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
)

// A plain name in a free-form map links to the one resource it is the own
// identifier of; not in a map many types have (tags), not when another
// resource sets the same value, and not when it is short.
func TestLinkNamesInFreeFormMaps(t *testing.T) {
	var schemas tf.Schemas
	tags := `"tags": {"type": ["map", "string"], "optional": true}`
	if err := json.Unmarshal([]byte(`{"resources": {
	 "x_fn": {"attributes": {`+tags+`}, "block_types": {"env": {"block": {"attributes": {"vars": {"type": ["map", "string"], "optional": true}}}}}},
	 "x_table": {"attributes": {"name": {"type": "string", "required": true}, `+tags+`}},
	 "x_alarm": {"attributes": {"name": {"type": "string", "required": true}, `+tags+`}},
	 "x_a": {"attributes": {`+tags+`}},
	 "x_b": {"attributes": {`+tags+`}}
	}}`), &schemas); err != nil {
		t.Fatal(err)
	}
	plan, err := tf.ParsePlan([]byte(`{"resource_changes": [
	 {"address": "x_table.items", "mode": "managed", "type": "x_table", "change": {"before": {"id": "shop-items", "name": "shop-items"}}},
	 {"address": "x_table.short", "mode": "managed", "type": "x_table", "change": {"before": {"id": "items", "name": "items"}}},
	 {"address": "x_table.orders", "mode": "managed", "type": "x_table", "change": {"before": {"id": "shop-orders", "name": "shop-orders"}}},
	 {"address": "x_alarm.orders", "mode": "managed", "type": "x_alarm", "change": {"before": {"id": "a-1", "name": "shop-orders"}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	rs := rules.NewSet(nil)
	ix, err := index.Build(plan, &schemas, rs)
	if err != nil {
		t.Fatal(err)
	}
	src := `resource "x_fn" "worker" {
  tags = {
    Table = "shop-items"
  }
  env {
    vars = {
      ITEMS  = "shop-items"
      ORDERS = "shop-orders"
      SHORT  = "items"
    }
  }
}
`
	res, err := Link([]byte(src), ix, Options{Rules: rs, Schemas: &schemas})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Edges) != 1 || res.Edges[0].Path != "env.vars.ITEMS" || res.Edges[0].Ref != "x_table.items.id" || res.Edges[0].Confidence != Inferred {
		t.Fatalf("edges = %+v", res.Edges)
	}
	out := string(res.Config)
	if !strings.Contains(out, "x_table.items.id") || !strings.Contains(out, `Table = "shop-items"`) {
		t.Errorf("config:\n%s", out)
	}
}
