package tfmap

import (
	"math"
	"sort"
)

// Architecture edges are drawn right-angled through the gaps between cards.
// They are routed here rather than in the browser, so the layout metrics,
// -tidy and the ASCII view all score exactly what is on screen.

const (
	cellW, cellH = 300.0, 170.0 // grid cell in px; must match CELL_W/CELL_H in cmd/desprawl/index.html
	cardW, cardH = 200.0, 108.0 // typical card size, for port stubs and the ASCII view
)

type Point [2]float64

// Port is where an edge leaves or enters a card.
type Port struct {
	Side   int     `json:"side"`   // +1 right edge, -1 left edge
	Offset float64 `json:"offset"` // px below the card's vertical centre
}

type Route struct {
	Points []Point `json:"points"` // corners between the two ports
	Src    Port    `json:"src"`
	Dst    Port    `json:"dst"`
}

// segment is one straight piece of a routed edge.
type segment struct {
	edge     int
	vertical bool
	at       float64 // x of a vertical segment, y of a horizontal one
	lo, hi   float64 // span along the other axis
}

// routeEdges routes every edge whose ends are both placed, index-aligned
// with a.Edges (nil where unrouted). Each edge leaves its source towards the
// target, runs in the vertical gap next to it, crosses in the horizontal gap
// between rows if it has to skip columns, and enters the target from the
// side facing it. Edges sharing a gap get their own lane; edges on the same
// side of a card get their own port.
func routeEdges(a Arch, l *Layout) []*Route {
	type plan struct {
		s, t           Point
		src, dst       Port
		xs             []float64 // vertical channels (1, or 2 with a horizontal run between)
		yh             float64
		vLane, hLane   []float64
		srcTo, dstFrom float64 // y the edge heads to after leaving / comes from before entering
	}
	plans := make([]*plan, len(a.Edges))
	type portKey struct {
		node string
		side int
	}
	type portUse struct {
		edge  int
		isSrc bool
		y     float64
	}
	ports := map[portKey][]portUse{}
	occupied := map[[2]int]bool{}
	for _, p := range l.Grid {
		occupied[p] = true
	}
	// clearRow reports whether every cell strictly between columns a and b
	// on row r is empty, so an edge can run straight along that row.
	clearRow := func(r, a, b int) bool {
		for c := min(a, b) + 1; c < max(a, b); c++ {
			if occupied[[2]int{c, r}] {
				return false
			}
		}
		return true
	}
	// Runs straight along a row get no lane of their own, so a row run
	// is only taken if no other card's run already covers that stretch:
	// two runs on one line would read as one edge joining their cards.
	type run struct {
		lo, hi int
		card   string
	}
	rowRuns := map[int][]run{}
	takeRow := func(r, a, b int, card string) bool {
		if !clearRow(r, a, b) {
			return false
		}
		lo, hi := min(a, b), max(a, b)
		for _, x := range rowRuns[r] {
			if x.card != card && lo < x.hi && x.lo < hi {
				return false
			}
		}
		rowRuns[r] = append(rowRuns[r], run{lo, hi, card})
		return true
	}

	for i, e := range a.Edges {
		p, ok1 := l.Grid[e.From]
		q, ok2 := l.Grid[e.To]
		if !ok1 || !ok2 || p == q {
			continue
		}
		pl := &plan{s: Point{float64(p[0]) * cellW, float64(p[1]) * cellH}, t: Point{float64(q[0]) * cellW, float64(q[1]) * cellH}}
		c1, r1, c2, r2 := p[0], p[1], q[0], q[1]
		switch {
		case c2 != c1:
			dir := 1
			if c2 < c1 {
				dir = -1
			}
			pl.src.Side, pl.dst.Side = dir, -dir
			nearSrc := (float64(c1) + float64(dir)*.5) * cellW
			nearDst := (float64(c2) - float64(dir)*.5) * cellW
			switch {
			case c2 == c1+dir:
				pl.xs = []float64{nearSrc}
			case takeRow(r1, c1, c2, e.From): // straight along the source row, turn next to the target
				pl.xs = []float64{nearDst}
			case takeRow(r2, c1, c2, e.To): // turn next to the source, then straight along the target row
				pl.xs = []float64{nearSrc}
			default: // down into the gap between rows, across, and back up
				pl.xs = []float64{nearSrc, nearDst}
			}
		default: // same column: out and back in on the right
			pl.src.Side, pl.dst.Side = 1, 1
			pl.xs = []float64{(float64(c1) + .5) * cellW}
		}
		if len(pl.xs) == 2 {
			switch {
			case r2 < r1:
				pl.yh = (float64(r1) - .5) * cellH
			default:
				pl.yh = (float64(r1) + .5) * cellH
			}
			pl.srcTo, pl.dstFrom = pl.yh, pl.yh
		} else {
			pl.srcTo, pl.dstFrom = pl.t[1], pl.s[1]
		}
		plans[i] = pl
		ports[portKey{e.From, pl.src.Side}] = append(ports[portKey{e.From, pl.src.Side}], portUse{i, true, pl.srcTo})
		ports[portKey{e.To, pl.dst.Side}] = append(ports[portKey{e.To, pl.dst.Side}], portUse{i, false, pl.dstFrom})
	}

	// Spread ports down a card's side, ordered by where each edge heads, so
	// edges fan out without crossing each other at the card.
	for _, uses := range ports {
		sort.SliceStable(uses, func(i, j int) bool {
			if uses[i].y != uses[j].y {
				return uses[i].y < uses[j].y
			}
			return uses[i].edge < uses[j].edge
		})
		n := len(uses)
		step := math.Min(16, (cardH-28)/math.Max(1, float64(n-1)))
		for k, u := range uses {
			off := (float64(k) - float64(n-1)/2) * step
			if u.isSrc {
				plans[u.edge].src.Offset = off
			} else {
				plans[u.edge].dst.Offset = off
			}
		}
	}

	// Lanes: segments sharing a gap are spread across it, ordered so that a
	// segment turning towards higher lanes sits above the ones it would
	// otherwise cut across (and the reverse), which avoids most crossings
	// inside a gap.
	type attach struct {
		pos  float64 // where the perpendicular piece leaves, along the gap
		side int     // which way it leaves: -1 towards lower lanes, +1 higher
	}
	type laneSeg struct {
		edge, idx int
		vertical  bool
		lo, hi    float64
		ends      [2]attach
	}
	sign := func(v float64) int {
		if v < 0 {
			return -1
		}
		return 1
	}
	channels := map[[2]float64][]laneSeg{}
	for i, pl := range plans {
		if pl == nil {
			continue
		}
		pl.vLane = make([]float64, len(pl.xs))
		ys := []float64{pl.s[1] + pl.src.Offset}
		if len(pl.xs) == 2 {
			ys = append(ys, pl.yh, pl.yh)
		}
		ys = append(ys, pl.t[1]+pl.dst.Offset)
		// x of the points before and after each vertical piece
		xsAround := append(append([]float64{pl.s[0] + float64(pl.src.Side)*cardW/2}, pl.xs...), pl.t[0]+float64(pl.dst.Side)*cardW/2)
		for k, x := range pl.xs {
			y0, y1 := ys[2*k], ys[2*k+1]
			channels[[2]float64{1, x}] = append(channels[[2]float64{1, x}], laneSeg{i, k, true, math.Min(y0, y1), math.Max(y0, y1),
				[2]attach{{y0, sign(xsAround[k] - x)}, {y1, sign(xsAround[k+2] - x)}}})
		}
		if len(pl.xs) == 2 {
			pl.hLane = make([]float64, 1)
			x0, x1 := pl.xs[0], pl.xs[1]
			channels[[2]float64{0, pl.yh}] = append(channels[[2]float64{0, pl.yh}], laneSeg{i, 0, false, math.Min(x0, x1), math.Max(x0, x1),
				[2]attach{{x0, sign(ys[0] - pl.yh)}, {x1, sign(ys[3] - pl.yh)}}})
		}
	}
	// cost of putting a on a lower lane than b: crossings that order causes.
	cost := func(a, b laneSeg) int {
		n := 0
		for _, e := range a.ends {
			if e.side > 0 && e.pos >= b.lo && e.pos <= b.hi {
				n++
			}
		}
		for _, e := range b.ends {
			if e.side < 0 && e.pos >= a.lo && e.pos <= a.hi {
				n++
			}
		}
		return n
	}
	for key, segs := range channels {
		// Only segments whose spans overlap, or meet end to end, compete for
		// lanes: sharing one, touching segments would join into a fake corner.
		sort.SliceStable(segs, func(i, j int) bool { return segs[i].lo < segs[j].lo })
		for start := 0; start < len(segs); {
			end, reach := start+1, segs[start].hi
			for end < len(segs) && segs[end].lo < reach+6 {
				reach = math.Max(reach, segs[end].hi)
				end++
			}
			group := append([]laneSeg(nil), segs[start:end]...)
			sort.SliceStable(group, func(i, j int) bool { return cost(group[i], group[j]) < cost(group[j], group[i]) })
			n := float64(len(group))
			step := math.Min(10, (cellW-cardW-24)/n)
			if key[0] == 0 {
				step = math.Min(8, (cellH-cardH-14)/n)
			}
			for lane, g := range group {
				off := (float64(lane) - (n-1)/2) * step
				if g.vertical {
					plans[g.edge].vLane[g.idx] = off
				} else {
					plans[g.edge].hLane[0] = off
				}
			}
			start = end
		}
	}

	routes := make([]*Route, len(a.Edges))
	for i, pl := range plans {
		if pl == nil {
			continue
		}
		sy, ty := pl.s[1]+pl.src.Offset, pl.t[1]+pl.dst.Offset
		x1 := pl.xs[0] + pl.vLane[0]
		r := &Route{Src: pl.src, Dst: pl.dst}
		if len(pl.xs) == 1 {
			r.Points = []Point{{x1, sy}, {x1, ty}}
		} else {
			x2, yh := pl.xs[1]+pl.vLane[1], pl.yh+pl.hLane[0]
			r.Points = []Point{{x1, sy}, {x1, yh}, {x2, yh}, {x2, ty}}
		}
		routes[i] = r
	}
	return routes
}

// routeSegments is every straight piece of a routed edge, including the
// stubs from the card edge to the first corner and from the last corner in.
func routeSegments(a Arch, l *Layout, routes []*Route) []segment {
	var segs []segment
	for i, r := range routes {
		if r == nil {
			continue
		}
		p, q := l.Grid[a.Edges[i].From], l.Grid[a.Edges[i].To]
		start := Point{float64(p[0])*cellW + float64(r.Src.Side)*cardW/2, float64(p[1])*cellH + r.Src.Offset}
		end := Point{float64(q[0])*cellW + float64(r.Dst.Side)*cardW/2, float64(q[1])*cellH + r.Dst.Offset}
		pts := append(append([]Point{start}, r.Points...), end)
		for k := 1; k < len(pts); k++ {
			a, b := pts[k-1], pts[k]
			switch {
			case math.Abs(a[0]-b[0]) < .5 && math.Abs(a[1]-b[1]) >= .5:
				segs = append(segs, segment{i, true, a[0], math.Min(a[1], b[1]), math.Max(a[1], b[1])})
			case math.Abs(a[1]-b[1]) < .5 && math.Abs(a[0]-b[0]) >= .5:
				segs = append(segs, segment{i, false, a[1], math.Min(a[0], b[0]), math.Max(a[0], b[0])})
			}
		}
	}
	return segs
}

// routeStats counts crossings between edges and stretches where two edges
// run along the same track, per edge as well as in total.
func routeStats(segs []segment, edges int) (crossings, shared int, length float64, per []int) {
	per = make([]int, edges)
	for i, s := range segs {
		length += s.hi - s.lo
		for _, t := range segs[i+1:] {
			if s.edge == t.edge {
				continue
			}
			switch {
			case s.vertical != t.vertical:
				v, h := s, t
				if !s.vertical {
					v, h = t, s
				}
				if v.at > h.lo+.5 && v.at < h.hi-.5 && h.at > v.lo+.5 && h.at < v.hi-.5 {
					crossings++
					per[s.edge]++
					per[t.edge]++
				}
			case math.Abs(s.at-t.at) < 2 && math.Min(s.hi, t.hi)-math.Max(s.lo, t.lo) > 4:
				shared++
				per[s.edge]++
				per[t.edge]++
			}
		}
	}
	return
}
