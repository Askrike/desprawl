// Package progress reports how far a long command (generate, verify) has
// got. People see a line on the terminal that redraws itself; agents and
// the map page read the same state from a workspace's progress file
// (paths.Progress), which the running command rewrites every second.
//
// A Reporter's methods do nothing on a nil Reporter, so code that may run
// without one calls them unconditionally.
package progress

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// State is what the progress file holds.
type State struct {
	Command string `json:"command"`
	// Step is what the command is doing now, e.g. "plan 3 (fix round 2)".
	Step string `json:"step"`
	// Done counts the step's resources so far; Total is how many it
	// expects, 0 when it can't tell.
	Done        int       `json:"done"`
	Total       int       `json:"total"`
	Started     time.Time `json:"started"`
	StepStarted time.Time `json:"step_started"`
	// Updated is the running command's heartbeat.
	Updated  time.Time `json:"updated"`
	Finished bool      `json:"finished,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// beat is how often a running command rewrites its file; a file older than
// stale belongs to a command that died.
const (
	beat  = time.Second
	stale = 10 * time.Second
)

// Running says whether the command is still going (it has neither
// finished nor stopped writing).
func (s State) Running(now time.Time) bool {
	return !s.Finished && now.Sub(s.Updated) < stale
}

// String describes the state in one line, e.g. "generate: plan 2 (fix
// round 1), 412/1200 resources read (34%), 1m12s in".
func (s State) String() string { return s.describe(time.Now()) }

func (s State) describe(now time.Time) string {
	switch {
	case s.Finished && s.Error != "":
		return fmt.Sprintf("%s failed %s ago after %s, at %s: %s", s.Command, ago(now, s.Updated), dur(s.Updated.Sub(s.Started)), s.Step, firstLine(s.Error))
	case s.Finished:
		return fmt.Sprintf("%s finished %s ago, in %s", s.Command, ago(now, s.Updated), dur(s.Updated.Sub(s.Started)))
	case !s.Running(now):
		return fmt.Sprintf("%s stopped without finishing %s ago, at %s%s", s.Command, ago(now, s.Updated), s.Step, s.count())
	}
	return fmt.Sprintf("%s: %s%s, %s in", s.Command, s.Step, s.count(), dur(now.Sub(s.Started)))
}

// count is ", 412/1200 resources read (34%)", or nothing for a step that
// counts none.
func (s State) count() string {
	switch {
	case s.Total > 0:
		return fmt.Sprintf(", %d/%d resources read (%d%%)", s.Done, s.Total, s.Percent())
	case s.Done > 0:
		return fmt.Sprintf(", %d resources read", s.Done)
	}
	return ""
}

// Percent is how much of the step is done, 0 when its total is unknown.
func (s State) Percent() int {
	if s.Total <= 0 {
		return 0
	}
	return min(100, 100*s.Done/s.Total)
}

// Read reads a progress file.
func Read(path string) (State, error) {
	var s State
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// Reporter tracks one command's progress.
type Reporter struct {
	path string
	out  io.Writer
	mode Show

	mu      sync.Mutex
	s       State
	printed time.Time // when a non-terminal last got a line
	shown   int       // Done when it did
	drawn   bool      // a terminal line is showing
	stop    chan struct{}
	stopped chan struct{}
}

// Show is how a Reporter shows progress in its output; the progress file
// is written whatever it is.
type Show int

const (
	// Hidden shows none.
	Hidden Show = iota
	// Bar draws a line that redraws itself, for a terminal.
	Bar
	// Lines prints a line when a step starts and then every 30 seconds
	// while it moves, for a log.
	Lines
)

// Start begins reporting command's progress to path and, as show says, to
// out (stderr, usually); log lines go to out whatever show is.
func Start(path, command string, out io.Writer, show Show) *Reporter {
	now := time.Now()
	r := &Reporter{path: path, out: out, mode: show,
		s:    State{Command: command, Step: "starting", Started: now, StepStarted: now},
		stop: make(chan struct{}), stopped: make(chan struct{})}
	r.mu.Lock()
	r.write()
	r.mu.Unlock()
	go func() {
		defer close(r.stopped)
		t := time.NewTicker(beat)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.mu.Lock()
				r.write()
				r.show()
				r.mu.Unlock()
			}
		}
	}()
	return r
}

// Step starts a step that expects total resources (0: unknown).
func (r *Reporter) Step(name string, total int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.s.Step, r.s.Done, r.s.Total, r.s.StepStarted = name, 0, total, time.Now()
	if r.mode == Lines {
		r.printed = time.Time{} // announce it
	}
	r.show()
}

// Reset zeroes the current step's count, for a step that starts over.
func (r *Reporter) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.s.Done = 0
}

// Add counts n more resources.
func (r *Reporter) Add(n int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.s.Done += n
	if r.mode == Bar {
		r.show()
	}
}

// Log prints a log line without breaking the terminal's progress line.
func (r *Reporter) Log(line string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clear()
	fmt.Fprintln(r.out, line)
	r.show()
}

// End records that the command finished (or failed with err) and stops
// reporting.
func (r *Reporter) End(err error) {
	if r == nil {
		return
	}
	close(r.stop)
	<-r.stopped
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clear()
	r.s.Finished = true
	if err != nil {
		r.s.Error = err.Error()
	}
	r.write()
}

// write saves the state with a fresh heartbeat. Errors are ignored:
// progress is a courtesy, never a reason to fail the command.
func (r *Reporter) write() {
	r.s.Updated = time.Now()
	b, err := json.MarshalIndent(r.s, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(r.path), 0o755) != nil {
		return
	}
	// Through a rename, so a reader never sees half a file.
	tmp := r.path + ".tmp"
	if os.WriteFile(tmp, append(b, '\n'), 0o644) == nil {
		os.Rename(tmp, r.path)
	}
}

// show draws the terminal line, or prints a line for a non-terminal when
// one is due.
func (r *Reporter) show() {
	now := time.Now()
	switch r.mode {
	case Hidden:
		return
	case Lines:
		if r.s.Step == "starting" || (!r.printed.IsZero() && (now.Sub(r.printed) < 30*time.Second || r.s.Done == r.shown)) {
			return
		}
		r.printed, r.shown = now, r.s.Done
		fmt.Fprintln(r.out, "•", r.s.describe(now))
		return
	}
	line := fmt.Sprintf("  %s · %s", r.s.Command, r.s.Step)
	if r.s.Total > 0 {
		const width = 24
		filled := width * r.s.Percent() / 100
		line += fmt.Sprintf(" %s%s %d/%d", strings.Repeat("█", filled), strings.Repeat("░", width-filled), r.s.Done, r.s.Total)
	} else if r.s.Done > 0 {
		line += fmt.Sprintf(" · %d read", r.s.Done)
	}
	line += " · " + dur(now.Sub(r.s.Started))
	fmt.Fprint(r.out, "\r\033[K"+line)
	r.drawn = true
}

func (r *Reporter) clear() {
	if r.drawn {
		fmt.Fprint(r.out, "\r\033[K")
		r.drawn = false
	}
}

// IsTerminal says whether f is a terminal a redrawing line works in.
func IsTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd())) && os.Getenv("TERM") != "dumb"
}

func dur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func ago(now, t time.Time) string { return dur(now.Sub(t)) }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
