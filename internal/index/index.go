// Package index maps attribute values back to the resources that own them.
package index

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
)

// Ref is a resource attribute that can be referenced: aws_iam_role.foo.arn
type Ref struct {
	Address string
	Type    string
	Attr    string
	// Key is the map key that ends Attr, when it is one: it may contain
	// dots ("labels.app.kubernetes.io/name").
	Key string
	// Assigned means the provider computes this attribute: the resource is
	// where the value comes from, not a resource that refers to it.
	Assigned bool
}

// String renders the reference; nested attributes ("identity.0.principal_id")
// use index syntax: azurerm_kubernetes_cluster.c.identity[0].principal_id.
func (r Ref) String() string {
	var b strings.Builder
	b.WriteString(r.Address)
	attr := r.Attr
	if r.Key != "" {
		attr = strings.TrimSuffix(attr, "."+r.Key)
	}
	for _, seg := range strings.Split(attr, ".") {
		if _, err := strconv.Atoi(seg); err == nil {
			b.WriteString("[" + seg + "]")
		} else if identifier.MatchString(seg) {
			b.WriteString("." + seg)
		} else {
			b.WriteString("[" + strconv.Quote(seg) + "]") // a map key such as "app/name"
		}
	}
	if r.Key != "" {
		b.WriteString("[" + strconv.Quote(r.Key) + "]")
	}
	return b.String()
}

// Value returns the value at a dotted path ("identity.0.principal_id").
func Value(v any, path string) any {
	for _, seg := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			v = x[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

type Index struct {
	byValue map[string][]Ref
	// unique values of at least prefixLen bytes, by prefix, for finding
	// values embedded in larger strings.
	byPrefix map[string][]string
	// values by "type attr", for finding names embedded in strings
	byAttr map[string][]string
	// aliases maps shortened spellings of unique values to the value, per
	// the rules' alias patterns.
	aliases map[string]string
	// scalars holds each resource's top-level string and number attribute
	// values (numbers in their decimal spelling), for spelling aliases.
	scalars map[string]map[string]string
	// setBy lists the resources that set a value in a top-level attribute
	// they don't compute: a property like a zone or a network, which a
	// resource computing it only copies.
	setBy map[string][]string
}

const prefixLen = 8

// minAlias is the shortest shortened spelling registered. Aliases shorter
// than prefixLen ("web:9") resolve whole-string literals only; they are not
// searched for inside larger strings.
const minAlias = 4

// Build indexes the identity attributes of every imported resource: "id",
// the provider-assigned (computed-only) strings and identity attributes
// from the schema, the attributes that rules point at, and the attribute
// holding the last part of a composite ID (see IDPartAttr).
func Build(plan *tf.PlanJSON, schemas *tf.Schemas, rs *rules.Set) (*Index, error) {
	ix := &Index{byValue: map[string][]Ref{}, byPrefix: map[string][]string{}, byAttr: map[string][]string{}, aliases: map[string]string{}, scalars: map[string]map[string]string{}, setBy: map[string][]string{}}
	for _, rc := range plan.ResourceChanges {
		if rc.Mode != "managed" {
			continue
		}
		vals, err := rc.Values()
		if err != nil {
			return nil, err
		}
		ix.scalars[rc.Address] = scalars(vals, schemas, rc.Type)
		if schemas != nil && schemas.Resources[rc.Type] != nil {
			for k, v := range ix.scalars[rc.Address] {
				if a := schemas.Resources[rc.Type].Attributes[k]; a != nil && (a.Optional || a.Required) && k != "id" {
					ix.setBy[v] = append(ix.setBy[v], rc.Address)
				}
			}
		}
		seen := map[string]bool{}
		assigned := map[string]bool{}
		if schemas != nil {
			for _, a := range schemas.ComputedStrings(rc.Type) {
				assigned[a] = true
			}
		}
		attrs := identityAttrs(rc.Type, schemas, rs)
		if id, _ := vals["id"].(string); id != "" {
			if a := IDPartAttr(vals, id); a != "" {
				attrs = append(attrs, a)
			}
		}
		for _, attr := range attrs {
			if seen[attr] {
				continue
			}
			seen[attr] = true
			if prefix, ok := strings.CutSuffix(attr, ".*"); ok {
				m, _ := Value(vals, prefix).(map[string]any)
				for k, v := range m {
					if s, ok := v.(string); ok && s != "" {
						ix.add(s, Ref{Address: rc.Address, Type: rc.Type, Attr: prefix + "." + k, Key: k})
					}
				}
				continue
			}
			s, ok := Value(vals, attr).(string)
			if !ok || s == "" {
				continue
			}
			ix.add(s, Ref{Address: rc.Address, Type: rc.Type, Attr: attr, Assigned: assigned[attr]})
		}
	}
	for v, refs := range ix.byValue {
		sort.SliceStable(refs, func(i, j int) bool { return better(refs[i], refs[j]) })
		ix.byValue[v] = refs
		if len(v) >= prefixLen && ix.Unique(v) {
			ix.byPrefix[v[:prefixLen]] = append(ix.byPrefix[v[:prefixLen]], v)
		}
	}
	ix.buildAliases(rs.Aliases())
	for p, vs := range ix.byPrefix {
		sort.Slice(vs, func(i, j int) bool { return len(vs[i]) > len(vs[j]) })
		ix.byPrefix[p] = vs
	}
	return ix, nil
}

// IDPartAttr finds the attribute holding the last part of a composite
// import ID ("name" of "namespace/name"): a resource's own name, which
// others refer to without its container. Of several attributes with that
// value, the least nested one is the resource's own; the others are copies
// (labels, templates).
func IDPartAttr(vals map[string]any, id string) string {
	i := strings.LastIndexAny(id, "/:")
	if i < 0 || i == len(id)-1 {
		return ""
	}
	part := id[i+1:]
	best, depth := "", 0
	var walk func(v any, path []string)
	walk = func(v any, path []string) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				walk(c, append(path, k))
			}
		case []any:
			for i, c := range x {
				walk(c, append(path, strconv.Itoa(i)))
			}
		case string:
			p := strings.Join(path, ".")
			if x == part && len(path) > 0 && (best == "" || len(path) < depth || len(path) == depth && p < best) {
				best, depth = p, len(path)
			}
		}
	}
	walk(vals, nil)
	return best
}

func identityAttrs(typ string, schemas *tf.Schemas, rs *rules.Set) []string {
	attrs := []string{"id"}
	if schemas != nil {
		attrs = append(attrs, schemas.ComputedStrings(typ)...)
		if is := schemas.Identities[typ]; is != nil {
			for name := range is.Attributes {
				attrs = append(attrs, name)
			}
		}
	}
	attrs = append(attrs, rs.TargetAttrs(typ)...)
	sort.Strings(attrs[1:])
	return attrs
}

// better orders candidates for the same value. When a parent and a child
// resource share an ID (a queue and its queue policy both use the queue
// URL), the parent has the shorter type name.
func better(a, b Ref) bool {
	if a.Assigned != b.Assigned {
		return a.Assigned
	}
	if ra, rb := attrRank(a.Attr), attrRank(b.Attr); ra != rb {
		return ra < rb
	}
	if len(a.Type) != len(b.Type) {
		return len(a.Type) < len(b.Type)
	}
	if a.Address != b.Address {
		return a.Address < b.Address
	}
	return a.Attr < b.Attr
}

// "id" is the least descriptive attribute to reference when another
// attribute holds the same value.
func attrRank(attr string) int {
	if attr == "id" {
		return 1
	}
	return 0
}

// Lookup returns all refs whose value equals v (or is the alias of a unique
// value), best first.
func (ix *Index) Lookup(v string) []Ref { return ix.byValue[ix.canon(v)] }

// Find returns the indexed attribute whose reference renders as ref
// (aws_x.y.id).
func (ix *Index) Find(ref string) (Ref, bool) {
	for _, rs := range ix.byValue {
		for _, r := range rs {
			if r.String() == ref {
				return r, true
			}
		}
	}
	return Ref{}, false
}

// canon maps an alias to the value it stands for.
func (ix *Index) canon(v string) string {
	if _, ok := ix.byValue[v]; ok {
		return v
	}
	if full, ok := ix.aliases[v]; ok {
		return full
	}
	return v
}

// IsAlias reports whether v is a shortened spelling of an indexed value
// rather than the value itself.
func (ix *Index) IsAlias(v string) bool {
	_, exact := ix.byValue[v]
	_, alias := ix.aliases[v]
	return !exact && alias
}

// scalars returns the top-level string and number attributes of vals,
// leaving out those the schema marks sensitive.
func scalars(vals map[string]any, schemas *tf.Schemas, typ string) map[string]string {
	out := map[string]string{}
	var attrs map[string]*tf.SchemaAttr
	if schemas != nil && schemas.Resources[typ] != nil {
		attrs = schemas.Resources[typ].Attributes
	}
	for k, v := range vals {
		if a := attrs[k]; a != nil && a.Sensitive {
			continue
		}
		switch x := v.(type) {
		case string:
			if x != "" {
				out[k] = x
			}
		case float64:
			out[k] = strconv.FormatFloat(x, 'f', -1, 64)
		}
	}
	return out
}

// Piece is part of a spelled alias: literal text, or the attribute of the
// target resource whose value it is.
type Piece struct {
	Lit  string
	Attr string
}

// Spell writes alias, a shortened spelling of the value r stands for, from
// the attributes of r's resource, so that a reference evaluates to the alias
// itself and not to the full identifier ("web:12" is family ":" revision;
// an ARN without its random suffix ends in the name). Attributes are matched
// from the end of the alias, longest value first, each starting on a word
// boundary and separated from the next by non-word bytes; whatever is left
// at the front stays literal. It returns nil if no attribute matches the
// end of the alias.
func (ix *Index) Spell(alias string, r Ref) []Piece {
	vals := ix.scalars[r.Address]
	var pieces []Piece // from the end
	matched := false
	rest := alias
	for rest != "" {
		attr := ""
		for k, v := range vals {
			if k == r.Attr || !strings.HasSuffix(rest, v) {
				continue
			}
			if start := len(rest) - len(v); start > 0 && isWordByte(rest[start-1]) {
				continue
			}
			if attr == "" || spellBetter(k, v, attr, vals[attr]) {
				attr = k
			}
		}
		if attr == "" {
			break
		}
		pieces, matched = append(pieces, Piece{Attr: attr}), true
		rest = rest[:len(rest)-len(vals[attr])]
		// The boundary check above leaves at least one separator byte.
		sep := len(rest)
		for sep > 0 && !isWordByte(rest[sep-1]) {
			sep--
		}
		if sep == len(rest) {
			break
		}
		pieces = append(pieces, Piece{Lit: rest[sep:]})
		rest = rest[:sep]
	}
	if !matched {
		return nil
	}
	if rest != "" {
		if last := &pieces[len(pieces)-1]; last.Lit != "" {
			last.Lit = rest + last.Lit
		} else {
			pieces = append(pieces, Piece{Lit: rest})
		}
	}
	for i, j := 0, len(pieces)-1; i < j; i, j = i+1, j-1 {
		pieces[i], pieces[j] = pieces[j], pieces[i]
	}
	return pieces
}

// spellBetter prefers the longer value, then any attribute over "id" (the
// least descriptive), then the first name.
func spellBetter(k, v, bk, bv string) bool {
	if len(v) != len(bv) {
		return len(v) > len(bv)
	}
	if ra, rb := attrRank(k), attrRank(bk); ra != rb {
		return ra < rb
	}
	return k < bk
}

// buildAliases registers the shortened spellings of unique structured
// values that the rules' alias patterns give (a revisioned object's
// "family:revision", a name without a random suffix). An alias is kept only
// if exactly one value claims it and no value is spelled that way itself.
func (ix *Index) buildAliases(patterns []rules.Alias) {
	claims := map[string]map[string]bool{}
	claim := func(alias, full string) {
		if len(alias) < minAlias {
			return
		}
		if claims[alias] == nil {
			claims[alias] = map[string]bool{}
		}
		claims[alias][full] = true
	}
	for v := range ix.byValue {
		if !structured.MatchString(v) || !ix.Unique(v) {
			continue
		}
		for _, p := range patterns {
			if short, ok := p.Short(v); ok {
				claim(short, v)
			}
		}
	}
	for alias, fulls := range claims {
		if _, taken := ix.byValue[alias]; taken || len(fulls) != 1 {
			continue
		}
		for full := range fulls {
			ix.aliases[alias] = full
			if len(alias) >= prefixLen {
				ix.byPrefix[alias[:prefixLen]] = append(ix.byPrefix[alias[:prefixLen]], alias)
			}
		}
	}
}

func (ix *Index) add(v string, r Ref) {
	ix.byValue[v] = append(ix.byValue[v], r)
	k := r.Type + " " + r.Attr
	ix.byAttr[k] = append(ix.byAttr[k], v)
}

// Values returns the values of one attribute of a resource type.
func (ix *Index) Values(typ, attr string) []string { return ix.byAttr[typ+" "+attr] }

var (
	identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	uuid       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	structured = regexp.MustCompile(`[:/]`)
	hasLetter  = regexp.MustCompile(`[A-Za-z]`)
	hasDigit   = regexp.MustCompile(`[0-9]`)
)

// Unique reports whether an exact match on v can be linked without a rule:
// exactly one resource owns it, and it is shaped like a generated
// identifier rather than a human-chosen name. Structured identifiers (ARNs,
// resource paths, URLs, self links), UUIDs and mixed letter/digit IDs
// qualify; "api" or "prod-db" do not.
func (ix *Index) Unique(v string) bool {
	_, ok := ix.Owner(v)
	return ok
}

// Owner returns the resource that owns v, if exactly one does (see Unique).
//
// The owner is the resource the provider assigned the value to. Resources
// that merely carry it as an input (a function policy's function ARN) don't
// count, unless no resource has it assigned.
//
// A value one resource computes and another sets (a zone a subnet sets,
// which a mount target computes as its zone name; a network's default
// group, which rules set) is a shared property: then only a resource whose
// own identifier it is can own it (the network itself, the default group).
// A computed value holding the resource's own id or name (a function ARN
// another resource sets as its target, a load balancer's DNS name) is
// never a copy.
func (ix *Index) Owner(v string) (Ref, bool) {
	v = ix.canon(v)
	refs := ix.byValue[v]
	property := false
	for _, r := range refs {
		if r.Assigned && !ix.selfID(r, v) && ix.setElsewhere(r, v) {
			property = true
		}
	}
	owners, assigned, ids := map[string]Ref{}, map[string]Ref{}, map[string]Ref{}
	for _, r := range refs {
		if property && !(ix.selfID(r, v) && (r.Assigned || r.Attr == "id")) {
			continue
		}
		if _, ok := owners[r.Address]; !ok {
			owners[r.Address] = r
		}
		if _, ok := assigned[r.Address]; r.Assigned && !ok {
			assigned[r.Address] = r
		}
		if _, ok := ids[r.Address]; r.Attr == "id" && !ok {
			ids[r.Address] = r
		}
	}
	if len(assigned) > 1 {
		// A child computing a copy of its parent's value (a mapping's
		// function ARN) isn't a second owner: the one resource whose own
		// id or name the value holds is.
		own := map[string]Ref{}
		for a, r := range assigned {
			if ix.selfID(r, v) {
				own[a] = r
			}
		}
		if len(own) == 1 {
			assigned = own
		}
	}
	switch {
	case len(assigned) > 0:
		owners = assigned
	case len(ids) > 0:
		// The resource whose own id it is owns the value; the others hold
		// it in an identity or input attribute (a child's parent_id), which
		// is a reference to the owner, not a second owner.
		owners = ids
	}
	if len(owners) != 1 || !IDShaped(v) {
		return Ref{}, false
	}
	for _, r := range owners {
		return r, true
	}
	return Ref{}, false
}

// Named returns the resource whose own identifier v is, even when v is a
// human-chosen name rather than ID-shaped: no other resource has v in an
// indexed attribute or sets it anywhere at the top level. Names are only
// reliable where nothing but a resource name is expected; callers decide
// where that is.
func (ix *Index) Named(v string) (Ref, bool) {
	refs := ix.byValue[v]
	if len(refs) == 0 {
		return Ref{}, false
	}
	addr, own := refs[0].Address, false
	for _, r := range refs {
		if r.Address != addr {
			return Ref{}, false
		}
		own = own || r.Assigned || r.Attr == "id"
	}
	for _, a := range ix.setBy[v] {
		if a != addr {
			return Ref{}, false
		}
	}
	return refs[0], own
}

// selfID reports whether r's value is its resource's own identifier: its
// id, or a value containing its id or name (an ARN ending in the name).
func (ix *Index) selfID(r Ref, v string) bool {
	id, name := ix.scalars[r.Address]["id"], ix.scalars[r.Address]["name"]
	return r.Attr == "id" || (id != "" && strings.Contains(v, id)) || (len(name) >= 4 && strings.Contains(v, name))
}

// setElsewhere reports whether another resource sets v in an attribute.
func (ix *Index) setElsewhere(r Ref, v string) bool {
	for _, a := range ix.setBy[v] {
		if a != r.Address {
			return true
		}
	}
	return false
}

// IDShaped reports whether v looks like a generated identifier rather than
// a human-chosen name: a UUID, a structured identifier (ARN, resource
// path, URL, self link) or a mixed letter/digit ID. "api" and "prod-db"
// are names.
func IDShaped(v string) bool {
	switch {
	case uuid.MatchString(v):
		return true
	case structured.MatchString(v):
		return len(v) >= 10
	default:
		return len(v) >= 8 && hasLetter.MatchString(v) && hasDigit.MatchString(v)
	}
}

// Span is a reference found inside a larger string.
type Span struct {
	Start, End int
	Value      string
}

// Embedded finds unique indexed values inside s, longest match first, that
// start and end on boundaries: "arn:aws:s3:::bucket/*" yields
// "arn:aws:s3:::bucket", but "role/foo" doesn't match inside "role/foo-bar".
func (ix *Index) Embedded(s string) []Span {
	var spans []Span
	for i := 0; i+prefixLen <= len(s); i++ {
		if i > 0 && isWordByte(s[i-1]) {
			continue
		}
		for _, v := range ix.byPrefix[s[i:i+prefixLen]] {
			end := i + len(v)
			if end > len(s) || s[i:end] != v || (end < len(s) && isWordByte(s[end])) {
				continue
			}
			if v == s {
				break // whole-string matches are handled as exact matches
			}
			spans = append(spans, Span{i, end, v})
			i = end - 1
			break
		}
	}
	return spans
}

func isWordByte(c byte) bool {
	return c == '_' || c == '-' || c == '.' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}
