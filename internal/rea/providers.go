package rea

// Several providers in one workspace: each provider block (two regions of
// one provider are two blocks, one aliased) is discovered on its own, with
// its provider's pack and its own scope (account, project, region); the
// targets it finds remember their block, so they're imported with it; and
// generate and link work on all of them at once, so references between
// providers (a DNS record holding a load balancer's hostname) are linked.

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/inventory"
	"github.com/askrike/desprawl/internal/mine"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/providers"
)

// scoped is the workspace as one provider block sees it: provider() is that
// block, pack() its provider's pack, params its targets' scope (before
// the first discovery, the scope its config sets).
func (w Workspace) scoped(p model.Provider) Workspace {
	w.current = &p
	var targets []model.Target
	w.ReadJSON("targets.json", &targets)
	w.params = w.detectParams(ofProvider(targets, w.Config, p.Ref()))
	return w
}

// detectParams are the scope parameters of targets (discover.Params), and
// those the provider block's config sets that no target carries.
func (w Workspace) detectParams(targets []model.Target) map[string]string {
	out := discover.Params(targets)
	for k, v := range w.configParams() {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

// configParams are the scope parameters the provider block's config sets:
// for each the pack declares, the setting its ref reads
// (data.google_client_config.current.region: region), so a region set on
// the provider scopes listing before anything that carries it is listed.
// Unscoped with several blocks: none.
func (w Workspace) configParams() map[string]string {
	if w.current == nil && len(w.Config.Providers) != 1 {
		return nil
	}
	p := w.Provider()
	settings := p.Settings()
	if len(settings) == 0 {
		return nil
	}
	b, err := providers.ReadFile(w.Packs(), p.Name, "pack.json")
	if err != nil {
		return nil
	}
	var c providers.Config
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	out := map[string]string{}
	for name, pp := range c.Params {
		if v := settings[pp.Ref[strings.LastIndex(pp.Ref, ".")+1:]]; v != "" {
			out[name] = v
		}
	}
	return out
}

// ofProvider are the targets imported with the provider block ref; one
// without a block is the first block's.
func ofProvider(targets []model.Target, cfg model.Config, ref string) []model.Target {
	var out []model.Target
	for _, t := range targets {
		if t.Provider == ref || t.Provider == "" && len(cfg.Providers) > 0 && cfg.Providers[0].Ref() == ref {
			out = append(out, t)
		}
	}
	return out
}

// providerOf is the provider a resource type belongs to: the block whose
// local name is the type's prefix (aws_s3_bucket: aws), else the first.
func (w Workspace) ProviderOf(typ string) model.Provider {
	prefix := strings.SplitN(typ, "_", 2)[0]
	for _, p := range w.Config.Providers {
		if p.Name == prefix {
			return p
		}
	}
	return w.Provider()
}

// pack is the scoped provider's pack, or, unscoped, every provider's
// together (generate and link use them all at once).
func (w Workspace) Pack() (*providers.Pack, error) {
	if w.current != nil || len(w.Config.Providers) <= 1 {
		return providers.Load(w.Packs(), w.Provider().Name)
	}
	merged := &providers.Pack{Imports: map[string]*mine.ImportExamples{}}
	seen := map[string]bool{}
	for _, p := range w.Config.Providers {
		if seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		pk, err := providers.Load(w.Packs(), p.Name)
		if err != nil {
			return nil, err
		}
		mergePack(merged, pk)
	}
	return merged, nil
}

// mergePack adds b to a. Types are named after their provider, so entries
// of two providers don't meet; scope parameters of the same name (two
// providers' "region") keep the first's, and link takes each provider's
// own (linkOptions).
func mergePack(a, b *providers.Pack) {
	a.Rules = append(a.Rules, b.Rules...)
	a.Aliases = append(a.Aliases, b.Aliases...)
	a.ListArgs = append(a.ListArgs, b.ListArgs...)
	maps.Copy(a.Imports, b.Imports)
	if b.Inventory != nil {
		if a.Inventory == nil {
			a.Inventory = &inventory.Inventory{Kinds: map[string]inventory.Mapping{}}
		}
		a.Inventory.Sources = append(a.Inventory.Sources, b.Inventory.Sources...)
		for k, m := range b.Inventory.Kinds {
			if _, ok := a.Inventory.Kinds[k]; !ok {
				a.Inventory.Kinds[k] = m
			}
		}
	}
	if b.ExcludeTypes != nil {
		if a.ExcludeTypes == nil {
			a.ExcludeTypes = map[string]string{}
		}
		maps.Copy(a.ExcludeTypes, b.ExcludeTypes)
	}
	if b.Params != nil {
		if a.Params == nil {
			a.Params = map[string]providers.Param{}
		}
		for k, v := range b.Params {
			if _, ok := a.Params[k]; !ok {
				a.Params[k] = v
			}
		}
	}
	if b.ExcludeAttributes != nil {
		a.ExcludeAttributes = append(a.ExcludeAttributes, b.ExcludeAttributes...)
		if a.ExcludeAttributes == nil {
			a.ExcludeAttributes = []string{}
		}
	}
	a.Skip = append(a.Skip, b.Skip...)
	if b.ManagedBy != nil {
		if a.ManagedBy == nil {
			a.ManagedBy = map[string][]string{}
		}
		maps.Copy(a.ManagedBy, b.ManagedBy)
	}
}

// mergeReport adds one provider block's discovery to the workspace's.
func mergeReport(a, b *discover.Report) {
	add := func(dst *map[string]int, src map[string]int) {
		if *dst == nil {
			*dst = map[string]int{}
		}
		for k, v := range src {
			(*dst)[k] += v
		}
	}
	add(&a.Listed, b.Listed)
	add(&a.Collapsed, b.Collapsed)
	add(&a.Inventory, b.Inventory)
	if a.Failed == nil {
		a.Failed = map[string]string{}
	}
	maps.Copy(a.Failed, b.Failed)
	union := func(x, y []string) []string {
		out := slices.Concat(x, y)
		slices.Sort(out)
		return slices.Compact(out)
	}
	a.NeedsArgs = union(a.NeedsArgs, b.NeedsArgs)
	a.Truncated = union(a.Truncated, b.Truncated)
	a.Slow = union(a.Slow, b.Slow)
	a.Unlistable = union(a.Unlistable, b.Unlistable)
	for k, v := range b.Superseded {
		if a.Superseded == nil {
			a.Superseded = map[string][]model.Target{}
		}
		a.Superseded[k] = append(a.Superseded[k], v...)
	}
	for k, v := range b.Held {
		if a.Held == nil {
			a.Held = map[string][]model.Target{}
		}
		a.Held[k] = append(a.Held[k], v...)
	}
}

// mergeInventory adds one provider block's inventory read to the
// workspace's.
func mergeInventory(a, b *inventoryReport) {
	if a.Kinds == nil {
		a.Kinds = map[string]inventory.KindSummary{}
	}
	for k, v := range b.Kinds {
		if _, ok := a.Kinds[k]; !ok {
			a.Kinds[k] = v
		}
	}
	addInts := func(dst *map[string]int, src map[string]int) {
		if len(src) == 0 {
			return
		}
		if *dst == nil {
			*dst = map[string]int{}
		}
		for k, v := range src {
			(*dst)[k] += v
		}
	}
	addInts(&a.Unmapped, b.Unmapped)
	addInts(&a.Misfit, b.Misfit)
	addStrs := func(dst *map[string]string, src map[string]string) {
		if len(src) == 0 {
			return
		}
		if *dst == nil {
			*dst = map[string]string{}
		}
		maps.Copy(*dst, src)
	}
	addStrs(&a.Errors, b.Errors)
	addStrs(&a.BadPartitions, b.BadPartitions)
	addStrs(&a.BadMappings, b.BadMappings)
	addStrs(&a.BadPerParent, b.BadPerParent)
	if len(b.Truncated) > 0 {
		if a.Truncated == nil {
			a.Truncated = map[string]inventory.Truncation{}
		}
		maps.Copy(a.Truncated, b.Truncated)
	}
}

// scopedParam is one scope parameter as link references it: through its
// provider block's data source, renamed apart for an aliased block
// (data "aws_region" "current_us" { provider = aws.us }).
func scopedParam(ref string, p providers.Param) (refExpr, data string) {
	_, alias, ok := strings.Cut(ref, ".")
	if !ok {
		return p.Ref, p.Data
	}
	// data.<type>.<name>.<attr> and data "<type>" "<name>" {}
	parts := strings.SplitN(p.Ref, ".", 4)
	if len(parts) != 4 {
		return p.Ref, p.Data
	}
	name := parts[2] + "_" + alias
	refExpr = strings.Join([]string{parts[0], parts[1], name, parts[3]}, ".")
	data = strings.Replace(p.Data, `"`+parts[2]+`" {`, `"`+name+`" {`+"\n  provider = "+ref+"\n", 1)
	return refExpr, data
}
