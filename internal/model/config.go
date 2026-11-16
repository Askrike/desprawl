package model

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/askrike/desprawl/internal/paths"
)

// Config is a workspace's desprawl.hcl: the cloud accounts to
// reverse-engineer (one provider block each), existing Terraform to draw
// beside them, and where desprawl puts what it makes.
//
//	provider "aws" {
//	  source  = "hashicorp/aws"
//	  version = "~> 6.0"
//	  env     = { AWS_PROFILE = "dev" }
//	  config {
//	    region = "eu-central-1"
//	  }
//	}
//	draw   = ["../infra/prod"]
//	output = ".desprawl/imported"
type Config struct {
	Providers []Provider
	// Draw lists the Terraform directories to draw, relative to the
	// workspace. The imported Terraform (Output) is drawn only if it's here.
	Draw []string
	// Output is where the imported Terraform goes, relative to the
	// workspace (default paths.DefaultOutput).
	Output string
}

// Provider is one provider block: a provider to reverse-engineer with.
type Provider struct {
	// Name is the provider's local name ("aws"): its pack, and its block
	// in the generated Terraform.
	Name string
	// Alias tells two blocks of one provider apart (two regions).
	Alias   string
	Source  string // "hashicorp/aws"
	Version string // a version constraint
	// Env is environment for terraform, e.g. AWS_PROFILE. No secrets:
	// desprawl.hcl is meant to be committed.
	Env map[string]string
	// Body is the config block's body as written: the provider block's
	// contents in the generated Terraform.
	Body string
}

// Ref is how Terraform names the provider configuration: "aws" or "aws.eu".
func (p Provider) Ref() string {
	if p.Alias != "" {
		return p.Name + "." + p.Alias
	}
	return p.Name
}

// Settings are the config block's attributes set to a literal string
// (region = "eu-west-1"), by name.
func (p Provider) Settings() map[string]string {
	f, d := hclsyntax.ParseConfig([]byte(p.Body), "config", hcl.InitialPos)
	if d.HasErrors() {
		return nil
	}
	out := map[string]string{}
	for name, a := range f.Body.(*hclsyntax.Body).Attributes {
		v, d := a.Expr.Value(nil)
		if !d.HasErrors() && v.Type() == cty.String && v.IsKnown() && !v.IsNull() && v.AsString() != "" {
			out[name] = v.AsString()
		}
	}
	return out
}

// IsWorkspace reports whether dir is a workspace: it holds a desprawl.hcl.
func IsWorkspace(dir string) bool {
	_, err := os.Stat(paths.ConfigFile(dir))
	return err == nil
}

// NotWorkspace is the error for a dir without a desprawl.hcl. Commands
// work on the workspace they're given and never look upward; when dir is
// inside one, the error says which, so nobody makes a second inside it.
func NotWorkspace(dir string) error {
	name := dir
	if name == "." {
		name = "the current directory"
	}
	abs, err := filepath.Abs(dir)
	if err == nil {
		for d := filepath.Dir(abs); d != filepath.Dir(d); d = filepath.Dir(d) {
			if IsWorkspace(d) {
				rel, _ := filepath.Rel(abs, d)
				return fmt.Errorf("%s has no desprawl.hcl: it is inside the workspace %s; run desprawl there (desprawl -C %s …)", name, d, rel)
			}
		}
	}
	return fmt.Errorf("%s has no desprawl.hcl, so it isn't a desprawl workspace: run `desprawl init` to make it one (`desprawl init -draw DIR` to draw Terraform you have)", name)
}

// LoadConfig reads dir's desprawl.hcl.
func LoadConfig(dir string) (Config, error) {
	path := paths.ConfigFile(dir)
	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, NotWorkspace(dir)
		}
		return Config{}, err
	}
	return ParseConfig(src, path)
}

// ParseConfig parses a desprawl.hcl. Values must be literals.
func ParseConfig(src []byte, filename string) (Config, error) {
	f, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return Config{}, diags
	}
	body := f.Body.(*hclsyntax.Body)
	c := Config{Output: paths.DefaultOutput}
	str := func(a *hclsyntax.Attribute) (string, error) {
		v, d := a.Expr.Value(nil)
		if d.HasErrors() || v.Type() != cty.String || v.IsNull() {
			return "", fmt.Errorf("%s: %s must be a string", a.SrcRange, a.Name)
		}
		return v.AsString(), nil
	}
	for name, a := range body.Attributes {
		switch name {
		case "output":
			s, err := str(a)
			if err != nil {
				return Config{}, err
			}
			c.Output = s
		case "draw":
			v, d := a.Expr.Value(nil)
			if d.HasErrors() || !(v.Type().IsTupleType() || v.Type().IsListType()) {
				return Config{}, fmt.Errorf("%s: draw must be a list of directories", a.SrcRange)
			}
			for _, e := range v.AsValueSlice() {
				if e.Type() != cty.String {
					return Config{}, fmt.Errorf("%s: draw must be a list of directories", a.SrcRange)
				}
				c.Draw = append(c.Draw, e.AsString())
			}
		default:
			return Config{}, fmt.Errorf("%s: unknown setting %q (output, draw, provider blocks)", a.SrcRange, name)
		}
	}
	for _, b := range body.Blocks {
		if b.Type != "provider" || len(b.Labels) != 1 {
			return Config{}, fmt.Errorf("%s: unknown block %q (provider \"<name>\" { … })", b.TypeRange, b.Type)
		}
		p := Provider{Name: b.Labels[0], Env: map[string]string{}}
		for name, a := range b.Body.Attributes {
			var err error
			switch name {
			case "source":
				p.Source, err = str(a)
			case "version":
				p.Version, err = str(a)
			case "alias":
				p.Alias, err = str(a)
			case "env":
				v, d := a.Expr.Value(nil)
				if d.HasErrors() || !(v.Type().IsObjectType() || v.Type().IsMapType()) {
					return Config{}, fmt.Errorf("%s: env must be { NAME = \"value\" }", a.SrcRange)
				}
				for k, e := range v.AsValueMap() {
					if e.Type() != cty.String {
						return Config{}, fmt.Errorf("%s: env values must be strings", a.SrcRange)
					}
					p.Env[k] = e.AsString()
				}
			default:
				err = fmt.Errorf("%s: unknown provider setting %q (source, version, alias, env, and a config block)", a.SrcRange, name)
			}
			if err != nil {
				return Config{}, err
			}
		}
		for _, cb := range b.Body.Blocks {
			if cb.Type != "config" || len(cb.Labels) != 0 {
				return Config{}, fmt.Errorf("%s: unknown block %q in provider %q (config { … })", cb.TypeRange, cb.Type, p.Name)
			}
			r := cb.Body.Range()
			inner := string(src[r.Start.Byte:r.End.Byte])
			inner = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(inner), "{"), "}")
			p.Body = dedent(inner)
		}
		if p.Source == "" {
			return Config{}, fmt.Errorf("%s: provider %q needs a source, e.g. \"hashicorp/%s\"", b.TypeRange, p.Name, p.Name)
		}
		c.Providers = append(c.Providers, p)
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if seen[p.Ref()] {
			return Config{}, fmt.Errorf("two provider blocks for %s: give one an alias", p.Ref())
		}
		seen[p.Ref()] = true
	}
	// Terraform runs once for every block, so the environment is shared: a
	// second account's profile would be used for the first's too.
	set := map[string]Provider{}
	for _, p := range c.Providers {
		for _, k := range slices.Sorted(maps.Keys(p.Env)) {
			if o, ok := set[k]; ok && o.Env[k] != p.Env[k] {
				return Config{}, fmt.Errorf("provider blocks %s and %s set %s differently, but terraform runs once for every block, so they share one environment: "+
					"choose each block's account in its config block instead (most providers take a profile or credentials setting there), and keep env for what they share", o.Ref(), p.Ref(), k)
			}
			set[k] = p
		}
	}
	return c, nil
}

// dedent trims a block body's common indentation and blank edges.
func dedent(s string) string {
	lines := strings.Split(strings.Trim(s, "\n"), "\n")
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	for i, l := range lines {
		if len(l) >= indent && indent > 0 {
			lines[i] = l[indent:]
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ProvidersTF is the generated Terraform's providers.tf: one
// required_providers entry per provider and one provider block per block.
func (c Config) ProvidersTF() []byte {
	var b strings.Builder
	b.WriteString("# Written by desprawl from desprawl.hcl: edit that instead.\n\nterraform {\n  required_providers {\n")
	done := map[string]bool{}
	for _, p := range c.Providers {
		if done[p.Name] {
			continue
		}
		done[p.Name] = true
		fmt.Fprintf(&b, "    %s = {\n      source = %q\n", p.Name, p.Source)
		if p.Version != "" {
			fmt.Fprintf(&b, "      version = %q\n", p.Version)
		}
		b.WriteString("    }\n")
	}
	b.WriteString("  }\n}\n")
	for _, p := range c.Providers {
		fmt.Fprintf(&b, "\nprovider %q {\n", p.Name)
		if p.Alias != "" {
			fmt.Fprintf(&b, "  alias = %q\n", p.Alias)
		}
		for _, l := range strings.Split(p.Body, "\n") {
			if l != "" {
				b.WriteString("  " + l + "\n")
			}
		}
		b.WriteString("}\n")
	}
	return []byte(b.String())
}

// Env is the environment for terraform: every provider's.
func (c Config) Env() map[string]string {
	env := map[string]string{}
	for _, p := range c.Providers {
		for k, v := range p.Env {
			env[k] = v
		}
	}
	return env
}

// OutputDir is the absolute directory the imported Terraform goes in.
func (c Config) OutputDir(dir string) string {
	if filepath.IsAbs(c.Output) {
		return c.Output
	}
	return filepath.Join(dir, c.Output)
}
