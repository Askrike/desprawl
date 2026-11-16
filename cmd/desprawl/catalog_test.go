package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/askrike/desprawl/internal/paths"
)

// TestWorkspaceCatalog: a catalog in the workspace's pack draws a type no
// shipped catalog knows, for serve, the map commands and MCP alike.
func TestWorkspaceCatalog(t *testing.T) {
	ws := t.TempDir()
	write := func(path, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(ws, "main.tf"), `resource "cloudamqp_instance" "main" {
  name = "queue"
}
`)
	load := func() int {
		t.Helper()
		g, err := mapLoader(ws)(ws, "")
		if err != nil {
			t.Fatal(err)
		}
		return len(g.Arch.Nodes)
	}
	if n := load(); n != 0 {
		t.Fatalf("without a catalog: %d components, want 0", n)
	}
	write(filepath.Join(paths.Pack(ws), "cloudamqp", "catalog.json"), `{"provider": "cloudamqp", "resources": {
  "cloudamqp_instance": {"role": "primary", "category": "messaging", "label": "RabbitMQ instance"}}}`)
	if n := load(); n != 1 {
		t.Errorf("with the workspace's catalog: %d components, want 1", n)
	}
}
