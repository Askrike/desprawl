// Package hclx has helpers for patching HCL files by source position.
//
// hclwrite preserves formatting but has no source ranges, while hclsyntax
// has ranges but can't be written back. We locate things with hclsyntax and
// then navigate the hclwrite tree by the same block indices.
package hclx

import (
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// Location identifies a spot in a file: a chain of block indices (each the
// index within its parent body's blocks), plus an optional attribute name in
// the innermost block.
type Location struct {
	Blocks []int
	Attr   string
}

// Find returns the deepest block/attribute containing the given line.
func Find(src []byte, filename string, line int) (Location, bool) {
	f, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return Location{}, false
	}
	var loc Location
	body := f.Body.(*hclsyntax.Body)
	found := false
	for {
		descended := false
		for name, a := range body.Attributes {
			if contains(a.SrcRange, line) {
				loc.Attr = name
				return loc, true
			}
		}
		for i, b := range body.Blocks {
			if contains(b.Range(), line) {
				loc.Blocks = append(loc.Blocks, i)
				body = b.Body
				descended, found = true, true
				break
			}
		}
		if !descended {
			return loc, found
		}
	}
}

func contains(r hcl.Range, line int) bool {
	return r.Start.Line <= line && line <= r.End.Line
}

// TopBlock returns the top-level hclwrite block at a location.
func TopBlock(f *hclwrite.File, loc Location) *hclwrite.Block {
	if len(loc.Blocks) == 0 {
		return nil
	}
	return f.Body().Blocks()[loc.Blocks[0]]
}

// Address returns "type.name" for a resource block.
func Address(b *hclwrite.Block) string {
	if b.Type() != "resource" || len(b.Labels()) != 2 {
		return ""
	}
	return b.Labels()[0] + "." + b.Labels()[1]
}

// RemoveResource removes the resource block with the given address.
func RemoveResource(f *hclwrite.File, addr string) bool {
	for _, b := range f.Body().Blocks() {
		if Address(b) == addr {
			f.Body().RemoveBlock(b)
			return true
		}
	}
	return false
}

// ImportTarget returns the address in the `to` of the import block that
// contains the given line.
func ImportTarget(src []byte, line int) string {
	f, diags := hclsyntax.ParseConfig(src, "imports.tf", hcl.InitialPos)
	if diags.HasErrors() {
		return ""
	}
	for _, b := range f.Body.(*hclsyntax.Body).Blocks {
		if b.Type == "import" && contains(b.Range(), line) {
			return importTo(b)
		}
	}
	return ""
}

// BlankImports blanks out the import blocks whose `to` is in addrs, keeping
// every other line where it was, so diagnostics' lines still point at
// their blocks.
func BlankImports(src []byte, addrs map[string]bool) []byte {
	return blank(src, "imports.tf", func(b *hclsyntax.Block) bool { return b.Type == "import" && addrs[importTo(b)] })
}

// BlankResources blanks out the resource blocks whose address is in addrs,
// like BlankImports.
func BlankResources(src []byte, addrs map[string]bool) []byte {
	return blank(src, "", func(b *hclsyntax.Block) bool {
		return b.Type == "resource" && len(b.Labels) == 2 && addrs[b.Labels[0]+"."+b.Labels[1]]
	})
}

func blank(src []byte, filename string, match func(*hclsyntax.Block) bool) []byte {
	f, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return src
	}
	lines := strings.SplitAfter(string(src), "\n")
	for _, b := range f.Body.(*hclsyntax.Body).Blocks {
		if !match(b) {
			continue
		}
		for l := b.Range().Start.Line; l <= b.Range().End.Line && l <= len(lines); l++ {
			lines[l-1] = "\n"
		}
	}
	return []byte(strings.Join(lines, ""))
}

func importTo(b *hclsyntax.Block) string {
	if to, ok := b.Body.Attributes["to"]; ok {
		if t, diags := hcl.AbsTraversalForExpr(to.Expr); !diags.HasErrors() && len(t) == 2 {
			return t.RootName() + "." + t[1].(hcl.TraverseAttr).Name
		}
	}
	return ""
}

// SensitivePlaceholder stands in for a sensitive value that is not
// exported; the attribute is in ignore_changes, so it is never applied.
const SensitivePlaceholder = "REDACTED: sensitive value not exported"

// IgnoreChanges adds attr to the resource's lifecycle ignore_changes.
func IgnoreChanges(body *hclwrite.Body, attr string) {
	var lc *hclwrite.Body
	for _, b := range body.Blocks() {
		if b.Type() == "lifecycle" {
			lc = b.Body()
		}
	}
	if lc == nil {
		lc = body.AppendNewBlock("lifecycle", nil).Body()
	}
	var elems []hclwrite.Tokens
	if a := lc.GetAttribute("ignore_changes"); a != nil {
		inner := strings.Trim(strings.TrimSpace(string(a.Expr().BuildTokens(nil).Bytes())), "[]")
		for _, x := range strings.Split(inner, ",") {
			if x = strings.TrimSpace(x); x != "" {
				elems = append(elems, hclwrite.TokensForTraversal(hcl.Traversal{hcl.TraverseRoot{Name: x}}))
			}
		}
	}
	elems = append(elems, hclwrite.TokensForTraversal(hcl.Traversal{hcl.TraverseRoot{Name: attr}}))
	lc.SetAttributeRaw("ignore_changes", hclwrite.TokensForTuple(elems))
}

// ExprTokens parses an HCL expression into tokens for hclwrite.
func ExprTokens(expr string) (hclwrite.Tokens, error) {
	f, diags := hclwrite.ParseConfig([]byte("x = "+expr+"\n"), "", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	return f.Body().GetAttribute("x").Expr().BuildTokens(nil), nil
}

// KeyName returns an object key that is a bare name or a literal string.
func KeyName(e hclsyntax.Expression) (string, bool) {
	if k, ok := e.(*hclsyntax.ObjectConsKeyExpr); ok {
		if t, diags := hcl.AbsTraversalForExpr(k.Wrapped); !diags.HasErrors() && len(t) == 1 {
			return t.RootName(), true
		}
		e = k.Wrapped
	}
	if t, ok := e.(*hclsyntax.TemplateExpr); ok && t.IsStringLiteral() {
		if v, diags := t.Value(nil); !diags.HasErrors() {
			return v.AsString(), true
		}
	}
	return "", false
}
