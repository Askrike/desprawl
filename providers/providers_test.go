package providers

import (
	"os"
	"path/filepath"
	"testing"
)

// A pack file on disk replaces only its embedded namesake.
func TestLoadOverlaysDisk(t *testing.T) {
	shipped, err := Load("", "aws")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "aws"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aws", "list_args.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir, "aws")
	if err != nil {
		t.Fatal(err)
	}
	if len(shipped.ListArgs) == 0 || len(p.ListArgs) != 0 {
		t.Errorf("list args: shipped %d, overlaid %d (want >0, 0)", len(shipped.ListArgs), len(p.ListArgs))
	}
	if len(p.Rules) != len(shipped.Rules) || len(p.Rules) == 0 {
		t.Errorf("rules: overlaid %d, shipped %d", len(p.Rules), len(shipped.Rules))
	}
	if b, err := ReadFile("", "aws", "vocabulary.docs.txt"); err != nil || len(b) == 0 {
		t.Errorf("vocabulary not embedded: %v", err)
	}
}
