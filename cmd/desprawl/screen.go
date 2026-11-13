package main

// The screen: the windows open on the map page (split.html) and what each
// shows. The server holds it, so the person at the browser and an agent
// over MCP change the same thing: the page posts every change the person
// makes (moving, resizing, closing a window; zooming, panning, selecting
// in one), and an agent's changes reach the page through the event
// stream. The layout is shared state, last writer wins; what happens
// inside a window is an agent's command, which the window applies and
// acknowledges in its next report.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"
)

// Screen is what the page shows: the workspace's overview (overview.html)
// or its maps, in windows: columns side by side, each with windows
// stacked. W and H are weights within the row or the column. The search
// and display options are the page's, applied in every window.
type Screen struct {
	Seq        int      `json:"seq"`
	Page       string   `json:"page"` // "overview" or "maps"
	Cols       []Column `json:"cols"`
	Maximized  string   `json:"maximized,omitempty"`
	Focused    string   `json:"focused,omitempty"`
	Search     string   `json:"search,omitempty"`
	HideLabels bool     `json:"hide_labels,omitempty"`
	Meta       bool     `json:"meta,omitempty"`
}

type Column struct {
	W     float64  `json:"w"`
	Panes []Window `json:"panes"`
}

// Window is one map on the page: Dir is its configuration, for as long as
// the window is open.
type Window struct {
	ID  string  `json:"id"`
	Dir string  `json:"dir"`
	H   float64 `json:"h"`
}

func (s Screen) windows() []Window {
	var ws []Window
	for _, c := range s.Cols {
		ws = append(ws, c.Panes...)
	}
	return ws
}

func (s Screen) window(id string) (Window, bool) {
	for _, w := range s.windows() {
		if w.ID == id {
			return w, true
		}
	}
	return Window{}, false
}

// Point is a position on the map, in its own coordinates (the ones
// get_windows reports), or a distance in screen pixels for PanBy.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// WindowCmd is what an agent asks one window to do; what it leaves out is
// left as it is. The window applies it once it shows Dir.
type WindowCmd struct {
	Seq      int      `json:"seq"`
	Dir      string   `json:"dir"`
	Level    string   `json:"level,omitempty"`
	FitAll   bool     `json:"fit_all,omitempty"`
	FitTo    []string `json:"fit_to,omitempty"`
	LOD      string   `json:"lod,omitempty"`
	Zoom     float64  `json:"zoom,omitempty"`
	Center   *Point   `json:"center,omitempty"`
	CenterOn string   `json:"center_on,omitempty"`
	PanBy    *Point   `json:"pan_by,omitempty"`
	Select   *string  `json:"select,omitempty"`
	Panel    *string  `json:"panel,omitempty"`
}

// WindowState is what a window reports it shows. Applied is the last
// command it carried out.
type WindowState struct {
	Applied  int      `json:"applied"`
	Dir      string   `json:"dir"`
	Level    string   `json:"level"`
	Zoom     float64  `json:"zoom"`
	Center   Point    `json:"center"`
	LOD      string   `json:"lod,omitempty"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Visible  []string `json:"visible"`
	Hidden   int      `json:"more_visible,omitempty"` // visible but left out of Visible
	Selected string   `json:"selected,omitempty"`
	Panel    string   `json:"panel,omitempty"`
	Error    string   `json:"error,omitempty"`         // the configuration didn't load
	Failed   string   `json:"command_error,omitempty"` // what the last command couldn't do
}

func newWindowID() string {
	const letters = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 4)
	rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// getScreen is the screen and what each window last reported.
func (b *board) getScreen() (Screen, map[string]WindowState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	states := map[string]WindowState{}
	for id, st := range b.states {
		states[id] = st
	}
	return b.screen, states
}

// setScreen replaces the layout: windows without an id get one, empty
// columns go, weights default to 1. It drops the commands and reports of
// windows that closed, and tells the page.
func (b *board) setScreen(s Screen) (Screen, error) {
	switch s.Page {
	case "":
		s.Page = "maps"
	case "overview", "maps":
	default:
		return Screen{}, fmt.Errorf("page is overview or maps, got %q", s.Page)
	}
	seen := map[string]bool{}
	cols := s.Cols[:0:0]
	for _, c := range s.Cols {
		var panes []Window
		for _, w := range c.Panes {
			if w.ID == "" {
				w.ID = newWindowID()
			}
			if seen[w.ID] {
				return Screen{}, fmt.Errorf("window %q is listed twice", w.ID)
			}
			seen[w.ID] = true
			if w.H <= 0 {
				w.H = 1
			}
			panes = append(panes, w)
		}
		if len(panes) == 0 {
			continue
		}
		if c.W <= 0 {
			c.W = 1
		}
		cols = append(cols, Column{W: c.W, Panes: panes})
	}
	s.Cols = cols
	if !seen[s.Maximized] {
		s.Maximized = ""
	}
	if !seen[s.Focused] {
		s.Focused = ""
		if len(cols) > 0 {
			s.Focused = cols[0].Panes[0].ID
		}
	}
	b.mu.Lock()
	s.Seq = b.screen.Seq + 1
	b.screen = s
	for id := range b.states {
		if !seen[id] {
			delete(b.states, id)
		}
	}
	for id := range b.cmds {
		if !seen[id] {
			delete(b.cmds, id)
		}
	}
	b.mu.Unlock()
	b.events.notify("screen")
	return s, nil
}

// showPage switches the page to the overview or the maps.
func (b *board) showPage(page string) {
	if s, _ := b.getScreen(); s.Page != page {
		s.Page = page
		b.setScreen(s)
	}
}

// showDir switches the page to the maps and makes sure a window shows dir
// and is focused: the one that does, else a new one right of the focused
// window (the only one, with none yet). It returns the window's id.
func (b *board) showDir(dir string) string {
	s, _ := b.getScreen()
	for _, w := range s.windows() {
		if w.Dir == dir {
			if s.Page != "maps" || s.Focused != w.ID || (s.Maximized != "" && s.Maximized != w.ID) {
				s.Page = "maps"
				s.Focused = w.ID
				if s.Maximized != "" {
					s.Maximized = w.ID
				}
				b.setScreen(s)
			}
			return w.ID
		}
	}
	at := len(s.Cols) - 1
	for ci, c := range s.Cols {
		for _, w := range c.Panes {
			if w.ID == s.Focused {
				at = ci
			}
		}
	}
	w := Window{ID: newWindowID(), Dir: dir, H: 1}
	width := 1.0
	if len(s.Cols) > 0 {
		width = 0
		for _, c := range s.Cols {
			width += c.W / float64(len(s.Cols))
		}
	}
	s.Cols = slices.Insert(s.Cols, at+1, Column{W: width, Panes: []Window{w}})
	s.Focused, s.Maximized, s.Page = w.ID, "", "maps"
	b.setScreen(s)
	return w.ID
}

// command gives window id a new command and tells the page; it returns
// the command's number, which the window's report acknowledges.
func (b *board) command(id string, c WindowCmd) int {
	b.mu.Lock()
	c.Seq = b.cmds[id].Seq + 1
	if st, ok := b.states[id]; ok && st.Applied >= c.Seq {
		c.Seq = st.Applied + 1 // the server restarted under an open page
	}
	b.cmds[id] = c
	b.mu.Unlock()
	b.events.notify("window")
	return c.Seq
}

func (b *board) report(id string, st WindowState) {
	b.mu.Lock()
	b.states[id] = st
	close(b.reported)
	b.reported = make(chan struct{})
	b.mu.Unlock()
}

// waitUntil waits until ok says the windows' reports are what the caller
// wants, or until timeout; it says whether they were.
func (b *board) waitUntil(timeout time.Duration, ok func(states map[string]WindowState) bool) bool {
	deadline := time.After(timeout)
	for {
		b.mu.Lock()
		done, ch := ok(b.states), b.reported
		b.mu.Unlock()
		if done {
			return true
		}
		select {
		case <-ch:
		case <-deadline:
			return false
		}
	}
}

func (b *board) state(id string) WindowState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.states[id]
}

// screenAPI is the page's side: GET and POST /api/screen for the layout,
// GET /api/window?id= for a window's command, POST it for its report.
func (b *board) screenAPI(mux *http.ServeMux) {
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /api/screen", func(w http.ResponseWriter, r *http.Request) {
		s, _ := b.getScreen()
		writeJSON(w, s)
	})
	mux.HandleFunc("POST /api/screen", func(w http.ResponseWriter, r *http.Request) {
		var s Screen
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s, err := b.setScreen(s)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, s)
	})
	mux.HandleFunc("GET /api/window", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		c := b.cmds[r.URL.Query().Get("id")]
		b.mu.Unlock()
		writeJSON(w, c)
	})
	mux.HandleFunc("POST /api/window", func(w http.ResponseWriter, r *http.Request) {
		var st WindowState
		if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b.report(r.URL.Query().Get("id"), st)
		w.WriteHeader(http.StatusNoContent)
	})
}
