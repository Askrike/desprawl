package link

import (
	"encoding/json"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/tf"
)

// Duplicate is an inline attribute that a standalone resource manages too
// (a queue's policy and a queue policy resource): generate fills every
// optional, computed attribute from state, so both would hold it.
type Duplicate struct {
	Addr, Attr string
	By         string // the standalone resource
}

// Duplicates finds attributes of a resource that are optional and computed
// and hold a document (a JSON object), which another resource requires
// under the same attribute name, equal as JSON, and which that resource
// attaches to the first through one of the first's own identifiers.
// Dropping a computed attribute can't drift; the standalone resource keeps
// the document. Plain strings (names, settings) are never duplicates: two
// resources share those without managing the same thing.
func Duplicates(plan *tf.PlanJSON, schemas *tf.Schemas, ix *index.Index) ([]Duplicate, error) {
	if schemas == nil {
		return nil, nil
	}
	type res struct {
		addr, typ string
		vals      map[string]any
	}
	var all []res
	required := map[string][]int{} // "attr document" -> resources requiring it
	for _, rc := range plan.ResourceChanges {
		s := schemas.Resources[rc.Type]
		if rc.Mode != "managed" || s == nil {
			continue
		}
		vals, err := rc.Values()
		if err != nil {
			return nil, err
		}
		all = append(all, res{rc.Address, rc.Type, vals})
		for name, a := range s.Attributes {
			if doc, ok := document(vals[name]); ok && a.Required {
				required[name+" "+doc] = append(required[name+" "+doc], len(all)-1)
			}
		}
	}
	var out []Duplicate
	for _, r := range all {
		for name, a := range schemas.Resources[r.typ].Attributes {
			doc, ok := document(r.vals[name])
			if !ok || !a.Optional || !a.Computed {
				continue
			}
			for _, i := range required[name+" "+doc] {
				b := all[i]
				if b.typ != r.typ && attaches(ix, b.vals, r.addr) {
					out = append(out, Duplicate{Addr: r.addr, Attr: name, By: b.addr})
					break
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Addr != out[j].Addr {
			return out[i].Addr < out[j].Addr
		}
		return out[i].Attr < out[j].Attr
	})
	return out, nil
}

// ManagedDuplicates are the inline attributes the pack says standalone
// types manage too (managedBy: "type.attr" to those types), on resources
// that a resource of one of those types attaches to: its import ID
// extends theirs ("<dataset>/roles/…"), or another of its attributes holds
// a value theirs owns (a security group's ID).
func ManagedDuplicates(plan *tf.PlanJSON, ix *index.Index, managedBy map[string][]string) ([]Duplicate, error) {
	if len(managedBy) == 0 {
		return nil, nil
	}
	type res struct {
		addr, typ, id string
		vals          map[string]any
	}
	byType := map[string][]res{}
	for _, rc := range plan.ResourceChanges {
		if rc.Mode != "managed" {
			continue
		}
		vals, err := rc.Values()
		if err != nil {
			return nil, err
		}
		id, _ := vals["id"].(string)
		byType[rc.Type] = append(byType[rc.Type], res{rc.Address, rc.Type, id, vals})
	}
	var out []Duplicate
	for _, key := range slices.Sorted(maps.Keys(managedBy)) {
		typ, attr, ok := strings.Cut(key, ".")
		if !ok {
			continue
		}
		for _, r := range byType[typ] {
			if r.vals[attr] == nil {
				continue
			}
		found:
			for _, st := range managedBy[key] {
				for _, s := range byType[st] {
					if (r.id != "" && strings.HasPrefix(strings.ToLower(s.id), strings.ToLower(r.id)+"/")) || attaches(ix, s.vals, r.addr) {
						out = append(out, Duplicate{Addr: r.addr, Attr: attr, By: s.addr})
						break found
					}
				}
			}
		}
	}
	return out, nil
}

// attaches reports whether a top-level attribute other than id holds a
// value that addr owns.
func attaches(ix *index.Index, vals map[string]any, addr string) bool {
	for name, v := range vals {
		s, ok := v.(string)
		if !ok || name == "id" {
			continue
		}
		if o, ok := ix.Owner(s); ok && o.Address == addr {
			return true
		}
	}
	return false
}

// document spells a string holding a JSON object canonically, so the same
// policy compares equal whatever its key order or spacing.
func document(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) != nil || len(m) == 0 {
		return "", false
	}
	b, err := json.Marshal(m)
	return string(b), err == nil
}
