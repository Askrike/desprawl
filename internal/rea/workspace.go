// Package rea reverse-engineers live infrastructure into Terraform whose
// resources reference each other instead of hardcoding IDs, for any
// Terraform provider. It runs the stages (internal/discover, generate,
// link, verify) on a workspace, with its provider packs, and reports what
// each couldn't do, with fixes (problems.go), and where the workspace
// stands in the loop (status.go).
package rea

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/askrike/desprawl/internal/logs"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/tf"
)

// Output says what commands print to stderr besides their results
// (stdout) and errors. The command line sets it from -verbose, -debug and
// -no-progress.
var Output struct {
	// Level is the most detailed log level printed.
	Level logs.Level
	// NoProgress hides progress: no line on a terminal, none in a log.
	NoProgress bool
}

// Log prints what commands and their stages say, at the levels Output
// asks for, on stderr.
var Log logs.Logger = func(level logs.Level, s string) {
	switch {
	case level > Output.Level:
	case reporter != nil:
		reporter.Log("• " + s)
	default:
		fmt.Fprintln(os.Stderr, "•", s)
	}
}

type Workspace struct {
	model.Workspace
	Config model.Config // desprawl.hcl
	// params are the scope parameters (account, project, region) shared by
	// the discovered resources' identities.
	params map[string]string
	// ReadOnly workspaces are looked at (status, the map page), not worked
	// in: nothing is written.
	ReadOnly bool
	// current is the provider block the workspace is scoped to (scoped).
	current *model.Provider
}

// open opens the workspace in dir: the directory holding desprawl.hcl.
func Open(dir string) (Workspace, error) {
	cfg, err := model.LoadConfig(dir)
	if err != nil {
		return Workspace{}, err
	}
	w := Workspace{Workspace: model.Workspace{Dir: dir, Out: cfg.OutputDir(dir)}, Config: cfg}
	// Before the first discovery there are no targets: the scope is what
	// the provider block's config sets.
	var targets []model.Target
	w.ReadJSON("targets.json", &targets)
	w.params = w.detectParams(targets)
	return w, nil
}

func (w Workspace) runner() tf.Runner {
	r := tf.Runner{Dir: w.Path(""), Env: w.Config.Env(), Log: Log, Progress: reporter}
	// Without it terraform only runs slower.
	if rc, err := tf.DevOverrides(w.Path(""), paths.CLIConfig(w.Dir), w.Config.Env()); err == nil {
		r.CLIConfig = rc
	}
	if d, err := paths.ProviderCache(); err == nil {
		r.ProviderCache = d
	}
	return r
}

// locations says where the workspace's commands write, init and status
// print it: nothing goes anywhere the user wasn't told about.
func (w Workspace) Locations() []string {
	abs, _ := filepath.Abs(w.Dir)
	out, _ := filepath.Abs(w.Path(""))
	cache, _ := paths.ProviderCache()
	return []string{
		"workspace (desprawl.hcl, and .desprawl/ for state, layouts, overrides): " + abs,
		"imported Terraform: " + out,
		"provider downloads, shared by workspaces (DESPRAWL_CACHE to move): " + cache,
		"provider packs: the workspace's fixes, over the shipped packs: " + w.Packs(),
	}
}

func (w Workspace) Schemas(ctx context.Context) (*tf.Schemas, error) {
	path := w.StatePath("schema.json")
	// Schemas saved before data sources were recorded are refreshed.
	if s, err := tf.LoadSchemas(path); err == nil && len(s.DataSources) > 0 {
		return s, nil
	}
	s, err := w.runner().ProvidersSchema(ctx)
	if err != nil {
		return nil, err
	}
	return s, tf.SaveSchemas(path, s)
}

// packs is where the workspace's fixes to provider knowledge are: one
// directory per provider, each file there replacing the shipped one of the
// same name (so the first fix copies the shipped file: packFile).
func (w Workspace) Packs() string { return paths.Pack(w.Dir) }

// provider is the provider block the workspace is scoped to, else the
// first.
func (w Workspace) Provider() model.Provider {
	if w.current != nil {
		return *w.current
	}
	if len(w.Config.Providers) == 0 {
		return model.Provider{}
	}
	return w.Config.Providers[0]
}

// ready makes what terraform needs from desprawl.hcl (prepare) and, when
// the providers changed since terraform last set them up (or never did),
// runs terraform init and saves their schema. Every command that runs
// terraform calls it first, so editing desprawl.hcl needs nothing else.
func (w Workspace) ready(ctx context.Context) error {
	changed, err := w.prepare()
	if err != nil {
		return err
	}
	if _, err := os.Stat(w.Path(".terraform.lock.hcl")); err == nil && !changed {
		if _, err := os.Stat(w.StatePath("schema.json")); err == nil {
			return nil
		}
	}
	Log.Info("terraform init")
	if err := w.runner().Init(ctx); err != nil {
		return err
	}
	s, err := w.runner().CachedProvidersSchema(ctx)
	if err != nil {
		return err
	}
	return tf.SaveSchemas(w.StatePath("schema.json"), s)
}

// prepare makes what terraform needs from desprawl.hcl: the output
// directory with its providers.tf, and the .gitignore keeping state out of
// git. changed says providers.tf changed.
func (w Workspace) prepare() (changed bool, err error) {
	if len(w.Config.Providers) == 0 {
		return false, errors.New("desprawl.hcl has no provider block: nothing to reverse-engineer (add one, or use the workspace only to draw)")
	}
	if err := os.MkdirAll(paths.Base(w.Dir), 0o755); err != nil {
		return false, err
	}
	if _, err := writeIfChanged(filepath.Join(paths.Base(w.Dir), ".gitignore"), []byte("# Machine state and account data: never committed.\nstate/\n")); err != nil {
		return false, err
	}
	if err := os.MkdirAll(w.Path(""), 0o755); err != nil {
		return false, err
	}
	return writeIfChanged(w.Path("providers.tf"), w.Config.ProvidersTF())
}

func writeIfChanged(path string, b []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(b) {
		return false, nil
	}
	return true, os.WriteFile(path, b, 0o644)
}

// ProviderName is the local name in a provider source: "aws" in
// "hashicorp/aws" or "registry.terraform.io/hashicorp/aws".
func ProviderName(source string) (string, error) {
	i := strings.LastIndex(source, "/")
	if i <= 0 || i == len(source)-1 {
		return "", fmt.Errorf("-provider must be namespace/name, e.g. hashicorp/aws; got %q", source)
	}
	return source[i+1:], nil
}

// NewProvider is a provider block from what init was told; env is
// NAME=value pairs.
func NewProvider(source, version, config string, env []string) (model.Provider, error) {
	name, err := ProviderName(source)
	if err != nil {
		return model.Provider{}, err
	}
	p := model.Provider{Name: name, Source: source, Version: version, Body: config, Env: map[string]string{}}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return model.Provider{}, fmt.Errorf("env %q isn't NAME=value", kv)
		}
		p.Env[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return p, nil
}

// Init sets up terraform for the workspace's providers: the
// output directory, terraform init, and the providers' schema.
func Init(dir string) error {
	started := time.Now()
	w, err := Open(dir)
	if err != nil {
		return err
	}
	for _, l := range w.Locations() {
		Log.Info("%s", l)
	}
	if err := w.ready(context.Background()); err != nil {
		return err
	}
	s, err := tf.LoadSchemas(w.StatePath("schema.json"))
	if err != nil {
		return err
	}
	pk, err := w.Pack()
	if err != nil {
		return err
	}
	Log.Info("%s: %d resource types, %d listable; pack: %d rules, %d list-arg specs",
		w.Provider().Source, len(s.Resources), len(s.Lists), len(pk.Rules), len(pk.ListArgs))
	w.recordStage("init", started, nil)
	return nil
}

// RenderConfig is a first desprawl.hcl: what init was told, and the other
// settings commented out with their defaults.
func RenderConfig(ps []model.Provider, draw []string, output string) []byte {
	var b strings.Builder
	b.WriteString("# desprawl workspace: see `desprawl help`.\n")
	for _, p := range ps {
		fmt.Fprintf(&b, "\nprovider %q {\n  source = %q\n", p.Name, p.Source)
		if p.Alias != "" {
			fmt.Fprintf(&b, "  alias = %q\n", p.Alias)
		}
		if p.Version != "" {
			fmt.Fprintf(&b, "  version = %q\n", p.Version)
		}
		if len(p.Env) > 0 {
			var kv []string
			for _, k := range slices.Sorted(maps.Keys(p.Env)) {
				kv = append(kv, fmt.Sprintf("%s = %q", k, p.Env[k]))
			}
			fmt.Fprintf(&b, "  env = { %s }\n", strings.Join(kv, ", "))
		}
		b.WriteString("  config {\n")
		for _, l := range strings.Split(strings.TrimSpace(p.Body), "\n") {
			if l != "" {
				b.WriteString("    " + strings.TrimSpace(l) + "\n")
			}
		}
		b.WriteString("  }\n}\n")
	}
	b.WriteString("\n")
	if len(draw) > 0 {
		var q []string
		for _, d := range draw {
			q = append(q, fmt.Sprintf("%q", d))
		}
		fmt.Fprintf(&b, "draw = [%s]\n", strings.Join(q, ", "))
	} else {
		b.WriteString("# draw = [\"../infra\"]\n")
	}
	if output != "" {
		fmt.Fprintf(&b, "output = %q\n", output)
	} else {
		fmt.Fprintf(&b, "# output = %q\n", paths.DefaultOutput)
	}
	return []byte(b.String())
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func joinDiags(diags []tf.Diagnostic) string {
	var out []string
	for _, d := range diags {
		out = append(out, d.String())
	}
	return strings.Join(out, "; ")
}

func errStrings(errs map[string]error) map[string]string {
	out := map[string]string{}
	for k, e := range errs {
		out[k] = e.Error()
	}
	return out
}

func set(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}
