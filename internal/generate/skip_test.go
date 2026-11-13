package generate

import (
	"testing"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

func TestSkipTargets(t *testing.T) {
	targets := []model.Target{
		{Type: "x_group", Name: "default", Identity: map[string]any{"name": "default.x1"}},
		{Type: "x_group", Name: "mine", Identity: map[string]any{"name": "mine"}},
		{Type: "x_key", Name: "k", Identity: map[string]any{"id": "k"}},
	}
	kept, dropped, err := skipTargets(targets, []Skip{
		{Type: "x_group", Attr: "name", Match: `^default\.`, Reason: "provider default"},
		{Type: "x_key", Attr: "description", Match: "^Default", Reason: "provider key"}, // not in identity: left for the plan
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 || len(dropped) != 1 || dropped[0].Name != "default" || dropped[0].Reason != skipReason+"provider default" {
		t.Errorf("kept %v, dropped %v", kept, dropped)
	}
}

func TestWithoutChanges(t *testing.T) {
	plan := []byte(`{"format_version": "1.2", "resource_changes": [
	 {"address": "x_key.aws", "type": "x_key"},
	 {"address": "x_key.mine", "type": "x_key"}
	]}`)
	out, err := withoutChanges(plan, map[string]string{"x_key.aws": "provider key"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := tf.ParsePlan(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ResourceChanges) != 1 || p.ResourceChanges[0].Address != "x_key.mine" {
		t.Errorf("resource changes = %+v, want only x_key.mine", p.ResourceChanges)
	}
}

func TestSkipped(t *testing.T) {
	plan := []byte(`{"resource_changes": [
	 {"address": "x_snapshot.auto", "type": "x_snapshot", "change": {"before": {"snapshot_type": "automated"}}},
	 {"address": "x_snapshot.manual", "type": "x_snapshot", "change": {"before": {"snapshot_type": "manual"}}},
	 {"address": "x_image.auto", "type": "x_image", "change": {"before": {"snapshot_type": "automated"}}}
	]}`)
	got, err := skipped(plan, []Skip{{Type: "x_snapshot", Attr: "snapshot_type", Match: "^automated$", Reason: "made by backups"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["x_snapshot.auto"] != "made by backups" {
		t.Errorf("skipped = %v, want only x_snapshot.auto", got)
	}
}
