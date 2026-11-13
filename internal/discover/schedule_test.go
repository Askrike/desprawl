package discover

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// When one type of a shared query fails, the types whose blocks terraform
// reported complete keep their results: only the rest are listed again.
func TestCompletedTypesAreNotListedAgain(t *testing.T) {
	bin := t.TempDir()
	queries := filepath.Join(t.TempDir(), "queries")
	needShell(t)
	script := `#!/bin/sh
dir="${1#-chdir=}"
q="$dir/main.tfquery.hcl"
cat "$q" >> ` + queries + `
echo "--" >> ` + queries + `
for typ in x_a x_b; do
  if grep -q "list \"$typ\"" "$q"; then
    echo '{"type":"list_resource_found","list_resource_found":{"address":"list.'$typ'.q0","resource_type":"'$typ'","display_name":"'$typ'","identity":{"id":"'$typ'1"}}}'
    echo '{"type":"list_complete","list_complete":{"address":"list.'$typ'.q0","resource_type":"'$typ'","total":1}}'
  fi
done
if grep -q 'list "x_bad"' "$q"; then
  echo '{"type":"diagnostic","diagnostic":{"severity":"error","summary":"denied","address":"list.x_bad.q0"}}'
  exit 1
fi
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	plain := &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{}}
	schemas := &tf.Schemas{
		Lists:     map[string]*tf.SchemaBlock{"x_a": plain, "x_b": plain, "x_bad": plain},
		Resources: map[string]*tf.SchemaBlock{"x_a": {}, "x_b": {}, "x_bad": {}},
	}
	d := Discoverer{
		WS: model.Workspace{Dir: t.TempDir()}, TF: tf.Runner{}, Schemas: schemas,
		Opts: Options{Parallel: 1, Timeout: 5 * time.Second, Limit: 100},
	}
	_, rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"x_a": 1, "x_b": 1}; !maps.Equal(rep.Listed, want) {
		t.Errorf("listed %v, want %v", rep.Listed, want)
	}
	if !strings.Contains(rep.Failed["x_bad"], "denied") {
		t.Errorf("failed %v, want x_bad denied", rep.Failed)
	}
	b, err := os.ReadFile(queries)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "--\n"); n != 1 {
		t.Errorf("%d queries, want 1:\n%s", n, b)
	}
}

// A child is listed as soon as its parent has been, not once every query
// started alongside the parent has finished.
func TestChildDoesNotWaitForOtherTypes(t *testing.T) {
	bin := t.TempDir()
	needShell(t)
	script := `#!/bin/sh
dir="${1#-chdir=}"
q="$dir/main.tfquery.hcl"
if grep -q 'list "x_long"' "$q"; then sleep 0.8; fi
if grep -q 'list "x_p"' "$q"; then
  echo '{"type":"list_resource_found","list_resource_found":{"resource_type":"x_p","display_name":"p","identity":{"id":"p1"}}}'
fi
if grep -q 'list "x_child"' "$q"; then
  sleep 0.6
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
			"x_long": plain, "x_p": plain,
			"x_child": {Attributes: map[string]*tf.SchemaAttr{"parent": {Required: true}}},
		},
		Resources: map[string]*tf.SchemaBlock{"x_long": {}, "x_p": {}, "x_child": {}},
	}
	d := Discoverer{
		WS: model.Workspace{Dir: t.TempDir()}, TF: tf.Runner{}, Schemas: schemas,
		Specs: []ListArg{{Type: "x_child", Parent: "x_p", Args: map[string]string{"parent": "id"}}},
		Opts:  Options{Parallel: 3, Timeout: 5 * time.Second, Limit: 100},
	}
	start := time.Now()
	_, rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Listed["x_child"] != 1 {
		t.Errorf("listed %v, want x_child", rep.Listed)
	}
	// x_child (600ms) runs alongside x_long (800ms); after it, 1.4s.
	if took := time.Since(start); took > 1200*time.Millisecond {
		t.Errorf("took %s, want x_child listed while x_long still lists", took)
	}
}

// Shared queries get about as many blocks each, not as many types.
func TestBatchesBalanceBlocks(t *testing.T) {
	blocks := map[string][]listBlock{
		"x_a": make([]listBlock, 6), "x_b": make([]listBlock, 1), "x_c": make([]listBlock, 1),
		"x_d": make([]listBlock, 1), "x_e": make([]listBlock, 1), "x_f": make([]listBlock, 1),
		"x_g": make([]listBlock, 1),
	}
	order := []string{"x_a", "x_b", "x_c", "x_d", "x_e", "x_f", "x_g"}
	jobs := batches(order, blocks, nil, 2)
	if len(jobs) != 2 || len(jobs[0]) != 1 || len(jobs[1]) != 6 {
		t.Errorf("jobs %v, want x_a alone and the six others together", jobs)
	}
}

// Shared queries never outnumber the free slots: a job beyond them would
// wait a whole query for one.
func TestBatchesFitFreeSlots(t *testing.T) {
	blocks := map[string][]listBlock{}
	var order []string
	for i := range 3*maxBatch + 1 {
		typ := fmt.Sprintf("x_%03d", i)
		order = append(order, typ)
		blocks[typ] = make([]listBlock, 1)
	}
	if jobs := batches(order, blocks, nil, 3); len(jobs) != 3 {
		t.Errorf("%d jobs, want 3", len(jobs))
	}
	if jobs := batches(order, blocks, nil, 0); len(jobs) != 4 {
		t.Errorf("%d jobs with no free slot, want 4 of at most %d types", len(jobs), maxBatch)
	}
}

// A block that fails doesn't make the type's completed blocks list again.
func TestCompletedBlocksAreNotListedAgain(t *testing.T) {
	bin := t.TempDir()
	queries := filepath.Join(t.TempDir(), "queries")
	needShell(t)
	script := `#!/bin/sh
echo "--" >> ` + queries + `
echo '{"type":"list_resource_found","list_resource_found":{"address":"list.x_e.q0","resource_type":"x_e","display_name":"e","identity":{"id":"e1"}}}'
echo '{"type":"list_complete","list_complete":{"address":"list.x_e.q0","resource_type":"x_e","total":1}}'
echo '{"type":"diagnostic","diagnostic":{"severity":"error","summary":"denied","address":"list.x_e.q1"}}'
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	schemas := &tf.Schemas{
		Lists:     map[string]*tf.SchemaBlock{"x_e": {Attributes: map[string]*tf.SchemaAttr{"kind": {Required: true}}}},
		Resources: map[string]*tf.SchemaBlock{"x_e": {}},
	}
	d := Discoverer{
		WS: model.Workspace{Dir: t.TempDir()}, TF: tf.Runner{}, Schemas: schemas,
		Specs: []ListArg{{Type: "x_e", Values: map[string][]string{"kind": {"ok", "bad"}}}},
		Opts:  Options{Parallel: 1, Timeout: 5 * time.Second, Limit: 100},
	}
	_, rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"x_e": 1}; !maps.Equal(rep.Listed, want) {
		t.Errorf("listed %v, want %v (failed %v)", rep.Listed, want, rep.Failed)
	}
	b, err := os.ReadFile(queries)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "--\n"); n != 1 {
		t.Errorf("%d queries, want 1", n)
	}
}
