package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/tfmap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect starts a session; roots are the agent's project directories.
func connect(t *testing.T, roots ...string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := newMCPServer().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	for _, r := range roots {
		// file:///C:/dir on Windows, file:///dir elsewhere.
		u := url.URL{Scheme: "file", Path: filepath.ToSlash(r)}
		if !strings.HasPrefix(u.Path, "/") {
			u.Path = "/" + u.Path
		}
		c.AddRoots(&mcp.Root{URI: u.String()})
	}
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestMCPTools(t *testing.T) {
	// A workspace drawing a copy of an example.
	root := t.TempDir()
	config := filepath.Join(root, "infra")
	os.Mkdir(config, 0o755)
	tf, err := os.ReadFile("../../examples/aws-s3-sqs-lambda/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(config, "main.tf"), tf, 0o644)

	cs := connect(t)
	if out, isErr := call(t, cs, "list_configs", nil); !isErr {
		t.Fatalf("list_configs without a workspace: %s", out)
	}
	if out, isErr := call(t, cs, "use_workspace", map[string]any{"dir": root}); !isErr {
		t.Fatalf("use_workspace on a directory without desprawl.hcl: %s", out)
	}
	os.WriteFile(filepath.Join(root, "desprawl.hcl"), []byte(`draw = ["infra"]`), 0o644)
	if out, isErr := call(t, cs, "use_workspace", map[string]any{"dir": root}); isErr {
		t.Fatalf("use_workspace: %s", out)
	}
	if out, isErr := call(t, cs, "list_configs", nil); isErr || !strings.Contains(out, "infra") {
		t.Fatalf("list_configs: %s", out)
	}
	out, isErr := call(t, cs, "get_map", map[string]any{"config": "infra"})
	if isErr || !strings.Contains(out, "aws_s3_bucket.MySourceS3Bucket") || !strings.Contains(out, "none saved yet") {
		t.Fatalf("get_map: %s", out)
	}
	if out, isErr := call(t, cs, "save_layout", map[string]any{"config": "infra", "grid": map[string]any{"nope": []int{0, 0}}}); !isErr {
		t.Fatalf("save_layout accepted an unknown id: %s", out)
	}
	if out, isErr := call(t, cs, "seed_layout", nil); isErr {
		t.Fatalf("seed_layout: %s", out)
	}
	// Where the CLI and the viewer look too.
	if got, want := tfmap.WorkspaceLayouts(root, "").Path(config), filepath.Join(root, ".desprawl", "layouts", "infra.json"); got != want {
		t.Fatalf("store = %s, want %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, ".desprawl", "layouts", "infra.json")); err != nil {
		t.Fatalf("seed_layout didn't save to the workspace's .desprawl/layouts: %v", err)
	}
	if out, isErr := call(t, cs, "get_map", map[string]any{"config": "elsewhere"}); !isErr {
		t.Fatalf("get_map on a config outside the workspace: %s", out)
	}

	p, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "map_infrastructure"})
	if err != nil || !strings.Contains(p.Messages[0].Content.(*mcp.TextContent).Text, "list_configs") {
		t.Fatalf("map_infrastructure prompt: %v", err)
	}
}

// The agent's project directory is the session's workspace when it holds
// a desprawl.hcl: no use_workspace needed.
func TestMCPWorkspaceFromRoots(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "infra"), 0o755)
	os.WriteFile(filepath.Join(root, "infra", "main.tf"), []byte(`resource "aws_s3_bucket" "b" {}`), 0o644)
	os.WriteFile(filepath.Join(root, "desprawl.hcl"), []byte(`draw = ["infra"]`), 0o644)
	cs := connect(t, root)
	if out, isErr := call(t, cs, "list_configs", nil); isErr || !strings.Contains(out, "infra") {
		t.Fatalf("list_configs: %s", out)
	}
}

func TestSetupConfigFiles(t *testing.T) {
	dir := t.TempDir()

	js := filepath.Join(dir, "mcp.json")
	os.WriteFile(js, []byte(`{"mcpServers": {"other": {"command": "x"}}, "theme": "dark"}`), 0o600)
	if err := addJSONServer(js, "/bin/desprawl"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(js)
	for _, want := range []string{`"other"`, `"theme": "dark"`, `"command": "/bin/desprawl"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("mcp.json lacks %s:\n%s", want, b)
		}
	}
	if _, err := os.Stat(js + ".bak"); err != nil {
		t.Error("no backup of mcp.json")
	}

	toml := filepath.Join(dir, "config.toml")
	os.WriteFile(toml, []byte("model = \"x\"\n\n[mcp_servers.desprawl]\ncommand = \"/old\"\nargs = [\"mcp\"]\n\n[mcp_servers.other]\ncommand = \"y\"\n"), 0o600)
	for range 2 {
		if err := addTOMLServer(toml, "/bin/desprawl"); err != nil {
			t.Fatal(err)
		}
	}
	b, _ = os.ReadFile(toml)
	s := string(b)
	if strings.Contains(s, "/old") || strings.Count(s, "[mcp_servers.desprawl]") != 1 || !strings.Contains(s, "[mcp_servers.other]\ncommand = \"y\"") || !strings.Contains(s, `model = "x"`) {
		t.Errorf("config.toml:\n%s", s)
	}
	// A commented header with CRLF line ends, and the user's env subtable.
	os.WriteFile(toml, []byte("[mcp_servers.desprawl] # old\r\ncommand = \"/old\"\r\n\r\n[mcp_servers.desprawl.env]\r\nK = \"v\"\r\n"), 0o600)
	if err := addTOMLServer(toml, "/bin/desprawl"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(toml)
	if s := string(b); strings.Contains(s, "/old") || strings.Count(s, "[mcp_servers.desprawl]") != 1 || !strings.Contains(s, `K = "v"`) {
		t.Errorf("config.toml:\n%s", s)
	}
	// An inline definition can't be edited safely: refuse.
	os.WriteFile(toml, []byte("[mcp_servers]\ndesprawl = { command = \"/old\" }\n"), 0o600)
	if err := addTOMLServer(toml, "/bin/desprawl"); err == nil {
		t.Error("inline desprawl table: want an error")
	}

	os.WriteFile(js, []byte(`{"n": 12345678901234567890}`), 0o600)
	if err := addJSONServer(js, "/bin/desprawl"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(js); !strings.Contains(string(b), "12345678901234567890") {
		t.Errorf("large number rewritten:\n%s", b)
	}
}
