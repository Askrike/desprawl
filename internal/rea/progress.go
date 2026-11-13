package rea

// Progress of reverse-engineering commands: on the terminal and in the
// workspace's progress file (package progress) while they run, in
// `desprawl status`, and on the map page for workspaces under its root.

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"time"

	"github.com/askrike/desprawl/internal/logs"
	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/progress"
)

// reporter is the running command's progress, if it reports any; Log
// prints through it so log lines don't break the terminal's progress line.
var reporter *progress.Reporter

// startProgress reports command's progress in w until end is called with
// how the command ended.
// It also records the run as the stage's last (status.go).
func startProgress(w Workspace, command string) (end func(error)) {
	started := time.Now()
	reporter = progress.Start(paths.Progress(w.Dir), command, os.Stderr, progressShow())
	r := reporter
	return func(err error) {
		r.End(err)
		if reporter == r {
			reporter = nil
		}
		w.recordStage(command, started, err)
	}
}

// progressShow is how stderr shows progress: a redrawing line on a
// terminal; elsewhere (an agent's shell, CI, a log file) nothing, since
// the progress file has it, unless -verbose asks for lines.
func progressShow() progress.Show {
	switch {
	case Output.NoProgress:
		return progress.Hidden
	case progress.IsTerminal(os.Stderr):
		return progress.Bar
	case Output.Level >= logs.Verbose:
		return progress.Lines
	}
	return progress.Hidden
}

// PrintProgress prints the last command's progress, for status.
func PrintProgress(w Workspace) {
	if s, err := progress.Read(paths.Progress(w.Dir)); err == nil {
		fmt.Println(s)
	}
}

// progressShown is how long the page keeps showing a command that ended.
const progressShown = time.Minute

// CommandProgress is a command the page shows: running, or ended less
// than progressShown ago.
type CommandProgress struct {
	progress.State
	Dir     string `json:"dir"`  // relative to the served root
	Text    string `json:"text"` // the State in a sentence
	Percent int    `json:"percent"`
	Running bool   `json:"running"`
	Failed  bool   `json:"failed"`
}

// CommandsUnder lists the commands to show for the workspace root.
func CommandsUnder(root string, now time.Time) []CommandProgress {
	var out []CommandProgress
	for _, dir := range []string{"."} {
		s, err := progress.Read(paths.Progress(filepath.Join(root, dir)))
		if err != nil {
			continue
		}
		running := s.Running(now)
		if !running && now.Sub(s.Updated) > progressShown {
			continue
		}
		out = append(out, CommandProgress{State: s, Dir: dir, Text: s.String(), Percent: s.Percent(), Running: running, Failed: s.Error != "" || !s.Finished && !running})
	}
	return out
}

// ProgressFingerprint changes whenever what the page shows of commands
// does: a running one's every heartbeat, and one ending or aging out.
func ProgressFingerprint(root string) uint64 {
	h := fnv.New64a()
	for _, c := range CommandsUnder(root, time.Now()) {
		fmt.Fprintf(h, "%s %d %t\n", c.Dir, c.Updated.UnixNano(), c.Running)
	}
	return h.Sum64()
}
