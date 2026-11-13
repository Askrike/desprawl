package generate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// A run after the first regenerates only what it has to: the resources
// left out last time and those whose override changed. Every plan reads
// each resource it imports from the cloud, so a full run costs minutes on
// a large account; fixing one resource shouldn't. The rest keep the config
// and imported values the last run gave them. They stay in the
// configuration (keptFile) and the plans target only what is regenerated,
// so references to them still resolve. A change to the targets or the
// pack's skip filters, or Generator.All, regenerates everything.

// lastRunFile records what the last run was given and what it kept.
const lastRunFile = "last-generate.json"

// keptFile holds, during a partial run, the config and import blocks of the
// resources it doesn't regenerate.
const keptFile = "kept.tf"

type lastRun struct {
	// Inputs is a hash of the targets and the skip filters.
	Inputs string `json:"inputs"`
	// Overrides holds a hash of each override.
	Overrides map[string]string `json:"overrides"`
	// Kept are the targets that ended up in the config, including older
	// versions it added.
	Kept []model.Target `json:"kept"`
}

// partial is the state of a partial run.
type partial struct {
	kept     []model.Target // not regenerated
	keptAddr map[string]bool
	config   *hclwrite.File // their config, from the last run
	plan     []byte         // the last run's plan.json
	dropped  []Dropped      // the last run's, of the kept
	recon    map[string][]Reconciled
}

func hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func overrideHashes(overrides map[string]Override) map[string]string {
	out := map[string]string{}
	for addr, o := range overrides {
		out[addr] = hash(o)
	}
	return out
}

// partialRun decides whether a run can be partial. It returns nil for a
// full run; otherwise the state of the partial run and the targets to
// regenerate (possibly none).
func (g Generator) partialRun(targets []model.Target, overrides map[string]Override, inputs string) (*partial, []model.Target, error) {
	if g.All {
		return nil, nil, nil
	}
	var last lastRun
	if err := g.WS.ReadJSON(lastRunFile, &last); err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if last.Inputs != inputs {
		return nil, nil, nil
	}
	src, err := os.ReadFile(g.WS.StatePath(generatedFile))
	if err != nil {
		return nil, nil, nil //nolint:nilerr // no earlier config: nothing to keep
	}
	planJSON, err := os.ReadFile(g.WS.StatePath("plan.json"))
	if err != nil {
		return nil, nil, nil //nolint:nilerr // no earlier plan: nothing to keep
	}
	var dropped []Dropped
	if err := g.WS.ReadJSON("left-out.json", &dropped); err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	recon := map[string][]Reconciled{}
	if err := g.WS.ReadJSON(reconciledFile, &recon); err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}

	redo := map[string]bool{}
	for _, d := range dropped {
		if !d.Intended {
			redo[d.Address()] = true
		}
	}
	now := overrideHashes(overrides)
	for addr, h := range now {
		if last.Overrides[addr] != h {
			redo[addr] = true
		}
	}
	for addr := range last.Overrides {
		if _, ok := now[addr]; !ok {
			redo[addr] = true
		}
	}
	f, diags := hclwrite.ParseConfig(src, generatedFile, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("parse %s: %s", generatedFile, diags.Error())
	}
	// A kept resource that references one being regenerated is
	// regenerated too: the other may be left out this time.
	for changed := true; changed; {
		changed = false
		for _, b := range f.Body().Blocks() {
			addr := hclx.Address(b)
			if redo[addr] {
				continue
			}
			text := string(b.Body().BuildTokens(nil).Bytes())
			for r := range redo {
				if strings.Contains(text, r+".") {
					redo[addr], changed = true, true
					break
				}
			}
		}
	}

	p := &partial{keptAddr: map[string]bool{}, plan: planJSON, recon: map[string][]Reconciled{}}
	for _, t := range last.Kept {
		if !redo[t.Address()] {
			p.kept = append(p.kept, t)
			p.keptAddr[t.Address()] = true
		}
	}
	for _, b := range f.Body().Blocks() {
		if !p.keptAddr[hclx.Address(b)] {
			f.Body().RemoveBlock(b)
		}
	}
	p.config = f
	for _, d := range dropped {
		if !redo[d.Address()] && !p.keptAddr[d.Address()] {
			p.dropped = append(p.dropped, d)
		}
	}
	for addr, r := range recon {
		if p.keptAddr[addr] {
			p.recon[addr] = r
		}
	}
	var todo []model.Target
	for _, t := range targets {
		if redo[t.Address()] {
			todo = append(todo, t)
		}
	}
	return p, todo, nil
}

// writeKept writes the kept resources' config and imports where the
// plans see them.
func (p *partial) writeKept(ws model.Workspace) error {
	imports, err := discover.ImportsTF(p.kept)
	if err != nil {
		return err
	}
	return os.WriteFile(ws.Path(keptFile), append(p.config.Bytes(), append([]byte("\n"), imports...)...), 0o644)
}

// plan runs terraform plan; in a partial run it targets the resources being
// regenerated, unless args already target some.
// It also returns the IDs the plan read, by address (tf.Runner.PlanIDs).
func (g Generator) plan(ctx context.Context, regen []model.Target, args ...string) ([]tf.Diagnostic, map[string]string, error) {
	if g.partial != nil && !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "-target=") }) {
		for _, t := range regen {
			args = append(args, "-target="+t.Address())
		}
	}
	return g.TF.PlanIDs(ctx, args...)
}

// finishPartial merges the regenerated resources into what the last run
// kept: config, imports, imported values, what was left out and what was
// reconciled.
func (g Generator) finishPartial(kept []model.Target, scrubbed []byte, dropped []Dropped) ([]model.Target, []byte, []Dropped, error) {
	p := g.partial
	_, f, err := g.readGenerated()
	if err != nil {
		return nil, nil, nil, err
	}
	for _, b := range f.Body().Blocks() {
		p.config.Body().AppendBlock(b)
	}
	if err := os.WriteFile(g.WS.Path(generatedFile), p.config.Bytes(), 0o644); err != nil {
		return nil, nil, nil, err
	}
	if err := os.Remove(g.WS.Path(keptFile)); err != nil {
		return nil, nil, nil, err
	}
	all := append(slices.Clone(p.kept), kept...)
	if err := g.writeImports(all); err != nil {
		return nil, nil, nil, err
	}
	regen := map[string]bool{}
	for _, t := range kept {
		regen[t.Address()] = true
	}
	plan, err := mergePlans(p.plan, scrubbed, func(addr string) bool { return regen[addr] })
	if err != nil {
		return nil, nil, nil, err
	}
	var recon map[string][]Reconciled
	if err := g.WS.ReadJSON(reconciledFile, &recon); err != nil && !os.IsNotExist(err) {
		return nil, nil, nil, err
	}
	for addr, r := range p.recon {
		recon[addr] = r
	}
	if err := g.WS.WriteJSON(reconciledFile, recon); err != nil {
		return nil, nil, nil, err
	}
	return all, plan, append(slices.Clone(p.dropped), dropped...), nil
}

// mergePlans returns base with the resource changes of update whose
// address take accepts in place of base's.
func mergePlans(base, update []byte, take func(string) bool) ([]byte, error) {
	var b, u map[string]json.RawMessage
	if err := json.Unmarshal(base, &b); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(update, &u); err != nil {
		return nil, err
	}
	var bc, uc []json.RawMessage
	if err := json.Unmarshal(b["resource_changes"], &bc); err != nil && len(b["resource_changes"]) > 0 {
		return nil, err
	}
	if err := json.Unmarshal(u["resource_changes"], &uc); err != nil && len(u["resource_changes"]) > 0 {
		return nil, err
	}
	addr := func(rc json.RawMessage) string {
		var a struct {
			Address string `json:"address"`
		}
		json.Unmarshal(rc, &a)
		return a.Address
	}
	var out []json.RawMessage
	for _, rc := range bc {
		if !take(addr(rc)) {
			out = append(out, rc)
		}
	}
	for _, rc := range uc {
		if take(addr(rc)) {
			out = append(out, rc)
		}
	}
	rcs, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	b["resource_changes"] = rcs
	return json.Marshal(b)
}
