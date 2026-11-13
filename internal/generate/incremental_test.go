package generate

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/askrike/desprawl/internal/model"
)

// A rerun regenerates the resources left out last time, those whose
// override changed (or went), and kept ones referencing either; a change to
// the targets regenerates everything.
func TestPartialRun(t *testing.T) {
	ws := model.Workspace{Dir: t.TempDir()}
	if err := os.MkdirAll(ws.StatePath(""), 0o755); err != nil {
		t.Fatal(err)
	}
	targets := []model.Target{
		{Type: "x_a", Name: "a", ID: "a"}, {Type: "x_b", Name: "b", ID: "b"},
		{Type: "x_c", Name: "c", ID: "c"}, {Type: "x_d", Name: "d", ID: "d"},
		{Type: "x_e", Name: "e", ID: "e"},
	}
	const config = `resource "x_a" "a" {
  name = "a"
}

resource "x_b" "b" {
  name = "b"
}

resource "x_c" "c" {
  peer = x_b.b.id
}

resource "x_e" "e" {
  name = "e"
}
`
	os.WriteFile(ws.StatePath(generatedFile), []byte(config), 0o644)
	os.WriteFile(ws.StatePath("plan.json"), []byte(`{"resource_changes":[]}`), 0o644)
	g := Generator{WS: ws}
	inputs := hash([]any{targets, g.Skip})
	old := map[string]Override{"x_b.b": {Remove: []string{"tags"}}, "x_e.e": {Remove: []string{"tags"}}}
	ws.WriteJSON(lastRunFile, lastRun{Inputs: inputs, Overrides: overrideHashes(old), Kept: []model.Target{targets[0], targets[1], targets[2], targets[4]}})
	ws.WriteJSON("left-out.json", []Dropped{{Target: targets[3], Reason: "error"}})

	now := map[string]Override{"x_b.b": {Remove: []string{"labels"}}} // b changed, e's went
	p, todo, err := g.partialRun(targets, now, inputs)
	if err != nil || p == nil {
		t.Fatalf("partial = %v, %v", p, err)
	}
	var got []string
	for _, t := range todo {
		got = append(got, t.Address())
	}
	slices.Sort(got)
	if want := []string{"x_b.b", "x_c.c", "x_d.d", "x_e.e"}; !slices.Equal(got, want) {
		t.Errorf("regenerate %v, want %v", got, want)
	}
	if len(p.kept) != 1 || p.kept[0].Address() != "x_a.a" || len(p.config.Body().Blocks()) != 1 {
		t.Errorf("kept %v", p.kept)
	}

	if p, _, _ := g.partialRun(targets, now, "other"); p != nil {
		t.Error("changed targets: partial run")
	}
	g.All = true
	if p, _, _ := g.partialRun(targets, now, inputs); p != nil {
		t.Error("-all: partial run")
	}
}

func TestMergePlans(t *testing.T) {
	base := `{"format_version":"1.2","resource_changes":[{"address":"a","v":1},{"address":"b","v":1}]}`
	update := `{"resource_changes":[{"address":"b","v":2},{"address":"dep","v":2}]}`
	out, err := mergePlans([]byte(base), []byte(update), func(a string) bool { return a == "b" })
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Format string `json:"format_version"`
		RC     []struct {
			Address string
			V       int
		} `json:"resource_changes"`
	}
	json.Unmarshal(out, &got)
	if got.Format != "1.2" || len(got.RC) != 2 || got.RC[0].Address != "a" || got.RC[1].Address != "b" || got.RC[1].V != 2 {
		t.Errorf("merged = %s", out)
	}
}
