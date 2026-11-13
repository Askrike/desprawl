package tf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// The provider cache (Runner.ProviderCache) saves init downloading and
// unpacking a provider of hundreds of MB, and dumping its schema, for every
// new workspace: seconds each. Init still asks the registry which version
// the constraints select, so a cached version is only used when it is the
// one a download would have installed.
//
// Terraform only installs from its plugin cache into a workspace without a
// lock file if allowed to break the lock file: the lock then gets only this
// platform's checksum of the cached package, not the registry's checksums
// for every platform. So the registry's checksums of every version
// downloaded into the cache are kept (hashes/), and put back into the lock
// file when they include the checksum terraform recorded: the lock file is
// then the one a download would have written.

const lockFile = ".terraform.lock.hcl"

// pluginCacheEnv points init at the plugin cache, unless the user already
// chose one (in the environment or their CLI config): then init uses theirs
// as they set it up.
func (r Runner) pluginCacheEnv() []string {
	if _, ok := os.LookupEnv("TF_PLUGIN_CACHE_DIR"); ok {
		return nil
	}
	if _, ok := r.Env["TF_PLUGIN_CACHE_DIR"]; ok {
		return nil
	}
	if c, err := readUserConfig(r.Env); err != nil || c.cacheDir {
		return nil
	}
	dir := filepath.Join(r.ProviderCache, "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	return []string{"TF_PLUGIN_CACHE_DIR=" + dir, "TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE=true"}
}

// lockProvider is a provider block of a lock file.
type lockProvider struct {
	source, version string
	hashes          []string
	block           *hclwrite.Block
}

func readLock(dir string) (*hclwrite.File, []lockProvider, error) {
	src, err := os.ReadFile(filepath.Join(dir, lockFile))
	if err != nil {
		return nil, nil, err
	}
	f, diags := hclwrite.ParseConfig(src, lockFile, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, nil, diags
	}
	var ps []lockProvider
	for _, b := range f.Body().Blocks() {
		version := quoted(b.Body().GetAttribute("version"))
		if b.Type() != "provider" || len(b.Labels()) != 1 || len(version) != 1 {
			continue
		}
		ps = append(ps, lockProvider{b.Labels()[0], version[0], quoted(b.Body().GetAttribute("hashes")), b})
	}
	return f, ps, nil
}

// quoted returns the string literals of an attribute's value.
func quoted(a *hclwrite.Attribute) []string {
	if a == nil {
		return nil
	}
	var out []string
	for _, t := range a.Expr().BuildTokens(nil) {
		if t.Type == hclsyntax.TokenQuotedLit {
			out = append(out, string(t.Bytes))
		}
	}
	return out
}

// keepLockHashes saves the registry's checksums of the providers init
// downloaded, and puts them back for those it installed from the cache.
func (r Runner) keepLockHashes() {
	f, ps, err := readLock(r.Dir)
	if err != nil {
		r.Log.Debug("provider cache: %v", err)
		return
	}
	changed := false
	for _, p := range ps {
		path := filepath.Join(r.ProviderCache, "hashes", filepath.FromSlash(p.source), p.version)
		if slices.ContainsFunc(p.hashes, func(h string) bool { return !strings.HasPrefix(h, "h1:") }) {
			// Downloaded from the registry: its checksums for every platform.
			if err := writeFile(path, []byte(strings.Join(p.hashes, "\n")+"\n")); err != nil {
				r.Log.Debug("provider cache: %v", err)
			}
			continue
		}
		b, _ := os.ReadFile(path)
		saved := strings.Fields(string(b))
		if len(saved) == 0 || !subset(p.hashes, saved) {
			r.Log.Verbose("%s: the lock file has only this platform's checksum of %s %s; `terraform providers lock` adds the others", lockFile, p.source, p.version)
			continue
		}
		if !slices.Equal(p.hashes, saved) {
			p.block.Body().SetAttributeRaw("hashes", listTokens(saved))
			changed = true
		}
	}
	if changed {
		if err := writeFile(filepath.Join(r.Dir, lockFile), hclwrite.Format(f.Bytes())); err != nil {
			r.Log.Debug("provider cache: %v", err)
		}
	}
}

func subset(xs, of []string) bool {
	for _, x := range xs {
		if !slices.Contains(of, x) {
			return false
		}
	}
	return true
}

// listTokens is a list of strings written one per line, as terraform
// writes a lock file's hashes.
func listTokens(xs []string) hclwrite.Tokens {
	tok := func(t hclsyntax.TokenType, s string) *hclwrite.Token {
		return &hclwrite.Token{Type: t, Bytes: []byte(s)}
	}
	ts := hclwrite.Tokens{tok(hclsyntax.TokenOBrack, "["), tok(hclsyntax.TokenNewline, "\n")}
	for _, x := range xs {
		ts = append(ts, hclwrite.TokensForValue(cty.StringVal(x))...)
		ts = append(ts, tok(hclsyntax.TokenComma, ","), tok(hclsyntax.TokenNewline, "\n"))
	}
	return append(ts, tok(hclsyntax.TokenCBrack, "]"))
}

// CachedProvidersSchema is ProvidersSchema, kept in the provider cache by
// the provider versions of the lock file and terraform's version: what the
// schema includes also depends on terraform (list resources since 1.14).
func (r Runner) CachedProvidersSchema(ctx context.Context) (*Schemas, error) {
	_, ps, err := readLock(r.Dir)
	if r.ProviderCache == "" || err != nil || len(ps) == 0 {
		return r.ProvidersSchema(ctx)
	}
	version, err := r.version(ctx)
	if err != nil {
		return r.ProvidersSchema(ctx)
	}
	key := []string{"terraform " + version}
	var sources []string
	for _, p := range ps {
		key = append(key, p.source+" "+p.version)
		sources = append(sources, p.source)
	}
	slices.Sort(key)
	sum := sha256.Sum256([]byte(strings.Join(key, "\n")))
	path := filepath.Join(r.ProviderCache, "schemas", hex.EncodeToString(sum[:16])+".json")
	if b, err := os.ReadFile(path); err == nil {
		if s, err := ParseProvidersSchema(b); err == nil {
			return s, nil
		}
	}
	out, err := r.providersSchemaJSON(ctx)
	if err != nil {
		return nil, err
	}
	s, got, err := parseProvidersSchema(out)
	if err != nil {
		return nil, err
	}
	// Only a schema of exactly the locked providers is theirs alone.
	slices.Sort(got)
	slices.Sort(sources)
	if slices.Equal(got, sources) {
		if err := writeFile(path, out); err != nil {
			r.Log.Debug("provider cache: %v", err)
		}
	}
	return s, nil
}

// version is terraform's version, e.g. "1.16.6".
func (r Runner) version(ctx context.Context) (string, error) {
	out, err := r.cmd(ctx, "version", "-json").Output()
	if err != nil {
		return "", fmt.Errorf("terraform version: %w", err)
	}
	var v struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.Version == "" {
		return "", fmt.Errorf("terraform version: unexpected output %q", out)
	}
	return v.Version, nil
}

// writeFile replaces path, so a concurrent reader sees the old or the new
// content, never part of it.
func writeFile(path string, b []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // after a rename, nothing to remove
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
