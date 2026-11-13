package discover

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// A child type that failed keeps its failure when Fill lists the children
// of only some of its parents (those a list missed), and loses it when Fill
// lists them all (its parent type failed: every parent is new).
func TestFillKeepsChildFailures(t *testing.T) {
	bin := t.TempDir()
	needShell(t)
	script := `#!/bin/sh
q="${1#-chdir=}/main.tfquery.hcl"
if grep -q 'list "x_c"' "$q"; then
  echo '{"type":"list_resource_found","list_resource_found":{"address":"list.x_c.q0","resource_type":"x_c","display_name":"c2","identity":{"id":"c2"}}}'
  echo '{"type":"list_complete","list_complete":{"address":"list.x_c.q0","resource_type":"x_c","total":1}}'
fi
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	plain := &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{}}
	schemas := &tf.Schemas{
		Lists:     map[string]*tf.SchemaBlock{"x_p": plain, "x_c": plain},
		Resources: map[string]*tf.SchemaBlock{"x_p": {Attributes: map[string]*tf.SchemaAttr{"name": {}}}, "x_c": {}},
	}
	if err := json.Unmarshal([]byte(`{"x_p": {"attributes": {"name": {"type": "string", "required_for_import": true}}}}`), &schemas.Identities); err != nil {
		t.Fatal(err)
	}
	fill := func(parentFailed bool) *Report {
		d := Discoverer{
			WS: model.Workspace{Dir: t.TempDir()}, TF: tf.Runner{}, Schemas: schemas,
			Specs: []ListArg{{Type: "x_c", Parent: "x_p", Args: map[string]string{"parent": "name"}}},
			Opts:  Options{Parallel: 1, Timeout: 5 * time.Second, Limit: 100},
		}
		rep := &Report{Listed: map[string]int{}, Failed: map[string]string{"x_c": "denied"}, Inventory: map[string]int{}}
		var found []model.Target
		if parentFailed {
			rep.Failed["x_p"] = "denied"
		} else {
			rep.Listed["x_p"] = 1
			found = []model.Target{{Type: "x_p", Name: "p1", Identity: map[string]any{"name": "p1"}}}
		}
		extra := []model.Target{
			{Type: "x_p", ID: "p1", Origin: "inventory", Attrs: map[string]string{"name": "p1"}},
			{Type: "x_p", ID: "p2", Origin: "inventory", Attrs: map[string]string{"name": "p2"}},
		}
		if _, err := d.Fill(context.Background(), found, rep, extra); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	if rep := fill(false); rep.Failed["x_c"] != "denied" || rep.Listed["x_c"] != 1 {
		t.Errorf("parent listed, one missed: failed %v, listed %v; want x_c still failed", rep.Failed, rep.Listed)
	}
	if rep := fill(true); rep.Failed["x_c"] != "" || rep.Listed["x_c"] != 1 {
		t.Errorf("parent failed: failed %v, listed %v; want x_c listed", rep.Failed, rep.Listed)
	}
}
