package link

import (
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
)

// generate writes the owner's reference for a sensitive attribute whose
// value is another resource's identifier; link records it as an edge, and
// if it closes a cycle puts the placeholder back rather than a value it
// never had.
func TestLinkRecordsSensitiveReferences(t *testing.T) {
	plan, err := tf.ParsePlan([]byte(`{"resource_changes": [
	 {"address": "x_net.main", "mode": "managed", "type": "x_net", "change": {"before": {"id": "net-0a1b2c3d4e"}}},
	 {"address": "x_param.net", "mode": "managed", "type": "x_param", "change": {"before": {"id": "/p/net-0f9e8d7c6b"}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	rs := rules.NewSet(nil)
	ix, err := index.Build(plan, &tf.Schemas{}, rs)
	if err != nil {
		t.Fatal(err)
	}
	src := `resource "x_net" "main" {
  name = "main"
}

resource "x_param" "net" {
  value = x_net.main.id
}
`
	res, err := Link([]byte(src), ix, Options{Rules: rs})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Config), "value = x_net.main.id") {
		t.Errorf("reference lost:\n%s", res.Config)
	}
	if len(res.Edges) != 1 || res.Edges[0].From != "x_param.net" || res.Edges[0].Ref != "x_net.main.id" || res.Edges[0].Value != "(sensitive)" {
		t.Fatalf("edges = %+v", res.Edges)
	}

	// The owner refers back: the cycle is broken at the sensitive
	// attribute, which gets the placeholder, never a value.
	cyclic := strings.Replace(src, `name = "main"`, `param = "/p/net-0f9e8d7c6b"`, 1)
	res, err = Link([]byte(cyclic), ix, Options{Rules: rs})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Config)
	if strings.Contains(out, "x_net.main.id") && strings.Contains(out, "x_param.net.id") {
		t.Errorf("cycle left in:\n%s", out)
	}
	if !strings.Contains(out, "x_net.main.id") && (!strings.Contains(out, "REDACTED") || !strings.Contains(out, "ignore_changes")) {
		t.Errorf("broken sensitive reference must get the placeholder:\n%s", out)
	}
	if strings.Contains(out, "net-0a1b2c3d4e") {
		t.Errorf("sensitive value written:\n%s", out)
	}
}
