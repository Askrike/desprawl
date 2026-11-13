// Package rules is the knowledge base for reference resolution.
//
// Globally unique values (ARNs, resource paths, generated IDs) are linked
// generically by value. Plain names are ambiguous ("foo" may be a bucket, a
// role and a log group at once), so they are only linked where a rule says
// that a given attribute holds a name of a given resource type.
//
// Rules are data: mined from provider docs or judged from real
// infrastructure (by whoever runs desprawl, from link's suggestions), and
// stored as JSON in provider packs.
package rules

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

type Match string

const (
	MatchExact    Match = "exact"    // attribute value is the target attribute
	MatchEmbedded Match = "embedded" // target attribute appears inside a string
)

type Origin string

const (
	OriginManual Origin = "manual"
	OriginDocs   Origin = "docs"
	OriginLLM    Origin = "llm"
)

// Rule says: attribute Path of resources of type Source holds the Attr of a
// resource of type Target. A Path ending in ".*" matches every key of a map
// and holds the same key of a map Attr ending in ".*": a selector whose
// entries must equal a target's labels.
type Rule struct {
	Source  string `json:"source"` // terraform type; a trailing '*' matches by prefix
	Path    string `json:"path"`   // dotted attribute path; list indices are omitted
	Target  string `json:"target"`
	Attr    string `json:"attr"`
	Match   Match  `json:"match"`
	Origin  Origin `json:"origin"`
	Support int    `json:"support,omitempty"` // number of examples it was seen in
	Note    string `json:"note,omitempty"`
	// Rejected records that the attribute does not name the target, though
	// values match (two settings that happen to be equal): link never uses
	// it, and doesn't suggest it again.
	Rejected bool `json:"rejected,omitempty"`
}

func (r Rule) Matches(srcType, path string) bool {
	if _, ok := r.Key(path); !ok {
		return false
	}
	if p, ok := strings.CutSuffix(r.Source, "*"); ok {
		return strings.HasPrefix(srcType, p)
	}
	return r.Source == srcType
}

// Key is the map key a wildcard rule matched in path ("" for other rules),
// and whether the rule's path matches at all.
func (r Rule) Key(path string) (string, bool) {
	if prefix, ok := strings.CutSuffix(r.Path, "*"); ok && strings.HasSuffix(prefix, ".") {
		key, ok := strings.CutPrefix(path, prefix)
		return key, ok && key != ""
	}
	return "", r.Path == path
}

// TargetAttr is the target attribute for a matched key.
func (r Rule) TargetAttr(key string) string {
	if prefix, ok := strings.CutSuffix(r.Attr, "*"); ok && key != "" {
		return prefix + key
	}
	return r.Attr
}

// Alias says how configuration shortens a kind of identifier: Match is a
// regexp over the full value, and its capture groups, joined, are the
// shortened spelling ("family:revision" for a revisioned object's full
// identifier, a name without the random suffix the provider appends).
type Alias struct {
	Match   string `json:"match"`
	Origin  Origin `json:"origin"`
	Support int    `json:"support,omitempty"` // examples it was seen in
	Note    string `json:"note,omitempty"`
	re      *regexp.Regexp
}

// Compile checks the pattern: it must be a regexp with a capture group.
func (a *Alias) Compile() error {
	re, err := regexp.Compile(a.Match)
	if err != nil {
		return err
	}
	if re.NumSubexp() == 0 {
		return fmt.Errorf("alias %q has no capture group", a.Match)
	}
	a.re = re
	return nil
}

// Short returns the shortened spelling of v, if the pattern applies.
func (a Alias) Short(v string) (string, bool) {
	if a.re == nil {
		return "", false
	}
	m := a.re.FindStringSubmatch(v)
	if m == nil {
		return "", false
	}
	short := strings.Join(m[1:], "")
	return short, short != "" && short != v
}

// Set is an indexed collection of rules.
type Set struct {
	rules   []Rule
	targets map[string][]string // target type -> attrs referenced by rules
	aliases []Alias
}

// WithAliases returns the set with alias patterns added; a pattern that
// doesn't compile is an error.
func (s *Set) WithAliases(as []Alias) (*Set, error) {
	out := *s
	out.aliases = slices.Clone(s.aliases)
	for _, a := range as {
		if err := a.Compile(); err != nil {
			return nil, err
		}
		out.aliases = append(out.aliases, a)
	}
	return &out, nil
}

// Aliases returns the set's alias patterns.
func (s *Set) Aliases() []Alias {
	if s == nil {
		return nil
	}
	return s.aliases
}

// NewSet indexes rules. When several rules match, hand-written ones win,
// then the ones seen in the most examples.
func NewSet(rs []Rule) *Set {
	// A rejection also takes out the same signature accepted elsewhere.
	rejected := map[string]bool{}
	for _, r := range rs {
		if r.Rejected {
			rejected[r.signature()] = true
		}
	}
	rs = slices.DeleteFunc(slices.Clone(rs), func(r Rule) bool { return rejected[r.signature()] })
	slices.SortStableFunc(rs, func(a, b Rule) int {
		if c := cmp.Compare(originRank(a.Origin), originRank(b.Origin)); c != 0 {
			return c
		}
		return cmp.Compare(b.Support, a.Support)
	})
	s := &Set{rules: rs, targets: map[string][]string{}}
	for _, r := range rs {
		s.targets[r.Target] = append(s.targets[r.Target], r.Attr)
	}
	return s
}

func (r Rule) signature() string { return r.Source + " " + r.Path + " " + r.Target + " " + r.Attr }

func originRank(o Origin) int {
	if o == OriginManual {
		return 0
	}
	return 1
}

// Without returns the set minus the rules of one origin.
func (s *Set) Without(o Origin) *Set {
	var rs []Rule
	for _, r := range s.rules {
		if r.Origin != o {
			rs = append(rs, r)
		}
	}
	return NewSet(rs)
}

// TargetAttrs returns the attributes of a type that rules point at.
func (s *Set) TargetAttrs(typ string) []string { return s.targets[typ] }

// For returns the rules of one match kind applying to an attribute path of
// a source type.
func (s *Set) For(srcType, path string, m Match) []Rule {
	var out []Rule
	for _, r := range s.rules {
		if r.Match == m && r.Matches(srcType, path) {
			out = append(out, r)
		}
	}
	return out
}

func Load(path string) ([]Rule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rs []Rule
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, err
	}
	return rs, nil
}

func Save(path string, rs []Rule) error {
	b, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func LoadAliases(path string) ([]Alias, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var as []Alias
	return as, json.Unmarshal(b, &as)
}

func SaveAliases(path string, as []Alias) error {
	b, err := json.MarshalIndent(as, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
