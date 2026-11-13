// Package model holds the types shared between pipeline stages.
package model

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/askrike/desprawl/internal/paths"
)

// Target is one live resource to be imported into Terraform. It is
// identified either by a resource identity (from `terraform query`) or by
// a classic import ID (added by hand or by an agent).
type Target struct {
	Type     string         `json:"type"`               // terraform resource type, e.g. aws_iam_role
	Name     string         `json:"name"`               // terraform resource name (label)
	Identity map[string]any `json:"identity,omitempty"` // resource identity
	ID       string         `json:"id,omitempty"`       // import ID, when there is no identity
	Display  string         `json:"display,omitempty"`  // human-readable name from the provider
	Added    bool           `json:"added,omitempty"`    // added by hand or by an agent, not by discovery
	// Provider is the provider block it's imported with, when the
	// workspace has more than one of its provider: "aws.us".
	Provider string `json:"provider,omitempty"`
	// Origin says where a target not listed by the provider came from:
	// "inventory" for targets found by an inventory data source.
	Origin string `json:"origin,omitempty"`
	// Attrs are what an inventory mapping's named groups took from the
	// entry's ID, by group name. Discovery uses the ones named after the
	// type's attributes; they aren't written out.
	Attrs map[string]string `json:"-"`
}

func (t Target) Address() string { return t.Type + "." + t.Name }

// Key identifies the remote object regardless of the terraform name.
func (t Target) Key() string {
	if t.ID != "" {
		return t.Type + "\x00id\x00" + t.ID
	}
	b, _ := json.Marshal(t.Identity) // map keys marshal sorted
	return t.Type + "\x00identity\x00" + string(b)
}

// Workspace is a directory with a desprawl.hcl (Config) that all stages
// operate on; README's "Where desprawl writes" lists everything else.
//
//	<dir>/desprawl.hcl                  settings: providers, what to draw, output (you edit)
//	<dir>/.desprawl/.gitignore          keeps state/ out of git
//	<dir>/.desprawl/overrides.json      fixes for generated config (you edit)
//	<dir>/.desprawl/layouts/            map layouts
//	<dir>/.desprawl/pack/<provider>/    fixes to provider knowledge
//	<out>/providers.tf                  written from desprawl.hcl (out: Config.Output)
//	<out>/imports.tf, main.tf, data.tf  the imported Terraform
//	<dir>/.desprawl/state/              machine state, account data, never committed:
//	    targets.json      discovered targets
//	    discovery.json    what discovery could and couldn't list, and the inventory read
//	    generated.tf      raw config from terraform -generate-config-out
//	    plan.json         imported values (sensitive values scrubbed)
//	    left-out.json     targets left out of the config, with reasons
//	    last-generate.json  what the last generate was given and kept (partial reruns)
//	    schema.json       provider schema subset
//	    inventory.json    what inventory sources found, by kind
//	    links.json        every reference link made, with its evidence
//	    verify.json       where the linked config differs from the cloud
//	    stages.json       each stage's last run, with its inputs' hashes
//	    tmp/<sub>/        scratch terraform dirs (query/<type>, inventory, pack checks)
type Workspace struct {
	// Dir is the workspace: the directory holding desprawl.hcl.
	Dir string
	// Out is where the imported Terraform goes and terraform runs; empty
	// means Dir.
	Out string
}

// Path is name in the imported Terraform's directory, where terraform runs.
func (w Workspace) Path(name string) string { return filepath.Join(w.out(), name) }

func (w Workspace) out() string {
	if w.Out != "" {
		return w.Out
	}
	return w.Dir
}

// StatePath is name in the workspace's state directory.
func (w Workspace) StatePath(name string) string { return filepath.Join(paths.State(w.Dir), name) }

// PlanFile is the absolute path of a plan file, in the state directory:
// it holds unscrubbed values until it's deleted after reading.
func (w Workspace) PlanFile(name string) string {
	p, _ := filepath.Abs(w.StatePath(name + ".tfplan"))
	return p
}

// TFDir returns the state's tmp/<sub>, a directory for running terraform
// on generated config with the workspace's provider setup (linked, not
// copied).
func (w Workspace) TFDir(sub string) (string, error) {
	root, err := filepath.Abs(w.Dir)
	if err != nil {
		return "", err
	}
	out, err := filepath.Abs(w.out())
	if err != nil {
		return "", err
	}
	dir := filepath.Join(paths.Tmp(root), sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, name := range []string{".terraform", ".terraform.lock.hcl", "providers.tf"} {
		link := filepath.Join(dir, name)
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join(out, name), link); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func (w Workspace) ReadJSON(name string, v any) error {
	b, err := os.ReadFile(w.StatePath(name))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	return nil
}

func (w Workspace) WriteJSON(name string, v any) error {
	if err := os.MkdirAll(paths.State(w.Dir), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(w.StatePath(name), append(b, '\n'), 0o644)
}
