package generate

import (
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/askrike/desprawl/internal/tf"
)

// Hint is what generate knows about fixing a resource it left out over a
// plan error: what its config sets, and an override that may fix it when
// the error says which attribute is at fault. Values are never included.
type Hint struct {
	// Set lists the attributes and blocks set in its generated config,
	// "(empty)" after those without a value.
	Set []string `json:"set,omitempty"`
	// Override is the suggested fix, if there is one; Why says what it
	// does and what to check before using it.
	Override *Override `json:"override,omitempty"`
	Why      string    `json:"why,omitempty"`
}

// hint suggests a fix for an error no built-in repair handled. The
// repairs already remove empty attributes the error names and set
// placeholders for missing strings, so what is left is a set attribute
// that has to go (losing its setting) or a missing non-string one.
func hint(body *hclwrite.Body, schema *tf.SchemaBlock, identity map[string]any, d tf.Diagnostic) *Hint {
	h := &Hint{Set: setPaths(body)}
	present, missing := mentioned(body, schema, identity, d)
	switch {
	case len(present) > 0:
		var names []string
		for _, n := range present {
			names = append(names, n.path)
		}
		h.Override = &Override{Remove: []string{present[0].path}}
		if len(names) == 1 {
			h.Why = "the error is about " + names[0] + "; removing it loses that setting"
		} else {
			h.Why = "the error is about " + strings.Join(names, ", ") + "; remove the one the resource can do without (removing loses that setting)"
		}
		if set, at := leftOut(present, schema); len(set) > 0 {
			// Generated config writes every settable attribute, null when
			// unset, except those Terraform leaves out (deprecated ones):
			// the error is likely about one of those, and the cloud has
			// its value.
			paths := slices.Sorted(maps.Keys(set))
			h.Override = &Override{Set: set}
			h.Why = "generated config leaves out " + strings.Join(paths, ", ") + " (Terraform omits deprecated attributes), which the error is likely about; replace each placeholder with the value the cloud holds (read the resource from the provider's API), or, if it has none, remove " + at + " instead (that loses its settings)"
		}
	case len(missing) > 0:
		p := missing[0]
		if v, ok := placeholderValue(schemaAttr(schema, p)); ok && !strings.Contains(p, ".") {
			h.Override = &Override{Set: map[string]string{p: v}, IgnoreChanges: []string{p}}
			h.Why = "the cloud doesn't return " + p + "; set a placeholder (ignore_changes keeps it from ever reaching the cloud), or the real value if you know it"
		}
	}
	return h
}

// leftOut finds, in the deepest nested block the error names, the
// settable attributes generated config has no line for, with a
// placeholder for each, and the block's path.
func leftOut(present []node, schema *tf.SchemaBlock) (map[string]string, string) {
	var deepest *node
	for i, n := range present {
		if n.block != nil && strings.Contains(n.path, ".") && (deepest == nil || strings.Count(n.path, ".") > strings.Count(deepest.path, ".")) {
			deepest = &present[i]
		}
	}
	if deepest == nil {
		return nil, ""
	}
	s := nestedSchema(schema, deepest.path)
	if s == nil {
		return nil, ""
	}
	body := deepest.block.Body()
	set := map[string]string{}
	for name, a := range s.Attributes {
		if body.GetAttribute(name) != nil {
			continue
		}
		if v, ok := placeholderValue(a); ok && !a.Sensitive {
			set[deepest.path+"."+name] = v
		}
	}
	return set, deepest.path
}

// nestedSchema is the schema of the nested block at a path ("a.0.b.1").
func nestedSchema(s *tf.SchemaBlock, path string) *tf.SchemaBlock {
	for _, seg := range strings.Split(path, ".") {
		if isIndex(seg) {
			continue
		}
		if s == nil || s.BlockTypes[seg] == nil {
			return nil
		}
		s = s.BlockTypes[seg].Block
	}
	return s
}

// placeholderValue is an HCL placeholder for a settable attribute.
func placeholderValue(a *tf.SchemaAttr) (string, bool) {
	if a == nil || !(a.Optional || a.Required) {
		return "", false
	}
	switch t := string(a.Type); {
	case t == `"string"`:
		return `"placeholder"`, true
	case t == `"number"`:
		return "0", true
	case t == `"bool"`:
		return "false", true
	case strings.HasPrefix(t, `["list"`), strings.HasPrefix(t, `["set"`):
		return "[]", true
	case strings.HasPrefix(t, `["map"`):
		return "{}", true
	}
	return "", false
}

// setPaths lists the top-level attributes and blocks set in a resource
// body, sorted, blocks by path ("rule.0").
func setPaths(body *hclwrite.Body) []string {
	var out []string
	for name := range body.Attributes() {
		n := node{parent: body, name: name, path: name}
		if isNullNode(n) {
			continue
		}
		if isEmptyNode(n) {
			name += " (empty)"
		}
		out = append(out, name)
	}
	idx := map[string]int{}
	for _, b := range body.Blocks() {
		if b.Type() == "lifecycle" {
			continue
		}
		p := b.Type() + "." + strconv.Itoa(idx[b.Type()])
		idx[b.Type()]++
		if isEmptyNode(node{block: b}) {
			p += " (empty)"
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
