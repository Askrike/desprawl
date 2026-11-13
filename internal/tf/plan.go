package tf

import "encoding/json"

// PlanJSON is the subset of `terraform show -json` output we use.
type PlanJSON struct {
	ResourceChanges []ResourceChange `json:"resource_changes"`
}

type ResourceChange struct {
	Address string `json:"address"`
	Mode    string `json:"mode"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Change  struct {
		Actions   []string        `json:"actions"`
		Before    json.RawMessage `json:"before"`
		After     json.RawMessage `json:"after"`
		Importing *struct {
			ID string `json:"id"`
		} `json:"importing"`
	} `json:"change"`
}

// Values returns the resource's current (imported) attribute values.
func (rc ResourceChange) Values() (map[string]any, error) {
	raw := rc.Change.Before
	if len(raw) == 0 || string(raw) == "null" {
		raw = rc.Change.After
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func (rc ResourceChange) IsNoop() bool {
	return len(rc.Change.Actions) == 1 && rc.Change.Actions[0] == "no-op"
}

func ParsePlan(b []byte) (*PlanJSON, error) {
	var p PlanJSON
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
