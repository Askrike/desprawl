package mine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/askrike/desprawl/internal/paths"
)

// Fetch downloads a provider's documentation for a version (latest if
// empty) and returns the directory holding it. The source repository and
// release tag come from the Terraform Registry; only the docs directories
// are checked out. Results are cached under the user cache directory.
func Fetch(ctx context.Context, source, version string) (dir, resolved string, err error) {
	ns, name, ok := strings.Cut(source, "/")
	if !ok {
		return "", "", fmt.Errorf("provider source must be namespace/name, got %q", source)
	}
	url := "https://registry.terraform.io/v1/providers/" + ns + "/" + name
	if version != "" {
		url += "/" + version
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("registry %s: %s", url, resp.Status)
	}
	var meta struct {
		Source  string `json:"source"`
		Tag     string `json:"tag"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return "", "", err
	}
	if meta.Source == "" || meta.Tag == "" {
		return "", "", fmt.Errorf("registry has no source repository for %s", source)
	}

	cache, err := paths.Cache("docs")
	if err != nil {
		return "", "", err
	}
	dir = filepath.Join(cache, ns, name, meta.Version)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return dir, meta.Version, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", "", err
	}
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	steps := [][]string{
		{"git", "clone", "--quiet", "--depth", "1", "--filter=blob:none", "--sparse", "--branch", meta.Tag, meta.Source, tmp},
		// Providers keep docs in website/docs (older layout) or docs (tfplugindocs).
		{"git", "-C", tmp, "sparse-checkout", "set", "website/docs", "docs"},
	}
	for _, args := range steps {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", "", fmt.Errorf("%s: %w\n%s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", "", err
	}
	return dir, meta.Version, nil
}
