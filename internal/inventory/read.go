package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/askrike/desprawl/internal/prof"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// Result is what reading one source found.
type Result struct {
	Entries []Entry
	// Kinds counts the entries by the kind the source reports (its Kind
	// field, before KindMatch); empty if the source has no kind field.
	Kinds map[string]int
	// Truncated: a read came back cut off at a limit and partitioning
	// (if the source has a Partition) didn't make up for it. Cut lists
	// the kinds whose own read was still cut off.
	Truncated bool     `json:"truncated,omitempty"`
	Cut       []string `json:"cut,omitempty"`
	// Partitioned counts the extra reads partitioning took.
	Partitioned int `json:"partitioned,omitempty"`
}

// Truncation is how a source's result stayed incomplete, for reports.
type Truncation struct {
	Cut         []string `json:"cut,omitempty"` // kinds still cut off on their own
	Rest        bool     `json:"rest,omitempty"`
	Partitioned bool     `json:"partitioned"` // the source has a partition and it was used
}

// Discover reads the sources and maps their entries to targets. A source
// that fails is reported in errs and skipped; one whose result stayed cut
// off in truncated.
func (inv *Inventory) Discover(ctx context.Context, ws model.Workspace, r tf.Runner, params map[string]string, targets []model.Target) (entries []Entry, found []model.Target, unmapped, misfit map[string]int, truncated map[string]Truncation, errs map[string]error) {
	errs = map[string]error{}
	truncated = map[string]Truncation{}
	entries = inv.ReadRounds(ctx, ws, r, params, targets, func(s Source, res Result, err error) {
		if err != nil {
			errs[s.Label()] = err
			return
		}
		if res.Truncated || len(res.Cut) > 0 {
			truncated[s.Label()] = Truncation{Cut: res.Cut, Rest: res.Truncated, Partitioned: s.Partition != nil}
		}
	})
	found, unmapped, misfit = inv.Targets(entries)
	return entries, found, unmapped, misfit, truncated, errs
}

// maxReads bounds how many sources are read at once: each read is a
// terraform process, and discovery's queries may be running too.
const maxReads = 8

// ReadRounds reads the sources in rounds. A source read per parent is read
// for each target of the parent type as it becomes known, from discovery
// or from an earlier round (namespaces listed by one source are the
// parents of another), so it may be read more than once. each sees every
// read; entries are those of the reads that worked.
func (inv *Inventory) ReadRounds(ctx context.Context, ws model.Workspace, r tf.Runner, params map[string]string, targets []model.Target, each func(Source, Result, error)) (entries []Entry) {
	done := make([]bool, len(inv.Sources))
	read := make([]map[string]bool, len(inv.Sources)) // parents each source has been read for
	var found []model.Target
	for {
		// A round's sources are read at once (each read pays seconds of
		// terraform start-up) and reported in order.
		type read1 struct {
			s   Source
			res Result
			err error
		}
		var reads []*read1
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxReads)
		for i, s := range inv.Sources {
			var parents []model.Target
			if s.Parent != "" {
				// Read again for parents found since (by another source).
				if read[i] == nil {
					read[i] = map[string]bool{}
				}
				for _, t := range append(slices.Clone(targets), found...) {
					if t.Type == s.Parent && !read[i][t.Key()] {
						read[i][t.Key()] = true
						parents = append(parents, t)
					}
				}
				if len(parents) == 0 {
					continue
				}
			} else if done[i] {
				continue
			}
			done[i] = true
			rd := &read1{s: s}
			reads = append(reads, rd)
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				rd.res, rd.err = ReadSource(ctx, ws, r, fmt.Sprintf("inventory/s%d", i), s, params, parents)
			}()
		}
		wg.Wait()
		if len(reads) == 0 {
			return entries
		}
		for _, rd := range reads {
			each(rd.s, rd.res, rd.err)
			if rd.err == nil {
				entries = append(entries, rd.res.Entries...)
			}
		}
		found, _, _ = inv.Targets(entries)
	}
}

// block is one data block of a read: parent arguments (string values) and
// config arguments overridden with other HCL expressions.
type block struct {
	args map[string]string
	set  map[string]string
}

type blockResult struct {
	objs     []map[string]any
	complete json.RawMessage
}

type reader struct {
	ctx    context.Context
	dir    string
	r      tf.Runner
	s      Source
	params map[string]string
	kindRe *regexp.Regexp
	plans  int
}

// ReadSource reads one source in .desprawl/<sub>: once, or once per
// target of its parent type. A read that comes back cut off at a limit is
// read again in parts if the source has a Partition.
func ReadSource(ctx context.Context, ws model.Workspace, r tf.Runner, sub string, s Source, params map[string]string, targets []model.Target) (Result, error) {
	end := prof.Span("inventory", "source", s.Label())
	res, err := readSource(ctx, ws, r, sub, s, params, targets)
	end(prof.Err(err)...)
	return res, err
}

func readSource(ctx context.Context, ws model.Workspace, r tf.Runner, sub string, s Source, params map[string]string, targets []model.Target) (Result, error) {
	rd, calls, err := newReader(ctx, ws, r, sub, s, params, targets)
	if err != nil || len(calls) == 0 {
		return Result{}, err
	}
	brs, err := rd.plan(calls)
	if err != nil {
		return Result{}, err
	}
	res := Result{Kinds: map[string]int{}}
	seen := map[string]bool{}
	for i, br := range brs {
		objs := br.objs
		if rd.cutOff(br) {
			if s.Partition == nil || s.Kind == "" {
				res.Truncated = true
			} else {
				more, rest, cut, err := rd.partition(calls[i].args, objs)
				if err != nil {
					return Result{}, fmt.Errorf("partition: %w", err)
				}
				objs = append(objs, more...)
				res.Truncated = res.Truncated || rest
				res.Cut = append(res.Cut, cut...)
			}
		}
		rd.collect(&res, seen, objs)
	}
	res.Partitioned = rd.plans - 1
	sort.Strings(res.Cut)
	res.Cut = compactStrings(res.Cut)
	return res, nil
}

func newReader(ctx context.Context, ws model.Workspace, r tf.Runner, sub string, s Source, params map[string]string, targets []model.Target) (*reader, []block, error) {
	dir, err := ws.TFDir(sub)
	if err != nil {
		return nil, nil, err
	}
	rd := &reader{ctx: ctx, dir: dir, r: r, s: s, params: params}
	for _, k := range slices.Sorted(maps.Keys(s.Config)) {
		// An unfilled {param} would reach the API as a literal string and
		// fail there, after a plan.
		if m := unfilled.FindStringSubmatch(rd.fill(s.Config[k])); m != nil {
			return nil, nil, fmt.Errorf("config %s: scope parameter %q was not detected", k, m[1])
		}
	}
	if s.KindMatch != "" {
		if rd.kindRe, err = regexp.Compile(s.KindMatch); err != nil {
			return nil, nil, fmt.Errorf("kind_match: %w", err)
		}
	}
	calls := []block{{}}
	if s.Parent != "" {
		calls = nil
		seen := map[string]bool{}
		for _, t := range targets {
			if t.Type != s.Parent {
				continue
			}
			args := map[string]string{}
			for arg, attr := range s.Args {
				v, _ := t.Identity[attr].(string)
				if attr == "id" && t.ID != "" {
					v = t.ID
				}
				if v == "" {
					return nil, nil, fmt.Errorf("parent %s has no %q", t.Address(), attr)
				}
				args[arg] = v
			}
			if k := fmt.Sprint(args); !seen[k] {
				seen[k] = true
				calls = append(calls, block{args: args})
			}
		}
	}
	return rd, calls, nil
}

// plan reads blocks in one plan.
func (rd *reader) plan(blocks []block) ([]blockResult, error) {
	rd.plans++
	s := rd.s
	f := hclwrite.NewEmptyFile()
	for i, b := range blocks {
		name := fmt.Sprintf("inventory%d", i)
		body := f.Body().AppendNewBlock("data", []string{s.DataSource, name}).Body()
		if s.Provider != "" && strings.Contains(s.Provider, ".") {
			root, attr, _ := strings.Cut(s.Provider, ".")
			body.SetAttributeTraversal("provider", hcl.Traversal{hcl.TraverseRoot{Name: root}, hcl.TraverseAttr{Name: attr}})
		}
		cfg := map[string]string{}
		for k, v := range s.Config {
			cfg[k] = v
		}
		for k, v := range b.set {
			cfg[k] = v
		}
		for _, k := range slices.Sorted(maps.Keys(cfg)) {
			toks, err := hclx.ExprTokens(rd.fill(cfg[k]))
			if err != nil {
				return nil, fmt.Errorf("config %s: %w", k, err)
			}
			body.SetAttributeRaw(k, toks)
		}
		for _, k := range slices.Sorted(maps.Keys(b.args)) {
			body.SetAttributeValue(k, cty.StringVal(b.args[k]))
		}
		out := f.Body().AppendNewBlock("output", []string{name}).Body()
		toks, _ := hclx.ExprTokens("data." + s.DataSource + "." + name + "." + s.List)
		out.SetAttributeRaw("value", toks)
		out.SetAttributeRaw("sensitive", hclwrite.TokensForIdentifier("true"))
		if s.Complete != "" {
			out := f.Body().AppendNewBlock("output", []string{name + "_complete"}).Body()
			toks, err := hclx.ExprTokens("data." + s.DataSource + "." + name + "." + s.Complete)
			if err != nil {
				return nil, fmt.Errorf("complete: %w", err)
			}
			out.SetAttributeRaw("value", toks)
		}
	}
	if err := os.WriteFile(filepath.Join(rd.dir, "main.tf"), f.Bytes(), 0o644); err != nil {
		return nil, err
	}
	outs, diags, err := rd.r.In(rd.dir).PlanOutputs(rd.ctx)
	if err != nil {
		return nil, err
	}
	if len(diags) > 0 {
		return nil, fmt.Errorf("%s", diags[0].String())
	}
	brs := make([]blockResult, len(blocks))
	for i := range blocks {
		name := fmt.Sprintf("inventory%d", i)
		if raw := outs[name]; raw != nil {
			if err := json.Unmarshal(raw, &brs[i].objs); err != nil {
				return nil, fmt.Errorf("%s.%s is not a list of objects: %w", s.DataSource, s.List, err)
			}
		}
		brs[i].complete = outs[name+"_complete"]
	}
	return brs, nil
}

// unfilled matches a {param} in a source's config (not an HCL ${…}
// interpolation).
var unfilled = regexp.MustCompile(`(?:^|[^$])\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// fill replaces {param} by scope parameter values.
func (rd *reader) fill(expr string) string {
	for p, v := range rd.params {
		expr = strings.ReplaceAll(expr, "{"+p+"}", v)
	}
	return expr
}

// cutOff reports whether a read's result was cut off: the source says so
// (Complete), or, if it can't, the result's size is a round number.
func (rd *reader) cutOff(br blockResult) bool {
	if rd.s.Complete != "" && len(br.complete) > 0 && string(br.complete) != "null" {
		var v any
		if json.Unmarshal(br.complete, &v) == nil {
			return hasFalse(v)
		}
	}
	return AtLimit(len(br.objs))
}

func hasFalse(v any) bool {
	switch x := v.(type) {
	case bool:
		return !x
	case []any:
		for _, e := range x {
			if hasFalse(e) {
				return true
			}
		}
	case map[string]any:
		for _, e := range x {
			if hasFalse(e) {
				return true
			}
		}
	}
	return false
}

// AtLimit reports whether n looks like a result limit: 100 or more and
// 1, 2, 2.5 or 5 times a power of ten. A list of 1000 is far more often
// the first 1000 than everything.
func AtLimit(n int) bool {
	if n < 100 {
		return false
	}
	for n%10 == 0 {
		n /= 10
	}
	return n == 1 || n == 2 || n == 5 || n == 25
}

// maxPartitionRounds bounds the reads of "everything but" a growing set of
// kinds.
const maxPartitionRounds = 4

// partition reads a cut-off result again in parts: everything but the
// kinds that crowd it (repeated while that is cut off too), and each of
// those kinds alone. It returns the extra objects, whether the "everything
// but" read stayed cut off, and the kinds whose own read was cut off.
func (rd *reader) partition(args map[string]string, objs []map[string]any) (more []map[string]any, rest bool, cut []string, err error) {
	p := rd.s.Partition
	counts := rd.rawKinds(objs)
	if p.Except == "" {
		// Only per-kind reads: kinds the first read never saw stay unseen.
		kinds := slices.Sorted(maps.Keys(counts))
		var bs []block
		for _, k := range kinds {
			bs = append(bs, block{args: args, set: map[string]string{p.Arg: p.only(k)}})
		}
		brs, err := rd.plan(bs)
		if err != nil {
			return nil, false, nil, err
		}
		for i, br := range brs {
			more = append(more, br.objs...)
			if rd.cutOff(br) {
				cut = append(cut, kinds[i])
			}
		}
		return more, true, cut, nil
	}
	var excluded []string
	last := counts
	rest = true
	for round := 0; round < maxPartitionRounds && rest; round++ {
		add := crowding(last, excluded)
		if len(add) == 0 {
			break
		}
		excluded = append(excluded, add...)
		bs := []block{{args: args, set: map[string]string{p.Arg: p.except(excluded)}}}
		if p.Only != "" {
			for _, k := range add {
				bs = append(bs, block{args: args, set: map[string]string{p.Arg: p.only(k)}})
			}
		}
		brs, err := rd.plan(bs)
		if err != nil {
			return nil, false, nil, err
		}
		for i, br := range brs {
			more = append(more, br.objs...)
			if i > 0 && rd.cutOff(br) {
				cut = append(cut, add[i-1])
			}
		}
		rest = rd.cutOff(brs[0])
		last = rd.rawKinds(brs[0].objs)
	}
	if p.Only == "" {
		cut = append(cut, excluded...) // never read alone
	}
	return more, rest, cut, nil
}

// crowding returns the most numerous kinds not yet excluded that together
// hold at least half of the entries.
func crowding(counts map[string]int, excluded []string) []string {
	skip := map[string]bool{}
	for _, k := range excluded {
		skip[k] = true
	}
	var kinds []string
	total := 0
	for k, n := range counts {
		total += n
		if !skip[k] {
			kinds = append(kinds, k)
		}
	}
	sort.Slice(kinds, func(i, j int) bool {
		if counts[kinds[i]] != counts[kinds[j]] {
			return counts[kinds[i]] > counts[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	var out []string
	sum := 0
	for _, k := range kinds {
		if sum*2 >= total {
			break
		}
		out = append(out, k)
		sum += counts[k]
	}
	return out
}

func (p *Partition) only(kind string) string {
	return strings.ReplaceAll(p.Only, "{kind}", hclEscape(kind))
}

func (p *Partition) except(kinds []string) string {
	sep := p.Sep
	if sep == "" {
		sep = " "
	}
	var parts []string
	for _, k := range kinds {
		parts = append(parts, strings.ReplaceAll(p.Each, "{kind}", hclEscape(k)))
	}
	return strings.ReplaceAll(p.Except, "{kinds}", strings.Join(parts, sep))
}

// hclEscape escapes s for use inside an HCL quoted string.
func hclEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "${", "$${", "%{", "%%{", "\n", `\n`).Replace(s)
}

// rawKinds counts objects by the source's kind field.
func (rd *reader) rawKinds(objs []map[string]any) map[string]int {
	out := map[string]int{}
	if rd.s.Kind == "" {
		return out
	}
	for _, o := range objs {
		if k := Field(o, rd.s.Kind); k != "" {
			out[k]++
		}
	}
	return out
}

// collect turns objects into entries, once per ID.
func (rd *reader) collect(res *Result, seen map[string]bool, objs []map[string]any) {
	s := rd.s
	for _, obj := range objs {
		id := s.EntryID(obj)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		e := Entry{ID: id, Kind: s.DataSource}
		e.Name = Field(obj, s.Name)
		if s.Kind != "" {
			e.Kind = Field(obj, s.Kind)
			if e.Kind != "" {
				res.Kinds[e.Kind]++
			}
		} else if rd.kindRe != nil {
			e.Kind = id
		}
		if rd.kindRe != nil {
			m := rd.kindRe.FindStringSubmatch(e.Kind)
			if m == nil {
				continue
			}
			e.Kind = strings.Join(m[1:], "/")
		}
		res.Entries = append(res.Entries, e)
	}
}

// CheckPartition checks a partition against the provider: reading
// everything but the most numerous kind returns none of it, and reading a
// rarer kind alone returns only that kind. kinds are the counts a read of the source
// reported (Result.Kinds).
func CheckPartition(ctx context.Context, ws model.Workspace, r tf.Runner, sub string, s Source, params map[string]string, targets []model.Target, kinds map[string]int) error {
	p := s.Partition
	if p == nil {
		return fmt.Errorf("no partition")
	}
	if s.Kind == "" {
		return fmt.Errorf("the source reports no kind to partition by")
	}
	top := crowding(kinds, nil)
	if len(top) == 0 {
		return fmt.Errorf("no kinds to partition by")
	}
	k := top[0]
	rd, calls, err := newReader(ctx, ws, r, sub, s, params, targets)
	if err != nil {
		return err
	}
	if len(calls) == 0 {
		return fmt.Errorf("no parent to read the source for")
	}
	if p.Except == "" && p.Only == "" {
		return fmt.Errorf("partition has neither only nor except")
	}
	// The kind read alone is a rare one where possible: a filter that
	// doesn't work returns everything, cut off to the crowding kind.
	alone := k
	for other, n := range kinds {
		if n < kinds[alone] || (n == kinds[alone] && other < alone) {
			alone = other
		}
	}
	if p.Except != "" {
		brs, err := rd.plan([]block{{args: calls[0].args, set: map[string]string{p.Arg: p.except([]string{k})}}})
		if err != nil {
			return err
		}
		got := rd.rawKinds(brs[0].objs)
		if got[k] > 0 {
			return fmt.Errorf("reading all but %q still returned %d of it", k, got[k])
		}
		if len(got) == 0 && len(kinds) > 1 {
			return fmt.Errorf("reading all but %q returned nothing, though other kinds exist", k)
		}
		if alone == k {
			for other := range got {
				if alone == k || other < alone {
					alone = other
				}
			}
		}
	}
	if p.Only != "" {
		brs, err := rd.plan([]block{{args: calls[0].args, set: map[string]string{p.Arg: p.only(alone)}}})
		if err != nil {
			return err
		}
		got := rd.rawKinds(brs[0].objs)
		if got[alone] == 0 {
			return fmt.Errorf("reading only %q returned none of it", alone)
		}
		for other, n := range got {
			if other != alone {
				return fmt.Errorf("reading only %q also returned %d of %q", alone, n, other)
			}
		}
	}
	return nil
}

func compactStrings(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
