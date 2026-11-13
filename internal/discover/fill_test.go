package discover

import (
	"context"
	"testing"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// Inventory results of an excluded type aren't imported, but are kept in
// the report so generate can bring back the ones exported config names.
func TestFillHoldsExcludedTypes(t *testing.T) {
	d := Discoverer{Schemas: &tf.Schemas{}, Opts: Options{Exclude: map[string]bool{"x_nic": true}}}
	rep := &Report{Listed: map[string]int{}, Failed: map[string]string{}, Inventory: map[string]int{}}
	got, err := d.Fill(context.Background(), nil, rep, []model.Target{
		{Type: "x_nic", Name: "n", ID: "nic-1a", Origin: "inventory"},
		{Type: "x_disk", Name: "d", ID: "disk-1a", Origin: "inventory"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "x_disk" {
		t.Errorf("targets = %+v, want only x_disk", got)
	}
	if h := rep.Held["x_nic"]; len(h) != 1 || h[0].ID != "nic-1a" || h[0].Name != "" {
		t.Errorf("held = %+v", rep.Held)
	}
}
