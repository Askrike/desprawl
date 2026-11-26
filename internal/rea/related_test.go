package rea

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/mine"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
	"github.com/askrike/desprawl/providers"
)

// Unlistable types a rule connects to a found type, or an imported value
// names, are listed first: those connected to the most found resources
// first. Unconnected ones, found ones and rejected rules don't count.
func TestRelatedUnlistable(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(paths.ConfigFile(dir), []byte("provider \"x\" {\n  source = \"ns/x\"\n}\n"), 0o644)
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteJSON("targets.json", []model.Target{
		{Type: "x_alert", Name: "a", ID: "1"}, {Type: "x_alert", Name: "b", ID: "2"},
		{Type: "x_board", Name: "main", ID: "m"},
	})
	os.WriteFile(w.StatePath("plan.json"), []byte(`{"resource_changes": [
		{"address": "x_alert.a", "type": "x_alert", "change": {"after": {"message": "down: notify @hook-oncall", "tags": ["service:api"]}}}
	]}`), 0o644)
	res := map[string]*tf.SchemaBlock{}
	for _, typ := range []string{"x_alert", "x_board", "x_board_folder", "x_alert_mute", "x_hook", "x_service", "x_orphan", "x_other"} {
		res[typ] = &tf.SchemaBlock{}
	}
	pk := &providers.Pack{
		Rules: []rules.Rule{
			{Source: "x_board_folder", Path: "item.board_id", Target: "x_board", Attr: "id"},
			{Source: "x_alert_mute", Path: "alert_id", Target: "x_alert", Attr: "id"},
			{Source: "x_alert", Path: "board", Target: "x_board", Attr: "id"},     // both found
			{Source: "x_other", Path: "alert", Target: "x_alert", Rejected: true}, // rejected
		},
		Imports: map[string]*mine.ImportExamples{"x_alert_mute": {IDs: []string{"mute-1"}}},
	}
	unlistable := []string{"x_alert", "x_alert_mute", "x_board", "x_board_folder", "x_hook", "x_orphan", "x_other", "x_service"}
	g := w.relatedUnlistable(&tf.Schemas{Resources: res}, pk, unlistable)
	var got []string
	for _, p := range g.Problems {
		got = append(got, p.Subject)
	}
	// x_hook: "@hook-oncall" names it, but "hook" is under 5 letters.
	// x_service: "service:api" is a tag, not a handle.
	if want := []string{"x_alert_mute", "x_board_folder"}; !slices.Equal(got, want) {
		t.Fatalf("related = %v, want %v", got, want)
	}
	if d := strings.Join(g.Problems[0].Detail, "\n"); !strings.Contains(d, "its alert_id can name x_alert (2 found)") || !strings.Contains(d, "mute-1") {
		t.Errorf("x_alert_mute detail = %q", d)
	}

	// A longer word names a type when it starts a handle.
	res["x_webhook"] = &tf.SchemaBlock{}
	os.WriteFile(w.StatePath("plan.json"), []byte(`{"resource_changes": [
		{"address": "x_alert.a", "type": "x_alert", "change": {"after": {"message": "notify @webhook-oncall", "note": "a webhooks-x and xwebhook-y"}}}
	]}`), 0o644)
	g = w.relatedUnlistable(&tf.Schemas{Resources: res}, pk, append(unlistable, "x_webhook"))
	var hook *Problem
	for i, p := range g.Problems {
		if p.Subject == "x_webhook" {
			hook = &g.Problems[i]
		}
	}
	if hook == nil || !strings.Contains(hook.Detail[0], `x_alert.a "notify @webhook-oncall"`) {
		t.Errorf("x_webhook = %+v", hook)
	}
}

func TestHandleAt(t *testing.T) {
	for s, want := range map[string]int{
		"@webhook-pager":    1,
		"webhook-pager":     0,
		"webhooks-pager":    -1,
		"xwebhook-pager":    -1,
		"webhook:pager":     -1,
		"a webhook":         -1,
		"xwebhook webhook-": 9,
	} {
		if got := handleAt(s, "webhook"); got != want {
			t.Errorf("handleAt(%q) = %d, want %d", s, got, want)
		}
	}
}
