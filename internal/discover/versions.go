package discover

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/askrike/desprawl/internal/model"
)

// Versions finds the older versions of a resource, dropped by discovery
// (Report.Superseded), that a value refers to: "web:12" or "…/web:12"
// names revision 12 of web whether the identity is {family: web,
// revision: 12} or an ARN ending in "web:12". A resource pinned to an old
// revision would otherwise be left pointing at nothing.
type Versions struct {
	byKey map[string]model.Target
}

func NewVersions(superseded map[string][]model.Target) *Versions {
	v := &Versions{byKey: map[string]model.Target{}}
	for typ, ts := range superseded {
		for _, t := range ts {
			t.Type = typ
			for _, k := range versionRefKeys(t.Identity) {
				v.byKey[k] = t
			}
		}
	}
	return v
}

func (v *Versions) Len() int { return len(v.byKey) }

// Lookup returns the older version a whole value names, if any.
func (v *Versions) Lookup(value string) (model.Target, bool) {
	toks := splitVersion(value)
	if len(toks) < 2 {
		return model.Target{}, false
	}
	t, ok := v.byKey[toks[len(toks)-2]+"\x00"+toks[len(toks)-1]]
	return t, ok
}

// versionRefKeys returns "name\x00revision" keys under which a value can
// name this version: the revision with the identity value before it.
func versionRefKeys(identity map[string]any) []string {
	if _, _, ok := versionKey(identity); !ok {
		return nil
	}
	var rev string
	var names []string
	for _, k := range slices.Sorted(maps.Keys(identity)) {
		switch x := identity[k].(type) {
		case float64:
			if rev == "" && (strings.Contains(k, "revision") || strings.Contains(k, "version")) {
				rev = strconv.FormatFloat(x, 'f', -1, 64)
			}
		case string:
			if m := versioned.FindStringSubmatch(x); m != nil && rev == "" {
				toks := splitVersion(x)
				if len(toks) >= 2 {
					return []string{toks[len(toks)-2] + "\x00" + toks[len(toks)-1]}
				}
			}
			if x != "" {
				names = append(names, x)
			}
		}
	}
	if rev == "" {
		return nil
	}
	var out []string
	for _, n := range names {
		out = append(out, n+"\x00"+rev)
	}
	return out
}

func splitVersion(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == '/' || r == '@' })
}

// AddNamed names new targets without renaming the existing ones: a name
// already taken gets the version (or a counter) appended.
func AddNamed(existing, add []model.Target) []model.Target {
	used := map[string]bool{}
	seen := map[string]bool{}
	for _, t := range existing {
		used[t.Address()] = true
		seen[t.Key()] = true
	}
	out := existing
	for _, t := range add {
		if seen[t.Key()] {
			continue
		}
		seen[t.Key()] = true
		base := Sanitize(hint(t))
		name := base
		if used[t.Type+"."+name] {
			if _, rev, ok := versionKey(t.Identity); ok {
				base = fmt.Sprintf("%s_%d", base, rev)
				name = base
			}
		}
		for i := 2; used[t.Type+"."+name]; i++ {
			name = fmt.Sprintf("%s_%d", base, i)
		}
		used[t.Type+"."+name] = true
		t.Name = name
		out = append(out, t)
	}
	return out
}
