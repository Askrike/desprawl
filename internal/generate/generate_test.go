package generate

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/askrike/desprawl/internal/hclx"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

func TestRepair(t *testing.T) {
	const record = `resource "aws_route53_record" "r" {
  name    = "app.example.com"
  records = []
  ttl     = 0
  multivalue_answer_routing_policy = false
  alias {
    name    = "lb.example.com"
    zone_id = "Z1"
  }
}
`
	const rule = `resource "aws_lb_listener_rule" "r" {
  priority = 10
  action {
    type = "forward"
    forward {
      stickiness {
        duration = 0
        enabled  = false
      }
    }
  }
  target_failover {
    on_unhealthy = null
  }
}
`
	const param = `resource "aws_ssm_parameter" "p" {
  name = "/app/token"
  type = "SecureString"
}
`
	schema := &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{
		"value":          {Type: []byte(`"string"`), Optional: true, Sensitive: true},
		"insecure_value": {Type: []byte(`"string"`), Optional: true},
		"value_wo":       {Type: []byte(`"string"`), Optional: true, Sensitive: true},
	}}
	cases := []struct {
		name     string
		src      string
		schema   *tf.SchemaBlock
		diag     tf.Diagnostic
		gone     []string // paths that must be removed
		intact   []string
		check    string // substring the result must contain
		fails    bool   // no repair possible
		identity map[string]any
		owned    func(string) bool
	}{
		{
			name: "conflict removes the empty side",
			src:  record,
			diag: tf.Diagnostic{Summary: "Invalid combination of arguments", Detail: "\"alias\": only one of `alias,records` can be specified, but `alias,records` were specified."},
			gone: []string{"records"}, intact: []string{"alias"},
		},
		{
			name: "conflicts with, unquoted",
			src:  record,
			diag: tf.Diagnostic{Summary: "Conflicting configuration arguments", Detail: "\"alias\": conflicts with ttl"},
			gone: []string{"ttl"}, intact: []string{"alias"},
		},
		{
			// Run4/5: which side survived depended on which error came first.
			name:  "conflict between two set sides keeps the one naming a resource",
			src:   "resource \"aws_autoscaling_group\" \"g\" {\n  availability_zones = [\"eu-central-1a\"]\n  vpc_zone_identifier = [\"subnet-1\"]\n}\n",
			diag:  tf.Diagnostic{Summary: "Conflicting configuration arguments", Detail: "\"availability_zones\": conflicts with vpc_zone_identifier"},
			owned: func(s string) bool { return s == "subnet-1" },
			gone:  []string{"availability_zones"}, intact: []string{"vpc_zone_identifier"},
		},
		{
			name:  "conflict between two set sides, reported the other way round",
			src:   "resource \"aws_autoscaling_group\" \"g\" {\n  availability_zones = [\"eu-central-1a\"]\n  vpc_zone_identifier = [\"subnet-1\"]\n}\n",
			diag:  tf.Diagnostic{Summary: "Conflicting configuration arguments", Detail: "\"vpc_zone_identifier\": conflicts with availability_zones"},
			owned: func(s string) bool { return s == "subnet-1" },
			gone:  []string{"availability_zones"}, intact: []string{"vpc_zone_identifier"},
		},
		{
			name: "all of: remove the one that is set",
			src:  record,
			diag: tf.Diagnostic{Summary: "Missing required argument", Detail: "\"multivalue_answer_routing_policy\": all of `multivalue_answer_routing_policy,set_identifier` must be specified"},
			gone: []string{"multivalue_answer_routing_policy"}, intact: []string{"alias", "name"},
		},
		{
			// Run3: placeholders for the unset ones looped until the subnet was dropped.
			name: "all of: remove the zero value, no placeholders for the rest",
			src: `resource "aws_subnet" "s" {
  cidr_block                      = "10.0.1.0/24"
  map_customer_owned_ip_on_launch = false
  vpc_id                          = "vpc-1"
}
`,
			schema: &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{
				"customer_owned_ipv4_pool":        {Type: []byte(`"string"`), Optional: true},
				"map_customer_owned_ip_on_launch": {Type: []byte(`"bool"`), Optional: true},
				"outpost_arn":                     {Type: []byte(`"string"`), Optional: true},
			}},
			diag: tf.Diagnostic{Summary: "Missing required argument", Detail: "\"map_customer_owned_ip_on_launch\": all of `customer_owned_ipv4_pool,map_customer_owned_ip_on_launch,outpost_arn` must be specified"},
			gone: []string{"map_customer_owned_ip_on_launch", "customer_owned_ipv4_pool", "outpost_arn"}, intact: []string{"cidr_block", "vpc_id"},
		},
		{
			name: "nested out-of-range zero",
			src:  rule,
			diag: tf.Diagnostic{Summary: "Invalid value", Detail: "expected action.0.forward.0.stickiness.0.duration to be in the range (1 - 604800), got 0"},
			gone: []string{"action.0.forward.0.stickiness.0.duration"}, intact: []string{"action.0.forward.0.stickiness", "priority"},
		},
		{
			name: "nested block missing a required field",
			src:  rule,
			diag: tf.Diagnostic{Summary: "Missing required argument", Detail: `The argument "target_failover.0.on_deregistration" is required, but no definition was found.`},
			gone: []string{"target_failover"}, intact: []string{"action"},
		},
		{
			name: "computed attribute in a nested block",
			src: `resource "azurerm_virtual_network" "v" {
  name = "vnet"
  subnet {
    id   = "/subscriptions/s/resourceGroups/g/providers/Microsoft.Network/virtualNetworks/vnet/subnets/a"
    name = "a"
  }
}
`,
			diag: tf.Diagnostic{Summary: "Value for unconfigurable attribute", Detail: `Can't configure a value for "subnet.0.id": its value will be decided automatically based on the result of applying this configuration.`},
			gone: []string{"subnet.0.id"}, intact: []string{"subnet.0.name"},
		},
		{
			name:   "computed attribute inside an attribute value",
			src:    "resource \"azurerm_virtual_network\" \"v\" {\n  name   = \"vnet\"\n  subnet = [{ id = \"/s/a\", name = \"a\" }]\n}\n",
			schema: &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{"subnet": {Optional: true, Computed: true}}},
			diag:   tf.Diagnostic{Summary: "Value for unconfigurable attribute", Detail: `Can't configure a value for "subnet.0.id": its value will be decided automatically based on the result of applying this configuration.`},
			check:  "subnet = null", intact: []string{"name"},
		},
		{
			name: "message only in the summary",
			src:  strings.Replace(rule, "priority = 10", "priority = 0", 1),
			diag: tf.Diagnostic{Summary: "expected priority to be at least (1), got 0"},
			gone: []string{"priority"}, intact: []string{"action"},
		},
		{
			name: "meaningful invalid values are not removed",
			src: `resource "aws_kms_alias" "a" {
  name = "alias/aws/ssm"
}
`,
			diag:   tf.Diagnostic{Summary: `"name" cannot begin with reserved AWS CMK prefix 'alias/aws/'`},
			intact: []string{"name"},
			fails:  true,
		},
		{
			name:     "missing value taken from identity",
			src:      "resource \"aws_route\" \"r\" {\n  route_table_id = \"rtb-1\"\n}\n",
			identity: map[string]any{"route_table_id": "rtb-1", "destination_prefix_list_id": "pl-1", "destination_cidr_block": nil},
			diag:     tf.Diagnostic{Summary: "Invalid combination of arguments", Detail: "\"destination_cidr_block\": one of `destination_cidr_block,destination_ipv6_cidr_block,destination_prefix_list_id` must be specified"},
			check:    `destination_prefix_list_id = "pl-1"`,
		},
		{
			name:   "sensitive value gets a placeholder",
			src:    param,
			schema: schema,
			diag:   tf.Diagnostic{Summary: "Invalid combination of arguments", Detail: "\"value\": one of `insecure_value,value,value_wo` must be specified"},
			intact: []string{"value", "name"},
			check:  "ignore_changes = [value]",
		},
		{
			name:   "url returned without its scheme",
			src:    "resource \"aws_iam_openid_connect_provider\" \"p\" {\n  url = \"token.example.com\"\n}\n",
			diag:   tf.Diagnostic{Summary: `expected "url" to have a host, got token.example.com`},
			check:  `url = "https://token.example.com"`,
			intact: []string{"url"},
		},
		{
			name: "empty url is removed",
			src:  "resource \"aws_iam_openid_connect_provider\" \"p\" {\n  url = \"\"\n  arn = \"arn:x:y\"\n}\n",
			diag: tf.Diagnostic{Summary: `expected "url" to have a host, got `},
			gone: []string{"url"}, intact: []string{"arn"},
		},
		{
			name: "enum one of on an empty value removes it",
			src:  "resource \"aws_s3_bucket_versioning\" \"v\" {\n  bucket = \"b\"\n  status = \"\"\n}\n",
			diag: tf.Diagnostic{Summary: "Invalid value", Detail: `expected status to be one of ["Enabled" "Disabled"], got `},
			gone: []string{"status"}, intact: []string{"bucket"},
		},
		{
			name: "one of: placeholder for the first in the list, not the one named first",
			src:  "resource \"aws_lambda_function\" \"f\" {\n  function_name = \"f\"\n  image_uri = null\n}\n",
			schema: &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{
				"filename":  {Type: []byte(`"string"`), Optional: true},
				"image_uri": {Type: []byte(`"string"`), Optional: true},
				"s3_bucket": {Type: []byte(`"string"`), Optional: true},
			}},
			diag:  tf.Diagnostic{Summary: "Invalid combination of arguments", Detail: "\"image_uri\": one of `filename,image_uri,s3_bucket` must be specified"},
			check: "ignore_changes = [filename]",
		},
		{
			// Run4: main route-table associations got a made-up gateway.
			name: "one of: no placeholder for an attribute naming another resource",
			src:  "resource \"aws_route_table_association\" \"a\" {\n  route_table_id = \"rtb-1\"\n  gateway_id = null\n  subnet_id = null\n}\n",
			schema: &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{
				"gateway_id": {Type: []byte(`"string"`), Optional: true},
				"subnet_id":  {Type: []byte(`"string"`), Optional: true},
			}},
			diag:  tf.Diagnostic{Summary: "Invalid combination of arguments", Detail: "\"gateway_id\": one of `gateway_id,subnet_id` must be specified"},
			fails: true,
		},
		{
			name: "required attribute the API doesn't return",
			src:  "resource \"aws_key_pair\" \"k\" {\n  key_name = \"k\"\n}\n",
			schema: &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{
				"public_key": {Type: []byte(`"string"`), Required: true},
			}},
			diag:  tf.Diagnostic{Summary: "Missing required argument", Detail: `The argument "public_key" is required, but no definition was found.`},
			check: "ignore_changes = [public_key]",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, diags := hclwrite.ParseConfig([]byte(c.src), "x.tf", hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags)
			}
			b := f.Body().Blocks()[0].Body()
			if _, ok := repair(b, c.schema, c.identity, c.owned, c.diag); ok == c.fails {
				t.Fatalf("repaired = %v, want %v", ok, !c.fails)
			}
			for _, p := range c.gone {
				if n, ok := resolve(b, p); ok && !isNullNode(n) {
					t.Errorf("%s still set", p)
				}
			}
			for _, p := range c.intact {
				if n, ok := resolve(b, p); !ok || isNullNode(n) {
					t.Errorf("%s was removed", p)
				}
			}
			if out := string(hclwrite.Format(f.Bytes())); !strings.Contains(out, c.check) {
				t.Errorf("output lacks %q:\n%s", c.check, out)
			}
		})
	}
}

func TestScrub(t *testing.T) {
	in := `{"format_version":"1.2","resource_changes":[{"address":"a.b","change":{
		"before":{"name":"db","password":"hunter2","nested":[{"token":"t","keep":"k"}]},
		"before_sensitive":{"password":true,"nested":[{"token":true}]},
		"after":{"name":"db","password":"hunter2"},
		"after_sensitive":{"password":true}}}],
		"prior_state":{"values":{"password":"hunter2"}}}`
	out, err := Scrub([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "hunter2") || strings.Contains(string(out), `"t"`) {
		t.Errorf("sensitive values survived: %s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"keep": "k"`) || !strings.Contains(string(out), `"name": "db"`) {
		t.Errorf("non-sensitive values lost: %s", out)
	}
}

func TestDefaultedPaths(t *testing.T) {
	plan := `{"resource_changes": [{"address": "x.c", "mode": "managed", "type": "x", "change": {"actions": ["update"],
	  "before": {"name": "c", "net": [{"lb": [{"idle": 0, "count": 2}]}], "pools": "", "tags": {"k8s.io/x": ""}},
	  "after":  {"name": "c", "net": [{"lb": [{"idle": 30, "count": 3}]}], "pools": "Auto", "tags": {"k8s.io/x": "y"}}}}]}`
	got, err := defaultedPaths([]byte(plan))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"net[0].lb[0].idle", "pools"}
	if fmt.Sprint(got["x.c"]) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got["x.c"], want)
	}
}

// Diagnostics about different attributes of one resource are repaired in the
// same round; two about the same attributes (both sides of one conflict) are
// not. A target without generated config is dropped before any plan.
func TestFixRound(t *testing.T) {
	ws := model.Workspace{Dir: t.TempDir()}
	const src = `resource "x_group" "g" {
  name        = "g"
  name_prefix = ""
  sg_names    = []
  sg_ids      = ["sg-1"]
  port        = 0
  health {
  }
}
`
	if err := os.WriteFile(ws.Path(generatedFile), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	targets := []model.Target{{Type: "x_group", Name: "g", ID: "g"}, {Type: "x_group", Name: "gone", ID: "gone"}}
	g := Generator{WS: ws, Schemas: &tf.Schemas{Resources: map[string]*tf.SchemaBlock{}}}

	// The generating plan's diagnostics: one per import block (lines 1-3
	// and 5-7), and one naming the dropped target.
	planDiags := []tf.Diagnostic{
		{Summary: "Cannot import", Range: &tf.Range{Filename: "imports.tf", Start: tf.Pos{Line: 2}}},
		{Summary: "Cannot import", Range: &tf.Range{Filename: "imports.tf", Start: tf.Pos{Line: 6}}},
		{Summary: "Import failed", Detail: `"x_group.gone" was deleted`},
	}
	targets, dropped, rest, err := g.dropUnconfigured(targets, planDiags, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || len(dropped) != 1 || dropped[0].Name != "gone" {
		t.Fatalf("kept %v, dropped %v", targets, dropped)
	}
	if want := `no config generated: Cannot import; Import failed: "x_group.gone" was deleted`; dropped[0].Reason != want {
		t.Errorf("reason %q, want %q", dropped[0].Reason, want)
	}
	if len(rest) != 1 || rest[0].Range.Start.Line != 2 {
		t.Fatalf("diagnostics kept: %v", rest)
	}
	imp, err := os.ReadFile(ws.Path("imports.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if a := hclx.ImportTarget(imp, 2); a != "x_group.g" || strings.Contains(string(imp), "gone") {
		t.Errorf("imports.tf lost line 2's block or kept the dropped one:\n%s", imp)
	}

	conflict := func(a, b string) tf.Diagnostic {
		return tf.Diagnostic{Address: "x_group.g", Summary: "Conflicting configuration arguments", Detail: fmt.Sprintf("%q: conflicts with %s", a, b)}
	}
	diags := []tf.Diagnostic{
		conflict("name", "name_prefix"),
		conflict("name_prefix", "name"),
		conflict("sg_names", "sg_ids"),
		{Address: "x_group.g", Summary: "Invalid value", Detail: `"port" must be between 1 and 65535`},
		{Address: "x_group.g", Summary: "Missing required argument", Detail: `"health.0.path" is required`},
	}
	history := map[string]map[string]bool{}
	kept, repaired, dropped, err := g.fix(t.Context(), targets, diags, history)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || len(dropped) != 0 {
		t.Fatalf("kept %v, dropped %v", kept, dropped)
	}
	if !slices.Equal(repaired, []string{"x_group.g"}) {
		t.Errorf("repaired = %v, want [x_group.g]", repaired)
	}
	if len(history["x_group.g"]) != 4 {
		t.Errorf("repairs = %v, want 4", history["x_group.g"])
	}
	out, err := os.ReadFile(ws.Path(generatedFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`name        = "g"`, `sg_ids      = ["sg-1"]`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("lost %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "health") {
		t.Errorf("health block kept:\n%s", out)
	}
}

// An error no built-in repair handles drops the resource, unless another
// error of it was repaired this round (the next plan may settle it), and
// the dropped one carries a hint: what its config sets and the override
// the error points at.
func TestFixRoundHints(t *testing.T) {
	ws := model.Workspace{Dir: t.TempDir()}
	const src = `resource "x_group" "g" {
  name = "g"
  port = 0
  tier = "gold"
}

resource "x_group" "h" {
  name = "h"
  zone = "z1"
  rule {
  }
}
`
	if err := os.WriteFile(ws.Path(generatedFile), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws.Path("imports.tf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	targets := []model.Target{{Type: "x_group", Name: "g", ID: "g"}, {Type: "x_group", Name: "h", ID: "h"}}
	g := Generator{WS: ws, Schemas: &tf.Schemas{Resources: map[string]*tf.SchemaBlock{}}}
	odd := func(addr, attr string) tf.Diagnostic {
		return tf.Diagnostic{Address: addr, Summary: "Unavailable", Detail: fmt.Sprintf("%q isn't offered here", attr)}
	}
	diags := []tf.Diagnostic{
		{Address: "x_group.g", Summary: "Invalid value", Detail: `"port" must be between 1 and 65535`},
		odd("x_group.g", "tier"),
		odd("x_group.h", "zone"),
	}
	kept, _, dropped, err := g.fix(t.Context(), targets, diags, map[string]map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].Name != "g" || len(dropped) != 1 {
		t.Fatalf("kept %v, dropped %v", kept, dropped)
	}
	h := dropped[0].Hint
	if h == nil || h.Override == nil || !slices.Equal(h.Override.Remove, []string{"zone"}) {
		t.Fatalf("hint = %+v, want remove zone", h)
	}
	if want := []string{"name", "rule.0 (empty)", "zone"}; !slices.Equal(h.Set, want) {
		t.Errorf("set = %q, want %q", h.Set, want)
	}
}

// An override removes a nested attribute or block by its path.
func TestOverrideRemovesNested(t *testing.T) {
	ws := model.Workspace{Dir: t.TempDir()}
	const src = `resource "x_group" "g" {
  name = "g"
  rule {
    port   = 1
    filter = "a"
  }
  rule {
    port = 2
  }
}
`
	if err := os.WriteFile(ws.Path(generatedFile), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	g := Generator{WS: ws}
	if err := g.applyOverrides(map[string]Override{"x_group.g": {Remove: []string{"rule.0.filter", "rule.1"}}}); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(ws.Path(generatedFile))
	if got := strings.Join(strings.Fields(string(out)), " "); strings.Contains(got, "filter") || strings.Contains(got, "port = 2") || !strings.Contains(got, "port = 1") {
		t.Errorf("got:\n%s", out)
	}
	if err := g.applyOverrides(map[string]Override{"x_group.g": {Remove: []string{"rule.5"}}}); err == nil {
		t.Error("removing a block that isn't there: no error")
	}
}

// A target found by inventory has no identity for a Skip to match before
// generation; its config shows the match, so an error in it is skipped,
// not repaired.
func TestFixRoundSkips(t *testing.T) {
	ws := model.Workspace{Dir: t.TempDir()}
	const src = `resource "x_group" "def" {
  name = "default.v1"
  port = 0
}

resource "x_group" "own" {
  name = "own"
  port = 0
}
`
	if err := os.WriteFile(ws.Path(generatedFile), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws.Path("imports.tf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	targets := []model.Target{{Type: "x_group", Name: "def", ID: "default.v1", Origin: "inventory"}, {Type: "x_group", Name: "own", ID: "own"}}
	g := Generator{WS: ws, Schemas: &tf.Schemas{Resources: map[string]*tf.SchemaBlock{}},
		Skip: []Skip{{Type: "x_group", Attr: "name", Match: `^default\.`, Reason: "provider-owned"}}}
	port := func(addr string) tf.Diagnostic {
		return tf.Diagnostic{Address: addr, Summary: "Invalid value", Detail: `"port" must be between 1 and 65535`}
	}
	kept, repaired, dropped, err := g.fix(t.Context(), targets, []tf.Diagnostic{port("x_group.def"), port("x_group.own")}, map[string]map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].Name != "own" || len(dropped) != 1 || dropped[0].Reason != skipReason+"provider-owned" {
		t.Fatalf("kept %v, dropped %v", kept, dropped)
	}
	if !slices.Equal(repaired, []string{"x_group.own"}) {
		t.Errorf("repaired = %v, want only the kept resource", repaired)
	}
}
