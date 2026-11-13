package generate

import (
	"encoding/json"
)

// Scrub removes sensitive values from `terraform show -json` plan output,
// using the before_sensitive/after_sensitive markers terraform provides, so
// that secrets read during import are never written to disk.
func Scrub(planJSON []byte) ([]byte, error) {
	var plan map[string]any
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return nil, err
	}
	rcs, _ := plan["resource_changes"].([]any)
	for _, x := range rcs {
		rc, _ := x.(map[string]any)
		change, _ := rc["change"].(map[string]any)
		if change == nil {
			continue
		}
		change["before"] = scrub(change["before"], change["before_sensitive"])
		change["after"] = scrub(change["after"], change["after_sensitive"])
	}
	// Other sections (prior_state, planned_values, configuration) repeat the
	// values without markers; the pipeline only needs resource_changes.
	out := map[string]any{"format_version": plan["format_version"], "resource_changes": rcs}
	return json.MarshalIndent(out, "", " ")
}

func scrub(v, marker any) any {
	switch m := marker.(type) {
	case bool:
		if m {
			return nil
		}
	case map[string]any:
		if obj, ok := v.(map[string]any); ok {
			for k, sub := range m {
				if _, has := obj[k]; has {
					obj[k] = scrub(obj[k], sub)
				}
			}
		}
	case []any:
		if list, ok := v.([]any); ok {
			for i := range list {
				if i < len(m) {
					list[i] = scrub(list[i], m[i])
				}
			}
		}
	}
	return v
}
