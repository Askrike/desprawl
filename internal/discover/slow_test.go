package discover

import (
	"context"
	"github.com/askrike/desprawl/internal/logs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// A type slower than the timeout keeps listing while later rounds go on,
// its children wait for it, and a hung type costs one long timeout.
func TestSlowTypeDoesNotBlockRounds(t *testing.T) {
	bin := t.TempDir()
	needShell(t)
	script := `#!/bin/sh
dir="${1#-chdir=}"
q="$dir/main.tfquery.hcl"
if grep -q 'list "x_hung"' "$q"; then exec sleep 30; fi
if grep -q 'list "x_slow"' "$q"; then
  sleep 0.6
  echo '{"type":"list_resource_found","list_resource_found":{"resource_type":"x_slow","display_name":"s","identity":{"id":"s1"}}}'
fi
if grep -q 'list "x_fast"' "$q"; then
  echo '{"type":"list_resource_found","list_resource_found":{"resource_type":"x_fast","display_name":"f","identity":{"id":"f1"}}}'
fi
if grep -q 'list "x_child"' "$q" && grep -q 's1' "$q"; then
  echo '{"type":"list_resource_found","list_resource_found":{"resource_type":"x_child","display_name":"c","identity":{"id":"c1"}}}'
fi
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	plain := &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{}}
	schemas := &tf.Schemas{
		Lists: map[string]*tf.SchemaBlock{
			"x_hung": plain, "x_slow": plain, "x_fast": plain,
			"x_child": {Attributes: map[string]*tf.SchemaAttr{"parent": {Required: true}}},
		},
		Resources: map[string]*tf.SchemaBlock{"x_hung": {}, "x_slow": {}, "x_fast": {}, "x_child": {}},
	}
	var mu sync.Mutex
	var logged []string
	d := Discoverer{
		WS: model.Workspace{Dir: t.TempDir()}, TF: tf.Runner{}, Schemas: schemas,
		Specs: []ListArg{{Type: "x_child", Parent: "x_slow", Args: map[string]string{"parent": "id"}}},
		Opts:  Options{Parallel: 4, Timeout: 300 * time.Millisecond, Limit: 100},
		Log:   func(_ logs.Level, s string) { mu.Lock(); defer mu.Unlock(); logged = append(logged, s) },
	}
	start := time.Now()
	_, rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s, want about one slow timeout (900ms)", took)
	}
	if rep.Listed["x_slow"] != 1 || rep.Listed["x_child"] != 1 || rep.Listed["x_fast"] != 1 {
		t.Errorf("listed %v, want x_slow, x_child and x_fast", rep.Listed)
	}
	if !strings.Contains(rep.Failed["x_hung"], context.DeadlineExceeded.Error()) {
		t.Errorf("x_hung failed with %q, want a deadline", rep.Failed["x_hung"])
	}
	if want := []string{"x_hung", "x_slow"}; !slices.Equal(rep.Slow, want) {
		t.Errorf("slow %v, want %v", rep.Slow, want)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "x_slow: still listing") {
		t.Errorf("logs %q don't say x_slow is still listing", logged)
	}
}

// Types share queries. A type whose blocks all fail is failed, a failing
// block of another type is dropped, and the rest are listed again.
func TestTypesShareQueries(t *testing.T) {
	bin := t.TempDir()
	queries := filepath.Join(t.TempDir(), "queries")
	needShell(t)
	script := `#!/bin/sh
dir="${1#-chdir=}"
q="$dir/main.tfquery.hcl"
echo "$dir" >> ` + queries + `
if grep -q 'list "x_bad"' "$q"; then
  echo '{"type":"diagnostic","diagnostic":{"severity":"error","summary":"denied","address":"list.x_bad.q0"}}'
  exit 1
fi
if grep -q 'p2' "$q"; then
  echo '{"type":"diagnostic","diagnostic":{"severity":"error","summary":"gone","address":"list.x_child.q1"}}'
  exit 1
fi
for typ in x_a x_b x_c x_child; do
  if grep -q "list \"$typ\"" "$q"; then
    echo '{"type":"list_resource_found","list_resource_found":{"resource_type":"'$typ'","display_name":"'$typ'","identity":{"id":"'$typ'1"}}}'
  fi
done
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	plain := &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{}}
	schemas := &tf.Schemas{
		Lists: map[string]*tf.SchemaBlock{
			"x_a": plain, "x_b": plain, "x_c": plain, "x_bad": plain,
			"x_child": {Attributes: map[string]*tf.SchemaAttr{"parent": {Required: true}}},
		},
		Resources: map[string]*tf.SchemaBlock{"x_a": {}, "x_b": {}, "x_c": {}, "x_bad": {}, "x_child": {}},
	}
	d := Discoverer{
		WS: model.Workspace{Dir: t.TempDir()}, TF: tf.Runner{}, Schemas: schemas,
		Specs: []ListArg{{Type: "x_child", Parent: "x_p", Args: map[string]string{"parent": "id"}}},
		Added: []model.Target{{Type: "x_p", Identity: map[string]any{"id": "p1"}}, {Type: "x_p", Identity: map[string]any{"id": "p2"}}},
		Opts:  Options{Parallel: 1, Timeout: 5 * time.Second, Limit: 100},
	}
	_, rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"x_a": 1, "x_b": 1, "x_c": 1, "x_child": 1}; !maps.Equal(rep.Listed, want) {
		t.Errorf("listed %v, want %v", rep.Listed, want)
	}
	if len(rep.Failed) != 1 || !strings.Contains(rep.Failed["x_bad"], "denied") {
		t.Errorf("failed %v, want x_bad denied", rep.Failed)
	}
	// One query for all five types, one without x_bad, one without the
	// failed x_child block.
	b, err := os.ReadFile(queries)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Errorf("%d queries, want 3:\n%s", n, b)
	}
}

// A hung type not known to be slow times out its shared query; its types
// are then listed alone, so the hung one is reported slow and the others
// are found.
func TestHungTypeInSharedQuery(t *testing.T) {
	bin := t.TempDir()
	needShell(t)
	script := `#!/bin/sh
dir="${1#-chdir=}"
if grep -q 'list "x_hung"' "$dir/main.tfquery.hcl"; then exec sleep 30; fi
echo '{"type":"list_resource_found","list_resource_found":{"resource_type":"x_a","display_name":"a","identity":{"id":"a1"}}}'
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	plain := &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{}}
	schemas := &tf.Schemas{
		Lists:     map[string]*tf.SchemaBlock{"x_a": plain, "x_hung": plain},
		Resources: map[string]*tf.SchemaBlock{"x_a": {}, "x_hung": {}},
	}
	d := Discoverer{
		WS: model.Workspace{Dir: t.TempDir()}, TF: tf.Runner{}, Schemas: schemas,
		Opts: Options{Parallel: 1, Timeout: 200 * time.Millisecond, Limit: 100},
	}
	_, rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Listed["x_a"] != 1 {
		t.Errorf("listed %v, want x_a", rep.Listed)
	}
	if !strings.Contains(rep.Failed["x_hung"], context.DeadlineExceeded.Error()) {
		t.Errorf("x_hung failed with %q, want a deadline", rep.Failed["x_hung"])
	}
	if want := []string{"x_hung"}; !slices.Equal(rep.Slow, want) {
		t.Errorf("slow %v, want %v", rep.Slow, want)
	}
}
