package discover

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// fakeTerraform puts a terraform on PATH whose query lists min(limit, 25)
// revisions of family "a" (1..20) and "b" (1..5), in name order.
func fakeTerraform(t *testing.T) {
	bin := t.TempDir()
	needShell(t)
	script := `#!/bin/sh
dir="${1#-chdir=}"
limit=$(sed -n 's/.*limit *= *\([0-9]*\).*/\1/p' "$dir/main.tfquery.hcl" | head -1)
n=0
for fam in a b; do
  max=20; [ "$fam" = b ] && max=5
  i=1
  while [ $i -le $max ]; do
    [ $n -ge "$limit" ] && exit 0
    echo '{"type":"list_resource_found","list_resource_found":{"resource_type":"x_def","display_name":"'$fam'","identity":{"family":"'$fam'","revision":'$i'}}}'
    n=$((n+1)); i=$((i+1))
  done
done
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestTruncatedListIsRetriedWithLargerLimit(t *testing.T) {
	fakeTerraform(t)
	ws := model.Workspace{Dir: t.TempDir()}
	schemas := &tf.Schemas{
		Lists:     map[string]*tf.SchemaBlock{"x_def": {Attributes: map[string]*tf.SchemaAttr{}}},
		Resources: map[string]*tf.SchemaBlock{"x_def": {}},
	}
	d := Discoverer{WS: ws, TF: tf.Runner{}, Schemas: schemas, Opts: Options{Parallel: 1, Timeout: time.Minute, Limit: 3}}
	targets, rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Limit 3 sees only a:1..3 (truncated), 30 sees all 25.
	if len(rep.Truncated) != 0 || rep.Listed["x_def"] != 25 {
		t.Fatalf("truncated %v, listed %d", rep.Truncated, rep.Listed["x_def"])
	}
	got := map[string]float64{}
	for _, tg := range targets {
		got[tg.Identity["family"].(string)] = tg.Identity["revision"].(float64)
	}
	if got["a"] != 20 || got["b"] != 5 || len(got) != 2 {
		t.Errorf("latest revisions = %v, want a:20 b:5", got)
	}
	if len(rep.Superseded["x_def"]) != 23 || rep.Collapsed["x_def"] != 23 {
		t.Errorf("superseded %d, collapsed %d, want 23", len(rep.Superseded["x_def"]), rep.Collapsed["x_def"])
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
