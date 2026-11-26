// Package inventory finds live resources through a provider's inventory
// data sources: a cloud's "list everything in scope" API exposed as a data
// source (a resource search, an asset or tag index, a role assignment
// listing). Discovery uses it for resource types the provider can't list.
//
// Which data sources to read and how their entries map to resource types
// and import IDs is pack data (inventory.json), added from discover's
// suggestions and checked with import plans; nothing here knows any provider.
package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// Inventory is inventory.json.
type Inventory struct {
	Sources []Source `json:"sources"`
	// Kinds maps the resource kinds sources report to resource types.
	Kinds map[string]Mapping `json:"kinds,omitempty"`
}

// Source is one data source whose entries are existing resources.
type Source struct {
	DataSource string `json:"data_source"`
	// Config maps arguments to HCL expressions; "{param}" is replaced by
	// the value of a scope parameter.
	Config map[string]string `json:"config,omitempty"`
	List   string            `json:"list"` // attribute holding the entries
	// ID is the entry attribute holding the resource ID, or a template of
	// attributes ("{namespace}/{name}"). Entry attributes are dotted paths
	// into nested objects ("metadata.name").
	ID string `json:"id"`
	// Kind is the entry attribute naming the resource's kind. Empty means
	// the kind is derived from the ID with KindMatch, or, without it, that
	// every entry is of one kind named after the data source.
	Kind string `json:"kind,omitempty"`
	// KindMatch is a regexp over the kind attribute (or the ID); its
	// capture groups, joined with "/", are the kind.
	KindMatch string `json:"kind_match,omitempty"`
	Name      string `json:"name,omitempty"` // entry attribute with a display name
	// Parent, when set, reads the source once per discovered resource of
	// this type (a resource group, a project), with Args mapping arguments
	// to the parent's identity attributes ("id" is its import ID).
	Parent string            `json:"parent,omitempty"`
	Args   map[string]string `json:"args,omitempty"`
	// Complete, when set, is a path into the data source's attributes
	// ("resource_count[*].complete") whose value is false when the
	// source cut its result off. Without it a result whose size is a round
	// number (100, 200, 500, 1000, ...) is taken as cut off.
	Complete string `json:"complete,omitempty"`
	// Partition says how to read the source again in narrower parts when
	// a read comes back cut off.
	Partition *Partition `json:"partition,omitempty"`
	Note      string     `json:"note,omitempty"`
	// Provider is the provider block to read with ("aws.us"), set by the
	// workspace; empty for the unaliased one.
	Provider string `json:"-"`
}

// Partition narrows a source by the kinds it reports (the Kind field, as
// reported, before KindMatch), through one argument. One added to a pack
// is kept only if a read with it does what it says (CheckPartition).
type Partition struct {
	Arg string `json:"arg"`
	// Only is the argument's HCL expression for reading one kind: "{kind}"
	// is the kind.
	Only string `json:"only,omitempty"`
	// Except is the expression for reading everything but some kinds:
	// "{kinds}" is Each, with "{kind}" filled in, for every such kind,
	// joined with Sep (default " ").
	Except string `json:"except,omitempty"`
	Each   string `json:"each,omitempty"`
	Sep    string `json:"sep,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Uses returns the scope parameters of params that some source's config
// or partition refers to ("{param}"): a read depends on no others.
func (inv *Inventory) Uses(params map[string]string) map[string]string {
	var exprs []string
	for _, s := range inv.Sources {
		exprs = slices.AppendSeq(exprs, maps.Values(s.Config))
		if p := s.Partition; p != nil {
			exprs = append(exprs, p.Only, p.Except, p.Each)
		}
	}
	used := map[string]string{}
	for p, v := range params {
		for _, e := range exprs {
			if strings.Contains(e, "{"+p+"}") {
				used[p] = v
			}
		}
	}
	return used
}

// Label names the source in messages: the data source and its config.
func (s Source) Label() string {
	if len(s.Config) == 0 {
		return s.DataSource
	}
	var kv []string
	for _, k := range slices.Sorted(maps.Keys(s.Config)) {
		kv = append(kv, k+"="+s.Config[k])
	}
	return s.DataSource + " (" + strings.Join(kv, ", ") + ")"
}

// Mapping says which resource type a kind is and how to import it.
type Mapping struct {
	// Type is the resource type; empty means the kind isn't managed as a
	// resource of its own (Note says why).
	Type string `json:"type,omitempty"`
	// Match is a regexp over the entry's ID whose named groups fill ID.
	Match string `json:"match,omitempty"`
	// ID is the import ID template: "{group}" for named groups of Match,
	// "{id}" for the entry's ID. Empty means the entry's ID as is.
	ID     string `json:"id,omitempty"`
	Origin string `json:"origin,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Entry is one resource reported by a source.
type Entry struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// EntryID is the ID of an entry: its ID attribute, or the ID template
// filled in. A template with an attribute the entry lacks keeps the rest
// ("{namespace}/{name}" is "name" for an entry without a namespace).
func (s Source) EntryID(obj map[string]any) string {
	if !strings.Contains(s.ID, "{") {
		return Field(obj, s.ID)
	}
	var parts []string
	for _, seg := range strings.Split(s.ID, "/") {
		v := pathPlaceholder.ReplaceAllStringFunc(seg, func(p string) string { return Field(obj, p[1:len(p)-1]) })
		if v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, "/")
}

// IDFields are the entry attributes the ID is made of.
func (s Source) IDFields() []string {
	if !strings.Contains(s.ID, "{") {
		return []string{s.ID}
	}
	var out []string
	for _, p := range pathPlaceholder.FindAllString(s.ID, -1) {
		out = append(out, p[1:len(p)-1])
	}
	return out
}

var pathPlaceholder = regexp.MustCompile(`\{[A-Za-z_][A-Za-z0-9_.]*\}`)

// Field is the string or number at a dotted path in an entry, as a
// string, or "".
func Field(obj map[string]any, path string) string {
	if path == "" {
		return ""
	}
	var v any = obj
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[k]
	}
	switch x := v.(type) {
	case string:
		return x
	case json.Number: // a numeric ID, as read (see reader.plan)
		return x.String()
	case float64: // a numeric ID, as plain JSON decoding gives it
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

// Targets maps entries to import targets. Entries of unknown kinds are
// counted in unmapped; entries whose ID doesn't fit their mapping in
// misfit.
func (inv *Inventory) Targets(entries []Entry) (targets []model.Target, unmapped map[string]int, misfit map[string]int) {
	unmapped, misfit = map[string]int{}, map[string]int{}
	seen := map[string]bool{}
	for _, e := range entries {
		m, ok := inv.Kinds[e.Kind]
		if !ok {
			unmapped[e.Kind]++
			continue
		}
		if m.Type == "" {
			continue
		}
		id, groups, ok := m.importID(e.ID)
		if !ok {
			misfit[e.Kind]++
			continue
		}
		t := model.Target{Type: m.Type, ID: id, Display: e.Name, Origin: "inventory", Attrs: groups}
		if t.Display == "" {
			t.Display = id[strings.LastIndexAny(id, "/:")+1:]
		}
		if !seen[t.Key()] {
			seen[t.Key()] = true
			targets = append(targets, t)
		}
	}
	return targets, unmapped, misfit
}

// ImportID applies the mapping to an entry ID.
func (m Mapping) ImportID(id string) (string, bool) {
	out, _, ok := m.importID(id)
	return out, ok
}

// Target is the import target the mapping makes of an entry ID, with
// Match's groups as Attrs.
func (m Mapping) Target(id string) (model.Target, bool) {
	out, groups, ok := m.importID(id)
	return model.Target{Type: m.Type, ID: out, Attrs: groups}, ok
}

// importID also returns the values of Match's named groups.
func (m Mapping) importID(id string) (string, map[string]string, bool) {
	if m.ID == "" && m.Match == "" {
		return id, nil, true
	}
	var groups map[string]string
	vals := map[string]string{"id": id}
	if m.Match != "" {
		re, err := regexp.Compile(m.Match)
		if err != nil {
			return "", nil, false
		}
		sub := re.FindStringSubmatch(id)
		if sub == nil {
			return "", nil, false
		}
		groups = map[string]string{}
		for i, name := range re.SubexpNames() {
			if name != "" {
				vals[name] = sub[i]
				if sub[i] != "" { // an optional group that didn't match says nothing
					groups[name] = sub[i]
				}
			}
		}
	}
	tmpl := m.ID
	if tmpl == "" {
		tmpl = "{id}"
	}
	ok := true
	out := placeholder.ReplaceAllStringFunc(tmpl, func(p string) string {
		v, found := vals[p[1:len(p)-1]]
		if !found {
			ok = false
		}
		return v
	})
	return out, groups, ok && out != ""
}

var placeholder = regexp.MustCompile(`\{[A-Za-z_][A-Za-z0-9_]*\}`)

// Kinds summarizes entries by kind: count, a sample ID, and samples of
// other ID shapes (a rule on the default bus, rule/name, and on another,
// rule/bus/name), so a mapping is checked against each.
func Kinds(entries []Entry) map[string]KindSummary {
	out := map[string]KindSummary{}
	shapes := map[string]map[string]bool{}
	for _, e := range entries {
		k := out[e.Kind]
		k.Count++
		if shapes[e.Kind] == nil {
			shapes[e.Kind] = map[string]bool{}
		}
		if sh := shape(e.ID); !shapes[e.Kind][sh] {
			shapes[e.Kind][sh] = true
			if k.Sample == "" {
				k.Sample = e.ID
			} else if len(k.Others) < maxShapes {
				k.Others = append(k.Others, e.ID)
			}
		}
		out[e.Kind] = k
	}
	return out
}

type KindSummary struct {
	Count  int    `json:"count"`
	Sample string `json:"sample"`
	// Others are samples of other ID shapes, at most maxShapes.
	Others []string `json:"others,omitempty"`
}

// Samples is Sample and Others.
func (k KindSummary) Samples() []string {
	if k.Sample == "" {
		return nil
	}
	return append([]string{k.Sample}, k.Others...)
}

const maxShapes = 2

// shape is an ID with its words replaced: IDs of one shape differ only in
// names, not in structure.
func shape(id string) string { return word.ReplaceAllString(id, "w") }

var word = regexp.MustCompile(`[A-Za-z0-9_-]+`)

// CheckImports plans an import of each target with generated config, in
// .desprawl/<sub>, and returns the import errors by target index. Errors in
// the generated config are not import errors: the ID worked. A target
// terraform generated no config for didn't import, whatever the error.
func CheckImports(ctx context.Context, ws model.Workspace, r tf.Runner, sub string, targets []model.Target) (map[int]string, error) {
	dir, err := ws.TFDir(sub)
	if err != nil {
		return nil, err
	}
	f := hclwrite.NewEmptyFile()
	byAddr := map[string]int{}
	for i, t := range targets {
		addr := fmt.Sprintf("%s.check%d", t.Type, i)
		byAddr[addr] = i
		b := f.Body().AppendNewBlock("import", nil).Body()
		toks, _ := hclx.ExprTokens(addr)
		b.SetAttributeRaw("to", toks)
		b.SetAttributeValue("id", cty.StringVal(t.ID))
		f.Body().AppendNewline()
	}
	src := f.Bytes()
	if err := os.WriteFile(filepath.Join(dir, "imports.tf"), src, 0o644); err != nil {
		return nil, err
	}
	gen := filepath.Join(dir, "generated.tf")
	os.Remove(gen)
	defer os.Remove(gen)
	diags, err := r.In(dir).Plan(ctx, "-generate-config-out=generated.tf")
	if err != nil {
		return nil, err
	}
	failed := map[int]string{}
	loose := "" // the first error tied to no target
	for _, d := range diags {
		addr := d.Address
		if d.Range != nil && d.Range.Filename == "imports.tf" {
			addr = hclx.ImportTarget(src, d.Range.Start.Line)
		} else if d.Range != nil {
			continue // generated config: the import itself worked
		}
		if i, ok := byAddr[addr]; ok {
			if failed[i] == "" {
				failed[i] = d.String()
			}
		} else if loose == "" {
			loose = d.String()
		}
	}
	// A group named after an attribute must hold the imported value:
	// discover takes it for that attribute.
	imported := importedStrings(gen)
	for addr, i := range byAddr {
		if failed[i] != "" {
			continue
		}
		if _, ok := imported[addr]; !ok {
			// Its read failed with an error terraform didn't tie to it.
			failed[i] = "no config generated"
			if loose != "" {
				failed[i] += "; the plan's error: " + loose
			}
			continue
		}
		for _, k := range slices.Sorted(maps.Keys(targets[i].Attrs)) {
			if v, ok := imported[addr][k]; ok && v != targets[i].Attrs[k] {
				failed[i] = fmt.Sprintf("the match's group %s is %q, but the imported %s is %q: a group named after an attribute must hold exactly its value", k, targets[i].Attrs[k], k, v)
				break
			}
		}
	}
	return failed, nil
}

// importedStrings returns the string attributes of each resource in
// generated config, by address.
func importedStrings(file string) map[string]map[string]string {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	f, diags := hclsyntax.ParseConfig(src, file, hcl.InitialPos)
	if diags.HasErrors() {
		return nil
	}
	out := map[string]map[string]string{}
	for _, b := range f.Body.(*hclsyntax.Body).Blocks {
		if b.Type != "resource" || len(b.Labels) != 2 {
			continue
		}
		attrs := map[string]string{}
		for name, a := range b.Body.Attributes {
			if v, d := a.Expr.Value(nil); !d.HasErrors() && v.Type() == cty.String && v.IsKnown() && !v.IsNull() {
				attrs[name] = v.AsString()
			}
		}
		out[b.Labels[0]+"."+b.Labels[1]] = attrs
	}
	return out
}
