package rea

// What the reverse-engineering stages couldn't do on their own, printed to
// stdout when a stage ends: grouped by kind, each group saying what
// happened and how to fix it, each problem with the fix desprawl suggests.
// The fixes go into the stages' inputs (overrides.json, targets.json, the
// pack); whoever runs desprawl, an agent or a person, decides and edits,
// then runs the stage again. desprawl never calls a model itself.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/generate"
	"github.com/askrike/desprawl/internal/inventory"
	"github.com/askrike/desprawl/internal/link"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/suggest"
	"github.com/askrike/desprawl/internal/tf"
	"github.com/askrike/desprawl/internal/verify"
	"github.com/askrike/desprawl/providers"
)

type Problem struct {
	Subject string   `json:"subject"`
	Detail  []string `json:"detail,omitempty"`
	// Fix is what to add to the group's file, nil when desprawl has
	// nothing to suggest: an item of its list, or, with Key, the value of
	// that key in its object.
	Key string `json:"key,omitempty"`
	Fix any    `json:"fix,omitempty"`
}

// fixText is the fix as it goes into the file.
func (p Problem) fixText() string {
	b, _ := marshal(p.Fix)
	if p.Key == "" {
		return b
	}
	k, _ := marshal(p.Key)
	return k + ": " + b
}

type ProblemGroup struct {
	Kind    string `json:"kind"`            // what went wrong, for -json
	Summary string `json:"summary"`         // what happened, with the count
	Howto   string `json:"howto"`           // how to fix one: the file and the command to run after
	File    string `json:"file,omitempty"`  // the input the fixes go into
	Rerun   string `json:"rerun,omitempty"` // the command to run after fixing
	// Brief groups print their subjects on a few lines, not one each, or,
	// when there are more than maxBrief, where Listed says they are.
	Brief    bool      `json:"brief,omitempty"`
	Listed   string    `json:"listed,omitempty"`
	Problems []Problem `json:"problems"`
}

// PrintProblems writes the groups as text, or as one JSON object per
// problem.
func PrintProblems(out io.Writer, groups []ProblemGroup, asJSON bool) {
	for _, g := range groups {
		if len(g.Problems) == 0 {
			continue
		}
		if asJSON {
			for _, p := range g.Problems {
				b, _ := marshal(struct {
					Kind string `json:"kind"`
					Problem
					File  string `json:"file,omitempty"`
					Rerun string `json:"rerun,omitempty"`
				}{g.Kind, p, g.File, g.Rerun})
				fmt.Fprintln(out, b)
			}
			continue
		}
		fmt.Fprintf(out, "%s\n%s\n\n", g.Summary, g.Howto)
		if g.Brief {
			if len(g.Problems) > maxBrief {
				// Too many to read: where the full list is says enough.
				fmt.Fprintf(out, "  (the full list is in %s)\n\n", g.Listed)
				continue
			}
			var subjects []string
			for _, p := range g.Problems {
				subjects = append(subjects, p.Subject)
			}
			fmt.Fprintf(out, "%s\n\n", wrap(strings.Join(subjects, ", "), "  ", 96))
			continue
		}
		for _, p := range g.Problems {
			fmt.Fprintf(out, "  %s\n", p.Subject)
			for _, d := range p.Detail {
				fmt.Fprintf(out, "    %s\n", d)
			}
			if p.Fix != nil {
				fmt.Fprintf(out, "    %s\n", p.fixText())
			}
			fmt.Fprintln(out)
		}
	}
}

const maxBrief = 30

func wrap(s, indent string, width int) string {
	var b strings.Builder
	line := indent
	for _, w := range strings.Fields(s) {
		if len(line) > len(indent) && len(line)+1+len(w) > width {
			b.WriteString(line + "\n")
			line = indent
		}
		if len(line) > len(indent) {
			line += " "
		}
		line += w
	}
	b.WriteString(line)
	return b.String()
}

// marshal is compact JSON as written in the files: no HTML escaping.
func marshal(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

// namesArgument says whether a list error is about an argument: it says
// "argument", or quotes one of the type's list arguments.
func namesArgument(e string, args []string) bool {
	if strings.Contains(strings.ToLower(e), "argument") {
		return true
	}
	for _, a := range args {
		for _, q := range []string{`"`, `'`, "`"} {
			if strings.Contains(e, q+a+q) {
				return true
			}
		}
	}
	return false
}

// briefList is items joined, the first n of them and how many more.
func briefList(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// cmd is how to run a workspace command on w from where desprawl runs:
// "desprawl generate", or with -C for a workspace elsewhere.
func (w Workspace) cmd(name string) string {
	if w.Dir == "." || w.Dir == "" {
		return "desprawl " + name
	}
	return "desprawl -C " + w.Dir + " " + name
}

// rel is a workspace path as the user passed the workspace.
func (w Workspace) rel(parts ...string) string {
	return filepath.Join(append([]string{w.Dir}, parts...)...)
}

// packFile is the path of one of the pack's files in the packs directory,
// (a read-only workspace, as status and the map page open it, only says
// where),
// where fixes to the pack go. A file there replaces the shipped one of the
// same name, so when it isn't there yet the shipped one is copied first:
// a fix adds to it rather than replacing it.
func (w Workspace) packFile(name string) (string, error) {
	path := filepath.Join(w.Packs(), w.Provider().Name, name)
	if _, err := os.Stat(path); err == nil || !errors.Is(err, fs.ErrNotExist) || w.ReadOnly {
		return path, nil
	}
	b, err := shippedOrEmpty(w.Provider().Name, name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, b, 0o644)
}

// shippedOrEmpty is what packFile starts a pack file with: the shipped
// one, or an empty list or object when none is shipped.
func shippedOrEmpty(provider, name string) ([]byte, error) {
	b, err := providers.ReadFile("", provider, name)
	if errors.Is(err, fs.ErrNotExist) {
		if name == "inventory.json" || name == "pack.json" {
			return []byte("{}\n"), nil
		}
		return []byte("[]\n"), nil
	}
	return b, err
}

// checkPack returns the pack less the list args, inventory sources and
// kind mappings that fail their checks against the schema (entries added
// by hand or by an agent), and a problem for each.
func (w Workspace) checkPack(schemas *tf.Schemas, p *providers.Pack) (*providers.Pack, []ProblemGroup) {
	out := *p
	// Everything a listing can be scoped by, not only the parameters known
	// so far: a spec whose parameter nothing supplies yet fails in discover
	// ("scope parameters … not found"), not here as invalid.
	scopes := slices.Concat(slices.Collect(maps.Keys(w.params)), slices.Collect(maps.Keys(p.Params)), suggest.ScopeCandidates(schemas))
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	var bad []Problem
	// Entries the workspace didn't change come with desprawl: say so, or
	// they read as the workspace's own edits to fix.
	shipped := false
	where := func(name string) string {
		path := filepath.Join(w.Packs(), w.Provider().Name, name)
		if w.unchangedPackFile(name) {
			shipped = true
			return "shipped pack " + w.Provider().Name + "/" + name
		}
		return path
	}
	out.ListArgs = nil
	for _, la := range p.ListArgs {
		if schemas.Lists[la.Type] == nil {
			continue // a list resource of another provider version: unused here
		}
		if err := suggest.CheckListArg(schemas, la, scopes); err != nil {
			bad = append(bad, Problem{Subject: where("list_args.json") + ": " + la.Type, Detail: []string{err.Error()}})
			continue
		}
		out.ListArgs = append(out.ListArgs, la)
	}
	if p.Inventory != nil {
		seen, _ := w.inventory()
		inv := *p.Inventory
		inv.Sources = nil
		for _, src := range p.Inventory.Sources {
			if err := suggest.CheckSource(schemas, src); err != nil {
				bad = append(bad, Problem{Subject: where("inventory.json") + ": source " + src.Label(), Detail: []string{err.Error()}})
				continue
			}
			inv.Sources = append(inv.Sources, src)
		}
		inv.Kinds = map[string]inventory.Mapping{}
		for k, m := range p.Inventory.Kinds {
			if err := suggest.CheckMapping(schemas, m, seen.Kinds[k].Samples()); err != nil {
				bad = append(bad, Problem{Subject: where("inventory.json") + ": kind " + k, Detail: []string{err.Error()}})
				continue
			}
			inv.Kinds[k] = m
		}
		out.Inventory = &inv
	}
	slices.SortFunc(bad, func(a, b Problem) int { return strings.Compare(a.Subject, b.Subject) })
	var groups []ProblemGroup
	if len(bad) > 0 {
		howto := fmt.Sprintf("Fix or remove each in the file it names, then run `%s` again.", w.cmd("discover"))
		if shipped {
			howto += fmt.Sprintf(" Entries of the shipped pack come with desprawl: to fix one, put the file in %s, which replaces the shipped one, and report it.", filepath.Join(w.Packs(), w.Provider().Name))
		}
		groups = append(groups, ProblemGroup{
			Kind:     "pack-invalid",
			Summary:  plural(len(bad), "entry in the provider's pack doesn't fit its schema, so desprawl leaves it out.", "entries in the provider's pack don't fit its schema, so desprawl leaves them out."),
			Howto:    howto,
			Rerun:    w.cmd("discover"),
			Problems: bad,
		})
	}
	return &out, groups
}

// unchangedPackFile says whether the workspace's pack has no file name of
// its own, or one identical to the shipped one.
func (w Workspace) unchangedPackFile(name string) bool {
	b, err := os.ReadFile(filepath.Join(w.Packs(), w.Provider().Name, name))
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	s, serr := providers.ReadFile("", w.Provider().Name, name)
	return err == nil && serr == nil && bytes.Equal(b, s)
}

// DiscoverProblems: types discovery couldn't list, and kinds the
// inventory reported that map to no resource type.
func (w Workspace) DiscoverProblems(schemas *tf.Schemas) ([]ProblemGroup, error) {
	return w.perProvider(func(w Workspace) ([]ProblemGroup, error) { return w.discoverProblemsOf(schemas) })
}

// perProvider builds problems once per provider block, scoped to it: its
// types, its pack; with several blocks, each group's heading names its
// block.
func (w Workspace) perProvider(build func(Workspace) ([]ProblemGroup, error)) ([]ProblemGroup, error) {
	if len(w.Config.Providers) <= 1 {
		return build(w)
	}
	var out []ProblemGroup
	for _, p := range w.Config.Providers {
		gs, err := build(w.scoped(p))
		if err != nil {
			return nil, err
		}
		for i := range gs {
			gs[i].Summary = p.Ref() + ": " + gs[i].Summary
		}
		out = append(out, gs...)
	}
	return out, nil
}

// mine reports whether a resource type is the scoped provider's (every
// type is when the workspace isn't scoped). Two blocks of one provider
// share its types: the first of them reports them.
func (w Workspace) mine(typ string) bool {
	if w.current == nil {
		return true
	}
	owner := w.ProviderOf(typ)
	return owner.Name == w.current.Name && owner.Ref() == w.current.Ref() || owner.Name == w.current.Name && w.firstOf(owner.Name) == w.current.Ref()
}

func (w Workspace) firstOf(name string) string {
	for _, p := range w.Config.Providers {
		if p.Name == name {
			return p.Ref()
		}
	}
	return ""
}

// minedDocs says whether a pack has anything mine-docs writes: import
// examples or rules from the docs.
func minedDocs(pk *providers.Pack) bool {
	if len(pk.Imports) > 0 {
		return true
	}
	return slices.ContainsFunc(pk.Rules, func(r rules.Rule) bool { return r.Origin == rules.OriginDocs })
}

func (w Workspace) discoverProblemsOf(schemas *tf.Schemas) ([]ProblemGroup, error) {
	if w.current != nil && w.firstOf(w.current.Name) != w.current.Ref() {
		return nil, nil // the provider's first block reports its types and pack
	}
	var rep discover.Report
	if err := w.ReadJSON("discovery.json", &rep); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	keep := func(ts []string) []string {
		return slices.DeleteFunc(slices.Clone(ts), func(t string) bool { return !w.mine(t) })
	}
	rep.NeedsArgs, rep.Truncated, rep.Unlistable = keep(rep.NeedsArgs), keep(rep.Truncated), keep(rep.Unlistable)
	maps.DeleteFunc(rep.Failed, func(t, _ string) bool { return !w.mine(t) })
	maps.DeleteFunc(rep.Listed, func(t string, _ int) bool { return !w.mine(t) })
	rerun := w.cmd("discover")
	scopes := slices.Sorted(maps.Keys(w.params))
	pk, err := w.Pack()
	if err != nil {
		return nil, err
	}
	_, out := w.checkPack(schemas, pk)

	if !minedDocs(pk) {
		mine := w.cmd("mine-docs -provider " + w.Provider().Source)
		out = append(out, ProblemGroup{
			Kind: "no-docs",
			Summary: "The pack has nothing from the provider's docs: no rules, so link finds few references,\n" +
				"and no import IDs, so import-format and generate's probes have nothing to go on.",
			Howto: fmt.Sprintf("Run `%s` (it writes the workspace's pack), then run `%s` again.", mine, w.cmd("run")),
			File:  filepath.Join(w.Packs(), w.Provider().Name), Rerun: w.cmd("run"),
			Problems: []Problem{{Subject: w.Provider().Source}},
		})
	}

	if len(rep.NeedsArgs) > 0 {
		file, err := w.packFile("list_args.json")
		if err != nil {
			return nil, err
		}
		g := ProblemGroup{
			Kind:    "needs-list-args",
			Summary: plural(len(rep.NeedsArgs), "resource type", "resource types") + " can only be listed with arguments desprawl doesn't know how to fill.",
			Howto: fmt.Sprintf("To list one, add its entry to the list in %s, then run `%s` again.\n"+
				"\"parent\" is the type whose resources supply the arguments; \"args\" maps each argument to the parent's attribute.", file, rerun),
			File: file, Rerun: rerun,
		}
		for _, typ := range slices.Sorted(slices.Values(rep.NeedsArgs)) {
			p := Problem{Subject: typ}
			if args := schemas.RequiredListArgs(typ); len(args) > 0 {
				p.Detail = append(p.Detail, "needs: "+strings.Join(args, ", "))
			}
			spec, parents, ok := suggest.ListArg(schemas, typ, scopes)
			if len(parents) > 1 {
				p.Detail = append(p.Detail, "possible parents: "+strings.Join(parents, ", "))
			}
			if ok {
				p.Fix = spec
			}
			g.Problems = append(g.Problems, p)
		}
		out = append(out, g)
	}

	if len(rep.Failed) > 0 {
		file, err := w.packFile("list_args.json")
		if err != nil {
			return nil, err
		}
		g := ProblemGroup{
			Kind:    "list-failed",
			Summary: plural(len(rep.Failed), "resource type", "resource types") + " failed to list. Permission errors and services that aren't enabled are expected: leave those.",
			Howto:   fmt.Sprintf("If an error says an argument is needed, add an entry for the type to the list in %s, then run `%s` again.", file, rerun),
			File:    file, Rerun: rerun,
		}
		// Only an error that names an argument ("argument", or one of the
		// type's list arguments quoted) has a fix: those are printed per
		// type. The rest (a service that isn't enabled fails every type
		// it serves) are counted, their errors in discovery.json.
		var other []string
		for _, typ := range slices.Sorted(maps.Keys(rep.Failed)) {
			args := suggest.OptionalListArgs(schemas, typ)
			if !namesArgument(rep.Failed[typ], args) {
				other = append(other, typ)
				continue
			}
			p := Problem{Subject: typ, Detail: []string{"error: " + oneLine(rep.Failed[typ])}}
			if len(args) > 0 {
				p.Detail = append(p.Detail, "optional arguments: "+strings.Join(args, ", "))
				if spec, _, ok := suggest.ListArg(schemas, typ, scopes); ok {
					p.Detail = append(p.Detail, "if the error says one is needed:")
					p.Fix = spec
				}
			}
			g.Problems = append(g.Problems, p)
		}
		if len(other) > 0 {
			g.Problems = append(g.Problems, Problem{
				Subject: plural(len(other), "type", "types") + " with errors that name no argument: " + briefList(other, 5),
				Detail:  []string{`errors: "failed" in ` + w.StatePath("discovery.json")},
			})
		}
		out = append(out, g)
	}

	if len(rep.Truncated) > 0 {
		g := ProblemGroup{
			Kind:    "truncated",
			Summary: plural(len(rep.Truncated), "resource type", "resource types") + " hit the result limit, so some of their resources are missing.",
			Howto:   fmt.Sprintf("To list them all, run `%s -limit 10000 -types %s`.", rerun, strings.Join(rep.Truncated, ",")),
			Rerun:   rerun, Brief: true,
			Listed: `"truncated" in ` + w.StatePath("discovery.json"),
		}
		for _, typ := range rep.Truncated {
			g.Problems = append(g.Problems, Problem{Subject: typ})
		}
		out = append(out, g)
	}

	inv, err := w.inventory()
	if err != nil {
		return nil, err
	}
	hasInventory := pk.Inventory != nil && len(pk.Inventory.Sources) > 0

	if len(rep.Unlistable) > 0 {
		g := ProblemGroup{
			Kind:    "unlistable",
			Summary: plural(len(rep.Unlistable), "resource type", "resource types") + " can't be listed by the provider, so desprawl only has those the inventory found.",
			Howto: fmt.Sprintf("If you need one that's missing, look up its import ID with `%s -type TYPE`\n"+
				"and add it with `%s -type TYPE -id ID`; then run `%s` again.", w.cmd("import-format"), w.cmd("add-target"), rerun),
			File: w.StatePath("targets.json"), Rerun: rerun, Brief: true,
			Listed: `"unlistable" in ` + w.StatePath("discovery.json"),
		}
		if !hasInventory {
			file, err := w.packFile("inventory.json")
			if err != nil {
				return nil, err
			}
			// Sources named after an unlistable type come first, all of
			// them; the rest only as a sample. Named ones whose schema
			// says how to read them get a complete entry of their own.
			srcs := suggest.InventorySources(schemas, rep.Unlistable)
			named := 0
			ready := ProblemGroup{
				Kind: "inventory-sources",
				Summary: "The pack has no inventory, so discover can't find resources of the types listed below that can't be listed.\n" +
					"These data sources are named after such types and can list them.",
				Howto: fmt.Sprintf("Add each source to \"sources\" in %s and its line under \"kinds\" (the type it lists),\n"+
					"then run `%s` again. Discover checks each with a sample import. Do this before adding targets one by one.", file, rerun),
				File: file, Rerun: rerun,
			}
			var rest []string
			for _, ds := range srcs {
				one := suggest.Singular(ds)
				if one == ds || !slices.Contains(rep.Unlistable, one) {
					break
				}
				named++
				s, ok := suggest.InventorySource(schemas, ds)
				if !ok {
					rest = append(rest, ds)
					continue
				}
				kind, _ := marshal(map[string]any{ds: inventory.Mapping{Type: one}})
				ready.Problems = append(ready.Problems, Problem{
					Subject: ds + " lists " + one,
					Detail:  []string{`under "kinds": ` + strings.TrimSuffix(strings.TrimPrefix(kind, "{"), "}")},
					Fix:     s,
				})
			}
			if len(ready.Problems) > 0 {
				out = append(out, ready)
			}
			rest = append(rest, srcs[named:min(len(srcs), named+10)]...)
			if len(rest) > 0 {
				g.Howto += fmt.Sprintf("\nThe pack has no inventory, which finds many of them at once: add a data source that reports\n"+
					"resources to \"sources\" in %s. Other data sources that may: %s.", file, strings.Join(rest, ", "))
			}
		}
		for _, typ := range rep.Unlistable {
			g.Problems = append(g.Problems, Problem{Subject: typ})
		}
		out = append(out, g)
		if rel := w.relatedUnlistable(schemas, pk, rep.Unlistable); len(rel.Problems) > 0 {
			out = append(out, rel)
		}
	}

	if len(inv.Unmapped) > 0 {
		file, err := w.packFile("inventory.json")
		if err != nil {
			return nil, err
		}
		g := ProblemGroup{
			Kind:    "unmapped-kinds",
			Summary: plural(len(inv.Unmapped), "kind of resource the inventory reported maps to no resource type, so it isn't imported.", "kinds of resource the inventory reported map to no resource type, so they aren't imported."),
			Howto: fmt.Sprintf("To import one, add its mapping under \"kinds\" in %s, then run `%s` again.\n"+
				"\"type\" is its resource type; \"id\" its import ID, \"{id}\" for the ID as reported.\n"+
				"A kind that isn't a resource of its own gets {\"note\": \"why\"} and no type.", file, rerun),
			File: file, Rerun: rerun,
		}
		for _, k := range slices.Sorted(maps.Keys(inv.Unmapped)) {
			p := Problem{Subject: k, Detail: []string{plural(inv.Unmapped[k], "found", "found")}}
			if ks, ok := inv.Kinds[k]; ok && ks.Sample != "" {
				p.Detail[0] += fmt.Sprintf(", e.g. ID %q", ks.Sample)
			}
			typ := "TYPE"
			if one := suggest.Singular(k); one != k && schemas.Resources[one] != nil {
				typ = one // a kind named after the type it lists
			}
			p.Key, p.Fix = k, map[string]string{"type": typ, "id": "{id}"}
			g.Problems = append(g.Problems, p)
		}
		out = append(out, g)
	}

	if len(inv.BadMappings) > 0 {
		file, err := w.packFile("inventory.json")
		if err != nil {
			return nil, err
		}
		g := ProblemGroup{
			Kind:    "mapping-failed",
			Summary: plural(len(inv.BadMappings), "kind mapping added to the pack doesn't import, so desprawl leaves its resources out.", "kind mappings added to the pack don't import, so desprawl leaves their resources out."),
			Howto:   fmt.Sprintf("Fix the mapping under \"kinds\" in %s (another type, or another import ID template), then run `%s` again.", file, rerun),
			File:    file, Rerun: rerun,
		}
		for _, k := range slices.Sorted(maps.Keys(inv.BadMappings)) {
			p := Problem{Subject: k, Detail: []string{"error: " + inv.BadMappings[k]}}
			if m, ok := pk.Inventory.Kinds[k]; ok && pk.Imports[m.Type] != nil && len(pk.Imports[m.Type].IDs) > 0 {
				p.Detail = append(p.Detail, "documented import IDs for "+m.Type+": "+strings.Join(quoteAll(pk.Imports[m.Type].IDs), ", "))
			}
			g.Problems = append(g.Problems, p)
		}
		out = append(out, g)
	}

	if len(inv.BadPartitions) > 0 {
		file, err := w.packFile("inventory.json")
		if err != nil {
			return nil, err
		}
		g := ProblemGroup{
			Kind:    "partition-failed",
			Summary: plural(len(inv.BadPartitions), "inventory source's partition doesn't do what it says, so desprawl reads the source without it.", "inventory sources' partitions don't do what they say, so desprawl reads the sources without them."),
			Howto:   fmt.Sprintf("Fix the source's \"partition\" in %s, then run `%s` again.", file, rerun),
			File:    file, Rerun: rerun,
		}
		for _, src := range slices.Sorted(maps.Keys(inv.BadPartitions)) {
			g.Problems = append(g.Problems, Problem{Subject: src, Detail: []string{"error: " + inv.BadPartitions[src]}})
		}
		out = append(out, g)
	}

	sourceByLabel := map[string]inventory.Source{}
	if pk.Inventory != nil {
		for _, src := range pk.Inventory.Sources {
			sourceByLabel[src.Label()] = src
		}
	}

	if len(inv.Truncated) > 0 {
		file, err := w.packFile("inventory.json")
		if err != nil {
			return nil, err
		}
		g := ProblemGroup{
			Kind:    "inventory-truncated",
			Summary: plural(len(inv.Truncated), "inventory source's result was cut off at a limit, so resources are missing.", "inventory sources' results were cut off at a limit, so resources are missing."),
			Howto: fmt.Sprintf("Give the source a \"partition\" in %s, so it's read again in parts: an argument\n"+
				"that narrows it to one kind (\"only\", an HCL expression with {kind}) or to all but some (\"except\" with {kinds},\n"+
				"each written as \"each\" with {kind}). Then run `%s` again; it checks the partition.", file, rerun),
			File: file, Rerun: rerun,
		}
		for _, label := range slices.Sorted(maps.Keys(inv.Truncated)) {
			tr := inv.Truncated[label]
			p := Problem{Subject: label}
			switch {
			case !tr.Partitioned:
				p.Detail = append(p.Detail, "it has no partition")
			case tr.Rest:
				p.Detail = append(p.Detail, "still cut off after partitioning")
			default:
				p.Detail = append(p.Detail, "kinds still cut off on their own read: "+strings.Join(tr.Cut, ", "))
			}
			if src, ok := sourceByLabel[label]; ok {
				if args := suggest.SourceArgs(schemas, src.DataSource); len(args) > 0 {
					p.Detail = append(p.Detail, "its optional arguments: "+strings.Join(args, ", "))
					if !tr.Partitioned {
						p.Fix = map[string]any{"partition": inventory.Partition{Arg: "ARG", Only: "\"{kind}\""}}
					}
				}
			}
			g.Problems = append(g.Problems, p)
		}
		out = append(out, g)
	}

	if pk.Inventory != nil {
		shipped := w.shippedInventory()
		discovered := map[string]bool{}
		for t := range rep.Listed {
			discovered[t] = true
		}
		file, err := w.packFile("inventory.json")
		if err != nil {
			return nil, err
		}
		g := ProblemGroup{
			Kind: "maybe-per-parent",
			Howto: fmt.Sprintf("If one only reports what's in one of them, read it once per parent: add \"parent\" and \"args\" to it\n"+
				"in %s, then run `%s` again; it checks that reading per parent misses nothing.", file, rerun),
			File: file, Rerun: rerun,
		}
		for _, src := range pk.Inventory.Sources {
			if src.Parent != "" || slices.ContainsFunc(shipped.Sources, func(o inventory.Source) bool { return o.Label() == src.Label() }) {
				continue
			}
			if arg, parent, attr, ok := suggest.PerParent(schemas, src.DataSource, discovered); ok {
				g.Problems = append(g.Problems, Problem{
					Subject: src.Label(),
					Detail:  []string{fmt.Sprintf("its argument %s may scope it to one %s", arg, parent)},
					Fix:     map[string]any{"parent": parent, "args": map[string]string{arg: attr}},
				})
			}
		}
		g.Summary = plural(len(g.Problems), "inventory source added to the pack may only report what's in one container (a resource group, a project).", "inventory sources added to the pack may only report what's in one container (a resource group, a project).")
		if len(g.Problems) > 0 {
			out = append(out, g)
		}
	}

	if len(inv.BadPerParent) > 0 {
		file, err := w.packFile("inventory.json")
		if err != nil {
			return nil, err
		}
		g := ProblemGroup{
			Kind:    "per-parent-failed",
			Summary: plural(len(inv.BadPerParent), "inventory source read per parent misses entries, so desprawl reads it once instead.", "inventory sources read per parent miss entries, so desprawl reads them once instead."),
			Howto:   fmt.Sprintf("Fix or remove its \"parent\" and \"args\" in %s, then run `%s` again.", file, rerun),
			File:    file, Rerun: rerun,
		}
		for _, src := range slices.Sorted(maps.Keys(inv.BadPerParent)) {
			g.Problems = append(g.Problems, Problem{Subject: src, Detail: []string{inv.BadPerParent[src]}})
		}
		out = append(out, g)
	}

	// A pack that excludes nothing hasn't been judged for types whose
	// instances are data rather than infrastructure; the most numerous
	// discovered types are where those show.
	if pk.ExcludeTypes == nil && len(rep.Listed) > 0 {
		types := slices.Collect(maps.Keys(rep.Listed))
		slices.SortFunc(types, func(a, b string) int { return rep.Listed[b] - rep.Listed[a] })
		var big []string
		for _, t := range types[:min(len(types), 10)] {
			if rep.Listed[t] >= 20 {
				big = append(big, t)
			}
		}
		if len(big) > 0 {
			file, err := w.packFile("pack.json")
			if err != nil {
				return nil, err
			}
			g := ProblemGroup{
				Kind:    "exclude-candidates",
				Summary: "The pack excludes no resource types. These have the most resources; some may be data rather than infrastructure.",
				Howto: fmt.Sprintf("Exclude types whose instances are data (objects in a bucket, items in a table, messages, log streams),\n"+
					"secret values or credentials (secret versions, access keys), or provider-owned defaults: add them under\n"+
					"\"exclude_types\" in %s, then run `%s` again. Never exclude infrastructure people configure.\n"+
					"If none should be, set \"exclude_types\": {} so this isn't printed again.", file, rerun),
				File: file, Rerun: rerun,
			}
			for _, t := range big {
				g.Problems = append(g.Problems, Problem{Subject: t, Detail: []string{plural(rep.Listed[t], "resource", "resources")}, Key: t, Fix: "why it isn't infrastructure"})
			}
			out = append(out, g)
		}
	}

	if len(inv.Errors) > 0 {
		file, err := w.packFile("inventory.json")
		if err != nil {
			return nil, err
		}
		g := ProblemGroup{
			Kind:    "inventory-failed",
			Summary: plural(len(inv.Errors), "inventory data source", "inventory data sources") + " failed to read.",
			Howto:   fmt.Sprintf("Fix or remove the source in \"sources\" in %s, then run `%s` again.", file, rerun),
			File:    file, Rerun: rerun,
		}
		for _, ds := range slices.Sorted(maps.Keys(inv.Errors)) {
			g.Problems = append(g.Problems, Problem{Subject: ds, Detail: []string{"error: " + oneLine(inv.Errors[ds])}})
		}
		out = append(out, g)
	}
	return out, nil
}

// GenerateProblems: resources left out of the generated config over a plan
// error. Those left out on purpose (an override's drop, the pack's skip
// filters) aren't problems.
func (w Workspace) GenerateProblems() ([]ProblemGroup, error) {
	var dropped []generate.Dropped
	if err := w.ReadJSON("left-out.json", &dropped); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	file := w.rel(".desprawl", "overrides.json")
	rerun := w.cmd("generate")
	g := ProblemGroup{
		Kind:    "not-generated",
		Summary: "",
		Howto: fmt.Sprintf("To include one, add its fix to the object in %s, then run `%s` again.\n"+
			"To leave one out on purpose, add {\"exclude\": true} for it.", file, rerun),
		File: file, Rerun: rerun,
	}
	for _, d := range dropped {
		if d.Intended {
			continue
		}
		p := Problem{Subject: d.Address(), Detail: []string{"error: " + oneLine(d.Reason)}}
		if h := d.Hint; h != nil {
			if len(h.Set) > 0 {
				p.Detail = append(p.Detail, "its config sets: "+strings.Join(h.Set, ", "))
			}
			if h.Why != "" {
				p.Detail = append(p.Detail, h.Why+":")
			}
			if h.Override != nil {
				p.Key, p.Fix = d.Address(), h.Override
			}
		}
		g.Problems = append(g.Problems, p)
	}
	g.Summary = plural(len(g.Problems), "resource was", "resources were") + " left out of the generated config."
	return []ProblemGroup{g}, nil
}

// LinkProblems: literals that may name another resource, and scope values
// (account, project, region) the pack has no data source for.
func (w Workspace) LinkProblems(schemas *tf.Schemas, opts link.Options, parametrize bool) ([]ProblemGroup, error) {
	return w.perProvider(func(w Workspace) ([]ProblemGroup, error) { return w.linkProblemsOf(schemas, opts, parametrize) })
}

func (w Workspace) linkProblemsOf(schemas *tf.Schemas, opts link.Options, parametrize bool) ([]ProblemGroup, error) {
	src, err := os.ReadFile(w.StatePath("generated.tf"))
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	pb, err := os.ReadFile(w.StatePath("plan.json"))
	if err != nil {
		return nil, err
	}
	plan, err := tf.ParsePlan(pb)
	if err != nil {
		return nil, err
	}
	all, err := suggest.Candidates(src, plan, schemas, opts)
	if err != nil {
		return nil, err
	}
	pk, err := w.Pack()
	if err != nil {
		return nil, err
	}
	rejected := map[string]bool{}
	for _, r := range pk.Rules {
		if r.Rejected {
			rejected[r.Source+" "+r.Path+" "+r.Target+" "+r.Attr] = true
		}
	}
	// The same setting on two resources (location = location, sku = sku)
	// is configuration they share, not a reference; a name or ID is. And
	// what someone already judged isn't one isn't suggested again.
	// Candidates resting on one plain word ("default" naming both an event
	// bus and a security group) are mostly coincidences: they're counted,
	// not listed.
	var cands []suggest.Candidate
	weak := 0
	for _, c := range all {
		if rejected[c.Source+" "+c.Path+" "+c.Target+" "+c.Attr] || !w.mine(c.Source) {
			continue
		}
		segs := strings.Split(c.Path, ".")
		if last := segs[len(segs)-1]; last == c.Attr && c.Attr != "name" && c.Attr != "id" {
			continue
		}
		if c.Weak() {
			weak++
			continue
		}
		cands = append(cands, c)
	}
	// Likeliest first: values of an identifying attribute (name, id, *_arn)
	// before settings that happen to be equal ("Disabled", "IPv4"), then
	// the more different values the better, then the more places.
	slices.SortStableFunc(cands, func(a, b suggest.Candidate) int {
		if ia, ib := identifying(a.Attr), identifying(b.Attr); ia != ib {
			if ia {
				return -1
			}
			return 1
		}
		if a.Values != b.Values {
			return b.Values - a.Values
		}
		return b.Count - a.Count
	})
	rerun := w.cmd("link")
	var out []ProblemGroup
	if len(cands) > 0 {
		file, err := w.packFile("rules.llm.json")
		if err != nil {
			return nil, err
		}
		g := ProblemGroup{
			Kind:    "maybe-reference",
			Summary: plural(len(cands), "kind of value was left as text because desprawl isn't sure it refers to a resource.", "kinds of value were left as text because desprawl isn't sure they refer to a resource."),
			Howto: fmt.Sprintf("If one does, add its rule to the list in %s, then run `%s` again;\n"+
				"its values become references, and connections on the map. If one doesn't, add its rule with\n"+
				"\"rejected\": true, so it isn't suggested again. Rules are shared with everyone using the pack.", file, rerun),
			File: file, Rerun: rerun,
		}
		if weak > 0 {
			g.Summary += " " + plural(weak, "more rests only on one plain word (\"default\") equal to a name, likely by chance; it isn't listed.", "more rest only on one plain word (\"default\") equal to a name, likely by chance; they aren't listed.")
		}
		for _, c := range cands {
			match := c.Match
			if match == "" {
				match = rules.MatchExact
			}
			how := "equals"
			if match == rules.MatchEmbedded {
				how = "contains"
			}
			p := Problem{
				Subject: c.Source + "." + c.Path,
				Detail: []string{
					fmt.Sprintf("%s %s.%s in %s, e.g. %s", how, c.Target, c.Attr, plural(c.Count, "place", "places"), strings.Join(quoteAll(shorten(c.Examples, 90)), ", ")),
				},
			}
			p.Fix = rules.Rule{Source: c.Source, Path: c.Path, Target: c.Target, Attr: c.Attr, Match: match, Origin: rules.OriginLLM, Note: "why it refers to it"}
			g.Problems = append(g.Problems, p)
		}
		out = append(out, g)
	}
	if pk.ExcludeAttributes == nil && (w.current == nil || w.firstOf(w.current.Name) == w.current.Ref()) {
		// A pack that excludes no attributes hasn't been judged for computed mirrors
		// of other attributes; those in this config are worth a look.
		present := map[string]bool{}
		for _, rc := range plan.ResourceChanges {
			if !w.mine(rc.Type) {
				continue // another provider's pack judges its attributes
			}
			if vals, err := rc.Values(); err == nil {
				for k, v := range vals {
					if v != nil {
						present[k] = true
					}
				}
			}
		}
		var drops []string
		for _, a := range suggest.DropCandidates(schemas) {
			if present[a] {
				drops = append(drops, a)
			}
		}
		if len(drops) > 0 {
			file, err := w.packFile("pack.json")
			if err != nil {
				return nil, err
			}
			g := ProblemGroup{
				Kind:    "attributes-to-exclude",
				Summary: "The pack excludes no attributes. These computed ones are on many resource types; some may mirror others.",
				Howto: fmt.Sprintf("Exclude those that mirror other attributes and that nobody writes (all tags including provider defaults,\n"+
					"effective labels): add them to \"exclude_attributes\" in %s, then run `%s` again.\n"+
					"Keep attributes that carry real configuration. If none should be excluded, set \"exclude_attributes\": []\n"+
					"so this isn't printed again.", file, rerun),
				File: file, Rerun: rerun, Brief: true, Listed: "the provider's schema",
			}
			for _, a := range drops {
				g.Problems = append(g.Problems, Problem{Subject: a, Fix: a})
			}
			out = append(out, g)
		}
	}
	if parametrize {
		var rep discover.Report
		w.ReadJSON("discovery.json", &rep)
		var missing []string
		for _, name := range slices.Sorted(maps.Keys(w.params)) {
			_, ok := pk.Params[name]
			if _, wrong := rep.BadParams[name]; !ok || wrong {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			file, err := w.packFile("pack.json")
			if err != nil {
				return nil, err
			}
			g := ProblemGroup{
				Kind:    "scope-literal",
				Summary: plural(len(missing), "scope value was", "scope values were") + " left as text, so the config only works in this account.",
				Howto: fmt.Sprintf("To read one from a data source instead, add or fix it under \"params\" in %s,\n"+
					"then run `%s` (which checks that the data source returns the value) and `%s` again.", file, w.cmd("discover"), rerun),
				File: file, Rerun: w.cmd("discover"),
			}
			for _, name := range missing {
				p := Problem{Subject: name, Detail: []string{fmt.Sprintf("value: %q", w.params[name])}}
				if why := rep.BadParams[name]; why != "" {
					p.Detail = append(p.Detail, "the pack's data source is wrong: "+why)
				}
				if srcs := suggest.ParamSources(schemas, name); len(srcs) > 0 {
					if len(srcs) > 1 {
						p.Detail = append(p.Detail, "data sources that may return it: "+strings.Join(srcs[:min(len(srcs), 5)], ", "))
					}
					p.Key, p.Fix = name, map[string]string{
						"ref":  "data." + srcs[0] + ".current." + name,
						"data": fmt.Sprintf("data %q \"current\" {}", srcs[0]),
					}
				}
				g.Problems = append(g.Problems, p)
			}
			out = append(out, g)
		}
	}
	return out, nil
}

// VerifyProblems: resources the linked config would change. Differences
// only in config (attributes the cloud doesn't store) aren't problems.
func (w Workspace) VerifyProblems() ([]ProblemGroup, error) {
	var drift []verify.Drift
	if err := w.ReadJSON("verify.json", &drift); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	file := w.rel(".desprawl", "overrides.json")
	g := ProblemGroup{
		Kind: "differs",
		Howto: fmt.Sprintf("Nothing in the cloud changed: desprawl only reads. If you only want the map, you're done.\n"+
			"To make the config match the cloud exactly, fix the attributes in %s\n"+
			"(\"set\" the right value, or \"ignore_changes\" to accept the difference), then run `%s`, `%s` and `%s` again.", file, w.cmd("generate"), w.cmd("link"), w.cmd("verify")),
		File: file, Rerun: w.cmd("generate"),
	}
	for _, d := range drift {
		if d.ConfigOnly {
			continue
		}
		p := Problem{Subject: d.Address, Detail: []string{differs(d)}}
		var top []string
		for _, path := range d.Paths {
			if t := strings.SplitN(path, ".", 2)[0]; !slices.Contains(top, t) {
				top = append(top, t)
			}
		}
		if len(top) > 0 {
			p.Key, p.Fix = d.Address, map[string][]string{"ignore_changes": top}
		}
		g.Problems = append(g.Problems, p)
	}
	g.Summary = plural(len(g.Problems), "resource's config doesn't quite match what's in the cloud yet.", "resources' config doesn't quite match what's in the cloud yet.")
	return []ProblemGroup{g}, nil
}

// differs says how a resource's config differs from the cloud, in
// desprawl's words rather than the plan's actions (update, replace),
// which read as if something were about to change.
func differs(d verify.Drift) string {
	what := "differs from the cloud"
	if slices.Contains(d.Actions, "delete") || slices.Contains(d.Actions, "create") {
		what = "differs in an attribute terraform can only set when it creates the resource"
	}
	if len(d.Paths) == 0 {
		return what
	}
	return what + ": " + strings.Join(d.Paths, ", ")
}

// AllProblems is every stage's problems, in stage order.
func (w Workspace) AllProblems(schemas *tf.Schemas) ([]ProblemGroup, error) {
	d, err := w.DiscoverProblems(schemas)
	if err != nil {
		return nil, err
	}
	g, err := w.GenerateProblems()
	if err != nil {
		return nil, err
	}
	opts, err := w.LinkOptions([]string{"pack"}, true)
	if err != nil {
		return nil, err
	}
	l, err := w.LinkProblems(schemas, opts, true)
	if err != nil {
		return nil, err
	}
	v, err := w.VerifyProblems()
	if err != nil {
		return nil, err
	}
	return slices.Concat(d, g, l, v, w.LeakProblems(schemas)), nil
}

// shorten cuts long values (ARNs, resource paths) to n bytes.
func shorten(xs []string, n int) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = truncate(x, n)
	}
	return out
}

// identifying reports an attribute named like an identifier.
func identifying(attr string) bool {
	if attr == "name" || attr == "id" || attr == "arn" {
		return true
	}
	for _, suffix := range []string{"_name", "_id", "_arn", "_prefix", "_url", "_uri", "fqdn", "endpoint"} {
		if strings.HasSuffix(attr, suffix) {
			return true
		}
	}
	return false
}

func quoteAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = fmt.Sprintf("%q", x)
	}
	return out
}
