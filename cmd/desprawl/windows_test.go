package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWindows drives the window tools against a stand-in for the page: it
// does what split.html and index.html do over the API (report what each
// window shows, acknowledge commands), without a browser.
func TestWindows(t *testing.T) {
	was, wait := openBrowser, pageWait
	openBrowser, pageWait = func(string) {}, 3*time.Second
	t.Cleanup(func() { openBrowser, pageWait = was, wait })

	root := t.TempDir()
	tf, err := os.ReadFile("../../examples/aws-s3-sqs-lambda/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"prod", "staging"} {
		os.Mkdir(filepath.Join(root, d), 0o755)
		os.WriteFile(filepath.Join(root, d, "main.tf"), tf, 0o644)
	}
	os.WriteFile(filepath.Join(root, "desprawl.hcl"), []byte(`draw = ["prod", "staging"]`), 0o644)
	cs := connect(t)
	if out, isErr := call(t, cs, "use_workspace", map[string]any{"dir": root}); isErr {
		t.Fatal(out)
	}

	// No page open: the layout is kept, and the windows haven't shown anything.
	out, isErr := call(t, cs, "arrange_windows", map[string]any{"columns": []any{
		map[string]any{"width": 2, "windows": []any{map[string]any{"id": "a", "config": "prod"}}},
		map[string]any{"windows": []any{map[string]any{"id": "b", "config": "staging"}}},
	}, "focused": "b"})
	if isErr {
		t.Fatal(out)
	}
	var got windowsView
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(out)
	}
	if len(got.Columns) != 2 || got.Columns[0].Width != 0.667 || got.Focused != "b" || got.Columns[1].Windows[0].Config != "staging" {
		t.Errorf("arrange_windows: %s", out)
	}
	if out, isErr := call(t, cs, "arrange_windows", map[string]any{"columns": []any{
		map[string]any{"windows": []any{map[string]any{"config": "nowhere"}}}}}); !isErr {
		t.Errorf("an unknown config should fail: %s", out)
	}
	if out, isErr := call(t, cs, "control_window", map[string]any{"window": "zz", "zoom": 1}); !isErr {
		t.Errorf("an unknown window should fail: %s", out)
	}
	if out, isErr := call(t, cs, "control_window", map[string]any{"lod": "far"}); !isErr {
		t.Errorf("a bad lod should fail: %s", out)
	}

	// The page: reports each window, acknowledging its latest command.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var cmds []WindowCmd
	go func() {
		for ctx.Err() == nil {
			var s Screen
			getJSONURL(got.URL+"/api/screen", &s)
			for _, w := range s.windows() {
				var c WindowCmd
				getJSONURL(got.URL+"/api/window?id="+w.ID, &c)
				st := WindowState{Dir: w.Dir, Level: "arch", Zoom: 1, Width: 800, Height: 600}
				if c.Seq > 0 {
					st.Applied, st.Zoom = c.Seq, c.Zoom
					if c.Seq == 1 && w.ID == "b" {
						cmds = append(cmds, c)
					}
				}
				body, _ := json.Marshal(st)
				if resp, err := http.Post(got.URL+"/api/window?id="+w.ID, "application/json", bytes.NewReader(body)); err == nil {
					resp.Body.Close()
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// The focused window by default.
	out, isErr = call(t, cs, "control_window", map[string]any{"zoom": 1.5, "center_on": "aws_s3_bucket.MySourceS3Bucket"})
	if isErr || !strings.Contains(out, `"zoom": 1.5`) || strings.Contains(out, `"id": "a"`) || strings.Contains(out, "hasn't done it") {
		t.Errorf("control_window: %s", out)
	}
	if len(cmds) == 0 || cmds[0].Dir != "staging" || cmds[0].CenterOn != "aws_s3_bucket.MySourceS3Bucket" {
		t.Errorf("command to b: %+v", cmds)
	}
	// A window keeps its configuration.
	if out, isErr := call(t, cs, "arrange_windows", map[string]any{"columns": []any{
		map[string]any{"windows": []any{map[string]any{"id": "b", "config": "prod"}}}}}); !isErr {
		t.Errorf("switching an open window's config should fail: %s", out)
	}
	// Closing a window by leaving it out; the page's options.
	out, _ = call(t, cs, "arrange_windows", map[string]any{"columns": []any{
		map[string]any{"windows": []any{map[string]any{"id": "b"}}}}, "search": "queue", "labels": false})
	if strings.Contains(out, `"id": "a"`) || !strings.Contains(out, `"config": "staging"`) ||
		!strings.Contains(out, `"search": "queue"`) || !strings.Contains(out, `"labels": false`) {
		t.Errorf("after closing a: %s", out)
	}
	// Marks for a configuration no window shows open a window for it.
	call(t, cs, "show_map", map[string]any{"config": "prod"})
	out, _ = call(t, cs, "get_windows", nil)
	if !strings.Contains(out, `"config": "prod"`) || !strings.Contains(out, `"config": "staging"`) {
		t.Errorf("show_map should open a window for prod: %s", out)
	}
	if out, isErr := call(t, cs, "show_map", map[string]any{"config": "prod", "window": "b"}); !isErr {
		t.Errorf("marks for prod in staging's window should fail: %s", out)
	}

	// The overview and back: windows stay behind the overview, and showing
	// something on a map comes back to the maps.
	out, _ = call(t, cs, "arrange_windows", map[string]any{"page": "overview"})
	if !strings.Contains(out, `"page": "overview"`) || !strings.Contains(out, `"config": "staging"`) {
		t.Errorf("page overview: %s", out)
	}
	if out, isErr := call(t, cs, "arrange_windows", map[string]any{"page": "status"}); !isErr {
		t.Errorf("an unknown page should fail: %s", out)
	}
	call(t, cs, "show_map", map[string]any{"config": "staging"})
	if out, _ = call(t, cs, "get_windows", nil); !strings.Contains(out, `"page": "maps"`) {
		t.Errorf("show_map should switch to the maps: %s", out)
	}
	call(t, cs, "arrange_windows", map[string]any{"page": "overview"})
	call(t, cs, "control_window", map[string]any{"fit": true})
	if out, _ = call(t, cs, "get_windows", nil); !strings.Contains(out, `"page": "maps"`) {
		t.Errorf("control_window should switch to the maps: %s", out)
	}
}

// The page opens on the overview when the workspace imports an account,
// on the maps when it only draws.
func TestFirstPage(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "desprawl.hcl"), []byte(`draw = ["."]`), 0o644)
	if s, _ := newBoard(root).getScreen(); s.Page != "maps" {
		t.Errorf("draw-only workspace opens on %q", s.Page)
	}
	os.WriteFile(filepath.Join(root, "desprawl.hcl"), []byte("provider \"x\" {\n  source = \"ns/x\"\n}\n"), 0o644)
	if s, _ := newBoard(root).getScreen(); s.Page != "overview" {
		t.Errorf("importing workspace opens on %q", s.Page)
	}
}

func getJSONURL(url string, v any) error {
	r, err := http.Get(url)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("%s: %s", url, r.Status)
	}
	return json.NewDecoder(r.Body).Decode(v)
}
