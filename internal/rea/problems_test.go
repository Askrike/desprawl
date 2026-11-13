package rea

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/generate"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/verify"
)

// A stage's problems are grouped, explain how to fix them, carry the
// suggested fix as it goes into the file, and leave out what was done on
// purpose or is harmless.
func TestProblems(t *testing.T) {
	w := Workspace{Workspace: model.Workspace{Dir: t.TempDir()}}
	if err := w.WriteJSON("left-out.json", []generate.Dropped{
		{Target: model.Target{Type: "x_fn", Name: "api"}, Reason: `"code" is required`,
			Hint: &generate.Hint{Set: []string{"name"}, Override: &generate.Override{Set: map[string]string{"code": `"placeholder"`}}, Why: "set a placeholder"}},
		{Target: model.Target{Type: "x_fn", Name: "old"}, Reason: "dropped by override", Intended: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteJSON("verify.json", []verify.Drift{
		{Address: "x_bucket.logs", Actions: []string{"update"}, Paths: []string{"tags.env"}},
		{Address: "x_bucket.tmp", Actions: []string{"update"}, Paths: []string{"force_destroy"}, ConfigOnly: true},
	}); err != nil {
		t.Fatal(err)
	}
	gen, err := w.GenerateProblems()
	if err != nil {
		t.Fatal(err)
	}
	ver, err := w.VerifyProblems()
	if err != nil {
		t.Fatal(err)
	}
	groups := append(gen, ver...)

	var text bytes.Buffer
	PrintProblems(&text, groups, false)
	for _, want := range []string{
		"1 resource was left out of the generated config.",
		"overrides.json, then run `desprawl -C " + w.Dir + " generate`",
		`"x_fn.api": {"set":{"code":"\"placeholder\""}}`,
		"1 resource's config doesn't quite match what's in the cloud yet.",
		"Nothing in the cloud changed",
		"differs from the cloud: tags.env",
		`"x_bucket.logs": {"ignore_changes":["tags"]}`,
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("missing %q in:\n%s", want, text.String())
		}
	}
	// Plan actions read as if something were about to change.
	for _, unwanted := range []string{"x_fn.old", "x_bucket.tmp", "update", "drop"} {
		if strings.Contains(text.String(), unwanted) {
			t.Errorf("%s printed:\n%s", unwanted, text.String())
		}
	}

	var lines bytes.Buffer
	PrintProblems(&lines, groups, true)
	var first struct {
		Kind, Subject, Key string
		Fix                map[string]any
	}
	if err := json.Unmarshal([]byte(strings.Split(lines.String(), "\n")[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.Kind != "not-generated" || first.Key != "x_fn.api" || first.Fix["set"] == nil {
		t.Errorf("first JSON line = %+v", first)
	}
}
