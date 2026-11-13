package link

import (
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
)

// A type-scoped drop (inline rules that standalone rule resources also
// manage) removes the attribute from that type only, before linking, so it
// makes no edge.
func TestDropForOneType(t *testing.T) {
	plan, err := tf.ParsePlan([]byte(`{"resource_changes": [
	 {"address": "x_net.n", "mode": "managed", "type": "x_net", "change": {"before": {"id": "net-0123456789"}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	ix, err := index.Build(plan, nil, rules.NewSet(nil))
	if err != nil {
		t.Fatal(err)
	}
	src := `resource "x_group" "g" {
  inline = "net-0123456789"
  net    = "net-0123456789"
}
resource "x_other" "o" {
  inline = "net-0123456789"
}
`
	res, err := Link([]byte(src), ix, Options{Rules: rules.NewSet(nil), Drop: []string{"x_group.inline"}})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Config)
	if strings.Count(out, "inline") != 1 || !strings.Contains(out, "inline = x_net.n.id") {
		t.Errorf("inline should be gone from x_group only:\n%s", out)
	}
	for _, e := range res.Edges {
		if e.From == "x_group.g" && e.Path == "inline" {
			t.Errorf("dropped attribute made an edge: %+v", e)
		}
	}
}

// An optional, computed attribute that a standalone resource attached to
// the same resource requires (equal JSON, any spelling) is dropped from
// that resource only; one no standalone resource holds stays, and so does
// a plain string both hold (a name).
func TestDuplicates(t *testing.T) {
	plan, err := tf.ParsePlan([]byte(`{"resource_changes": [
	 {"address": "x_queue.a", "mode": "managed", "type": "x_queue", "change": {"before": {"id": "https://q/a1", "url": "https://q/a1", "name": "queue-a", "policy": "{\"b\":1,\"a\":2}"}}},
	 {"address": "x_queue.b", "mode": "managed", "type": "x_queue", "change": {"before": {"id": "https://q/b1", "name": "queue-b", "policy": "{\"a\":3}"}}},
	 {"address": "x_queue_policy.a", "mode": "managed", "type": "x_queue_policy", "change": {"before": {"id": "https://q/a1", "queue": "https://q/a1", "name": "queue-a", "policy": "{\"a\": 2, \"b\": 1}"}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	schemas := &tf.Schemas{Resources: map[string]*tf.SchemaBlock{
		"x_queue": {Attributes: map[string]*tf.SchemaAttr{
			"id": {Computed: true}, "url": {Computed: true, Type: []byte(`"string"`)},
			"name": {Optional: true, Computed: true}, "policy": {Optional: true, Computed: true},
		}},
		"x_queue_policy": {Attributes: map[string]*tf.SchemaAttr{
			"id": {Computed: true}, "queue": {Required: true}, "name": {Required: true}, "policy": {Required: true},
		}},
	}}
	ix, err := index.Build(plan, schemas, rules.NewSet(nil))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Duplicates(plan, schemas, ix)
	if err != nil {
		t.Fatal(err)
	}
	want := []Duplicate{{Addr: "x_queue.a", Attr: "policy", By: "x_queue_policy.a"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	src := `resource "x_queue" "a" {
  policy = "{\"b\":1,\"a\":2}"
}
resource "x_queue" "b" {
  policy = "{\"a\":3}"
}
`
	res, err := Link([]byte(src), ix, Options{Rules: rules.NewSet(nil), Duplicates: got})
	if err != nil {
		t.Fatal(err)
	}
	if out := string(res.Config); strings.Count(out, "policy") != 1 {
		t.Errorf("only x_queue.a's policy should be gone:\n%s", out)
	}
}
