package inventory

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// Kinds keeps a sample of each ID shape, so a mapping is checked against
// rules on the default bus and on others.
func TestKindsSamplesShapes(t *testing.T) {
	ks := Kinds([]Entry{
		{Kind: "rule", ID: "arn:x:rule/a"},
		{Kind: "rule", ID: "arn:x:rule/b"},
		{Kind: "rule", ID: "arn:x:rule/bus/c"},
		{Kind: "rule", ID: "arn:x:rule/bus/d"},
	})
	if got := ks["rule"].Samples(); len(got) != 2 || got[0] != "arn:x:rule/a" || got[1] != "arn:x:rule/bus/c" || ks["rule"].Count != 4 {
		t.Errorf("samples = %v, count %d", got, ks["rule"].Count)
	}
}

// An optional group that didn't match isn't an attribute value.
func TestOptionalGroups(t *testing.T) {
	m := Mapping{Type: "x_rule", Match: `rule/(?P<path>(?:(?P<bus>[^/]+)/)?(?P<name>[^/]+))$`, ID: "{path}"}
	if got, ok := m.Target("arn:x:rule/a"); !ok || got.ID != "a" || len(got.Attrs) != 2 || got.Attrs["name"] != "a" {
		t.Errorf("default bus: %+v", got)
	}
	if got, ok := m.Target("arn:x:rule/bus/c"); !ok || got.ID != "bus/c" || got.Attrs["bus"] != "bus" || got.Attrs["name"] != "c" {
		t.Errorf("custom bus: %+v", got)
	}
}

// A group named after an attribute must hold the value the import finds.
func TestCheckImportsGroups(t *testing.T) {
	bin := t.TempDir()
	needShell(t)
	script := `#!/bin/sh
dir="${1#-chdir=}"
cat > "$dir/generated.tf" <<'TF'
resource "x_rule" "check0" {
  name           = "c"
  event_bus_name = "bus"
}
resource "x_rule" "check1" {
  name           = "c"
  event_bus_name = "bus"
}
TF
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ws := model.Workspace{Dir: t.TempDir()}
	bad, err := CheckImports(context.Background(), ws, tf.Runner{}, "check", []model.Target{
		{Type: "x_rule", ID: "bus/c", Attrs: map[string]string{"name": "bus/c"}},
		{Type: "x_rule", ID: "bus/c", Attrs: map[string]string{"name": "c", "event_bus_name": "bus", "path": "bus/c"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bad[0], `group name is "bus/c", but the imported name is "c"`) || len(bad) != 1 {
		t.Errorf("failures = %v", bad)
	}
}

// needShell skips a test whose fake terraform is a shell script, which
// Windows can't run. What such tests check doesn't depend on the OS.
func needShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake terraform is a shell script")
	}
}
