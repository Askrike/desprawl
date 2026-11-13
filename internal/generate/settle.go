package generate

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/tf"
)

// defaultedPaths returns, per resource, the attribute paths that would
// change on import from an empty live value (0, "", false, []) to
// something else: the cloud reports "unset" where the provider fills in
// its default. The resource runs on that default already, so the
// difference is ignored rather than applied.
func defaultedPaths(planJSON []byte) (map[string][]string, error) {
	plan, err := tf.ParsePlan(planJSON)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, rc := range plan.ResourceChanges {
		if rc.Mode != "managed" || rc.IsNoop() {
			continue
		}
		var before, after any
		if json.Unmarshal(rc.Change.Before, &before) != nil || json.Unmarshal(rc.Change.After, &after) != nil {
			continue
		}
		var paths []string
		walkDefaulted(before, after, "", &paths)
		if len(paths) > 0 {
			sort.Strings(paths)
			out[rc.Address] = paths
		}
	}
	return out, nil
}

func walkDefaulted(before, after any, path string, out *[]string) {
	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok {
			return
		}
		for k, bv := range b {
			if ident.MatchString(k) {
				walkDefaulted(bv, a[k], join(path, k), out)
			}
		}
	case []any:
		a, ok := after.([]any)
		if !ok || len(a) != len(b) || len(b) == 0 {
			if isZero(before) && after != nil && !isZero(after) && path != "" {
				*out = append(*out, path)
			}
			return
		}
		for i := range b {
			walkDefaulted(b[i], a[i], path+"["+strconv.Itoa(i)+"]", out)
		}
	default:
		if path != "" && isZero(before) && after != nil && !isZero(after) {
			*out = append(*out, path)
		}
	}
}

// ident matches keys usable in an ignore_changes path; map keys like
// "kubernetes.io/name" aren't.
var ident = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

func isZero(v any) bool {
	switch x := v.(type) {
	case string:
		return x == ""
	case float64:
		return x == 0
	case bool:
		return !x
	case []any:
		return len(x) == 0
	}
	return false
}

// dropSetIndexes drops the defaulted paths that index into a set:
// ignore_changes can't name a set element (terraform rejects the config),
// and a set's indexes in the plan follow no element anyway.
func (g Generator) dropSetIndexes(defaulted map[string][]string) {
	for addr, ps := range defaulted {
		typ, _, _ := strings.Cut(addr, ".")
		var kept []string
		for _, p := range ps {
			if indexesSet(g.Schemas.Resources[typ], p) {
				g.Log.Debug("can't ignore %s.%s (empty live value): it indexes a set", addr, p)
				continue
			}
			kept = append(kept, p)
		}
		defaulted[addr] = kept
	}
}

// pathStep matches one step of a defaulted path: a name or an index.
var pathStep = regexp.MustCompile(`[^.\[\]]+|\[\d+\]`)

// indexesSet reports whether a path (a.b[0].c) indexes a set in a resource
// schema: a set-nested block, or an attribute of set type.
func indexesSet(s *tf.SchemaBlock, path string) bool {
	steps := pathStep.FindAllString(path, -1)
	var ty any // the cty type of the attribute walked into, once in one
	for i := 0; i < len(steps); i++ {
		step, index := steps[i], strings.HasPrefix(steps[i], "[")
		if ty == nil {
			if s == nil || index {
				return false
			}
			if a := s.Attributes[step]; a != nil {
				if json.Unmarshal(a.Type, &ty) != nil {
					return false
				}
				continue
			}
			bt := s.BlockTypes[step]
			if bt == nil {
				return false
			}
			if bt.NestingMode == "list" || bt.NestingMode == "set" {
				if i+1 < len(steps) && strings.HasPrefix(steps[i+1], "[") {
					if bt.NestingMode == "set" {
						return true
					}
					i++
				}
			}
			s = bt.Block
			continue
		}
		// A cty type: "string", ["list", T], ["set", T], ["map", T], ["object", {...}].
		t, ok := ty.([]any)
		if !ok || len(t) != 2 {
			return false
		}
		switch t[0] {
		case "set":
			return index
		case "list", "map":
			ty = t[1]
		case "object":
			fields, _ := t[1].(map[string]any)
			if ty = fields[step]; ty == nil || index {
				return false
			}
		default:
			return false
		}
	}
	return false
}

// ignoreDefaulted adds ignore_changes for defaulted paths.
func (g Generator) ignoreDefaulted(paths map[string][]string) error {
	if len(paths) == 0 {
		return nil
	}
	_, f, err := g.readGenerated()
	if err != nil {
		return err
	}
	for _, b := range f.Body().Blocks() {
		for _, p := range paths[hclx.Address(b)] {
			hclx.IgnoreChanges(b.Body(), p)
			g.Log.Debug("ignore %s.%s (empty live value, provider default)", hclx.Address(b), p)
		}
	}
	return os.WriteFile(g.WS.Path(generatedFile), f.Bytes(), 0o644)
}
