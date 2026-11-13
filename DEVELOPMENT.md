# Working on desprawl

README.md is for users. This is how desprawl works inside, and how to change it. AGENTS.md
has the repository layout and working rules.

## Build, check, release

```sh
go build -o bin/desprawl ./cmd/desprawl
go test ./...                                    # every example against internal/tfmap/testdata/golden
go test ./internal/tfmap ./internal/link -update # accept an intended golden change (review the diff first)
scripts/check-ui.sh                              # renders every example in headless Chrome
go run ./cmd/desprawl -C examples -layouts ../layouts  # serve the examples (their layouts are in layouts/)
```

Each `v*` tag is released by GoReleaser (`.goreleaser.yaml`, `.github/workflows/release.yml`),
with the tagged commit's message as the release notes. Set `DESPRAWL_PROFILE=<file>` to append
one JSON line per command, terraform process, type listing and fix round (name, start, seconds,
attributes).

## Code layout

- `internal/tfmap`: drawing Terraform as a map, with no terraform CLI: parse, classify,
  connect, lay out, route, the ASCII view and diffs against git. Pure, and pinned by the
  goldens.
- `internal/rea`: reverse-engineering a workspace: running the stages (`internal/discover`,
  `generate`, `link`, `verify`) with its provider packs, what each couldn't do with fixes
  (`problems.go`), and where the workspace stands in the loop (`status.go`).
- `cmd/desprawl`: the shell around them: the command line (`main.go`, `maps.go`, `rea.go`),
  the map server and its page (`serve.go`, `index.html`, `overview.html`, and
  `split.html`, the page at `/`: the page's bar (Overview or Maps, open a map, search, display
  options, what's running), the workspace's overview (`overview.html` at `/overview?embed` in an
  iframe: its maps and its import's stages) or windows side by side, each with its own title bar (view, info, reset, maximize,
  close) and `index.html` at `/map?embed` in an iframe, which keeps only the map and its zoom; `screen.go` holds the windows in the server so the page and an agent share them
  (the address stays `/`: a reload, another tab or the browser an agent opens all show the server's screen),
  `windows.go` the MCP tools for them), the MCP server
  (`mcp.go`, with `view.go` for what an agent puts on the page), `setup.go` and `update.go`.
  Commands parse flags and print; the work is in the packages.

## How a config becomes a map

1. **Parse** (`internal/tfmap/graph.go`). Every block is a node; every reference in any attribute
   is an edge labelled with that attribute. Module inputs and outputs connect references
   across modules; `each.value` reaches what `for_each` iterates over, and `local.m["k"]` only
   the `k` entry of a map literal.
2. **Classify** (`internal/tfmap/catalog.go` and `abstract.go`, `catalog/*.json`). Each resource type has a role, looked up per
   provider (see the table in README). Types not in a catalog are treated as wiring. A
   variable or local that is *exactly* a component's name stands for it (so
   `arn:…:table/${var.table}` reaches the table); resource policies (`inverse`) point from the
   caller to the resource, as do attributes in `inverse_attrs` (a security group's `ingress`);
   `depends_on` is ignored.
3. **Connect.** All references between two components merge into one arrow, labelled by
   meaning: `access` (through IAM), `network` (security groups), a connector's verb, or `uses`.
4. **Lay out** (`internal/tfmap/layout.go`, `route.go`). Components sit on the saved grid (or the flow-ordered
   seed); edges are routed in Go, right-angled through the gaps between cards, one lane per
   edge, so `desprawl map`, `layout tidy` and the browser all score what the browser draws.

Catalogs: `desprawl unclassified [config]` lists types a config uses that no catalog covers;
`scripts/provider-types.sh <provider> <initialized dir> [prefix]` lists a provider's types with
their reference-like attributes, to classify in `catalog/<provider>.json` (the roles are in
the comment at the top of `internal/tfmap/catalog.go`).

## How reverse-engineering works

1. **Discover** with `terraform query`: the provider lists its own resources and returns their
   identities. Types listable only within a parent (services in a cluster, records in a zone)
   run in later rounds with one list block per parent. Revisions collapse to the latest.
2. **Generate** with `terraform plan -generate-config-out`, then repair what Terraform generates
   but can't validate (the emptiest offending attribute is removed), reconcile drift with the
   imported values (verify backs out any its plan rejects), and apply `overrides.json`.
   The IDs the first plan reads are probed as the unlistable types documented with the same
   import ID (an association named by the subnet it attaches to); the plan decides which exist,
   and a probe is kept only if its config names something its object's config doesn't.
3. **Link**: index every resource's identity attributes (from the provider schema), then
   rewrite literals. Values shaped like generated identifiers and owned by one resource are
   linked wherever they appear, including inside policy documents; plain names only where a
   rule says that attribute names that kind of resource; scope values (account, project,
   region) become data sources; reference cycles are broken by leaving one edge literal.
4. **Verify**: plan the result; anything other than config-only drift means the config doesn't
   match reality.

While they run, the stages log through `internal/logs` at three levels: `Info` for what a
command did and warnings its problems don't list, `Verbose` (`-verbose`) for what it's doing
and why it takes time, and `Debug` (`-debug`) for each resource's and retry's details and lines
the problems repeat. Results go to stdout; logs and progress go to stderr. Messages use
desprawl's words, not terraform's change verbs (exclude, left out, doesn't match the cloud),
so nobody reads them as desprawl changing something.

### Provider packs

Provider knowledge is data, in `providers/<name>/`, built into the binary; a file in a
workspace's `.desprawl/pack/<name>/` replaces its built-in namesake (so the first fix to a
file copies the shipped one there: `packFile` in `internal/rea/problems.go`). Fixes worth
sharing are copied into `providers/<name>/` and built in. desprawl never calls a model: a stage
prints what the pack is missing with the entry to add (`internal/suggest` finds candidates
from the schema and the workspace), whoever runs it adds the entries, and running the stage
again checks them:

| file | built by |
|---|---|
| `rules.docs.json`, `imports.docs.json` | `mine-docs`: every reference and import example in the provider's docs |
| `list_args.json` | `discover`'s suggestions: list arguments mapped to parent resources; the next discover lists with them or reports the failure |
| `pack.json` | `link`'s suggestions: scope parameter data sources (verify shows a wrong one as drift); by hand: excluded types and attributes, skip filters and `query_provider` |
| `inventory.json` | `discover`'s suggestions: inventory data sources and how each kind maps to a type and import ID (generate leaves out an import that fails) |
| `rules.llm.json` | `link`'s suggestions: name references judged from real infrastructure, and rejected ones (`"rejected": true`) |
| `aliases.json` | by hand, reviewed: a wrong alias evaluates to the same string, so no plan rejects it |

Entries added to a pack are checked before they're used (`internal/suggest/check.go` against
the schema; `internal/rea/packcheck.go` against the provider, only for entries that differ
from the shipped pack); one that fails is left out and printed. Account data in added lines
is printed too (`packLeaks`).

See `providers/README.md` for the pack files.

### Workspaces

A workspace is a directory with a `desprawl.hcl` (`internal/model/config.go`): provider
blocks, `draw` entries and `output`. Commands work on the one in the current directory;
`-C DIR` changes directory first, as with git. Nothing searches upward. Each provider block
is scoped on its own (`scoped` in `internal/rea/providers.go`): `discover` runs once per block and records
it on each target (`"provider": "aws.us"`), problems are built per block
(`perProvider`), and `generate`, `link` and `verify` work on all blocks at once.
`providers.tf` in the output directory is rewritten from `desprawl.hcl` before each stage,
and `terraform init` reruns when the providers changed. Terraform runs in the output
directory.

### Workspace state

Every location is decided in `internal/paths`; README's "Where desprawl writes" lists them.
A workspace's `.desprawl/` holds what you commit: `overrides.json`, `layouts/`, `pack/` and,
by default, the imported Terraform (`imported/`); and a `.gitignore` keeping `state/` out of
git. `state/` (see `internal/model`) holds `targets.json`, `discovery.json`, `inventory.json`,
`generated.tf`, `plan.json` (scrubbed), `left-out.json`, `last-generate.json`,
`reconciled.json`, `schema.json`, `links.json`, `verify.json`, `stages.json` (each stage's last
run, for `status`, the page's overview (`cmd/desprawl/overview.html`)
and the `workspace_status` MCP tool: `internal/rea/status.go`), `progress.json`,
`terraformrc` and `tmp/` (scratch terraform directories). Plan files are written to `state/`
and deleted after reading. Scope parameters (account, project, region) aren't stored: they're
computed from `targets.json`.
