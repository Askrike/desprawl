// Package suggest finds fixes to suggest for what the stages couldn't do
// on their own, from the workspace's data and the provider's schema.
// desprawl never calls a model: whoever runs it (an agent or a person)
// decides which suggestions are right and writes them into the inputs.
//
// Literals the linker couldn't resolve are matched against every string
// attribute of every imported resource. Each match is a candidate signature
// (source type, path, target type, target attr); whoever runs desprawl
// decides whether it is a real reference or a coincidence of naming, and
// adds the real ones to the pack as rules.
package suggest

import (
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/link"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
)

// Candidate is a signature seen in the data, with examples.
type Candidate struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	Target string `json:"target"`
	Attr   string `json:"attr"`
	Count  int    `json:"count"`
	// Values is how many different values were seen: a rule backed by one
	// value ("default") may be a coincidence, one backed by several less so.
	Values   int      `json:"values"`
	Examples []string `json:"examples"`
	// Match is "embedded" when the target value appears inside the source
	// string; empty means the whole value (rules.MatchExact).
	Match rules.Match `json:"match,omitempty"`
}

func (c Candidate) key() string {
	k := c.Source + " " + c.Path + " " + c.Target + " " + c.Attr
	if c.Match != "" && c.Match != rules.MatchExact {
		k += " " + string(c.Match)
	}
	return k
}

// Values that occur as an attribute of this many resources are configuration
// conventions (a shared prefix, a region), not identities.
const maxOwners = 4

// Map paths found on this many resource types are conventions.
const conventionTypes = 5

var notNameLike = regexp.MustCompile(`^([0-9.]+|true|false|null|enabled|disabled|[A-Z]+)$`)

type owner struct{ addr, typ, attr string }

// Candidates finds unresolved literals that equal some other resource's
// string attribute. opts carries the known rules and the attributes the
// linker drops.
func Candidates(src []byte, plan *tf.PlanJSON, schemas *tf.Schemas, opts link.Options) ([]Candidate, error) {
	ix, err := index.Build(plan, schemas, opts.Rules)
	if err != nil {
		return nil, err
	}
	lits, err := link.Unresolved(src, ix, opts)
	if err != nil {
		return nil, err
	}
	// Like link, "type.attr" drops an attribute from one type only.
	dropped := func(typ, path string) bool {
		for _, d := range opts.Drop {
			if t, a, ok := strings.Cut(d, "."); ok {
				if t != typ {
					continue
				}
				d = a
			}
			if path == d || strings.HasPrefix(path, d+".") {
				return true
			}
		}
		return false
	}
	// Locations that already reference something can't be reference targets:
	// aws_iam_role_policy.role equals aws_iam_role.name because it points at
	// the role, not the other way round.
	linked, err := link.Link(src, ix, opts)
	if err != nil {
		return nil, err
	}
	isRef := map[string]bool{}
	carries := map[[3]string]bool{} // from, to, value
	for _, e := range linked.Edges {
		isRef[e.From+"."+e.Path] = true
		carries[[3]string{e.From, e.To(), e.Value}] = true
	}
	// Of the resources that have a value, one that has it because it refers
	// to another of them with it carries that one's name (a bucket's ACL,
	// whose ID is the bucket's name): the other is the one the value names.
	// A resource's own name that merely equals what it refers to (a cluster
	// named like its resource group) is still its own.
	parents := func(owners []owner, value string) []owner {
		var out []owner
		for _, o := range owners {
			child := false
			for _, p := range owners {
				if p.addr != o.addr && carries[[3]string{o.addr, p.addr, value}] && (o.attr == "id" || isRef[o.addr+"."+o.attr]) {
					child = true
					break
				}
			}
			if !child {
				out = append(out, o)
			}
		}
		return out
	}

	broad := map[string][]owner{}
	names := map[string][]owner{} // resources' own names, for embedded candidates
	entries := map[string][]mapEntry{}
	byEntry := map[[2]string][]owner{} // map entries by value and key
	for _, rc := range plan.ResourceChanges {
		if rc.Mode != "managed" {
			continue
		}
		vals, err := rc.Values()
		if err != nil {
			return nil, err
		}
		for _, e := range mapEntries(vals) {
			if !dropped(rc.Type, e.attr) {
				entries[rc.Address] = append(entries[rc.Address], e)
				byEntry[[2]string{e.value, e.key}] = append(byEntry[[2]string{e.value, e.key}], owner{rc.Address, rc.Type, e.attr + ".*"})
			}
		}
		if id, _ := vals["id"].(string); id != "" && wholeName.MatchString(id) {
			names[id] = append(names[id], owner{rc.Address, rc.Type, "id"})
		} else if attr := index.IDPartAttr(vals, id); attr != "" {
			if v, _ := index.Value(vals, attr).(string); wholeName.MatchString(v) {
				names[v] = append(names[v], owner{rc.Address, rc.Type, attr})
			}
		}
		for attr, v := range vals {
			s, ok := v.(string)
			if !ok || len(s) < 3 || notNameLike.MatchString(s) || dropped(rc.Type, attr) {
				continue
			}
			broad[s] = append(broad[s], owner{rc.Address, rc.Type, attr})
		}
		// A name nested in a block ("metadata.0.name") when the import ID
		// is composite ("namespace/name").
		if id, _ := vals["id"].(string); id != "" {
			if attr := index.IDPartAttr(vals, id); strings.Contains(attr, ".") {
				if s, _ := index.Value(vals, attr).(string); len(s) >= 3 && !notNameLike.MatchString(s) && !dropped(rc.Type, attr) {
					broad[s] = append(broad[s], owner{rc.Address, rc.Type, attr})
				}
			}
		}
	}

	// A map path that many types have is a convention (labels, tags), not
	// a selector or what one selects.
	typesAt := map[string]map[string]bool{}
	for addr, es := range entries {
		typ := addr[:strings.Index(addr, ".")]
		for _, e := range es {
			if typesAt[e.plain] == nil {
				typesAt[e.plain] = map[string]bool{}
			}
			typesAt[e.plain][typ] = true
		}
	}
	conventional := func(plain string) bool { return len(typesAt[plain]) >= conventionTypes }

	byKey := map[string]*Candidate{}
	values := map[string]map[string]bool{}
	add := func(c Candidate, value string) {
		if byKey[c.key()] == nil {
			byKey[c.key()] = &c
			values[c.key()] = map[string]bool{}
		}
		cc := byKey[c.key()]
		cc.Count++
		values[c.key()][value] = true
		cc.Values = len(values[c.key()])
		if len(cc.Examples) < 3 && !slices.Contains(cc.Examples, value) {
			cc.Examples = append(cc.Examples, value)
		}
	}
	for _, l := range lits {
		// A value that became a reference whole (a scope value such as the
		// region) needs no rule.
		if dropped(l.Type, l.Path) || slices.Contains(l.Linked, l.Value) {
			continue
		}
		// A map entry equal to the same key's entry of another resource's
		// map (a selector and the labels it selects). The same map path on
		// both sides is a shared setting (labels on two resources).
		for _, e := range entries[l.Addr] {
			if e.plain+"."+e.key != l.Path || e.value != l.Value || conventional(e.plain) || !labelValue.MatchString(e.value) {
				continue
			}
			groups := map[string][]owner{}
			for _, o := range byEntry[[2]string{e.value, e.key}] {
				if plain := stripIndices(strings.TrimSuffix(o.attr, ".*")); o.addr != l.Addr && plain != e.plain && !conventional(plain) && !laterItem.MatchString(o.attr) {
					groups[o.typ+" "+o.attr] = append(groups[o.typ+" "+o.attr], o)
				}
			}
			for _, os := range groups {
				if len(os) <= maxOwners {
					add(Candidate{Source: l.Type, Path: e.plain + ".*", Target: os[0].typ, Attr: os[0].attr, Match: rules.MatchExact}, e.value)
				}
			}
		}
		// Names of other resources inside a longer string (a host name).
		for _, word := range nameWord.FindAllString(l.Value, -1) {
			if word == l.Value || len(word) < 3 || insideLinked(word, l.Linked) {
				continue
			}
			groups := map[string][]owner{}
			for _, o := range parents(names[word], word) {
				if o.addr != l.Addr {
					groups[o.typ+" "+o.attr] = append(groups[o.typ+" "+o.attr], o)
				}
			}
			for _, os := range groups {
				if len(os) <= maxOwners {
					add(Candidate{Source: l.Type, Path: l.Path, Target: os[0].typ, Attr: os[0].attr, Match: rules.MatchEmbedded}, l.Value)
				}
			}
		}
		// The same name on many resources of one type is a convention; on
		// one resource each of several types it may still be a reference.
		owners := parents(dropRedundantIDs(broad[l.Value]), l.Value)
		perType := map[string]map[string]bool{}
		for _, o := range owners {
			if perType[o.typ] == nil {
				perType[o.typ] = map[string]bool{}
			}
			perType[o.typ][o.addr] = true
		}
		for _, o := range owners {
			if len(perType[o.typ]) > maxOwners {
				continue
			}
			// Same attribute of the same type is a shared setting, not a reference.
			sibling := o.typ == l.Type && o.attr == l.Path
			if o.addr == l.Addr || sibling || isRef[o.addr+"."+o.attr] {
				continue
			}
			add(Candidate{Source: l.Type, Path: l.Path, Target: o.typ, Attr: o.attr}, l.Value)
		}
	}
	out := make([]Candidate, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out, nil
}

// insideLinked says whether word is part of a value already linked in the
// same string (the ID in "integrations/abc123").
func insideLinked(word string, linked []string) bool {
	for _, v := range linked {
		if strings.Contains(v, word) {
			return true
		}
	}
	return false
}

// plainWord is a single ordinary word ("default", "backend", "ipv4"): as
// the only value behind a candidate, more likely a setting that happens to
// equal a name than a reference.
var plainWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

// Weak says whether the candidate rests only on one plain word.
func (c Candidate) Weak() bool {
	return c.Values == 1 && plainWord.MatchString(c.Examples[0])
}

// nameWord is a run of characters names are made of; dots and slashes
// separate names in host names and paths.
var (
	nameWord  = regexp.MustCompile(`[A-Za-z0-9_-]+`)
	wholeName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// labelValue is a map value that can name something: quantities ("100m",
// "512Mi") and flags can't.
var labelValue = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{2,}$`)

// laterItem matches attribute paths into a list item other than the first:
// one of several blocks is no fixed place for a rule to point at.
var laterItem = regexp.MustCompile(`\.[1-9][0-9]*(\.|$)`)

// mapEntry is one string entry of a map attribute: plain is the map's path
// without list indices (as in rule paths), attr with them (as in references).
type mapEntry struct{ plain, attr, key, value string }

// mapEntries finds the string entries of maps in a resource's values. In
// plan JSON, blocks are lists of objects; an object directly under another
// object is a map (or an object-typed attribute).
func mapEntries(vals map[string]any) []mapEntry {
	var out []mapEntry
	var walk func(v any, plain, attr []string, inList bool)
	walk = func(v any, plain, attr []string, inList bool) {
		switch x := v.(type) {
		case map[string]any:
			if len(plain) > 0 && !inList {
				for k, c := range x {
					if s, ok := c.(string); ok && s != "" {
						out = append(out, mapEntry{strings.Join(plain, "."), strings.Join(attr, "."), k, s})
					}
				}
				return
			}
			for k, c := range x {
				walk(c, append(slices.Clone(plain), k), append(slices.Clone(attr), k), false)
			}
		case []any:
			for i, c := range x {
				walk(c, plain, append(slices.Clone(attr), strconv.Itoa(i)), true)
			}
		}
	}
	walk(vals, nil, nil, false)
	return out
}

func stripIndices(attr string) string {
	var segs []string
	for _, seg := range strings.Split(attr, ".") {
		if _, err := strconv.Atoi(seg); err != nil {
			segs = append(segs, seg)
		}
	}
	return strings.Join(segs, ".")
}

// dropRedundantIDs removes a resource's "id" when another of its attributes
// has the same value, since the named attribute is the meaningful one.
func dropRedundantIDs(owners []owner) []owner {
	named := map[string]bool{}
	for _, o := range owners {
		if o.attr != "id" {
			named[o.addr] = true
		}
	}
	var out []owner
	for _, o := range owners {
		if o.attr == "id" && named[o.addr] {
			continue
		}
		out = append(out, o)
	}
	return out
}
