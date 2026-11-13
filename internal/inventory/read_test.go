package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tf"
)

// The test binary doubles as a fake terraform: run as "terraform", it
// serves x_search, a resource search that returns at most 100 results.
// Its query_string is "*" (everything), "kind:K" (only K) or
// "-kind:K -kind:L" (all but those), like a search query language.
func TestMain(m *testing.M) {
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "terraform" {
		os.Exit(fakeTerraform(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// searchData: 150 of a big kind first (crowding the limit), then 30 and 5
// of two others.
func searchData() []map[string]any {
	var out []map[string]any
	for _, k := range []struct {
		kind string
		n    int
	}{{"svc:big", 150}, {"svc:mid", 30}, {"other:rare", 5}} {
		for i := range k.n {
			out = append(out, map[string]any{"arn": fmt.Sprintf("arn:%s/%d", k.kind, i), "resource_type": k.kind})
		}
	}
	return out
}

var blockRe = regexp.MustCompile(`(?s)data "x_search" "(inventory\d+)" \{(.*?)\n\}`)
var queryRe = regexp.MustCompile(`query_string *= *"([^"]*)"`)

func fakeTerraform(args []string) int {
	dir := strings.TrimPrefix(args[0], "-chdir=")
	switch args[1] {
	case "plan":
		src, err := os.ReadFile(filepath.Join(dir, "main.tf"))
		if err != nil {
			return 1
		}
		outs := map[string]any{}
		for _, m := range blockRe.FindAllStringSubmatch(string(src), -1) {
			q := queryRe.FindStringSubmatch(m[2])
			if q == nil {
				return 1
			}
			var res []map[string]any
			for _, o := range searchData() {
				if match(q[1], o["resource_type"].(string)) {
					res = append(res, o)
				}
			}
			complete := len(res) <= 100
			if !complete {
				res = res[:100]
			}
			outs[m[1]] = map[string]any{"value": res}
			outs[m[1]+"_complete"] = map[string]any{"value": []any{complete}}
		}
		b, _ := json.Marshal(map[string]any{"planned_values": map[string]any{"outputs": outs}})
		for _, a := range args {
			if p, ok := strings.CutPrefix(a, "-out="); ok {
				os.WriteFile(filepath.Join(dir, p), b, 0o644)
			}
		}
		return 0
	case "show":
		b, err := os.ReadFile(filepath.Join(dir, args[len(args)-1]))
		if err != nil {
			return 1
		}
		os.Stdout.Write(b)
		return 0
	}
	return 1
}

func match(q, kind string) bool {
	if q == "*" {
		return true
	}
	if k, ok := strings.CutPrefix(q, "kind:"); ok {
		return k == kind
	}
	for _, t := range strings.Fields(q) {
		if k, ok := strings.CutPrefix(t, "-kind:"); ok && k == kind {
			return false
		}
	}
	return true
}

func onFakePath(t *testing.T) {
	bin := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A copy, not a symlink: Windows needs privileges for symlinks, and
	// finds commands by their .exe name.
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	name := "terraform"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, name), b, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

var search = Source{DataSource: "x_search", Config: map[string]string{"query_string": `"*"`}, List: "resources", ID: "arn", Kind: "resource_type"}

var searchPartition = &Partition{Arg: "query_string", Only: `"kind:{kind}"`, Except: `"{kinds}"`, Each: "-kind:{kind}"}

func read(t *testing.T, s Source) Result {
	t.Helper()
	onFakePath(t)
	ws := model.Workspace{Dir: t.TempDir()}
	res, err := ReadSource(context.Background(), ws, tf.Runner{}, "inventory/s0", s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestCutOffReadIsTruncated(t *testing.T) {
	res := read(t, search)
	if !res.Truncated || len(res.Entries) != 100 || res.Kinds["svc:big"] != 100 || res.Partitioned != 0 {
		t.Errorf("truncated %v, %d entries, kinds %v, partitioned %d", res.Truncated, len(res.Entries), res.Kinds, res.Partitioned)
	}
}

func TestCutOffReadIsPartitioned(t *testing.T) {
	for _, complete := range []string{"", "resource_count[*].complete"} {
		s := search
		s.Partition = searchPartition
		s.Complete = complete
		res := read(t, s)
		// The rare kinds the first read never saw come back; the big kind
		// stays cut off at 100 on its own read.
		want := map[string]int{"svc:big": 100, "svc:mid": 30, "other:rare": 5}
		if fmt.Sprint(res.Kinds) != fmt.Sprint(want) || res.Truncated || fmt.Sprint(res.Cut) != "[svc:big]" || res.Partitioned != 1 {
			t.Errorf("complete %q: kinds %v, truncated %v, cut %v, partitioned %d", complete, res.Kinds, res.Truncated, res.Cut, res.Partitioned)
		}
	}
}

func TestCompleteOverridesRoundCount(t *testing.T) {
	// Exactly 100 of a kind is complete when the source says so.
	s := search
	s.Config = map[string]string{"query_string": `"kind:svc:big"`}
	s.Complete = "resource_count[*].complete"
	res := read(t, s)
	if !res.Truncated {
		t.Errorf("150 cut to 100 not seen as truncated")
	}
	s.Config = map[string]string{"query_string": `"-kind:svc:big -kind:other:rare"`}
	if res := read(t, s); res.Truncated || len(res.Entries) != 30 {
		t.Errorf("30 entries: truncated %v, %d", res.Truncated, len(res.Entries))
	}
}

func TestUndetectedParamFailsBeforePlan(t *testing.T) {
	s := search
	s.Config = map[string]string{"query_string": `"scope:/accounts/{account}"`}
	ws := model.Workspace{Dir: t.TempDir()}
	// No fake terraform on PATH: the read must fail before any plan.
	_, err := ReadSource(context.Background(), ws, tf.Runner{}, "inventory/s0", s, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `"account" was not detected`) {
		t.Errorf("unfilled placeholder: %v", err)
	}
	// Filled, it reads.
	onFakePath(t)
	s.Config = map[string]string{"query_string": `"{account}"`}
	if _, err := ReadSource(context.Background(), ws, tf.Runner{}, "inventory/s0", s, map[string]string{"account": "*"}, nil); err != nil {
		t.Errorf("filled placeholder: %v", err)
	}
	// An HCL interpolation is not a placeholder.
	if m := unfilled.FindStringSubmatch(`"${local.x}"`); m != nil {
		t.Errorf("interpolation taken for a placeholder: %v", m)
	}
}

func TestCheckPartition(t *testing.T) {
	onFakePath(t)
	ws := model.Workspace{Dir: t.TempDir()}
	kinds := map[string]int{"svc:big": 100}
	s := search
	s.Partition = searchPartition
	if err := CheckPartition(context.Background(), ws, tf.Runner{}, "inventory/p", s, nil, nil, kinds); err != nil {
		t.Errorf("working partition rejected: %v", err)
	}
	// A filter the search doesn't understand reads everything: rejected.
	s.Partition = &Partition{Arg: "query_string", Except: `"{kinds}"`, Each: "NOT {kind}"}
	if err := CheckPartition(context.Background(), ws, tf.Runner{}, "inventory/p", s, nil, nil, kinds); err == nil || !strings.Contains(err.Error(), "still returned") {
		t.Errorf("broken except accepted: %v", err)
	}
	// Without except, a rarer kind than the crowding one is read alone.
	s.Partition = &Partition{Arg: "query_string", Only: `"type={kind}"`}
	if err := CheckPartition(context.Background(), ws, tf.Runner{}, "inventory/p", s, nil, nil, map[string]int{"svc:big": 95, "svc:mid": 5}); err == nil {
		t.Error("broken only accepted")
	}
}

func TestAtLimit(t *testing.T) {
	for n, want := range map[int]bool{0: false, 50: false, 100: true, 200: true, 250: true, 500: true, 1000: true, 999: false, 1001: false, 300: false, 10000: true} {
		if AtLimit(n) != want {
			t.Errorf("AtLimit(%d) = %v", n, !want)
		}
	}
}
