package generate

import (
	"testing"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/model"
)

func TestVersionsIn(t *testing.T) {
	const src = `resource "x_service" "s" {
  task = "web:12"
  tags = { note = "web:11" }
  target {
    def_arn = "arn:x:r:1:def/web:10"
  }
  doc = jsonencode({ x = "web:9" })
}
`
	var older []model.Target
	for _, rev := range []float64{9, 10, 11, 12} {
		older = append(older, model.Target{Display: "web", Identity: map[string]any{"family": "web", "revision": rev}})
	}
	got := versionsIn([]byte(src), discover.NewVersions(map[string][]model.Target{"x_def": older}))
	revs := map[float64]bool{}
	for _, tg := range got {
		revs[tg.Identity["revision"].(float64)] = true
	}
	// Whole literal values only: a function call isn't evaluated.
	if len(revs) != 3 || !revs[10] || !revs[11] || !revs[12] {
		t.Errorf("got revisions %v, want 10 11 12", revs)
	}
}

func TestHeldIn(t *testing.T) {
	const src = `resource "x_route" "r" {
  network_interface_id = "nic-0abc123def"
  name                 = "default"
  ids                  = ["nic-0ff1ce2"]
}
`
	held := map[string][]model.Target{"x_nic": {
		{ID: "nic-0abc123def", Origin: "inventory"},
		{ID: "nic-0ff1ce2", Origin: "inventory"},
		{ID: "nic-0unused9", Origin: "inventory"},
		{ID: "default", Origin: "inventory"}, // a name, not an ID: never matched
	}}
	got := referencedIn([]byte(src), heldLookup(held))
	if len(got) != 2 || got[0].ID != "nic-0abc123def" || got[1].ID != "nic-0ff1ce2" || got[0].Type != "x_nic" {
		t.Errorf("got %+v, want the two referenced nics", got)
	}
}
