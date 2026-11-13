package tfmap

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

type layoutMetrics struct {
	Overlaps   []string // cells holding more than one component
	Crossings  int      // pairs of routed edges that cross
	Shared     int      // stretches where two routed edges run on the same track
	Intrusions []string // components inside another module's bounding box
	Crowded    []string // module boxes with no empty row/column between them
	Length     int      // total routed edge length, in units of 100px
	Unplaced   []string
	Worst      []string // edges involved in the most crossings, worst first
}

func measure(a Arch, l *Layout) layoutMetrics {
	var m layoutMetrics
	cells := map[[2]int][]string{}
	for _, n := range a.Nodes {
		p, ok := l.Grid[n.ID]
		if !ok {
			m.Unplaced = append(m.Unplaced, n.ID)
			continue
		}
		cells[p] = append(cells[p], n.ID)
	}
	for p, ids := range cells {
		if len(ids) > 1 {
			m.Overlaps = append(m.Overlaps, fmt.Sprintf("(%d,%d): %s", p[0], p[1], strings.Join(ids, ", ")))
		}
	}
	sort.Strings(m.Overlaps)

	// Score the edges exactly as the viewer draws them.
	segs := routeSegments(a, l, routeEdges(a, l))
	var per []int
	var length float64
	m.Crossings, m.Shared, length, per = routeStats(segs, len(a.Edges))
	m.Length = int(math.Round(length / 100))
	idx := make([]int, len(a.Edges))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool { return per[idx[x]] > per[idx[y]] })
	for _, i := range idx[:min(8, len(idx))] {
		if per[i] > 0 {
			e := a.Edges[i]
			m.Worst = append(m.Worst, fmt.Sprintf("%d crossings/shared tracks: %s %v -> %s %v",
				per[i], e.From, l.Grid[e.From], e.To, l.Grid[e.To]))
		}
	}

	// A module's container is the bounding box of everything inside it,
	// including nested modules; anything else in that box breaks the grouping.
	boxes := map[string][4]int{}
	for _, n := range a.Nodes {
		p, ok := l.Grid[n.ID]
		if !ok {
			continue
		}
		for _, mod := range moduleAncestors(n.Module) {
			b, seen := boxes[mod]
			if !seen {
				b = [4]int{p[0], p[1], p[0], p[1]}
			}
			b = [4]int{min(b[0], p[0]), min(b[1], p[1]), max(b[2], p[0]), max(b[3], p[1])}
			boxes[mod] = b
		}
	}
	for mod, b := range boxes {
		for _, n := range a.Nodes {
			p, ok := l.Grid[n.ID]
			if !ok || n.Module == mod || strings.HasPrefix(n.Module, mod+".") {
				continue
			}
			if p[0] >= b[0] && p[0] <= b[2] && p[1] >= b[1] && p[1] <= b[3] {
				m.Intrusions = append(m.Intrusions, fmt.Sprintf("%s is inside %s", n.ID, mod))
			}
		}
	}
	sort.Strings(m.Intrusions)
	mods := make([]string, 0, len(boxes))
	for mod := range boxes {
		mods = append(mods, mod)
	}
	sort.Strings(mods)
	for i, x := range mods {
		for _, y := range mods[i+1:] {
			if strings.HasPrefix(y, x+".") {
				continue // nested: y's box is meant to be inside x's
			}
			bx, by := boxes[x], boxes[y]
			if bx[0]-1 <= by[2] && by[0] <= bx[2]+1 && bx[1]-1 <= by[3] && by[1] <= bx[3]+1 {
				m.Crowded = append(m.Crowded, x+" touches "+y)
			}
		}
	}
	return m
}

// moduleAncestors("module.a.module.b") = [module.a, module.a.module.b].
func moduleAncestors(mod string) []string {
	if mod == "" {
		return nil
	}
	parts := strings.Split(mod, ".")
	var out []string
	for i := 2; i <= len(parts); i += 2 {
		out = append(out, strings.Join(parts[:i], "."))
	}
	return out
}
