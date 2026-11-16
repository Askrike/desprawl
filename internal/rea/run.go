package rea

// Running the stages on a workspace.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/generate"
	"github.com/askrike/desprawl/internal/inventory"
	"github.com/askrike/desprawl/internal/link"
	"github.com/askrike/desprawl/internal/mine"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/suggest"
	"github.com/askrike/desprawl/internal/tf"
	"github.com/askrike/desprawl/internal/verify"
	"github.com/askrike/desprawl/providers"
)

var DiscoverDefaults = discover.Options{Parallel: 4, Timeout: 2 * time.Minute, Limit: 1000}

func (w Workspace) Discover(types, exclude []string, opts discover.Options) (err error) {
	if err := w.ready(context.Background()); err != nil {
		return err
	}
	end := startProgress(w, "discover")
	defer func() { end(err) }()
	ctx := context.Background()
	schemas, err := w.Schemas(ctx)
	if err != nil {
		return err
	}
	// Keep targets added by hand or by an agent across re-discovery.
	var old, added []model.Target
	if err := w.ReadJSON("targets.json", &old); err == nil {
		for _, t := range old {
			if t.Added {
				added = append(added, t)
			}
		}
	}
	// Each provider block on its own: its pack, its scope.
	var targets []model.Target
	rep := &discover.Report{}
	var inv *inventoryReport
	for _, prov := range w.Config.Providers {
		pw := w.scoped(prov)
		if len(w.Config.Providers) > 1 {
			Log.Info("discovering with provider %s", prov.Ref())
		}
		ts, r, ir, err := pw.discoverProvider(ctx, schemas, types, exclude, opts, old, ofProvider(added, w.Config, prov.Ref()))
		if err != nil {
			return fmt.Errorf("provider %s: %w", prov.Ref(), err)
		}
		for i := range ts {
			ts[i].Provider = prov.Ref()
		}
		targets = append(targets, ts...)
		mergeReport(rep, r)
		if ir != nil {
			if inv == nil {
				inv = &inventoryReport{}
			}
			mergeInventory(inv, ir)
		}
	}
	if inv != nil {
		if err := w.WriteJSON("inventory.json", inv); err != nil {
			return err
		}
	}
	targets = discover.Rename(append(targets, added...))
	w.params = w.detectParams(targets)
	if err := w.WriteJSON("targets.json", targets); err != nil {
		return err
	}
	if rep.BadParams, err = w.checkParams(ctx, schemas); err != nil {
		return err
	}
	if err := w.WriteJSON("discovery.json", rep); err != nil {
		return err
	}
	Log.Info("found %d resources of %d types", len(targets), len(rep.Listed))
	if len(w.params) > 0 {
		var kv []string
		for _, k := range slices.Sorted(maps.Keys(w.params)) {
			kv = append(kv, k+" "+w.params[k])
		}
		Log.Verbose("the resources share %s", strings.Join(kv, ", "))
	}
	return nil
}

// discoverProvider discovers with w's provider block: the types it lists
// and what its pack's inventory reports. old are the targets of the last
// discovery (parents for per-parent reads), added those added by hand for
// this block.
func (w Workspace) discoverProvider(ctx context.Context, schemas *tf.Schemas, types, exclude []string, opts discover.Options, old, added []model.Target) ([]model.Target, *discover.Report, *inventoryReport, error) {
	p, err := w.Pack()
	if err != nil {
		return nil, nil, nil, err
	}
	// Entries that don't fit the schema are left out (and printed).
	p, _ = w.checkPack(schemas, p)
	opts.Types = set(types)
	opts.Exclude = set(exclude)
	for t := range p.ExcludeTypes {
		opts.Exclude[t] = true
	}
	// Only this provider's types: another block lists its own.
	for t := range schemas.Lists {
		if w.ProviderOf(t).Name != w.Provider().Name {
			opts.Exclude[t] = true
		}
	}
	opts.ProviderAttrs = p.QueryProvider
	opts.Provider = w.Provider().Ref()
	d := discover.Discoverer{WS: w.Workspace, TF: w.runner(), Schemas: schemas, Specs: p.ListArgs, Added: added, Params: w.configParams(), Opts: opts, Log: Log}
	inv := p.Inventory
	if inv != nil && len(inv.Sources) == 0 {
		inv = nil
	}
	if inv != nil {
		// Read with this block (an aliased one names itself).
		c := *inv
		c.Sources = slices.Clone(inv.Sources)
		for i := range c.Sources {
			c.Sources[i].Provider = w.Provider().Ref()
		}
		inv = &c
	}
	var badPartitions, badPerParent map[string]string
	if inv != nil {
		badPartitions = w.checkPartitions(ctx, inv, ofProvider(old, w.Config, w.Provider().Ref()))
		badPerParent = w.checkPerParent(ctx, inv, ofProvider(old, w.Config, w.Provider().Ref()))
	}
	type invRead struct {
		params           map[string]string
		entries          []inventory.Entry
		found            []model.Target
		unmapped, misfit map[string]int
		truncated        map[string]inventory.Truncation
		errs             map[string]error
	}
	readInventory := func(targets []model.Target) invRead {
		r := invRead{params: w.detectParams(targets)}
		r.entries, r.found, r.unmapped, r.misfit, r.truncated, r.errs = inv.Discover(ctx, w.Workspace, w.runner(), r.params, targets)
		return r
	}
	// Inventory needs no listed type unless it reads per instance of one,
	// so it is read while types are listed.
	var early chan invRead
	if inv != nil && !slices.ContainsFunc(inv.Sources, func(s inventory.Source) bool { return s.Parent != "" }) {
		Log.Verbose("reading inventory while types are listed")
		early = make(chan invRead, 1)
		go func() { early <- readInventory(added) }()
	}
	targets, rep, err := d.Run(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var invRep *inventoryReport
	if inv != nil {
		all := slices.Concat(targets, added)
		var r invRead
		if early != nil {
			r = <-early
		}
		if early == nil || !maps.Equal(inv.Uses(r.params), inv.Uses(w.detectParams(all))) {
			// Listing changed a scope parameter the sources use: read again.
			r = readInventory(all)
		}
		entries, found, unmapped, misfit, truncated, errs := r.entries, r.found, r.unmapped, r.misfit, r.truncated, r.errs
		found, badMappings, err := w.checkMappings(ctx, inv, entries, found)
		if err != nil {
			return nil, nil, nil, err
		}
		for ds, err := range errs {
			Log.Debug("inventory source %s failed: %v", ds, err)
		}
		for ds, tr := range truncated {
			logTruncation(ds, tr)
		}
		if targets, err = d.Fill(ctx, targets, rep, found); err != nil {
			return nil, nil, nil, err
		}
		invRep = &inventoryReport{Kinds: inventory.Kinds(entries), Unmapped: unmapped, Misfit: misfit, Errors: errStrings(errs), Truncated: truncated,
			BadPartitions: badPartitions, BadMappings: badMappings, BadPerParent: badPerParent}
	}
	return targets, rep, invRep, nil
}

// checkParams checks the pack's data source for each discovered scope
// parameter (account, project, region): it must fit the schema, and a plan
// reading it must return the discovered value. It returns those that
// don't, with why. A plan that fails outright checks nothing.
func (w Workspace) checkParams(ctx context.Context, schemas *tf.Schemas) (map[string]string, error) {
	bad := map[string]string{}
	var src strings.Builder
	declared := map[string]bool{}
	type check struct{ key, value, ref string }
	var checks []check
	for i, sp := range w.scopeParams() {
		if err := suggest.CheckParam(schemas, sp.pack.Ref, sp.pack.Data); err != nil {
			bad[sp.key] = err.Error()
			continue
		}
		ref, data := scopedParam(sp.provider, sp.pack)
		if !declared[data] {
			declared[data] = true
			src.WriteString(data + "\n")
		}
		out := fmt.Sprintf("p%d", i)
		fmt.Fprintf(&src, "output %q { value = %s }\n", out, ref)
		checks = append(checks, check{sp.key, sp.value, out})
	}
	if len(checks) > 0 {
		dir, err := w.TFDir("param-check")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(src.String()), 0o644); err != nil {
			return nil, err
		}
		outs, diags, err := w.runner().In(dir).PlanOutputs(ctx)
		if err != nil || len(diags) > 0 {
			Log.Info("couldn't check the scope parameters' data sources: %v %s", err, joinDiags(diags))
			return nil, nil
		}
		for _, c := range checks {
			var v string
			json.Unmarshal(outs[c.ref], &v)
			if v != c.value {
				bad[c.key] = fmt.Sprintf("its data source returns %q, not the discovered %q", v, c.value)
			}
		}
	}
	for key, why := range bad {
		Log.Debug("scope parameter %s stays literal: %s", key, why)
	}
	return bad, nil
}

// scopeParam is a scope parameter of one provider block that its pack can
// reference through a data source. key names it in reports: the
// parameter, with its block when the workspace has several.
type scopeParam struct {
	key, name, value, provider string
	pack                       providers.Param
}

// scopeParams are every provider block's scope parameters its pack has a
// data source for.
func (w Workspace) scopeParams() []scopeParam {
	var out []scopeParam
	for _, prov := range w.Config.Providers {
		pw := w.scoped(prov)
		pk, err := pw.Pack()
		if err != nil {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(pw.params)) {
			pp, ok := pk.Params[name]
			if !ok {
				continue
			}
			key := name
			if len(w.Config.Providers) > 1 {
				key = name + " (" + prov.Ref() + ")"
			}
			out = append(out, scopeParam{key: key, name: name, value: pw.params[name], provider: prov.Ref(), pack: pp})
		}
	}
	return out
}

// inventoryReport is .desprawl/inventory.json: what inventory sources
// reported, for discover's problems and for filling gaps by hand.
type inventoryReport struct {
	Kinds    map[string]inventory.KindSummary `json:"kinds"`
	Unmapped map[string]int                   `json:"unmapped,omitempty"`
	Misfit   map[string]int                   `json:"misfit,omitempty"` // entries whose ID doesn't fit their kind's mapping
	Errors   map[string]string                `json:"errors,omitempty"`
	// Truncated are sources whose result stayed cut off at a limit.
	Truncated map[string]inventory.Truncation `json:"truncated,omitempty"`
	// BadPartitions and BadMappings are partitions and kind mappings added
	// to the pack that failed their check against the provider, with why.
	BadPartitions map[string]string `json:"bad_partitions,omitempty"`
	BadMappings   map[string]string `json:"bad_mappings,omitempty"`
	BadPerParent  map[string]string `json:"bad_per_parent,omitempty"`
}

func logTruncation(ds string, tr inventory.Truncation) {
	switch {
	case !tr.Partitioned:
		Log.Debug("inventory source %s: the result sits at a limit and the source has no partition, so resources are probably missing (give it a \"partition\" in the pack's inventory.json)", ds)
	case tr.Rest:
		Log.Debug("inventory source %s: still cut off after partitioning; resources are probably missing", ds)
	default:
		Log.Debug("inventory source %s: kinds still cut off on their own read: %s", ds, strings.Join(tr.Cut, ", "))
	}
}

func (w Workspace) Generate(all bool) (err error) {
	if err := w.ready(context.Background()); err != nil {
		return err
	}
	end := startProgress(w, "generate")
	defer func() { end(err) }()
	ctx := context.Background()
	schemas, err := w.Schemas(ctx)
	if err != nil {
		return err
	}
	p, err := w.Pack()
	if err != nil {
		return err
	}
	g := generate.Generator{WS: w.Workspace, TF: w.runner(), Schemas: schemas, Log: Log, Skip: p.Skip, Docs: importDocs(p.Imports), Exclude: map[string]bool{}, All: all}
	for t := range p.ExcludeTypes {
		g.Exclude[t] = true
	}
	return g.Run(ctx)
}

// importDocs are the documented import IDs by type.
func importDocs(imports map[string]*mine.ImportExamples) map[string][]string {
	docs := map[string][]string{}
	for typ, ex := range imports {
		if ex != nil && len(ex.IDs) > 0 {
			docs[typ] = ex.IDs
		}
	}
	return docs
}

func (w Workspace) LinkOptions(sources []string, parametrize bool) (link.Options, error) {
	p, err := w.Pack()
	if err != nil {
		return link.Options{}, err
	}
	var all []rules.Rule
	for _, s := range sources {
		switch s {
		case "none":
		case "pack":
			all = append(all, p.Rules...)
		default:
			rs, err := rules.Load(s)
			if err != nil {
				return link.Options{}, fmt.Errorf("load rules %s: %w", s, err)
			}
			all = append(all, rs...)
		}
	}
	rs, err := rules.NewSet(all).WithAliases(p.Aliases)
	if err != nil {
		return link.Options{}, fmt.Errorf("pack aliases.json: %w", err)
	}
	opts := link.Options{Rules: rs, Drop: p.ExcludeAttributes, ManagedBy: p.ManagedBy}
	if parametrize {
		var rep discover.Report
		w.ReadJSON("discovery.json", &rep)
		for _, sp := range w.scopeParams() {
			if _, wrong := rep.BadParams[sp.key]; wrong {
				continue
			}
			ref, data := scopedParam(sp.provider, sp.pack)
			opts.Params = append(opts.Params, link.Param{Value: sp.value, Ref: ref, Data: data})
		}
	}
	return opts, nil
}

func (w Workspace) Link(opts link.Options) (err error) {
	if err := w.ready(context.Background()); err != nil {
		return err
	}
	end := startProgress(w, "link")
	defer func() { end(err) }()
	_, err = link.Run(w.Workspace, opts, Log)
	return err
}

func (w Workspace) Verify() (err error) {
	if err := w.ready(context.Background()); err != nil {
		return err
	}
	end := startProgress(w, "verify")
	defer func() { end(err) }()
	reporter.Step("plan the linked config", 0)
	backout := generate.Backout{WS: w.Workspace}
	undo := func(diags []tf.Diagnostic) ([]string, error) {
		addrs, err := backout.Undo(diags)
		if len(addrs) > 0 {
			Log.Verbose("backed out the reconciled values the plan rejected: %s", strings.Join(addrs, ", "))
		}
		return addrs, err
	}
	drift, n, err := verify.Run(context.Background(), w.Workspace, w.runner(), undo)
	if err != nil {
		// Backing out didn't make the plan pass: it wasn't the cause.
		if rerr := backout.Restore(); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	if err := w.WriteJSON("verify.json", drift); err != nil {
		return err
	}
	var real int
	for _, d := range drift {
		tag := ""
		if d.ConfigOnly {
			tag = " (config-only, harmless)"
		} else {
			real++
		}
		Log.Debug("%s differs: %s%s", d.Address, strings.Join(d.Paths, ", "), tag) // the problems list them
	}
	if real == 0 {
		Log.Info("verified: the config matches the cloud for all %d resources", n)
	}
	return nil
}

// DriftError fails a command whose config would change real resources, so
// scripts can tell; what changes was printed already.
func DriftError(groups []ProblemGroup) error {
	for _, g := range groups {
		if g.Kind == "differs" && len(g.Problems) > 0 {
			return fmt.Errorf("config for %s doesn't match the cloud yet (see above; nothing in the cloud changed)", plural(len(g.Problems), "resource", "resources"))
		}
	}
	return nil
}
