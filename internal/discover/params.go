package discover

import (
	"maps"
	"slices"
	"strings"

	"github.com/askrike/desprawl/internal/model"
)

// Params detects scope parameters: identity attributes that most resources
// carry with one shared value, such as the account, project or region
// everything lives in. Values that differ only in case count as one scope
// (some APIs report the same identifier in either case); the parameter
// takes its most common spelling.
func Params(targets []model.Target) map[string]string {
	carriers := map[string]int{}
	folded := map[string]map[string]int{}    // lower-cased value -> count
	spellings := map[string]map[string]int{} // exact value -> count
	for _, t := range targets {
		for k, v := range t.Identity {
			s, ok := v.(string)
			if !ok || s == "" {
				continue
			}
			carriers[k]++
			if folded[k] == nil {
				folded[k] = map[string]int{}
				spellings[k] = map[string]int{}
			}
			folded[k][strings.ToLower(s)]++
			spellings[k][s]++
		}
	}
	out := map[string]string{}
	for k, n := range carriers {
		// Carried by at least a third of resources, with one value at least
		// 95% of the time: a scope, not an identifier.
		if n*3 < len(targets) || n < 3 {
			continue
		}
		for v, c := range folded[k] {
			if c*100 >= n*95 && len(v) >= 4 {
				out[k] = commonest(spellings[k], v)
			}
		}
	}
	return out
}

// commonest returns the most common spelling of a lower-cased value (ties go
// to the first in sort order, so the result is deterministic).
func commonest(spellings map[string]int, lower string) string {
	best, bestN := "", 0
	for _, s := range slices.Sorted(maps.Keys(spellings)) {
		if strings.ToLower(s) == lower && spellings[s] > bestN {
			best, bestN = s, spellings[s]
		}
	}
	return best
}
