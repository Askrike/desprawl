package tfmap

import (
	"slices"
	"strings"
)

type ArchNode struct {
	ID       string   `json:"id"`
	Module   string   `json:"module,omitempty"`
	Label    string   `json:"label"`
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Members  []string `json:"members"`
}

type Via struct {
	Node string `json:"node"`
	Attr string `json:"attr"`
	Ref  string `json:"ref"`
}

type ArchEdge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Label    string `json:"label"`
	Kind     string `json:"kind"` // reference or connector
	Directed bool   `json:"directed"`
	Both     bool   `json:"both,omitempty"` // references run both ways; drawn with two arrowheads
	Via      []Via  `json:"via"`
	Route    *Route `json:"route,omitempty"` // set when a saved grid layout places both ends
}

type Arch struct {
	Nodes []ArchNode `json:"nodes"`
	Edges []ArchEdge `json:"edges"`
}

// Abstract classifies every node and collapses the reference graph into an
// architecture view. It fills Role/Category/Classified on g.Nodes.
func Abstract(g *Graph, c *Catalog) Arch {
	a := newAbstractor(g, c)
	for _, n := range g.Nodes {
		a.resolve(n.ID)
	}
	return Arch{Nodes: a.components(g), Edges: a.connections(g)}
}

// abstractor is one abstraction in progress: the classified reference graph
// and what each node folds into, decided as resolve reaches it.
type abstractor struct {
	byID    map[string]*Node
	rules   map[string]Rule
	out, in map[string][]Edge
	// names: a variable or local that is exactly a component's name stands
	// for that component, so an IAM policy's "arn:…:table/${var.table}" or a
	// function's TABLE = var.table reaches the table even without a direct
	// reference to the resource.
	names map[string][]string
	// transparent marks shared satellites: they aren't components, so
	// references pass through them.
	transparent, ignored map[string]bool
	rep                  map[string]string // node -> architecture node it collapses into ("" = hidden)
	visiting             map[string]bool
	cut                  int // cycles cut short so far
}

func newAbstractor(g *Graph, c *Catalog) *abstractor {
	a := &abstractor{
		byID: map[string]*Node{}, rules: map[string]Rule{}, out: map[string][]Edge{}, in: map[string][]Edge{},
		names: map[string][]string{}, transparent: map[string]bool{}, ignored: map[string]bool{},
		rep: map[string]string{}, visiting: map[string]bool{},
	}
	types := map[string]bool{}
	for i := range g.Nodes {
		a.byID[g.Nodes[i].ID] = &g.Nodes[i]
		if g.Nodes[i].Kind == "resource" {
			types[g.Nodes[i].Type] = true
		}
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		r, ok := c.classify(*n, types)
		a.rules[n.ID] = r
		n.Role, n.Category, n.Classified = r.Role, r.Category, ok
	}
	for _, e := range g.Edges {
		a.out[e.From] = append(a.out[e.From], e)
		a.in[e.To] = append(a.in[e.To], e)
		k := a.byID[e.To].Kind
		if e.Whole && nameAttrs[e.Attr] && a.rules[e.From].Role == "primary" && (k == "variable" || k == "local") {
			a.names[e.To] = append(a.names[e.To], e.From)
		}
	}
	return a
}

func (a *abstractor) named(id string) []string {
	if len(a.names[id]) == 1 { // a name shared by several components identifies none
		return a.names[id]
	}
	return nil
}

// passThrough reports nodes that only carry values along: locals, module
// inputs and outputs, and helpers.
func (a *abstractor) passThrough(id string) bool {
	k := a.byID[id].Kind
	return k == "local" || k == "variable" || k == "output" || a.rules[id].Role == "helper"
}

// targets follows an edge through pass-through nodes and transparent
// satellites to the real nodes behind it, so a service using a shared role
// reaches whatever that role's policies grant. byName applies the names
// substitution; it's off for attributes that name or describe their own
// resource ("${var.name}-jobs", tags), which share a component's name
// without referring to it.
func (a *abstractor) targets(id string, seen map[string]bool, byName bool) []string {
	if !a.passThrough(id) && !a.transparent[id] {
		return []string{id}
	}
	if seen[id] {
		return nil
	}
	seen[id] = true
	var ts []string
	if byName {
		ts = a.named(id)
	}
	for _, e := range a.out[id] {
		ts = append(ts, a.targets(e.To, seen, byName)...)
	}
	if a.transparent[id] {
		// A shared role's effects include its inline policies (children)
		// and the policies attached to it.
		for _, e := range a.in[id] {
			f, r := e.From, a.rules[e.From]
			switch {
			case r.Role == "child" && !seen[f]:
				seen[f] = true
				for _, ce := range a.out[f] {
					if ce.To != id {
						ts = append(ts, a.targets(ce.To, seen, byName)...)
					}
				}
			case r.Role == "connector" && r.Attaches:
				for _, ce := range a.out[f] {
					if ce.Attr == r.FromAttr || strings.HasPrefix(ce.Attr, r.FromAttr+".") {
						ts = append(ts, a.targets(ce.To, seen, byName)...)
					}
				}
			}
		}
	}
	return ts
}

// direct is targets without passing through shared wiring: structural
// questions (what is my parent, what are a connector's ends) must stop at a
// shared role rather than land on whatever its policies grant.
func (a *abstractor) direct(id string, seen map[string]bool) []string {
	if !a.passThrough(id) {
		return []string{id}
	}
	if seen[id] {
		return nil
	}
	seen[id] = true
	ts := a.named(id)
	for _, e := range a.out[id] {
		ts = append(ts, a.direct(e.To, seen)...)
	}
	return ts
}

// refsVia is what id's attribute attr (any, if "") directly refers to.
func (a *abstractor) refsVia(id, attr string) []string {
	var ts []string
	for _, e := range a.out[id] {
		if attr == "" || e.Attr == attr || strings.HasPrefix(e.Attr, attr+".") {
			ts = append(ts, a.direct(e.To, map[string]bool{})...)
		}
	}
	return ts
}

func (a *abstractor) parentCandidates(id string) []string {
	r := a.rules[id]
	if t, ok := strings.CutPrefix(r.ParentAttr, "type:"); ok {
		var cs []string
		for _, c := range a.refsVia(id, "") {
			if a.byID[c].Type == t {
				cs = append(cs, c)
			}
		}
		return cs
	}
	if r.ParentAttr != "" {
		if cs := a.refsVia(id, r.ParentAttr); len(cs) > 0 {
			return cs
		}
	}
	return a.refsVia(id, "")
}

// usersOf finds what really uses a satellite, looking past the things in
// between: pass-through nodes (a role ARN exported from one module and used
// by a service in another), its own children (an instance references the
// role's instance profile, not the role), and attachment connectors (a
// policy is used by the role it is attached to). Other connectors and
// children depend on it rather than use it, so they don't count.
func (a *abstractor) usersOf(sat, id string, seen map[string]bool) []string {
	var users []string
	for _, e := range a.in[id] {
		f := e.From
		if seen[f] || a.ignored[f] {
			continue
		}
		seen[f] = true
		r := a.rules[f]
		switch {
		case a.passThrough(f):
			users = append(users, a.usersOf(sat, f, seen)...)
		case r.Role == "child":
			if slices.Contains(a.parentCandidates(f), sat) {
				users = append(users, a.usersOf(sat, f, seen)...)
			} else {
				users = append(users, f)
			}
		case r.Role == "connector":
			if r.Attaches && slices.Contains(a.refsVia(f, r.FromAttr), sat) {
				users = append(users, a.refsVia(f, r.ToAttr)...)
			}
		default:
			users = append(users, f)
		}
	}
	return users
}

// satelliteUsers resolves a satellite's users to components; a user that is
// itself shared plumbing (a policy's role used by four services) stands for
// its own users.
func (a *abstractor) satelliteUsers(id string) []string {
	var users []string
	for _, u := range a.usersOf(id, id, map[string]bool{}) {
		if r := a.resolve(u); r != "" {
			users = append(users, r)
		} else if a.transparent[u] {
			users = append(users, a.satelliteUsers(u)...)
		}
	}
	slices.Sort(users)
	return slices.Compact(users)
}

// fallbackRefs is what an unused satellite points at, including through its
// children (a DNS zone's records alias the load balancer).
func (a *abstractor) fallbackRefs(id string) []string {
	ts := a.refsVia(id, "")
	for _, e := range a.in[id] {
		if a.rules[e.From].Role == "child" && slices.Contains(a.parentCandidates(e.From), id) {
			for _, t := range a.refsVia(e.From, "") {
				if t != id {
					ts = append(ts, t)
				}
			}
		}
	}
	return ts
}

// connectorEnds is what a connector's attribute reaches, drawn. An end at
// shared plumbing stands for its users: a role granted to a managed
// identity two components use is granted to both.
func (a *abstractor) connectorEnds(id, attr string) []string {
	var ids []string
	for _, t := range a.refsVia(id, attr) {
		if a.resolve(t) == "" && a.transparent[t] {
			ids = append(ids, a.satelliteUsers(t)...)
		} else {
			ids = append(ids, t)
		}
	}
	return a.uniqueReps(ids)
}

func (a *abstractor) uniqueReps(ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if r := a.resolve(id); r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// resolve decides what a node folds into: itself if it's a component,
// its parent or user if it's wiring, or nothing (hidden).
func (a *abstractor) resolve(id string) string {
	if r, ok := a.rep[id]; ok {
		return r
	}
	if a.visiting[id] {
		a.cut++
		return ""
	}
	// An answer worked out while a cycle was cut short may be wrong (a
	// role's inline policy resolved while the role itself is still being
	// decided), so only top-level or cycle-free answers are kept.
	top, cutBefore := len(a.visiting) == 0, a.cut
	a.visiting[id] = true
	defer delete(a.visiting, id)

	r := a.rules[id]
	result := ""
	switch r.Role {
	case "primary":
		result = id
	case "ignore":
		a.ignored[id] = true
	case "child":
		for _, t := range a.parentCandidates(id) {
			if p := a.resolve(t); p != "" {
				result = p
				break
			}
			if a.ignored[t] {
				a.ignored[id] = true
				break
			}
			if a.transparent[t] {
				a.transparent[id] = true // part of shared wiring (a shared role's inline policy)
				break
			}
		}
		if result != "" || a.ignored[id] || a.transparent[id] {
			break
		}
		// No component parent (a DNS record in a zone that's only looked
		// up): treat it as plumbing.
		fallthrough
	case "satellite":
		// Plumbing is never a component: it folds into its one user, or the
		// one compute user among several; otherwise it's shared and becomes
		// see-through.
		users := a.satelliteUsers(id)
		if len(users) == 0 {
			users = a.uniqueReps(a.fallbackRefs(id))
		}
		var compute []string
		for _, u := range users {
			if isCompute(a.rules[u]) {
				compute = append(compute, u)
			}
		}
		switch {
		case len(users) == 1:
			result = users[0]
		case len(compute) == 1:
			result = compute[0]
		default:
			a.transparent[id] = true
		}
	case "connector":
		// A connector that only reaches one primary (e.g. a policy
		// attachment to a hardcoded AWS-managed ARN) is just config of it.
		if ends := a.connectorEnds(id, ""); len(ends) == 1 {
			result = ends[0]
		}
	}
	if top || a.cut == cutBefore {
		a.rep[id] = result
	} else {
		delete(a.transparent, id)
		delete(a.ignored, id)
	}
	return result
}

// components are the nodes that resolve to themselves, with what folds in.
func (a *abstractor) components(g *Graph) []ArchNode {
	nodes := []ArchNode{} // [] rather than null in JSON
	members := map[string][]string{}
	for _, n := range g.Nodes {
		if r := a.rep[n.ID]; r != "" {
			members[r] = append(members[r], n.ID)
		}
	}
	for _, n := range g.Nodes {
		if a.rep[n.ID] != n.ID {
			continue
		}
		r := a.rules[n.ID]
		label := r.Label
		if label == "" {
			label = n.Type
		}
		nodes = append(nodes, ArchNode{
			ID: n.ID, Module: n.Module, Label: label, Name: n.Name, Category: r.Category, Members: members[n.ID],
		})
	}
	return nodes
}

// throughIdentity reports whether the pass-through path from start to
// target crosses shared IAM (a service's role ARN arriving via a module
// input, then the role's policies).
func (a *abstractor) throughIdentity(start, target string) bool {
	var walk func(id string, seen map[string]bool) bool
	walk = func(id string, seen map[string]bool) bool {
		if seen[id] {
			return false
		}
		seen[id] = true
		if a.transparent[id] && a.rules[id].Category == "identity" {
			return slices.Contains(a.targets(id, map[string]bool{}, true), target)
		}
		if !a.passThrough(id) {
			return false
		}
		for _, e := range a.out[id] {
			if walk(e.To, seen) {
				return true
			}
		}
		return false
	}
	return walk(start, map[string]bool{})
}

// meaning of a plain reference: IAM resources grant access, security groups
// open network paths, wiring with a verb (a listener rule routes traffic)
// says so; everything else is a plain dependency.
func (a *abstractor) meaning(viaNode string) string {
	r := a.rules[viaNode]
	switch {
	case r.Verb != "" && r.Role != "connector":
		return r.Verb
	case r.Category == "identity":
		return "access"
	case strings.Contains(a.byID[viaNode].Type, "security_group"):
		return "network"
	}
	return "uses"
}

// connections are the arrows between components: every reference that
// crosses from one component to another, and every connector.
func (a *abstractor) connections(g *Graph) []ArchEdge {
	var es edgeSet
	for _, e := range g.Edges {
		if e.Attr == "depends_on" {
			continue // creation order, not a relationship
		}
		if a.rules[e.From].Role == "connector" && a.rep[e.From] == "" {
			continue // handled below as a single edge
		}
		from := a.rep[e.From]
		m := a.meaning(e.From)
		v := Via{Node: e.From, Attr: e.Attr, Ref: e.Ref}
		if r := a.rules[e.From]; r.Inverse || slices.ContainsFunc(r.InverseAttrs, func(at string) bool { return e.Attr == at || strings.HasPrefix(e.Attr, at+".") }) {
			// A queue/topic/bucket policy names who may call its parent, an
			// ingress rule who may connect: the edge runs from them.
			if r.Inverse {
				m = "access"
			}
			for _, t := range a.targets(e.To, map[string]bool{}, true) {
				es.add(a.rep[t], from, "reference", m, true, v)
			}
			continue
		}
		for _, t := range a.targets(e.To, map[string]bool{}, !selfNaming(e.Attr)) {
			mt := m
			if mt == "uses" && a.throughIdentity(e.To, t) {
				mt = "access" // granted by a shared role or policy on the way
			}
			es.add(from, a.rep[t], "reference", mt, true, v)
		}
	}
	for _, n := range g.Nodes {
		r := a.rules[n.ID]
		if r.Role != "connector" || a.rep[n.ID] != "" {
			continue
		}
		// An attachment (policy to role) usually means nothing on its own:
		// its effect is in how the policy and role fold into their users.
		// But a policy attached to several roles is shared, so it folds into
		// none: its grants reach whoever each attachment's role stands for.
		if r.Attaches {
			for _, f := range a.refsVia(n.ID, r.FromAttr) {
				if !a.transparent[f] {
					continue
				}
				for _, to := range a.connectorEnds(n.ID, r.ToAttr) {
					for _, t := range a.targets(f, map[string]bool{}, true) {
						es.add(to, a.rep[t], "reference", a.meaning(f), true, Via{Node: n.ID, Attr: r.FromAttr})
					}
				}
			}
			continue
		}
		// Grants read as access and firewall rules as network, like plain
		// references; other connectors keep their verb ("routes to").
		verb := r.Verb
		switch {
		case r.Category == "identity":
			verb = "access"
		case r.Category == "security":
			verb = "network"
		case verb == "":
			verb = "uses"
		}
		v := Via{Node: n.ID}
		if r.FromAttr != "" && r.ToAttr != "" {
			for _, from := range a.connectorEnds(n.ID, r.FromAttr) {
				for _, to := range a.connectorEnds(n.ID, r.ToAttr) {
					es.add(from, to, "connector", verb, true, v)
				}
			}
			continue
		}
		// Direction unknown: link the first endpoint to the others.
		ends := a.connectorEnds(n.ID, "")
		if len(ends) < 2 {
			continue // connects nothing that is drawn
		}
		for _, to := range ends[1:] {
			es.add(ends[0], to, "connector", verb, false, v)
		}
	}
	return es.merged()
}

// edgeSet collects one connection per pair of components, with every
// meaning found for it: access (IAM), network (security groups), routes,
// uses…
type edgeSet struct {
	edges    map[string]*ArchEdge
	meanings map[string][]string
	order    []string
}

func (s *edgeSet) add(from, to, kind, meaning string, directed bool, v Via) {
	if from == "" || to == "" || from == to {
		return
	}
	if s.edges == nil {
		s.edges, s.meanings = map[string]*ArchEdge{}, map[string][]string{}
	}
	key := from + "|" + to
	e, ok := s.edges[key]
	if !ok {
		e = &ArchEdge{From: from, To: to, Kind: kind, Directed: directed}
		s.edges[key] = e
		s.order = append(s.order, key)
	}
	if kind == "connector" {
		e.Kind, e.Directed = kind, e.Directed || directed
	}
	if !slices.Contains(s.meanings[key], meaning) {
		s.meanings[key] = append(s.meanings[key], meaning)
	}
	e.Via = append(e.Via, v)
}

// merged returns the connections, labelled, with A→B and B→A of the same
// meaning (network both ways) as one line with arrows at both ends;
// different meanings stay separate arrows.
func (s *edgeSet) merged() []ArchEdge {
	for _, k := range s.order {
		e := s.edges[k]
		if e == nil {
			continue // already merged into its reverse
		}
		back := e.To + "|" + e.From
		sameMeaning := func() bool {
			x, y := slices.Clone(s.meanings[k]), slices.Clone(s.meanings[back])
			slices.Sort(x)
			slices.Sort(y)
			return slices.Equal(x, y)
		}
		if r, ok := s.edges[back]; ok && r != nil && slices.Index(s.order, back) > slices.Index(s.order, k) && sameMeaning() {
			e.Both = true
			e.Via = append(e.Via, r.Via...)
			for _, m := range s.meanings[back] {
				if !slices.Contains(s.meanings[k], m) {
					s.meanings[k] = append(s.meanings[k], m)
				}
			}
			if r.Kind == "connector" {
				e.Kind = "connector"
			}
			s.edges[back] = nil
		}
	}
	out := []ArchEdge{} // [] rather than null in JSON
	for _, k := range s.order {
		e := s.edges[k]
		if e == nil {
			continue
		}
		// The specific verb first ("routes to · access"), then the generic
		// meanings; "uses" only when there is nothing better.
		ms := slices.Clone(s.meanings[k])
		if len(ms) > 1 {
			ms = slices.DeleteFunc(ms, func(m string) bool { return m == "uses" })
		}
		generic := func(m string) bool { return m == "access" || m == "network" }
		slices.SortStableFunc(ms, func(x, y string) int {
			switch {
			case generic(x) == generic(y):
				return 0
			case generic(x):
				return 1
			}
			return -1
		})
		e.Label = strings.Join(ms, " · ")
		out = append(out, *e)
	}
	return out
}
