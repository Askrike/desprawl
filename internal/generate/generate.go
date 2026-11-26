// Package generate turns a target list into raw terraform config and
// imported values, using terraform's own config generation.
//
// Nothing is applied: imported values are read from the saved plan, so this
// stage is read-only against the cloud.
package generate

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/askrike/desprawl/internal/logs"
	"github.com/askrike/desprawl/internal/prof"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/tf"
)

const (
	generatedFile = "generated.tf"
	maxFixRounds  = 20
)

// Override is an agent- or user-written fix for one resource's generated
// config, for problems no generic repair can solve (e.g. a function's code
// source, which the cloud API doesn't return).
type Override struct {
	// Set maps attributes to HCL expressions, e.g.
	// {"filename": "\"placeholder.zip\""}; a dotted path
	// ("rule.0.filter") sets one inside a nested block.
	Set map[string]string `json:"set,omitempty"`
	// Remove lists attributes or blocks to delete: a top-level name
	// removes the attribute or every block of that type; a dotted path
	// ("rule.0.filter") removes one nested attribute or block.
	Remove []string `json:"remove,omitempty"`
	// IgnoreChanges adds attributes to lifecycle ignore_changes, for
	// placeholders that must never reach the cloud.
	IgnoreChanges []string `json:"ignore_changes,omitempty"`
	// Note says why, for whoever reads the overrides later.
	Note string `json:"note,omitempty"`
	// Exclude leaves the resource out of the config entirely.
	Exclude bool `json:"exclude,omitempty"`
}

// Dropped is a target left out of the generated config.
type Dropped struct {
	model.Target
	Reason string `json:"reason"`
	// Intended is set when it was left out on purpose: by an override's
	// exclude, or by the pack's skip filters (resources the provider or
	// another resource manages).
	Intended bool `json:"intended,omitempty"`
	// Hint helps fix it by hand, for one left out over a plan error.
	Hint *Hint `json:"hint,omitempty"`
}

type Generator struct {
	WS      model.Workspace
	TF      tf.Runner
	Schemas *tf.Schemas
	Log     logs.Logger
	// Skip leaves out resources managed by the provider or another
	// resource (from the provider pack).
	Skip []Skip
	// Docs are the documented import IDs by type (from the provider
	// pack), for probing types that can't be listed (see probes).
	Docs map[string][]string
	// Exclude are types never to probe (the pack's excluded types).
	Exclude map[string]bool
	// All regenerates every resource, not only those left out last time
	// or whose override changed (incremental.go).
	All bool

	partial *partial // set during a partial run
}

func (g Generator) Run(ctx context.Context) error {
	var targets []model.Target
	if err := g.WS.ReadJSON("targets.json", &targets); err != nil {
		return err
	}
	overrides, err := LoadOverrides(g.WS)
	if err != nil {
		return err
	}
	inputs := hash([]any{targets, g.Skip})
	p, todo, err := g.partialRun(targets, overrides, inputs)
	if err != nil {
		return err
	}
	if p != nil {
		if len(todo) == 0 {
			g.Log.Info("nothing to regenerate: no resource was left out last time and no override changed (-all regenerates everything)")
			return nil
		}
		g.partial = p
		g.Log.Info("regenerating %d resources (left out last time or with a changed override); keeping the other %d", len(todo), len(p.kept))
		targets = todo
	}
	// What verify found was about the config about to be replaced.
	if err := os.Remove(g.WS.StatePath("verify.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	var dropped []Dropped
	var kept []model.Target
	for _, t := range targets {
		if overrides[t.Address()].Exclude {
			dropped = append(dropped, Dropped{Target: t, Reason: "excluded by an override", Intended: true})
			continue
		}
		kept = append(kept, t)
	}
	targets, skippedTargets, err := skipTargets(kept, g.Skip)
	if err != nil {
		return err
	}
	dropped = append(dropped, skippedTargets...)
	if len(targets) == 0 && g.partial == nil {
		return fmt.Errorf("no targets to import")
	}

	if err := g.writeImports(targets); err != nil {
		return err
	}
	// Stale outputs from a previous run would collide with the new generated config.
	for _, name := range []string{generatedFile, "main.tf", "data.tf", keptFile} {
		if err := os.Remove(g.WS.Path(name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if g.partial != nil {
		if err := g.partial.writeKept(g.WS); err != nil {
			return err
		}
		defer os.Remove(g.WS.Path(keptFile))
	}

	// The plan holds unscrubbed values: never leave it behind, even on error.
	defer os.Remove(g.planFile())
	// Every plan reads each target from the cloud, the slow part: each is
	// a progress step counting them.
	plans := 0
	var read map[string]string // the IDs the latest plan read, by address
	planOf := func(n int, why string, args ...string) ([]tf.Diagnostic, error) {
		plans++
		g.TF.Progress.Step(fmt.Sprintf("plan %d (%s)", plans, why), n)
		diags, ids, err := g.plan(ctx, targets, args...)
		read = ids
		return diags, err
	}
	runPlan := func(why string, args ...string) ([]tf.Diagnostic, error) {
		return planOf(len(targets), why, args...)
	}
	g.Log.Verbose("generating config for %d targets", len(targets))
	diags, err := runPlan("generating config", "-generate-config-out="+generatedFile, "-out="+g.planFile())
	if err != nil {
		return err
	}
	older, err := g.referencedVersions()
	if err != nil {
		return err
	}
	n := len(targets)
	targets = g.addNamed(targets, older)
	if len(targets) > n {
		g.Log.Verbose("importing %d older versions or excluded-type resources that generated config references", len(targets)-n)
	}
	m := len(targets)
	targets = g.addNamed(targets, g.probes(targets, read))
	if len(targets) > m {
		g.Log.Verbose("probing %d imported IDs as types that can't be listed but document the same import ID", len(targets)-m)
	}
	if len(targets) > n {
		if err := g.writeImports(targets); err != nil {
			return err
		}
		if err := os.Remove(g.WS.Path(generatedFile)); err != nil {
			return err
		}
		if diags, err = runPlan("generating config again, with the targets added", "-generate-config-out="+generatedFile, "-out="+g.planFile()); err != nil {
			return err
		}
	}
	// Terraform reports an import without config only once everything else
	// validates, which costs a plan at the end of the fix rounds. The other
	// diagnostics stay valid: generated config is unchanged and imports.tf
	// keeps its lines. Without any, a plan is still needed to save one.
	var unconfigured []Dropped
	n = len(targets)
	if targets, unconfigured, diags, err = g.dropUnconfigured(targets, diags, read); err != nil {
		return err
	} else if len(targets) < n {
		dropped = append(dropped, unconfigured...)
		if len(diags) == 0 {
			if diags, err = runPlan("without imports that got no config", "-out="+g.planFile()); err != nil {
				return err
			}
		}
	}
	if len(overrides) > 0 {
		if err := g.applyOverrides(overrides); err != nil {
			return err
		}
		diags, err = runPlan("with overrides", "-out="+g.planFile())
		if err != nil {
			return err
		}
	}
	history := map[string]map[string]bool{} // repairs done per resource, across rounds
	// A round re-plans only the resources it repaired: the others passed
	// the last plan with config that hasn't changed, and every plan reads
	// each resource it imports from the cloud. Once those pass, a full
	// plan saves the plan file and reports any errors terraform holds back
	// until everything else validates.
	for round := 0; len(diags) > 0; round++ {
		if round == maxFixRounds {
			return fmt.Errorf("plan still failing after %d fix rounds:\n%s", maxFixRounds, joinDiags(diags))
		}
		var more []Dropped
		g.TF.Progress.Step(fmt.Sprintf("fix round %d: repairing %d errors", round+1, len(diags)), 0)
		end := prof.Span("fix", "round", strconv.Itoa(round), "errors", strconv.Itoa(len(diags)))
		var repaired []string
		targets, repaired, more, err = g.fix(ctx, targets, diags, history)
		end(append([]string{"dropped", strconv.Itoa(len(more))}, prof.Err(err)...)...)
		if err != nil {
			return err
		}
		dropped = append(dropped, more...)
		if len(targets) == 0 { // a partial run whose every target was left out
			break
		}
		if len(repaired) > 0 {
			var args []string
			for _, a := range repaired {
				args = append(args, "-target="+a)
			}
			if diags, err = planOf(len(repaired), fmt.Sprintf("the %d resources fix round %d repaired", len(repaired), round+1), args...); err != nil {
				return err
			}
			if len(diags) > 0 {
				continue
			}
		}
		if diags, err = runPlan(fmt.Sprintf("everything, after fix round %d", round+1), "-out="+g.planFile()); err != nil {
			return err
		}
	}

	// A partial run that left out everything it regenerated keeps what the
	// last run imported: nothing new to read.
	scrubbed := []byte("{}")
	if len(targets) > 0 {
		if scrubbed, targets, dropped, err = g.readImported(ctx, targets, dropped); err != nil {
			return err
		}
	}
	if g.partial != nil {
		if targets, scrubbed, dropped, err = g.finishPartial(targets, scrubbed, dropped); err != nil {
			return err
		}
	}
	if err := os.WriteFile(g.WS.StatePath("plan.json"), scrubbed, 0o644); err != nil {
		return err
	}
	if err := g.WS.WriteJSON("left-out.json", dropped); err != nil {
		return err
	}
	// Move the raw generated config out of the module so it doesn't
	// conflict with the linked main.tf.
	if err := os.Rename(g.WS.Path(generatedFile), g.WS.StatePath(generatedFile)); err != nil {
		return err
	}
	if err := g.WS.WriteJSON(lastRunFile, lastRun{Inputs: inputs, Overrides: overrideHashes(overrides), Kept: targets}); err != nil {
		return err
	}
	g.Log.Info("wrote config for %d resources, left out %d", len(targets), len(dropped))
	return nil
}

// readImported reads the imported values from the saved plan, settles the
// config against them and returns them scrubbed.
func (g Generator) readImported(ctx context.Context, targets []model.Target, dropped []Dropped) ([]byte, []model.Target, []Dropped, error) {
	g.TF.Progress.Step("reading imported values", 0)
	plan, err := g.TF.ShowJSON(ctx, g.planFile())
	if err != nil {
		return nil, nil, nil, err
	}
	var skippedTargets []Dropped
	if targets, skippedTargets, plan, err = g.skip(targets, plan); err != nil {
		return nil, nil, nil, err
	}
	dropped = append(dropped, skippedTargets...)
	if plan, err = g.restoreIdentifiers(ctx, targets, plan); err != nil {
		return nil, nil, nil, err
	}
	// No plan after this: the pipeline reads only imported values from the
	// plan, which config doesn't change. ignore_changes trips no validator;
	// a reconciled value can, and verify backs those out (Unreconcile).
	defaulted, err := defaultedPaths(plan)
	if err != nil {
		return nil, nil, nil, err
	}
	g.dropSetIndexes(defaulted)
	if err := g.reconcile(plan, defaulted); err != nil {
		return nil, nil, nil, err
	}
	if err := g.ignoreDefaulted(defaulted); err != nil {
		return nil, nil, nil, err
	}

	scrubbed, err := Scrub(plan)
	if err != nil {
		return nil, nil, nil, err
	}
	return scrubbed, targets, dropped, nil
}

// addNamed adds the older versions to targets, named apart from them and,
// in a partial run, from the kept resources; those already kept aren't
// added again.
func (g Generator) addNamed(targets, older []model.Target) []model.Target {
	if g.partial == nil {
		return discover.AddNamed(targets, older)
	}
	all := discover.AddNamed(append(slices.Clone(g.partial.kept), targets...), older)
	return append(slices.Clone(targets), all[len(g.partial.kept)+len(targets):]...)
}

// planFile is where the plans go: in the state directory, deleted after
// reading.
func (g Generator) planFile() string { return g.WS.PlanFile("import") }

// LoadOverrides reads the workspace's overrides.json, if there is one.
func LoadOverrides(ws model.Workspace) (map[string]Override, error) {
	overrides := map[string]Override{}
	b, err := os.ReadFile(paths.Overrides(ws.Dir))
	if os.IsNotExist(err) {
		return overrides, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &overrides); err != nil {
		return nil, fmt.Errorf("%s: %w", paths.Overrides(ws.Dir), err)
	}
	return overrides, nil
}

// writeImports writes import blocks for the targets being generated.
// targets.json is left alone: dropped targets stay there, so they are
// retried once an override fixes them.
func (g Generator) writeImports(targets []model.Target) error {
	b, err := discover.ImportsTF(targets)
	if err != nil {
		return err
	}
	return os.WriteFile(g.WS.Path("imports.tf"), b, 0o644)
}

// dropUnconfigured drops targets the generating plan wrote no resource block
// for: without config they can never import. When nothing was generated at
// all, the plan failed for another reason, which the fix rounds report.
// A dropped target's reason is the plan's diagnostics about it, so a read
// the credential isn't allowed shows as such. Probes that link nothing
// (see unlinked; ids are the IDs the plan read) go too, config and all.
// It returns the plan's diagnostics less those, and blanks the dropped
// targets' import and resource blocks so the others' lines still hold.
func (g Generator) dropUnconfigured(targets []model.Target, diags []tf.Diagnostic, ids map[string]string) ([]model.Target, []Dropped, []tf.Diagnostic, error) {
	gen, f, err := g.readGenerated()
	if err != nil {
		return nil, nil, nil, err
	}
	configured := map[string]bool{}
	for _, b := range f.Body().Blocks() {
		configured[hclx.Address(b)] = true
	}
	if len(configured) == 0 {
		return targets, nil, diags, nil
	}
	blank := unlinked(gen, targets, ids)
	var kept []model.Target
	var dropped []Dropped
	gone := map[string]bool{}
	for _, t := range targets {
		if configured[t.Address()] && !blank[t.Address()] {
			kept = append(kept, t)
			continue
		}
		// The fallback when no diagnostic names the target.
		dropped = append(dropped, Dropped{Target: t, Reason: "no config generated: configuration for import target does not exist"})
		gone[t.Address()] = true
	}
	if len(dropped) == 0 {
		return targets, nil, diags, nil
	}
	// The same file writeImports wrote for the plan.
	src, err := discover.ImportsTF(targets)
	if err != nil {
		return nil, nil, nil, err
	}
	byAddr := map[string]model.Target{}
	for _, t := range targets {
		byAddr[t.Address()] = t
	}
	var rest []tf.Diagnostic
	about := map[string][]string{} // a dropped target's diagnostics: often the provider's read error
	var loose []string             // diagnostics about no target and no config
	for _, d := range diags {
		addr := d.Address
		switch {
		case d.Range != nil && d.Range.Filename == generatedFile:
			// A dropped target has no config, but a blanked probe had.
			if loc, ok := hclx.Find(gen, generatedFile, d.Range.Start.Line); !ok || !blank[hclx.Address(hclx.TopBlock(f, loc))] {
				rest = append(rest, d)
			}
			continue
		case d.Range != nil && d.Range.Filename == "imports.tf":
			addr = hclx.ImportTarget(src, d.Range.Start.Line)
		}
		if _, ok := byAddr[addr]; !ok {
			addr = quotedTarget(d.String(), byAddr)
		}
		switch {
		case addr == "" && d.Range == nil:
			// Likely a dropped import's read error. The next plan,
			// without those, reports it again if it isn't.
			loose = append(loose, diagText(d))
		case !gone[addr]:
			rest = append(rest, d)
		case !slices.Contains(about[addr], diagText(d)):
			about[addr] = append(about[addr], diagText(d))
		}
	}
	var reported []Dropped // most probes find nothing: that's no failure
	for _, d := range dropped {
		if d.Origin != probeOrigin {
			reported = append(reported, d)
		}
	}
	for i, d := range reported {
		a := about[d.Address()]
		if len(a) == 0 && len(dropped) == 1 {
			a = slices.Compact(loose) // they can only be about it
		}
		if len(a) > 0 {
			reported[i].Reason = "no config generated: " + strings.Join(a, "; ")
		}
		g.Log.Debug("left out %s: %s", d.Address(), reported[i].Reason)
	}
	if n := len(dropped) - len(reported); n > 0 {
		g.Log.Verbose("%d probes found nothing new to import", n)
	}
	if len(blank) > 0 {
		if err := os.WriteFile(g.WS.Path(generatedFile), hclx.BlankResources(gen, blank), 0o644); err != nil {
			return nil, nil, nil, err
		}
	}
	return kept, reported, rest, os.WriteFile(g.WS.Path("imports.tf"), hclx.BlankImports(src, gone), 0o644)
}

// diagText is a diagnostic's summary and detail, without the address and
// line that only point into this run's files.
func diagText(d tf.Diagnostic) string {
	if d.Detail == "" {
		return d.Summary
	}
	return d.Summary + ": " + strings.Join(strings.Fields(d.Detail), " ")
}

func (g Generator) readGenerated() ([]byte, *hclwrite.File, error) {
	src, err := os.ReadFile(g.WS.Path(generatedFile))
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	f, diags := hclwrite.ParseConfig(src, generatedFile, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("parse %s: %s", generatedFile, diags.Error())
	}
	return src, f, nil
}

func (g Generator) applyOverrides(overrides map[string]Override) error {
	_, f, err := g.readGenerated()
	if err != nil {
		return err
	}
	for _, b := range f.Body().Blocks() {
		o, ok := overrides[hclx.Address(b)]
		if !ok {
			continue
		}
		for _, name := range o.Remove {
			if strings.Contains(name, ".") {
				n, ok := resolve(b.Body(), name)
				if !ok {
					return fmt.Errorf("override %s: remove %s: no such attribute or block", hclx.Address(b), name)
				}
				if n.block != nil {
					n.parent.RemoveBlock(n.block)
				} else {
					n.parent.RemoveAttribute(n.name)
				}
				continue
			}
			b.Body().RemoveAttribute(name)
			for _, nb := range b.Body().Blocks() {
				if nb.Type() == name {
					b.Body().RemoveBlock(nb)
				}
			}
		}
		for _, name := range slices.Sorted(maps.Keys(o.Set)) {
			expr := o.Set[name]
			toks, err := hclx.ExprTokens(expr)
			if err != nil {
				return fmt.Errorf("override %s.%s: %w", hclx.Address(b), name, err)
			}
			body, attr := b.Body(), name
			if i := strings.LastIndex(name, "."); i >= 0 {
				// A nested attribute: set it in the block its path names.
				n, ok := resolve(b.Body(), name[:i])
				if !ok || n.block == nil {
					return fmt.Errorf("override %s: set %s: no such block", hclx.Address(b), name)
				}
				body, attr = n.block.Body(), name[i+1:]
			}
			body.SetAttributeRaw(attr, toks)
		}
		for _, name := range o.IgnoreChanges {
			hclx.IgnoreChanges(b.Body(), name)
		}
		g.Log.Debug("used the override for %s", hclx.Address(b))
	}
	return os.WriteFile(g.WS.Path(generatedFile), f.Bytes(), 0o644)
}

// ownedValues reports whether a value is a target's own identifier: its
// import ID, or the one identity value no other target has (a subnet's id,
// not the region or account all share). A composite identity (a target
// group attachment's zone, address and port) names no single value.
func ownedValues(targets []model.Target) func(string) bool {
	n := map[string]int{}
	values := make([][]string, len(targets))
	for i, t := range targets {
		seen := map[string]bool{}
		for _, v := range t.Identity {
			if s, ok := v.(string); ok && s != "" && !seen[s] {
				seen[s] = true
				values[i] = append(values[i], s)
				n[s]++
			}
		}
	}
	owned := map[string]bool{}
	for i, t := range targets {
		if t.ID != "" {
			owned[t.ID] = true
		}
		var own []string
		for _, s := range values[i] {
			if n[s] == 1 {
				own = append(own, s)
			}
		}
		if len(own) == 1 {
			owned[own[0]] = true
		}
	}
	return func(s string) bool { return owned[s] }
}

// fix handles one round of plan errors. Errors inside generated config are
// repaired (see repair.go); errors that can't be repaired drop the target,
// as do repairs that repeat (two validators undoing each other's fixes).
// It returns the targets kept and the addresses of those it repaired.
func (g Generator) fix(ctx context.Context, targets []model.Target, diags []tf.Diagnostic, history map[string]map[string]bool) (kept []model.Target, repairedAddrs []string, dropped []Dropped, err error) {
	src, f, err := g.readGenerated()
	if err != nil {
		return nil, nil, nil, err
	}
	byAddr := map[string]model.Target{}
	for _, t := range targets {
		byAddr[t.Address()] = t
	}
	all := targets
	if g.partial != nil {
		all = append(slices.Clone(targets), g.partial.kept...)
	}
	owned := ownedValues(all)
	impSrc, err := os.ReadFile(g.WS.Path("imports.tf"))
	if err != nil {
		return nil, nil, nil, err
	}
	skips, err := skipMatcher(g.Skip)
	if err != nil {
		return nil, nil, nil, err
	}

	drop := map[string]string{}
	// Several repairs per resource per round, as long as no two touch the
	// same top-level attribute or block: both sides of one conflict, or a
	// block whose index the other's path counts, would interact.
	repaired := map[string]map[string]bool{}
	type addrDiag struct {
		addr string
		d    tf.Diagnostic
	}
	var unfixed []addrDiag
	hints := map[string]*Hint{} // for the resources this round may drop
	// record keeps a repair unless it repeats one tried before.
	record := func(addr, what string, d tf.Diagnostic, scope map[string]bool) {
		if history[addr][what] {
			drop[addr] = "repairs repeat without converging: " + what + "; last error: " + d.String()
			g.Log.Debug("left out %s: %s", addr, drop[addr])
			return
		}
		if history[addr] == nil {
			history[addr] = map[string]bool{}
		}
		history[addr][what] = true
		g.Log.Debug("repaired %s: %s (%s)", addr, what, d.Summary)
		if repaired[addr] == nil {
			repaired[addr] = map[string]bool{}
		}
		maps.Copy(repaired[addr], scope)
	}
	for _, d := range diags {
		addr := d.Address
		switch {
		case (d.Range != nil && d.Range.Filename == generatedFile) || (d.Range == nil && d.Address != ""):
			a, body := resourceBody(f, src, d)
			if a != "" {
				addr = a
			}
			if drop[addr] != "" || body == nil {
				break
			}
			t := byAddr[addr]
			// A skip the target's identity couldn't show (one found by
			// inventory knows only its import ID) shows in its config.
			if r := skips(t.Type, configStrings(body)); r != "" {
				drop[addr] = skipReason + r
				g.Log.Debug("left out %s: %s", addr, r)
				continue
			}
			scope := diagScope(body, g.Schemas.Resources[t.Type], t.Identity, d)
			if overlaps(repaired[addr], scope) {
				break
			}
			if what, ok := repair(body, g.Schemas.Resources[t.Type], t.Identity, owned, d); ok {
				record(addr, what, d, scope)
				if drop[addr] != "" {
					// The repair repeats: suggesting it again won't help.
					hints[addr] = &Hint{Set: setPaths(body)}
				}
				continue
			}
			hints[addr] = hint(body, g.Schemas.Resources[t.Type], t.Identity, d)
		case d.Range != nil && d.Range.Filename == "imports.tf":
			if a := hclx.ImportTarget(impSrc, d.Range.Start.Line); a != "" {
				addr = a
			}
		}
		if _, ok := byAddr[addr]; !ok {
			addr = quotedTarget(d.String(), byAddr)
		}
		if _, ok := byAddr[addr]; ok {
			unfixed = append(unfixed, addrDiag{addr, d})
		}
	}
	// Drop only after every diagnostic had its chance: a repair later in
	// the list (removing a whole attribute) may fix an error listed
	// earlier, which then won't come back next round.
	for _, u := range unfixed {
		if drop[u.addr] == "" && repaired[u.addr] == nil {
			g.Log.Debug("left out %s: %s", u.addr, u.d.String())
			drop[u.addr] = u.d.String()
		}
	}
	if len(drop) == 0 && len(repaired) == 0 {
		return nil, nil, nil, fmt.Errorf("plan failed with errors that can't be fixed automatically:\n%s", joinDiags(diags))
	}

	for _, t := range targets {
		if reason := drop[t.Address()]; reason != "" {
			hclx.RemoveResource(f, t.Address())
			dropped = append(dropped, Dropped{Target: t, Reason: reason, Hint: hints[t.Address()]})
			continue
		}
		kept = append(kept, t)
		if repaired[t.Address()] != nil {
			repairedAddrs = append(repairedAddrs, t.Address())
		}
	}
	// In a partial run that is a result: the kept resources stand.
	if len(kept) == 0 && g.partial == nil {
		return nil, nil, nil, fmt.Errorf("every target failed to import:\n%s", joinDiags(diags))
	}
	if err := os.WriteFile(g.WS.Path(generatedFile), f.Bytes(), 0o644); err != nil {
		return nil, nil, nil, err
	}
	return kept, repairedAddrs, dropped, g.writeImports(kept)
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

func isNull(a *hclwrite.Attribute) bool {
	return a == nil || strings.TrimSpace(string(a.Expr().BuildTokens(nil).Bytes())) == "null"
}

func nullValue() cty.Value { return cty.NullVal(cty.DynamicPseudoType) }

func joinDiags(diags []tf.Diagnostic) string {
	var b strings.Builder
	for _, d := range diags {
		b.WriteString("  - " + d.String() + "\n")
	}
	return b.String()
}

// quotedTarget finds the target a diagnostic without an address or range
// names in its text, e.g. an import of an object deleted since discovery.
func quotedTarget(text string, byAddr map[string]model.Target) string {
	for _, m := range quotedAddr.FindAllStringSubmatch(text, -1) {
		if _, ok := byAddr[m[1]]; ok {
			return m[1]
		}
	}
	return ""
}

var quotedAddr = regexp.MustCompile(`"([A-Za-z0-9_]+\.[A-Za-z0-9_-]+)"`)
