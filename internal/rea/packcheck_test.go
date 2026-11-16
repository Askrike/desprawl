package rea

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/inventory"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/tf"
	"github.com/askrike/desprawl/providers"
)

// A list spec scoped by a parameter no target carries yet is kept on a
// rerun as on a first discover, which reports the type as failed;
// one scoped by something that is no scope parameter is still invalid.
func TestCheckPackScopes(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(paths.ConfigFile(dir), []byte("provider \"x\" {\n  source = \"ns/x\"\n}\n"), 0o644)
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A rerun: the targets carry only the project.
	var targets []model.Target
	for _, n := range []string{"a", "b", "c"} {
		targets = append(targets, model.Target{Type: "x_bucket", Name: n, Identity: map[string]any{"project": "my-project", "name": n}})
	}
	w.WriteJSON("targets.json", targets)
	if w, err = Open(dir); err != nil {
		t.Fatal(err)
	}
	var schemas tf.Schemas
	if err := json.Unmarshal([]byte(`{
  "lists": {
    "x_job":    {"attributes": {"location": {"type": "string"}}},
    "x_bucket": {"attributes": {"project": {"type": "string"}}},
    "x_queue":  {"attributes": {"zone": {"type": "string"}}}
  },
  "identities": {
    "x_job":    {"attributes": {"project": {"type": "string"}, "location": {"type": "string"}, "name": {"type": "string"}}},
    "x_bucket": {"attributes": {"project": {"type": "string"}, "name": {"type": "string"}}}
  }
}`), &schemas); err != nil {
		t.Fatal(err)
	}
	pack := &providers.Pack{ListArgs: []discover.ListArg{
		{Type: "x_job", Scope: map[string]string{"location": "location"}},
		{Type: "x_bucket", Scope: map[string]string{"project": "project"}},
		{Type: "x_queue", Scope: map[string]string{"zone": "zone"}},
	}}
	out, groups := w.checkPack(&schemas, pack)
	var kept []string
	for _, la := range out.ListArgs {
		kept = append(kept, la.Type)
	}
	if len(kept) != 2 || kept[0] != "x_job" || kept[1] != "x_bucket" {
		t.Errorf("kept %v, want [x_job x_bucket]", kept)
	}
	if len(groups) != 1 || len(groups[0].Problems) != 1 {
		t.Fatalf("problems = %+v, want one: x_queue", groups)
	}
	// The workspace has no list_args.json of its own: the entry is the
	// shipped pack's, not one to look for in the workspace.
	if got, want := groups[0].Problems[0].Subject, "shipped pack x/list_args.json: x_queue"; got != want {
		t.Errorf("subject %q, want %q", got, want)
	}
	// One the workspace wrote is named by its path.
	own := filepath.Join(paths.Pack(dir), "x", "list_args.json")
	os.MkdirAll(filepath.Dir(own), 0o755)
	os.WriteFile(own, []byte(`[{"type": "x_queue", "scope": {"zone": "zone"}}]`), 0o644)
	_, groups = w.checkPack(&schemas, pack)
	if got, want := groups[0].Problems[0].Subject, own+": x_queue"; got != want {
		t.Errorf("subject %q, want %q", got, want)
	}
}

// Discover stamps the provider block reading a source on it; a shipped
// source read by any block is still the shipped one, so its partition and
// per-parent read aren't checked again.
func TestIsShipped(t *testing.T) {
	src := inventory.Source{DataSource: "x_search", List: "results", ID: "name"}
	shipped := &inventory.Inventory{Sources: []inventory.Source{src}}
	read := src
	read.Provider = "x.west"
	if !isShipped(shipped, read) {
		t.Error("a shipped source read by a provider block isn't recognised as shipped")
	}
	changed := read
	changed.List = "items"
	if isShipped(shipped, changed) {
		t.Error("a changed source is taken for the shipped one")
	}
}
