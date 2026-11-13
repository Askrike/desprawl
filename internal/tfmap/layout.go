package tfmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// Layout places architecture components on a grid: id -> [col, row]. It is
// shared between the browser (drag to move) and Claude (edit the JSON, check
// with -ascii), so it uses coarse cells rather than pixels.
type Layout struct {
	Grid map[string][2]int `json:"grid"`
}

// LoadLayout returns nil when no layout has been saved for the directory.
func LoadLayout(path string) (*Layout, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var l Layout
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &l, nil
}

// SaveLayout writes the layout; an empty grid resets the viewer to automatic
// layout. It reports whether anything changed.
func SaveLayout(path string, l Layout) (bool, error) {
	var b []byte
	if len(l.Grid) > 0 {
		var err error
		if b, err = json.MarshalIndent(l, "", "  "); err != nil {
			return false, err
		}
		b = append(b, '\n')
	}
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if b == nil {
			return false, nil
		}
	case err != nil:
		return false, err
	case bytes.Equal(old, b):
		return false, nil
	}
	if b == nil {
		return true, os.Remove(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, b, 0o644)
}

// Seed lays components out by the flow of information. An arrow
// points from what acts to what it acts on (a load balancer routes to a
// service, a service reads a secret), so columns follow arrows left to
// right: a component's column is its longest path from an entry point
// (something nothing points at), so everything sits right of what uses it.
// Columns taller than about twice the layout's width spill into extra
// columns further right. Within a column, rows follow the average row of
// each component's neighbours (a few sweeps), so connected components sit
// level with each other. Groups of components with no arrows between them get
// their own band of rows, with an empty row between modules; components with
// no connections go in a block underneath. Finally, tidy removes what
// crossings it can.
func Seed(a Arch) *Layout {
	in := map[string][]string{}
	nbrs := map[string][]string{}
	for _, e := range a.Edges {
		in[e.To] = append(in[e.To], e.From)
		nbrs[e.From] = append(nbrs[e.From], e.To)
		nbrs[e.To] = append(nbrs[e.To], e.From)
	}

	// Split the connected components into groups that arrows don't join.
	group := map[string]int{}
	var groups [][]ArchNode
	var loose []ArchNode
	byID := map[string]ArchNode{}
	for _, n := range a.Nodes {
		byID[n.ID] = n
	}
	for _, n := range a.Nodes {
		if len(nbrs[n.ID]) == 0 {
			loose = append(loose, n)
			continue
		}
		if _, ok := group[n.ID]; ok {
			continue
		}
		g := len(groups)
		groups = append(groups, nil)
		stack := []string{n.ID}
		group[n.ID] = g
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			groups[g] = append(groups[g], byID[id])
			for _, m := range nbrs[id] {
				if _, ok := group[m]; !ok {
					group[m] = g
					stack = append(stack, m)
				}
			}
		}
	}
	// Lay out module by module (a group belongs to its first module), with
	// an empty row between modules: a module's groups, largest first so its
	// main flow comes first, then its unconnected components. Modules with
	// nothing connected share one block at the bottom.
	firstModule := func(g []ArchNode) string {
		m := g[0].Module
		for _, n := range g {
			m = min(m, n.Module)
		}
		return m
	}
	sort.SliceStable(groups, func(i, j int) bool { return len(groups[i]) > len(groups[j]) })
	groupsOf := map[string][]int{}
	for i, g := range groups {
		groupsOf[firstModule(g)] = append(groupsOf[firstModule(g)], i)
	}
	mods := make([]string, 0, len(groupsOf))
	for m := range groupsOf {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	looseOf := map[string][]ArchNode{}
	var rest []ArchNode
	for _, n := range loose {
		if groupsOf[n.Module] != nil {
			looseOf[n.Module] = append(looseOf[n.Module], n)
		} else {
			rest = append(rest, n)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Module < rest[j].Module })

	tallest := max(4, int(math.Ceil(math.Sqrt(2*float64(len(a.Nodes)-len(loose))))))
	cols := make([][][]ArchNode, len(groups))
	width := int(math.Ceil(math.Sqrt(float64(len(loose)))))
	for i, g := range groups {
		cols[i] = flowColumns(g, in, nbrs, tallest)
		width = max(width, len(cols[i]))
	}
	l := &Layout{Grid: map[string][2]int{}}
	top := 0
	block := func(ns []ArchNode) {
		for k, n := range ns {
			l.Grid[n.ID] = [2]int{k % width, top + k/width}
		}
		top += (len(ns) + width - 1) / width
	}
	for i, m := range mods {
		if i > 0 {
			top++
		}
		for _, g := range groupsOf[m] {
			height := 0
			for c, col := range cols[g] {
				for r, n := range col {
					l.Grid[n.ID] = [2]int{c, top + r}
				}
				height = max(height, len(col))
			}
			top += height
		}
		if len(looseOf[m]) > 0 {
			top++
			block(looseOf[m])
		}
	}
	if len(rest) > 0 {
		if len(mods) > 0 {
			top++
		}
		block(rest)
	}
	Tidy(a, l)
	return l
}

// flowColumns arranges one group of connected components into columns by
// longest path from an entry point, spilling columns taller than tallest,
// and orders each column so neighbours sit level (see seedLayout).
func flowColumns(nodes []ArchNode, in, nbrs map[string][]string, tallest int) [][]ArchNode {
	fromEntry := map[string]int{}
	var walk func(id string, onPath map[string]bool) int
	walk = func(id string, onPath map[string]bool) int {
		if d, ok := fromEntry[id]; ok {
			return d
		}
		onPath[id] = true
		d := 0
		for _, f := range in[id] {
			if !onPath[f] {
				d = max(d, walk(f, onPath)+1)
			}
		}
		delete(onPath, id)
		fromEntry[id] = d
		return d
	}
	// Walk in a fixed order: with cycles, depths depend on where walks start.
	sorted := append([]ArchNode(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var cols [][]ArchNode
	for _, n := range sorted {
		c := walk(n.ID, map[string]bool{})
		for len(cols) <= c {
			cols = append(cols, nil)
		}
		cols[c] = append(cols[c], n)
	}

	row := map[string]float64{}
	order := func() {
		for _, col := range cols {
			sort.SliceStable(col, func(i, j int) bool {
				if col[i].Module != col[j].Module {
					return col[i].Module < col[j].Module
				}
				return col[i].Name < col[j].Name
			})
			for r, n := range col {
				row[n.ID] = float64(r)
			}
		}
		for sweep := 0; sweep < 8; sweep++ {
			for _, col := range cols {
				bary := map[string]float64{}
				for _, n := range col {
					sum := 0.0
					for _, m := range nbrs[n.ID] {
						sum += row[m]
					}
					bary[n.ID] = sum / float64(len(nbrs[n.ID]))
				}
				sort.SliceStable(col, func(i, j int) bool { return bary[col[i].ID] < bary[col[j].ID] })
				for r, n := range col {
					row[n.ID] = float64(r)
				}
			}
		}
	}
	order()

	// Spill tall columns rightwards, keeping their order.
	var split [][]ArchNode
	for _, col := range cols {
		for len(col) > tallest {
			split = append(split, col[:tallest])
			col = col[tallest:]
		}
		split = append(split, col)
	}
	cols = split
	order()
	return cols
}

// Tidy improves a layout without changing its flow: it only moves
// components within their column (swapping two of them, or moving one to an
// empty cell), keeping any change that lowers crossings, shared tracks, edge
// length and module intrusions. Each try re-routes and re-scores the whole
// layout, which costs about the square of its size, so large layouts get
// fewer tries (a fixed budget, so the result stays deterministic).
func Tidy(a Arch, l *Layout) {
	const budget = 50_000_000
	size := len(a.Nodes) + len(a.Edges)
	tries := budget / (size*size + 1)
	score := func() (int, bool) {
		tries--
		m := measure(a, l)
		return m.Crossings*4 + m.Shared*3 + m.Length + 4*len(m.Intrusions), len(m.Overlaps) == 0
	}
	best, _ := score()
	for improved := true; improved; {
		improved = false
		for i, n := range a.Nodes {
			p, ok := l.Grid[n.ID]
			if !ok {
				continue
			}
			for _, o := range a.Nodes[i+1:] {
				if tries <= 0 {
					return
				}
				q, ok := l.Grid[o.ID]
				if !ok || q[0] != p[0] {
					continue
				}
				l.Grid[n.ID], l.Grid[o.ID] = q, p
				if s, valid := score(); valid && s < best {
					best, p, improved = s, q, true
					continue
				}
				l.Grid[n.ID], l.Grid[o.ID] = p, q
			}
			taken := map[[2]int]bool{}
			lo, hi := p[1], p[1]
			for _, q := range l.Grid {
				taken[q] = true
				if q[0] == p[0] {
					lo, hi = min(lo, q[1]), max(hi, q[1])
				}
			}
			for r := lo - 1; r <= hi+1; r++ {
				if tries <= 0 {
					return
				}
				if taken[[2]int{p[0], r}] {
					continue
				}
				l.Grid[n.ID] = [2]int{p[0], r}
				if s, valid := score(); valid && s < best {
					best, p, improved = s, [2]int{p[0], r}, true
					continue
				}
				l.Grid[n.ID] = p
			}
		}
	}
}
