# desprawl — notes for coding agents

Read README.md (what users do) and DEVELOPMENT.md (how it works) first.

## Repository layout

One Go module (`github.com/askrike/desprawl`) and one binary, `desprawl`
(`go build -o bin/desprawl ./cmd/desprawl`). With no command it serves diagrams; `ls`, `map`,
`diff`, `layout`, `unclassified` and `html` work on maps from the shell; `init`, `discover`, `generate`,
`link`, `verify`, … reverse-engineer live infrastructure into linked Terraform. Every
command works on the workspace in the current directory: a directory with a `desprawl.hcl`
(`internal/model/config.go`). `-C DIR` before the command runs it as if from `DIR`, as with
git; nothing searches upward. The map commands take a configuration path in the workspace.

- `cmd/desprawl/`: the shell: commands parse flags and print, the work is in the packages
  below. `main.go` dispatch and help, `maps.go` the map commands, `rea.go` the
  reverse-engineering ones; `serve.go` the map server with `index.html`, `overview.html` and `split.html` (the page
  at `/`: the workspace's overview, `overview.html`, or its maps in windows side by side, each
  `index.html` at `/map?embed` in an iframe, all fed by its one event stream); `screen.go` the
  tab (overview or maps) and the windows, held in the server so the person and an agent
  (`windows.go`: `get_windows`, `arrange_windows`, `control_window`) change the same ones;
  `mcp.go` the MCP server (`desprawl mcp`), with `view.go` (what an agent shows on the open
  page); `setup.go` and `harnesses.json` install it and the skills into agent harnesses.
- `internal/tfmap/`: drawing Terraform as a map: parse (`graph.go`), catalogs and folding
  (`catalog.go`, `abstract.go`), layout and routing (`layout.go`, `route.go`), the ASCII view
  (`text.go`), git diffs (`diff.go`), which configurations a workspace draws (`configs.go`);
  test goldens in `testdata/`.
- `internal/rea/`: reverse-engineering a workspace: running the stages (`run.go`), their
  problems with fixes (`problems.go`), where the workspace stands in the loop (`status.go`:
  `status`, the page's overview, the `workspace_status` tool), one provider block's scope
  (`providers.go`), pack checks (`packcheck.go`, `leaks.go`).
- `skills/`: the skills users get (embedded, installed by `desprawl setup`, portable
  SKILL.md: no harness-specific features). The `desprawl-map` skill is also the MCP
  `map_infrastructure` prompt.
- Releases: `.goreleaser.yaml`, run by `.github/workflows/release.yml` for each `v*` tag.
- `internal/`: the reverse-engineering pipeline (discover, generate, link, verify, suggest…).
- `providers/`: provider packs, everything provider-specific as data: how the map draws a
  provider's types (`catalog.json`) and how the reverse-engineering commands handle them.
- `examples/`, `layouts/`: Terraform examples and their saved layouts. `examples/desprawl.hcl`
  (`draw = ["."]`) makes `examples/` a workspace that draws every example.
- `scripts/`: helper scripts; run them from anywhere.

## Design decisions (settled, don't re-litigate)

- **Keep things as simple as possible.** Prefer a catalog change over new code, and new code
  over new concepts.
- **Abstraction level:** only things that run
  code, store data or take traffic are boxes; everything else is wiring. Boxes keep the
  provider's own labels ("ECS service", "S3 bucket"). Determinism matters: same Terraform plus
  same catalog gives the same picture.
- **Layout:** follow the flow of information left to right. Flow beats keeping a module's
  components together. Grouping by logical runtime groups was considered and declined.
- **Edges:** right-angled, routed by desprawl itself (`route.go`), so metrics, `layout tidy` and the
  ASCII view score what the browser draws.
- **Networks** (VPC etc.) are ignored rather than drawn as a box or a container.

## Working rules

- After any change to `internal/tfmap` or a catalog, run `go test ./...`. If goldens
  change, check each diff makes sense before `go test -update`. Run `scripts/check-ui.sh`
  after UI changes. Changes have broken examples that weren't re-checked before.
- `cmd/desprawl/index.html` is embedded in the binary: rebuild
  (`go build -o bin/desprawl ./cmd/desprawl`) and restart the server after editing it.
  Syntax-check its script first: extract the largest `<script>` block and run `node --check`. A syntax error leaves the page blank with no error shown.
- Headless Chrome: use a fresh `--user-data-dir` per run, or it serves a cached page.
- This repo's layouts live in `layouts/`, not `examples/.desprawl/layouts/`: always pass
  `-layouts ../layouts` from the examples workspace
  (`go run ./cmd/desprawl -C examples serve -layouts ../layouts` to serve,
  `go run ./cmd/desprawl -C examples map -layouts ../layouts X`, likewise `layout seed|tidy`).
- The headless check uses `--virtual-time-budget`; the page's live-update stream
  (`/api/events`) keeps a request open, so for screenshots of the real server use `--timeout`.
- Never overwrite `layouts/*.json` without saying so: people drag components and those
  positions are theirs. Say what you replaced.
- Every place desprawl writes is decided in `internal/paths` and listed in README's "Where
  desprawl writes": a new location goes in both.

## Known gaps

- Fully hardcoded ARNs (no shared variable) don't link to their component.
- Remote (registry/git) modules are opaque boxes until `terraform init` installs them in
  `.terraform/modules`; then they're expanded like local ones.
- The GCP and Azure catalogs predate the component-vs-wiring rule. GKE cluster, Container
  Apps environment and Application Insights are now wiring, but DNS zones, container
  registries and the AKS cluster are still components there while AWS treats them as wiring.
- Edge direction follows references, which sometimes runs against traffic (an ECS service
  registers in a Cloud Map namespace, so the arrow points service → namespace).

## Reverse-engineering commands (`internal/rea` and the stage packages, `providers/`)

To run the workflow on an account, use the `desprawl-reverse-engineer` skill
(`skills/`); the pack format is in `providers/README.md`. Rules:

- **Provider-agnostic code.** Nothing in `internal/` or `cmd/desprawl/rea.go` may name a
  provider, resource type or cloud API. Provider knowledge is data in
  `providers/<name>/` (see `providers/providers.go`), produced by
  `mine-docs` or added from the stages' suggested fixes (which go into a
  workspace's `.desprawl/pack/<name>/` first);
  alias patterns (`aliases.json`), skip filters (`pack.json` `skip`) and
  query provider settings (`pack.json` `query_provider`) are the
  hand-reviewed exceptions (see below).
  The only provider-shaped code is in tests and testdata.
- **Read-only against the cloud.** Terraform runs `init`, `query`, `plan`,
  `show` and `providers schema`, never `apply`. Plan files are deleted after
  reading; values written to `.desprawl/state/` are scrubbed of anything the
  provider marks sensitive (`generate/scrub.go`). A sensitive value is never
  written; when it equals a non-sensitive identifier exactly one other
  resource owns, config gets that owner's reference instead
  (`generate/sensitive.go`).
- **Verify is the oracle.** Any change to generation or linking must keep
  `verify` clean on a real workspace (no non-config-only drift).
- **desprawl never calls a model.** People use it through their own agent
  or by hand. Each stage (discover, generate, link, verify) prints what it
  couldn't do to stdout, grouped by kind, each problem with the fix to copy
  where `internal/suggest` or the hint can find one (`internal/rea/problems.go`;
  `-json` for one object per problem). Fixes go only into the stages'
  inputs (`.desprawl/overrides.json`, `targets.json`, the workspace's pack), and are checked before
  they're used: pack entries against the schema (`suggest/check.go`) and,
  where they differ from the shipped pack, against the provider
  (`rea/packcheck.go`: mappings import, partitions and per-parent reads do what
  they say, scope data sources return the discovered value); overrides by
  the next plan. Alias patterns are the exception:
  a wrong alias link evaluates to the same string, so the plan can't reject
  it (an inferred RDS pattern linked parameter groups that only share a
  family name). They are written by hand and reviewed. So are skip
  filters (resources the provider or another resource manages: automated
  backups, provider-owned keys): terraform imports them fine and can't say
  who manages them. Query provider settings (fewer retries, so a failing
  list API is reported in seconds) change only how discovery's queries
  run, not what a pack says about resources. Terraform is the only runtime
  dependency; never shell out to cloud CLIs.
- **Packs hold no account data.** Packs are shared, and fixes to them are
  made from a real account: `desprawl -C D check-pack` audits workspace D's
  pack with `internal/privacy` (tokens from the account's values that
  the provider's docs, `vocabulary.docs.txt`, and schema don't use are
  leaks).

### Pipeline

| stage | package | in -> out (`<ws>/.desprawl/state/…` unless noted; `<out>` is the output directory, `.desprawl/imported/` by default) |
|---|---|---|
| init | `rea` | flags or questions -> `<ws>/desprawl.hcl`; with a provider, `<out>/providers.tf`, `schema.json` |
| discover | `discover`, `inventory` | schema, pack list-args + inventory, once per provider block -> `targets.json` (each with its block), `discovery.json`, `inventory.json` |
| generate | `generate` | targets, `<ws>/.desprawl/overrides.json` -> `generated.tf`, `plan.json`, `left-out.json`, `reconciled.json`, `last-generate.json`, `<out>/imports.tf` |
| link | `link`, `index`, `rules` | generated.tf, plan.json, pack rules, scope params from targets -> `<out>/main.tf`, `<out>/data.tf`, `links.json` |
| verify | `verify` | `<out>/*.tf`, `reconciled.json` -> `verify.json` (backs out reconciled values its plan rejects) |
| mine-docs | `mine` | provider docs (registry -> GitHub) -> pack `rules.docs.json`, `imports.docs.json` (the workspace's `.desprawl/pack/<name>/`, or `-out`) |
| (every stage) | `rea` (`problems.go`), `suggest` | its outputs + schema -> problems with suggested fixes on stdout; `run` and `status` print all stages' |

Key ideas, in the code's own words:
- `discover`: types share `terraform query` processes (one per slot: the
  provider's start-up costs seconds per process), in rounds (children after
  parents), failing blocks retried away, versions collapsed. Types a shared
  query couldn't settle (timeout, truncation) are listed alone.
- `generate/repair.go`: generated config fails validation by saying too
  much; repairs always remove the emptiest offending attribute. What no
  repair handles is left out with a `Hint` (`generate/hint.go`): what the
  config sets and the override the error points at.
- `generate/incremental.go`: a rerun regenerates only what was left out,
  whose override changed, or references those; the rest stay in config
  (`kept.tf` during the run) and plans `-target` the rest.
- `generate/probe.go`: unlistable types documented with an imported
  resource's own import ID are probed at that ID; kept if the plan imports
  them and their config links something new.
- `index.Unique`: values linked without rules are those owned by one
  resource and shaped like generated identifiers.
- `link`: two passes; reference cycles broken by DFS back edges. Every
  edge records its evidence (`link/evidence.go`): observed / derived /
  inferred, a basis and limitations; `why` prints it.

### Commands

```sh
go test ./internal/link -update   # accept a golden change after reviewing it
```

Workspaces go in `ws/` (gitignored), each a directory with its own `desprawl.hcl`. Pack
fixes made there land in `ws/<name>/.desprawl/pack/<provider>/`; copy the ones worth sharing
into `providers/<provider>/` and rebuild.
