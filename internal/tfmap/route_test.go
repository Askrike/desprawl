package tfmap

import "testing"

// Two edges whose vertical runs meet end to end in one gap get separate
// lanes, so their corners don't join into what reads as a crossing.
func TestTouchingRunsGetLanes(t *testing.T) {
	a := Arch{
		Nodes: []ArchNode{{ID: "s1"}, {ID: "t1"}, {ID: "s2"}, {ID: "t2"}},
		Edges: []ArchEdge{{From: "s1", To: "t1"}, {From: "s2", To: "t2"}},
	}
	l := &Layout{Grid: map[string][2]int{"s1": {0, -1}, "t1": {1, 0}, "s2": {0, 0}, "t2": {1, 1}}}
	r := routeEdges(a, l)
	for _, p := range r[0].Points {
		for _, q := range r[1].Points {
			if p == q {
				t.Errorf("routes share the corner %v: %v and %v", p, r[0].Points, r[1].Points)
			}
		}
	}
}

// Two edges can't both run straight along one row from different cards:
// on one line they'd read as an edge between those cards.
func TestRowRunsDontOverlap(t *testing.T) {
	a := Arch{
		Nodes: []ArchNode{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}},
		Edges: []ArchEdge{{From: "a", To: "c"}, {From: "b", To: "d"}},
	}
	l := &Layout{Grid: map[string][2]int{"a": {0, 0}, "b": {2, 0}, "c": {2, 1}, "d": {0, 1}}}
	// They must cross in the gap; they mustn't run along the row together.
	segs := routeSegments(a, l, routeEdges(a, l))
	for i, x := range segs {
		for _, y := range segs[i+1:] {
			if x.edge != y.edge && !x.vertical && !y.vertical && x.at == y.at && min(x.hi, y.hi)-max(x.lo, y.lo) > cellW-cardW {
				t.Errorf("edges %d and %d share row y=%v from %v to %v", x.edge, y.edge, x.at, max(x.lo, y.lo), min(x.hi, y.hi))
			}
		}
	}
}
