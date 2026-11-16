---
name: desprawl-reverse-engineer
description: Reverse-engineer a live cloud account, project or subscription into Terraform whose resources reference each other (aws_iam_role.x.arn instead of hardcoded ARNs), for any Terraform provider, and draw it as a desprawl map. Use when asked to export, reverse-engineer or import existing cloud infrastructure as Terraform, or to map infrastructure that has no Terraform.
---

# Reverse-engineer infrastructure into linked Terraform

The `desprawl` CLI does the deterministic work and never calls a model:
where it needs a judgment, it prints the problem with a suggested fix, and
you decide. Everything is read-only against the cloud. **Never run
`terraform apply`** in a workspace.

A **workspace** is a directory with a `desprawl.hcl`: one `provider` block
per account (or region, or project), and `draw`: the Terraform the map
shows. `init` lists the imported Terraform's directory there; it's drawn
only while it's listed, so keep it, and add any existing Terraform to draw
beside it. Every command below runs in the workspace (`cd`
there, or `desprawl -C <ws> <command>`). It holds account details (IDs,
names, ARNs) in `.desprawl/`, and the imported Terraform goes to
`.desprawl/imported/` unless `desprawl.hcl` sets `output`. Ask the user
where to put it, together with the scope in step 1; if they have no
preference, propose a directory outside their git repositories and say
which before running `init`. Never pick a location silently. `init`
prints every place it writes (the workspace, the imported Terraform, the
shared provider download cache, the workspace's pack): pass that on to the
user.

Requires Terraform >= 1.14 (`terraform query`, identity-based import);
check with `terraform version` first.

## 1. Scope and credentials

Ask the user (or infer from the request) which provider, which account /
project / subscription, and which region(s). Read-only credentials are
enough. Pass credentials through the environment, never into files: `env`
in `desprawl.hcl` is for a profile name, not a secret, since the file is
committed. Without a terminal, `init` takes flags:

```sh
mkdir -p <ws> && cd <ws>
desprawl init -no-progress -provider hashicorp/aws \
  -config 'region = "eu-central-1"' -env AWS_PROFILE=<profile> -output <dir> \
  && desprawl run -no-progress
# google:  -provider hashicorp/google  -config 'project = "p"' -config 'region = "europe-west1"' -env GOOGLE_PROJECT=p
# azurerm: -provider hashicorp/azurerm -config 'features {}' -config 'subscription_id = "…"'
# (one -config per attribute)
```

Chain the loop's first `run` (step 2) onto `init` like this: it runs only
if `init` succeeds, and saves a round trip.

`-output <dir>` sets where the imported Terraform goes (`-output .` puts it
in the workspace itself), and `init` adds it to `draw`. To change
`desprawl.hcl` or `.desprawl/overrides.json` later, edit them with your
file tools, not `sed` or heredocs.

Leave `-version` out, even when the user's existing code pins an older
major version: the generated config imports and plans fine with the newer
one. Newer providers can list more resource types (`init` prints `N
resource types, M listable`), and older ones may need more permissions to
read a resource. If you must pin, compare that `listable` count with an
unpinned `init`, and expect fewer resources.

`init` writes `desprawl.hcl`. Run again with flags, it rewrites the file
from them (losing comments and anything else in it: prefer editing the
file); without flags or a terminal it sets terraform up again, e.g. after
a failed init. For more regions, accounts or providers, add `provider` blocks to
it; two blocks of one provider need an `alias`. Terraform runs once for
every block, so `env` is shared: two blocks may not set one variable
differently. Choose another account in the block's `config` instead:

```hcl
provider "aws" {
  source = "hashicorp/aws"
  alias  = "us"
  config {
    profile = "<another profile>"   # another account; leave out for the same one
    region  = "us-east-1"
  }
}
draw = [".desprawl/imported", "../infra"]   # the imported Terraform, and the user's existing Terraform beside it
```

The next stage picks up any edit: it rewrites the generated
`providers.tf` and reruns `terraform init`. `discover` runs once per block;
`generate`, `link` and `verify` work on all blocks together, so references
between providers (a DNS record holding a load balancer's hostname) are
linked. With several blocks, each printed problem's heading starts with
its block (`aws.us: …`).

Short-lived credentials (SSO sessions) expire mid-run, and providers often
hang on the credential refresh instead of failing. If a stage stalls or
times out, check the credentials first (e.g. `aws sts get-caller-identity`)
and ask the user to log in again. Providers that authenticate through a
CLI (azurerm via `az`) can block on a CLI token call that never returns:
look for a stuck `az account get-access-token` child process. Never enable
`TF_LOG` debug logging on a real account: the log contains credentials.

desprawl ships packs (provider knowledge) for aws, google, azurerm,
kubernetes and auth0. For another provider, run `desprawl mine-docs
-provider <ns>/<name>` in the workspace first; the loop below fills in the
rest. Fixes to a pack go into the workspace, in `.desprawl/pack/<name>/`.

## 2. The loop

```sh
desprawl run -no-progress   # discover, generate, link, verify
```

`desprawl help` lists the commands and `desprawl help <command>` a
command's flags.

Pass `-no-progress` to every command: its progress line redraws itself,
which makes a mess of captured output if your shell looks like a terminal.
The problems go to stdout and a few lines on what the command did go to
stderr. Add `-verbose` only when a run seems stuck or you need to know
what a stage is doing (a slow type, a limit hit); `-debug` shows every
resource's details.

Each stage prints what it couldn't do, grouped by kind. Every group says
which file the fixes go into and which command to run after; every problem
comes with the fix desprawl suggests where it has one. Read it, decide,
edit the file, run the command it names (which reruns from that stage),
and repeat until nothing is printed, or only what you've decided to
accept. `run` ends with a line per stage saying what it produced (the
counts to report) and the next step. Verify runs `terraform plan` on the
output directory: when its line says "verified", that plan imports
everything and changes nothing, so don't run `terraform plan` again
yourself. `desprawl status` prints the same; `-json` gives one object per
problem. Each stage line says whether it ran, failed or is out of date
because an input changed; the `workspace_status` MCP
tool returns the same as data, and the map page's Overview shows it to the
user live (`arrange_windows` with `page: "overview"` takes them there).

On a large account the first `generate` takes a while: every plan reads
each target from the cloud. Reruns only redo the resources left out and
those whose override changed, so fixing a few is quick. Run it in the background and check `status`, which starts
with what a running command is doing (`generate: plan 3 (after fix round
2), 412/1200 resources read (34%), 6m10s in`). Pass that on to the user
rather than waiting silently.

## 3. Deciding

A suggested fix is a starting point, not an answer: desprawl guesses from
names and the schema. Check each one against the error, the provider's
docs (`desprawl import-format -type T` shows a type's import ID) and
what you know of the account. Where there's no suggestion, work out the fix
from the error and the attributes the problem lists.

- **Failed list types**: permission errors and services that aren't
  enabled are expected; mention them, don't chase them.
- **Can't be listed**: only add what matters to the user
  (`add-target -type T -id ID`, with `-provider aws.us` for a block other
  than the type's provider's first); the rest stays out.
- **Left out of the config**: a placeholder must be a value that is
  obviously one and is never applied (`ignore_changes`); never one that
  would change infrastructure if applied. Tell the user every placeholder
  you introduced. Removing an attribute loses that setting: remove the one
  the resource can do without. If a resource doesn't matter, `exclude` it.
- **Maybe references**: add a rule only if the attribute really names that
  kind of resource (a cluster name, a role ID), not when two settings
  happen to be equal ("Disabled", "RS256"). Write the reason in its note.
  Add the ones that aren't with `"rejected": true`, so they stop coming
  back.
- **Types and attributes to exclude** (a pack that has none): exclude
  only types that are data, secrets and provider-owned defaults, never
  infrastructure people configure, and only attributes that are computed
  mirrors of other attributes. If
  none apply, write `"exclude_types": {}` or `"exclude_attributes": []`.
- **Config differs from the cloud**: harmless if the user only wants the
  map; say so. Otherwise fix the attribute with an override.
- **Pack files** are shared with everyone using the pack: keep account
  values (names, IDs) out of rules and notes. If you added to a pack, run
  `desprawl check-pack` before offering to share it.

Every reference in `.desprawl/state/links.json` carries its evidence;
`desprawl why -from A -to B` prints it. Use it to answer "why are
these connected?" and to review `inferred` edges before trusting the map.

## 4. Map it

The imported `main.tf` is ordinary Terraform: map it with the
`desprawl-map` skill (`use_workspace` with the workspace's absolute path,
`list_configs`, then `get_map` and `open_map` on `.desprawl/imported`), or
run `desprawl serve` in the workspace. Its layouts are saved in the workspace's
`.desprawl/layouts/`, since they name real resources. Components without any connection usually mean a
missing target or a value left as text.

## Reporting back

Tell the user: how many resources and of which kinds were exported, what
couldn't be (failed list types that matter, resources left out, types you
didn't add), placeholders you introduced, what verify found, and where the
output is. If you added to a provider pack, offer to contribute it to
desprawl (github.com/askrike/desprawl, `providers/<name>/`) after
`desprawl check-pack` reports no account data. The fixes are the files in
the workspace's `.desprawl/pack/<name>/`.
