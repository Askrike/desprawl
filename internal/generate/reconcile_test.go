package generate

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// Reconcile sets drifting attributes to their imported values without a
// plan; when verify's plan rejects one, Unreconcile backs the resource's
// values out of the linked and generated config, ignoring defaulted paths
// instead.
func TestReconcileUndo(t *testing.T) {
	ws := model.Workspace{Dir: t.TempDir()}
	const src = `resource "x_q" "a" {
  name   = "a"
  mode   = null
  count_ = 3
}

resource "x_q" "b" {
  name = "b"
  mode = null
}
`
	if err := os.WriteFile(ws.Path(generatedFile), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	str, num := json.RawMessage(`"string"`), json.RawMessage(`"number"`)
	g := Generator{WS: ws, Schemas: &tf.Schemas{Resources: map[string]*tf.SchemaBlock{
		"x_q": {Attributes: map[string]*tf.SchemaAttr{
			"name": {Type: str, Required: true}, "mode": {Type: str, Optional: true}, "count_": {Type: num, Optional: true},
		}},
	}}}
	plan := `{"resource_changes": [
	  {"address": "x_q.a", "mode": "managed", "type": "x_q", "change": {"actions": ["update"],
	    "before": {"name": "a", "mode": "fast", "count_": 0}, "after": {"name": "a", "mode": "slow", "count_": 3}}},
	  {"address": "x_q.b", "mode": "managed", "type": "x_q", "change": {"actions": ["update"],
	    "before": {"name": "b", "mode": "fast"}, "after": {"name": "b", "mode": "slow"}}}]}`
	defaulted, err := defaultedPaths([]byte(plan))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.reconcile([]byte(plan), defaulted); err != nil {
		t.Fatal(err)
	}
	if len(defaulted["x_q.a"]) != 0 {
		t.Errorf("reconciled path still defaulted: %v", defaulted)
	}
	out, _ := os.ReadFile(ws.Path(generatedFile))
	if strings.Count(string(out), `"fast"`) != 2 || !strings.Contains(string(out), "count_ = 0") {
		t.Fatalf("not reconciled:\n%s", out)
	}
	// Generate moves generated.tf into .desprawl; link writes main.tf from it.
	if err := os.Rename(ws.Path(generatedFile), ws.StatePath(generatedFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws.Path("main.tf"), out, 0o644); err != nil {
		t.Fatal(err)
	}

	// A diagnostic by range (line 3 is in x_q.a), and one about a resource
	// reconcile didn't touch.
	diags := []tf.Diagnostic{
		{Summary: "Conflicting configuration arguments", Range: &tf.Range{Filename: "main.tf", Start: tf.Pos{Line: 3}}},
		{Summary: "Other", Address: "x_q.c"},
	}
	got, err := Unreconcile(ws, diags)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[x_q.a]" {
		t.Fatalf("undone %v, want [x_q.a]", got)
	}
	for _, file := range []string{ws.Path("main.tf"), ws.StatePath(generatedFile)} {
		b, _ := os.ReadFile(file)
		for _, want := range []string{"mode   = null", "count_ = 3", "ignore_changes = [count_]", `mode = "fast"`} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s: want %q in\n%s", file, want, b)
			}
		}
	}
	if again, err := Unreconcile(ws, diags); err != nil || len(again) != 0 {
		t.Errorf("undone twice: %v %v", again, err)
	}
}

// Attributes generated config has no line for (deprecated ones) are
// restored from the import, even on a resource that plans clean, except
// the id, empty values, and values another type's resource holds under the
// same name.
func TestReconcileRestoresLeftOut(t *testing.T) {
	ws := model.Workspace{Dir: t.TempDir()}
	const src = `resource "x_q" "a" {
  name = "a"
}

resource "x_q" "b" {
  name = "b"
}

resource "x_policy" "p" {
  doc = "{}"
}
`
	if err := os.WriteFile(ws.Path(generatedFile), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	str, set := json.RawMessage(`"string"`), json.RawMessage(`["set","string"]`)
	g := Generator{WS: ws, Schemas: &tf.Schemas{Resources: map[string]*tf.SchemaBlock{
		"x_q": {Attributes: map[string]*tf.SchemaAttr{
			"id": {Type: str, Optional: true, Computed: true}, "name": {Type: str, Required: true},
			"roles": {Type: set, Optional: true, Computed: true}, "flag": {Type: json.RawMessage(`"bool"`), Optional: true},
			"doc": {Type: str, Optional: true, Computed: true},
		}},
		"x_policy": {Attributes: map[string]*tf.SchemaAttr{"doc": {Type: str, Required: true}}},
	}}}
	a := `{"id": "1", "name": "a", "roles": ["r1", "r2"], "flag": false, "doc": "{\"x\":1}"}`
	b := `{"id": "2", "name": "b", "roles": [], "doc": null}`
	plan := `{"resource_changes": [
	  {"address": "x_q.a", "mode": "managed", "type": "x_q", "change": {"actions": ["no-op"], "before": ` + a + `, "after": ` + a + `}},
	  {"address": "x_q.b", "mode": "managed", "type": "x_q", "change": {"actions": ["no-op"], "before": ` + b + `, "after": ` + b + `}},
	  {"address": "x_policy.p", "mode": "managed", "type": "x_policy", "change": {"actions": ["no-op"],
	    "before": {"doc": "{\"x\":1}"}, "after": {"doc": "{\"x\":1}"}}}]}`
	if err := g.reconcile([]byte(plan), map[string][]string{}); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(ws.Path(generatedFile))
	if !strings.Contains(string(out), `roles = ["r1", "r2"]`) {
		t.Errorf("roles not restored:\n%s", out)
	}
	for _, not := range []string{"id", "flag", "roles = []", "doc = \"{\\\"x"} {
		if strings.Contains(string(out), not) {
			t.Errorf("%q restored:\n%s", not, out)
		}
	}
	var rec map[string][]Reconciled
	if err := ws.ReadJSON(reconciledFile, &rec); err != nil || fmt.Sprint(rec) != "map[x_q.a:[{roles  []}]]" {
		t.Errorf("reconciled = %v, %v", rec, err)
	}
}

// A plan that still fails after the back-out failed for another reason:
// Restore puts the reconciled values back, and edges in backed-out
// attributes leave links.json only while the back-out holds.
func TestBackoutRestores(t *testing.T) {
	ws := model.Workspace{Dir: t.TempDir()}
	const src = `resource "x_q" "a" {
  name = "live"
  role = x_role.missing.arn
  peer = x_q.b.id
}
`
	if err := os.WriteFile(ws.Path("main.tf"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteJSON(reconciledFile, map[string][]Reconciled{"x_q.a": {{Attr: "name"}, {Attr: "peer"}}}); err != nil {
		t.Fatal(err)
	}
	edges := `[{"from": "x_q.a", "path": "peer", "ref": "x_q.b.id"}, {"from": "x_q.a", "path": "role", "ref": "x_role.missing.arn"}]`
	if err := os.WriteFile(ws.StatePath(linksFile), []byte(edges), 0o644); err != nil {
		t.Fatal(err)
	}
	b := Backout{WS: ws}
	diags := []tf.Diagnostic{{Severity: "error", Summary: "Reference to undeclared resource", Address: "x_q.a"}}
	if got, err := b.Undo(diags); err != nil || len(got) != 1 {
		t.Fatalf("undo = %v, %v", got, err)
	}
	if got, _ := os.ReadFile(ws.Path("main.tf")); strings.Contains(string(got), "live") || strings.Contains(string(got), "peer") {
		t.Errorf("not backed out:\n%s", got)
	}
	var left []struct{ Path string }
	if err := ws.ReadJSON(linksFile, &left); err != nil || len(left) != 1 || left[0].Path != "role" {
		t.Errorf("links after back-out = %+v, %v", left, err)
	}
	if got, _ := b.Undo(diags); len(got) != 0 {
		t.Errorf("second undo = %v, want nothing left to back out", got)
	}
	if err := b.Restore(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(ws.Path("main.tf")); string(got) != src {
		t.Errorf("main.tf after restore:\n%s", got)
	}
	if got, _ := os.ReadFile(ws.StatePath(linksFile)); string(got) != edges {
		t.Errorf("links.json after restore: %s", got)
	}
	var rec map[string][]Reconciled
	if err := ws.ReadJSON(reconciledFile, &rec); err != nil || len(rec["x_q.a"]) != 2 {
		t.Errorf("reconciled after restore = %v, %v", rec, err)
	}
}
