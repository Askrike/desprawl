package main

// The MCP tools for the page's windows (screen.go): an agent sees and does
// what the person at the browser can: switch between the overview and the
// maps; open, close, move, resize, maximize and focus windows, set the
// page's search and display options, and in each window switch the view,
// select, open panels, zoom and pan.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/askrike/desprawl/internal/tfmap"
)

// How long a tool waits for the page to carry out a change: long enough
// for a browser to open and load the page.
var pageWait = 8 * time.Second

type arrangeArg struct {
	Page      string          `json:"page,omitempty" jsonschema:"what the page shows: overview (the maps the workspace draws, and how far its import got, as workspace_status says) or maps (the windows). Default: maps when columns are given, else as it is."`
	Columns   []arrangeColumn `json:"columns,omitempty" jsonschema:"the windows, as columns left to right, each with its windows top to bottom. Windows left out are closed. Leave it out to keep the windows as they are."`
	Maximized *string         `json:"maximized,omitempty" jsonschema:"id of a window to fill the page (the others stay, hidden); empty for none"`
	Focused   string          `json:"focused,omitempty" jsonschema:"id of the window to focus (new windows open right of it)"`
	Search    *string         `json:"search,omitempty" jsonschema:"the page's search, in every window: fades everything but the matches and their neighbours; empty clears it"`
	Labels    *bool           `json:"labels,omitempty" jsonschema:"show connection labels, in every window"`
	Meta      *bool           `json:"meta,omitempty" jsonschema:"show variables, locals and outputs in the resources view, in every window"`
}

type arrangeColumn struct {
	Width   float64         `json:"width,omitempty" jsonschema:"relative width (weights: 2 is twice a column with 1; default 1)"`
	Windows []arrangeWindow `json:"windows"`
}

type arrangeWindow struct {
	ID     string  `json:"id,omitempty" jsonschema:"an open window's id (get_windows) to keep it, or a new id (or none) to open a window"`
	Config string  `json:"config,omitempty" jsonschema:"for a new window, the configuration it shows, by its path in the workspace (default: the first). An open window keeps its configuration: to show another, open a window for it."`
	Height float64 `json:"height,omitempty" jsonschema:"relative height within its column (default 1)"`
}

type controlArg struct {
	Window   string   `json:"window,omitempty" jsonschema:"the window's id from get_windows (default: the focused one)"`
	Level    string   `json:"level,omitempty" jsonschema:"arch (components) or raw (every Terraform block and reference)"`
	Fit      bool     `json:"fit,omitempty" jsonschema:"fit the whole map in the window"`
	FitTo    []string `json:"fit_to,omitempty" jsonschema:"fit these component (or, at raw level, resource) ids in the window"`
	LOD      string   `json:"lod,omitempty" jsonschema:"zoom to a level of detail: providers (one card per provider), components (the whole map), resources (cards list what's inside them)"`
	Zoom     float64  `json:"zoom,omitempty" jsonschema:"zoom level, 1 = 100% (0.05 to 4), around the window's centre"`
	Center   *Point   `json:"center,omitempty" jsonschema:"put this map position (the coordinates get_windows reports) in the window's centre"`
	CenterOn string   `json:"center_on,omitempty" jsonschema:"put this component (or resource) in the window's centre"`
	PanBy    *Point   `json:"pan_by,omitempty" jsonschema:"move the view by this many screen pixels: x > 0 shows more of the right, y > 0 more of the bottom"`
	Select   *string  `json:"select,omitempty" jsonschema:"click a component, resource or connection ('<from> -> <to>'): highlights it and opens its details in the side panel; empty clears the selection"`
	Panel    string   `json:"panel,omitempty" jsonschema:"the side panel: help (legend and overview), marks (what show_map marked) or none (close it)"`
}

// windowsView is get_windows' answer.
type windowsView struct {
	PageOpen  bool             `json:"page_open"`
	URL       string           `json:"url"`
	Page      string           `json:"page"` // overview or maps
	Search    string           `json:"search,omitempty"`
	Labels    bool             `json:"labels"`
	Meta      bool             `json:"meta"`
	Focused   string           `json:"focused,omitempty"`
	Maximized string           `json:"maximized,omitempty"`
	Columns   []windowsViewCol `json:"columns"`
	Note      string           `json:"note,omitempty"`
}

type windowsViewCol struct {
	Width   float64           `json:"width"` // fraction of the page
	Windows []windowsViewItem `json:"windows"`
}

type windowsViewItem struct {
	ID     string       `json:"id"`
	Config string       `json:"config"`
	Height float64      `json:"height"` // fraction of the column
	Shows  *WindowState `json:"shows,omitempty"`
}

func (m *mapServer) windowBoard() (*viewerRef, string, error) {
	ws, err := m.workspace()
	if err != nil {
		return nil, "", err
	}
	v, err := m.viewer(ws)
	return v, ws, err
}

// describeWindows answers with the screen; only, if set, keeps just that window.
func (m *mapServer) describeWindows(v *viewerRef, note, only string) (*mcp.CallToolResult, any, error) {
	s, states := v.board.getScreen()
	out := windowsView{PageOpen: v.board.events.watching() > 0, URL: v.url, Page: s.Page, Search: s.Search, Labels: !s.HideLabels, Meta: s.Meta,
		Focused: s.Focused, Maximized: s.Maximized, Columns: []windowsViewCol{}, Note: note}
	sumW := 0.0
	for _, c := range s.Cols {
		sumW += c.W
	}
	round := func(f float64) float64 { return float64(int(f*1000+.5)) / 1000 }
	for _, c := range s.Cols {
		sumH := 0.0
		for _, w := range c.Panes {
			sumH += w.H
		}
		col := windowsViewCol{Width: round(c.W / sumW)}
		for _, w := range c.Panes {
			if only != "" && w.ID != only {
				continue
			}
			item := windowsViewItem{ID: w.ID, Config: w.Dir, Height: round(w.H / sumH)}
			if st, ok := states[w.ID]; ok {
				item.Shows = &st
			}
			col.Windows = append(col.Windows, item)
		}
		if len(col.Windows) > 0 {
			out.Columns = append(out.Columns, col)
		}
	}
	if !out.PageOpen && out.Note == "" {
		out.Note = "No page is open: arrange_windows, control_window or open_map opens it."
	} else if out.Page == "overview" && out.Note == "" {
		out.Note = "The page shows the overview; the windows are behind it (arrange_windows with page maps shows them)."
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return text(string(b)), nil, nil
}

func (m *mapServer) getWindows(ctx context.Context, req *mcp.CallToolRequest, _ noArg) (*mcp.CallToolResult, any, error) {
	v, _, err := m.windowBoard()
	if err != nil {
		return nil, nil, err
	}
	return m.describeWindows(v, "", "")
}

// relConfig is config's path in the workspace, checked.
func (m *mapServer) relConfig(ws, config string) (string, error) {
	_, abs, err := m.config(config)
	if err != nil {
		return "", err
	}
	return tfmap.ConfigNameIn(ws, abs)
}

func (m *mapServer) arrangeWindows(ctx context.Context, req *mcp.CallToolRequest, in arrangeArg) (*mcp.CallToolResult, any, error) {
	v, ws, err := m.windowBoard()
	if err != nil {
		return nil, nil, err
	}
	old, _ := v.board.getScreen()
	s := old
	if in.Maximized != nil {
		s.Maximized = *in.Maximized
	}
	if in.Focused != "" {
		s.Focused = in.Focused
	}
	if in.Search != nil {
		s.Search = *in.Search
	}
	if in.Labels != nil {
		s.HideLabels = !*in.Labels
	}
	if in.Meta != nil {
		s.Meta = *in.Meta
	}
	switch in.Page {
	case "":
		if in.Columns != nil {
			s.Page = "maps"
		}
	case "overview", "maps":
		s.Page = in.Page
	default:
		return nil, nil, fmt.Errorf("page is overview or maps, got %q", in.Page)
	}
	var fresh []string // windows that are new
	if in.Columns != nil {
		s.Cols = nil
		for _, c := range in.Columns {
			col := Column{W: c.Width}
			for _, w := range c.Windows {
				prev, open := old.window(w.ID)
				dir := prev.Dir
				if !open {
					if dir, err = m.relConfig(ws, w.Config); err != nil {
						return nil, nil, err
					}
				} else if w.Config != "" {
					rel, err := m.relConfig(ws, w.Config)
					if err != nil {
						return nil, nil, err
					}
					if rel != prev.Dir {
						return nil, nil, fmt.Errorf("window %q shows %s, and a window keeps its configuration: open another window for %s", w.ID, prev.Dir, rel)
					}
				}
				win := Window{ID: w.ID, Dir: dir, H: w.Height}
				if win.ID == "" {
					win.ID = newWindowID()
				}
				if !open {
					fresh = append(fresh, win.ID)
				}
				col.Panes = append(col.Panes, win)
			}
			s.Cols = append(s.Cols, col)
		}
	}
	if in.Columns != nil && len(s.windows()) == 0 {
		return nil, nil, fmt.Errorf("give at least one window in columns: the page always shows one")
	}
	if s, err = v.board.setScreen(s); err != nil {
		return nil, nil, err
	}
	m.reveal(v)
	note := ""
	if s.Page == "maps" && !v.board.waitUntil(pageWait, func(states map[string]WindowState) bool {
		for _, id := range fresh {
			if states[id].Width == 0 {
				return false
			}
		}
		return true
	}) {
		note = "Some windows haven't shown their map yet; get_windows tells when they have."
	}
	return m.describeWindows(v, note, "")
}

func (m *mapServer) controlWindow(ctx context.Context, req *mcp.CallToolRequest, in controlArg) (*mcp.CallToolResult, any, error) {
	v, ws, err := m.windowBoard()
	if err != nil {
		return nil, nil, err
	}
	if in.Level != "" && in.Level != "arch" && in.Level != "raw" {
		return nil, nil, fmt.Errorf("level is arch or raw, got %q", in.Level)
	}
	if in.LOD != "" && !slices.Contains([]string{"providers", "components", "resources"}, in.LOD) {
		return nil, nil, fmt.Errorf("lod is providers, components or resources, got %q", in.LOD)
	}
	if in.Panel != "" && !slices.Contains([]string{"help", "marks", "none"}, in.Panel) {
		return nil, nil, fmt.Errorf("panel is help, marks or none, got %q", in.Panel)
	}
	if in.Zoom != 0 && (in.Zoom < 0.05 || in.Zoom > 4) {
		return nil, nil, fmt.Errorf("zoom is between 0.05 and 4, got %v", in.Zoom)
	}
	s, _ := v.board.getScreen()
	id := in.Window
	if id == "" {
		id = s.Focused
	}
	w, open := s.window(id)
	switch {
	case !open && len(s.windows()) == 0 && in.Window == "":
		// No page yet: open it with one window, on the first configuration.
		dir, err := m.relConfig(ws, "")
		if err != nil {
			return nil, nil, err
		}
		w = Window{ID: newWindowID(), Dir: dir, H: 1}
		if _, err = v.board.setScreen(Screen{Cols: []Column{{W: 1, Panes: []Window{w}}}}); err != nil {
			return nil, nil, err
		}
	case !open:
		return nil, nil, fmt.Errorf("no window %q: get_windows lists them", id)
	}
	v.board.showPage("maps")
	cmd := WindowCmd{Dir: w.Dir, Level: in.Level, FitAll: in.Fit, FitTo: in.FitTo, LOD: in.LOD, Zoom: in.Zoom,
		Center: in.Center, CenterOn: in.CenterOn, PanBy: in.PanBy, Select: in.Select}
	if in.Panel != "" {
		cmd.Panel = &in.Panel
	}
	seq := v.board.command(w.ID, cmd)
	m.reveal(v)
	note := ""
	if !v.board.waitUntil(pageWait, func(states map[string]WindowState) bool {
		return states[w.ID].Applied >= seq
	}) {
		note = "The window hasn't done it yet (it does once it has loaded); get_windows tells when it has (shows.applied)."
	} else if st := v.board.state(w.ID); st.Error != "" {
		note = "The window couldn't load its configuration: " + st.Error
	} else if st.Failed != "" {
		note = "The window did the rest, but: " + st.Failed
	}
	return m.describeWindows(v, note, w.ID)
}
