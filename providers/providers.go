// Package providers embeds per-provider packs: everything provider-specific
// that the pipeline needs, as data. A pack directory holds any of:
//
//	rules.*.json         reference rules (mined from docs; rules.llm.json: judged
//	                     from real infrastructure, added from link's suggestions)
//	aliases.json         how configuration shortens identifiers (a name without
//	                     its random suffix); hand-reviewed, since verify can't
//	                     reject a wrong alias link
//	list_args.json       how to fill required arguments of list resources
//	imports.docs.json    documented import IDs/identities per type (mined)
//	vocabulary.docs.txt  every word in the docs, for the privacy guard
//	inventory.json       data sources that enumerate resources, and how their
//	                     entries map to resource types and import IDs
//	pack.json            excluded types, scope parameters, dropped attributes,
//	                     and skip filters for resources managed elsewhere
//	                     (hand-reviewed, like aliases)
//	catalog.json         what each type is on the map (package tfmap); a
//	                     workspace's adds to the shipped one type by type
//
// Packs are optional. Without one, discovery lists only types that need no
// arguments and linking relies on unique values alone. `mine-docs` builds
// part of a pack for any provider; the rest is added from what discover
// and link print as fixes, and checked by running them again.
package providers

import (
	"cmp"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/generate"
	"github.com/askrike/desprawl/internal/inventory"
	"github.com/askrike/desprawl/internal/mine"
	"github.com/askrike/desprawl/internal/rules"
)

//go:embed */*.json */*.txt
var embedded embed.FS

// Catalogs holds the shipped catalogs, as <provider>/catalog.json.
var Catalogs fs.FS = embedded

type Pack struct {
	Rules     []rules.Rule
	Aliases   []rules.Alias
	ListArgs  []discover.ListArg
	Imports   map[string]*mine.ImportExamples
	Inventory *inventory.Inventory
	Config
}

// Config is pack.json.
type Config struct {
	// ExcludeTypes maps types never to discover to the reason, e.g. data
	// plane objects (bucket objects, table items) or secret versions.
	ExcludeTypes map[string]string `json:"exclude_types,omitempty"`
	// Params maps scope parameters found in resource identities (account_id,
	// project, region) to how config should reference them.
	Params map[string]Param `json:"params,omitempty"`
	// ExcludeAttributes are left out of linked config everywhere: computed
	// mirrors of other attributes, such as tags_all. "type.attr" leaves
	// one out only from that type (inline rules that standalone rule
	// resources also manage). nil: the pack hasn't said; empty: none.
	ExcludeAttributes []string `json:"exclude_attributes,omitempty"`
	// Skip leaves out resources the provider or another resource manages
	// (automated backups, provider-owned keys). Written by hand and
	// reviewed, like aliases: terraform can't check who manages a resource.
	Skip []generate.Skip `json:"skip,omitempty"`
	// ManagedBy maps "type.attr", an inline attribute or block, to the
	// standalone types that manage the same things one by one (a dataset's
	// access entries and its IAM members, a security group's inline rules
	// and rule resources). When one of those attached to a resource is
	// imported, link leaves the inline one out of that resource and
	// ignores changes to it, so both don't manage the same grant.
	ManagedBy map[string][]string `json:"managed_by,omitempty"`
	// QueryProvider holds provider block attributes for discover's terraform
	// queries, where providers.tf doesn't set them: e.g. fewer provider
	// retries, so a list API that keeps failing is reported in seconds
	// instead of retried until the timeout.
	QueryProvider map[string]any `json:"query_provider,omitempty"`
}

type Param struct {
	Ref  string `json:"ref"`  // e.g. data.aws_caller_identity.current.account_id
	Data string `json:"data"` // block declaring the data source, e.g. data "aws_caller_identity" "current" {}
}

// Load reads the pack for a provider: the embedded files, each replaced by
// the same file in dir/<provider> if there is one (so packs can be grown in
// place without rebuilding, and a file written there doesn't hide the rest).
// A provider with no pack gets an empty one.
func Load(dir, provider string) (*Pack, error) {
	p := &Pack{}
	files := map[string]bool{}
	for _, fsys := range []fs.FS{embedded, os.DirFS(cmp.Or(dir, "."))} {
		names, _ := fs.Glob(fsys, provider+"/*.json")
		for _, n := range names {
			files[filepath.Base(n)] = true
		}
	}
	for _, f := range slices.Sorted(maps.Keys(files)) {
		b, err := ReadFile(dir, provider, f)
		if err != nil {
			return nil, err
		}
		switch {
		case strings.HasPrefix(f, "rules."):
			var rs []rules.Rule
			err = json.Unmarshal(b, &rs)
			p.Rules = append(p.Rules, rs...)
		case f == "aliases.json":
			err = json.Unmarshal(b, &p.Aliases)
		case f == "list_args.json":
			err = json.Unmarshal(b, &p.ListArgs)
		case f == "imports.docs.json":
			err = json.Unmarshal(b, &p.Imports)
		case f == "inventory.json":
			err = json.Unmarshal(b, &p.Inventory)
		case f == "pack.json":
			err = json.Unmarshal(b, &p.Config)
		}
		if err != nil {
			return nil, &fs.PathError{Op: "parse", Path: filepath.Join(provider, f), Err: err}
		}
	}
	return p, nil
}

// ReadFile reads one file of a provider's pack: dir/<provider>/<name> if it
// exists, else the embedded one.
func ReadFile(dir, provider, name string) ([]byte, error) {
	if dir != "" {
		b, err := os.ReadFile(filepath.Join(dir, provider, name))
		if !errors.Is(err, fs.ErrNotExist) {
			return b, err
		}
	}
	return fs.ReadFile(embedded, provider+"/"+name)
}
