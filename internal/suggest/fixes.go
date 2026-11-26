package suggest

import (
	"maps"
	"slices"
	"strings"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/inventory"
	"github.com/askrike/desprawl/internal/tf"
)

// ListArg suggests a list-arg spec for a type that can only be listed with
// arguments: scope arguments (project, region) filled from the scope, the
// others from the first candidate parent's attribute each most likely
// names. It also returns the candidate parents, best first, for whoever
// checks the suggestion; ok is false when there is no parent to suggest.
func ListArg(schemas *tf.Schemas, typ string, scopes []string) (spec discover.ListArg, parents []string, ok bool) {
	args := schemas.RequiredListArgs(typ)
	if len(args) == 0 {
		// Optional arguments are often alternatives ("one of a, b"): fill
		// the first one a resource type is named after.
		opt := optionalListArgs(schemas, typ)
		args = opt[:min(len(opt), 1)]
		for _, a := range opt {
			if len(namedAfter(schemas, typ, trimRef(a))) > 0 {
				args = []string{a}
				break
			}
		}
	}
	spec = discover.ListArg{Type: typ}
	var rest []string
	for _, a := range args {
		if slices.Contains(scopes, a) {
			if spec.Scope == nil {
				spec.Scope = map[string]string{}
			}
			spec.Scope[a] = a
		} else {
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return spec, nil, len(spec.Scope) > 0
	}
	// A parent named after the first argument is the likeliest: ending in
	// it (cluster_name -> *_cluster), else holding each of its words
	// (recovery_vault_id -> *_recovery_services_vault).
	stem := trimRef(rest[0])
	named := namedAfter(schemas, typ, stem)
	parents = candidateParents(schemas, typ)
	parents = append(slices.DeleteFunc(parents, func(p string) bool { return slices.Contains(named, p) }), named...)
	rank := func(p string) int {
		switch {
		case strings.HasSuffix(p, "_"+stem):
			return 2
		case slices.Contains(named, p):
			return 1
		}
		return 0
	}
	slices.SortStableFunc(parents, func(a, b string) int { return rank(b) - rank(a) })
	if len(parents) == 0 {
		return spec, nil, false
	}
	spec.Parent = parents[0]
	spec.Args = map[string]string{}
	attrs := parentAttrs(schemas, spec.Parent)
	for _, a := range rest {
		spec.Args[a] = parentAttr(a, attrs)
	}
	return spec, parents[:min(len(parents), 5)], true
}

// namedAfter are resource types other than typ named after an argument's
// stem: ending in it, or ending in its last word and holding the others
// (recovery_vault -> *_recovery_services_vault).
func namedAfter(schemas *tf.Schemas, typ, stem string) []string {
	words := strings.Split(stem, "_")
	var out []string
	for _, r := range slices.Sorted(maps.Keys(schemas.Resources)) {
		segs := strings.Split(r, "_")
		if r != typ && (strings.HasSuffix(r, "_"+stem) || segs[len(segs)-1] == words[len(words)-1] && containsAll(segs, words)) {
			out = append(out, r)
		}
	}
	return out
}

func containsAll(segs, words []string) bool {
	for _, w := range words {
		if !slices.Contains(segs, w) {
			return false
		}
	}
	return true
}

var refSuffixes = []string{"_id", "_arn", "_name", "_identifier", "_ids", "_arns", "_names"}

// trimRef strips a reference suffix: cluster_name -> cluster.
func trimRef(arg string) string {
	for _, s := range refSuffixes {
		if t, ok := strings.CutSuffix(arg, s); ok {
			return t
		}
	}
	return arg
}

// parentAttr is the parent attribute an argument most likely takes its
// value from: one of the same name, else the one its suffix names
// (cluster_name -> name), else id.
func parentAttr(arg string, attrs []string) string {
	if slices.Contains(attrs, arg) {
		return arg
	}
	for _, s := range refSuffixes {
		if strings.HasSuffix(arg, s) {
			a := strings.TrimSuffix(strings.TrimPrefix(s, "_"), "s")
			if slices.Contains(attrs, a) {
				return a
			}
		}
	}
	return "id"
}

// ParamSources are data sources that may return a scope parameter's value:
// small ones without required arguments with a computed string attribute
// of the parameter's name.
func ParamSources(schemas *tf.Schemas, param string) []string {
	var out []string
	for _, ds := range contextSources(schemas) {
		if slices.Contains(computedStrings(schemas.DataSources[ds]), param) {
			out = append(out, ds)
		}
	}
	return out
}

// InventorySources are data sources that report many resources at once
// (a computed list of objects), which the pack's inventory can read.
func InventorySources(schemas *tf.Schemas, types []string) []string {
	return sourceCandidates(schemas, types)
}

// Singular is the resource type a listing data source is named after
// (its name's singular form).
func Singular(name string) string { return singular(name) }

// OptionalListArgs are a list type's optional arguments, which a failed
// listing may need anyway.
func OptionalListArgs(schemas *tf.Schemas, typ string) []string {
	return optionalListArgs(schemas, typ)
}

// DropCandidates are computed map or object attributes many resource types
// have (tags including provider defaults, effective labels): mirrors of
// other attributes that nobody writes, which link can drop.
func DropCandidates(schemas *tf.Schemas) []string {
	return slices.Sorted(maps.Keys(dropCandidates(schemas)))
}

// SourceArgs are a data source's optional arguments, which may narrow what
// it reports (for a partition) or scope it to one container.
func SourceArgs(schemas *tf.Schemas, ds string) []string {
	blk := schemas.DataSources[ds]
	if blk == nil {
		return nil
	}
	var out []string
	for name, a := range blk.Attributes {
		if a.Optional {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// PerParent suggests reading a data source once per container: an optional
// argument named after one of the discovered types (resource_group_name
// after *_resource_group), mapped to the attribute its suffix names. ok is
// false when no argument is.
func PerParent(schemas *tf.Schemas, ds string, discovered map[string]bool) (arg, parent, attr string, ok bool) {
	for _, a := range SourceArgs(schemas, ds) {
		stem := trimRef(a)
		if stem == a {
			continue
		}
		for _, p := range namedAfter(schemas, "", stem) {
			if discovered[p] {
				return a, p, parentAttr(a, parentAttrs(schemas, p)), true
			}
		}
	}
	return "", "", "", false
}

// InventorySource suggests a complete inventory source for a listing data
// source: its one list of entries, their "id" field as the ID, and a name
// field where there is one ("name", else one ending in "name", else
// "title" or "handle"). Its entries are all of one kind, the data source's
// name, which Singular maps to the type it's named after. ok is false when
// the data source needs arguments, has no or several entry lists, or its
// entries have no "id" field: those want a person to write the source.
func InventorySource(schemas *tf.Schemas, ds string) (s inventory.Source, ok bool) {
	blk := schemas.DataSources[ds]
	if blk == nil {
		return s, false
	}
	var lists, fields []string
	for name, a := range blk.Attributes {
		if a.Required {
			return s, false
		}
		if f := objectListFields(a); a.Computed && !a.Optional && f != nil {
			lists, fields = append(lists, name), f
		}
	}
	for name := range blk.BlockTypes {
		if f, _ := entryFields(blk, name); f != nil {
			lists, fields = append(lists, name), f
		}
	}
	if len(lists) != 1 || !slices.Contains(fields, "id") {
		return s, false
	}
	s = inventory.Source{DataSource: ds, List: lists[0], ID: "id", Name: nameField(fields)}
	return s, CheckSource(schemas, s) == nil
}

// nameField is the entry field most likely holding a display name.
func nameField(fields []string) string {
	if slices.Contains(fields, "name") {
		return "name"
	}
	for _, f := range slices.Sorted(slices.Values(fields)) {
		if strings.HasSuffix(f, "name") {
			return f
		}
	}
	for _, f := range []string{"title", "handle"} {
		if slices.Contains(fields, f) {
			return f
		}
	}
	return ""
}
