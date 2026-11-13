package suggest

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/askrike/desprawl/internal/tf"
)

// ScopeCandidates returns identity attributes shared by many resource types
// (project, region, account_id, ...): the parameters a listing can be scoped
// by.
func ScopeCandidates(schemas *tf.Schemas) []string {
	counts := map[string]int{}
	for _, is := range schemas.Identities {
		for name := range is.Attributes {
			counts[name]++
		}
	}
	var out []string
	for name, n := range counts {
		if n*10 >= len(schemas.Identities) && name != "name" && name != "id" && name != "arn" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// candidateParents returns resource types (listable or not) that could
// supply typ's arguments: types from the same service (sharing the longest
// name prefix, by underscore-separated segments), plus types named after an
// argument's stem (instance_id -> *_instance, cluster_name -> *_cluster):
// the required arguments', or the optional ones' if none is required.
func candidateParents(schemas *tf.Schemas, typ string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != typ && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	segs := strings.Split(typ, "_")
	// Prefixes that are types themselves: azurerm_lb for
	// azurerm_lb_backend_address_pool.
	for n := len(segs) - 1; n >= 2; n-- {
		if p := strings.Join(segs[:n], "_"); schemas.Resources[p] != nil {
			add(p)
		}
	}
	for n := len(segs) - 1; n >= 2; n-- {
		prefix := strings.Join(segs[:n], "_")
		var same []string
		for p := range schemas.Resources {
			if p != typ && (p == prefix || strings.HasPrefix(p, prefix+"_")) {
				same = append(same, p)
			}
		}
		if len(same) > 0 {
			sort.Strings(same)
			for _, p := range same[:min(len(same), 25)] {
				add(p)
			}
			break
		}
	}
	args := schemas.RequiredListArgs(typ)
	if len(args) == 0 {
		args = optionalListArgs(schemas, typ)
	}
	for _, arg := range args {
		stem := arg
		for _, suffix := range []string{"_id", "_arn", "_name", "_identifier", "_ids", "_arns", "_names"} {
			stem = strings.TrimSuffix(stem, suffix)
		}
		var named []string
		for p := range schemas.Resources {
			if strings.HasSuffix(p, "_"+stem) {
				named = append(named, p)
			}
		}
		sort.Strings(named)
		for _, p := range named {
			add(p)
		}
	}
	return out
}

// optionalListArgs are the optional, non-computed arguments of a list
// resource, sorted.
func optionalListArgs(schemas *tf.Schemas, typ string) []string {
	var out []string
	if b := schemas.Lists[typ]; b != nil {
		for name, a := range b.Attributes {
			if a.Optional && !a.Required && !a.Computed {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// parentAttrs lists identity attributes and top-level string attributes.
func parentAttrs(schemas *tf.Schemas, typ string) []string {
	seen := map[string]bool{}
	var out []string
	if is := schemas.Identities[typ]; is != nil {
		for name := range is.Attributes {
			seen[name] = true
			out = append(out, name)
		}
	}
	if r := schemas.Resources[typ]; r != nil {
		for name, a := range r.Attributes {
			if !seen[name] && string(a.Type) == `"string"` {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// sourceCandidates are data sources with a computed list of objects that
// carry string fields.
func sourceCandidates(schemas *tf.Schemas) []string {
	var out []string
	for ds, blk := range schemas.DataSources {
		for _, a := range blk.Attributes {
			if a.Computed && (!a.Optional && objectListFields(a) != nil || isDynamic(a)) {
				out = append(out, ds)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// isDynamic reports an attribute whose type is only known once read.
func isDynamic(a *tf.SchemaAttr) bool { return string(a.Type) == `"dynamic"` }

// objectListFields returns the string fields of a list or set of objects
// attribute, or nil if it isn't one.
func objectListFields(a *tf.SchemaAttr) []string {
	var t []json.RawMessage
	if json.Unmarshal(a.Type, &t) != nil || len(t) != 2 {
		return nil
	}
	var kind string
	if json.Unmarshal(t[0], &kind) != nil || (kind != "list" && kind != "set") {
		return nil
	}
	var obj []json.RawMessage
	if json.Unmarshal(t[1], &obj) != nil || len(obj) != 2 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(obj[1], &fields) != nil {
		return nil
	}
	var out []string
	for name, ft := range fields {
		if string(ft) == `"string"` {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// contextSources are small data sources without required arguments: the
// ones that describe the current account, project, region or caller.
func contextSources(schemas *tf.Schemas) []string {
	var out []string
	for ds, blk := range schemas.DataSources {
		if !hasRequired(blk) && len(blk.Attributes) <= 12 && len(computedStrings(blk)) > 0 {
			out = append(out, ds)
		}
	}
	sort.Strings(out)
	return out
}

func hasRequired(blk *tf.SchemaBlock) bool {
	for _, a := range blk.Attributes {
		if a.Required {
			return true
		}
	}
	for _, bt := range blk.BlockTypes {
		if bt.MinItems > 0 {
			return true
		}
	}
	return false
}

func computedStrings(blk *tf.SchemaBlock) []string {
	var out []string
	for name, a := range blk.Attributes {
		if a.Computed && string(a.Type) == `"string"` {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// dropCandidates are computed map or object attributes present in at least
// 2% of resource types, by how many types have them.
func dropCandidates(schemas *tf.Schemas) map[string]int {
	counts := map[string]int{}
	for _, blk := range schemas.Resources {
		for name, a := range blk.Attributes {
			if a.Computed && !a.Required && (strings.HasPrefix(string(a.Type), `["map"`) || strings.HasPrefix(string(a.Type), `["object"`)) {
				counts[name]++
			}
		}
	}
	for name, n := range counts {
		if n*50 < len(schemas.Resources) {
			delete(counts, name)
		}
	}
	return counts
}
