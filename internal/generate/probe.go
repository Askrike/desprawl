package generate

import (
	"maps"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/inventory"
	"github.com/askrike/desprawl/internal/model"
)

// probeOrigin is the Origin of targets generate probes for (see probes).
const probeOrigin = "probe"

// maxProbes bounds the types one ID is probed as.
const maxProbes = 10

// probes are the targets to try at the IDs the generating plan read
// (ids, by address): for each resource it imported, the types that can't
// be listed whose documented import ID is spelled like that resource's
// type's own (inventory.SameID), such as an association named by the
// object it attaches to. Nothing lists those and no inventory returns
// them; the plan decides which exist, and a probe that gets no config is
// left out without being reported as dropped.
func (g Generator) probes(targets []model.Target, ids map[string]string) []model.Target {
	if len(g.Docs) == 0 {
		return nil
	}
	key := func(typ, id string) string { return typ + "\x00" + strings.ToLower(id) }
	have := map[string]bool{}
	for _, t := range targets {
		if t.ID != "" {
			have[key(t.Type, t.ID)] = true
		}
	}
	var out []model.Target
	for _, t := range targets {
		id := ids[t.Address()]
		if id == "" {
			continue
		}
		for _, typ := range inventory.SameID(id, t.Type, g.Docs, maxProbes) {
			if g.Schemas.Lists[typ] != nil || g.Schemas.Resources[typ] == nil || g.Exclude[typ] || have[key(typ, id)] {
				continue
			}
			have[key(typ, id)] = true
			// Named after the object it shares the ID with.
			out = append(out, model.Target{Type: typ, ID: id, Display: t.Name, Provider: t.Provider, Origin: probeOrigin})
		}
	}
	return out
}

// unlinked are the probes in generated config (src) that link their
// object to nothing its own config doesn't: none of their identifiers
// (index.IDShaped) other than the shared ID is missing from the config of
// the target the plan read that ID for (ids, by address). An association
// names what it attaches; another flavour of the same object, or settings
// that exist for every such object, add nothing and would be duplicates.
func unlinked(src []byte, targets []model.Target, ids map[string]string) map[string]bool {
	strs := resourceStrings(src)
	out := map[string]bool{}
	for _, p := range targets {
		if p.Origin != probeOrigin || strs[p.Address()] == nil {
			continue
		}
		own := map[string]bool{}
		for _, t := range targets {
			if t.Origin != probeOrigin && strings.EqualFold(ids[t.Address()], p.ID) {
				maps.Copy(own, strs[t.Address()])
			}
		}
		links := false
		for v := range strs[p.Address()] {
			if index.IDShaped(v) && !strings.EqualFold(v, p.ID) && !own[v] {
				links = true
				break
			}
		}
		if !links {
			out[p.Address()] = true
		}
	}
	return out
}

// resourceStrings are the literal strings in each resource block of src,
// nested blocks and collections included, by address.
func resourceStrings(src []byte) map[string]map[string]bool {
	f, diags := hclsyntax.ParseConfig(src, generatedFile, hcl.InitialPos)
	if diags.HasErrors() {
		return nil
	}
	out := map[string]map[string]bool{}
	var walk func(b *hclsyntax.Body, into map[string]bool)
	walk = func(b *hclsyntax.Body, into map[string]bool) {
		for _, a := range b.Attributes {
			v, d := a.Expr.Value(nil)
			if d.HasErrors() {
				continue
			}
			cty.Walk(v, func(_ cty.Path, v cty.Value) (bool, error) {
				if v.IsKnown() && !v.IsNull() && v.Type() == cty.String {
					into[v.AsString()] = true
				}
				return true, nil
			})
		}
		for _, nb := range b.Blocks {
			walk(nb.Body, into)
		}
	}
	for _, b := range f.Body.(*hclsyntax.Body).Blocks {
		if b.Type == "resource" && len(b.Labels) == 2 {
			strs := map[string]bool{}
			walk(b.Body, strs)
			out[b.Labels[0]+"."+b.Labels[1]] = strs
		}
	}
	return out
}
