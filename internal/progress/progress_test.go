package progress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDescribe(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := start.Add(5 * time.Minute)
	s := State{Command: "generate", Step: "plan 2 (after fix round 1)", Done: 300, Total: 1200, Started: start, Updated: now}
	for _, c := range []struct {
		edit func(*State)
		want string
	}{
		{func(*State) {}, "generate: plan 2 (after fix round 1), 300/1200 resources read (25%), 5m00s in"},
		{func(s *State) { s.Total = 0 }, "generate: plan 2 (after fix round 1), 300 resources read, 5m00s in"},
		{func(s *State) { s.Step, s.Done, s.Total = "fix round 1: repairing 3 errors", 0, 0 }, "generate: fix round 1: repairing 3 errors, 5m00s in"},
		{func(s *State) { s.Updated = start.Add(time.Minute) }, "generate stopped without finishing 4m00s ago, at plan 2 (after fix round 1), 300/1200 resources read (25%)"},
		{func(s *State) { s.Finished = true }, "generate finished 0s ago, in 5m00s"},
		{func(s *State) { s.Finished, s.Error = true, "terraform plan: boom\nmore" }, "generate failed 0s ago after 5m00s, at plan 2 (after fix round 1): terraform plan: boom"},
	} {
		got := s
		c.edit(&got)
		if d := got.describe(now); d != c.want {
			t.Errorf("got  %q\nwant %q", d, c.want)
		}
	}
}

func TestReporter(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".desprawl", "progress.json")
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close() // Windows can't remove an open file
	r := Start(path, "generate", out, Lines)
	r.Step("plan 1 (generating config)", 4)
	r.Add(1)
	r.Add(2)
	r.Log("• a log line")
	r.End(nil)
	s, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Finished || s.Done != 3 || s.Total != 4 || s.Step != "plan 1 (generating config)" {
		t.Errorf("state %+v", s)
	}
	b, _ := os.ReadFile(out.Name())
	// Not a terminal: plain lines, no redraws.
	if strings.Contains(string(b), "\r") || !strings.Contains(string(b), "• generate: plan 1 (generating config), 0/4 resources read") || !strings.Contains(string(b), "• a log line") {
		t.Errorf("output %q", b)
	}
	// Hidden: log lines only.
	var hidden strings.Builder
	h := Start(filepath.Join(t.TempDir(), "progress.json"), "generate", &hidden, Hidden)
	h.Step("plan 1", 4)
	h.Add(1)
	h.Log("• a log line")
	h.End(nil)
	if hidden.String() != "• a log line\n" {
		t.Errorf("hidden output %q", hidden.String())
	}
	var nilReporter *Reporter
	nilReporter.Step("x", 1)
	nilReporter.Add(1)
	nilReporter.Reset()
	nilReporter.Log("x")
	nilReporter.End(nil)
}
