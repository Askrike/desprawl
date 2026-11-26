package generate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// reconciledFile records what reconcile changed, so verify can back it out.
const reconciledFile = "reconciled.json"

// linksFile is where link records its references (link.Run).
const linksFile = "links.json"

// Reconciled is an attribute reconcile set to its imported value. Was is
// the expression it replaced ("" if there was none); Ignore holds the
// defaulted paths under it, which ignore_changes covers once it's backed out.
type Reconciled struct {
	Attr   string   `json:"attr"`
	Was    string   `json:"was,omitempty"`
	Ignore []string `json:"ignore,omitempty"`
}

// reconcile sets top-level attributes that would change on import back to
// their imported values, where the schema allows setting them. This covers
// generated config that doesn't round-trip (a null that the provider
// defaults to something other than the live value). Defaulted paths under
// a reconciled attribute are taken out of defaulted.
//
// It also restores attributes generated config has no line for at all,
// where the import holds a value: Terraform writes every settable
// attribute, null when unset, except deprecated ones, which still carry
// live settings (and references) that would otherwise be lost silently,
// since an optional+computed attribute left out plans clean. A value
// another type's resource holds under the same name isn't restored: that
// resource manages it (a setting split out of the deprecated attribute
// into its own resource).
//
// There is no plan to check it: restoring an imported value can trip a
// validator (two attributes the API returns but config may not both set),
// which is rare, so verify's plan is the check and Unreconcile the way back.
func (g Generator) reconcile(planJSON []byte, defaulted map[string][]string) error {
	plan, err := tf.ParsePlan(planJSON)
	if err != nil {
		return err
	}
	_, f, err := g.readGenerated()
	if err != nil {
		return err
	}
	blocks := map[string]*hclwrite.Block{}
	for _, b := range f.Body().Blocks() {
		blocks[hclx.Address(b)] = b
	}
	held := heldByType(plan)
	done := map[string][]Reconciled{}
	n := 0
	for _, rc := range plan.ResourceChanges {
		if rc.Mode != "managed" || blocks[rc.Address] == nil {
			continue
		}
		var before, after map[string]json.RawMessage
		if json.Unmarshal(rc.Change.Before, &before) != nil || json.Unmarshal(rc.Change.After, &after) != nil {
			continue
		}
		schema := g.Schemas.Resources[rc.Type]
		if schema == nil {
			continue
		}
		body := blocks[rc.Address].Body()
		for _, name := range slices.Sorted(maps.Keys(before)) {
			b := before[name]
			a := schema.Attributes[name]
			if a == nil || !(a.Optional || a.Required) || a.Sensitive || string(b) == "null" {
				continue
			}
			changed := !rc.IsNoop() && !jsonEqual(b, after[name])
			if !changed && !(body.GetAttribute(name) == nil && restorable(name, b, rc.Type, held)) {
				continue
			}
			ty, err := ctyjson.UnmarshalType(a.Type)
			if err != nil {
				continue
			}
			v, err := ctyjson.Unmarshal(b, ty)
			if err != nil {
				continue
			}
			r := Reconciled{Attr: name}
			if old := body.GetAttribute(name); old != nil {
				r.Was = strings.TrimSpace(string(old.Expr().BuildTokens(nil).Bytes()))
			}
			var rest []string
			for _, p := range defaulted[rc.Address] {
				if p == name || strings.HasPrefix(p, name+".") || strings.HasPrefix(p, name+"[") {
					r.Ignore = append(r.Ignore, p)
				} else {
					rest = append(rest, p)
				}
			}
			if len(r.Ignore) > 0 {
				defaulted[rc.Address] = rest
			}
			body.SetAttributeValue(name, v)
			done[rc.Address] = append(done[rc.Address], r)
			n++
		}
	}
	if n > 0 {
		if err := os.WriteFile(g.WS.Path(generatedFile), f.Bytes(), 0o644); err != nil {
			return err
		}
		g.Log.Verbose("reconciled %d attributes with imported values", n)
	}
	return g.WS.WriteJSON(reconciledFile, done)
}

// restorable says whether an imported value generated config left out
// goes back in: not the resource's own id (Terraform's identifier, never
// written), not an empty value, and not one another type's resource holds
// under the same name (see reconcile).
func restorable(name string, value json.RawMessage, typ string, held map[string]map[string]map[string]bool) bool {
	var v any
	if name == "id" || json.Unmarshal(value, &v) != nil || v == nil || isZero(v) {
		return false
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return false
	}
	for t := range held[name][canonical(value)] {
		if t != typ {
			return false
		}
	}
	return true
}

// heldByType indexes the imported top-level values by attribute name and
// value: the resource types holding each.
func heldByType(plan *tf.PlanJSON) map[string]map[string]map[string]bool {
	held := map[string]map[string]map[string]bool{}
	for _, rc := range plan.ResourceChanges {
		var before map[string]json.RawMessage
		if rc.Mode != "managed" || json.Unmarshal(rc.Change.Before, &before) != nil {
			continue
		}
		for name, v := range before {
			if string(v) == "null" {
				continue
			}
			k := canonical(v)
			if held[name] == nil {
				held[name] = map[string]map[string]bool{}
			}
			if held[name][k] == nil {
				held[name][k] = map[string]bool{}
			}
			held[name][k][rc.Type] = true
		}
	}
	return held
}

// canonical is a JSON value re-encoded, so equal values compare equal.
func canonical(v json.RawMessage) string {
	var x any
	if json.Unmarshal(v, &x) != nil {
		return string(v)
	}
	b, _ := json.Marshal(x)
	return string(b)
}

// Unreconcile backs out the reconciled attributes of the resources a
// failing plan's diagnostics name, in the workspace's config and in the
// generated config link reads, and returns their addresses.
func Unreconcile(ws model.Workspace, diags []tf.Diagnostic) ([]string, error) {
	var rec map[string][]Reconciled
	if err := ws.ReadJSON(reconciledFile, &rec); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	addrs := map[string]bool{}
	for _, d := range diags {
		a := d.Address
		if a == "" && d.Range != nil {
			a = blockAt(ws.Path(d.Range.Filename), d.Range.Start.Line)
		}
		if rec[a] != nil {
			addrs[a] = true
		}
	}
	if len(addrs) == 0 {
		return nil, nil
	}
	files, err := filepath.Glob(ws.Path("*.tf"))
	if err != nil {
		return nil, err
	}
	for _, file := range append(files, ws.StatePath(generatedFile)) {
		src, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		f, d := hclwrite.ParseConfig(src, file, hcl.InitialPos)
		if d.HasErrors() {
			return nil, fmt.Errorf("parse %s: %s", file, d.Error())
		}
		changed := false
		for _, b := range f.Body().Blocks() {
			if !addrs[hclx.Address(b)] {
				continue
			}
			for _, r := range rec[hclx.Address(b)] {
				if t, err := hclx.ExprTokens(r.Was); r.Was != "" && err == nil {
					b.Body().SetAttributeRaw(r.Attr, t)
				} else {
					b.Body().RemoveAttribute(r.Attr)
				}
				for _, p := range r.Ignore {
					hclx.IgnoreChanges(b.Body(), p)
				}
				changed = true
			}
		}
		if changed {
			if err := os.WriteFile(file, f.Bytes(), 0o644); err != nil {
				return nil, err
			}
		}
	}
	backedOut := map[string][]string{}
	for a := range addrs {
		for _, r := range rec[a] {
			backedOut[a] = append(backedOut[a], r.Attr)
		}
		delete(rec, a)
	}
	if err := dropEdges(ws, addrs, backedOut); err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(addrs)), ws.WriteJSON(reconciledFile, rec)
}

// dropEdges removes from links.json the references link made in attributes
// that were backed out.
func dropEdges(ws model.Workspace, addrs map[string]bool, attrs map[string][]string) error {
	var edges []json.RawMessage
	if err := ws.ReadJSON(linksFile, &edges); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	kept := edges[:0]
	for _, raw := range edges {
		var e struct{ From, Path string }
		if json.Unmarshal(raw, &e) == nil && addrs[e.From] && slices.ContainsFunc(attrs[e.From], func(a string) bool {
			return e.Path == a || strings.HasPrefix(e.Path, a+".") || strings.HasPrefix(e.Path, a+"[")
		}) {
			continue
		}
		kept = append(kept, raw)
	}
	if len(kept) == len(edges) {
		return nil
	}
	return ws.WriteJSON(linksFile, kept)
}

// Backout is verify's way back from reconciled values its plan rejects.
// The back-out holds only if the plan then passes: Restore puts back every
// file Undo changed, so a plan that fails for another reason leaves the
// workspace as it was.
type Backout struct {
	WS    model.Workspace
	saved map[string][]byte // nil: absent
}

// Undo is Unreconcile, saving the files it may change the first time.
func (b *Backout) Undo(diags []tf.Diagnostic) ([]string, error) {
	if b.saved == nil {
		files, err := filepath.Glob(b.WS.Path("*.tf"))
		if err != nil {
			return nil, err
		}
		b.saved = map[string][]byte{}
		for _, f := range append(files, b.WS.StatePath(generatedFile), b.WS.StatePath(reconciledFile), b.WS.StatePath(linksFile)) {
			src, err := os.ReadFile(f)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			b.saved[f] = src
		}
	}
	return Unreconcile(b.WS, diags)
}

// Restore puts back the files as they were before the first Undo.
func (b *Backout) Restore() error {
	for f, src := range b.saved {
		if src == nil {
			continue
		}
		if err := os.WriteFile(f, src, 0o644); err != nil {
			return err
		}
	}
	b.saved = nil
	return nil
}

// blockAt returns the address of the resource block at a line of a file.
func blockAt(file string, line int) string {
	src, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	loc, ok := hclx.Find(src, file, line)
	if !ok {
		return ""
	}
	f, d := hclwrite.ParseConfig(src, file, hcl.InitialPos)
	if d.HasErrors() {
		return ""
	}
	if b := hclx.TopBlock(f, loc); b != nil {
		return hclx.Address(b)
	}
	return ""
}
