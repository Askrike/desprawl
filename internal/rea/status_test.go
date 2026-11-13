package rea

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/paths"
)

// A stage whose input changed after it ran is out of date, and next says
// to run it; one that ran before runs were recorded counts as done.
func TestWorkspaceStages(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(paths.ConfigFile(dir), []byte("provider \"x\" {\n  source = \"ns/x\"\n}\n"), 0o644)
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Before terraform is set up (or after init failed), init is next.
	if v, err := ViewWorkspace(dir, time.Now()); err != nil || v.Stages[0].State != "not run" || !strings.Contains(v.Next, " init`") {
		t.Fatalf("before init: stages %+v, next %q, err %v", v.Stages, v.Next, err)
	}
	if err := w.WriteJSON("schema.json", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteJSON("targets.json", []model.Target{{Type: "x_bucket", Name: "logs", ID: "logs"}}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(paths.Overrides(dir), []byte("{}"), 0o644)
	w.recordStage("generate", time.Now().Add(-time.Minute), nil)

	v, err := ViewWorkspace(w.Dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]StageView{}
	for _, s := range v.Stages {
		states[s.Name] = s
	}
	if states["discover"].State != "done" || states["generate"].State != "done" || len(states["generate"].Changed) != 0 || states["link"].State != "not run" {
		t.Fatalf("stages = %+v", v.Stages)
	}
	if !strings.Contains(v.Next, " link`") {
		t.Errorf("next = %q, want link", v.Next)
	}

	os.WriteFile(paths.Overrides(dir), []byte(`{"x_bucket.logs": {"drop": true}}`), 0o644)
	v, _ = ViewWorkspace(w.Dir, time.Now())
	for _, s := range v.Stages {
		if s.Name == "generate" && (len(s.Changed) != 1 || s.Changed[0] != ".desprawl/overrides.json") {
			t.Errorf("generate changed = %v", s.Changed)
		}
	}
	if !strings.Contains(v.Next, " generate`") {
		t.Errorf("next = %q, want generate", v.Next)
	}
}

// Printing a fix copies the pack file it goes into into the workspace;
// until someone edits the copy, the stage that read it isn't out of date.
func TestPackCopyKeepsStageCurrent(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(paths.ConfigFile(dir), []byte("provider \"x\" {\n  source = \"ns/x\"\n}\n"), 0o644)
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := w.inputHash("pack/inventory.json")
	path, err := w.packFile("inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	if w.inputHash("pack/inventory.json") != before {
		t.Error("copying the pack file changed the stage's input")
	}
	os.WriteFile(path, []byte(`{"sources": []}`), 0o644)
	if w.inputHash("pack/inventory.json") == before {
		t.Error("editing the copy didn't change the stage's input")
	}
}
