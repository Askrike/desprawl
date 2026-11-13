package main

// The map server: the page (split.html, with the workspace's overview,
// overview.html, and its maps, index.html) and its API, and the event
// stream that tells open pages about changes.

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/askrike/desprawl/internal/rea"
	"github.com/askrike/desprawl/internal/tfmap"
)

//go:embed index.html
var indexHTML []byte

// overviewHTML is the workspace's overview, at /overview in an iframe on
// the page: what it draws, and where its import stands in the loop
// (rea.Status).
//
//go:embed overview.html
var overviewHTML []byte

// splitHTML is the page at /: the overview, or the windows (screen.go),
// each a configuration's map, index.html in an iframe.
//
//go:embed split.html
var splitHTML []byte

// watcher polls the .tf files under root once a second and tells every
// subscriber when any of them changes ("change"); notify sends other events
// ("view", when an agent changes what the page shows). Polling needs no
// dependency and, unlike most OS file watchers, sees nested directories and
// symlinks.
type watcher struct {
	mu   sync.Mutex
	subs map[chan string]bool
}

func watch(root string) *watcher {
	w := &watcher{subs: map[chan string]bool{}}
	fingerprint := func() uint64 {
		h := fnv.New64a()
		tfmap.WalkTF(root, func(path string, info os.FileInfo) {
			fmt.Fprintf(h, "%s %d %d\n", path, info.Size(), info.ModTime().UnixNano())
		})
		return h.Sum64()
	}
	go func() {
		last, lastProgress, lastStatus := fingerprint(), rea.ProgressFingerprint(root), rea.StatusFingerprint(root)
		for range time.Tick(time.Second) {
			if fp := fingerprint(); fp != last {
				last = fp
				w.notify("change")
			}
			if fp := rea.ProgressFingerprint(root); fp != lastProgress {
				lastProgress = fp
				w.notify("progress")
			}
			if fp := rea.StatusFingerprint(root); fp != lastStatus {
				lastStatus = fp
				w.notify("status")
			}
		}
	}()
	return w
}

func (w *watcher) notify(event string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.subs {
		select {
		case ch <- event:
		default: // the page is behind; it re-reads everything on the next event anyway
		}
	}
}

// watching says how many pages are open.
func (w *watcher) watching() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.subs)
}

// ServeHTTP streams a server-sent event per change.
func (w *watcher) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan string, 4)
	w.mu.Lock()
	w.subs[ch] = true
	w.mu.Unlock()
	defer func() { w.mu.Lock(); delete(w.subs, ch); w.mu.Unlock() }()

	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(rw, ": watching\n\n")
	flusher.Flush()
	for {
		select {
		case ev := <-ch:
			fmt.Fprintf(rw, "data: %s\n\n", ev)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// site is a workspace's map as the page serves it: root is the workspace,
// current the configuration it opens on.
type site struct {
	root, current string
	load          tfmap.Loader
	layouts       tfmap.Layouts
}

// newSite serves the workspace ws, opening on its first configuration.
func newSite(ws string, load tfmap.Loader, layouts tfmap.Layouts) site {
	s := site{root: ws, load: load, layouts: layouts}
	if configs := tfmap.Configs(ws); len(configs) > 0 {
		s.current = configs[0]
	}
	return s
}

// serve serves the site at addr, with v on the page if it names a config
// (desprawl diff -open).
func (s site) serve(addr string, explicitAddr, open bool, v View) error {
	ln, url, err := listen(addr, explicitAddr)
	if err != nil {
		return err
	}
	abs, _ := filepath.Abs(s.root)
	fmt.Printf("desprawl: the workspace in %s, %d Terraform configurations\n  %s\n", abs, len(tfmap.Configs(s.root)), url)
	if open {
		openBrowser(url)
	}
	b := newBoard(s.root)
	if v.Dir != "" {
		b.showDir(v.Dir)
		b.set(v)
	}
	return http.Serve(ln, s.handler(b))
}

// handler is the page and its API. b is what an agent has put on the page
// (see view.go).
func (s site) handler(b *board) http.Handler {
	root := s.root
	mux := http.NewServeMux()
	// The page is the window manager (split.html); each window in it is
	// the map page, /map?embed.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(splitHTML)
	})
	// A window (/map?embed&pane=ID) and the overview (/overview?embed) are
	// parts of that page; on their own they lead to it.
	part := func(page []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !r.URL.Query().Has("embed") {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(page)
		}
	}
	mux.HandleFunc("GET /map", part(indexHTML))
	mux.HandleFunc("GET /overview", part(overviewHTML))
	// problems are desprawl.hcl's draw entries that draw nothing.
	mux.HandleFunc("/api/dirs", func(w http.ResponseWriter, r *http.Request) {
		abs, _ := filepath.Abs(root)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"root": abs, "current": s.current, "dirs": tfmap.Configs(root), "problems": tfmap.DrawProblems(root)})
	})
	// ?dir= is one of the workspace's configurations, by its path from it:
	// nothing else is served.
	dirOf := func(r *http.Request) (string, error) {
		rel := filepath.Clean(r.URL.Query().Get("dir"))
		if !slices.Contains(tfmap.Configs(root), rel) {
			return "", fmt.Errorf("%q is not one of the workspace's configurations", rel)
		}
		return filepath.Join(root, rel), nil
	}
	// The viewer saves the grid here when components are dragged; an empty
	// grid resets to automatic layout.
	mux.HandleFunc("POST /api/layout", func(w http.ResponseWriter, r *http.Request) {
		dir, err := dirOf(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var l tfmap.Layout
		if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := tfmap.SaveLayout(s.layouts.Path(dir), l); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Re-parse on every request so edits to .tf files and layouts show up.
	mux.HandleFunc("/api/graph", func(w http.ResponseWriter, r *http.Request) {
		dir, err := dirOf(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g, err := s.graph(dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(g)
	})
	mux.HandleFunc("GET /api/view", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(b.get())
	})
	// The user dismissed the agent's view.
	mux.HandleFunc("POST /api/view/clear", func(w http.ResponseWriter, r *http.Request) {
		b.clear()
		w.WriteHeader(http.StatusNoContent)
	})
	// Reverse-engineering commands running in the workspace; the page
	// re-reads this on every "progress" event.
	mux.HandleFunc("GET /api/progress", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rea.CommandsUnder(root, time.Now()))
	})
	// Where the workspace stands in the loop, worked out again only when
	// one of its files changed.
	var stMu sync.Mutex
	var stFP uint64
	var st *rea.WorkspaceView
	statusNow := func() rea.WorkspaceView {
		stMu.Lock()
		defer stMu.Unlock()
		if fp := rea.StatusFingerprint(root); fp != stFP || st == nil || st.Running() {
			v := rea.Status(root)
			stFP, st = fp, &v
		}
		return *st
	}
	// Working it out takes seconds on a large workspace: start now, so the
	// page usually finds it done.
	go statusNow()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(statusNow())
	})
	b.screenAPI(mux)
	mux.Handle("/api/events", b.events)
	return sameSite(mux)
}

// graph is a config as the page draws it. Until a layout is saved, that
// is the flow-ordered seed, routed like a saved one; it is written only once
// someone drags a component.
func (s site) graph(dir string) (*tfmap.Graph, error) {
	g, err := s.load(dir, s.layouts.Path(dir))
	if err != nil {
		return nil, err
	}
	if g.Layout == nil {
		g.SetLayout(tfmap.Seed(g.Arch))
		g.Seeded = true
	}
	return g, nil
}

// static is the page with every config built in, as `desprawl html` saves
// it: it opens from a file and needs no server (index.html reads
// DESPRAWL_STATIC instead of the API).
func (s site) static() ([]byte, error) {
	abs, _ := filepath.Abs(s.root)
	dirs := []string{}
	graphs := map[string]*tfmap.Graph{}
	for _, d := range tfmap.Configs(s.root) {
		g, err := s.graph(filepath.Join(s.root, d))
		if err != nil {
			fmt.Fprintf(os.Stderr, "desprawl: skipping %s: %v\n", filepath.Join(s.root, d), err)
			continue
		}
		dirs = append(dirs, d)
		graphs[d] = g
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no Terraform configurations under %s", s.root)
	}
	current := s.current
	if !slices.Contains(dirs, current) {
		current = dirs[0]
	}
	// json.Marshal escapes <, > and &, so no value can close the script.
	data, err := json.Marshal(map[string]any{
		"dirs":   map[string]any{"root": abs, "current": current, "dirs": dirs},
		"graphs": graphs,
	})
	if err != nil {
		return nil, err
	}
	const marker = "<!--desprawl:static-->"
	if !bytes.Contains(indexHTML, []byte(marker)) {
		return nil, errors.New("index.html has no " + marker)
	}
	return bytes.Replace(indexHTML, []byte(marker), []byte("<script>window.DESPRAWL_STATIC = "+string(data)+";</script>"), 1), nil
}

// sameSite keeps other web pages away from the API: a DNS-rebinding page
// arrives with its own host name (IP addresses and localhost can't be
// rebound), and a cross-site page can only POST JSON after a CORS preflight,
// which the server never grants.
func sameSite(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "localhost" && net.ParseIP(strings.Trim(host, "[]")) == nil {
			http.Error(w, "unexpected Host "+r.Host, http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "POST needs Content-Type: application/json", http.StatusUnsupportedMediaType)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// listen opens addr; without an explicit -addr it falls back to any free
// port when the default is taken.
func listen(addr string, explicitAddr bool) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil && !explicitAddr {
		ln, err = net.Listen("tcp", "localhost:0")
	}
	if err != nil {
		return nil, "", err
	}
	return ln, fmt.Sprintf("http://localhost:%d", ln.Addr().(*net.TCPAddr).Port), nil
}

// openBrowser is a variable so tests can keep it from opening tabs.
var openBrowser = func(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start() // best effort: the URL is printed anyway
}
