---
name: desprawl-map
description: Generate a cloud map (architecture diagram) of a project's infrastructure with desprawl, from its Terraform, showing the services, data stores and entry points and how they connect. Use when asked to map, draw, diagram or visualize a project's infrastructure or cloud architecture, or when desprawl is mentioned.
---

# Map a project's infrastructure with desprawl

desprawl reads Terraform (`.tf` files) directly and draws what someone would
draw on a whiteboard: only things that run code, store data or take traffic
are boxes; IAM, security groups and other wiring become the arrows between
them. Drawing needs no Terraform CLI and no cloud credentials.

Use desprawl's MCP tools (`use_workspace`, `list_configs`, `get_map`,
`open_map`, `show_map`, `show_changes`, `seed_layout`, `save_layout`,
`tidy_layout`, and for the page's windows `get_windows`, `arrange_windows`,
`control_window`). Without the tools, the same works from the shell, run in the
workspace: `desprawl` serves the map, `desprawl ls` lists the
configurations, `desprawl map <config>` prints one as text and
`desprawl diff <config>` lists what changed since HEAD.

## 1. Find the Terraform

desprawl works on a **workspace**: a directory with a `desprawl.hcl` whose
`draw` entries list the Terraform to draw (`draw = ["infra"]`, paths
relative to the file). The session's workspace is the project directory
when it holds a `desprawl.hcl`; for another, call `use_workspace` with its
absolute path. If the project has none, look for its Terraform first (the
directories with `.tf` files), then create one in the project root with
`desprawl init -draw <dir>,...` (`desprawl init -draw .` draws everything
under the root), then call `use_workspace` with that directory. Tell the
user: it writes only `desprawl.hcl`, which they can commit.

Then run `list_configs`. It lists the workspace's configurations by their
path in it; every other tool takes one of those as `config` (default: the
first). A project often has several configurations (one per environment or
stack, and modules); the ones that call the others are the ones to map.
Local modules (`source = "./…"`) are
expanded in place, and so are registry and git modules once `terraform init`
has installed them (in `.terraform/modules`); otherwise each is one opaque
box. Don't run `terraform init` without asking.

If there is no Terraform, say so. desprawl can reverse-engineer a live
cloud account into Terraform first: offer that (the
`desprawl-reverse-engineer` skill), and don't start it without the user's
go-ahead, since it reads their account.

## 2. Read each map

Run `get_map` on each configuration you'll show. It returns the components
with their ids, the connections, a drawing of the layout and its metrics.
Check it tells the story a person would tell: where traffic comes in, what
handles it, what that touches. Look for:

- **Components with no connections.** Usually a reference the Terraform
  hardcodes (an ARN typed out instead of `aws_x.y.arn`), so desprawl can't
  see the link. Mention it; don't edit the user's Terraform to fix it unless
  asked.
- **Types not in desprawl's catalog** (listed at the end). Unknown types are
  treated as wiring, so a real component of that type is missing from the
  map. Name them to the user.
- **Parse errors** (listed at the end): the rest of that file may be missing
  from the map. Tell the user which file.
- **Services the code uses that the Terraform doesn't create** (a SaaS API
  called with a key, a database managed elsewhere). Skim the code and deploy
  config for them and list them separately: the map shows only what the
  Terraform declares.

## 3. Arrange (only if needed)

Until a layout is saved, the map uses an automatic flow-ordered one, which is
usually fine. If it reads badly, use the `desprawl-layout` skill (MCP prompt
`arrange_map`). A saved
layout is the user's work (they drag components in the browser): never
replace one without saying so.

## 4. Show it

Run `open_map` with `config` set to the configuration you mapped: it opens
the browser on it, with every configuration of the workspace in a picker. Then tell the user, briefly:

- which configuration(s) you mapped and the main flow, in a sentence or two;
- what's missing or uncertain (unconnected components, unknown types,
  opaque modules, services outside the Terraform);
- that dragging components saves the layout to `.desprawl/layouts/` in the
  workspace, which they can commit for their team.

## 5. Point things out on the map

The map is also where you explain. Whenever an answer is about specific
parts of the infrastructure, show them rather than only describing them:

- **"What did you change?"** Run `show_changes` on the configuration. It
  compares the `.tf` files on disk with git `HEAD` (pass `base`, e.g.
  `main`, for work you've committed) and marks every added, changed and
  removed resource and connection, down to the attributes, with each
  block's diff in the side panel. Add a `note` saying why, in a sentence
  or two. Its result lists the changes, so use it to answer in text too.
- **Anything else** (a request's path, a security finding, what a
  migration touches, where a cost comes from): `show_map` with marks on the
  components, resources or connections (`"<from id> -> <to id>"`) involved.
  Pick the kind (`highlight`, `added`, `changed`, `removed`, `warning`),
  give each a few-word `note`, and use `focus` to fade everything else. Use
  `zoom: "resources"` when the point is an attribute or a folded-in
  resource (an IAM policy, a security group rule), so the cards show them.

Each call replaces the previous marks, and the page that's open follows it
(a new tab opens only if none is). Ids are the component ids from `get_map`
and Terraform addresses for resources; the result names any it couldn't
find.

## 6. Arrange the page

The page has two tabs: the workspace's overview (its maps, and how far an
import of a cloud account got) and the maps, which can sit side by side in
windows the user moves, resizes and zooms; you can do all of it too.
`get_windows` says which tab is open and what each window shows: its configuration, zoom, the components in view, what's
selected. `arrange_windows` switches tabs (`page`: overview or maps), opens, closes,
moves and resizes windows (e.g.
staging beside prod to compare them) and sets the page's search and
display options, which apply in every window. `control_window` does in one
window what the user does there with the mouse: zoom, pan, fit, centre on a
component, select one to open its details, switch between components and
resources. A window keeps the configuration it opened on: to show another,
open a window for it; `show_map` and `show_changes` open one when no window
shows their configuration (or take `window` to pick one that does).
Check `get_windows` before rearranging: the user may have set things up.
