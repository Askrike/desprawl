package discover

import (
	"testing"

	"github.com/askrike/desprawl/internal/tf"
)

func TestCollapseVersions(t *testing.T) {
	rs := []tf.ListResult{
		{Identity: map[string]any{"family": "web", "revision": float64(2)}},
		{Identity: map[string]any{"family": "web", "revision": float64(10)}},
		{Identity: map[string]any{"family": "api", "revision": float64(1)}},
		{Identity: map[string]any{"arn": "arn:x:task-definition/jobs:3"}},
		{Identity: map[string]any{"arn": "arn:x:task-definition/jobs:12"}},
		{Identity: map[string]any{"id": "sg-0abc123"}}, // trailing digits without a separator aren't a revision
		{Identity: map[string]any{"id": "sg-0abc124"}},
		{Identity: map[string]any{"network_acl_id": "acl-1", "rule_number": float64(100)}},
		{Identity: map[string]any{"network_acl_id": "acl-1", "rule_number": float64(200)}},
		{Identity: map[string]any{"route_table_id": "rtb-1", "destination_cidr_block": "10.0.0.0/16"}}, // CIDRs aren't revisions
		{Identity: map[string]any{"route_table_id": "rtb-1", "destination_cidr_block": "10.0.0.0/24"}},
		{Identity: map[string]any{"route_table_id": "rtb-1", "destination_ipv6_cidr_block": "::/0"}},
		{Identity: map[string]any{"route_table_id": "rtb-1", "destination_ipv6_cidr_block": "::/1"}},
	}
	got, older := collapseVersions(rs)
	if dropped := len(older); dropped != 2 {
		t.Fatalf("dropped %d, want 2: %v", dropped, got)
	}
	want := map[string]bool{"web/10": false, "api/1": false, "jobs:12": false}
	for _, r := range got {
		switch {
		case r.Identity["family"] == "web" && r.Identity["revision"] == float64(10):
			want["web/10"] = true
		case r.Identity["family"] == "api":
			want["api/1"] = true
		case r.Identity["arn"] == "arn:x:task-definition/jobs:12":
			want["jobs:12"] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("missing %s", k)
		}
	}
}

// One full block is truncation, however few the others found.
func TestTruncated(t *testing.T) {
	var rs []tf.ListResult
	for range 3 {
		rs = append(rs, tf.ListResult{Block: "list.x.q0"})
	}
	rs = append(rs, tf.ListResult{Block: "list.x.q1"})
	if !truncated(rs, 3) {
		t.Error("q0 reached the limit: want truncated")
	}
	if truncated(rs, 4) {
		t.Error("no block reached 4: want not truncated")
	}
}
