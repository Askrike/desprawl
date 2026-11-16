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

// A dropped attribute the import plan would change (labels the provider
// adds) goes into that resource's ignore_changes, once even when an
// override already put it there; one the plan leaves alone doesn't.
func TestDropIgnoresPlannedChange(t *testing.T) {
	plan, err := tf.ParsePlan([]byte(`{"resource_changes": [
	 {"address": "x_topic.a", "mode": "managed", "type": "x_topic", "change": {"actions": ["update"],
	  "before": {"id": "a", "all_labels": {}, "name": "a"}, "after": {"id": "a", "all_labels": {"added": "true"}, "name": "a"}}},
	 {"address": "x_topic.b", "mode": "managed", "type": "x_topic", "change": {"actions": ["no-op"],
	  "before": {"id": "b", "all_labels": {}}, "after": {"id": "b", "all_labels": {}}}},
	 {"address": "x_topic.c", "mode": "managed", "type": "x_topic", "change": {"actions": ["update"],
	  "before": {"id": "c", "all_labels": {}}, "after": {"id": "c", "all_labels": {"added": "true"}}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	ix, err := index.Build(plan, nil, rules.NewSet(nil))
	if err != nil {
		t.Fatal(err)
	}
	src := `resource "x_topic" "a" {
  name = "a"
}
resource "x_topic" "b" {
  name = "b"
}
resource "x_topic" "c" {
  name = "c"
  lifecycle {
    ignore_changes = [all_labels]
  }
}
`
	opts := Options{Rules: rules.NewSet(nil), Drop: []string{"all_labels"}}
	opts.changed, opts.imported = importPlan(plan)
	res, err := Link([]byte(src), ix, opts)
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Config)
	if strings.Count(out, "ignore_changes = [all_labels]") != 2 || strings.Count(out, "all_labels") != 2 {
		t.Errorf("x_topic.a and x_topic.c should ignore all_labels once each, x_topic.b not:\n%s", out)
	}
	if want := []string{"x_topic.a.all_labels", "x_topic.c.all_labels"}; strings.Join(res.Ignored, " ") != strings.Join(want, " ") {
		t.Errorf("Ignored = %v, want %v", res.Ignored, want)
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

// An inline block the pack says standalone resources manage is left out
// of a resource such a resource attaches to (by ID prefix), and its
// changes ignored; a resource nothing attaches to keeps it. So is an
// attribute config alone sets; a computed one keeps its value without.
func TestManagedDuplicates(t *testing.T) {
	plan, err := tf.ParsePlan([]byte(`{"resource_changes": [
	 {"address": "x_dataset.a", "mode": "managed", "type": "x_dataset", "change": {"before": {"id": "projects/p/datasets/a", "access": [{"role": "WRITER", "user": "w@x"}]}}},
	 {"address": "x_dataset.b", "mode": "managed", "type": "x_dataset", "change": {"before": {"id": "projects/p/datasets/b", "access": [{"role": "READER", "user": "r@x"}]}}},
	 {"address": "x_dataset_member.w", "mode": "managed", "type": "x_dataset_member", "change": {"before": {"id": "Projects/p/datasets/a/roles/editor/user:w@x", "member": "user:w@x"}}},
	 {"address": "x_table.t", "mode": "managed", "type": "x_table", "change": {"before": {"id": "t", "routes": ["r1"], "rules": ["s1"]}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	schemas := &tf.Schemas{Resources: map[string]*tf.SchemaBlock{
		"x_dataset": {},
		"x_table":   {Attributes: map[string]*tf.SchemaAttr{"routes": {Optional: true}, "rules": {Optional: true, Computed: true}}},
	}}
	ix, err := index.Build(plan, schemas, rules.NewSet(nil))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ManagedDuplicates(plan, ix, map[string][]string{"x_dataset.access": {"x_dataset_member"}})
	if err != nil {
		t.Fatal(err)
	}
	want := Duplicate{Addr: "x_dataset.a", Attr: "access", By: "x_dataset_member.w"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	src := `resource "x_dataset" "a" {
  name = "a"
  access {
    role = "WRITER"
  }
  access {
    role = "OWNER"
  }
}
resource "x_dataset" "b" {
  access {
    role = "READER"
  }
}
`
	src += `resource "x_table" "t" {
  routes = ["r1"]
  rules  = ["s1"]
}
`
	got = append(got, Duplicate{Addr: "x_table.t", Attr: "routes"}, Duplicate{Addr: "x_table.t", Attr: "rules"})
	opts := Options{Rules: rules.NewSet(nil), Duplicates: got, Schemas: schemas}
	opts.changed, opts.imported = importPlan(plan)
	res, err := Link([]byte(src), ix, opts)
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Config)
	a, b, _ := strings.Cut(out, `resource "x_dataset" "b"`)
	if strings.Contains(a, "access {") || !strings.Contains(a, "ignore_changes = [access]") || !strings.Contains(b, "access {") {
		t.Fatalf("config:\n%s", out)
	}
	if want := "x_dataset.a.access x_table.t.routes"; strings.Join(res.Ignored, " ") != want {
		t.Errorf("Ignored = %v, want %s", res.Ignored, want)
	}
}
