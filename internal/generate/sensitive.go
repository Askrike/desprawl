package generate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
)

// sensitiveRef is a sensitive string attribute whose whole value is the
// identifier another resource owns. Value is never written anywhere: config
// gets Owner's reference.
type sensitiveRef struct {
	Address, Attr, Value string
	Owner                index.Ref
}

// sensitiveIdentifiers finds top-level sensitive string attributes whose
// value is exactly an identifier uniquely owned by a different resource. A
// parameter store entry holding a network id is marked sensitive because
// the attribute is, but the value is no secret: it is already in the
// owner's own, unscrubbed attributes. Values that match nothing are never
// returned.
//
// The index is built from the scrubbed plan, so sensitive values can only
// match what isn't sensitive. In a partial run prior, the last run's
// scrubbed plan, adds the resources kept from it: an owner may be one.
func sensitiveIdentifiers(raw, prior []byte, schemas *tf.Schemas) ([]sensitiveRef, error) {
	scrubbed, err := Scrub(raw)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		fresh, err := tf.ParsePlan(scrubbed)
		if err != nil {
			return nil, err
		}
		inRaw := map[string]bool{}
		for _, rc := range fresh.ResourceChanges {
			inRaw[rc.Address] = true
		}
		if scrubbed, err = mergePlans(prior, scrubbed, func(a string) bool { return inRaw[a] }); err != nil {
			return nil, err
		}
	}
	scrubbedPlan, err := tf.ParsePlan(scrubbed)
	if err != nil {
		return nil, err
	}
	ix, err := index.Build(scrubbedPlan, schemas, rules.NewSet(nil))
	if err != nil {
		return nil, err
	}
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Mode    string `json:"mode"`
			Type    string `json:"type"`
			Change  struct {
				Before          map[string]any `json:"before"`
				BeforeSensitive map[string]any `json:"before_sensitive"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, err
	}
	var out []sensitiveRef
	for _, rc := range plan.ResourceChanges {
		if rc.Mode != "managed" {
			continue
		}
		for attr, marker := range rc.Change.BeforeSensitive {
			if m, _ := marker.(bool); !m {
				continue
			}
			v, _ := rc.Change.Before[attr].(string)
			if v == "" || !ix.Unique(v) || ix.IsAlias(v) {
				// An alias is a shortened spelling: the owner's reference
				// would evaluate to the full one.
				continue
			}
			var owner index.Ref
			owned := false
			for _, r := range ix.Lookup(v) {
				if r.Address == rc.Address {
					owned = true
				} else if owner.Address == "" {
					owner = r
				}
			}
			if owned || owner.Address == "" {
				continue
			}
			out = append(out, sensitiveRef{rc.Address, attr, v, owner})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Address != out[j].Address {
			return out[i].Address < out[j].Address
		}
		return out[i].Attr < out[j].Attr
	})
	return out, nil
}

// restoreIdentifiers sets identifier-valued sensitive attributes in the
// generated config (replacing a placeholder, if one stands there) to a
// reference to their owner, and keeps them only if the plan still passes:
// the reference evaluates to the live value, which itself is never
// written. `link` records each as an edge. It returns the plan to use from
// here on.
func (g Generator) restoreIdentifiers(ctx context.Context, targets []model.Target, plan []byte) ([]byte, error) {
	var prior []byte
	if g.partial != nil {
		prior = g.partial.plan
	}
	refs, err := sensitiveIdentifiers(plan, prior, g.Schemas)
	if err != nil || len(refs) == 0 {
		return plan, err
	}
	backup, err := os.ReadFile(g.WS.Path(generatedFile))
	if err != nil {
		return nil, err
	}
	_, f, err := g.readGenerated()
	if err != nil {
		return nil, err
	}
	n := 0
	for _, b := range f.Body().Blocks() {
		for _, r := range refs {
			if hclx.Address(b) != r.Address {
				continue
			}
			if s := g.Schemas.Resources[b.Labels()[0]]; s == nil || s.Attributes[r.Attr] == nil || (!s.Attributes[r.Attr].Optional && !s.Attributes[r.Attr].Required) {
				continue
			}
			t, diags := hclsyntax.ParseTraversalAbs([]byte(r.Owner.String()), "", hcl.InitialPos)
			if diags.HasErrors() {
				continue
			}
			b.Body().SetAttributeTraversal(r.Attr, t)
			n++
		}
	}
	if n == 0 {
		return plan, nil
	}
	if err := os.WriteFile(g.WS.Path(generatedFile), f.Bytes(), 0o644); err != nil {
		return nil, err
	}
	g.TF.Progress.Step(fmt.Sprintf("plan referencing the owners of %d sensitive values", n), 0)
	diags, _, err := g.plan(ctx, targets, "-out="+g.planFile())
	if err == nil && len(diags) == 0 {
		g.Log.Verbose("referenced the owners of %d sensitive values that are another resource's identifier", n)
		return g.TF.ShowJSON(ctx, g.planFile())
	}
	// Terraform didn't accept it: back out. (The reference evaluates to the
	// matched value; verify catches any drift.)
	if err := os.WriteFile(g.WS.Path(generatedFile), backup, 0o644); err != nil {
		return nil, err
	}
	g.TF.Progress.Step("plan without the sensitive-value references", 0)
	if diags, _, err := g.plan(ctx, targets, "-out="+g.planFile()); err != nil || len(diags) > 0 {
		return nil, fmt.Errorf("plan failed after reverting sensitive identifiers: %v %s", err, joinDiags(diags))
	}
	return g.TF.ShowJSON(ctx, g.planFile())
}
