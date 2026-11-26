package link

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
)

var update = flag.Bool("update", false, "rewrite golden files")

func linkFixture(t *testing.T, dir string) *Result {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(dir, "generated.tf"))
	if err != nil {
		t.Fatal(err)
	}
	pb, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := tf.ParsePlan(pb)
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := tf.LoadSchemas(filepath.Join(dir, "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rules.Load(filepath.Join(dir, "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Rules: rules.NewSet(rs),
		Params: []Param{
			{Value: "111111111111", Ref: "data.aws_caller_identity.current.account_id", Data: `data "aws_caller_identity" "current" {}`},
			{Value: "eu-central-1", Ref: "data.aws_region.current.region", Data: `data "aws_region" "current" {}`},
		},
		Drop: []string{"tags_all"},
	}
	ix, err := index.Build(plan, schemas, opts.Rules)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Link(src, ix, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestLinkGolden(t *testing.T) {
	dir := "testdata/basic"
	res := linkFixture(t, dir)
	got := string(res.Config) + "\n# --- data.tf ---\n" + string(res.Data)

	golden := filepath.Join(dir, "main.tf.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (run with -update to accept):\n%s", golden, got)
	}
}

func TestLinkCases(t *testing.T) {
	res := linkFixture(t, "testdata/basic")
	out := string(res.Config)

	mustContain := []string{
		// name via rule
		`role       = aws_iam_role.app_lambda.name`,
		// unique ARN
		`role          = aws_iam_role.app_lambda.arn`,
		`event_source_arn = aws_sqs_queue.jobs.arn`,
		`function_name    = aws_lambda_function.app.arn`,
		// prefixed EC2 IDs in nested blocks
		`security_group_ids = [aws_security_group.a.id]`,
		// embedded in a policy, with suffix kept
		`"${aws_s3_bucket.data.arn}/*"`,
		`"${aws_cloudwatch_log_group.aws_lambda_app.arn}:*"`,
		// bucket sub-resource via wildcard rule
		`bucket = aws_s3_bucket.data.bucket`,
		// rule inside jsonencode'd container definitions
		`awslogs-group  = aws_cloudwatch_log_group.aws_lambda_app.name`,
		// account/region parametrization of unmatched ARNs and ECR hosts
		`"arn:aws:logs:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:log-group:/aws/lambda/app-other:*"`,
		`"${data.aws_caller_identity.current.account_id}.dkr.ecr.${data.aws_region.current.region}.amazonaws.com/web:latest"`,
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q", s)
		}
	}

	mustNotContain := []string{
		// a name with no rule for this path must stay literal
		`Name = aws_iam_role`,
		// AWS-managed policy isn't in the index
		`policy_arn = aws_`,
		// tidy
		`tags_all`,
		`= null`,
		`__generated__`,
	}
	for _, s := range mustNotContain {
		if strings.Contains(out, s) {
			t.Errorf("unexpected %q", s)
		}
	}

	// sg a <-> sg b is a cycle; exactly one direction must be left literal.
	if len(res.CycleBreaks) != 1 {
		t.Errorf("cycle breaks = %v, want 1", res.CycleBreaks)
	}
	refsA := strings.Contains(out, `security_groups = [aws_security_group.b.id]`)
	refsB := strings.Contains(out, `security_groups = [aws_security_group.a.id]`)
	if refsA == refsB {
		t.Errorf("expected exactly one SG direction linked, a->b=%v b->a=%v", refsA, refsB)
	}
}

// Names repeat across containers (namespaces): a selector matched per map
// key, a name inside a DNS string, and the choice among same-named targets
// settled by the container both reference.
func TestNamesInContainers(t *testing.T) {
	res := func(typ, name, ns string, extra string) string {
		return `{"address":"` + typ + `.` + name + `","mode":"managed","type":"` + typ + `","name":"` + name + `","change":{"before":{"id":"` + ns + `/` + name + `","meta":[{"name":"` + strings.Split(name, "_")[0] + `","namespace":"` + ns + `"}]` + extra + `}}}`
	}
	labels := `,"labels":{"app.kubernetes.io/name":"redis"}`
	plan, err := tf.ParsePlan([]byte(`{"resource_changes":[` + strings.Join([]string{
		`{"address":"x_ns.a","mode":"managed","type":"x_ns","name":"a","change":{"before":{"id":"a"}}}`,
		`{"address":"x_ns.b","mode":"managed","type":"x_ns","name":"b","change":{"before":{"id":"b"}}}`,
		res("x_service", "redis_a", "a", ""),
		res("x_service", "redis_b", "b", ""),
		res("x_app", "redis_a", "a", labels),
		res("x_app", "redis_b", "b", labels),
		res("x_app", "web", "a", ""),
	}, ",") + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`
resource "x_service" "redis_a" {
  meta {
    name      = "redis"
    namespace = "a"
  }
  selector = { "app.kubernetes.io/name" = "redis" }
}
resource "x_app" "redis_a" {
  meta {
    name      = "redis"
    namespace = "a"
  }
}
resource "x_app" "redis_b" {
  meta {
    name      = "redis"
    namespace = "b"
  }
}
resource "x_app" "web" {
  meta {
    name      = "web"
    namespace = "a"
  }
  env = "redis.a.svc.cluster.local:6379"
}
`)
	rs := rules.NewSet([]rules.Rule{
		{Source: "x_service", Path: "meta.namespace", Target: "x_ns", Attr: "id", Match: rules.MatchExact},
		{Source: "x_app", Path: "meta.namespace", Target: "x_ns", Attr: "id", Match: rules.MatchExact},
		{Source: "x_service", Path: "selector.*", Target: "x_app", Attr: "labels.*", Match: rules.MatchExact},
		{Source: "x_app", Path: "env", Target: "x_service", Attr: "meta.0.name", Match: rules.MatchEmbedded},
	})
	ix, err := index.Build(plan, nil, rs)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Link(src, ix, Options{Rules: rs})
	if err != nil {
		t.Fatal(err)
	}
	got := string(r.Config)
	for _, want := range []string{
		`"app.kubernetes.io/name" = x_app.redis_a.labels["app.kubernetes.io/name"]`,
		`env = "${x_service.redis_a.meta[0].name}.a.svc.cluster.local:6379"`,
	} {
		if !strings.Contains(strings.Join(strings.Fields(got), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
}

// An embedded rule finds a name after a prefix in the same word
// ("@hook-my-pager"), but not one that ends inside a word.
func TestEmbeddedAfterPrefix(t *testing.T) {
	plan, err := tf.ParsePlan([]byte(`{"resource_changes":[
	 {"address":"x_hook.h","mode":"managed","type":"x_hook","name":"h","change":{"before":{"id":"my-pager","name":"my-pager"}}},
	 {"address":"x_alert.a","mode":"managed","type":"x_alert","name":"a","change":{"before":{"id":"1","message":"Down. @hook-my-pager","other":"Down. @hook-my-pagers"}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`
resource "x_alert" "a" {
  message = "Down. @hook-my-pager"
  other   = "Down. @hook-my-pagers"
}
`)
	rs := rules.NewSet([]rules.Rule{
		{Source: "x_alert", Path: "message", Target: "x_hook", Attr: "name", Match: rules.MatchEmbedded},
		{Source: "x_alert", Path: "other", Target: "x_hook", Attr: "name", Match: rules.MatchEmbedded},
	})
	ix, err := index.Build(plan, nil, rs)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Link(src, ix, Options{Rules: rs})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Fields(string(r.Config)), " ")
	if want := `message = "Down. @hook-${x_hook.h.name}"`; !strings.Contains(got, want) {
		t.Errorf("missing %s in:\n%s", want, r.Config)
	}
	if want := `other = "Down. @hook-my-pagers"`; !strings.Contains(got, want) {
		t.Errorf("missing %s in:\n%s", want, r.Config)
	}
}

// A "$" right before a reference must not escape it into literal text.
func TestQuoteDollarBeforeRef(t *testing.T) {
	got := quote([]part{{lit: "cost$"}, {ref: "x_a.b.id"}})
	if want := `"cost${"$"}${x_a.b.id}"`; got != want {
		t.Errorf("quote = %s, want %s", got, want)
	}
}

// A cycle is cut at its weak pair (to a non-id attribute), whichever pair
// the search happens to close it with.
func TestBackEdgesCutsWeakest(t *testing.T) {
	for _, c := range []struct{ parent, child string }{{"x_a.p", "x_b.c"}, {"x_b.p", "x_a.c"}} {
		edges := []Edge{
			{From: c.child, Ref: c.parent + ".id", to: c.parent},  // child.parent_id = parent.id
			{From: c.parent, Ref: c.child + ".name", to: c.child}, // parent.name = child's computed copy
		}
		got := backEdges(edges)
		if want := [2]string{c.parent, c.child}; len(got) != 1 || got[0] != want {
			t.Errorf("parent %s: cut %v, want %v", c.parent, got, want)
		}
	}
}

// Some providers keep a parent's numeric id in a child's number attribute:
// a rule links it, whether the target keeps the id as a string or a number.
// A number no rule covers stays literal, even when it equals an id.
func TestNumbersByRule(t *testing.T) {
	plan, err := tf.ParsePlan([]byte(`{"resource_changes":[
{"address":"x_instance.main","mode":"managed","type":"x_instance","name":"main","change":{"before":{"id":"245738","vpc_id":91}}},
{"address":"x_vpc.main","mode":"managed","type":"x_vpc","name":"main","change":{"before":{"id":"vpc-91","vpc_id":91}}},
{"address":"x_alarm.cpu","mode":"managed","type":"x_alarm","name":"cpu","change":{"before":{"id":"7","instance_id":245738,"threshold":245738}}}
]}`))
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`
resource "x_instance" "main" {
  vpc_id = 91
}
resource "x_alarm" "cpu" {
  instance_id = 245738
  threshold   = 245738
}
`)
	rs := rules.NewSet([]rules.Rule{
		{Source: "x_alarm", Path: "instance_id", Target: "x_instance", Attr: "id", Match: rules.MatchExact},
		{Source: "x_instance", Path: "vpc_id", Target: "x_vpc", Attr: "vpc_id", Match: rules.MatchExact},
	})
	ix, err := index.Build(plan, nil, rs)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Link(src, ix, Options{Rules: rs})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Fields(string(r.Config)), " ")
	for _, want := range []string{
		`instance_id = x_instance.main.id`,
		`vpc_id = x_vpc.main.vpc_id`,
		`threshold = 245738`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, r.Config)
		}
	}
}
