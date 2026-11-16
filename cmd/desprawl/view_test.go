package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/askrike/desprawl/internal/tfmap"
)

// show_changes compares the files on disk with HEAD and puts the
// differences on the viewer's page, which a browser reads from /api/view.
func TestShowChanges(t *testing.T) {
	opened, was := []string{}, openBrowser
	openBrowser = func(url string) { opened = append(opened, url) }
	t.Cleanup(func() { openBrowser = was })
	root := t.TempDir()
	config := filepath.Join(root, "infra")
	os.Mkdir(config, 0o755)
	tf, err := os.ReadFile("../../examples/aws-s3-sqs-lambda/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(config, "main.tf")
	os.WriteFile(main, tf, 0o644)
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "x")

	changed := strings.Replace(string(tf), `runtime       = "nodejs24.x"`, `runtime       = "nodejs22.x"
  memory_size   = 512`, 1) + "\nresource \"aws_sqs_queue\" \"dlq\" {}\n"
	os.WriteFile(main, []byte(changed), 0o644)

	os.WriteFile(filepath.Join(root, "desprawl.hcl"), []byte(`draw = ["infra"]`), 0o644)
	m := &mapServer{viewers: map[string]*viewerRef{}, ws: root}
	res, _, err := m.showChanges(context.Background(), nil, changesArg{Config: "infra"})
	if err != nil {
		t.Fatal(err)
	}
	b := res.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"1 added, 1 changed, 0 removed", "~ aws_lambda_function.MyHandlerFunction-Function: runtime, memory_size", "+ aws_sqs_queue.dlq"} {
		if !strings.Contains(b, want) {
			t.Errorf("show_changes lacks %q:\n%s", want, b)
		}
	}
	if len(opened) != 1 || strings.Contains(opened[0], "?") {
		t.Errorf("opened %v, want the page's address alone", opened)
	}

	v := m.viewers[root]
	if v == nil {
		t.Fatalf("no viewer for the workspace; viewers: %v", m.viewers)
	}
	// The page opens on what the board holds: a window on infra.
	if s, _ := v.board.getScreen(); s.Page != "maps" || len(s.windows()) != 1 || s.windows()[0].Dir != "infra" || s.Focused != s.windows()[0].ID {
		t.Errorf("screen = %+v, want one focused window on infra", s)
	}
	r, err := http.Get(v.url + "/api/view")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var got View
	json.NewDecoder(r.Body).Decode(&got)
	if got.Dir != "infra" || got.Title != "Changes since HEAD" || len(got.Marks) != 2 {
		t.Errorf("view = %+v", got)
	}

	// show_map replaces the marks; unknown ids are reported, not fatal.
	res, _, err = m.showMap(context.Background(), nil, viewArg{Config: "infra", Marks: []tfmap.Mark{
		{ID: "aws_sqs_queue.MyHandlerQueue->aws_lambda_function.MyHandlerFunction-Function", Note: "polls"},
		{ID: "aws_nope.x"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b = res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(b, "not drawn: aws_nope.x") {
		t.Errorf("show_map didn't report the unknown id:\n%s", b)
	}
	if got := v.board.get(); got.Marks[0].ID != "aws_sqs_queue.MyHandlerQueue -> aws_lambda_function.MyHandlerFunction-Function" {
		t.Errorf("connection id not normalised: %q", got.Marks[0].ID)
	}
	if _, _, err := m.showMap(context.Background(), nil, viewArg{Config: "infra", Marks: []tfmap.Mark{{ID: "x", Kind: "bogus"}}}); err == nil {
		t.Error("show_map accepted an unknown kind")
	}
}
