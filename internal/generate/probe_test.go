package generate

import (
	"os"
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// An imported subnet's ID is probed as the unlistable types documented
// with the same import ID, except listable, excluded or already targeted
// ones; a probe that gets no config is left out silently.
func TestProbes(t *testing.T) {
	const doc = "/subs/0000/rg/group1/providers/Net/nets/net1/subnets/sub1"
	block := &tf.SchemaBlock{}
	g := Generator{
		Schemas: &tf.Schemas{
			Resources: map[string]*tf.SchemaBlock{"x_subnet": block, "x_nsg_assoc": block, "x_rt_assoc": block, "x_listed": block, "x_excluded": block, "x_had": block},
			Lists:     map[string]*tf.SchemaBlock{"x_subnet": block, "x_listed": block},
		},
		Docs: map[string][]string{
			"x_subnet": {doc}, "x_nsg_assoc": {doc}, "x_rt_assoc": {doc}, "x_listed": {doc}, "x_excluded": {doc}, "x_had": {doc},
			"x_peering": {"/subs/0000/rg/group1/providers/Net/nets/net1/peerings/p1"},
		},
		Exclude: map[string]bool{"x_excluded": true},
	}
	id := "/subs/abcd/rg/main/providers/Net/nets/vnet/subnets/data"
	targets := []model.Target{
		{Type: "x_subnet", Name: "data", Identity: map[string]any{"name": "data"}},
		{Type: "x_had", Name: "data", ID: id},
	}
	got := g.probes(targets, map[string]string{"x_subnet.data": id, "x_had.data": id})
	if len(got) != 2 || got[0].Type != "x_nsg_assoc" || got[1].Type != "x_rt_assoc" || got[0].ID != id || got[0].Display != "data" || got[0].Origin != probeOrigin {
		t.Fatalf("probes = %+v", got)
	}

}

// Of the probes, the one that imports and links its object to something
// new is kept; one that imports but adds nothing (settings every such
// object has) is blanked out of the config, and one that didn't import is
// left out; none is reported as dropped.
func TestDropProbes(t *testing.T) {
	const id = "/subs/abcd/rg/main/providers/Net/nets/vnet/subnets/data"
	const gen = `resource "x_subnet" "data" {
  name    = "data"
  address = "10.0.1.0/24"
}

resource "x_nsg_assoc" "data" {
  subnet_id = "/subs/abcd/rg/main/providers/Net/nets/vnet/subnets/data"
  nsg_id    = "/subs/abcd/rg/main/providers/Net/nsgs/data"
}

resource "x_settings" "data" {
  subnet_id = "/subs/abcd/rg/main/providers/Net/nets/vnet/subnets/data"
  logging {
    enabled = true
  }
}
`
	ws := model.Workspace{Dir: t.TempDir()}
	if err := os.WriteFile(ws.Path(generatedFile), []byte(gen), 0o644); err != nil {
		t.Fatal(err)
	}
	g := Generator{WS: ws}
	targets := []model.Target{{Type: "x_subnet", Name: "data", Identity: map[string]any{"name": "data"}}}
	for _, typ := range []string{"x_nsg_assoc", "x_settings", "x_rt_assoc"} {
		targets = append(targets, model.Target{Type: typ, Name: "data", ID: id, Origin: probeOrigin})
	}
	ids := map[string]string{"x_subnet.data": id, "x_nsg_assoc.data": id, "x_settings.data": id}
	diags := []tf.Diagnostic{
		{Summary: "Cannot import non-existent remote object"},
		{Summary: "Invalid value", Range: &tf.Range{Filename: generatedFile, Start: tf.Pos{Line: 13}}}, // in x_settings
		{Summary: "Invalid value", Range: &tf.Range{Filename: generatedFile, Start: tf.Pos{Line: 3}}},  // in x_subnet
	}
	kept, dropped, rest, err := g.dropUnconfigured(targets, diags, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 || kept[1].Type != "x_nsg_assoc" || len(dropped) != 0 {
		t.Errorf("dropUnconfigured kept %v, dropped %v", kept, dropped)
	}
	if len(rest) != 1 || rest[0].Range.Start.Line != 3 {
		t.Errorf("diagnostics left: %v", rest)
	}
	src, err := os.ReadFile(ws.Path(generatedFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "x_settings") || !strings.Contains(string(src), "x_nsg_assoc") || strings.Count(string(src), "\n") != strings.Count(gen, "\n") {
		t.Errorf("generated config:\n%s", src)
	}
}
