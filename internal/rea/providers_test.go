package rea

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/tf"
)

// A scope parameter the provider block's config sets (the setting its ref
// reads) is known even when no listed resource carries it; one that
// listing detects wins.
func TestConfigParams(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(paths.ConfigFile(dir), []byte(`provider "x" {
  source = "ns/x"
  config {
    project = "configured"
    region  = "north-1"
    zone    = "north-1a"
  }
}
`), 0o644)
	pack := filepath.Join(paths.Pack(dir), "x")
	os.MkdirAll(pack, 0o755)
	os.WriteFile(filepath.Join(pack, "pack.json"), []byte(`{"params": {
  "project":  {"ref": "data.x_client.current.project"},
  "region":   {"ref": "data.x_client.current.region"},
  "location": {"ref": "data.x_client.current.region"}
}}`), 0o644)
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Before the first discovery (no targets.json), the config is the scope.
	configured := map[string]string{"project": "configured", "region": "north-1", "location": "north-1"}
	if !maps.Equal(w.params, configured) {
		t.Errorf("Open params = %v, want %v", w.params, configured)
	}
	if s := w.scoped(w.Config.Providers[0]); !maps.Equal(s.params, configured) {
		t.Errorf("scoped params = %v, want %v", s.params, configured)
	}
	var targets []model.Target
	for _, n := range []string{"a", "b", "c"} {
		targets = append(targets, model.Target{Type: "x_bucket", Name: n, Identity: map[string]any{"project": "listed", "name": n}})
	}
	got := w.detectParams(targets)
	want := map[string]string{"project": "listed", "region": "north-1", "location": "north-1"}
	if !maps.Equal(got, want) {
		t.Errorf("params = %v, want %v (zone isn't a pack parameter)", got, want)
	}
}

// A failed type is printed on its own only when its error names a list
// argument (there's a fix to try); the others are counted on one line.
func TestListFailedCollapsed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(paths.ConfigFile(dir), []byte(`provider "x" {
  source = "ns/x"
}
`), 0o644)
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteJSON("targets.json", []model.Target{})
	w = w.scoped(w.Config.Providers[0])
	off := func(typ, path string) string {
		return `API Error [list.` + typ + `.q0] (main.tfquery.hcl:12): reading https://api.example/v1/p-1/` + path + `: Error 403: Widget API has not been used in project p-1 before. Details: [ {"@type": "info", "metadata": {"url": "https://x/` + path + `"}} ]`
	}
	w.WriteJSON("discovery.json", map[string]any{"failed": map[string]string{
		"x_widget":      off("x_widget", "widgets"),
		"x_widget_pool": off("x_widget_pool", "widgetPools"),
		"x_gadget":      "Error 403: Gadget API has not been used in project p-1 before.",
		"x_job":         "Missing required argument: location",
		"x_task":        "Missing required argument: location",
		// x_zoned's error quotes its own list argument.
		"x_queue":  "Bad value for 'tier'.",
		"x_stream": "Bad value for 'shards'.",
		"x_zoned":  "Bad value for 'zone'.",
	}})
	groups, err := w.discoverProblemsOf(&tf.Schemas{Lists: map[string]*tf.SchemaBlock{
		"x_zoned": {Attributes: map[string]*tf.SchemaAttr{"zone": {Type: json.RawMessage(`"string"`), Optional: true}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	PrintProblems(&text, groups, false)
	got := text.String()
	for _, want := range []string{
		"8 resource types failed to list",
		"  x_job\n",
		"  x_task\n",
		"  x_zoned\n",
		"  5 types with errors that name no argument: x_gadget, x_queue, x_stream, x_widget, x_widget_pool\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Widget API") {
		t.Errorf("an error that names no argument is printed:\n%s", got)
	}

}
