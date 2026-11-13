// Package mine extracts reference rules from a terraform provider's
// documentation. Every provider documents resources as markdown with HCL
// examples, and those examples show which attributes reference which other
// resources: `role = aws_iam_role.example.name` is a rule.
package mine

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/rules"
)

var fence = regexp.MustCompile("^```\\s*(terraform|hcl|tf)\\s*$")

// Docs mines every markdown file under dir.
func Docs(dir string) ([]rules.Rule, error) {
	counts := map[rules.Rule]int{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isMarkdown(d.Name()) {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, block := range codeBlocks(b) {
			for _, r := range fromHCL(block) {
				counts[r]++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]rules.Rule, 0, len(counts))
	for r, n := range counts {
		r.Support = n
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Target+"."+a.Attr != b.Target+"."+b.Attr {
			return a.Target+"."+a.Attr < b.Target+"."+b.Attr
		}
		return a.Match < b.Match
	})
	return out, nil
}

func isMarkdown(name string) bool {
	for _, ext := range []string{".md", ".markdown", ".mdx"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

func codeBlocks(md []byte) [][]byte {
	var blocks [][]byte
	var cur *bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(md))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case cur == nil && fence.MatchString(strings.TrimSpace(line)):
			cur = &bytes.Buffer{}
		case cur != nil && strings.TrimSpace(line) == "```":
			blocks = append(blocks, cur.Bytes())
			cur = nil
		case cur != nil:
			cur.WriteString(line + "\n")
		}
	}
	return blocks
}

// Roots that are never resource types.
var nonResourceRoots = map[string]bool{
	"var": true, "local": true, "module": true, "each": true, "count": true,
	"path": true, "self": true, "terraform": true,
}

func fromHCL(src []byte) []rules.Rule {
	f, diags := hclsyntax.ParseConfig(src, "example.tf", hcl.InitialPos)
	if diags.HasErrors() || f == nil {
		return nil
	}
	var out []rules.Rule
	for _, b := range f.Body.(*hclsyntax.Body).Blocks {
		if b.Type != "resource" || len(b.Labels) != 2 {
			continue
		}
		m := miner{src: b.Labels[0], out: &out}
		m.body(b.Body, nil)
	}
	return out
}

type miner struct {
	src string
	out *[]rules.Rule
}

func (m miner) body(b *hclsyntax.Body, path []string) {
	for name, a := range b.Attributes {
		m.expr(a.Expr, join(path, name))
	}
	for _, blk := range b.Blocks {
		if blk.Type == "lifecycle" || blk.Type == "provisioner" || blk.Type == "connection" {
			continue
		}
		m.body(blk.Body, join(path, blk.Type))
	}
}

func join(path []string, seg string) []string {
	out := make([]string, len(path), len(path)+1)
	copy(out, path)
	return append(out, seg)
}

func (m miner) expr(e hclsyntax.Expression, path []string) {
	switch e := e.(type) {
	case *hclsyntax.ScopeTraversalExpr:
		m.ref(e.Traversal, path, rules.MatchExact)
	case *hclsyntax.RelativeTraversalExpr, *hclsyntax.IndexExpr, *hclsyntax.SplatExpr:
		// aws_x.y[0].arn, aws_x.y[*].arn
		for _, t := range e.Variables() {
			m.ref(t, path, rules.MatchExact)
		}
	case *hclsyntax.TemplateWrapExpr:
		m.expr(e.Wrapped, path)
	case *hclsyntax.TemplateExpr:
		for _, p := range e.Parts {
			if t, ok := p.(*hclsyntax.ScopeTraversalExpr); ok {
				m.ref(t.Traversal, path, rules.MatchEmbedded)
			}
		}
	case *hclsyntax.TupleConsExpr:
		for _, it := range e.Exprs {
			m.expr(it, path)
		}
	case *hclsyntax.ObjectConsExpr:
		for _, it := range e.Items {
			if k, ok := hclx.KeyName(it.KeyExpr); ok {
				m.expr(it.ValueExpr, join(path, k))
			}
		}
	case *hclsyntax.FunctionCallExpr:
		// Only encoders pass values through unchanged; other functions
		// transform them, so a traversal inside isn't the attribute's value.
		if e.Name == "jsonencode" || e.Name == "yamlencode" || e.Name == "tolist" || e.Name == "toset" {
			for _, a := range e.Args {
				m.expr(a, path)
			}
		}
	}
}

// ref records a rule for traversals shaped like TYPE.NAME[...].ATTR or
// data.TYPE.NAME.ATTR. References to data sources are recorded against the
// same-named resource type, since data sources mirror resources.
func (m miner) ref(t hcl.Traversal, path []string, match rules.Match) {
	var names []string
	// After TYPE.NAME, the attribute path; indexes inside it are kept
	// (identity[0].principal_id -> identity.0.principal_id), the instance
	// index right after NAME (aws_x.y[0].arn) isn't.
	var attr []string
	for _, step := range t {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			names = append(names, s.Name)
		case hcl.TraverseAttr:
			if len(names) < 2 || (len(names) == 2 && names[0] == "data") {
				names = append(names, s.Name)
			} else {
				attr = append(attr, s.Name)
			}
		case hcl.TraverseIndex:
			if len(attr) > 0 && s.Key.Type() == cty.Number {
				attr = append(attr, s.Key.AsBigFloat().Text('f', 0))
			}
		}
	}
	if len(names) > 0 && names[0] == "data" {
		names = names[1:]
	}
	if len(names) < 2 || len(attr) == 0 || nonResourceRoots[names[0]] || !strings.Contains(names[0], "_") {
		return
	}
	// Trailing indexes select from a list; the value is the list element's.
	for len(attr) > 0 && isNumber(attr[len(attr)-1]) {
		attr = attr[:len(attr)-1]
	}
	if len(attr) == 0 {
		return
	}
	a := strings.Join(attr, ".")
	// `runtime = aws_lambda_function.other.runtime` copies a setting from
	// a sibling; it doesn't identify it.
	if names[0] == m.src && strings.Join(path, ".") == a {
		return
	}
	*m.out = append(*m.out, rules.Rule{
		Source: m.src,
		Path:   strings.Join(path, "."),
		Target: names[0],
		Attr:   a,
		Match:  match,
		Origin: rules.OriginDocs,
	})
}

func isNumber(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}
