package tfmap

// The map as text, for agents and terminals: the grid, the cards and
// routed edges in box-drawing characters, and the layout's metrics.

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

var blankRuns = regexp.MustCompile(`\n{3,}`)

// RenderASCII draws the grid for Claude (or a terminal): one cell per
// component, then the module boxes, edges and metrics.
func RenderASCII(a Arch, l *Layout) string {
	const w = 26
	short := func(n ArchNode) string {
		s := n.Label + " " + n.Name
		if len(s) > w-2 {
			s = s[:w-3] + "…"
		}
		return s
	}
	byID := map[string]ArchNode{}
	minC, minR, maxC, maxR, first := 0, 0, 0, 0, true // cells may be negative
	cells := map[[2]int][]ArchNode{}
	for _, n := range a.Nodes {
		byID[n.ID] = n
		if p, ok := l.Grid[n.ID]; ok {
			if first {
				minC, minR, maxC, maxR, first = p[0], p[1], p[0], p[1], false
			}
			cells[p] = append(cells[p], n)
			minC, minR = min(minC, p[0]), min(minR, p[1])
			maxC, maxR = max(maxC, p[0]), max(maxR, p[1])
		}
	}

	var b strings.Builder
	b.WriteString("      ")
	for c := minC; c <= maxC; c++ {
		fmt.Fprintf(&b, "%-*s", w, fmt.Sprintf("col %d", c))
	}
	b.WriteString("\n")
	for r := minR; r <= maxR; r++ {
		fmt.Fprintf(&b, "r%-4d ", r)
		for c := minC; c <= maxC; c++ {
			ns := cells[[2]int{c, r}]
			switch len(ns) {
			case 0:
				fmt.Fprintf(&b, "%-*s", w, "  ·")
			case 1:
				fmt.Fprintf(&b, "%-*s", w, "["+short(ns[0])+"]")
			default:
				fmt.Fprintf(&b, "%-*s", w, fmt.Sprintf("[!! %d OVERLAP]", len(ns)))
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("\nas drawn (cards ╔═╗, routed edges, ▶ where an edge enters its target):\n")
	b.WriteString(blankRuns.ReplaceAllString(drawRoutes(a, l, routeEdges(a, l)), "\n\n"))

	b.WriteString("\nmodules (container = bounding box of its components):\n")
	mods := map[string][]string{}
	for _, n := range a.Nodes {
		mods[n.Module] = append(mods[n.Module], n.ID)
	}
	names := make([]string, 0, len(mods))
	for m := range mods {
		names = append(names, m)
	}
	sort.Strings(names)
	for _, m := range names {
		label := m
		if label == "" {
			label = "(root)"
		}
		fmt.Fprintf(&b, "  %s:\n", label)
		for _, id := range mods[m] {
			p, ok := l.Grid[id]
			pos := "unplaced"
			if ok {
				pos = fmt.Sprintf("(%d,%d)", p[0], p[1])
			}
			fmt.Fprintf(&b, "    %-9s %-28s %s\n", pos, short(byID[id]), id)
		}
	}

	// Edge endpoints carry their module, since names repeat across modules.
	qual := func(n ArchNode) string {
		if n.Module == "" {
			return short(n)
		}
		return n.Module[strings.LastIndex(n.Module, ".")+1:] + "/" + short(n)
	}
	b.WriteString("\nedges:\n")
	for _, e := range a.Edges {
		tail := "--"
		if e.Both {
			tail = "<--"
		}
		fmt.Fprintf(&b, "  %s %s%s--> %s\n", qual(byID[e.From]), tail, e.Label, qual(byID[e.To]))
	}

	m := measure(a, l)
	fmt.Fprintf(&b, "\nmetrics: crossings=%d shared_tracks=%d overlaps=%d module_intrusions=%d crowded_modules=%d edge_length=%d unplaced=%d\n",
		m.Crossings, m.Shared, len(m.Overlaps), len(m.Intrusions), len(m.Crowded), m.Length, len(m.Unplaced))
	// Overlaps and unplaced components are listed in full (they're what to
	// fix); the rest is capped, so a large map stays readable.
	list := func(prefix string, items []string, limit int, plural string) {
		for i, s := range items {
			if i == limit {
				fmt.Fprintf(&b, "  … and %d more %s\n", len(items)-limit, plural)
				break
			}
			b.WriteString("  " + prefix + s + "\n")
		}
	}
	list("overlap ", m.Overlaps, len(m.Overlaps), "")
	list("unplaced: ", m.Unplaced, len(m.Unplaced), "")
	list("crowded: ", m.Crowded, 8, "crowded modules")
	list("intrusion: ", m.Intrusions, 8, "intrusions")
	list("worst edge, ", m.Worst, len(m.Worst), "")
	return b.String()
}

// drawRoutes renders cards and routed edges as box-drawing characters, at
// 20 characters and 7 lines per grid cell.
func drawRoutes(a Arch, l *Layout, routes []*Route) string {
	const cx, cy = 20 / cellW, 7 / cellH
	type cellMask = uint8
	const (
		up cellMask = 1 << iota
		down
		left
		right
	)
	// Dragging can leave cells at negative coordinates; the canvas spans
	// the cells in use.
	minX, minY, maxX, maxY, first := 0, 0, 0, 0, true
	for _, n := range a.Nodes {
		if p, ok := l.Grid[n.ID]; ok {
			if first {
				minX, minY, maxX, maxY, first = p[0], p[1], p[0], p[1], false
			}
			minX, minY = min(minX, p[0]), min(minY, p[1])
			maxX, maxY = max(maxX, p[0]), max(maxY, p[1])
		}
	}
	w, h := int(float64(maxX-minX+1)*cellW*cx)+12, int(float64(maxY-minY+1)*cellH*cy)+4
	ox := 10 - float64(minX)*cellW*cx // margin so the leftmost cards and channels fit
	oy := 2 - float64(minY)*cellH*cy
	at := func(p Point) (int, int) {
		return int(math.Round(p[0]*cx + ox)), int(math.Round(p[1]*cy + oy))
	}
	mask := make([][]cellMask, h)
	canvas := make([][]rune, h)
	for y := range canvas {
		mask[y] = make([]cellMask, w)
		canvas[y] = []rune(strings.Repeat(" ", w))
	}
	set := func(x, y int, m cellMask) {
		if y >= 0 && y < h && x >= 0 && x < w {
			mask[y][x] |= m
		}
	}
	for _, s := range routeSegments(a, l, routes) {
		if s.vertical {
			x, y0 := at(Point{s.at, s.lo})
			_, y1 := at(Point{s.at, s.hi})
			for y := y0; y <= y1; y++ {
				if y > y0 {
					set(x, y, up)
				}
				if y < y1 {
					set(x, y, down)
				}
			}
		} else {
			x0, y := at(Point{s.lo, s.at})
			x1, _ := at(Point{s.hi, s.at})
			for x := x0; x <= x1; x++ {
				if x > x0 {
					set(x, y, left)
				}
				if x < x1 {
					set(x, y, right)
				}
			}
		}
	}
	glyph := map[cellMask]rune{
		left: '─', right: '─', left | right: '─', up: '│', down: '│', up | down: '│',
		right | down: '┌', left | down: '┐', right | up: '└', left | up: '┘',
		left | right | down: '┬', left | right | up: '┴', up | down | right: '├', up | down | left: '┤',
		up | down | left | right: '┼',
	}
	for y := range mask {
		for x, m := range mask[y] {
			if g, ok := glyph[m]; ok {
				canvas[y][x] = g
			}
		}
	}
	// Arrowheads where edges enter their target.
	for i, r := range routes {
		if r == nil {
			continue
		}
		q := l.Grid[a.Edges[i].To]
		x, y := at(Point{float64(q[0])*cellW + float64(r.Dst.Side)*cardW/2, float64(q[1])*cellH + r.Dst.Offset})
		x += r.Dst.Side // just outside the card border
		if y >= 0 && y < h && x >= 0 && x < w {
			canvas[y][x] = '▶'
			if r.Dst.Side == 1 {
				canvas[y][x] = '◀'
			}
		}
	}
	// Cards on top.
	for _, n := range a.Nodes {
		p, ok := l.Grid[n.ID]
		if !ok {
			continue
		}
		x0, y0 := at(Point{float64(p[0])*cellW - cardW/2, float64(p[1])*cellH - cardH/2})
		x1, y1 := at(Point{float64(p[0])*cellW + cardW/2, float64(p[1])*cellH + cardH/2})
		x0, x1 = x0+1, x1-1
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				if y < 0 || y >= h || x < 0 || x >= w {
					continue
				}
				c := ' '
				switch {
				case y == y0 && x == x0:
					c = '╔'
				case y == y0 && x == x1:
					c = '╗'
				case y == y1 && x == x0:
					c = '╚'
				case y == y1 && x == x1:
					c = '╝'
				case y == y0 || y == y1:
					c = '═'
				case x == x0 || x == x1:
					c = '║'
				}
				canvas[y][x] = c
			}
		}
		label := []rune(n.Name)
		if len(label) > x1-x0-1 {
			label = append(label[:x1-x0-2], '…')
		}
		for k, c := range label {
			if y := (y0 + y1) / 2; y >= 0 && y < h && x0+1+k >= 0 && x0+1+k < w {
				canvas[y][x0+1+k] = c
			}
		}
	}
	var b strings.Builder
	for _, row := range canvas {
		b.WriteString(strings.TrimRight(string(row), " ") + "\n")
	}
	return b.String()
}

// Describe renders a config as text: the layout (saved or seed) as an
// ASCII drawing with ids and metrics, then the types no catalog covers.
// `desprawl map` prints it and the MCP get_map tool returns it.
func Describe(dir string, g *Graph, layoutPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "configuration %s: %d components, %d connections\n", dir, len(g.Arch.Nodes), len(g.Arch.Edges))
	l := g.Layout
	if l == nil {
		l = Seed(g.Arch)
		fmt.Fprintf(&b, "layout: none saved yet (%s), showing the automatic one\n\n", layoutPath)
	} else {
		fmt.Fprintf(&b, "layout: %s\n\n", layoutPath)
	}
	b.WriteString(RenderASCII(g.Arch, l))
	if missing := Unclassified(g); len(missing) > 0 {
		b.WriteString("\ntypes not in desprawl's catalog (guessed):\n")
		for _, n := range missing {
			fmt.Fprintf(&b, "  %s %s (treated as %s)\n", n.Kind, n.Type, n.Role)
		}
	}
	if len(g.Diags) > 0 {
		fmt.Fprintf(&b, "\nparse errors (blocks after them may be missing):\n  %s\n", strings.Join(g.Diags, "\n  "))
	}
	return b.String()
}

// Unclassified is one node per resource or data source type that no
// catalog covers.
func Unclassified(g *Graph) []Node {
	seen := map[string]bool{}
	var out []Node
	for _, n := range g.Nodes {
		key := n.Kind + " " + n.Type
		if (n.Kind == "resource" || n.Kind == "data") && !n.Classified && !seen[key] {
			seen[key] = true
			out = append(out, n)
		}
	}
	return out
}
