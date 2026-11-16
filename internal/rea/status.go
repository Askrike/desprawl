package rea

// Where a workspace stands in the loop, for the map page's overview, the
// workspace_status MCP tool and status: each stage's last run, whether
// an input changed since (it's out of date), what it produced, and its
// problems. A stage's last run is recorded in .desprawl/stages.json when it
// ends (startProgress), with a hash of each input it read.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/askrike/desprawl/internal/generate"
	"github.com/askrike/desprawl/internal/link"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/progress"
	"github.com/askrike/desprawl/internal/tf"
	"github.com/askrike/desprawl/internal/verify"
	"github.com/askrike/desprawl/providers"
)

// stages are the loop's stages, in order.
var stages = []string{"init", "discover", "generate", "link", "verify"}

const stagesFile = "stages.json"

// stageRun is a stage's last run.
type stageRun struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Error    string    `json:"error,omitempty"`
	// Inputs hashes each input as the stage read it ("" for a missing one).
	Inputs map[string]string `json:"inputs"`
}

// stageInputs are the files a stage reads: "state/<name>" in the state
// directory, "out/<name>" in the imported Terraform's, "pack/<name>" the
// pack's (as loaded: the packs directory's, else shipped), others relative
// to the workspace.
func stageInputs(stage string) []string {
	switch stage {
	case "discover":
		return []string{"desprawl.hcl", "pack/list_args.json", "pack/inventory.json", "pack/pack.json"}
	case "generate":
		return []string{"state/targets.json", ".desprawl/overrides.json", "state/discovery.json", "pack/pack.json"}
	case "link":
		return []string{"state/generated.tf", "state/plan.json", "state/discovery.json",
			"pack/rules.docs.json", "pack/rules.llm.json", "pack/aliases.json", "pack/pack.json"}
	case "verify":
		return []string{"out/providers.tf", "out/imports.tf", "out/main.tf", "out/data.tf"}
	}
	return nil
}

func (w Workspace) inputHash(name string) string {
	var b []byte
	var err error
	if pk, ok := strings.CutPrefix(name, "pack/"); ok {
		// Every provider's file of that name.
		seen := map[string]bool{}
		for _, p := range w.Config.Providers {
			if !seen[p.Name] {
				seen[p.Name] = true
				fb, _ := providers.ReadFile(w.Packs(), p.Name, pk)
				// The copy packFile makes when printing a fix is the same
				// input until someone edits it.
				if start, err := shippedOrEmpty(p.Name, pk); err == nil && bytes.Equal(fb, start) {
					fb, _ = providers.ReadFile("", p.Name, pk)
				}
				b = append(b, fb...)
			}
		}
	} else if st, ok := strings.CutPrefix(name, "state/"); ok {
		b, err = os.ReadFile(w.StatePath(st))
	} else if out, ok := strings.CutPrefix(name, "out/"); ok {
		b, err = os.ReadFile(w.Path(out))
	} else {
		b, err = os.ReadFile(filepath.Join(w.Dir, name))
	}
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func (w Workspace) stageRuns() map[string]stageRun {
	runs := map[string]stageRun{}
	w.ReadJSON(stagesFile, &runs)
	return runs
}

// recordStage records how a stage's run ended, with its inputs as they are
// now (a stage that rewrites one of them, as verify may main.tf, isn't out
// of date because of it).
func (w Workspace) recordStage(stage string, started time.Time, err error) {
	runs := w.stageRuns()
	r := stageRun{Started: started, Finished: time.Now(), Inputs: map[string]string{}}
	if err != nil {
		r.Error = err.Error()
	}
	for _, in := range stageInputs(stage) {
		r.Inputs[in] = w.inputHash(in)
	}
	runs[stage] = r
	if werr := w.WriteJSON(stagesFile, runs); werr != nil {
		Log.Info("couldn't record the %s run: %v", stage, werr)
	}
}

// stageOutput is the state file a stage always writes.
func stageOutput(stage string) string {
	return map[string]string{"init": "schema.json", "discover": "targets.json", "generate": "plan.json", "link": "links.json", "verify": "verify.json"}[stage]
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// StageView is one stage as the overview shows it.
type StageView struct {
	Name string `json:"name"`
	// State is "not run", "running", "done" or "failed".
	State    string     `json:"state"`
	Progress string     `json:"progress,omitempty"` // while running
	Percent  int        `json:"percent,omitempty"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
	Error    string     `json:"error,omitempty"`
	// Changed lists the inputs that changed since it ran: it's out of date.
	Changed  []string       `json:"changed,omitempty"`
	Result   string         `json:"result,omitempty"` // what it produced, in a sentence
	Problems []ProblemGroup `json:"problems,omitempty"`
}

// WorkspaceView is the workspace as the overview shows it.
type WorkspaceView struct {
	Name string `json:"name"` // its directory's name
	Path string `json:"path"` // absolute
	// Imported is where the imported Terraform goes, by its path in the
	// workspace (as tfmap.Configs names it).
	Imported string      `json:"imported,omitempty"`
	Provider string      `json:"provider"`
	Stages   []StageView `json:"stages"`
	// Pack holds problems with what was added to the provider's pack.
	Pack []ProblemGroup `json:"pack,omitempty"`
	// Next says what to do next.
	Next string `json:"next"`
}

// ViewWorkspace looks at the workspace in dir without writing anything.
func ViewWorkspace(dir string, now time.Time) (WorkspaceView, error) {
	w, err := Open(dir)
	if err != nil {
		return WorkspaceView{}, err
	}
	w.ReadOnly = true
	var sources []string
	for _, p := range w.Config.Providers {
		sources = append(sources, p.Source)
	}
	abs, _ := filepath.Abs(dir)
	v := WorkspaceView{Name: filepath.Base(abs), Path: abs, Provider: strings.Join(slices.Compact(sources), ", ")}
	if len(w.Config.Providers) > 0 {
		imported, _ := filepath.Rel(dir, w.Path(""))
		v.Imported = filepath.ToSlash(imported)
	}
	// Problems need the schema; without one (init didn't finish) there are
	// none to show, and the map page runs no terraform to get it.
	schemas, _ := tf.LoadSchemas(w.StatePath("schema.json"))
	runs := w.stageRuns()
	live, liveErr := progress.Read(paths.Progress(w.Dir))
	for _, name := range stages {
		s := StageView{Name: name, State: "not run"}
		if r, ok := runs[name]; ok {
			s.State = "done"
			if r.Error != "" {
				s.State, s.Error = "failed", r.Error
			}
			s.Started, s.Finished = &r.Started, &r.Finished
			for _, in := range stageInputs(name) {
				if w.inputHash(in) != r.Inputs[in] {
					s.Changed = append(s.Changed, in)
				}
			}
		} else if fileExists(w.StatePath(stageOutput(name))) {
			// It ran before runs were recorded: done, but when and from
			// what isn't known.
			s.State = "done"
		}
		if liveErr == nil && live.Command == name && live.Running(now) {
			s.State, s.Progress, s.Percent = "running", live.String(), live.Percent()
		}
		if s.State == "done" || s.State == "failed" {
			s.Result = w.stageResult(name)
			s.Problems = w.stageProblems(name, schemas)
		}
		v.Stages = append(v.Stages, s)
	}
	if schemas != nil {
		v.Pack = w.LeakProblems(schemas)
	}
	v.Next = next(w, v.Stages)
	return v, nil
}

// Status is the workspace in dir now; one that can't be read says why in
// Next.
func Status(dir string) WorkspaceView {
	v, err := ViewWorkspace(dir, time.Now())
	if err != nil {
		abs, _ := filepath.Abs(dir)
		v = WorkspaceView{Name: filepath.Base(abs), Path: abs, Next: "can't read the workspace: " + err.Error()}
	}
	return v
}

// Running: a running stage's progress changes without its files doing so.
func (v WorkspaceView) Running() bool {
	return slices.ContainsFunc(v.Stages, func(s StageView) bool { return s.State == "running" })
}

// next is what to do next: run the first stage that hasn't run, failed or
// is out of date; else fix what the stages printed.
func next(w Workspace, ss []StageView) string {
	if len(w.Config.Providers) == 0 {
		return "nothing to run: this workspace only draws. To reverse-engineer an account, add a provider block to desprawl.hcl, then run `" + w.cmd("init") + "`"
	}
	for _, s := range ss {
		switch {
		case s.State == "running":
			return s.Name + " is running"
		case s.State == "not run":
			return fmt.Sprintf("run `%s`", w.cmd(s.Name))
		case s.State == "failed":
			return fmt.Sprintf("%s failed: fix it, then run `%s` again", s.Name, w.cmd(s.Name))
		case len(s.Changed) > 0:
			return fmt.Sprintf("%s changed since %s ran: run `%s`", strings.Join(s.Changed, ", "), s.Name, w.cmd(s.Name))
		}
	}
	for _, s := range ss {
		for _, g := range s.Problems {
			if len(g.Problems) > 0 {
				return "fix what the stages report below, or accept it; each says where the fix goes and what to run after"
			}
		}
	}
	return "nothing left to do: the workspace is up to date"
}

// stageResult is what a stage produced, from its outputs.
func (w Workspace) stageResult(stage string) string {
	switch stage {
	case "init":
		s, err := tf.LoadSchemas(w.StatePath("schema.json"))
		if err != nil {
			return ""
		}
		var refs []string
		for _, p := range w.Config.Providers {
			refs = append(refs, p.Ref())
		}
		return fmt.Sprintf("%s: %d resource types, %d listable", strings.Join(refs, ", "), len(s.Resources), len(s.Lists))
	case "discover":
		var targets []model.Target
		if w.ReadJSON("targets.json", &targets) != nil {
			return ""
		}
		types := map[string]bool{}
		for _, t := range targets {
			types[t.Type] = true
		}
		res := fmt.Sprintf("%s of %s to import", plural(len(targets), "resource", "resources"), plural(len(types), "type", "types"))
		if p := w.detectParams(targets); len(p) > 0 {
			var kv []string
			for _, k := range slices.Sorted(maps.Keys(p)) {
				kv = append(kv, k+" "+p[k])
			}
			res += "; scope: " + strings.Join(kv, ", ")
		}
		return res
	case "generate":
		var dropped []generate.Dropped
		w.ReadJSON("left-out.json", &dropped)
		left, intended := 0, 0
		for _, d := range dropped {
			if d.Intended {
				intended++
			} else {
				left++
			}
		}
		pb, err := os.ReadFile(w.StatePath("plan.json"))
		if err != nil {
			return ""
		}
		plan, err := tf.ParsePlan(pb)
		if err != nil {
			return ""
		}
		return fmt.Sprintf("%s in the config, %d left out, %d left out on purpose", plural(len(plan.ResourceChanges), "resource", "resources"), left, intended)
	case "link":
		var edges []link.Edge
		if w.ReadJSON("links.json", &edges) != nil {
			return ""
		}
		return plural(len(edges), "reference", "references") + " made"
	case "verify":
		var drift []verify.Drift
		if w.ReadJSON("verify.json", &drift) != nil {
			return ""
		}
		real, only := 0, 0
		for _, d := range drift {
			if d.ConfigOnly {
				only++
			} else {
				real++
			}
		}
		if real == 0 {
			return fmt.Sprintf("verified: terraform plan imports everything and changes nothing (%d differ only in config, which the plan ignores)", only)
		}
		return fmt.Sprintf("config for %s doesn't match the cloud yet; %d differ only in config", plural(real, "resource", "resources"), only)
	}
	return ""
}

// stageProblems are a stage's problems, as it printed them.
func (w Workspace) stageProblems(stage string, schemas *tf.Schemas) []ProblemGroup {
	var gs []ProblemGroup
	var err error
	switch stage {
	case "discover":
		if schemas != nil {
			gs, err = w.DiscoverProblems(schemas)
		}
	case "generate":
		gs, err = w.GenerateProblems()
	case "link":
		if schemas != nil {
			var opts link.Options
			if opts, err = w.LinkOptions([]string{"pack"}, true); err == nil {
				gs, err = w.LinkProblems(schemas, opts, true)
			}
		}
	case "verify":
		gs, err = w.VerifyProblems()
	}
	if err != nil {
		return []ProblemGroup{{Kind: "unreadable", Summary: "desprawl couldn't read what " + stage + " left: " + err.Error()}}
	}
	return slices.DeleteFunc(gs, func(g ProblemGroup) bool { return len(g.Problems) == 0 })
}

// StatusFingerprint changes whenever what the overview shows could: any
// file in the workspace's root or state, or in its packs directory.
func StatusFingerprint(dir string) uint64 {
	h := fnv.New64a()
	stat := func(dir string) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if info, err := e.Info(); err == nil && !e.IsDir() {
				fmt.Fprintf(h, "%s %d %d\n", filepath.Join(dir, e.Name()), info.Size(), info.ModTime().UnixNano())
			}
		}
	}
	stat(dir)
	stat(paths.Base(dir))
	stat(paths.State(dir))
	packs, _ := os.ReadDir(paths.Pack(dir))
	for _, p := range packs {
		if p.IsDir() {
			stat(filepath.Join(paths.Pack(dir), p.Name()))
		}
	}
	return h.Sum64()
}
