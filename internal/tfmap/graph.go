// Package tfmap draws a Terraform configuration as an architecture map. It
// parses .tf files directly (no terraform CLI, no credentials), so every
// edge knows which attribute created it; collapses the reference graph into
// components and connections with the provider catalogs (abstract.go);
// lays them out on a grid (layout.go) and routes the edges (route.go), so
// the browser, the ASCII view and the layout metrics all score the same
// picture.
package tfmap

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

type Node struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // resource, data, variable, local, output, module, provider
	Type   string `json:"type,omitempty"`
	Name   string `json:"name"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Source string `json:"source"`
	Module string `json:"module,omitempty"` // e.g. module.backend for blocks inside an expanded local module

	Role       string `json:"role"`
	Category   string `json:"category,omitempty"`
	Classified bool   `json:"classified"` // false = role guessed by heuristics
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Attr string `json:"attr"` // attribute path in From that holds the reference
	Ref  string `json:"ref"`  // the full reference expression, e.g. aws_iam_role.x.arn
	// Whole is set when the attribute is exactly this reference (name =
	// var.table), not a reference inside a larger expression ("${var.x}-api").
	Whole bool `json:"whole,omitempty"`
}

type Graph struct {
	Dir    string   `json:"dir"`
	Nodes  []Node   `json:"nodes"`
	Edges  []Edge   `json:"edges"`
	Arch   Arch     `json:"arch"`
	Layout *Layout  `json:"layout,omitempty"` // saved grid positions for the architecture view
	Seeded bool     `json:"seeded,omitempty"` // Layout is the automatic one, not saved
	Diags  []string `json:"diagnostics,omitempty"`
}

// pending holds a block's body until all nodes are known, so references can
// be resolved regardless of file or declaration order.
type pending struct {
	id     string
	prefix string // module address the expressions are evaluated in, e.g. "module.backend."
	body   hcl.Body
	attr   *hclsyntax.Attribute // set for locals and module arguments, where each attribute is its own edge source
	each   hclsyntax.Expression // the block's for_each, which each.key and each.value stand for
}

type parser struct {
	g      *Graph
	root   string
	nodes  map[string]bool
	todo   []pending
	remote map[string]string // installed registry/git modules: key ("a.b") -> directory
}

// Parse reads the configuration in dir, expanding local modules and those
// terraform init installed.
func Parse(dir string) (*Graph, error) {
	p := &parser{g: &Graph{Dir: dir, Nodes: []Node{}, Edges: []Edge{}}, root: dir, nodes: map[string]bool{}, remote: installedModules(dir)}
	if err := p.module(dir, "", 0); err != nil {
		return nil, err
	}

	// Locals and module outputs whose value is an object literal: a
	// reference to one key (local.queues["a"], module.m.arns.a) reaches only
	// that entry, not every value in the object.
	type object struct {
		prefix string
		expr   *hclsyntax.ObjectConsExpr
	}
	objects := map[string]object{}
	for _, t := range p.todo {
		var e hclsyntax.Expression
		switch own := strings.TrimPrefix(t.id, t.prefix); {
		case t.attr != nil && strings.HasPrefix(own, "local."):
			e = t.attr.Expr
		case t.body != nil && strings.HasPrefix(own, "output."):
			if a := t.body.(*hclsyntax.Body).Attributes["value"]; a != nil {
				e = a.Expr
			}
		}
		if o, ok := e.(*hclsyntax.ObjectConsExpr); ok {
			objects[t.id] = object{t.prefix, o}
		}
	}

	seen := map[string]bool{}
	depth := 0
	var emit func(prefix, from, attr string, expr, each hclsyntax.Expression)
	emit = func(prefix, from, attr string, expr, each hclsyntax.Expression) {
		if !p.nodes[from] || depth > 8 {
			return
		}
		vars := expr.Variables()
		// each.value.arn reaches whatever for_each iterates over.
		if each != nil && slices.ContainsFunc(vars, func(t hcl.Traversal) bool { return t.RootName() == "each" }) {
			vars = append(vars, each.Variables()...)
		}
		whole := false
		if len(vars) == 1 {
			switch x := expr.(type) {
			case *hclsyntax.ScopeTraversalExpr:
				whole = true
			case *hclsyntax.TemplateWrapExpr:
				_, whole = x.Wrapped.(*hclsyntax.ScopeTraversalExpr)
			}
		}
		for _, t := range vars {
			to, ref, output := resolve(t)
			if to == "" {
				continue
			}
			to = prefix + to
			// module.X.out points at the output inside an expanded module.
			if out := to + ".output." + output; output != "" && p.nodes[out] {
				to = out
			}
			if to == from || !p.nodes[to] {
				continue
			}
			if o, ok := objects[to]; ok {
				if v := entry(o.expr, t, map[string]int{"local": 2, "module": 3}[t.RootName()]); v != nil {
					depth++
					emit(o.prefix, from, attr, v, nil)
					depth--
					continue
				}
			}
			key := from + "|" + to + "|" + attr
			if seen[key] {
				continue
			}
			seen[key] = true
			p.g.Edges = append(p.g.Edges, Edge{From: from, To: to, Attr: attr, Ref: ref, Whole: whole})
		}
	}
	for _, t := range p.todo {
		if t.attr != nil {
			emit(t.prefix, t.id, t.attr.Name, t.attr.Expr, t.each)
			continue
		}
		walkBody(t.body.(*hclsyntax.Body), "", func(attr string, expr hclsyntax.Expression) {
			emit(t.prefix, t.id, attr, expr, t.each)
		})
	}
	return p.g, nil
}

// Module block arguments that configure the call rather than set an input.
var moduleMetaArgs = map[string]bool{
	"source": true, "version": true, "count": true, "for_each": true, "depends_on": true, "providers": true,
}

// module parses one module directory. Local child modules (source "./x"),
// and registry/git ones that terraform init installed, are expanded in place
// under the address prefix "module.<name>.", and their call arguments become
// edges from the child's variables, so references flow through module
// boundaries. Other modules stay single opaque nodes.
func (p *parser) module(dir, prefix string, depth int) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.tf"))
	if err != nil {
		return err
	}
	sort.Strings(files)

	add := func(n Node, src []byte, rng hcl.Range) {
		n.ID = prefix + n.ID
		n.Module = strings.TrimSuffix(prefix, ".")
		n.Line = rng.Start.Line
		n.Source = string(rng.SliceBytes(src))
		p.nodes[n.ID] = true
		p.g.Nodes = append(p.g.Nodes, n)
	}

	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, diags := hclsyntax.ParseConfig(src, path, hcl.InitialPos)
		for _, d := range diags {
			p.g.Diags = append(p.g.Diags, d.Error())
		}
		if f == nil {
			continue
		}
		rel, _ := filepath.Rel(p.root, path)
		rel = filepath.ToSlash(rel)
		for _, b := range f.Body.(*hclsyntax.Body).Blocks {
			n := Node{Kind: b.Type, File: rel}
			switch {
			case b.Type == "resource" && len(b.Labels) == 2:
				n.Type, n.Name, n.ID = b.Labels[0], b.Labels[1], b.Labels[0]+"."+b.Labels[1]
			case b.Type == "data" && len(b.Labels) == 2:
				n.Type, n.Name, n.ID = b.Labels[0], b.Labels[1], "data."+b.Labels[0]+"."+b.Labels[1]
			case b.Type == "variable" && len(b.Labels) == 1:
				n.Name, n.ID = b.Labels[0], "var."+b.Labels[0]
			case b.Type == "output" && len(b.Labels) == 1:
				n.Name, n.ID = b.Labels[0], "output."+b.Labels[0]
			case b.Type == "module" && len(b.Labels) == 1:
				child := prefix + "module." + b.Labels[0] + "."
				src := localSource(b, dir)
				if src == "" {
					src = p.remote[moduleKey(child)]
				}
				if src != "" && depth < 10 {
					if err := p.module(src, child, depth+1); err != nil {
						return err
					}
					for _, name := range slices.Sorted(maps.Keys(b.Body.Attributes)) {
						a := b.Body.Attributes[name]
						if !moduleMetaArgs[name] {
							p.todo = append(p.todo, pending{id: child + "var." + name, prefix: prefix, attr: a, each: forEach(b.Body)})
						}
					}
					continue
				}
				n.Name, n.ID = b.Labels[0], "module."+b.Labels[0]
			case b.Type == "provider" && len(b.Labels) == 1:
				n.Name, n.ID = b.Labels[0], "provider."+b.Labels[0]
			case b.Type == "locals":
				for _, name := range slices.Sorted(maps.Keys(b.Body.Attributes)) {
					a := b.Body.Attributes[name]
					add(Node{Kind: "local", Name: name, ID: "local." + name, File: rel}, src, a.SrcRange)
					p.todo = append(p.todo, pending{id: prefix + "local." + name, prefix: prefix, attr: a})
				}
				continue
			default:
				continue // terraform {}, moved {}, import {}, etc.
			}
			add(n, src, b.Range())
			p.todo = append(p.todo, pending{id: prefix + n.ID, prefix: prefix, body: b.Body, each: forEach(b.Body)})
		}
	}
	return nil
}

// entry is the value in o under the key that traversal t names at step i
// (["a"] or .a), or nil.
func entry(o *hclsyntax.ObjectConsExpr, t hcl.Traversal, i int) hclsyntax.Expression {
	if i == 0 || i >= len(t) {
		return nil
	}
	var key string
	switch s := t[i].(type) {
	case hcl.TraverseAttr:
		key = s.Name
	case hcl.TraverseIndex:
		if s.Key.Type() != cty.String || !s.Key.IsKnown() || s.Key.IsNull() {
			return nil
		}
		key = s.Key.AsString()
	default:
		return nil
	}
	for _, item := range o.Items {
		if k, ok := hclx.KeyName(item.KeyExpr); ok && k == key {
			return item.ValueExpr
		}
	}
	return nil
}

// forEach is a block's for_each expression, or nil.
func forEach(b *hclsyntax.Body) hclsyntax.Expression {
	if a, ok := b.Attributes["for_each"]; ok {
		return a.Expr
	}
	return nil
}

// installedModules reads where `terraform init` put the config's registry
// and git modules (.terraform/modules/modules.json), so they can be expanded
// like local ones. Without it they stay opaque.
func installedModules(root string) map[string]string {
	b, err := os.ReadFile(filepath.Join(root, ".terraform", "modules", "modules.json"))
	if err != nil {
		return nil
	}
	var m struct {
		Modules []struct{ Key, Dir string }
	}
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range m.Modules {
		if d := filepath.Join(root, filepath.FromSlash(e.Dir)); e.Key != "" && isDir(d) {
			out[e.Key] = d
		}
	}
	return out
}

// moduleKey is a module's key in modules.json: "a.b" for "module.a.module.b.".
func moduleKey(prefix string) string {
	parts := strings.Split(strings.TrimSuffix(prefix, "."), ".")
	var names []string
	for i := 1; i < len(parts); i += 2 {
		names = append(names, parts[i])
	}
	return strings.Join(names, ".")
}

// localSource returns the directory of a module block whose source is a
// local path, or "" for registry, git and other remote sources.
func localSource(b *hclsyntax.Block, dir string) string {
	a, ok := b.Body.Attributes["source"]
	if !ok {
		return ""
	}
	v, diags := a.Expr.Value(nil)
	if diags.HasErrors() || !v.Type().Equals(cty.String) || v.IsNull() {
		return ""
	}
	src := v.AsString()
	if !strings.HasPrefix(src, "./") && !strings.HasPrefix(src, "../") {
		return ""
	}
	path := filepath.Join(dir, src)
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return ""
	}
	return path
}

// walkBody visits every attribute, including those in nested blocks, with a
// dotted path like "rule.object_ownership".
func walkBody(b *hclsyntax.Body, prefix string, fn func(string, hclsyntax.Expression)) {
	names := make([]string, 0, len(b.Attributes))
	for name := range b.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fn(prefix+name, b.Attributes[name].Expr)
	}
	for _, blk := range b.Blocks {
		walkBody(blk.Body, prefix+blk.Type+".", fn)
	}
}

// resolve maps a traversal like aws_iam_role.lambda_exec.arn to the node
// address it refers to, plus a readable form of the whole reference. For
// module.X.out it also returns the output name.
func resolve(t hcl.Traversal) (string, string, string) {
	var parts []string
	for _, step := range t {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			parts = append(parts, s.Name)
		case hcl.TraverseAttr:
			parts = append(parts, s.Name)
		case hcl.TraverseIndex:
			parts = append(parts, "[…]")
		}
	}
	ref := strings.ReplaceAll(strings.Join(parts, "."), ".[…]", "[…]")
	if len(parts) < 2 {
		return "", ref, ""
	}
	switch parts[0] {
	case "var", "local":
		return parts[0] + "." + parts[1], ref, ""
	case "module":
		// module.q.out, or module.q[0].out with count/for_each.
		rest := parts[2:]
		if len(rest) > 0 && rest[0] == "[…]" {
			rest = rest[1:]
		}
		if len(rest) > 0 && rest[0] != "[…]" {
			return "module." + parts[1], ref, rest[0]
		}
		return "module." + parts[1], ref, ""
	case "data":
		if len(parts) < 3 {
			return "", ref, ""
		}
		return "data." + parts[1] + "." + parts[2], ref, ""
	case "each", "count", "self", "path", "terraform":
		return "", ref, ""
	default:
		return parts[0] + "." + parts[1], ref, ""
	}
}

// Loader loads a configuration as the map shows it: parsed, abstracted with
// the catalogs, and with the layout saved at layout ("" for none), routed.
type Loader func(dir, layout string) (*Graph, error)

// NewLoader loads with the catalogs, read on every load so edits to a
// -catalog directory show up.
func NewLoader(catalogs ...fs.FS) Loader {
	return func(dir, layout string) (*Graph, error) {
		g, err := Parse(dir)
		if err != nil {
			return nil, err
		}
		c, err := LoadCatalog(catalogs...)
		if err != nil {
			return nil, err
		}
		g.Arch = Abstract(g, c)
		if layout == "" {
			return g, nil
		}
		l, err := LoadLayout(layout)
		if l != nil {
			g.SetLayout(l)
		}
		return g, err
	}
}

// SetLayout places g's components on l (nil for none) and routes its edges.
func (g *Graph) SetLayout(l *Layout) {
	g.Layout = l
	if l == nil {
		return
	}
	for i, r := range routeEdges(g.Arch, l) {
		g.Arch.Edges[i].Route = r
	}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
