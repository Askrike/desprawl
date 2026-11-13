package tf

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDevOverrides(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no executable bit")
	}
	dir := t.TempDir()
	install := func(src, ver string) string {
		d := filepath.Join(dir, ".terraform", "providers", src, ver, runtime.GOOS+"_"+runtime.GOARCH)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		typ := filepath.Base(src)
		if err := os.WriteFile(filepath.Join(d, "terraform-provider-"+typ+"_v"+ver), nil, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	rc := filepath.Join(dir, ".desprawl", "terraformrc")
	user := filepath.Join(dir, "user.tfrc")
	env := map[string]string{"TF_CLI_CONFIG_FILE": user} // none yet
	if got, err := DevOverrides(dir, rc, env); err != nil || got != "" {
		t.Fatalf("nothing installed: got %q, %v", got, err)
	}
	a := install("registry.terraform.io/acme/a", "1.0.0")
	install("registry.terraform.io/acme/b", "1.0.0")
	install("registry.terraform.io/acme/b", "2.0.0") // two versions: keep the lock check
	got, err := DevOverrides(dir, rc, env)
	if err != nil || got != rc {
		t.Fatalf("got %q, %v", got, err)
	}
	b, _ := os.ReadFile(rc)
	if !strings.Contains(string(b), `"registry.terraform.io/acme/a" = "`+a+`"`) || strings.Contains(string(b), "acme/b") {
		t.Errorf("config:\n%s", b)
	}

	// The user's config is kept, unless it installs providers itself.
	os.WriteFile(user, []byte("credentials \"example.com\" {\n  token = \"x\"\n}\n"), 0o644)
	if got, err := DevOverrides(dir, rc, env); err != nil || got != rc {
		t.Fatalf("with user config: got %q, %v", got, err)
	}
	if b, _ := os.ReadFile(rc); !strings.Contains(string(b), `credentials "example.com"`) || !strings.Contains(string(b), "dev_overrides") {
		t.Errorf("config with user's:\n%s", b)
	}
	os.WriteFile(user, []byte("provider_installation {\n  direct {}\n}\n"), 0o644)
	if got, err := DevOverrides(dir, rc, env); err != nil || got != "" {
		t.Errorf("user installs providers: got %q, %v", got, err)
	}
}
