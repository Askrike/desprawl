package generate

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/tf"
)

// Attribute paths in diagnostic details: "domain_dns_ips",
// "action.0.forward.0.stickiness.0.duration".
var pathToken = regexp.MustCompile(`[a-z_][a-z0-9_]*(?:\.[0-9]+\.[a-z_][a-z0-9_]*)*`)

// resourceBody finds the resource a diagnostic is about. Diagnostics carry
// the resource address; their source ranges are relative to each resource's
// own generated snippet, not to the file, so they're only used when there
// is no address.
func resourceBody(f *hclwrite.File, src []byte, d tf.Diagnostic) (string, *hclwrite.Body) {
	if d.Address != "" {
		for _, b := range f.Body().Blocks() {
			if hclx.Address(b) == d.Address {
				return d.Address, b.Body()
			}
		}
		return d.Address, nil
	}
	if d.Range != nil {
		if loc, ok := hclx.Find(src, generatedFile, d.Range.Start.Line); ok && len(loc.Blocks) > 0 {
			b := hclx.TopBlock(f, loc)
			return hclx.Address(b), b.Body()
		}
	}
	return "", nil
}

// node is what a path resolves to: an attribute or a nested block, within
// its parent body.
type node struct {
	parent *hclwrite.Body
	name   string
	block  *hclwrite.Block // nil for attributes
	path   string
}

// resolve finds a dotted path ("a.0.b") in a resource body.
func resolve(body *hclwrite.Body, path string) (node, bool) {
	segs := strings.Split(path, ".")
	for i := 0; i < len(segs); i++ {
		name := segs[i]
		if a := body.GetAttribute(name); a != nil && i == len(segs)-1 {
			return node{parent: body, name: name, path: path}, true
		}
		idx := 0
		if i+1 < len(segs) && isIndex(segs[i+1]) {
			idx, _ = strconv.Atoi(segs[i+1])
			i++
		}
		var blk *hclwrite.Block
		n := 0
		for _, b := range body.Blocks() {
			if b.Type() == name {
				if n == idx {
					blk = b
					break
				}
				n++
			}
		}
		if blk == nil {
			return node{}, false
		}
		if i == len(segs)-1 {
			return node{parent: body, name: name, block: blk, path: path}, true
		}
		body = blk.Body()
	}
	return node{}, false
}

func isIndex(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// mentioned returns the paths named in a diagnostic that are set in body,
// in order of mention, and those that aren't (missing arguments): nested
// paths, and top-level names the schema or the identity knows.
func mentioned(body *hclwrite.Body, schema *tf.SchemaBlock, identity map[string]any, d tf.Diagnostic) (present []node, missing []string) {
	seen := map[string]bool{}
	// Some validators put the whole message in the summary.
	for _, p := range pathToken.FindAllString(d.Summary+" "+d.Detail, -1) {
		if seen[p] {
			continue
		}
		seen[p] = true
		if n, ok := resolve(body, p); ok && !isNullNode(n) {
			present = append(present, n)
		} else if _, inIdentity := identity[p]; strings.Contains(p, ".") || inIdentity || (schema != nil && schema.Attributes[p] != nil) {
			missing = append(missing, p)
		}
	}
	return present, missing
}

// anyPath is the scope of a diagnostic naming nothing in the resource: its
// repair may touch anything.
const anyPath = "*"

// diagScope returns the top-level attributes and blocks a diagnostic names,
// which its repair may touch.
func diagScope(body *hclwrite.Body, schema *tf.SchemaBlock, identity map[string]any, d tf.Diagnostic) map[string]bool {
	present, missing := mentioned(body, schema, identity, d)
	scope := map[string]bool{}
	for _, n := range present {
		scope[strings.Split(n.path, ".")[0]] = true
	}
	for _, p := range missing {
		scope[strings.Split(p, ".")[0]] = true
	}
	if len(scope) == 0 {
		scope[anyPath] = true
	}
	return scope
}

// overlaps reports whether a repair in scope b could interact with repairs
// already made in scope a.
func overlaps(a, b map[string]bool) bool {
	if len(a) == 0 {
		return false
	}
	if a[anyPath] || b[anyPath] {
		return true
	}
	for k := range b {
		if a[k] {
			return true
		}
	}
	return false
}

// repair fixes one validation error in a resource and describes what it
// did. The generated config is a faithful dump of remote state, so errors
// come from it saying too much: conflicting attributes, zero values where
// unset was meant, nested blocks generated without their required fields.
// The fix is almost always to remove something, and the best thing to
// remove is whatever carries no information.
//
// identity is the resource's import identity; it sometimes holds values
// that generated config left out. owned reports whether a value is the
// identity of one imported resource (a subnet id, not a zone name).
func repair(body *hclwrite.Body, schema *tf.SchemaBlock, identity map[string]any, owned func(string) bool, d tf.Diagnostic) (string, bool) {
	present, missing := mentioned(body, schema, identity, d)
	text := d.Summary + " " + d.Detail
	missing = listOrder(missing, text)

	switch {
	case urlError.MatchString(text):
		// The API returned a URL without its scheme ("host/path") and the
		// validator wants one: the value is right, only its form differs.
		// Weak point: https is assumed (a provider storing http:// would
		// show as drift in verify, not import wrongly), and urlError only
		// knows the English wording of the validators seen so far; other
		// wording falls through to the cases below.
		for _, n := range present {
			if n.block != nil {
				continue
			}
			if v, ok := literalString(n.parent.GetAttribute(n.name)); ok && schemeless.MatchString(v) {
				n.parent.SetAttributeValue(n.name, cty.StringVal("https://"+v))
				return "add https:// to " + n.path, true
			}
		}
		// An empty URL: unset was meant.
		return removeEmpty(present)

	case strings.Contains(text, "onflict") || strings.Contains(text, "only one of") || strings.Contains(text, "cannot be specified"):
		// Remove the empty side; otherwise an attribute rather than a
		// block, which loses the least information.
		for _, n := range present {
			if isEmptyNode(n) {
				return remove(n), true
			}
		}
		// Both set (an autoscaling group's zones and its subnets): keep
		// the side that names imported resources, which links.
		for i := len(present) - 1; i >= 0; i-- {
			if present[i].block == nil && !namesOwned(present[i], owned) && slices.ContainsFunc(present, func(n node) bool { return namesOwned(n, owned) }) {
				return remove(present[i]), true
			}
		}
		for i := len(present) - 1; i >= 0; i-- {
			if present[i].block == nil {
				return remove(present[i]), true
			}
		}
		if len(present) > 0 {
			return remove(present[len(present)-1]), true
		}

	case strings.Contains(text, "is required") || strings.Contains(text, "all of"):
		// "all of `a,b,c` must be specified" when one is set: the set one
		// is a zero value the API returned for a feature not in use (a
		// subnet's customer-owned IP flag), so remove it, the emptiest
		// first. Placeholders for the others would claim the feature is.
		if strings.Contains(text, "all of") {
			for _, n := range present {
				if isEmptyNode(n) {
					return remove(n), true
				}
			}
		} else if what, ok := placeholder(body, schema, missing, false); ok {
			// A required top-level string the API never returned (a key's
			// public key): stand in a placeholder rather than lose the
			// resource.
			return what, true
		}
		// A nested block was generated without a required field: remove
		// the block. "all of `a,b` must be specified": remove the one set.
		for _, p := range missing {
			segs := strings.Split(p, ".")
			if len(segs) >= 3 {
				if n, ok := resolve(body, strings.Join(segs[:len(segs)-2], ".")); ok && n.block != nil {
					return remove(n), true
				}
			}
		}
		for _, n := range present {
			return remove(n), true
		}

	case strings.Contains(text, "must be specified") || strings.Contains(text, "one of"):
		// "one of `a,b,c` must be specified" with none set. The identity
		// may hold the value generated config left out.
		for _, p := range missing {
			if v, ok := identity[p].(string); ok && v != "" {
				body.SetAttributeValue(p, cty.StringVal(v))
				return "set " + p + " from identity", true
			}
		}
		// When one is sensitive the value exists but was (rightly) not
		// exported: stand in a placeholder and keep terraform from ever
		// writing it.
		if what, ok := placeholder(body, schema, missing, true); ok {
			return what, true
		}
		// Otherwise any settable string the API didn't return (a function's
		// code source): the placeholder is never applied, and only kept if
		// terraform accepts it. Not one naming another resource (a
		// gateway_id): none set means there is none, and a placeholder
		// would invent the link.
		if what, ok := placeholder(body, schema, slices.DeleteFunc(slices.Clone(missing), namesResource), false); ok {
			return what, true
		}
		// "expected x to be one of [...], got " is an enum check failing
		// on a zero value: unset was meant.
		return removeEmpty(present)

	case strings.Contains(text, "unconfigurable attribute"):
		// Terraform core: a computed-only attribute (usually inside a
		// nested block) was generated as if it could be set.
		for _, n := range present {
			if n.block == nil {
				return remove(n), true
			}
		}
		// Inside an attribute holding objects (subnet = [{ id = ... }]),
		// which can't be edited in place: drop the whole attribute when the
		// provider computes it if unset.
		for _, p := range pathToken.FindAllString(d.Detail, -1) {
			top := strings.Split(p, ".")[0]
			if a := schemaAttr(schema, top); a != nil && a.Optional && a.Computed && body.GetAttribute(top) != nil {
				body.SetAttributeValue(top, nullValue())
				return "null " + top + " (computed if unset)", true
			}
		}

	default:
		// Out-of-range, zero or empty values where unset was meant. A
		// meaningful value that fails validation (a reserved name) means
		// the resource can't be managed; leave it to be dropped.
		return removeEmpty(present)
	}
	return "", false
}

// removeEmpty removes the first empty node: a zero value where unset was
// meant.
func removeEmpty(present []node) (string, bool) {
	for _, n := range present {
		if isEmptyNode(n) {
			return remove(n), true
		}
	}
	return "", false
}

// schemeless matches a URL missing its scheme: a host, optionally with a
// path. Not an identifier with colons (urn:..., a:b:c) or free text.
var schemeless = regexp.MustCompile(`^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+(/\S*)?$`)

// urlError matches validators rejecting a value for not being an absolute
// URL.
var urlError = regexp.MustCompile(`(?i)to have a host|to have a (url )?scheme|valid (absolute )?url|absolute url|missing protocol scheme`)

// backticked finds the attribute list in "one of `a,b,c` must be specified".
var backticked = regexp.MustCompile("`([a-z0-9_.,]+)`")

// listOrder puts the paths a "one of `a,b,c`" message lists first, in the
// list's order: which attribute a provider names before the list varies
// between runs, the list doesn't.
func listOrder(paths []string, text string) []string {
	m := backticked.FindStringSubmatch(text)
	if m == nil {
		return paths
	}
	rank := map[string]int{}
	for i, p := range strings.Split(m[1], ",") {
		rank[p] = i + 1
	}
	last := len(rank) + 1
	out := slices.Clone(paths)
	slices.SortStableFunc(out, func(a, b string) int {
		ra, rb := rank[a], rank[b]
		if ra == 0 {
			ra = last
		}
		if rb == 0 {
			rb = last
		}
		return ra - rb
	})
	return out
}

// literalString returns an attribute's value if it is a plain string.
func literalString(a *hclwrite.Attribute) (string, bool) {
	if a == nil {
		return "", false
	}
	expr, diags := hclsyntax.ParseExpression(a.Expr().BuildTokens(nil).Bytes(), "", hcl.InitialPos)
	if diags.HasErrors() {
		return "", false
	}
	v, diags := expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || !v.IsKnown() || v.Type() != cty.String {
		return "", false
	}
	return v.AsString(), true
}

// placeholder sets the first missing top-level string attribute (settable,
// and sensitive if onlySensitive) to a placeholder and ignores changes to
// it, so the resource survives with a value that's never applied. Whether
// terraform accepts it is for the next plan to say; a repeat is dropped.
func placeholder(body *hclwrite.Body, schema *tf.SchemaBlock, missing []string, onlySensitive bool) (string, bool) {
	if schema == nil {
		return "", false
	}
	for _, p := range missing {
		a := schema.Attributes[p]
		if a == nil || strings.Contains(p, ".") || strings.HasSuffix(p, "_wo") || string(a.Type) != `"string"` || !(a.Optional || a.Required) {
			continue
		}
		if onlySensitive != a.Sensitive {
			continue
		}
		v := "placeholder: value not returned by the API"
		if a.Sensitive {
			v = hclx.SensitivePlaceholder
		}
		body.SetAttributeValue(p, cty.StringVal(v))
		hclx.IgnoreChanges(body, p)
		return "placeholder for " + p, true
	}
	return "", false
}

// namesOwned reports whether an attribute holds a value owned accepts.
func namesOwned(n node, owned func(string) bool) bool {
	if n.block != nil || owned == nil {
		return false
	}
	expr, diags := hclsyntax.ParseExpression(n.parent.GetAttribute(n.name).Expr().BuildTokens(nil).Bytes(), "", hcl.InitialPos)
	if diags.HasErrors() {
		return false
	}
	v, diags := expr.Value(nil)
	if diags.HasErrors() {
		return false
	}
	found := false
	eachString(v, func(s string) { found = found || owned(s) })
	return found
}

// namesResource reports whether an attribute's name says it points at
// another resource (gateway_id, role_arn, subnet_ids).
func namesResource(attr string) bool {
	for _, suffix := range []string{"_id", "_ids", "_arn", "_arns", "_name", "_names"} {
		if strings.HasSuffix(attr, suffix) {
			return true
		}
	}
	return false
}

func isNullNode(n node) bool {
	return n.block == nil && isNull(n.parent.GetAttribute(n.name))
}

var emptyValues = map[string]bool{`[]`: true, `{}`: true, `""`: true, `0`: true, `false`: true}

// isEmptyNode reports whether an attribute or block carries no value.
func isEmptyNode(n node) bool {
	if n.block != nil {
		return len(n.block.Body().Attributes()) == 0 && len(n.block.Body().Blocks()) == 0
	}
	v := strings.Join(strings.Fields(string(n.parent.GetAttribute(n.name).Expr().BuildTokens(nil).Bytes())), "")
	return emptyValues[v]
}

func remove(n node) string {
	if n.block != nil {
		n.parent.RemoveBlock(n.block)
		return "remove block " + n.path
	}
	n.parent.SetAttributeValue(n.name, nullValue())
	return "null " + n.path
}

func schemaAttr(schema *tf.SchemaBlock, name string) *tf.SchemaAttr {
	if schema == nil {
		return nil
	}
	return schema.Attributes[name]
}
