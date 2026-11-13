package tfmap

// Which configurations a workspace draws, and where their layouts are saved.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/paths"
)

// WalkTF visits every .tf file under root, following symlinked directories
// (a checkout linked in from elsewhere, say) and skipping hidden directories (.git,
// .terraform, .desprawl) and node_modules.
func WalkTF(root string, visit func(path string, info os.FileInfo)) {
	seen := map[string]bool{}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || seen[real] || depth > 12 {
			return
		}
		seen[real] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name, path := e.Name(), filepath.Join(dir, e.Name())
			if strings.HasPrefix(name, ".") || name == "node_modules" {
				continue
			}
			info, err := os.Stat(path) // follows symlinks
			if err != nil {
				continue
			}
			if info.IsDir() {
				walk(path, depth+1)
			} else if strings.HasSuffix(name, ".tf") {
				visit(path, info)
			}
		}
	}
	walk(root, 0)
}

// configDirs lists the directories under root holding .tf files, relative to
// root ("." for root itself), in walk order.
func configDirs(root string) []string {
	dirs := []string{}
	seen := map[string]bool{}
	WalkTF(root, func(path string, _ os.FileInfo) {
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		rel = filepath.ToSlash(rel)
		if !seen[rel] {
			seen[rel] = true
			dirs = append(dirs, rel)
		}
	})
	return dirs
}

// Configs are the Terraform configurations a workspace draws, by path
// from it with / on every OS (ConfigName): every configuration under each
// of desprawl.hcl's draw entries.
// The imported Terraform is one only when draw lists it, like any other.
func Configs(ws string) []string {
	cfg, err := model.LoadConfig(ws)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range cfg.Draw {
		for _, sub := range configDirs(filepath.Join(ws, d)) {
			if rel := ConfigName(filepath.Join(d, sub)); !slices.Contains(out, rel) {
				out = append(out, rel)
			}
		}
	}
	return out
}

// ConfigName is how a configuration at path (from the workspace) is named
// in Configs, the API, URLs and layouts: cleaned, with / on every OS, like
// an io/fs path. Names become OS paths again only through filepath.Join.
func ConfigName(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}

// ConfigNameIn is the name in workspace ws of the configuration at path;
// either may be absolute or relative to the current directory.
func ConfigNameIn(ws, path string) (string, error) {
	ws, err := filepath.Abs(ws)
	if err != nil {
		return "", err
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(ws, path)
	if err != nil {
		return "", err
	}
	return ConfigName(rel), nil
}

// DrawProblems are desprawl.hcl's draw entries that draw nothing: a
// directory that isn't there (a typo) or holds no Terraform.
func DrawProblems(ws string) []string {
	cfg, err := model.LoadConfig(ws)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range cfg.Draw {
		switch dir := filepath.Join(ws, d); {
		case !isDir(dir):
			out = append(out, fmt.Sprintf("draw entry %q: no such directory", d))
		case len(configDirs(dir)) == 0:
			out = append(out, fmt.Sprintf("draw entry %q: no .tf files in it", d))
		}
	}
	return out
}

// Layouts says where a workspace's configurations' layouts are saved:
// Dir/<config relative to Base>.json, with Base itself saved as _root.json.
type Layouts struct{ Dir, Base string }

// WorkspaceLayouts are the layouts of the workspace ws: its
// .desprawl/layouts, or dir when it isn't "" (-layouts).
func WorkspaceLayouts(ws, dir string) Layouts {
	if dir == "" {
		dir = paths.Layouts(ws)
	}
	return Layouts{Dir: dir, Base: ws}
}

// Path is where config's layout is saved.
func (s Layouts) Path(config string) string {
	return filepath.Join(s.Dir, layoutName(s.Base, config))
}

func layoutName(base, config string) string {
	// Either may be relative (from the command line) and the other absolute.
	base, _ = filepath.Abs(base)
	config, _ = filepath.Abs(config)
	rel, err := filepath.Rel(base, config)
	if err != nil || rel == "." {
		rel = "_root"
	}
	// A configuration beside the workspace (draw = ["../infra"]) is kept
	// apart from one inside it of the same name.
	rel = strings.ReplaceAll(filepath.ToSlash(rel), "..", "^")
	// The imported Terraform (.desprawl/imported) is "imported".
	rel = strings.TrimPrefix(rel, ".desprawl/")
	return filepath.FromSlash(rel) + ".json"
}
