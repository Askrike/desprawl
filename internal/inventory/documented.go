package inventory

import (
	"slices"
	"strings"
)

// SameID returns the other types whose documented import ID spells id
// exactly as well as typ's own does: objects that share the ID of an
// object of type typ, like an association named by what it attaches to.
// Only a plan can tell which of them exist. None when typ's docs don't
// fit id; at most max, sorted.
func SameID(id, typ string, docs map[string][]string, max int) []string {
	best := documentedScores(id, docs)
	own := best[typ]
	if own == 0 {
		return nil
	}
	var types []string
	for t, s := range best {
		if t != typ && s >= own {
			types = append(types, t)
		}
	}
	slices.Sort(types)
	if len(types) > max {
		types = types[:max]
	}
	return types
}

// documentedScores are, by type, the most segments one of its documented
// import IDs shares with id, for those of id's shape: as many segments,
// and more of them equal, ignoring case, than different.
func documentedScores(id string, docs map[string][]string) map[string]int {
	segs := segments(id)
	best := map[string]int{}
	for typ, ids := range docs {
		for _, doc := range ids {
			ds := segments(doc)
			if len(ds) != len(segs) {
				continue
			}
			same := 0
			for i := range ds {
				if strings.EqualFold(ds[i], segs[i]) {
					same++
				}
			}
			if 2*same > len(segs) && same > best[typ] {
				best[typ] = same
			}
		}
	}
	return best
}

func segments(id string) []string {
	return strings.FieldsFunc(id, func(r rune) bool { return r == '/' || r == ':' })
}
