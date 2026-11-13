package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Terraform runs once for every provider block, so two blocks can't set one
// environment variable differently: the second's profile would be used for
// the first's account too. The same value in both is fine.
func TestConfigEnvShared(t *testing.T) {
	block := func(alias, profile string) string {
		return "provider \"x\" {\n  source = \"ns/x\"\n  alias = \"" + alias + "\"\n  env = { PROFILE = \"" + profile + "\" }\n}\n"
	}
	if _, err := ParseConfig([]byte(block("a", "dev")+block("b", "prod")), "desprawl.hcl"); err == nil || !strings.Contains(err.Error(), "x.a and x.b set PROFILE differently") {
		t.Errorf("conflicting env: err = %v", err)
	}
	if _, err := ParseConfig([]byte(block("a", "dev")+block("b", "dev")), "desprawl.hcl"); err != nil {
		t.Errorf("shared env: %v", err)
	}
}

// A directory inside a workspace isn't one, and the error names the
// workspace rather than suggesting init.
func TestNotWorkspaceNamesEnclosing(t *testing.T) {
	ws := t.TempDir()
	sub := filepath.Join(ws, "infra", "prod")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(ws, "desprawl.hcl"), nil, 0o644)
	_, err := LoadConfig(sub)
	if err == nil || !strings.Contains(err.Error(), "inside the workspace "+ws) || !strings.Contains(err.Error(), "-C "+filepath.Join("..", "..")) {
		t.Errorf("err = %v", err)
	}
	if err := NotWorkspace(t.TempDir()); !strings.Contains(err.Error(), "desprawl init") {
		t.Errorf("outside any workspace: %v", err)
	}
}
