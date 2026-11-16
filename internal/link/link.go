// Package link rewrites literal values in generated terraform config into
// references to the resources that own them.
package link

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
)

type Options struct {
	Rules *rules.Set
	// Params are scope values (account, project, region) to replace with
	// references wherever they appear as a whole word in a string.
	Params []Param
	// Drop lists attributes to remove everywhere, typically computed
	// mirrors of other attributes (e.g. tags_all); "type.attr" removes one
	// from that type only, before linking, so it makes no edges.
	Drop []string
	// Duplicates are inline attributes of single resources that a
	// standalone resource manages too (see Duplicates); they are removed
	// before linking.
	Duplicates []Duplicate
	// ManagedBy maps "type.attr" to standalone types managing the same
	// things (see ManagedDuplicates).
	ManagedBy map[string][]string
	// Schemas, when set, tell which values are entries of a free-form map
	// (an environment's variables), where a resource's plain name links
	// (see named).
	Schemas *tf.Schemas
	// unresolved, when set, receives every whole-string literal that no
	// rule or unique value resolved.
	unresolved func(Literal)
	// changed and imported hold, per address, the top-level attributes
	// the import plan would change and those it imported a value for
	// (see importPlan).
	changed, imported map[string]map[string]bool
}

type Param struct {
	Value string
	Ref   string // e.g. data.aws_caller_identity.current.account_id
	Data  string // the data block declaring Ref's data source
}

type Kind string

const (
	KindUnique   Kind = "unique"   // exact match on a globally unique value
	KindRule     Kind = "rule"     // exact match on a name, via a rule
	KindEmbedded Kind = "embedded" // unique value inside a larger string
)

// Edge is one reference written into the config.
type Edge struct {
	From string `json:"from"`
	Path string `json:"path"`
	Ref  string `json:"ref"`
	Kind Kind   `json:"kind"`
	// Rule is the origin of the rule used, for KindRule edges.
	Rule rules.Origin `json:"rule,omitempty"`
	// Value is the literal the reference replaced.
	Value      string     `json:"value"`
	Confidence Confidence `json:"confidence"`
	// Basis says, in one sentence, why the reference was made.
	Basis string `json:"basis"`
	// Limitations say what the reference doesn't establish.
	Limitations []string `json:"limitations,omitempty"`
	to          string
}

// To is the address of the resource the edge points at.
func (e Edge) To() string { return e.to }

type Result struct {
	Config      []byte
	Data        []byte // data source blocks, empty if none are used
	Edges       []Edge
	CycleBreaks [][2]string // from -> to pairs left as literals to avoid cycles
	// Ignored are "address.attr" pairs config leaves out that a plan
	// would change, now in the resource's ignore_changes.
	Ignored []string
}

// Link rewrites src (terraform-generated config) using the index.
func Link(src []byte, ix *index.Index, opts Options) (*Result, error) {
	blocked := map[[2]string]bool{}
	var breaks [][2]string
	var prev []Edge
	// Blocking an edge can make a value fall back to another candidate,
	// which may itself close a cycle, so iterate until stable. A run that
	// linked names by rule is repeated once with the edges it found, which
	// tell which container each resource is in (see pick).
	for range 10 {
		w, err := run(src, ix, opts, blocked, prev)
		if err != nil {
			return nil, err
		}
		back := backEdges(w.edges)
		if len(back) == 0 {
			if w.byRule && prev == nil {
				prev = w.edges
				continue
			}
			return w.result(breaks)
		}
		for _, e := range back {
			blocked[e] = true
			breaks = append(breaks, e)
		}
	}
	return nil, fmt.Errorf("could not break reference cycles")
}

// Literal is a string value at an attribute path of a resource.
type Literal struct {
	Addr, Type, Path, Value string
	// Linked are the parts of Value that did become references: unique
	// values found inside it, names embedded rules found, scope values. A
	// Value that is one of them whole (a region) is already a reference.
	Linked []string
}

// Unresolved returns the whole-string literals that the rules and unique
// values in opts don't resolve as a whole; these are candidates for new
// rules.
func Unresolved(src []byte, ix *index.Index, opts Options) ([]Literal, error) {
	var out []Literal
	opts.unresolved = func(l Literal) { out = append(out, l) }
	if _, err := run(src, ix, opts, map[[2]string]bool{}, nil); err != nil {
		return nil, err
	}
	return out, nil
}

func run(src []byte, ix *index.Index, opts Options, blocked map[[2]string]bool, prev []Edge) (*walker, error) {
	f, diags := hclwrite.ParseConfig(src, "generated.tf", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse generated config: %s", diags.Error())
	}
	w := &walker{ix: ix, opts: opts, blocked: blocked, prev: prev, file: f, usedParams: map[string]Param{}}
	for _, b := range f.Body().Blocks() {
		addr := hclx.Address(b)
		if addr == "" {
			continue
		}
		w.addr, w.typ = addr, b.Labels()[0]
		var removed []string
		for _, d := range opts.Drop {
			if typ, name, ok := strings.Cut(d, "."); !ok {
				removed = append(removed, d) // dropped everywhere, by tidy
			} else if typ == w.typ {
				b.Body().RemoveAttribute(name)
				removed = append(removed, name)
			}
		}
		for _, d := range opts.Duplicates {
			if d.Addr != addr {
				continue
			}
			b.Body().RemoveAttribute(d.Attr)
			for _, nb := range b.Body().Blocks() {
				if nb.Type() == d.Attr {
					b.Body().RemoveBlock(nb)
				}
			}
			removed = append(removed, d.Attr)
		}
		// What config leaves out, the plan must not change: someone else
		// manages it (the provider, a standalone resource).
		for _, name := range removed {
			if opts.wouldChange(addr, w.typ, name) && !slices.Contains(w.ignored, addr+"."+name) {
				hclx.IgnoreChanges(b.Body(), name)
				w.ignored = append(w.ignored, addr+"."+name)
			}
		}
		if err := w.body(b.Body(), nil); err != nil {
			return nil, fmt.Errorf("%s: %w", addr, err)
		}
	}
	return w, nil
}

type walker struct {
	ix      *index.Index
	opts    Options
	blocked map[[2]string]bool
	file    *hclwrite.File

	addr, typ  string
	edges      []Edge
	usedParams map[string]Param  // by Ref
	prev       []Edge            // edges of the previous run, for pick
	byRule     bool              // pick was asked to choose a rule's target
	containers map[string]bool   // "path type" pairs that hold containers
	entries    map[string]string // literal entries of the object being walked
	freeMaps   map[string]bool   // map paths by whether few types have them
	linked     []string          // values linked inside the literal being resolved
	ignored    []string          // see Result.Ignored
}

func (w *walker) body(b *hclwrite.Body, path []string) error {
	attrs := b.Attributes()
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		src := attrs[name].Expr().BuildTokens(nil).Bytes()
		expr, diags := hclsyntax.ParseExpression(src, "", hcl.InitialPos)
		if diags.HasErrors() {
			continue
		}
		if t, ok := expr.(*hclsyntax.ScopeTraversalExpr); ok && path == nil {
			w.reference(b, name, strings.TrimSpace(string(t.Range().SliceBytes(src))))
			continue
		}
		n, changed := w.node(toNode(expr, src), join(path, name))
		if !changed {
			continue
		}
		toks, err := hclx.ExprTokens(emit(n))
		if err != nil {
			return fmt.Errorf("re-parse generated expression: %w", err)
		}
		b.SetAttributeRaw(name, toks)
	}
	for _, blk := range b.Blocks() {
		if err := w.body(blk.Body(), join(path, blk.Type())); err != nil {
			return err
		}
	}
	return nil
}

// wouldChange says whether a plan would change attribute or block name of
// the resource at addr once config leaves it out: the import plan already
// changes it, or it was imported with a value that config alone sets (a
// computed attribute keeps its value; whether a nested block's entries are
// computed, the schema doesn't say).
func (o Options) wouldChange(addr, typ, name string) bool {
	if o.changed[addr][name] {
		return true
	}
	if !o.imported[addr][name] {
		return false
	}
	if o.Schemas == nil || o.Schemas.Resources[typ] == nil {
		return false
	}
	a := o.Schemas.Resources[typ].Attributes[name]
	return a == nil || !a.Computed
}

// reference records a reference generate already wrote: a sensitive
// attribute whose value is another resource's identifier gets the owner's
// reference instead of the value, which is never written. If the edge
// closes a cycle, the attribute gets the placeholder for an unexported
// sensitive value back.
func (w *walker) reference(b *hclwrite.Body, name, ref string) {
	r, ok := w.ix.Find(ref)
	if !ok || r.Address == w.addr {
		return
	}
	if w.blocked[[2]string{w.addr, r.Address}] {
		b.SetAttributeValue(name, cty.StringVal(hclx.SensitivePlaceholder))
		hclx.IgnoreChanges(b, name)
		return
	}
	ev := uniqueEvidence(r, false)
	ev.basis = fmt.Sprintf("the provider marks this attribute sensitive; its value is %s, which no other resource owns, and the import plan accepted the reference", r)
	ev.limitations = append(ev.limitations, "the value itself is not exported, only that it equals the target's")
	w.edges = append(w.edges, Edge{
		From: w.addr, Path: name, Ref: ref, Kind: KindUnique, to: r.Address,
		Value: "(sensitive)", Confidence: ev.confidence, Basis: ev.basis, Limitations: ev.limitations,
	})
}

func join(path []string, seg string) []string {
	out := make([]string, len(path), len(path)+1)
	copy(out, path)
	return append(out, seg)
}

func (w *walker) node(n node, path []string) (node, bool) {
	switch n := n.(type) {
	case strNode:
		s, ok := n.literal()
		if !ok {
			return n, false
		}
		return w.str(s, path)
	case numNode:
		return w.num(n, path)
	case listNode:
		changed := false
		for i, it := range n.items {
			var c bool
			n.items[i], c = w.node(it, path)
			changed = changed || c
		}
		return n, changed
	case objNode:
		changed := false
		outer := w.entries
		w.entries = map[string]string{}
		for _, it := range n.items {
			if s, ok := it.val.(strNode); ok {
				if v, ok := s.literal(); ok {
					w.entries[it.key] = v
				}
			}
		}
		defer func() { w.entries = outer }()
		for i, it := range n.items {
			var c bool
			n.items[i].val, c = w.node(it.val, join(path, it.key))
			changed = changed || c
		}
		return n, changed
	case callNode:
		changed := false
		for i, a := range n.args {
			var c bool
			n.args[i], c = w.node(a, path)
			changed = changed || c
		}
		return n, changed
	}
	return n, false
}

func addresses(refs []index.Ref) map[string]bool {
	out := map[string]bool{}
	for _, r := range refs {
		out[r.Address] = true
	}
	return out
}

func (w *walker) allowed(r index.Ref) bool {
	return r.Address != w.addr && !w.blocked[[2]string{w.addr, r.Address}]
}

// pick chooses among resources matching a rule. Names are not unique: when
// several resources have the value, prefer one this resource already
// references (an autoscaling policy's service_namespace belongs to the
// target its resource_id points at), else the only one that references
// something this resource references too (the same namespace or resource
// group, from the previous run); otherwise it's a guess, so don't. why
// says how a choice among several was made.
func (w *walker) pick(matches []index.Ref) (ref index.Ref, why string, ok bool) {
	w.byRule = true
	all := len(addresses(matches))
	matches = slices.DeleteFunc(slices.Clone(matches), w.elsewhere)
	if len(matches) == 0 {
		return index.Ref{}, "", false
	}
	if len(addresses(matches)) == 1 {
		if all > 1 {
			return matches[0], "in this resource's container", true
		}
		return matches[0], "", true
	}
	for _, m := range matches {
		for _, e := range w.edges {
			if e.From == w.addr && e.to == m.Address {
				return m, "this resource already references", true
			}
		}
	}
	mine := map[string]bool{}
	for _, e := range w.prev {
		if e.From == w.addr {
			mine[e.to] = true
		}
	}
	var shared []index.Ref
	var via string
	seen := map[string]bool{}
	for _, m := range matches {
		for _, e := range w.prev {
			if e.From == m.Address && mine[e.to] && !seen[m.Address] {
				seen[m.Address] = true
				shared = append(shared, m)
				via = e.to
			}
		}
	}
	if len(shared) == 1 {
		return shared[0], "that also references " + via, true
	}
	return index.Ref{}, "", false
}

// elsewhere reports a candidate in another container than this resource:
// both reference resources of one type through the same attribute path,
// a path many types use (a namespace, a resource group), but not the same
// one. Names are only unique within a container. Needs the previous run.
func (w *walker) elsewhere(m index.Ref) bool {
	if w.prev == nil {
		return false
	}
	if w.containers == nil {
		users := map[string]map[string]bool{} // "path type" -> source types
		for _, e := range w.prev {
			k := e.Path + " " + typeOf(e.to)
			if users[k] == nil {
				users[k] = map[string]bool{}
			}
			users[k][typeOf(e.From)] = true
		}
		w.containers = map[string]bool{}
		for k, ts := range users {
			w.containers[k] = len(ts) >= containerTypes
		}
	}
	mine := map[string]string{}
	for _, e := range w.prev {
		if e.From == w.addr && w.containers[e.Path+" "+typeOf(e.to)] {
			mine[e.Path+" "+typeOf(e.to)] = e.to
		}
	}
	for _, e := range w.prev {
		if e.From != m.Address {
			continue
		}
		if x, ok := mine[e.Path+" "+typeOf(e.to)]; ok && x != e.to {
			return true
		}
	}
	return false
}

// Paths referenced by this many resource types hold containers.
const containerTypes = 5

func typeOf(addr string) string {
	t, _, _ := strings.Cut(addr, ".")
	return t
}

// link records an edge from the current resource to r for value and
// returns the parts that replace value. A shortened spelling of r's
// identifier (name:revision, an ARN without its random suffix) is written
// from the target's own attributes so the reference evaluates to the literal
// itself: the full identifier would change the configuration. If it can't
// be, link returns false and the value stays literal.
func (w *walker) link(r index.Ref, path []string, kind Kind, value string, ev evidence) ([]part, bool) {
	parts := []part{{ref: r.String()}}
	ref := r.String()
	if w.ix.IsAlias(value) {
		pieces := w.ix.Spell(value, r)
		if pieces == nil {
			return nil, false
		}
		parts = parts[:0]
		var attrs []string
		for _, pc := range pieces {
			if pc.Attr == "" {
				parts = append(parts, w.parametrize(pc.Lit)...)
				continue
			}
			ar := index.Ref{Address: r.Address, Type: r.Type, Attr: pc.Attr}
			parts = append(parts, part{ref: ar.String()})
			if attrs == nil {
				ref = ar.String() // the edge names the first attribute
			}
			attrs = append(attrs, pc.Attr)
		}
		ev.confidence = Derived
		// A rule's basis doesn't start from the value: say it separately.
		if b := strings.Replace(ev.basis, "the value is ", "the value is a shortened spelling of ", 1); b != ev.basis {
			ev.basis = b
		} else {
			ev.basis += "; the value is a shortened spelling of the identifier"
		}
		ev.basis += fmt.Sprintf(", written from the target's %s", strings.Join(attrs, ", "))
	}
	w.linked = append(w.linked, value)
	w.edges = append(w.edges, Edge{
		From: w.addr, Path: strings.Join(path, "."), Ref: ref, Kind: kind, to: r.Address,
		Value: value, Confidence: ev.confidence, Basis: ev.basis, Limitations: ev.limitations,
	})
	return parts, true
}

// num resolves one number literal, by rule only: a number on its own (a
// port, a count) is too ambiguous to link by value. The reference is a
// string where the target keeps one, which Terraform converts back.
func (w *walker) num(n numNode, path []string) (node, bool) {
	pooled, ruleOf := w.ruleMatches(n.val, path)
	if len(pooled) == 0 {
		return n, false
	}
	parts, ok := w.ruleRef(ruleOf, pooled, path, n.val, KindRule)
	if !ok {
		return n, false
	}
	return strNode{parts: parts}, true
}

// ruleMatches returns the resources the exact rules for path find with
// value v, and the rule that found each (by address). Matches of every
// applicable rule are chosen among together: a selector's rules for two
// workload types must not each settle for a workload of another namespace.
func (w *walker) ruleMatches(v string, path []string) ([]index.Ref, map[string]rules.Rule) {
	p := strings.Join(path, ".")
	cands := w.ix.Lookup(v)
	var pooled []index.Ref
	ruleOf := map[string]rules.Rule{}
	for _, rule := range w.opts.Rules.For(w.typ, p, rules.MatchExact) {
		key, _ := rule.Key(p)
		matches := w.matches(cands, rule.Target, rule.TargetAttr(key))
		if key != "" {
			matches = w.selected(matches, rule, key)
		}
		for _, m := range matches {
			if _, ok := ruleOf[m.Address]; !ok {
				ruleOf[m.Address] = rule
				pooled = append(pooled, m)
			}
		}
	}
	return pooled, ruleOf
}

// str resolves one string literal.
func (w *walker) str(s string, path []string) (node, bool) {
	p := strings.Join(path, ".")
	if pooled, ruleOf := w.ruleMatches(s, path); len(pooled) > 0 {
		if parts, ok := w.ruleRef(ruleOf, pooled, path, s, KindRule); ok {
			return strNode{parts: parts}, true
		}
	}
	// Only a target's own identifier makes a reference: a resource that
	// merely carries the value as an input (a policy's resource ARN) is a
	// mention of the real owner, not the owner.
	if c, ok := w.ix.Owner(s); ok && w.allowed(c) && own(c) {
		if parts, ok := w.link(c, path, KindUnique, s, uniqueEvidence(c, false)); ok {
			return strNode{parts: parts}, true
		}
	}
	if c, ok := w.named(s, p); ok && w.allowed(c) {
		if parts, ok := w.link(c, path, KindUnique, s, namedEvidence(c)); ok {
			return strNode{parts: parts}, true
		}
	}
	// Reported after what's inside it is linked, so suggestions can leave
	// out what already became a reference.
	w.linked = nil
	if w.opts.unresolved != nil {
		defer func() {
			w.opts.unresolved(Literal{Addr: w.addr, Type: w.typ, Path: p, Value: s, Linked: w.linked})
		}()
	}

	if parts, ok := w.embeddedRules(s, path); ok {
		return strNode{parts: parts}, true
	}

	var parts []part
	changed := false
	pos := 0
	for _, sp := range w.ix.Embedded(s) {
		c, ok := w.ix.Owner(sp.Value)
		if !ok || !w.allowed(c) || !own(c) || !mentionable(c, sp.Value) {
			continue
		}
		refParts, ok := w.link(c, path, KindEmbedded, sp.Value, uniqueEvidence(c, true))
		if !ok {
			continue
		}
		parts = append(parts, w.parametrize(s[pos:sp.Start])...)
		parts = append(parts, refParts...)
		pos = sp.End
		changed = true
	}
	parts = append(parts, w.parametrize(s[pos:])...)
	for _, pt := range parts {
		if pt.ref != "" {
			changed = true
		}
	}
	return strNode{parts: parts}, changed
}

// Plain names shorter than this, or without a separator, are too likely
// to be ordinary words ("default", "prod") to link by name.
const minName = 6

// named finds the resource a plain name in a free-form map entry
// ("environment.variables.TABLE") is the own identifier of. Such a map holds
// whatever its author put there, so no rule can say what an entry names; a
// value that is exactly one resource's identifier, which no other resource
// has or sets, is taken to name it. Maps that many resource types have
// (tags, labels) are conventions and hold names for other reasons.
func (w *walker) named(s, path string) (index.Ref, bool) {
	sch := w.opts.Schemas
	if sch == nil || len(s) < minName || !strings.ContainsAny(s, "-_.") {
		return index.Ref{}, false
	}
	key, ok := sch.MapKey(w.typ, path)
	if !ok {
		return index.Ref{}, false
	}
	for _, p := range w.opts.Params {
		if p.Value == s {
			return index.Ref{}, false
		}
	}
	m := strings.TrimSuffix(path, "."+key)
	if w.freeMaps == nil {
		w.freeMaps = map[string]bool{}
	}
	free, seen := w.freeMaps[m]
	if !seen {
		free = sch.MapTypes(m) < containerTypes
		w.freeMaps[m] = free
	}
	if !free {
		return index.Ref{}, false
	}
	return w.ix.Named(s)
}

func (w *walker) matches(cands []index.Ref, typ, attr string) []index.Ref {
	var out []index.Ref
	for _, c := range cands {
		if c.Type == typ && c.Attr == attr && w.allowed(c) {
			out = append(out, c)
		}
	}
	return out
}

// selected keeps the matches of a wildcard rule whose map has every other
// literal entry of the map being walked too: a selector picks a resource
// only if all its entries match.
func (w *walker) selected(matches []index.Ref, rule rules.Rule, key string) []index.Ref {
	var out []index.Ref
	for _, m := range matches {
		all := true
		for k, v := range w.entries {
			if k == key || !all {
				continue
			}
			found := false
			for _, c := range w.ix.Lookup(v) {
				found = found || c.Address == m.Address && c.Attr == rule.TargetAttr(k)
			}
			all = found
		}
		if all {
			out = append(out, m)
		}
	}
	return out
}

// ruleRef links a value to the resource picked among matches, each found
// by a rule (by address).
func (w *walker) ruleRef(ruleOf map[string]rules.Rule, matches []index.Ref, path []string, value string, kind Kind) ([]part, bool) {
	c, why, ok := w.pick(matches)
	if !ok {
		return nil, false
	}
	rule := ruleOf[c.Address]
	parts, ok := w.link(c, path, kind, value, ruleEvidence(rule, c, index.IDShaped(value), len(addresses(matches)), why))
	if ok {
		w.edges[len(w.edges)-1].Rule = rule.Origin
	}
	return parts, ok
}

// embeddedRules replaces the names that embedded rules find inside s
// ("redis" in "redis.ns.svc.cluster.local"), leftmost and longest first.
// A name is bounded by characters names don't contain.
func (w *walker) embeddedRules(s string, path []string) ([]part, bool) {
	rs := w.opts.Rules.For(w.typ, strings.Join(path, "."), rules.MatchEmbedded)
	if len(rs) == 0 {
		return nil, false
	}
	var parts []part
	pos, changed := 0, false
	for i := 0; i < len(s); i++ {
		if i > 0 && isNameByte(s[i-1]) {
			continue
		}
		var best string
		var bestRule rules.Rule
		for _, r := range rs {
			for _, v := range w.ix.Values(r.Target, r.Attr) {
				end := i + len(v)
				if len(v) > len(best) && len(v) < len(s) && end <= len(s) && s[i:end] == v && (end == len(s) || !isNameByte(s[end])) {
					best, bestRule = v, r
				}
			}
		}
		if best == "" {
			continue
		}
		matches := w.matches(w.ix.Lookup(best), bestRule.Target, bestRule.Attr)
		ruleOf := map[string]rules.Rule{}
		for _, m := range matches {
			ruleOf[m.Address] = bestRule
		}
		refParts, ok := w.ruleRef(ruleOf, matches, path, best, KindEmbedded)
		if !ok {
			continue
		}
		parts = append(parts, w.parametrize(s[pos:i])...)
		parts = append(parts, refParts...)
		pos, changed = i+len(best), true
		i = pos - 1
	}
	if !changed {
		return nil, false
	}
	return append(parts, w.parametrize(s[pos:])...), true
}

func isNameByte(c byte) bool {
	return c == '_' || c == '-' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// replacement is a span of a string to be replaced by a parameter reference.
type replacement struct {
	start, end int
	p          Param
}

// parametrize replaces scope parameter values that appear as whole words in
// s ("arn:aws:iam::123456789012:root", "bucket-123456789012") with
// references.
func (w *walker) parametrize(s string) []part {
	if s == "" {
		return nil
	}
	var rs []replacement
	for _, p := range w.opts.Params {
		if p.Value == "" {
			continue // would match everywhere without advancing
		}
		for i := 0; ; {
			j := strings.Index(s[i:], p.Value)
			if j < 0 {
				break
			}
			start, end := i+j, i+j+len(p.Value)
			if (start == 0 || !isAlnum(s[start-1])) && (end == len(s) || !isAlnum(s[end])) && !overlaps(rs, start, end) {
				rs = append(rs, replacement{start, end, p})
			}
			i = end
		}
	}
	if len(rs) == 0 {
		return []part{{lit: s}}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].start < rs[j].start })
	var parts []part
	pos := 0
	for _, r := range rs {
		parts = append(parts, part{lit: s[pos:r.start]}, part{ref: r.p.Ref})
		pos = r.end
		w.usedParams[r.p.Ref] = r.p
		w.linked = append(w.linked, r.p.Value)
	}
	return append(parts, part{lit: s[pos:]})
}

func overlaps(rs []replacement, start, end int) bool {
	for _, r := range rs {
		if start < r.end && r.start < end {
			return true
		}
	}
	return false
}

func isAlnum(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// backEdges returns resource pairs whose references close a cycle, found by
// a depth-first search in deterministic order: for each cycle, its weakest
// pair. A pair is strong when one of its references is the target's own id
// (a child's parent_id), weak when every one is to another attribute (a
// parent's name matching a computed copy on its child); ties go to the
// back edge itself.
func backEdges(edges []Edge) [][2]string {
	adj := map[string][]string{}
	strong := map[[2]string]bool{}
	for _, e := range edges {
		k := [2]string{e.From, e.to}
		if _, ok := strong[k]; !ok {
			strong[k] = false
			adj[e.From] = append(adj[e.From], e.to)
		}
		strong[k] = strong[k] || strings.HasSuffix(e.Ref, ".id")
	}
	nodes := make([]string, 0, len(adj))
	for n := range adj {
		nodes = append(nodes, n)
		sort.Strings(adj[n])
	}
	sort.Strings(nodes)

	const (
		onStack = 1
		done    = 2
	)
	state := map[string]int{}
	var stack []string
	var back [][2]string
	var visit func(n string)
	visit = func(n string) {
		state[n] = onStack
		stack = append(stack, n)
		for _, m := range adj[n] {
			switch state[m] {
			case onStack:
				cut := [2]string{n, m}
				i := slices.Index(stack, m)
				for j := i; j < len(stack)-1 && strong[cut]; j++ {
					if k := [2]string{stack[j], stack[j+1]}; !strong[k] {
						cut = k
					}
				}
				if !slices.Contains(back, cut) {
					back = append(back, cut)
				}
			case 0:
				visit(m)
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = done
	}
	for _, n := range nodes {
		if state[n] == 0 {
			visit(n)
		}
	}
	return back
}

func (w *walker) result(breaks [][2]string) (*Result, error) {
	drop := map[string]bool{}
	for _, name := range w.opts.Drop {
		if !strings.Contains(name, ".") {
			drop[name] = true
		}
	}
	tidy(w.file.Body(), drop)
	out := hclwrite.Format(stripGeneratedComments(w.file.Bytes()))

	var data bytes.Buffer
	seen := map[string]bool{}
	for _, ref := range slices.Sorted(maps.Keys(w.usedParams)) {
		if d := w.usedParams[ref].Data; d != "" && !seen[d] {
			seen[d] = true
			data.WriteString(d + "\n")
		}
	}
	return &Result{Config: out, Data: data.Bytes(), Edges: w.edges, CycleBreaks: breaks, Ignored: w.ignored}, nil
}

// tidy drops attributes that carry no information: explicit nulls (the same
// as omitting them) and the provider pack's computed mirrors (e.g. tags_all).
func tidy(b *hclwrite.Body, drop map[string]bool) {
	for name, a := range b.Attributes() {
		v := strings.TrimSpace(string(a.Expr().BuildTokens(nil).Bytes()))
		if v == "null" || drop[name] {
			b.RemoveAttribute(name)
		}
	}
	for _, blk := range b.Blocks() {
		tidy(blk.Body(), drop)
	}
}

func stripGeneratedComments(src []byte) []byte {
	var out [][]byte
	for _, line := range bytes.Split(src, []byte("\n")) {
		t := bytes.TrimSpace(line)
		if bytes.HasPrefix(t, []byte("# __generated__")) || bytes.HasPrefix(t, []byte("# Please review these resources")) {
			continue
		}
		out = append(out, line)
	}
	return bytes.TrimLeft(bytes.Join(out, []byte("\n")), "\n")
}
