package suggest

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/inventory"
	"github.com/askrike/desprawl/internal/tf"
)

// Checks of pack entries against the provider's schema, for entries added
// by hand or by an agent: a stage leaves out the ones that fail and prints
// why, instead of failing on them.

// CheckListArg checks a list-arg spec: every argument it fills is a list
// argument, it fills at least one, every required argument is covered,
// scope arguments name a scope parameter, and parent attributes exist.
func CheckListArg(schemas *tf.Schemas, s discover.ListArg, scopes []string) error {
	lb := schemas.Lists[s.Type]
	if lb == nil {
		return fmt.Errorf("%s has no list resource", s.Type)
	}
	filled := slices.Concat(slices.Collect(maps.Keys(s.Args)), slices.Collect(maps.Keys(s.Scope)), slices.Collect(maps.Keys(s.Values)))
	if len(filled) == 0 {
		return fmt.Errorf("no argument filled")
	}
	for _, arg := range filled {
		if lb.Attributes[arg] == nil && lb.BlockTypes[arg] == nil {
			return fmt.Errorf("no list argument %q", arg)
		}
	}
	for _, arg := range schemas.RequiredListArgs(s.Type) {
		if _, ok := s.Args[arg]; ok || len(s.Values[arg]) > 0 || s.Scope[arg] != "" {
			continue
		}
		return fmt.Errorf("required argument %q not filled", arg)
	}
	for arg, p := range s.Scope {
		if !slices.Contains(scopes, p) {
			return fmt.Errorf("argument %q mapped to %q, which isn't a scope parameter (%s)", arg, p, strings.Join(scopes, ", "))
		}
	}
	if len(s.Args) == 0 {
		return nil
	}
	if s.Parent == "" {
		return fmt.Errorf("args without a parent")
	}
	if schemas.Resources[s.Parent] == nil {
		return fmt.Errorf("parent %s is not a resource type", s.Parent)
	}
	for _, attr := range s.Args {
		if !hasAttr(schemas, s.Parent, attr) {
			return fmt.Errorf("parent %s has no attribute %q", s.Parent, attr)
		}
	}
	return nil
}

func hasAttr(schemas *tf.Schemas, typ, attr string) bool {
	if is := schemas.Identities[typ]; is != nil {
		if _, ok := is.Attributes[attr]; ok {
			return true
		}
	}
	if r := schemas.Resources[typ]; r != nil {
		if _, ok := r.Attributes[attr]; ok {
			return true
		}
	}
	return false
}

// CheckSource checks an inventory source: the data source exists, the list
// attribute holds entries with the fields it names, arguments exist and
// the required ones are set, and its partition, if any, is well formed.
func CheckSource(schemas *tf.Schemas, s inventory.Source) error {
	blk := schemas.DataSources[s.DataSource]
	if blk == nil {
		return fmt.Errorf("no data source %s", s.DataSource)
	}
	a := blk.Attributes[s.List]
	if a == nil {
		return fmt.Errorf("no attribute %q", s.List)
	}
	if !isDynamic(a) { // untyped entries are checked by reading them
		fields := objectListFields(a)
		for _, f := range append(s.IDFields(), s.Kind, s.Name) {
			if f != "" && !slices.Contains(fields, f) {
				return fmt.Errorf("%s has no string field %q (it has %s)", s.List, f, strings.Join(fields, ", "))
			}
		}
	}
	if s.ID == "" {
		return fmt.Errorf("no ID field")
	}
	if s.KindMatch != "" {
		if _, err := regexp.Compile(s.KindMatch); err != nil {
			return fmt.Errorf("kind_match: %w", err)
		}
	}
	for arg := range s.Config {
		if a := blk.Attributes[arg]; a == nil || !(a.Optional || a.Required) {
			return fmt.Errorf("no argument %q", arg)
		}
	}
	for arg := range s.Args {
		if a := blk.Attributes[arg]; a == nil || !(a.Optional || a.Required) {
			return fmt.Errorf("no argument %q", arg)
		}
	}
	if (s.Parent == "") != (len(s.Args) == 0) {
		return fmt.Errorf("parent and args go together")
	}
	if s.Parent != "" && schemas.Resources[s.Parent] == nil {
		return fmt.Errorf("parent %s is not a resource type", s.Parent)
	}
	for name, a := range blk.Attributes {
		_, inArgs := s.Args[name]
		if _, ok := s.Config[name]; a.Required && !ok && !inArgs {
			return fmt.Errorf("required argument %q not set", name)
		}
	}
	if s.Partition != nil {
		if err := checkPartition(blk, s, s.Partition, s.Complete); err != nil {
			return fmt.Errorf("partition: %w", err)
		}
	}
	return nil
}

var pathRoot = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*`)

// checkPartition checks a partition (and a completeness path) against the
// data source and the placeholders it needs.
func checkPartition(blk *tf.SchemaBlock, s inventory.Source, p *inventory.Partition, complete string) error {
	if s.Kind == "" {
		return fmt.Errorf("%s reports no kind field to partition by", s.DataSource)
	}
	if a := blk.Attributes[p.Arg]; a == nil || !(a.Optional || a.Required) {
		return fmt.Errorf("no argument %q", p.Arg)
	}
	if _, inArgs := s.Args[p.Arg]; inArgs {
		return fmt.Errorf("argument %q comes from the parent", p.Arg)
	}
	if p.Only == "" && p.Except == "" {
		return fmt.Errorf("neither only nor except")
	}
	if p.Only != "" && !strings.Contains(p.Only, "{kind}") {
		return fmt.Errorf("only has no {kind}")
	}
	if p.Except != "" && (!strings.Contains(p.Except, "{kinds}") || !strings.Contains(p.Each, "{kind}")) {
		return fmt.Errorf("except needs {kinds} and each needs {kind}")
	}
	for _, e := range []string{p.Only, strings.ReplaceAll(p.Except, "{kinds}", strings.ReplaceAll(p.Each, "{kind}", "k"))} {
		if e != "" && !hclExpr(strings.ReplaceAll(e, "{kind}", "k")) {
			return fmt.Errorf("%s is not an HCL expression", e)
		}
	}
	if complete != "" {
		root := pathRoot.FindString(complete)
		if a := blk.Attributes[root]; a == nil || !a.Computed {
			return fmt.Errorf("complete: no computed attribute %q", root)
		}
	}
	return nil
}

func hclExpr(s string) bool {
	_, diags := hclsyntax.ParseExpression([]byte(s), "expr.hcl", hcl.InitialPos)
	return !diags.HasErrors()
}

// CheckMapping checks an inventory kind's mapping: its type is a resource
// type, its pattern compiles, and its import ID template fits every
// sample ID. A mapping without a type (the kind isn't a resource of its
// own) needs a note saying why.
func CheckMapping(schemas *tf.Schemas, m inventory.Mapping, samples []string) error {
	if m.Type == "" {
		if m.Note == "" {
			return fmt.Errorf("no type and no note saying why")
		}
		return nil
	}
	if schemas.Resources[m.Type] == nil {
		return fmt.Errorf("no resource type %s", m.Type)
	}
	if m.Match != "" {
		if _, err := regexp.Compile(m.Match); err != nil {
			return fmt.Errorf("match: %w", err)
		}
	}
	for _, s := range samples {
		if _, ok := m.Target(s); !ok {
			return fmt.Errorf("the import ID template doesn't fit %q", s)
		}
	}
	return nil
}

// paramRef matches a scope parameter's reference: data.<source>.<name>.<attr>.
var paramRef = regexp.MustCompile(`^data\.([A-Za-z0-9_]+)\.([A-Za-z0-9_]+)\.([A-Za-z0-9_]+)$`)

// CheckParam checks a scope parameter's data source: a data source without
// required arguments, declared by data, with the attribute ref reads.
func CheckParam(schemas *tf.Schemas, ref, data string) error {
	m := paramRef.FindStringSubmatch(ref)
	if m == nil {
		return fmt.Errorf("ref %q isn't data.<data source>.<name>.<attribute>", ref)
	}
	blk := schemas.DataSources[m[1]]
	if blk == nil {
		return fmt.Errorf("no data source %s", m[1])
	}
	if hasRequired(blk) {
		return fmt.Errorf("%s needs arguments", m[1])
	}
	if blk.Attributes[m[3]] == nil {
		return fmt.Errorf("%s has no attribute %q", m[1], m[3])
	}
	if !strings.Contains(data, fmt.Sprintf("data %q %q", m[1], m[2])) {
		return fmt.Errorf("data doesn't declare data %q %q", m[1], m[2])
	}
	return nil
}
