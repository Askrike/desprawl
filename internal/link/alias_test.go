package link

import (
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
)

// A shortened spelling of an identifier must link to an expression that
// evaluates to the literal itself: the full identifier changes the config
// (a task definition's container secret, a service's task revision) and
// verify reports drift.
func TestLinkSpellsAliases(t *testing.T) {
	plan, err := tf.ParsePlan([]byte(`{"resource_changes": [
	 {"address": "x_task.web", "mode": "managed", "type": "x_task", "change": {"before": {
	   "id": "web", "arn": "arn:p:ecs:r:1:task/web:12", "family": "web", "revision": 12}}},
	 {"address": "x_task.job", "mode": "managed", "type": "x_task", "change": {"before": {
	   "id": "j", "arn": "arn:p:ecs:r:1:task/job:3"}}},
	 {"address": "x_secret.env", "mode": "managed", "type": "x_secret", "change": {"before": {
	   "id": "arn:p:sm:r:1:secret:app-env-AbC123", "arn": "arn:p:sm:r:1:secret:app-env-AbC123", "name": "app-env"}}},
	 {"address": "x_service.s", "mode": "managed", "type": "x_service", "change": {"before": {"id": "s"}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	computed := &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{"arn": {Type: []byte(`"string"`), Computed: true}}}
	schemas := &tf.Schemas{Resources: map[string]*tf.SchemaBlock{"x_task": computed, "x_secret": computed}}
	// Run4: an edge made by a docs rule lost the note.
	rs, err := rules.NewSet([]rules.Rule{
		{Source: "x_service", Path: "ruled", Target: "x_task", Attr: "arn", Match: rules.MatchExact, Origin: rules.OriginDocs, Support: 6},
	}).WithAliases([]rules.Alias{
		{Match: `^.*/([^/:\s]+:[0-9]+)$`},
		{Match: `^(arn:.+[:/].+)-[A-Za-z0-9]{6}$`},
	})
	if err != nil {
		t.Fatal(err)
	}
	ix, err := index.Build(plan, schemas, rs)
	if err != nil {
		t.Fatal(err)
	}
	src := `resource "x_service" "s" {
  task   = "web:12"
  ruled  = "web:12"
  job    = "job:3"
  secret = "arn:p:sm:r:1:secret:app-env"
  policy = "arn:p:sm:r:1:secret:app-env:KEY::"
}
`
	res, err := Link([]byte(src), ix, Options{Rules: rs, Params: []Param{{Value: "r", Ref: "data.x_region.current.name"}}})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Config)
	for _, s := range []string{
		`task   = "${x_task.web.family}:${x_task.web.revision}"`,
		`ruled  = "${x_task.web.family}:${x_task.web.revision}"`,
		`job    = "job:3"`, // nothing in the target spells it: left literal
		`secret = "arn:p:sm:${data.x_region.current.name}:1:secret:${x_secret.env.name}"`,
		`policy = "arn:p:sm:${data.x_region.current.name}:1:secret:${x_secret.env.name}:KEY::"`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in\n%s", s, out)
		}
	}
	for _, e := range res.Edges {
		if strings.HasSuffix(e.Ref, ".arn") {
			t.Errorf("alias linked to the full identifier: %+v", e)
		}
		if !strings.Contains(e.Basis, "shortened spelling of") || strings.Contains(e.Basis, "the value is x_") {
			t.Errorf("basis doesn't say the value is a shortened spelling: %q", e.Basis)
		}
	}
}
