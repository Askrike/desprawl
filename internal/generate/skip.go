package generate

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/model"
)

// Skip leaves out resources of a type whose attribute matches: objects the
// provider creates and manages itself (automated backups, keys it owns),
// or that another resource already manages (an alarm owned by a scaling
// policy). Pack data, written by hand: terraform can't tell who manages a
// resource, only whether it imports.
type Skip struct {
	Type   string `json:"type"`
	Attr   string `json:"attr"`
	Match  string `json:"match"` // regular expression on the attribute's string value
	Reason string `json:"reason"`
}

const skipReason = "skipped by the provider pack: "

// skipMatcher returns the reason a resource of a type with the given
// attribute values matches a Skip, or "".
func skipMatcher(skips []Skip) (func(typ string, values map[string]any) string, error) {
	res := make([]*regexp.Regexp, len(skips))
	for i, s := range skips {
		re, err := regexp.Compile(s.Match)
		if err != nil {
			return nil, fmt.Errorf("skip %s.%s: %w", s.Type, s.Attr, err)
		}
		res[i] = re
	}
	return func(typ string, values map[string]any) string {
		for i, s := range skips {
			if v, ok := values[s.Attr].(string); ok && typ == s.Type && res[i].MatchString(v) {
				return s.Reason
			}
		}
		return ""
	}, nil
}

// skipTargets leaves out targets whose identity already shows that a Skip
// matches, so their config is never generated and plan errors in it never
// need fixing.
func skipTargets(targets []model.Target, skips []Skip) ([]model.Target, []Dropped, error) {
	match, err := skipMatcher(skips)
	if err != nil {
		return nil, nil, err
	}
	var kept []model.Target
	var dropped []Dropped
	for _, t := range targets {
		if r := match(t.Type, t.Identity); r != "" {
			dropped = append(dropped, Dropped{Target: t, Reason: skipReason + r, Intended: true})
			continue
		}
		kept = append(kept, t)
	}
	return kept, dropped, nil
}

// configStrings returns a resource body's top-level string literals, the
// values its Skips are matched against.
func configStrings(body *hclwrite.Body) map[string]any {
	out := map[string]any{}
	for name, a := range body.Attributes() {
		if v, ok := literalString(a); ok {
			out[name] = v
		}
	}
	return out
}

// skipped returns the reason each resource in plan matches a Skip.
func skipped(planJSON []byte, skips []Skip) (map[string]string, error) {
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Type    string `json:"type"`
			Change  struct {
				Before map[string]any `json:"before"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return nil, err
	}
	match, err := skipMatcher(skips)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, rc := range plan.ResourceChanges {
		if r := match(rc.Type, rc.Change.Before); r != "" {
			out[rc.Address] = r
		}
	}
	return out, nil
}

// skip removes the resources the pack's Skips match from the generated
// config, imports and plan. It doesn't plan again: generated config holds
// only literals, so removing an import changes no other resource's plan
// (and verify plans the result anyway).
func (g Generator) skip(targets []model.Target, plan []byte) ([]model.Target, []Dropped, []byte, error) {
	why, err := skipped(plan, g.Skip)
	if err != nil || len(why) == 0 {
		return targets, nil, plan, err
	}
	_, f, err := g.readGenerated()
	if err != nil {
		return nil, nil, nil, err
	}
	var kept []model.Target
	var dropped []Dropped
	for _, t := range targets {
		if r, ok := why[t.Address()]; ok {
			hclx.RemoveResource(f, t.Address())
			dropped = append(dropped, Dropped{Target: t, Reason: skipReason + r, Intended: true})
			continue
		}
		kept = append(kept, t)
	}
	if err := os.WriteFile(g.WS.Path(generatedFile), f.Bytes(), 0o644); err != nil {
		return nil, nil, nil, err
	}
	if err := g.writeImports(kept); err != nil {
		return nil, nil, nil, err
	}
	g.Log.Verbose("skipped %d resources the provider pack marks as managed elsewhere", len(dropped))
	plan, err = withoutChanges(plan, why)
	return kept, dropped, plan, err
}

// withoutChanges removes the given addresses' resource changes from a
// plan; the pipeline reads nothing else from it (see Scrub).
func withoutChanges(planJSON []byte, addrs map[string]string) ([]byte, error) {
	var plan map[string]json.RawMessage
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return nil, err
	}
	var rcs []json.RawMessage
	if err := json.Unmarshal(plan["resource_changes"], &rcs); err != nil {
		return nil, err
	}
	kept := []json.RawMessage{}
	for _, rc := range rcs {
		var a struct {
			Address string `json:"address"`
		}
		if err := json.Unmarshal(rc, &a); err != nil {
			return nil, err
		}
		if _, ok := addrs[a.Address]; !ok {
			kept = append(kept, rc)
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	plan["resource_changes"] = b
	return json.Marshal(plan)
}
