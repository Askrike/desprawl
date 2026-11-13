package rules

import "testing"

// A rejected rule is never used, and takes out the same signature accepted
// elsewhere: a later judgment that it isn't a reference wins.
func TestRejected(t *testing.T) {
	keep := Rule{Source: "x_svc", Path: "cluster", Target: "x_cluster", Attr: "name", Match: MatchExact, Origin: OriginDocs}
	gone := Rule{Source: "x_client", Path: "alg", Target: "x_server", Attr: "signing_alg", Match: MatchExact, Origin: OriginLLM}
	no := gone
	no.Rejected = true
	s := NewSet([]Rule{keep, gone, no})
	if len(s.rules) != 1 || s.rules[0] != keep {
		t.Errorf("rules = %+v, want only %+v", s.rules, keep)
	}
	if got := s.targets["x_server"]; len(got) != 0 {
		t.Errorf("x_server targets = %v", got)
	}
}
