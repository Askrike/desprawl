package link

import (
	"fmt"
	"strings"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// node is a simplified expression tree. Generated config is almost entirely
// literals, lists, objects and jsonencode() calls; anything else is kept
// verbatim as raw source.
type node interface{}

type rawNode struct{ src string }

// strNode is a string template made of literal text and references.
type strNode struct{ parts []part }

type part struct {
	lit string
	ref string // traversal such as aws_iam_role.foo.arn; set means not literal
}

type listNode struct{ items []node }

type objNode struct{ items []kv }

type kv struct {
	keySrc string // key as written, e.g. Action or "sts:ExternalId"
	key    string // key name, for rule paths
	val    node
}

type callNode struct {
	name string
	args []node
}

// literal returns the string value of a strNode made only of literal text.
func (s strNode) literal() (string, bool) {
	var b strings.Builder
	for _, p := range s.parts {
		if p.ref != "" {
			return "", false
		}
		b.WriteString(p.lit)
	}
	return b.String(), true
}

func toNode(e hclsyntax.Expression, src []byte) node {
	raw := func() node { return rawNode{string(e.Range().SliceBytes(src))} }
	switch e := e.(type) {
	case *hclsyntax.TemplateExpr:
		var b strings.Builder
		for _, p := range e.Parts {
			lit, ok := p.(*hclsyntax.LiteralValueExpr)
			if !ok || lit.Val.Type() != cty.String || lit.Val.IsNull() {
				return raw()
			}
			b.WriteString(lit.Val.AsString())
		}
		return strNode{parts: []part{{lit: b.String()}}}
	case *hclsyntax.TupleConsExpr:
		var n listNode
		for _, it := range e.Exprs {
			n.items = append(n.items, toNode(it, src))
		}
		return n
	case *hclsyntax.ObjectConsExpr:
		var n objNode
		for _, it := range e.Items {
			k := kv{
				keySrc: string(it.KeyExpr.Range().SliceBytes(src)),
				val:    toNode(it.ValueExpr, src),
			}
			if name, ok := hclx.KeyName(it.KeyExpr); ok {
				k.key = name
			}
			n.items = append(n.items, k)
		}
		return n
	case *hclsyntax.FunctionCallExpr:
		if e.ExpandFinal {
			return raw()
		}
		n := callNode{name: e.Name}
		for _, a := range e.Args {
			n.args = append(n.args, toNode(a, src))
		}
		return n
	}
	return raw()
}

// emit renders a node as HCL source. Formatting is left to hclwrite.Format.
func emit(n node) string {
	switch n := n.(type) {
	case rawNode:
		return n.src
	case strNode:
		var parts []part
		for _, p := range n.parts {
			if p.ref != "" || p.lit != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) == 1 && parts[0].ref != "" {
			return parts[0].ref
		}
		return quote(parts)
	case listNode:
		items := make([]string, len(n.items))
		for i, it := range n.items {
			items[i] = emit(it)
		}
		return "[" + strings.Join(items, ", ") + "]"
	case objNode:
		if len(n.items) == 0 {
			return "{}"
		}
		var b strings.Builder
		b.WriteString("{\n")
		for _, it := range n.items {
			fmt.Fprintf(&b, "%s = %s\n", it.keySrc, emit(it.val))
		}
		b.WriteString("}")
		return b.String()
	case callNode:
		args := make([]string, len(n.args))
		for i, a := range n.args {
			args[i] = emit(a)
		}
		return n.name + "(" + strings.Join(args, ", ") + ")"
	}
	panic(fmt.Sprintf("unknown node %T", n))
}

func quote(parts []part) string {
	var b strings.Builder
	b.WriteByte('"')
	for i, p := range parts {
		if p.ref != "" {
			b.WriteString("${" + p.ref + "}")
			continue
		}
		// "$${" would escape the reference that follows into literal text.
		if lit, ok := strings.CutSuffix(p.lit, "$"); ok && i+1 < len(parts) && parts[i+1].ref != "" {
			b.WriteString(escape(lit) + `${"$"}`)
			continue
		}
		b.WriteString(escape(p.lit))
	}
	b.WriteByte('"')
	return b.String()
}

var escaper = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	"\n", `\n`,
	"\r", `\r`,
	"\t", `\t`,
	"${", "$${",
	"%{", "%%{",
)

func escape(s string) string { return escaper.Replace(s) }
