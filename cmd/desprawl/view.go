package main

import (
	"sync"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/tfmap"
)

// View is what an agent wants the page to show: which configuration, and
// marks on its components, resources and connections that explain
// something (what changed, where a problem is, what a request goes
// through). The page draws it over the map until the user dismisses it or
// the agent shows another.
type View struct {
	Seq   int          `json:"seq"`             // bumped on every change, so the page knows it's new
	Dir   string       `json:"dir,omitempty"`   // the configuration, relative to the viewer's root
	Pane  string       `json:"pane,omitempty"`  // the window to show it in (screen.go); else the one showing Dir, else the focused one
	Level string       `json:"level,omitempty"` // "arch" or "raw"
	Zoom  string       `json:"zoom,omitempty"`  // "providers", "components" or "resources"
	Title string       `json:"title,omitempty"`
	Note  string       `json:"note,omitempty"`
	Focus []string     `json:"focus,omitempty"`
	Marks []tfmap.Mark `json:"marks,omitempty"`
}

var markKinds = map[string]bool{"": true, "highlight": true, "added": true, "changed": true, "removed": true, "warning": true}

// board holds a viewer's View, its Screen (screen.go) and the event stream
// that tells its pages about changes to them and to the .tf files.
type board struct {
	mu       sync.Mutex
	view     View
	screen   Screen
	cmds     map[string]WindowCmd   // window id -> the agent's latest command
	states   map[string]WindowState // window id -> what it last reported
	reported chan struct{}          // closed and replaced on every report
	events   *watcher
}

// newBoard is the board for the workspace root. The page opens on the
// overview when the workspace imports a cloud account, so how far the
// import got is the first thing seen; on the maps when it only draws.
func newBoard(root string) *board {
	page := "maps"
	if cfg, err := model.LoadConfig(root); err == nil && len(cfg.Providers) > 0 {
		page = "overview"
	}
	return &board{events: watch(root), screen: Screen{Page: page}, cmds: map[string]WindowCmd{}, states: map[string]WindowState{}, reported: make(chan struct{})}
}

func (b *board) get() View {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.view
}

// set replaces the view and tells every open page.
func (b *board) set(v View) {
	b.mu.Lock()
	v.Seq = b.view.Seq + 1
	b.view = v
	b.mu.Unlock()
	b.events.notify("view")
}

// clear drops the marks but stays on the configuration.
func (b *board) clear() {
	b.mu.Lock()
	b.view = View{Seq: b.view.Seq + 1, Dir: b.view.Dir, Pane: b.view.Pane}
	b.mu.Unlock()
	b.events.notify("view")
}
