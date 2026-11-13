// Package paths says where desprawl writes. Every location is decided here;
// README's "Where desprawl writes" lists them.
package paths

import (
	"os"
	"path/filepath"
)

// A workspace is a directory with a desprawl.hcl (ConfigFile); everything
// desprawl writes for it is in its Base directory: State (ignored by git,
// through the .gitignore desprawl writes there), Layouts, the workspace's
// Pack and, by default, the imported Terraform (DefaultOutput).

// ConfigFile is a workspace's settings, written by init and by you.
func ConfigFile(dir string) string { return filepath.Join(dir, "desprawl.hcl") }

// Base is a workspace's .desprawl directory.
func Base(dir string) string { return filepath.Join(dir, ".desprawl") }

// State holds a workspace's machine state: what the stages found and made,
// scratch terraform directories (Tmp). Account data: never committed.
func State(dir string) string { return filepath.Join(Base(dir), "state") }

// Layouts holds the saved layouts of the configs below root, by path.
func Layouts(root string) string { return filepath.Join(Base(root), "layouts") }

// Overrides holds the fixes for generated config, keyed by resource.
func Overrides(dir string) string { return filepath.Join(Base(dir), "overrides.json") }

// Pack holds the fixes to provider knowledge made in a workspace, one
// directory per provider, over the pack desprawl ships.
func Pack(dir string) string { return filepath.Join(Base(dir), "pack") }

// DefaultOutput is where the imported Terraform goes unless desprawl.hcl's
// output says otherwise (relative to the workspace).
const DefaultOutput = ".desprawl/imported"

// Progress is where a running reverse-engineering command reports how far
// it has got (package progress), for `desprawl status` and the map page.
func Progress(dir string) string { return filepath.Join(State(dir), "progress.json") }

// Tmp holds a workspace's scratch terraform directories.
func Tmp(dir string) string { return filepath.Join(State(dir), "tmp") }

// CLIConfig is the terraform CLI config desprawl runs a workspace's
// terraform with (tf.DevOverrides).
func CLIConfig(dir string) string { return filepath.Join(State(dir), "terraformrc") }

// Scratch creates a temporary directory for a git revision's Terraform
// files (show_changes reads them, then removes it).
func Scratch() (string, error) { return os.MkdirTemp("", "desprawl-base-") }

// ProviderCache holds what init would otherwise fetch again for every
// workspace: terraform's plugin cache (plugins/, which workspaces'
// .terraform links into), the lock-file checksums of the provider versions
// downloaded into it (hashes/) and their schemas (schemas/).
func ProviderCache() (string, error) { return Cache("terraform") }

// Cache is a directory for things desprawl can fetch again, such as
// provider docs: $DESPRAWL_CACHE/<name>, else the user's cache directory's
// desprawl/<name>.
func Cache(name string) (string, error) {
	if d := os.Getenv("DESPRAWL_CACHE"); d != "" {
		return filepath.Join(d, name), nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "desprawl", name), nil
}

// Exe is the running desprawl binary, which `desprawl update` replaces.
func Exe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
