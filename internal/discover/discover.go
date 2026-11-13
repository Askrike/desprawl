// Package discover finds live resources through the provider itself, using
// `terraform query` list resources. It knows nothing about any particular
// cloud: what can be listed comes from the provider schema, and how to fill
// the arguments of list resources that need them (an ECS service needs its
// cluster, a DNS record its zone) comes from ListArg specs in the provider
// pack.
//
// Discovery runs in rounds. Types whose list resource needs no arguments go
// first; a type that needs arguments runs once its parent type has been
// listed, with one list block per parent instance.
package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/askrike/desprawl/internal/logs"
	"github.com/askrike/desprawl/internal/prof"
	"github.com/hashicorp/hcl/v2"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// ListArg says how to fill the required arguments of a list resource.
type ListArg struct {
	Type string `json:"type"`
	// Parent is the resource type whose instances supply arguments, and
	// Args maps each list argument to the parent attribute holding its value.
	Parent string            `json:"parent,omitempty"`
	Args   map[string]string `json:"args,omitempty"`
	// Values lists fixed values for enumeration-like arguments; every
	// combination is listed.
	Values map[string][]string `json:"values,omitempty"`
	// Scope maps arguments to scope parameters (project, region, ...) whose
	// values come from the identities discovered so far.
	Scope  map[string]string `json:"scope,omitempty"`
	Origin string            `json:"origin,omitempty"`
	Note   string            `json:"note,omitempty"`
}

type Options struct {
	Types       map[string]bool // only these types; empty means all
	Exclude     map[string]bool // never these types
	Parallel    int
	Timeout     time.Duration // how long a round waits for a list type (which may run 3x longer)
	Limit       int           // results per list block
	AllVersions bool          // keep every revision of versioned resources
	// ProviderAttrs are set on the provider blocks of the query directories
	// where providers.tf doesn't set them (e.g. fewer retries).
	ProviderAttrs map[string]any
	// Provider is the provider block to list with, "aws" or "aws.us"; empty
	// means each type's provider, unaliased.
	Provider string
}

// Report records what discovery could and couldn't list. It is the
// starting point for filling gaps by hand or with an agent.
type Report struct {
	Listed    map[string]int    `json:"listed"`     // type -> resources found
	Failed    map[string]string `json:"failed"`     // type -> error
	NeedsArgs []string          `json:"needs_args"` // list types needing arguments with no ListArg spec
	Collapsed map[string]int    `json:"collapsed"`  // type -> older versions dropped
	// Superseded holds the older versions dropped (unnamed targets), so
	// that generate can bring back one another resource still uses.
	Superseded map[string][]model.Target `json:"superseded,omitempty"`
	Truncated  []string                  `json:"truncated"`      // types that hit the result limit: some resources are missing
	Slow       []string                  `json:"slow,omitempty"` // types still listing after the timeout
	Unlistable []string                  `json:"unlistable"`     // resource types the provider can't list
	Inventory  map[string]int            `json:"inventory"`      // type -> resources found by inventory data sources instead
	// Held holds the inventory targets of excluded types (unnamed), so
	// that generate can bring back those an exported resource names (a
	// route through a network interface the pack leaves out as
	// service-owned).
	Held map[string][]model.Target `json:"held,omitempty"`
	// BadParams holds the scope parameters whose data source in the pack
	// doesn't return the discovered value, with why; link keeps them
	// literal.
	BadParams map[string]string `json:"bad_params,omitempty"`
}

type Discoverer struct {
	WS      model.Workspace
	TF      tf.Runner
	Schemas *tf.Schemas
	Specs   []ListArg
	// Added are targets added by hand. They supply parent instances for
	// types the provider can't list (a DNS zone whose records can be).
	Added []model.Target
	Opts  Options
	Log   logs.Logger
}

func (d Discoverer) Run(ctx context.Context) ([]model.Target, *Report, error) {
	rep := &Report{Listed: map[string]int{}, Failed: map[string]string{}, Collapsed: map[string]int{}, Inventory: map[string]int{}}
	specs := map[string]ListArg{}
	for _, s := range d.Specs {
		specs[s.Type] = s
	}

	pending := map[string]bool{}
	for typ := range d.Schemas.Lists {
		if d.wants(typ) {
			pending[typ] = true
		}
	}
	for typ := range d.Schemas.Resources {
		if d.Schemas.Lists[typ] == nil && d.wants(typ) {
			rep.Unlistable = append(rep.Unlistable, typ)
		}
	}
	sort.Strings(rep.Unlistable)

	// Types are listed as soon as they are ready: a child as soon as its
	// parent has been listed, not when every query started with the
	// parent has finished. A type still listing after the timeout keeps
	// running in the background (up to slowTimeout) while the rest go
	// ahead; only its children wait for it, since results[typ] is set when
	// it finishes.
	results := map[string][]tf.ListResult{}
	var mu sync.Mutex
	sem := make(chan struct{}, max(d.Opts.Parallel, 1))
	// Jobs signal wake when they finish or outlive the timeout. What they
	// change is read under mu, so signals may merge.
	wake := make(chan struct{}, 1)
	signal := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	running := 0               // jobs not finished
	alone := map[string]bool{} // types a shared query couldn't settle
	for round := 0; ; {
		mu.Lock()
		params := scopeParams(results)
		var ready []string
		for typ := range pending {
			if d.ready(typ, specs, results, pending, params) {
				ready = append(ready, typ)
			}
		}
		if len(ready) == 0 {
			if running == 0 {
				mu.Unlock()
				break
			}
			mu.Unlock()
			<-wake
			continue
		}
		round++
		sort.Strings(ready)
		d.TF.Progress.Step(fmt.Sprintf("round %d: listing %d types", round, len(ready)), 0)
		d.Log.Verbose("round %d: listing %d types", round, len(ready))

		// Build every block before querying: queries write results, which
		// blocks read.
		blocks := map[string][]listBlock{}
		have := d.instances(results)
		for _, typ := range ready {
			delete(pending, typ)
			if parent := specs[typ].Parent; parent != "" && rep.Failed[parent] != "" && len(have[parent]) == 0 {
				// A parent that failed to list isn't known to be empty:
				// listing per another type would guess at what it holds.
				rep.Failed[typ] = "parent type " + parent + " failed to list"
				results[typ] = nil
				continue
			}
			b, err := d.blocks(typ, specs[typ], results, params)
			if err != nil {
				rep.Failed[typ] = err.Error()
				results[typ] = nil // children see it as listed, with nothing found
				continue
			}
			blocks[typ] = b
		}

		jobs := batches(slices.Sorted(maps.Keys(blocks)), blocks, alone, d.Opts.Parallel-running)
		running += len(jobs)
		mu.Unlock()

		for _, job := range jobs {
			sem <- struct{}{}
			if len(job) > 1 {
				go func() {
					defer func() { <-sem; signal() }()
					n := 0
					for _, typ := range job {
						n += len(blocks[typ])
					}
					end := prof.Span("list", "type", strings.Join(job, ","), "blocks", strconv.Itoa(n))
					found, failed, err := d.listBatch(ctx, job, blocks, d.Opts.Timeout)
					end(prof.Err(err)...)
					mu.Lock()
					defer mu.Unlock()
					running--
					if err != nil {
						d.Log.Debug("listing %d types together failed, listing them one by one (%v)", len(job), err)
					}
					for _, typ := range job {
						switch {
						case err != nil || truncated(found[typ], d.Opts.Limit):
							// Listed alone, it gets its own timeout
							// (and slow report) or a larger limit.
							pending[typ] = true
							alone[typ] = true
						case failed[typ] != "":
							results[typ] = nil
							rep.Failed[typ] = failed[typ]
						default:
							results[typ] = found[typ]
							rep.Listed[typ] = len(found[typ])
						}
					}
				}()
				continue
			}
			typ := job[0]
			bs := blocks[typ]
			go func() {
				defer func() { <-sem; signal() }()
				finished := false
				timer := time.AfterFunc(d.Opts.Timeout, func() {
					mu.Lock()
					defer mu.Unlock()
					if !finished {
						rep.Slow = append(rep.Slow, typ)
						d.Log.Verbose("%s: still listing after %s, going on without it (giving it up to %s)", typ, d.Opts.Timeout, d.slowTimeout())
					}
				})
				defer timer.Stop()
				var found []tf.ListResult
				var trunc bool
				var err error
				if len(bs) > 0 {
					end := prof.Span("list", "type", typ, "blocks", strconv.Itoa(len(bs)))
					found, trunc, err = d.list(ctx, typ, bs)
					end(append([]string{"found", strconv.Itoa(len(found))}, prof.Err(err)...)...)
				}
				mu.Lock()
				defer mu.Unlock()
				finished = true
				running--
				if err != nil {
					// Partial results (e.g. on timeout) would silently
					// under-report; the type is failed and reported.
					results[typ] = nil
					rep.Failed[typ] = err.Error()
					return
				}
				results[typ] = found
				rep.Listed[typ] = len(found)
				if trunc {
					rep.Truncated = append(rep.Truncated, typ)
				}
				if s := specs[typ]; len(bs) == 0 && s.Parent != "" && rep.Failed[s.Parent] != "" {
					rep.Failed[typ] = "parent type " + s.Parent + " failed to list"
				}
			}()
		}
	}
	for typ := range pending {
		if s, ok := specs[typ]; ok {
			if s.Parent != "" {
				rep.Failed[typ] = "parent type " + s.Parent + " was not listed"
			} else {
				rep.Failed[typ] = fmt.Sprintf("scope parameters %v not found in any discovered identity", s.Scope)
			}
		} else {
			rep.NeedsArgs = append(rep.NeedsArgs, typ)
		}
	}
	sort.Strings(rep.NeedsArgs)
	sort.Strings(rep.Truncated)
	sort.Strings(rep.Slow)
	for typ, n := range rep.Listed {
		if n == 0 {
			delete(rep.Listed, typ)
		}
	}

	var found []tf.ListResult
	for _, typ := range slices.Sorted(maps.Keys(results)) {
		rs := results[typ]
		if !d.Opts.AllVersions {
			var older []tf.ListResult
			rs, older = collapseVersions(rs)
			if len(older) > 0 {
				rep.Collapsed[typ] = len(older)
				if rep.Superseded == nil {
					rep.Superseded = map[string][]model.Target{}
				}
				for _, r := range older {
					rep.Superseded[typ] = append(rep.Superseded[typ], model.Target{Type: r.Type, Identity: r.Identity, Display: r.Display})
				}
			}
		}
		found = append(found, rs...)
	}
	return Name(found), rep, nil
}

// slowTimeout bounds a type's listing. Rounds stop waiting for a type
// after Opts.Timeout, but it keeps listing until this: some types are
// just slow (thousands of revisions), not hung.
func (d Discoverer) slowTimeout() time.Duration { return 3 * d.Opts.Timeout }

// list lists one type. A type that hits the result limit is listed again
// with a larger one: results come back in the provider's order (often by
// name), so a truncated list loses whole groups of resources, not just old
// versions, and collapsing versions can't make up for it. A provider that
// rejects the larger limit keeps the truncated result.
func (d Discoverer) list(ctx context.Context, typ string, blocks []listBlock) ([]tf.ListResult, bool, error) {
	found, err := d.query(ctx, typ, blocks)
	if err != nil {
		return nil, false, err
	}
	limit := d.Opts.Limit
	for range maxLimitGrowth {
		if !truncated(found, limit) {
			break
		}
		limit *= 10
		d.Log.Verbose("%s: hit the result limit, listing again with limit %d", typ, limit)
		more, err := d.query(context.WithValue(ctx, limitKey{}, limit), typ, blocks)
		if err != nil {
			d.Log.Debug("%s: listing with limit %d failed, keeping the truncated result: %v", typ, limit, err)
			return found, true, nil
		}
		found = more
	}
	return found, truncated(found, limit), nil
}

// truncated reports whether any list block may have stopped at the limit.
// Without block addresses, any total reaching the limit counts.
func truncated(found []tf.ListResult, limit int) bool {
	per := map[string]int{}
	for _, r := range found {
		per[r.Block]++
		if per[r.Block] >= limit {
			return true
		}
	}
	return false
}

// maxLimitGrowth bounds how often a truncated type is listed again with a
// ten times larger limit.
const maxLimitGrowth = 2

// limitKey overrides the result limit for a retry.
type limitKey struct{}

func (d Discoverer) wants(typ string) bool {
	if d.Opts.Exclude[typ] {
		return false
	}
	return len(d.Opts.Types) == 0 || d.Opts.Types[typ]
}

func (d Discoverer) identityHas(typ, attr string) bool {
	is := d.Schemas.Identities[typ]
	if is == nil {
		return false
	}
	_, ok := is.Attributes[attr]
	return ok
}

// ready reports whether a type can be listed now: it needs no arguments,
// or its parent has been listed (and isn't itself still pending) and its
// scope parameters are known.
func (d Discoverer) ready(typ string, specs map[string]ListArg, results map[string][]tf.ListResult, pending map[string]bool, params map[string]string) bool {
	s, ok := specs[typ]
	if len(d.Schemas.RequiredListArgs(typ)) == 0 && !ok {
		return true
	}
	if !ok {
		return false
	}
	for _, p := range s.Scope {
		if params[p] == "" {
			return false
		}
	}
	if s.Parent == "" || d.Schemas.Lists[s.Parent] == nil || !d.wants(s.Parent) {
		return true // parents, if any, come from added targets
	}
	_, listed := results[s.Parent]
	return listed && !pending[s.Parent]
}

// scopeParams detects scope parameters from everything listed so far.
func scopeParams(results map[string][]tf.ListResult) map[string]string {
	var all []tf.ListResult
	for _, rs := range results {
		all = append(all, rs...)
	}
	return Params(Name(all))
}

// listBlock is one list block: its config arguments, if any.
type listBlock map[string]string

func (d Discoverer) blocks(typ string, s ListArg, results map[string][]tf.ListResult, params map[string]string) ([]listBlock, error) {
	if len(d.Schemas.RequiredListArgs(typ)) == 0 && s.Type == "" {
		return []listBlock{{}}, nil
	}
	scope := listBlock{}
	for arg, p := range s.Scope {
		scope[arg] = params[p]
	}
	combos := []listBlock{scope}
	if s.Parent != "" {
		combos = nil
		seen := map[string]bool{}
		parents := slices.Concat(results[s.Parent], d.added(s.Parent))
		if len(parents) == 0 && (d.Schemas.Lists[s.Parent] == nil || !d.wants(s.Parent)) {
			return nil, fmt.Errorf("parent type %s can't be listed; add its instances with add-target", s.Parent)
		}
		// A parent without an argument's attribute is left out (one found
		// by inventory may know only its import ID); the type fails only
		// if no parent has them all.
		var missing error
	parent:
		for _, p := range parents {
			b := listBlock{}
			for k, v := range scope {
				b[k] = v
			}
			for _, arg := range slices.Sorted(maps.Keys(s.Args)) {
				v, ok := value(p, s.Args[arg])
				if !ok {
					missing = fmt.Errorf("parent %s has no attribute %q", s.Parent, s.Args[arg])
					continue parent
				}
				b[arg] = v
			}
			if k := fmt.Sprint(b); !seen[k] {
				seen[k] = true
				combos = append(combos, b)
			}
		}
		if missing != nil {
			if len(combos) == 0 {
				return nil, missing
			}
			d.Log.Debug("%s: some parents left out: %v", typ, missing)
		}
	}
	for _, arg := range slices.Sorted(maps.Keys(s.Values)) {
		var next []listBlock
		for _, b := range combos {
			for _, v := range s.Values[arg] {
				nb := listBlock{arg: v}
				for k, x := range b {
					nb[k] = x
				}
				next = append(next, nb)
			}
		}
		combos = next
	}
	return combos, nil
}

// Fill adds targets found by other means (inventory data sources) for
// types discovery couldn't list, and those a list missed, then lists the
// children of the ones that are parents.
func (d Discoverer) Fill(ctx context.Context, found []model.Target, rep *Report, extra []model.Target) ([]model.Target, error) {
	listed := map[string][]model.Target{}
	for _, t := range found {
		listed[t.Type] = append(listed[t.Type], t)
	}
	seen := map[string]int{}
	for _, t := range extra {
		seen[t.Type]++
	}
	var use []model.Target
	for _, t := range extra {
		if d.Opts.Exclude[t.Type] && (len(d.Opts.Types) == 0 || d.Opts.Types[t.Type]) {
			if rep.Held == nil {
				rep.Held = map[string][]model.Target{}
			}
			t.Name = ""
			rep.Held[t.Type] = append(rep.Held[t.Type], t)
			continue
		}
		if !d.wants(t.Type) || (d.Schemas.Lists[t.Type] != nil && rep.Failed[t.Type] == "" && !d.missed(t, listed[t.Type], seen[t.Type])) {
			continue
		}
		use = append(use, t)
		rep.Inventory[t.Type]++
	}
	found = append(found, use...)
	parents := map[string]bool{}
	for _, t := range use {
		parents[t.Type] = true
	}
	children := map[string]bool{}
	for _, s := range d.Specs {
		if parents[s.Parent] && d.wants(s.Type) && d.Schemas.Lists[s.Type] != nil {
			children[s.Type] = true
		}
	}
	if len(children) == 0 {
		return found, nil
	}
	// The children of listed and hand-added parents were listed already:
	// list only the new parents'.
	sub := d
	sub.Added = use
	sub.Opts.Types = children
	more, srep, err := sub.Run(ctx)
	if err != nil {
		return nil, err
	}
	// A child type that failed keeps its failure unless the sub-run lists
	// the children of all its parents: when no parent type listed, all
	// its parents are new. Children of parents a list missed are only some.
	listedBefore := map[string]bool{}
	for typ := range children {
		listedBefore[typ] = rep.Failed[typ] == ""
		if d.allParentsNew(typ, rep) {
			delete(rep.Failed, typ)
		}
	}
	for typ, n := range srep.Listed {
		rep.Listed[typ] += n
	}
	for typ, e := range srep.Failed {
		if listedBefore[typ] {
			// The type lists; only the new parents' children didn't.
			d.Log.Debug("%s: listing the children of parents found by inventory failed: %s", typ, e)
			continue
		}
		rep.Failed[typ] = e
	}
	for typ, n := range srep.Collapsed {
		rep.Collapsed[typ] += n
	}
	for typ, ids := range srep.Superseded {
		if rep.Superseded == nil {
			rep.Superseded = map[string][]model.Target{}
		}
		rep.Superseded[typ] = append(rep.Superseded[typ], ids...)
	}
	rep.Truncated = append(rep.Truncated, srep.Truncated...)
	sort.Strings(rep.Truncated)
	return append(found, more...), nil
}

// allParentsNew reports whether every parent type of typ failed to list or
// can't, so all its parents come from Fill.
func (d Discoverer) allParentsNew(typ string, rep *Report) bool {
	for _, s := range d.Specs {
		if s.Type == typ && s.Parent != "" && d.Schemas.Lists[s.Parent] != nil && rep.Failed[s.Parent] == "" {
			return false
		}
	}
	return true
}

// missed reports whether listing missed an object the inventory found
// (found of the type in all): the inventory found more of the type, and
// the object's ID parts named after identity attributes (Attrs) match no
// listed object's identity. Unless the parts include every attribute the
// identity requires, two objects can't be told apart (services of one
// cluster, by cluster), and listing is trusted.
func (d Discoverer) missed(t model.Target, listed []model.Target, found int) bool {
	is := d.Schemas.Identities[t.Type]
	if found <= len(listed) || is == nil {
		return false
	}
	var keys []string
	for k, a := range is.Attributes {
		if _, ok := t.Attrs[k]; ok {
			keys = append(keys, k)
		} else if a.RequiredForImport {
			return false
		}
	}
	if len(keys) == 0 {
		return false
	}
	for _, l := range listed {
		same := true
		for _, k := range keys {
			if v, ok := value(tf.ListResult{Identity: l.Identity}, k); !ok || v != t.Attrs[k] {
				same = false
				break
			}
		}
		if same {
			return false
		}
	}
	return true
}

// added returns the targets of typ added by hand or by inventory, as list
// results: their identity, their import ID as "id", and the attributes of
// the type their inventory mapping took from the ID.
func (d Discoverer) added(typ string) []tf.ListResult {
	var out []tf.ListResult
	for _, t := range d.Added {
		if t.Type == typ {
			obj := map[string]any{"id": t.ID}
			if s := d.Schemas.Resources[typ]; s != nil {
				for k, v := range t.Attrs {
					if s.Attributes[k] != nil && k != "id" {
						obj[k] = v
					}
				}
			}
			out = append(out, tf.ListResult{Type: t.Type, Display: t.Display, Identity: t.Identity, Object: obj})
		}
	}
	return out
}

func value(r tf.ListResult, attr string) (string, bool) {
	for _, m := range []map[string]any{r.Identity, r.Object} {
		switch v := m[attr].(type) {
		case string:
			return v, v != ""
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64), true
		}
	}
	return "", false
}

// query lists one type in its own directory, so that a failing or hanging
// list resource doesn't affect the others (types that a shared query
// couldn't settle end up here). Blocks that fail on their own
// (one parent without children, one invalid enumeration value) are dropped
// and the rest retried.
func (d Discoverer) query(ctx context.Context, typ string, blocks []listBlock) ([]tf.ListResult, error) {
	found, failed, err := d.listBatch(ctx, []string{typ}, map[string][]listBlock{typ: blocks}, d.slowTimeout())
	if err == nil && failed[typ] != "" {
		err = errors.New(failed[typ])
	}
	return found[typ], err
}

// queryOnce lists types in one directory named name, each type's blocks
// addressed list.<type>.q<i>.
func (d Discoverer) queryOnce(ctx context.Context, name string, types []string, blocks map[string][]listBlock) ([]tf.ListResult, []tf.Diagnostic, map[string]bool, error) {
	dir, err := d.queryDir(name)
	if err != nil {
		return nil, nil, nil, err
	}
	f := hclwrite.NewEmptyFile()
	for _, typ := range types {
		d.writeBlocks(ctx, f, typ, blocks[typ])
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tfquery.hcl"), f.Bytes(), 0o644); err != nil {
		return nil, nil, nil, err
	}
	return d.TF.In(dir).Query(ctx)
}

func (d Discoverer) writeBlocks(ctx context.Context, f *hclwrite.File, typ string, blocks []listBlock) {
	for i, b := range blocks {
		blk := f.Body().AppendNewBlock("list", []string{typ, fmt.Sprintf("q%d", i)})
		body := blk.Body()
		provider := strings.SplitN(typ, "_", 2)[0]
		if d.Opts.Provider != "" {
			provider = d.Opts.Provider
		}
		body.SetAttributeTraversal("provider", traversal(provider))
		limit := d.Opts.Limit
		if l, ok := ctx.Value(limitKey{}).(int); ok {
			limit = l
		}
		body.SetAttributeValue("limit", cty.NumberIntVal(int64(limit)))
		if d.includeResource(typ) {
			body.SetAttributeValue("include_resource", cty.True)
		}
		if len(b) > 0 {
			cfg := body.AppendNewBlock("config", nil).Body()
			for _, k := range slices.Sorted(maps.Keys(b)) {
				cfg.SetAttributeValue(k, cty.StringVal(b[k]))
			}
		}
		f.Body().AppendNewline()
	}
}

// maxBatch bounds how many types share one terraform query, unless that
// would make more queries than free slots.
const maxBatch = 25

// batches groups a round's types into jobs, one terraform query each.
// Every query pays seconds to start terraform and the provider, whatever
// it lists, so types share queries: as many as there are free slots, with
// about as many blocks each. A job beyond the free slots would wait a
// whole query for one, so jobs then hold more than maxBatch types. Types
// with nothing to list and types a shared query couldn't settle go alone.
func batches(order []string, blocks map[string][]listBlock, alone map[string]bool, free int) [][]string {
	var jobs [][]string
	var shared []string
	total := 0
	for _, typ := range order {
		if alone[typ] || len(blocks[typ]) == 0 {
			jobs = append(jobs, []string{typ})
			if len(blocks[typ]) > 0 {
				free--
			}
		} else {
			shared = append(shared, typ)
			total += len(blocks[typ])
		}
	}
	n := free
	if n < 1 {
		// No slot is free: jobs start as slots free up.
		n = max((len(shared)+maxBatch-1)/maxBatch, 1)
	}
	size := max(maxBatch, (len(shared)+n-1)/n)
	var job []string
	sum, k := 0, 1 // blocks so far; shared jobs so far, this one included
	for _, typ := range shared {
		// The k-th job ends at the k-th n-th of the blocks.
		if len(job) > 0 && k < n && (len(job) == size || sum*n >= total*k) {
			jobs = append(jobs, job)
			job = nil
			k++
		}
		job = append(job, typ)
		sum += len(blocks[typ])
	}
	if len(job) > 0 {
		jobs = append(jobs, job)
	}
	return jobs
}

// listBatch lists types in one query, within timeout. Blocks that
// fail are dropped, and a type whose blocks all fail is failed. Blocks
// that completed without a diagnostic keep their results; only the rest
// (cut short by another block's failure) are listed again. An error (a
// timeout, a diagnostic that names no block) means the types must be
// listed alone.
func (d Discoverer) listBatch(ctx context.Context, types []string, blocks map[string][]listBlock, timeout time.Duration) (map[string][]tf.ListResult, map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	blocks = maps.Clone(blocks)
	failed := map[string]string{}
	found := map[string][]tf.ListResult{}
	listed := map[string]bool{}
	some := map[string]bool{} // types with a block whose results are kept
	for {
		var live []string
		for _, typ := range types {
			if failed[typ] == "" && !listed[typ] {
				live = append(live, typ)
			}
		}
		if len(live) == 0 {
			return found, failed, nil
		}
		name := live[0]
		if len(live) > 1 {
			name = fmt.Sprintf("%s+%d", live[0], len(live)-1)
		}
		results, diags, complete, err := d.queryOnce(ctx, name, live, blocks)
		if err != nil {
			return nil, nil, err
		}
		bad := map[string]map[int]bool{}
		first := map[string]tf.Diagnostic{}
		for _, dg := range diags {
			typ, i, ok := blockOf(dg.Address, blocks)
			if !ok || failed[typ] != "" {
				return nil, nil, fmt.Errorf("%s", dg.String())
			}
			if bad[typ] == nil {
				bad[typ] = map[int]bool{}
				first[typ] = dg
			}
			bad[typ][i] = true
		}
		done := map[string]bool{} // blocks whose results are kept
		for _, typ := range live {
			var rest []listBlock
			for i, b := range blocks[typ] {
				addr := fmt.Sprintf("list.%s.q%d", typ, i)
				switch {
				case bad[typ][i]:
				case len(diags) == 0 || complete[addr]:
					done[addr] = true
					some[typ] = true
				default:
					rest = append(rest, b)
				}
			}
			if bad[typ] != nil && len(bad[typ]) < len(blocks[typ]) {
				d.Log.Debug("%s: %d of %d list blocks failed, leaving them out (%s)", typ, len(bad[typ]), len(blocks[typ]), first[typ].Summary)
			}
			switch {
			case len(rest) == 0 && !some[typ]:
				failed[typ] = first[typ].String()
			case len(rest) == 0:
				listed[typ] = true
			default:
				blocks[typ] = rest
			}
		}
		for _, r := range results {
			if len(diags) == 0 || done[r.Block] {
				found[r.Type] = append(found[r.Type], r)
			}
		}
	}
}

// blockOf parses a list block address, list.<type>.q<i>, into one of
// blocks.
func blockOf(addr string, blocks map[string][]listBlock) (string, int, bool) {
	j := strings.LastIndex(addr, ".q")
	if !strings.HasPrefix(addr, "list.") || j < 0 {
		return "", 0, false
	}
	typ := addr[len("list."):j]
	i, err := strconv.Atoi(addr[j+2:])
	if err != nil || i < 0 || i >= len(blocks[typ]) {
		return "", 0, false
	}
	return typ, i, true
}

// includeResource reports whether a type must be listed with full resource
// objects: it is a parent whose spec'd attributes aren't in its identity.
func (d Discoverer) includeResource(typ string) bool {
	for _, s := range d.Specs {
		if s.Parent == typ {
			for _, attr := range s.Args {
				if !d.identityHas(typ, attr) {
					return true
				}
			}
		}
	}
	return false
}

func (d Discoverer) queryDir(typ string) (string, error) {
	dir, err := d.WS.TFDir(filepath.Join("query", typ))
	if err != nil || len(d.Opts.ProviderAttrs) == 0 {
		return dir, err
	}
	// Its own providers.tf instead of the link to the workspace's.
	src, err := os.ReadFile(d.WS.Path("providers.tf"))
	if err != nil {
		return "", err
	}
	b, err := withProviderAttrs(src, d.Opts.ProviderAttrs, strings.SplitN(d.Opts.Provider, ".", 2)[0])
	if err != nil {
		return "", fmt.Errorf("providers.tf: %w", err)
	}
	dst := filepath.Join(dir, "providers.tf")
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return dir, os.WriteFile(dst, b, 0o644)
}

// withProviderAttrs sets attrs on the provider blocks of provider (every
// block when it's empty) that don't set them already: they are that
// provider's settings. Values are JSON scalars (string, number, bool).
func withProviderAttrs(src []byte, attrs map[string]any, provider string) ([]byte, error) {
	f, diags := hclwrite.ParseConfig(src, "providers.tf", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	for _, blk := range f.Body().Blocks() {
		if blk.Type() != "provider" || provider != "" && (len(blk.Labels()) != 1 || blk.Labels()[0] != provider) {
			continue
		}
		for _, k := range slices.Sorted(maps.Keys(attrs)) {
			if blk.Body().GetAttribute(k) != nil {
				continue
			}
			switch v := attrs[k].(type) {
			case string:
				blk.Body().SetAttributeValue(k, cty.StringVal(v))
			case float64:
				blk.Body().SetAttributeValue(k, cty.NumberFloatVal(v))
			case bool:
				blk.Body().SetAttributeValue(k, cty.BoolVal(v))
			default:
				return nil, fmt.Errorf("provider attribute %s: unsupported value %v", k, v)
			}
		}
	}
	return f.Bytes(), nil
}

// versioned matches identity strings ending in a revision number after a
// separator that follows a name: "…task-definition/web:12", but not a CIDR
// ("10.0.0.0/16", "::/0") or a port after an ID ("sg-1/443").
var versioned = regexp.MustCompile(`^(.*[A-Za-z_-][:/@])(\d+)$`)

// collapseVersions keeps only the newest of resources whose identities are
// equal except for a revision: a numeric revision/version identity
// attribute ({family, revision}), or a trailing number in a string
// ("…/web:12"). It returns the kept and the dropped results.
func collapseVersions(rs []tf.ListResult) ([]tf.ListResult, []tf.ListResult) {
	type best struct {
		i   int
		rev int
	}
	groups := map[string]best{}
	keep := make([]bool, len(rs))
	for i, r := range rs {
		key, rev, ok := versionKey(r.Identity)
		if !ok {
			keep[i] = true
			continue
		}
		if b, seen := groups[key]; !seen || rev > b.rev {
			groups[key] = best{i, rev}
		}
	}
	for _, b := range groups {
		keep[b.i] = true
	}
	var out, older []tf.ListResult
	for i, r := range rs {
		if keep[i] {
			out = append(out, r)
		} else {
			older = append(older, r)
		}
	}
	return out, older
}

func versionKey(identity map[string]any) (string, int, bool) {
	rev, found := -1, false
	norm := map[string]any{}
	for _, k := range slices.Sorted(maps.Keys(identity)) {
		v := identity[k]
		norm[k] = v
		if found {
			continue
		}
		switch v := v.(type) {
		case float64:
			// Only numbers named as revisions: ports and rule numbers
			// identify distinct resources.
			if strings.Contains(k, "revision") || strings.Contains(k, "version") {
				norm[k] = nil
				rev, found = int(v), true
			}
		case string:
			if m := versioned.FindStringSubmatch(v); m != nil {
				if n, err := strconv.Atoi(m[2]); err == nil {
					norm[k] = m[1]
					rev, found = n, true
				}
			}
		}
	}
	if !found {
		return "", 0, false
	}
	b, _ := json.Marshal(norm)
	return string(b), rev, true
}

// IdentityValue converts an identity to a cty object for import blocks.
func IdentityValue(identity map[string]any) (cty.Value, error) {
	b, err := json.Marshal(identity)
	if err != nil {
		return cty.NilVal, err
	}
	ty, err := ctyjson.ImpliedType(b)
	if err != nil {
		return cty.NilVal, err
	}
	return ctyjson.Unmarshal(b, ty)
}

// instances returns what discovery has of every type so far: listed
// results and added targets.
func (d Discoverer) instances(results map[string][]tf.ListResult) map[string][]tf.ListResult {
	have := map[string][]tf.ListResult{}
	for typ, rs := range results {
		have[typ] = append(have[typ], rs...)
	}
	done := map[string]bool{}
	for _, t := range d.Added {
		if !done[t.Type] {
			done[t.Type] = true
			have[t.Type] = append(have[t.Type], d.added(t.Type)...)
		}
	}
	return have
}
