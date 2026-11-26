package rea

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
	"github.com/askrike/desprawl/providers"
)

// maxRelated caps the related-unlistable group: the first ones, by how
// much points at them, are what to look up.
const maxRelated = 15

// relatedUnlistable is the short list of types the provider can't list,
// and no inventory found, that the account likely has: a pack rule
// connects them to a type that was found (either way), or an imported
// value names them ("webhook-pager" in a message names a *_webhook type).
// desprawl can't find these itself, so they're only imported when someone
// looks them up and adds them; among a provider's many unlistable types,
// these are the ones to look up first.
func (w Workspace) relatedUnlistable(schemas *tf.Schemas, pk *providers.Pack, unlistable []string) ProblemGroup {
	g := ProblemGroup{
		Kind: "related-unlistable",
		Howto: fmt.Sprintf("Look each up through the provider's own API and add every one that exists with `%s -type TYPE -id ID`\n"+
			"(`%s -type TYPE` shows its import ID), then run `%s` again. desprawl can't find these by itself:\n"+
			"none is imported unless added. Add rather than judge from a partial API listing: an import that fails costs one plan.",
			w.cmd("add-target"), w.cmd("import-format"), w.cmd("run")),
		File: w.StatePath("targets.json"), Rerun: w.cmd("run"),
	}
	var targets []model.Target
	w.ReadJSON("targets.json", &targets)
	found := map[string]int{}
	for _, t := range targets {
		if w.mine(t.Type) {
			found[t.Type]++
		}
	}
	open := map[string]bool{}
	for _, typ := range unlistable {
		if found[typ] == 0 && schemas.Resources[typ] != nil {
			open[typ] = true
		}
	}
	why := map[string][]string{}
	weight := map[string]int{}
	add := func(typ, reason string, n int) {
		if !slices.Contains(why[typ], reason) {
			why[typ] = append(why[typ], reason)
			weight[typ] += n
		}
	}
	for _, r := range pk.Rules {
		if r.Rejected {
			continue
		}
		if n := found[r.Source]; n > 0 && open[r.Target] {
			add(r.Target, fmt.Sprintf("%s.%s can name it (%d found)", r.Source, r.Path, n), n)
		}
		if n := found[r.Target]; n > 0 && open[r.Source] {
			add(r.Source, fmt.Sprintf("its %s can name %s (%d found)", r.Path, r.Target, n), n)
		}
	}
	for typ, mention := range w.namedInValues(open) {
		add(typ, "named in an imported value: "+mention, 1)
	}
	types := slices.SortedFunc(maps.Keys(why), func(a, b string) int {
		return cmp.Or(cmp.Compare(weight[b], weight[a]), cmp.Compare(a, b))
	})
	g.Summary = plural(len(types), "resource type that can't be listed is", "resource types that can't be listed are") +
		" connected to what desprawl found, so the account likely has some."
	if len(types) > maxRelated {
		g.Summary += fmt.Sprintf(" The first %d:", maxRelated)
		types = types[:maxRelated]
	}
	for _, typ := range types {
		p := Problem{Subject: typ, Detail: why[typ]}
		if ex := pk.Imports[typ]; ex != nil && len(ex.IDs) > 0 {
			p.Detail = append(p.Detail, "import ID, e.g. "+ex.IDs[0])
		}
		g.Problems = append(g.Problems, p)
	}
	return g
}

// namedInValues finds the types whose last name word (at least 5 letters,
// so not "test" or "rule") starts a word followed by '-' in a string the last plan imported: such a value names an object of that type
// by its handle ("@webhook-pager"). Each type gets one example: the
// resource and an excerpt.
func (w Workspace) namedInValues(types map[string]bool) map[string]string {
	words := map[string][]string{}
	for typ := range types {
		word := typ[strings.LastIndex(typ, "_")+1:]
		if len(word) >= 5 {
			words[word] = append(words[word], typ)
		}
	}
	out := map[string]string{}
	if len(words) == 0 {
		return out
	}
	b, err := os.ReadFile(w.StatePath("plan.json"))
	if err != nil {
		return out
	}
	plan, err := tf.ParsePlan(b)
	if err != nil {
		return out
	}
	for _, rc := range plan.ResourceChanges {
		if !w.mine(rc.Type) {
			continue
		}
		var v any
		if json.Unmarshal(rc.Change.After, &v) != nil {
			continue
		}
		eachString(v, func(s string) {
			for _, word := range slices.Sorted(maps.Keys(words)) {
				typs := words[word]
				if at := handleAt(s, word); at >= 0 {
					for _, typ := range typs {
						if _, ok := out[typ]; !ok {
							out[typ] = fmt.Sprintf("%s %q", rc.Address, excerpt(s, at, len(word)))
						}
					}
				}
			}
		})
	}
	return out
}

// handleAt is where word starts a handle in s: at the start or after a
// character that isn't a letter or digit, followed by '-' ("key:value"
// tags would make every tag key a type). -1 if it doesn't.
func handleAt(s, word string) int {
	for i := 0; ; {
		j := strings.Index(s[i:], word)
		if j < 0 {
			return -1
		}
		j += i
		end := j + len(word)
		if (j == 0 || !isAlnum(s[j-1])) && end < len(s) && s[end] == '-' {
			return j
		}
		i = j + 1
	}
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// excerpt is up to 30 characters either side of s[at:at+n], on one line.
func excerpt(s string, at, n int) string {
	from, to := max(0, at-30), min(len(s), at+n+30)
	e := strings.Join(strings.Fields(strings.ToValidUTF8(s[from:to], "")), " ")
	if from > 0 {
		e = "…" + e
	}
	if to < len(s) {
		e += "…"
	}
	return e
}

func eachString(v any, f func(string)) {
	switch v := v.(type) {
	case string:
		f(v)
	case []any:
		for _, x := range v {
			eachString(x, f)
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			eachString(v[k], f)
		}
	}
}
