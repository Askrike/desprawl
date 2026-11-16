package link

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/askrike/desprawl/internal/logs"

	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// Run links a workspace's generated config into main.tf and data.tf.
func Run(ws model.Workspace, opts Options, log logs.Logger) (*Result, error) {
	src, err := os.ReadFile(ws.StatePath("generated.tf"))
	if err != nil {
		return nil, fmt.Errorf("%w (run generate first)", err)
	}
	pb, err := os.ReadFile(ws.StatePath("plan.json"))
	if err != nil {
		return nil, err
	}
	plan, err := tf.ParsePlan(pb)
	if err != nil {
		return nil, err
	}
	schemas, err := tf.LoadSchemas(ws.StatePath("schema.json"))
	if err != nil {
		return nil, err
	}
	ix, err := index.Build(plan, schemas, opts.Rules)
	if err != nil {
		return nil, err
	}
	dups, err := Duplicates(plan, schemas, ix)
	if err != nil {
		return nil, err
	}
	opts.Duplicates = append(opts.Duplicates, dups...)
	managed, err := ManagedDuplicates(plan, ix, opts.ManagedBy)
	if err != nil {
		return nil, err
	}
	opts.Duplicates = append(opts.Duplicates, managed...)
	dups = append(dups, managed...)
	opts.Schemas = schemas
	opts.changed, opts.imported = importPlan(plan)
	for _, d := range dups {
		log.Debug("left out %s.%s: %s manages the same value", d.Addr, d.Attr, d.By)
	}
	res, err := Link(src, ix, opts)
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(ws.Path("main.tf"), res.Config, 0o644); err != nil {
		return nil, err
	}
	if len(res.Data) > 0 {
		err = os.WriteFile(ws.Path("data.tf"), res.Data, 0o644)
	} else {
		err = os.Remove(ws.Path("data.tf"))
		if os.IsNotExist(err) {
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	if err := ws.WriteJSON("links.json", res.Edges); err != nil {
		return nil, err
	}

	if len(res.Ignored) > 0 {
		log.Debug("ignore_changes added (config leaves them out, the plan would change them): %s", strings.Join(res.Ignored, ", "))
	}

	counts := map[Kind]int{}
	conf := map[Confidence]int{}
	for _, e := range res.Edges {
		counts[e.Kind]++
		conf[e.Confidence]++
	}
	log.Info("linked %d references: %d observed, %d derived, %d inferred (`desprawl why` explains each)",
		len(res.Edges), conf[Observed], conf[Derived], conf[Inferred])
	log.Debug("by kind: %d unique, %d by rule, %d embedded", counts[KindUnique], counts[KindRule], counts[KindEmbedded])
	for _, b := range res.CycleBreaks {
		log.Debug("left %s -> %s literal to avoid a reference cycle", b[0], b[1])
	}
	return res, nil
}

// importPlan is, per resource, the top-level attributes the import plan
// would change (planned value differs from the imported one) and those it
// imported a value for (not null, not empty).
func importPlan(plan *tf.PlanJSON) (changed, imported map[string]map[string]bool) {
	changed, imported = map[string]map[string]bool{}, map[string]map[string]bool{}
	add := func(m map[string]map[string]bool, addr, name string) {
		if m[addr] == nil {
			m[addr] = map[string]bool{}
		}
		m[addr][name] = true
	}
	for _, rc := range plan.ResourceChanges {
		if rc.Mode != "managed" {
			continue
		}
		var before, after map[string]any
		if json.Unmarshal(rc.Change.Before, &before) != nil {
			continue
		}
		for name, b := range before {
			if !empty(b) {
				add(imported, rc.Address, name)
			}
		}
		if rc.IsNoop() || json.Unmarshal(rc.Change.After, &after) != nil {
			continue
		}
		for name, b := range before {
			if !reflect.DeepEqual(b, after[name]) {
				add(changed, rc.Address, name)
			}
		}
	}
	return changed, imported
}

func empty(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}
