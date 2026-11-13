package tf

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ListResult is one resource found by `terraform query`.
type ListResult struct {
	Type     string         `json:"type"`
	Display  string         `json:"display"`
	Identity map[string]any `json:"identity"`
	// Object is the full resource, only present with include_resource.
	// It may contain sensitive values and must not be persisted.
	Object map[string]any `json:"-"`
	// Block is the list block that found it ("list.T.q0"), when terraform
	// says.
	Block string `json:"-"`
}

// Query runs `terraform query -json` over the .tfquery.hcl files in the
// runner's directory. complete holds the addresses of the list blocks
// terraform reported as finished.
func (r Runner) Query(ctx context.Context) (results []ListResult, diags []Diagnostic, complete map[string]bool, err error) {
	complete = map[string]bool{}
	var last []string // the latest progress messages, to say where a query hung
	stderr, err := r.stream(ctx, []string{"query", "-json"}, func(line []byte) {
		var msg struct {
			Type       string      `json:"type"`
			Message    string      `json:"@message"`
			Diagnostic *Diagnostic `json:"diagnostic"`
			Found      *struct {
				Address      string         `json:"address"`
				ResourceType string         `json:"resource_type"`
				DisplayName  string         `json:"display_name"`
				Identity     map[string]any `json:"identity"`
				Object       map[string]any `json:"resource_object"`
			} `json:"list_resource_found"`
			Complete *struct {
				Address string `json:"address"`
			} `json:"list_complete"`
		}
		if json.Unmarshal(line, &msg) != nil {
			return
		}
		switch {
		case msg.Found != nil:
			results = append(results, ListResult{
				Type:     msg.Found.ResourceType,
				Display:  msg.Found.DisplayName,
				Identity: msg.Found.Identity,
				Object:   msg.Found.Object,
				Block:    msg.Found.Address,
			})
		case msg.Complete != nil && msg.Complete.Address != "":
			complete[msg.Complete.Address] = true
		case msg.Type == "diagnostic" && msg.Diagnostic != nil && msg.Diagnostic.Severity == "error":
			diags = append(diags, *msg.Diagnostic)
		case msg.Message != "":
			last = append(last, msg.Message)
			if len(last) > hungLines {
				last = last[1:]
			}
		}
	})
	if ctx.Err() != nil {
		// Keep what terraform last said: without it a hung list can't be
		// told apart from a slow one.
		return results, diags, complete, fmt.Errorf("terraform query: %w%s", ctx.Err(), hungDetail(len(results), last, stderr))
	}
	if err != nil && len(diags) == 0 {
		return results, nil, complete, fmt.Errorf("terraform query: %w\n%s", err, stderr)
	}
	return results, diags, complete, nil
}

// hungLines bounds how much of a hung query's output its error keeps.
const hungLines = 5

// hungDetail describes a query stopped at its deadline: how many results
// it had found, its last progress messages and the end of its stderr.
func hungDetail(found int, last []string, stderr string) string {
	s := fmt.Sprintf(" (%d found before stopping)", found)
	if len(last) > 0 {
		s += "; last messages: " + strings.Join(last, " | ")
	}
	if lines := strings.Split(strings.TrimSpace(stderr), "\n"); lines[0] != "" {
		s += "; stderr: " + strings.Join(lines[max(len(lines)-hungLines, 0):], " | ")
	}
	return s
}
