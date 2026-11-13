# desprawl

desprawl does two things:

- **Converts live infrastructure to Terraform**: any cloud or SaaS with a Terraform provider.
- **Draws Terraform as an architecture map**: the components, and the connections between them.

desprawl comes as a CLI, an MCP server and skills for your agent, and a web UI with the live map.

## Install

```sh
curl -fsSL https://desprawl.spawned.app | sh    # into ~/.local/bin (INSTALL_DIR, DESPRAWL_VERSION to change)
desprawl setup                                 # add desprawl to your agents, then restart them
```

Or download an archive for your OS from [Releases](https://github.com/askrike/desprawl/releases)
and put `desprawl` on your PATH, or install with Go 1.27.4 or later:

```sh
go install github.com/askrike/desprawl/cmd/desprawl@latest
```

`desprawl update` replaces the binary with the latest release (`-version V` for another) and
reruns `desprawl setup`; restart your agents afterwards to use the new MCP server.

### Build from source

You need Go 1.27.4 or later (`go.mod`) and git; drawing maps needs nothing else, and
reverse-engineering an account also needs Terraform 1.14 or later.

```sh
gh repo clone askrike/desprawl && cd desprawl       # or git clone git@github.com:askrike/desprawl.git
go build -o ~/.local/bin/desprawl ./cmd/desprawl     # any directory on your PATH
desprawl setup                                     # register its MCP server and install its skills
```

`setup` registers the MCP server with every agent harness it finds (Claude Code, Cursor,
Codex, Gemini CLI) pointing at the binary that runs it, and installs that binary's skills, so
run it from the binary you built; restart your agents afterwards. `desprawl setup -print`
shows the configuration for other harnesses.

To work on desprawl without replacing the installed binary, build into the checkout instead
(`go build -o bin/desprawl ./cmd/desprawl`) and install only its skills, leaving the MCP
server as it is: `bin/desprawl setup -skills ~/.claude/skills` for Claude Code,
`bin/desprawl setup -skills ~/.agents/skills` for Cursor, Codex and Gemini CLI.

A source build reports its version from git (`v0.1.11+dirty`) and `desprawl update` won't
replace it: to update, `git pull`, build and run `desprawl setup` again.

## Turn live infrastructure into Terraform

desprawl imports live infrastructure through any Terraform provider, from clouds to SaaS.
You get Terraform for every resource it finds, in the workspace's output directory (default:
`.desprawl/imported/`):

- **Linked:** resources reference each other instead of repeating IDs, like hand-written Terraform.

  ```hcl
  role = "arn:aws:iam::123456789012:role/app-lambda"   # import tools
  role = aws_iam_role.app_lambda.arn                   # desprawl
  ```

- **Ready to adopt:** with import blocks, so you can copy it into your own repository.
- **Drawn:** desprawl shows it as a map, like any other Terraform.

Importing needs Terraform 1.14 or later and credentials for the account. desprawl only reads:
it never changes anything in your cloud, and it doesn't write secrets to disk.

### With your agent

> turn my AWS account into Terraform with desprawl

Your agent runs the import, fixes what it reports, and runs it again until the Terraform
matches the cloud. We recommend giving your agent read-only credentials, such as AWS's
`ReadOnlyAccess` policy.

### By hand

```sh
mkdir acme-dev && cd acme-dev
desprawl init           # asks for the provider and account
desprawl run            # write the account as Terraform; prints what's left to fix
ls .desprawl/imported   # the Terraform: imports.tf, main.tf, data.tf
desprawl                # see it as a map in your browser
```

See [Fixing an import](#fixing-an-import) for what to do with what `desprawl run` prints.

## Create a diagram from Terraform files

### With your agent

> map my infrastructure with desprawl

Your agent sees the same map you do in the browser, and can drive it. Ask it to:

- **Point things out:** highlight components and connections with notes, like the path a request takes.
- **Show what it changed:** mark everything that differs from a git revision, down to the attributes.
- **Put maps side by side:** open several maps in one view, like staging beside prod.
- **Take you there:** zoom, pan and select in any map, or tidy its layout.

### By hand

In a directory with Terraform:

```sh
desprawl init -draw .     # set up this directory
desprawl                  # open the map in your browser
```

## Reference

### Workspaces

desprawl works on a **workspace**: a directory with a `desprawl.hcl` that says what to draw
and which accounts to import. `desprawl init` writes it; edit it by hand after that.

Every command works on the workspace in the current directory. `desprawl -C DIR …` runs one
as if from `DIR`.

### Fixing an import

`desprawl run` does four things in order: (1) discover the account's resources, (2) generate
Terraform for them, (3) link IDs into references between resources and (4) verify the result
against the cloud. Each is also its own command, like `desprawl generate`.

`desprawl run` prints what it couldn't do, with a fix for each problem. Fixes go in
`.desprawl/overrides.json` or the provider pack in `.desprawl/pack/`; then run again.
`desprawl status` prints the problems again, and `desprawl why` explains why a reference was
made.

The imported Terraform is in the `output` directory `desprawl.hcl` names (default:
`.desprawl/imported/`): plain Terraform you can copy into your own repository, but it holds
account IDs and names, so keep it out of public repositories.

### The map

The map shows your infrastructure's components, anything that runs code, stores data or takes
traffic (services, databases, buckets, load balancers), and the connections between them.
Supporting resources like IAM roles and security groups don't get their own box: they're
folded into the component or connection they belong to.

- Click a component or connection to see its Terraform.
- Drag components to rearrange them.
- Zoom out to see one card per provider.
- The map updates as you save your `.tf` files.

Component positions are saved per configuration as JSON in `.desprawl/layouts/`, so they can
be committed with your Terraform. Deleting a layout file restores the automatic layout.

### Commands

| command | what it does |
|---|---|
| `desprawl` | Open the map in your browser |
| `desprawl init` | Make this directory a workspace |
| `desprawl run` | Import the workspace's accounts |
| `desprawl status` | Where an import stands, and what's left to fix |
| `desprawl diff -open` | What changed since a git revision, marked on the map |
| `desprawl html -o map.html` | The map as one file to share |
| `desprawl setup` | Add desprawl to your agents |
| `desprawl update` | Update to the latest version |

### Help

`desprawl help` lists every command, and `desprawl help <command>` its flags. Resource types
desprawl doesn't know are drawn as wiring; `desprawl unclassified` lists them.

`desprawl setup` finds Claude Code, Cursor, Codex and Gemini CLI. For any other agent that
supports MCP, `desprawl setup -print` shows the config to add.

### Where desprawl writes

`desprawl init` and `desprawl status` print these locations for a workspace.

| what | where | change with |
|---|---|---|
| a workspace's settings | `desprawl.hcl` (written by `init`) | edit it |
| layouts | `.desprawl/layouts/` in the workspace | `-layouts DIR` |
| fixes for generated config, and to provider packs | `.desprawl/overrides.json`, `.desprawl/pack/<provider>/` | |
| the imported Terraform | `.desprawl/imported/` | `output` in `desprawl.hcl` |
| a workspace's state and account data (git-ignored by `.desprawl/.gitignore`) | `.desprawl/state/` | |
| a running command's progress (`desprawl status`, the map page) | `.desprawl/state/progress.json` | |
| provider downloads and schemas, shared by workspaces | `<user cache dir>/desprawl/terraform/` (macOS: `~/Library/Caches`) | `DESPRAWL_CACHE` |
| provider docs (`mine-docs`) | `<user cache dir>/desprawl/docs/` | `DESPRAWL_CACHE` |
| `desprawl diff`: a git revision's `.tf` files, removed afterwards | system temp directory | `TMPDIR` |
| `desprawl html -o FILE` | the file you name (without `-o`, standard output) | `-o` |
| `desprawl update` | the desprawl binary itself (replaced by a rename in its directory) | |
| `desprawl setup` | each agent harness's MCP config (the old one kept as `.bak`) and skills directory | |

Working on desprawl itself? See [DEVELOPMENT.md](DEVELOPMENT.md).

## Contributing and license

Contributions are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md), and report security issues
as described in [SECURITY.md](SECURITY.md).

desprawl is licensed under the [Apache License 2.0](LICENSE). Copyright 2026 Askrike AI AB.
