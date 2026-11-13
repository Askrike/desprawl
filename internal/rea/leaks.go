package rea

// Account data in provider packs: packs are shared, and fixes to them are
// made from real accounts.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/askrike/desprawl/internal/privacy"
	"github.com/askrike/desprawl/internal/tf"
	"github.com/askrike/desprawl/providers"
)

// packFixFiles are the pack's files that fixes from accounts go into.
var packFixFiles = []string{"rules.llm.json", "aliases.json", "list_args.json", "pack.json", "inventory.json"}

// guard returns the privacy guard for pack files inferred from this
// workspace: everything the workspace knows about the account is private,
// unless the provider's schema or docs use the same word.
func (w Workspace) guard(schemas *tf.Schemas) *privacy.Guard {
	var account []string
	for _, name := range []string{"plan.json", "generated.tf", "targets.json", "inventory.json", "discovery.json"} {
		if b, err := os.ReadFile(w.StatePath(name)); err == nil {
			account = append(account, string(b))
		}
	}
	var public []string
	for _, name := range []string{"vocabulary.docs.txt", "rules.docs.json", "imports.docs.json"} {
		if b, err := providers.ReadFile(w.Packs(), w.Provider().Name, name); err == nil {
			public = append(public, string(b))
		}
	}
	return privacy.NewGuard(account, schemas, public...)
}

// LeakProblems: account data in what was added to the pack.
func (w Workspace) LeakProblems(schemas *tf.Schemas) []ProblemGroup {
	leaks := w.packLeaks(schemas)
	if len(leaks) == 0 {
		return nil
	}
	return []ProblemGroup{{
		Kind:     "pack-account-data",
		Summary:  plural(len(leaks), "line of the provider's pack mentions", "lines of the provider's pack mention") + " this account's data. Packs are shared with everyone using them.",
		Howto:    "Take the account's values (names, IDs) out of each line; a rule or mapping should hold only what the provider's docs use.",
		Problems: leaks,
	}}
}

// packLeaks finds lines added to the pack's files (not in the shipped
// ones, which were reviewed) that mention account data: words of this
// workspace's values that the provider's docs and schema don't use.
func (w Workspace) packLeaks(schemas *tf.Schemas) []Problem {
	var out []Problem
	seen := map[string]bool{}
	for _, p := range w.Config.Providers {
		if !seen[p.Name] {
			seen[p.Name] = true
			out = append(out, w.scoped(p).packLeaksOf(schemas)...)
		}
	}
	return out
}

func (w Workspace) packLeaksOf(schemas *tf.Schemas) []Problem {
	g := w.guard(schemas)
	var out []Problem
	for _, name := range packFixFiles {
		b, err := providers.ReadFile(w.Packs(), w.Provider().Name, name)
		if err != nil {
			continue
		}
		shipped := map[string]bool{}
		if sb, err := providers.ReadFile("", w.Provider().Name, name); err == nil {
			for _, line := range strings.Split(string(sb), "\n") {
				shipped[strings.TrimSpace(line)] = true
			}
		}
		for i, line := range strings.Split(string(b), "\n") {
			if shipped[strings.TrimSpace(line)] {
				continue
			}
			if leaks := g.Leaks(line); len(leaks) > 0 {
				out = append(out, Problem{
					Subject: fmt.Sprintf("%s:%d", filepath.Join(w.Packs(), w.Provider().Name, name), i+1),
					Detail:  []string{strings.TrimSpace(truncate(line, 160)), "account data: " + strings.Join(leaks, ", ")},
				})
			}
		}
	}
	return out
}

// CheckPack scans the pack files fixed from accounts, as loaded (the
// workspace's, else shipped), for this workspace's account data, line by
// line: "<file>:<line>: [tokens]" for each line that mentions some.
func (w Workspace) CheckPack(schemas *tf.Schemas) []string {
	g := w.guard(schemas)
	var out []string
	seen := map[string]bool{}
	for _, p := range w.Config.Providers {
		if seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		for _, name := range packFixFiles {
			b, err := providers.ReadFile(w.Packs(), p.Name, name)
			if err != nil {
				continue
			}
			for i, line := range strings.Split(string(b), "\n") {
				if leaks := g.Leaks(line); len(leaks) > 0 {
					out = append(out, fmt.Sprintf("%s:%d: %v", filepath.Join(p.Name, name), i+1, leaks))
				}
			}
		}
	}
	return out
}
