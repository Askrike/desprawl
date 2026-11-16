package tfmap

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/askrike/desprawl/providers"
)

var update = flag.Bool("update", false, "rewrite testdata/golden from the current output")

// TestCatalogOverlay: a workspace pack's catalog adds to and replaces
// built-in entries type by type, keeping the rest.
func TestCatalogOverlay(t *testing.T) {
	extra := fstest.MapFS{"aws/catalog.json": {Data: []byte(`{"provider": "aws", "resources": {
		"aws_s3_bucket": {"role": "satellite"},
		"aws_made_up_thing": {"role": "primary", "label": "Thing"}}}`)}}
	c, err := LoadCatalog(providers.Catalogs, extra)
	if err != nil {
		t.Fatal(err)
	}
	for typ, want := range map[string]string{"aws_s3_bucket": "satellite", "aws_made_up_thing": "primary", "aws_lambda_function": "primary"} {
		if got := c.resources[typ].Role; got != want {
			t.Errorf("%s: role %q, want %q", typ, got, want)
		}
	}
}

// TestExamples pins the architecture view of every example: its components
// and connections. A change in folding, catalogs or routing that alters any
// example shows up as a diff here; when the change is intended, review the
// diff and run `go test -update`.
func TestExamples(t *testing.T) {
	cat, err := LoadCatalog(providers.Catalogs)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range examples() {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			g, err := Parse(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(g.Diags) > 0 {
				t.Errorf("parse diagnostics: %v", g.Diags)
			}
			got := goldenText(Abstract(g, cat))

			path := filepath.Join("testdata", "golden", name+".txt")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("architecture changed (go test -update if intended)\n--- want\n%s\n--- got\n%s", want, got)
			}
		})
	}
}

// goldenText renders an architecture view as stable, readable text.
func goldenText(a Arch) string {
	var lines []string
	for _, n := range a.Nodes {
		lines = append(lines, "component "+n.Label+" "+n.ID)
	}
	sort.Strings(lines)
	var edges []string
	for _, e := range a.Edges {
		arrow := " -> "
		if e.Both {
			arrow = " <-> "
		}
		edges = append(edges, "connection "+e.From+arrow+e.To+" ["+e.Label+"]")
	}
	sort.Strings(edges)
	return strings.Join(append(lines, edges...), "\n") + "\n"
}

// TestSeedLayouts checks that every example's seed layout places each
// component in its own cell and keeps modules out of each other's boxes.
func TestSeedLayouts(t *testing.T) {
	cat, err := LoadCatalog(providers.Catalogs)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range examples() {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			g, err := Parse(dir)
			if err != nil {
				t.Fatal(err)
			}
			a := Abstract(g, cat)
			m := measure(a, Seed(a))
			if len(m.Overlaps)+len(m.Intrusions)+len(m.Unplaced) > 0 {
				t.Errorf("overlaps %v, intrusions %v, unplaced %v", m.Overlaps, m.Intrusions, m.Unplaced)
			}
		})
	}
}

// examples lists the example directories, skipping symlinked ones (private
// repos, whose contents change outside this project).
func examples() []string {
	var out []string
	dirs, _ := filepath.Glob("../../examples/*")
	for _, dir := range dirs {
		if st, err := os.Lstat(dir); err == nil && st.Mode()&os.ModeSymlink == 0 && st.IsDir() {
			out = append(out, dir)
		}
	}
	return out
}

// A config's files and its subdirectories interleave in the walk; each
// directory is still listed once.
func TestConfigDirs(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"c/a.tf", "c/b/m.tf", "c/z.tf"} {
		path := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(configDirs(root), " "); got != "c c/b" {
		t.Errorf("configDirs = %q, want \"c c/b\"", got)
	}
}

// Config names use / on every OS, however the path came in.
func TestConfigNameIn(t *testing.T) {
	ws := t.TempDir()
	for _, path := range []string{filepath.Join(ws, "infra", "prod"), filepath.Join(ws, "infra", ".", "x", "..", "prod")} {
		if got, err := ConfigNameIn(ws, path); err != nil || got != "infra/prod" {
			t.Errorf("ConfigNameIn(%q) = %q, %v; want infra/prod", path, got, err)
		}
	}
	if got, _ := ConfigNameIn(ws, filepath.Dir(ws)); got != ".." {
		t.Errorf("parent = %q, want ..", got)
	}
}

// each.value reaches what the block's for_each iterates over.
func TestForEachReferences(t *testing.T) {
	dir := t.TempDir()
	src := `
resource "aws_sqs_queue" "q" {
  for_each = toset(["a", "b"])
  name     = each.key
}
resource "aws_lambda_function" "f" {
  function_name = "f"
}
resource "aws_lambda_event_source_mapping" "m" {
  for_each         = aws_sqs_queue.q
  event_source_arn = each.value.arn
  function_name    = aws_lambda_function.f.arn
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := LoadCatalog(providers.Catalogs)
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := goldenText(Abstract(g, cat))
	if want := "connection aws_sqs_queue.q -> aws_lambda_function.f [triggers]"; !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}

// A registry module installed by `terraform init` is expanded like a local
// one, found through .terraform/modules/modules.json.
func TestInstalledModules(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"main.tf": `
module "app" {
  source = "./app"
}
resource "aws_lambda_function" "f" {
  function_name = "f"
  environment {
    variables = { TABLE = module.app.table }
  }
}`,
		"app/main.tf": `
module "db" {
  source  = "terraform-aws-modules/dynamodb-table/aws"
  version = "4.0.0"
  name    = "orders"
}
output "table" { value = module.db.arn }`,
		".terraform/modules/app.db/main.tf": `
variable "name" {}
resource "aws_dynamodb_table" "this" { name = var.name }
output "arn" { value = aws_dynamodb_table.this.arn }`,
		".terraform/modules/modules.json": `{"Modules":[{"Key":"","Source":"","Dir":"."},{"Key":"app","Source":"./app","Dir":"app"},{"Key":"app.db","Source":"registry.terraform.io/terraform-aws-modules/dynamodb-table/aws","Version":"4.0.0","Dir":".terraform/modules/app.db"}]}`,
	}
	for name, src := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := LoadCatalog(providers.Catalogs)
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse(root)
	if err != nil {
		t.Fatal(err)
	}
	got := goldenText(Abstract(g, cat))
	if want := "connection aws_lambda_function.f -> module.app.module.db.aws_dynamodb_table.this [uses]"; !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}

// archOf writes files (path -> source) under a new temp dir and returns
// its architecture as describe renders it.
func archOf(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, src := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := LoadCatalog(providers.Catalogs)
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse(root)
	if err != nil {
		t.Fatal(err)
	}
	return goldenText(Abstract(g, cat))
}

// module.q[0].arn and module.qs["x"].arn reach the output of a module with
// count or for_each.
func TestIndexedModuleOutputs(t *testing.T) {
	got := archOf(t, map[string]string{
		"q/main.tf": `
resource "aws_sqs_queue" "this" { name = "q" }
output "arn" { value = aws_sqs_queue.this.arn }`,
		"main.tf": `
module "q" {
  source = "./q"
  count  = 1
}
module "qs" {
  source   = "./q"
  for_each = toset(["x"])
}
resource "aws_lambda_function" "f" { function_name = "f" }
resource "aws_lambda_event_source_mapping" "a" {
  event_source_arn = module.q[0].arn
  function_name    = aws_lambda_function.f.arn
}
resource "aws_lambda_event_source_mapping" "b" {
  event_source_arn = module.qs["x"].arn
  function_name    = aws_lambda_function.f.arn
}`,
	})
	for _, want := range []string{
		"connection module.q.aws_sqs_queue.this -> aws_lambda_function.f [triggers]",
		"connection module.qs.aws_sqs_queue.this -> aws_lambda_function.f [triggers]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Resources named after a shared variable ("${var.name}-jobs") don't point
// at the component the variable names; a policy using it does.
func TestNameVariableSiblings(t *testing.T) {
	got := archOf(t, map[string]string{"main.tf": `
variable "name" {}
resource "aws_ecs_service" "this" { name = var.name }
resource "aws_sqs_queue" "jobs" { name = "${var.name}-jobs" }
resource "aws_s3_bucket" "artifacts" {
  bucket = "${var.name}-artifacts"
  tags   = { Service = var.name }
}
resource "aws_iam_role" "r" { name = "r" }
resource "aws_lambda_function" "f" {
  function_name = "f"
  role          = aws_iam_role.r.arn
}
resource "aws_iam_role_policy" "p" {
  role   = aws_iam_role.r.id
  policy = jsonencode({ Resource = "arn:aws:ecs:*:*:service/${var.name}" })
}`})
	if strings.Contains(got, "-> aws_ecs_service.this [uses]") {
		t.Errorf("a sibling named after var.name points at the service:\n%s", got)
	}
	if want := "connection aws_lambda_function.f -> aws_ecs_service.this [access]"; !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}

// Ingress names who may connect, so the arrow runs from them: through
// standalone rules, inline ingress blocks, and both ways for mutual rules.
func TestSecurityGroupDirection(t *testing.T) {
	services := `
resource "aws_security_group" "api" { name = "api" }
resource "aws_ecs_service" "api" {
  name = "api"
  network_configuration { security_groups = [aws_security_group.api.id] }
}
resource "aws_db_instance" "db" {
  identifier             = "db"
  vpc_security_group_ids = [aws_security_group.db.id]
}`
	for name, db := range map[string]string{
		"rule": `
resource "aws_security_group" "db" { name = "db" }
resource "aws_vpc_security_group_ingress_rule" "db_from_api" {
  security_group_id            = aws_security_group.db.id
  referenced_security_group_id = aws_security_group.api.id
}`,
		"inline": `
resource "aws_security_group" "db" {
  name = "db"
  ingress {
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.api.id]
  }
}`,
	} {
		got := archOf(t, map[string]string{"main.tf": services + db})
		if want := "connection aws_ecs_service.api -> aws_db_instance.db [network]"; !strings.Contains(got, want) {
			t.Errorf("%s: missing %q in:\n%s", name, want, got)
		}
	}
}

// A managed policy attached to two roles grants access to each role's user.
func TestSharedManagedPolicy(t *testing.T) {
	got := archOf(t, map[string]string{"main.tf": `
resource "aws_s3_bucket" "data" { bucket = "data" }
resource "aws_iam_policy" "read" {
  name   = "read"
  policy = jsonencode({ Statement = [{ Resource = aws_s3_bucket.data.arn }] })
}
resource "aws_iam_role" "d" { name = "d" }
resource "aws_iam_role" "e" { name = "e" }
resource "aws_iam_role_policy_attachment" "d" {
  role       = aws_iam_role.d.name
  policy_arn = aws_iam_policy.read.arn
}
resource "aws_iam_role_policy_attachment" "e" {
  role       = aws_iam_role.e.name
  policy_arn = aws_iam_policy.read.arn
}
resource "aws_lambda_function" "d" {
  function_name = "d"
  role          = aws_iam_role.d.arn
}
resource "aws_lambda_function" "e" {
  function_name = "e"
  role          = aws_iam_role.e.arn
}`})
	for _, want := range []string{
		"connection aws_lambda_function.d -> aws_s3_bucket.data [access]",
		"connection aws_lambda_function.e -> aws_s3_bucket.data [access]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// A reference to one key of a map output or local reaches only that entry.
func TestKeyedObjectReferences(t *testing.T) {
	got := archOf(t, map[string]string{
		"queues/main.tf": `
resource "aws_sqs_queue" "a" { name = "a" }
resource "aws_sqs_queue" "b" { name = "b" }
output "arns" {
  value = { a = aws_sqs_queue.a.arn, b = aws_sqs_queue.b.arn }
}`,
		"main.tf": `
module "queues" { source = "./queues" }
resource "aws_lambda_function" "f" { function_name = "f" }
resource "aws_lambda_event_source_mapping" "m" {
  event_source_arn = module.queues.arns["a"]
  function_name    = aws_lambda_function.f.arn
}`,
	})
	if !strings.Contains(got, "connection module.queues.aws_sqs_queue.a -> aws_lambda_function.f [triggers]") || strings.Contains(got, "aws_sqs_queue.b ->") {
		t.Errorf("want only queue a to trigger f:\n%s", got)
	}
}

// The same Terraform parses to the same graph, in the same order: the
// resources view lays out in element order.
func TestParseDeterministic(t *testing.T) {
	many := t.TempDir() // several locals and module arguments, read from maps
	var src strings.Builder
	src.WriteString("module \"m\" {\n  source = \"./m\"\n")
	for _, k := range "abcdefgh" {
		fmt.Fprintf(&src, "  %c = local.%c\n", k, k)
	}
	src.WriteString("}\nlocals {\n")
	for _, k := range "abcdefgh" {
		fmt.Fprintf(&src, "  %c = aws_sqs_queue.q.arn\n", k)
	}
	src.WriteString("}\nresource \"aws_sqs_queue\" \"q\" { name = \"q\" }\n")
	os.MkdirAll(filepath.Join(many, "m"), 0o755)
	os.WriteFile(filepath.Join(many, "main.tf"), []byte(src.String()), 0o644)
	os.WriteFile(filepath.Join(many, "m", "main.tf"), []byte("variable \"a\" {}\n"), 0o644)
	for _, dir := range append(examples(), many) {
		var first []byte
		for range 5 {
			g, err := Parse(dir)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(g)
			if first == nil {
				first = b
			} else if !bytes.Equal(b, first) {
				t.Errorf("%s: parse differs between runs", filepath.Base(dir))
				break
			}
		}
	}
}

// An unchanged save writes nothing; an empty grid removes the layout.
func TestSaveLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	for _, l := range []Layout{{Grid: map[string][2]int{"a": {0, 0}}}, {Grid: map[string][2]int{"a": {2, 0}}}} {
		if changed, err := SaveLayout(path, l); err != nil || !changed {
			t.Fatalf("save: changed=%v err=%v", changed, err)
		}
	}
	if changed, _ := SaveLayout(path, Layout{Grid: map[string][2]int{"a": {2, 0}}}); changed {
		t.Error("an identical save reported a change")
	}
	if changed, err := SaveLayout(path, Layout{}); err != nil || !changed {
		t.Fatalf("reset: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("an empty grid should remove the layout")
	}
}

// A draw entry that draws nothing (a typo, or no Terraform in it) is
// reported rather than silently missing from the map.
func TestDrawProblems(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "infra"), 0o755)
	os.WriteFile(filepath.Join(ws, "infra", "main.tf"), nil, 0o644)
	os.MkdirAll(filepath.Join(ws, "docs"), 0o755)
	os.WriteFile(filepath.Join(ws, "desprawl.hcl"), []byte(`draw = ["infra", "infar", "docs"]`), 0o644)
	got := strings.Join(DrawProblems(ws), "; ")
	if want := `draw entry "infar": no such directory; draw entry "docs": no .tf files in it`; got != want {
		t.Errorf("DrawProblems = %q, want %q", got, want)
	}
}

// The imported Terraform is drawn like any other: only when draw lists it.
func TestConfigsOnlyDraw(t *testing.T) {
	ws := t.TempDir()
	for _, d := range []string{"infra", ".desprawl/imported"} {
		os.MkdirAll(filepath.Join(ws, d), 0o755)
		os.WriteFile(filepath.Join(ws, d, "main.tf"), nil, 0o644)
	}
	hcl := filepath.Join(ws, "desprawl.hcl")
	os.WriteFile(hcl, []byte(`draw = ["infra"]`), 0o644)
	if got := strings.Join(Configs(ws), ","); got != "infra" {
		t.Errorf("Configs = %q, want infra", got)
	}
	os.WriteFile(hcl, []byte(`draw = [".desprawl/imported", "infra"]`), 0o644)
	if got := strings.Join(Configs(ws), ","); got != ".desprawl/imported,infra" {
		t.Errorf("Configs = %q, want .desprawl/imported,infra", got)
	}
}
