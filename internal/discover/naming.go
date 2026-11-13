package discover

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// Name assigns unique, valid terraform resource names to listed resources.
func Name(found []tf.ListResult) []model.Target {
	targets := make([]model.Target, 0, len(found))
	for _, r := range found {
		targets = append(targets, model.Target{Type: r.Type, Identity: r.Identity, Display: r.Display})
	}
	return Rename(targets)
}

// Rename (re)assigns names to targets, deduplicating identical ones.
func Rename(targets []model.Target) []model.Target {
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].Type != targets[j].Type {
			return targets[i].Type < targets[j].Type
		}
		return targets[i].Key() < targets[j].Key()
	})
	used := map[string]bool{}
	seen := map[string]bool{}
	var out []model.Target
	for _, t := range targets {
		if seen[t.Key()] {
			continue
		}
		seen[t.Key()] = true
		base := Sanitize(hint(t))
		name := base
		for i := 2; used[t.Type+"."+name]; i++ {
			name = fmt.Sprintf("%s_%d", base, i)
		}
		used[t.Type+"."+name] = true
		t.Name = name
		out = append(out, t)
	}
	return out
}

func hint(t model.Target) string {
	if t.Display != "" {
		return lastSegment(t.Display)
	}
	if t.ID != "" {
		return lastSegment(t.ID)
	}
	for _, k := range []string{"name", "id", "arn"} {
		if s, ok := t.Identity[k].(string); ok && s != "" {
			return lastSegment(s)
		}
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(t.Identity)) {
		if s, ok := t.Identity[k].(string); ok {
			parts = append(parts, lastSegment(s))
		}
	}
	return strings.Join(parts, "_")
}

// lastSegment shortens ARN- or path-like values to their final segment.
func lastSegment(s string) string {
	if i := strings.LastIndexAny(s, ":/"); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	return s
}

var nonIdent = regexp.MustCompile(`[^a-z0-9_]+`)

// Sanitize turns an arbitrary string into a valid terraform identifier.
func Sanitize(s string) string {
	s = nonIdent.ReplaceAllString(strings.ToLower(s), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "this"
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "r_" + s
	}
	return s
}

// traversal is a reference: "aws", or "aws.us" for an aliased provider.
func traversal(ref string) hcl.Traversal {
	root, attr, ok := strings.Cut(ref, ".")
	t := hcl.Traversal{hcl.TraverseRoot{Name: root}}
	if ok {
		t = append(t, hcl.TraverseAttr{Name: attr})
	}
	return t
}

// ProviderRef is the provider meta-argument of a block using the provider
// configuration ref ("aws.us"); nil for an unaliased one, the default.
func ProviderRef(ref string) hcl.Traversal {
	if !strings.Contains(ref, ".") {
		return nil
	}
	return traversal(ref)
}

// ImportsTF renders import blocks for targets.
func ImportsTF(targets []model.Target) ([]byte, error) {
	f := hclwrite.NewEmptyFile()
	for i, t := range targets {
		if i > 0 {
			f.Body().AppendNewline()
		}
		b := f.Body().AppendNewBlock("import", nil).Body()
		b.SetAttributeTraversal("to", hcl.Traversal{hcl.TraverseRoot{Name: t.Type}, hcl.TraverseAttr{Name: t.Name}})
		if p := ProviderRef(t.Provider); p != nil {
			b.SetAttributeTraversal("provider", p)
		}
		if t.ID != "" {
			b.SetAttributeValue("id", cty.StringVal(t.ID))
			continue
		}
		v, err := IdentityValue(t.Identity)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Address(), err)
		}
		b.SetAttributeValue("identity", v)
	}
	return hclwrite.Format(f.Bytes()), nil
}
