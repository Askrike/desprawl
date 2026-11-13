package link

import (
	"testing"

	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/rules"
)

func TestRuleEvidence(t *testing.T) {
	docs := rules.Rule{Source: "x_subnet", Path: "vpc_id", Target: "x_vpc", Attr: "id", Origin: rules.OriginDocs, Support: 3}
	llm := rules.Rule{Source: "x_alarm", Path: "dimensions.ClusterName", Target: "x_cluster", Attr: "name", Origin: rules.OriginLLM}
	vpc := index.Ref{Address: "x_vpc.main", Type: "x_vpc", Attr: "id"}
	cluster := index.Ref{Address: "x_cluster.c", Type: "x_cluster", Attr: "name"}
	for _, c := range []struct {
		name    string
		e       evidence
		want    Confidence
		nLimits int
	}{
		{"unique ID of the target", ruleEvidence(docs, vpc, true, 1, "this resource already references"), Observed, 0},
		{"ID shared by two targets", ruleEvidence(docs, vpc, true, 2, "this resource already references"), Derived, 1},
		{"documented name", ruleEvidence(docs, cluster, false, 1, "this resource already references"), Derived, 1},
		{"name judged by Claude", ruleEvidence(llm, cluster, false, 1, "this resource already references"), Inferred, 1},
	} {
		if c.e.confidence != c.want || len(c.e.limitations) != c.nLimits {
			t.Errorf("%s: got %s with %v", c.name, c.e.confidence, c.e.limitations)
		}
	}
}

func TestMentionable(t *testing.T) {
	assigned := index.Ref{Attr: "id", Assigned: true}
	chosen := index.Ref{Attr: "id"}
	if !mentionable(assigned, "vol-0abc12345") {
		t.Error("provider-assigned id mentioned in a string should link")
	}
	if !mentionable(chosen, "arn:p:s:r:1:thing/x") {
		t.Error("structured identifier should link")
	}
	if mentionable(chosen, "my-statement-1A2b3C4d") {
		t.Error("author-chosen bare name inside a document must not link")
	}
}
