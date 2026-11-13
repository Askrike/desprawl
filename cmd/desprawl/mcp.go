package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/askrike/desprawl/catalog"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/rea"
	"github.com/askrike/desprawl/internal/tfmap"
	"github.com/askrike/desprawl/skills"
)

const mcpInstructions = `desprawl draws a project's infrastructure as an architecture map: the
services, data stores and entry points, and how they connect. It reads
Terraform (.tf) files directly; no Terraform CLI or credentials are needed
to draw a map.

Everything works on one workspace: a directory with a desprawl.hcl, which
says what to draw (draw = ["infra"]) and which cloud accounts to
reverse-engineer. The session's workspace is your project directory when it
holds one; otherwise call use_workspace with its absolute path (no
desprawl.hcl yet: run "desprawl init" there; with -draw DIR it only draws).

To map a project, read the map_infrastructure prompt (or the desprawl-map
skill) first. In short: list_configs for the workspace's Terraform, get_map
on each config to see what it contains, then open_map to show the user.
Tools take a configuration by its path in the workspace, as list_configs
gives it.

To point something out on the map (what you changed, where a problem is,
what a request passes through), use show_map: it switches the open page to a
configuration and marks components, resources and connections with notes.
show_changes marks everything that differs from a git revision, down to
the attributes, e.g. to answer "what did you change?".

The page shows either the workspace's overview (the maps it draws, and how
far importing a cloud account got: each stage, what's out of date, the
problems left; workspace_status gives the same as data) or its maps, in one
or more windows side by side. The person switches between them and arranges
and moves around in the windows, and so can you: get_windows says which the
page shows and what each window shows (what's in view, the zoom, the
selection), arrange_windows switches between overview and maps, opens,
closes, moves and resizes windows and sets the page's search and display
options, and control_window zooms, pans, selects and switches the view in
one window. A window shows one configuration for as long as it's open;
show_map opens one if needed. Use them to put things side by side (staging
beside prod), to take the person to a part of the map, or to the overview
to go through what an import left to fix.`

// mapServer is one agent session: its workspace and the viewer serving it.
type mapServer struct {
	load tfmap.Loader

	mu      sync.Mutex
	viewers map[string]*viewerRef // workspace -> viewer
	ws      string                // the session's workspace
	chosen  bool                  // ws was set with use_workspace, not from the roots
	// rootsAsked: the agent's roots were asked for (learnRoots).
	rootsAsked bool
}

type viewerRef struct {
	url   string
	board *board
}

func cmdMCP(args []string) error {
	return newMCPServer().Run(context.Background(), &mcp.StdioTransport{})
}

func newMCPServer() *mcp.Server {
	m := &mapServer{load: tfmap.NewLoader(catalog.FS), viewers: map[string]*viewerRef{}}
	s := mcp.NewServer(&mcp.Implementation{Name: "desprawl", Title: "desprawl", Version: versionString()},
		&mcp.ServerOptions{Instructions: mcpInstructions})

	mcp.AddTool(s, &mcp.Tool{Name: "use_workspace", Description: "Set this session's workspace: the directory holding desprawl.hcl (absolute path). Every other tool works on it alone. Without it the session uses your project directory if it holds a desprawl.hcl."}, m.useWorkspace)
	mcp.AddTool(s, &mcp.Tool{Name: "list_configs", Description: "List the Terraform configurations the session's workspace draws: what its desprawl.hcl's draw entries hold (the imported Terraform is one, .desprawl/imported, when draw lists it), by their path in the workspace (what the other tools take as config)."}, withRoots(m, m.listConfigs))
	mcp.AddTool(s, &mcp.Tool{Name: "get_map", Description: "Describe the architecture map of one Terraform configuration as text: its components (with ids), connections, the grid layout drawn as boxes and routed edges, layout metrics, and resource types the catalog doesn't know."}, withRoots(m, m.getMap))
	mcp.AddTool(s, &mcp.Tool{Name: "open_map", Description: "Open the interactive map of the session's workspace in the user's browser, showing config if given. Returns its URL. If the page is already open, config is shown in the window that has it, or a new window opens for it, instead of another tab. The page updates as .tf files change; after a layout change the user clicks reload."}, withRoots(m, m.openMap))
	mcp.AddTool(s, &mcp.Tool{Name: "show_map", Description: "Show the user something on the map: switch the open page (opening it if needed) to one configuration and draw marks on it. Mark components, resources (drawn on the component they're folded into) or connections (\"<from id> -> <to id>\") as highlight, added, changed, removed or warning, each with a short note; focus fades everything else and zooms to what's left. Use it to explain: what you changed, where a problem is, what a request goes through. The marks go to the window showing config; if none does, a window opens for it. Ids come from get_map (components) and the Terraform (resources). Calling it again replaces the previous marks; no marks clears them."}, withRoots(m, m.showMap))
	mcp.AddTool(s, &mcp.Tool{Name: "show_changes", Description: "Compare one configuration's .tf files on disk with a git revision (default HEAD, so: uncommitted changes) and show the differences on the map: added, changed and removed resources and connections, with the attributes that changed and each block's diff in the side panel. Returns the list of changes. Use it when the user asks what you changed; pass base (e.g. main, HEAD~3, a commit) for committed work. Add a note to explain the change in your own words."}, withRoots(m, m.showChanges))
	mcp.AddTool(s, &mcp.Tool{Name: "save_layout", Description: "Save a grid layout for one configuration: component id (from get_map) -> [col, row]. Arrows should mostly point right: entry points on the left, then services, then the data and secrets they use. Give every component a cell (the metrics list any left unplaced); an empty grid removes the saved layout. Cells must be distinct. Returns the new drawing and metrics."}, withRoots(m, m.saveLayout))
	mcp.AddTool(s, &mcp.Tool{Name: "seed_layout", Description: "Replace one configuration's layout with an automatic flow-ordered one and return its drawing. A good starting point before arranging by hand."}, withRoots(m, m.seedLayout))
	mcp.AddTool(s, &mcp.Tool{Name: "workspace_status", Description: "Where the session's workspace stands in the reverse-engineering loop: per stage (init, discover, generate, link, verify) whether it ran, failed or is running, which of its inputs changed since (it's out of date), what it produced, and the problems it left with their suggested fixes, plus what to do next. The same as the map page's overview (arrange_windows with page overview shows it to the user), as JSON. Nothing is run or written."}, withRoots(m, m.workspaceStatus))
	mcp.AddTool(s, &mcp.Tool{Name: "get_windows", Description: "What the map page shows: the overview or the maps (page), the page's search and display options, its windows (each a configuration's map) as columns left to right with their sizes, which is focused or maximized, and what each window shows: view level, zoom (1 = 100%), centre in map coordinates, level of detail, the components in view, the selection and the side panel. The person at the browser changes these too; this is how they are now."}, withRoots(m, m.getWindows))
	mcp.AddTool(s, &mcp.Tool{Name: "arrange_windows", Description: "Arrange the map page, as the person does with the page's bar and by dragging windows: switch between the workspace's overview and its maps (page), and give every window, as columns left to right of windows top to bottom, with relative widths and heights. Keep a window by its id (get_windows), open one by giving a new id or none and a config (a window shows one configuration for as long as it's open); windows left out are closed. Optionally maximize one, set the focus, and set the page's search and display options, which apply in every window. Opens the page if it isn't. Returns the windows as get_windows does."}, withRoots(m, m.arrangeWindows))
	mcp.AddTool(s, &mcp.Tool{Name: "control_window", Description: "Do in one window what the person does in it with mouse and keyboard: switch its view level (components or every resource), select a component or connection (opening its details), open or close the side panel, and move the camera: fit, zoom to a level of detail, zoom, centre on a component or position, pan. Waits until the window has done it and returns what it shows then, as get_windows does."}, withRoots(m, m.controlWindow))
	mcp.AddTool(s, &mcp.Tool{Name: "tidy_layout", Description: "Reduce crossings in one configuration's saved layout by moving components within their column, save it, and return the drawing."}, withRoots(m, m.tidyLayout))

	// The skills double as prompts, for harnesses that don't load skills.
	for _, p := range []struct{ name, skill, desc string }{
		{"map_infrastructure", "desprawl-map", "How to map a project's infrastructure with desprawl"},
		{"arrange_map", "desprawl-layout", "How to arrange a desprawl map so it reads well"},
	} {
		s.AddPrompt(&mcp.Prompt{Name: p.name, Description: p.desc},
			func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				text, err := skills.Body(p.skill)
				if err != nil {
					return nil, err
				}
				return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}}}, nil
			})
	}

	return s
}

type dirArg struct {
	Dir string `json:"dir" jsonschema:"absolute path of the workspace: the directory holding desprawl.hcl"`
}

type noArg struct{}

type configArg struct {
	Config string `json:"config,omitempty" jsonschema:"the configuration, by its path in the workspace as list_configs gives it (default: the first)"`
}

type viewArg struct {
	Config string       `json:"config,omitempty" jsonschema:"the configuration to show, by its path in the workspace as list_configs gives it (default: the first)"`
	Title  string       `json:"title,omitempty" jsonschema:"heading of the side panel, e.g. 'Why checkout is slow'"`
	Note   string       `json:"note,omitempty" jsonschema:"explanation shown in the side panel (plain text, a few sentences)"`
	Marks  []tfmap.Mark `json:"marks,omitempty" jsonschema:"what to mark on the map"`
	Focus  []string     `json:"focus,omitempty" jsonschema:"component or resource ids to keep visible; everything else is faded and the view zooms to these"`
	Level  string       `json:"level,omitempty" jsonschema:"arch (components, the default) or raw (every Terraform block and reference)"`
	Zoom   string       `json:"zoom,omitempty" jsonschema:"providers (one card per provider), components (default) or resources (cards list the resources and settings inside them; use it for attribute-level detail)"`
	Window string       `json:"window,omitempty" jsonschema:"the window to show it in (get_windows); default: the one showing config, else the focused one"`
}

type changesArg struct {
	Config string   `json:"config,omitempty" jsonschema:"the configuration, by its path in the workspace as list_configs gives it (default: the first)"`
	Base   string   `json:"base,omitempty" jsonschema:"git revision to compare with (default HEAD: uncommitted changes)"`
	Title  string   `json:"title,omitempty" jsonschema:"heading of the side panel (default: Changes since <base>)"`
	Note   string   `json:"note,omitempty" jsonschema:"your explanation of the change, shown in the side panel"`
	Focus  []string `json:"focus,omitempty" jsonschema:"component or resource ids to keep visible; everything else is faded"`
	Zoom   string   `json:"zoom,omitempty" jsonschema:"providers, components (default) or resources (attribute-level detail on the cards)"`
	Window string   `json:"window,omitempty" jsonschema:"the window to show it in (get_windows); default: the one showing config, else the focused one"`
}

type layoutArg struct {
	Config string            `json:"config,omitempty" jsonschema:"the configuration, by its path in the workspace as list_configs gives it (default: the first)"`
	Grid   map[string][2]int `json:"grid" jsonschema:"component id -> [col, row]"`
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func absDir(dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("dir is required")
	}
	// The server's working directory is nobody's project: a relative path
	// would silently resolve against it.
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("dir must be an absolute path, got %q", dir)
	}
	abs := filepath.Clean(dir)
	if !isDir(abs) {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}

// workspace is the session's workspace: the one use_workspace set, else
// the agent's project directory (its MCP roots) when it holds a
// desprawl.hcl. Each agent session runs its own server, so this lasts
// exactly the session.
func (m *mapServer) workspace() (string, error) {
	m.mu.Lock()
	ws := m.ws
	m.mu.Unlock()
	if ws != "" {
		return ws, nil
	}
	return "", errors.New("no workspace for this session: call use_workspace with the directory holding desprawl.hcl (`desprawl init` makes one)")
}

// withRoots wraps a tool: until the session has a workspace, it first
// asks the agent for its project directories (MCP roots, as an input
// request the client answers by calling again) and takes the first holding
// a desprawl.hcl, unless use_workspace set one. A server may not ask for
// roots while serving a call, and asks only agents that have roots.
func withRoots[In any](m *mapServer, h mcp.ToolHandlerFor[In, any]) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		m.mu.Lock()
		known := m.ws != "" || m.rootsAsked
		m.mu.Unlock()
		if !known && req != nil && req.Params != nil {
			if res, ok := req.Params.InputResponses["roots"].(*mcp.ListRootsResult); ok {
				m.learnRoots(res)
			} else if caps := req.ClientCapabilities(); caps != nil && caps.RootsV2 != nil {
				return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"roots": &mcp.ListRootsParams{}}}, nil, nil
			} else {
				m.mu.Lock()
				m.rootsAsked = true
				m.mu.Unlock()
			}
		}
		return h(ctx, req, in)
	}
}

// learnRoots takes the first of the agent's roots holding a desprawl.hcl
// as the session's workspace.
func (m *mapServer) learnRoots(res *mcp.ListRootsResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rootsAsked = true
	if m.chosen {
		return
	}
	for _, r := range res.Roots {
		if u, err := url.Parse(r.URI); err == nil && u.Scheme == "file" {
			if dir := filePath(u); model.IsWorkspace(dir) {
				m.ws = dir
				return
			}
		}
	}
}

// filePath is the local path a file: URI names. On Windows that is
// file:///C:/dir, whose URL path /C:/dir loses its leading slash.
func filePath(u *url.URL) string {
	p := u.Path
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.Clean(filepath.FromSlash(p))
}

func (m *mapServer) useWorkspace(_ context.Context, _ *mcp.CallToolRequest, in dirArg) (*mcp.CallToolResult, any, error) {
	dir, err := absDir(in.Dir)
	if err != nil {
		return nil, nil, err
	}
	if !model.IsWorkspace(dir) {
		return nil, nil, model.NotWorkspace(dir)
	}
	m.mu.Lock()
	m.ws, m.chosen = dir, true
	m.mu.Unlock()
	configs := tfmap.Configs(dir)
	return text(fmt.Sprintf("This session's workspace is %s; the other tools work on it. Its configurations: %s.", dir, strings.Join(configs, ", ")) + drawNote(dir)), nil, nil
}

func (m *mapServer) listConfigs(ctx context.Context, req *mcp.CallToolRequest, _ noArg) (*mcp.CallToolResult, any, error) {
	ws, err := m.workspace()
	if err != nil {
		return nil, nil, err
	}
	dirs := tfmap.Configs(ws)
	if len(dirs) == 0 {
		return text("the workspace " + ws + " has nothing to draw yet: add a draw entry to its desprawl.hcl, or import Terraform with `desprawl run`" + drawNote(ws)), nil, nil
	}
	n := fmt.Sprintf("%d Terraform configurations", len(dirs))
	if len(dirs) == 1 {
		n = "1 Terraform configuration"
	}
	return text(fmt.Sprintf("%s in the workspace %s (pass them as config):\n%s\n", n, ws, strings.Join(dirs, "\n")) + drawNote(ws)), nil, nil
}

// drawNote tells about desprawl.hcl's draw entries that draw nothing.
func drawNote(ws string) string {
	if ps := tfmap.DrawProblems(ws); len(ps) > 0 {
		return "\nIn desprawl.hcl, " + strings.Join(ps, "; ") + "."
	}
	return ""
}

func (m *mapServer) workspaceStatus(ctx context.Context, req *mcp.CallToolRequest, _ noArg) (*mcp.CallToolResult, any, error) {
	ws, err := m.workspace()
	if err != nil {
		return nil, nil, err
	}
	b, err := json.MarshalIndent(rea.Status(ws), "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return text(string(b)), nil, nil
}

// config is one of the session workspace's configurations: by its path in
// the workspace or an absolute path to it; empty is the first.
func (m *mapServer) config(config string) (ws, abs string, err error) {
	if ws, err = m.workspace(); err != nil {
		return "", "", err
	}
	configs := tfmap.Configs(ws)
	if len(configs) == 0 {
		return "", "", errors.New("the workspace has nothing to draw yet")
	}
	rel := configs[0]
	if config != "" {
		path := config
		if !filepath.IsAbs(path) {
			path = filepath.Join(ws, path)
		}
		if rel, err = tfmap.ConfigNameIn(ws, path); err != nil {
			return "", "", err
		}
	}
	if !slices.Contains(configs, rel) {
		return "", "", fmt.Errorf("%s is not one of the workspace's configurations (list_configs): %s", config, strings.Join(configs, ", "))
	}
	return ws, filepath.Join(ws, rel), nil
}

// loadConfig loads one of the workspace's configurations and says where
// its layout is saved.
func (m *mapServer) loadConfig(config string) (dir string, g *tfmap.Graph, layout string, err error) {
	ws, abs, err := m.config(config)
	if err != nil {
		return "", nil, "", err
	}
	layout = tfmap.WorkspaceLayouts(ws, "").Path(abs)
	if g, err = m.load(abs, layout); err != nil {
		return "", nil, "", err
	}
	if len(g.Nodes) == 0 {
		return "", nil, "", errors.New("no Terraform in " + abs)
	}
	return abs, g, layout, nil
}

func (m *mapServer) getMap(ctx context.Context, req *mcp.CallToolRequest, in configArg) (*mcp.CallToolResult, any, error) {
	dir, g, layout, err := m.loadConfig(in.Config)
	if err != nil {
		return nil, nil, err
	}
	return text(tfmap.Describe(dir, g, layout)), nil, nil
}

func (m *mapServer) openMap(ctx context.Context, req *mcp.CallToolRequest, in configArg) (*mcp.CallToolResult, any, error) {
	ws, abs, err := m.config(in.Config)
	if err != nil {
		return nil, nil, err
	}
	v, err := m.viewer(ws)
	if err != nil {
		return nil, nil, err
	}
	rel, _ := tfmap.ConfigNameIn(ws, abs)
	if in.Config != "" {
		v.board.showDir(rel)
		v.board.set(View{Dir: rel})
	} else {
		v.board.showPage("maps")
	}
	how := m.reveal(v)
	return text(fmt.Sprintf("%s %s (the workspace %s: %d configurations; the page's Overview lists them and shows how far the import got). Layouts the user drags are saved in %s.",
		how, v.url, ws, len(tfmap.Configs(ws)), paths.Layouts(ws))), nil, nil
}

// viewer returns the viewer serving the workspace ws, starting one if
// there is none.
func (m *mapServer) viewer(ws string) (*viewerRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.viewers[ws]; ok {
		return v, nil
	}
	ln, url, err := listen("localhost:8080", false)
	if err != nil {
		return nil, err
	}
	v := &viewerRef{url: url, board: newBoard(ws)}
	m.viewers[ws] = v
	go http.Serve(ln, newSite(ws, m.load, tfmap.WorkspaceLayouts(ws, "")).handler(v.board))
	return v, nil
}

// viewerFor is the session workspace's viewer and config's path in it.
func (m *mapServer) viewerFor(config string) (*viewerRef, string, error) {
	ws, err := m.workspace()
	if err != nil {
		return nil, "", err
	}
	v, err := m.viewer(ws)
	if err != nil {
		return nil, "", err
	}
	rel, err := tfmap.ConfigNameIn(ws, config)
	return v, rel, err
}

// reveal opens the page in the browser unless one is already open. Either
// way the page shows the board: what to show is put there first.
func (m *mapServer) reveal(v *viewerRef) string {
	if v.board.events.watching() > 0 {
		return "the page that is open shows it:"
	}
	openBrowser(v.url)
	return "opened in the browser:"
}

func (m *mapServer) showMap(ctx context.Context, req *mcp.CallToolRequest, in viewArg) (*mcp.CallToolResult, any, error) {
	dir, g, _, err := m.loadConfig(in.Config)
	if err != nil {
		return nil, nil, err
	}
	return m.show(dir, g, View{Title: in.Title, Note: in.Note, Marks: in.Marks, Focus: in.Focus, Level: in.Level, Zoom: in.Zoom, Pane: in.Window}, "")
}

func (m *mapServer) showChanges(ctx context.Context, req *mcp.CallToolRequest, in changesArg) (*mcp.CallToolResult, any, error) {
	dir, g, _, err := m.loadConfig(in.Config)
	if err != nil {
		return nil, nil, err
	}
	base := in.Base
	if base == "" {
		base = "HEAD"
	}
	ch, err := tfmap.Since(dir, g, base, m.load)
	if err != nil {
		return nil, nil, err
	}
	title := in.Title
	if title == "" {
		title = "Changes since " + base
	}
	note := in.Note
	if len(ch.Marks) == 0 && note == "" {
		note = "Nothing in this configuration differs from " + base + "."
	}
	return m.show(dir, g, View{Title: title, Note: note, Marks: ch.Marks, Focus: in.Focus, Zoom: in.Zoom, Pane: in.Window}, ch.Summary+"\n")
}

// show puts v on the page showing dir, after checking what it names.
func (m *mapServer) show(dir string, g *tfmap.Graph, v View, prefix string) (*mcp.CallToolResult, any, error) {
	if v.Level != "" && v.Level != "arch" && v.Level != "raw" {
		return nil, nil, fmt.Errorf("level is arch or raw, got %q", v.Level)
	}
	if v.Zoom != "" && v.Zoom != "providers" && v.Zoom != "components" && v.Zoom != "resources" {
		return nil, nil, fmt.Errorf("zoom is providers, components or resources, got %q", v.Zoom)
	}
	known := map[string]bool{}
	for _, n := range g.Nodes {
		known[n.ID] = true
	}
	for _, n := range g.Arch.Nodes {
		known[n.ID] = true
	}
	for _, e := range g.Arch.Edges {
		known[e.From+" -> "+e.To] = true
	}
	for _, e := range g.Edges {
		known[e.From+" -> "+e.To] = true
	}
	var unknown []string
	for i, mk := range v.Marks {
		if !markKinds[mk.Kind] {
			return nil, nil, fmt.Errorf("mark %q: kind is highlight, added, changed, removed or warning, got %q", mk.ID, mk.Kind)
		}
		// Connections may be written with any spacing around the arrow.
		if from, to, ok := strings.Cut(mk.ID, "->"); ok {
			v.Marks[i].ID = strings.TrimSpace(from) + " -> " + strings.TrimSpace(to)
		}
		if id := v.Marks[i].ID; !known[id] && !known[mk.Owner] && mk.Kind != "removed" {
			unknown = append(unknown, id)
		}
	}
	for _, id := range v.Focus {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	vr, rel, err := m.viewerFor(dir)
	if err != nil {
		return nil, nil, err
	}
	v.Dir = rel
	if v.Pane != "" {
		s, _ := vr.board.getScreen()
		if w, ok := s.window(v.Pane); !ok {
			return nil, nil, fmt.Errorf("no window %q: get_windows lists them", v.Pane)
		} else if w.Dir != rel {
			return nil, nil, fmt.Errorf("window %q shows %s, not %s; leave window out to show it in a window that does (one opens if none does)", v.Pane, w.Dir, rel)
		}
	} else {
		vr.board.showDir(rel)
	}
	vr.board.set(v)
	how := m.reveal(vr)
	msg := fmt.Sprintf("%s%s %s (configuration %s, %d marks).", prefix, how, vr.url, dir, len(v.Marks))
	if len(unknown) > 0 {
		msg += "\nNot on this map, so not drawn: " + strings.Join(unknown, ", ") + ". get_map lists component ids; resource ids are the Terraform addresses (module.x.aws_y.z)."
	}
	return text(msg), nil, nil
}

func (m *mapServer) saveLayout(ctx context.Context, req *mcp.CallToolRequest, in layoutArg) (*mcp.CallToolResult, any, error) {
	dir, g, layout, err := m.loadConfig(in.Config)
	if err != nil {
		return nil, nil, err
	}
	ids := map[string]bool{}
	for _, n := range g.Arch.Nodes {
		ids[n.ID] = true
	}
	at := map[[2]int][]string{}
	for _, id := range slices.Sorted(maps.Keys(in.Grid)) {
		if !ids[id] {
			return nil, nil, fmt.Errorf("no component %q in %s; get_map lists the ids", id, dir)
		}
		at[in.Grid[id]] = append(at[in.Grid[id]], id)
	}
	var clash []string
	for p, ids := range at {
		if len(ids) > 1 {
			clash = append(clash, fmt.Sprintf("[%d, %d]: %s", p[0], p[1], strings.Join(ids, ", ")))
		}
	}
	if len(clash) > 0 {
		slices.Sort(clash)
		return nil, nil, fmt.Errorf("components share a cell, nothing saved:\n%s", strings.Join(clash, "\n"))
	}
	return m.write(dir, g, layout, &tfmap.Layout{Grid: in.Grid})
}

func (m *mapServer) seedLayout(ctx context.Context, req *mcp.CallToolRequest, in configArg) (*mcp.CallToolResult, any, error) {
	dir, g, layout, err := m.loadConfig(in.Config)
	if err != nil {
		return nil, nil, err
	}
	return m.write(dir, g, layout, tfmap.Seed(g.Arch))
}

func (m *mapServer) tidyLayout(ctx context.Context, req *mcp.CallToolRequest, in configArg) (*mcp.CallToolResult, any, error) {
	dir, g, layout, err := m.loadConfig(in.Config)
	if err != nil {
		return nil, nil, err
	}
	if g.Layout == nil {
		return nil, nil, fmt.Errorf("no saved layout for %s: seed_layout first", dir)
	}
	tfmap.Tidy(g.Arch, g.Layout)
	return m.write(dir, g, layout, g.Layout)
}

// write saves l and describes the result.
func (m *mapServer) write(dir string, g *tfmap.Graph, path string, l *tfmap.Layout) (*mcp.CallToolResult, any, error) {
	changed, err := tfmap.SaveLayout(path, *l)
	if err != nil {
		return nil, nil, err
	}
	msg := "unchanged: the layout was already this."
	switch {
	case len(l.Grid) == 0:
		msg = "layout removed; the map now uses the automatic layout below."
		l = nil
	case changed:
		msg = "saved."
	}
	if changed {
		msg += " Tell the user to click reload in the browser if the map is open."
	}
	g.SetLayout(l)
	return text(msg + "\n\n" + tfmap.Describe(dir, g, path)), nil, nil
}
