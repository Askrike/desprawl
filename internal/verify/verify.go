// Package verify checks that linked config is equivalent to live infrastructure:
// a plan over it must import every resource with no changes.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

type Drift struct {
	Address string   `json:"address"`
	Actions []string `json:"actions"`
	Paths   []string `json:"paths"` // attributes that differ
	// ConfigOnly means every difference is an attribute the cloud doesn't
	// store (e.g. force_destroy): applying would only update state.
	ConfigOnly bool `json:"config_only"`
}

// Run plans the workspace. When the plan fails, undo may back out changes
// to the resources the diagnostics name (returning which it changed), and
// the plan runs again.
func Run(ctx context.Context, ws model.Workspace, r tf.Runner, undo func([]tf.Diagnostic) ([]string, error)) ([]Drift, int, error) {
	// The plan file holds unscrubbed values; don't leave it behind.
	planFile := ws.PlanFile("verify")
	defer os.Remove(planFile)
	for {
		diags, err := r.Plan(ctx, "-out="+planFile)
		if err != nil {
			return nil, 0, err
		}
		if len(diags) == 0 {
			break
		}
		var undone []string
		if undo != nil {
			if undone, err = undo(diags); err != nil {
				return nil, 0, err
			}
		}
		if len(undone) == 0 {
			msg := ""
			for _, d := range diags {
				msg += "\n  - " + d.String()
			}
			return nil, 0, fmt.Errorf("plan failed:%s", msg)
		}
	}
	b, err := r.ShowJSON(ctx, planFile)
	if err != nil {
		return nil, 0, err
	}
	plan, err := tf.ParsePlan(b)
	if err != nil {
		return nil, 0, err
	}
	var drift []Drift
	n := 0
	for _, rc := range plan.ResourceChanges {
		if rc.Mode != "managed" {
			continue
		}
		n++
		if rc.IsNoop() {
			continue
		}
		var before, after any
		_ = json.Unmarshal(rc.Change.Before, &before)
		_ = json.Unmarshal(rc.Change.After, &after)
		var paths []string
		diff("", before, after, &paths)
		sort.Strings(paths)
		configOnly := len(paths) > 0 && rc.Change.Actions[0] == "update"
		bm, _ := before.(map[string]any)
		for _, p := range paths {
			if strings.Contains(p, ".") || bm[p] != nil {
				configOnly = false
			}
		}
		drift = append(drift, Drift{Address: rc.Address, Actions: rc.Change.Actions, Paths: paths, ConfigOnly: configOnly})
	}
	return drift, n, nil
}

func diff(path string, a, b any, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			diff(p, am[k], bm[k], out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, path)
	}
}
