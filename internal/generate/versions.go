package generate

import (
	"os"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/model"
)

// referencedVersions finds older versions dropped by discovery that the
// generated config still names ("web:12" while web:14 is the latest): a
// service pinned to an old revision; and held-back targets of excluded
// types it names by ID (a route's network interface). They are imported
// too, so the reference can be linked instead of left as a literal.
func (g Generator) referencedVersions() ([]model.Target, error) {
	var rep discover.Report
	if err := g.WS.ReadJSON("discovery.json", &rep); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(rep.Superseded) == 0 && len(rep.Held) == 0 {
		return nil, nil
	}
	versions := discover.NewVersions(rep.Superseded)
	src, err := os.ReadFile(g.WS.Path(generatedFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return referencedIn(src, func(s string) (model.Target, bool) {
		if t, ok := versions.Lookup(s); ok {
			return t, true
		}
		return heldLookup(rep.Held)(s)
	}), nil
}

// heldLookup finds the held-back targets (inventory results of types the
// pack excludes) by import ID. Only IDs shaped like generated identifiers
// count: a name ("default") turns up in config for other reasons.
func heldLookup(held map[string][]model.Target) func(string) (model.Target, bool) {
	byID := map[string]model.Target{}
	dup := map[string]bool{}
	for typ, ts := range held {
		for _, t := range ts {
			t.Type = typ
			if t.ID == "" || !index.IDShaped(t.ID) {
				continue
			}
			if o, ok := byID[t.ID]; ok && o.Key() != t.Key() {
				dup[t.ID] = true
			}
			byID[t.ID] = t
		}
	}
	return func(s string) (model.Target, bool) {
		t, ok := byID[s]
		return t, ok && !dup[s]
	}
}

// versionsIn returns the versions named by whole literal string values in
// config source.
func versionsIn(src []byte, versions *discover.Versions) []model.Target {
	return referencedIn(src, versions.Lookup)
}

// referencedIn returns the targets lookup finds for whole literal string
// values in config source.
func referencedIn(src []byte, lookup func(string) (model.Target, bool)) []model.Target {
	f, _ := hclsyntax.ParseConfig(src, generatedFile, hcl.InitialPos)
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}
	found := map[string]model.Target{}
	var walk func(b *hclsyntax.Body)
	walk = func(b *hclsyntax.Body) {
		for _, a := range b.Attributes {
			v, diags := a.Expr.Value(nil)
			if diags.HasErrors() {
				continue
			}
			eachString(v, func(s string) {
				if t, ok := lookup(s); ok {
					found[t.Key()] = t
				}
			})
		}
		for _, blk := range b.Blocks {
			walk(blk.Body)
		}
	}
	walk(body)
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]model.Target, 0, len(keys))
	for _, k := range keys {
		out = append(out, found[k])
	}
	return out
}

func eachString(v cty.Value, fn func(string)) {
	if v.IsNull() || !v.IsKnown() {
		return
	}
	ty := v.Type()
	switch {
	case ty == cty.String:
		fn(v.AsString())
	case ty.IsListType() || ty.IsSetType() || ty.IsTupleType() || ty.IsMapType() || ty.IsObjectType():
		for it := v.ElementIterator(); it.Next(); {
			_, e := it.Element()
			eachString(e, fn)
		}
	}
}
