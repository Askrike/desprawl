---
name: desprawl-layout
description: Arrange a desprawl infrastructure map so it reads well, by placing its components on a grid and checking the routed edges and layout metrics. Use when asked to tidy, organize, lay out or rearrange a desprawl map, or when one looks messy.
---

# Arrange a desprawl map

A layout maps each component id to a grid cell `[col, row]`. The browser and
you edit the same file (`.desprawl/layouts/<config>.json` in the workspace): the user
drags components, you use the tools. desprawl routes the edges itself
(right-angled, through the gaps between cards), and the browser draws exactly
those routes, so what the metrics score is what the user sees.

A saved layout is the user's work. Before replacing one, say so.

## Loop

1. `get_map` on the configuration: the grid, a drawing of cards and routed
   edges (`▶` marks where an edge enters its target), every component's cell
   and id, the edges, and metrics. Without a saved layout it shows the
   automatic one.
2. Start from `seed_layout` unless the user has a layout worth keeping. The
   seed already follows the flow rule below.
3. Move components with `save_layout` (the whole grid: id -> `[col, row]`).
   Cells are coarse, so neighbouring cells don't touch; negative coordinates
   are fine. It returns the new drawing and metrics: compare them. The
   `worst edge` lines name the edges with the most crossings or shared
   tracks, which usually tells you what to move.
4. Polish with `tidy_layout`: it moves components only within their column,
   keeping changes that reduce crossings, so it never breaks the flow.
   Choosing columns is your job.
5. Tell the user to click **reload** in the browser.

From the shell instead, in the workspace: `desprawl map <config>` (it names the layout file),
`desprawl layout seed <config>`, `desprawl layout tidy <config>`, and edit
the JSON file.

## What good looks like, in priority order

1. **Information flows left to right.** An arrow points from what acts to
   what it acts on: a load balancer routes to a service, a service reads a
   secret or writes to a bucket. Entry points (load balancers, APIs, DNS,
   external users) on the left, the services they reach next, then the data,
   secrets and queues those services use. Nearly every arrow points right.
2. **No overlaps, few crossings and shared tracks.** Within a column, order
   components so connected things sit level with each other.
3. **Short edges, compact shape.** Roughly as wide as tall; split a very tall
   column into two adjacent ones. Components with no connections go in a
   block underneath.
4. **Modules are secondary.** Keep a module's components together when it
   costs nothing, but never break the flow for it.

Metrics only score geometry; also read the drawing and make sure it tells the
story a person would tell.
