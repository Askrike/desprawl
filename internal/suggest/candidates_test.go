package suggest

import (
	"testing"

	"github.com/askrike/desprawl/internal/link"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
)

// A type-scoped drop ("type.attr") keeps that attribute from being a rule
// target, as link removes it from that type only.
func TestCandidatesHonourTypeDrop(t *testing.T) {
	plan, err := tf.ParsePlan([]byte(`{"resource_changes": [
	 {"address": "x_topic.t", "mode": "managed", "type": "x_topic", "change": {"before": {"id": "t1", "label": "orders"}}},
	 {"address": "x_app.a", "mode": "managed", "type": "x_app", "change": {"before": {"id": "a1", "topic": "orders"}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`resource "x_topic" "t" {
  label = "orders"
}
resource "x_app" "a" {
  topic = "orders"
}
`)
	targets := func(drop ...string) bool {
		cs, err := Candidates(src, plan, nil, link.Options{Rules: rules.NewSet(nil), Drop: drop})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.Target == "x_topic" && c.Attr == "label" {
				return true
			}
		}
		return false
	}
	if !targets() {
		t.Fatal("without a drop, x_topic.label should be a candidate target")
	}
	if targets("x_topic.label") {
		t.Error("x_topic.label is dropped but still a candidate target")
	}
	if !targets("x_other.label") {
		t.Error("a drop for another type removed x_topic.label")
	}
}

func candidates(t *testing.T, planJSON, src string, opts link.Options) []Candidate {
	t.Helper()
	plan, err := tf.ParsePlan([]byte(planJSON))
	if err != nil {
		t.Fatal(err)
	}
	if opts.Rules == nil {
		opts.Rules = rules.NewSet(nil)
	}
	cs, err := Candidates([]byte(src), plan, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// A scope value (the region) becomes a reference to its data source, so
// other resources that have it aren't suggested as its target.
func TestCandidatesSkipScopeValues(t *testing.T) {
	plan := `{"resource_changes": [
	 {"address": "x_view.v", "mode": "managed", "type": "x_view", "change": {"before": {"id": "v1", "name": "eu-central-1"}}},
	 {"address": "x_app.a", "mode": "managed", "type": "x_app", "change": {"before": {"id": "a1", "region": "eu-central-1"}}}
	]}`
	src := `resource "x_view" "v" {
  name = "eu-central-1"
}
resource "x_app" "a" {
  region = "eu-central-1"
}
`
	if cs := candidates(t, plan, src, link.Options{}); len(cs) == 0 {
		t.Fatal("without the region as a scope value, x_app.region should be a candidate")
	}
	params := []link.Param{{Value: "eu-central-1", Ref: "data.x_region.current.name"}}
	if cs := candidates(t, plan, src, link.Options{Params: params}); len(cs) != 0 {
		t.Errorf("scope value suggested: %+v", cs)
	}
}

// A name that an embedded rule already linked inside a string isn't
// suggested again.
func TestCandidatesSkipLinkedInside(t *testing.T) {
	plan := `{"resource_changes": [
	 {"address": "x_integration.i", "mode": "managed", "type": "x_integration", "change": {"before": {"id": "abc123x"}}},
	 {"address": "x_route.r", "mode": "managed", "type": "x_route", "change": {"before": {"id": "r1", "target": "integrations/abc123x"}}}
	]}`
	src := `resource "x_integration" "i" {
}
resource "x_route" "r" {
  target = "integrations/abc123x"
}
`
	if cs := candidates(t, plan, src, link.Options{}); len(cs) == 0 {
		t.Fatal("without a rule, x_route.target should be a candidate")
	}
	set := rules.NewSet([]rules.Rule{{Source: "x_route", Path: "target", Target: "x_integration", Attr: "id", Match: rules.MatchEmbedded}})
	if cs := candidates(t, plan, src, link.Options{Rules: set}); len(cs) != 0 {
		t.Errorf("already linked, still suggested: %+v", cs)
	}
}

// Of the resources a value names, those that refer to another of them
// (a bucket's ACL) carry its name: only the one they refer to is suggested.
func TestCandidatesPreferParent(t *testing.T) {
	plan := `{"resource_changes": [
	 {"address": "x_bucket.b", "mode": "managed", "type": "x_bucket", "change": {"before": {"id": "my-bucket", "bucket": "my-bucket"}}},
	 {"address": "x_bucket_acl.a", "mode": "managed", "type": "x_bucket_acl", "change": {"before": {"id": "my-bucket", "bucket": "my-bucket"}}},
	 {"address": "x_policy.p", "mode": "managed", "type": "x_policy", "change": {"before": {"id": "p1", "resource": "arn:x:::my-bucket/*"}}}
	]}`
	src := `resource "x_bucket" "b" {
  bucket = "my-bucket"
}
resource "x_bucket_acl" "a" {
  bucket = "my-bucket"
}
resource "x_policy" "p" {
  resource = "arn:x:::my-bucket/*"
}
`
	set := rules.NewSet([]rules.Rule{{Source: "x_bucket_acl", Path: "bucket", Target: "x_bucket", Attr: "bucket", Match: rules.MatchExact}})
	var targets []string
	for _, c := range candidates(t, plan, src, link.Options{Rules: set}) {
		if c.Source == "x_policy" {
			targets = append(targets, c.Target)
		}
	}
	if len(targets) != 1 || targets[0] != "x_bucket" {
		t.Errorf("x_policy.resource targets = %v, want [x_bucket]", targets)
	}
}

// Values counts the different values behind a candidate; one plain word
// alone is weak.
func TestCandidateValues(t *testing.T) {
	plan := `{"resource_changes": [
	 {"address": "x_bus.d", "mode": "managed", "type": "x_bus", "change": {"before": {"id": "b1", "name": "default"}}},
	 {"address": "x_group.g", "mode": "managed", "type": "x_group", "change": {"before": {"id": "g1", "label": "default"}}}
	]}`
	src := `resource "x_bus" "d" {
  name = "default"
}
resource "x_group" "g" {
  label = "default"
}
`
	cs := candidates(t, plan, src, link.Options{})
	if len(cs) == 0 {
		t.Fatal("no candidates")
	}
	for _, c := range cs {
		if c.Values != 1 || !c.Weak() {
			t.Errorf("%+v: want one value, weak", c)
		}
	}
	if (Candidate{Values: 1, Examples: []string{"orders-queue"}}).Weak() {
		t.Error("a name with a separator is not weak")
	}
}
