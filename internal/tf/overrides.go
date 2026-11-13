package tf

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// DevOverrides writes to path a terraform CLI config whose dev_overrides
// point at the providers `terraform init` installed in dir, and returns
// path's absolute form. Without it every terraform process checks each
// provider against the lock file, hashing binaries of hundreds of MB:
// seconds per process. The binaries are the ones init accepted, so
// terraform runs the same provider. A provider with more than one installed
// version for this platform keeps the lock check; with none, it returns "".
//
// TF_CLI_CONFIG_FILE replaces the user's own CLI config, so the config
// written starts with it. When the user's config installs providers itself
// (a provider_installation block) or can't be read, it returns "": their
// config wins and terraform only runs slower. env is the workspace's extra
// environment, which may set the user's config.
func DevOverrides(dir, path string, env map[string]string) (string, error) {
	user, err := readUserConfig(env)
	if err != nil || user.installs {
		return "", err
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return "", err
	}
	// .terraform/providers/<host>/<namespace>/<type>/<version>/<os_arch>
	found, err := filepath.Glob(filepath.Join(dir, ".terraform", "providers", "*", "*", "*", "*", runtime.GOOS+"_"+runtime.GOARCH))
	if err != nil {
		return "", err
	}
	byProvider := map[string][]string{}
	for _, d := range found {
		ver := filepath.Dir(d)
		typ := filepath.Base(filepath.Dir(ver))
		if !hasExecutable(d, "terraform-provider-"+typ) {
			continue
		}
		rel, err := filepath.Rel(filepath.Join(dir, ".terraform", "providers"), filepath.Dir(ver))
		if err != nil {
			return "", err
		}
		src := filepath.ToSlash(rel)
		byProvider[src] = append(byProvider[src], d)
	}
	var srcs []string
	for src, ds := range byProvider {
		if len(ds) == 1 {
			srcs = append(srcs, src)
		}
	}
	if len(srcs) == 0 {
		return "", nil
	}
	sort.Strings(srcs)
	var b bytes.Buffer
	if len(user.src) > 0 {
		fmt.Fprintf(&b, "# From %s:\n%s\n", user.path, bytes.TrimSpace(user.src))
	}
	b.WriteString("# Written by desprawl: terraform runs the providers init installed without\n# re-checking them against the lock file on every run.\nprovider_installation {\n  dev_overrides {\n")
	for _, src := range srcs {
		fmt.Fprintf(&b, "    %q = %q\n", src, byProvider[src][0])
	}
	b.WriteString("  }\n  direct {}\n}\n")
	if path, err = filepath.Abs(path); err != nil {
		return "", err
	}
	// Other terraform processes may be reading it: replace, don't rewrite.
	return path, writeFile(path, b.Bytes())
}

// userConfig is the user's own terraform CLI config file.
type userConfig struct {
	path     string
	src      []byte // nil: there is none
	installs bool   // it has a provider_installation block
	cacheDir bool   // it sets plugin_cache_dir
}

// readUserConfig reads the CLI config terraform would use without
// desprawl's: TF_CLI_CONFIG_FILE (env's, else the process's), else the
// default file. A config it can't parse is an error.
func readUserConfig(env map[string]string) (userConfig, error) {
	var c userConfig
	if v, ok := env["TF_CLI_CONFIG_FILE"]; ok {
		c.path = v
	} else if v, ok := os.LookupEnv("TF_CLI_CONFIG_FILE"); ok {
		c.path = v
	} else if runtime.GOOS == "windows" {
		c.path = filepath.Join(os.Getenv("APPDATA"), "terraform.rc")
	} else if home, err := os.UserHomeDir(); err == nil {
		c.path = filepath.Join(home, ".terraformrc")
	}
	if c.path == "" {
		return c, nil
	}
	src, err := os.ReadFile(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	} else if err != nil {
		return c, err
	}
	f, diags := hclwrite.ParseConfig(src, c.path, hcl.InitialPos)
	if diags.HasErrors() {
		return c, fmt.Errorf("terraform CLI config %s: %s", c.path, diags.Error())
	}
	c.src = src
	for _, b := range f.Body().Blocks() {
		c.installs = c.installs || b.Type() == "provider_installation"
	}
	c.cacheDir = f.Body().GetAttribute("plugin_cache_dir") != nil
	return c, nil
}

// hasExecutable says whether dir holds a file named prefix* that terraform
// would run as the provider.
func hasExecutable(dir, prefix string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}
