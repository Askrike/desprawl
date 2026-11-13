package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/askrike/desprawl/catalog"
	"github.com/askrike/desprawl/internal/tfmap"
)

// TestStaticPage checks `desprawl html` builds every example and its saved
// layout into the page.
func TestStaticPage(t *testing.T) {
	s := site{root: "../../examples", current: "aws-s3-sqs-lambda", load: tfmap.NewLoader(catalog.FS), layouts: tfmap.WorkspaceLayouts("../../examples", "../../layouts")}
	page, err := s.static()
	if err != nil {
		t.Fatal(err)
	}
	start := []byte("<script>window.DESPRAWL_STATIC = ")
	i := bytes.Index(page, start)
	if i < 0 {
		t.Fatal("no static data in the page")
	}
	rest := page[i+len(start):]
	var data struct {
		Dirs   struct{ Current string }
		Graphs map[string]*tfmap.Graph
	}
	if err := json.Unmarshal(rest[:bytes.Index(rest, []byte(";</script>"))], &data); err != nil {
		t.Fatal(err)
	}
	if data.Dirs.Current != "aws-s3-sqs-lambda" {
		t.Errorf("current = %q", data.Dirs.Current)
	}
	g := data.Graphs["aws-s3-sqs-lambda"]
	if g == nil || g.Layout == nil || len(g.Layout.Grid) == 0 || g.Seeded {
		t.Fatalf("aws-s3-sqs-lambda lacks its saved layout: %+v", g)
	}
	for _, e := range g.Arch.Edges {
		if e.Route == nil {
			t.Errorf("edge %s -> %s is not routed", e.From, e.To)
		}
	}
}
