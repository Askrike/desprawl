package link

import (
	"fmt"
	"github.com/askrike/desprawl/internal/logs"
	"os"

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
	opts.Schemas = schemas
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
