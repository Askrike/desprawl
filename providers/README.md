# Provider packs

Everything provider-specific lives in `providers/<name>/` as data. `<name>`
is the provider's local name (`aws`, `google`, `azurerm`, `cloudflare`).
A pack holds how to draw the provider's resources (`catalog.json`) and how
to reverse-engineer them (the rest); any part can be missing.
These shipped packs are embedded into the binary at build time: rebuild
after changing them. A workspace's fixes go into its
`.desprawl/pack/<name>/`, where a file replaces the shipped file of the same
name (the first fix copies the shipped one there), except `catalog.json`,
whose entries add to or replace the shipped ones type by type. To contribute fixes,
copy those files into `providers/<name>/`, review the diff and rebuild.
desprawl never proposes pack content with a model: the stages print what a pack is missing, with the entry to add, and
whoever runs them (you) decides; running the stage again checks it.

| file | made by | holds | checked by |
|---|---|---|---|
| `rules.docs.json` | `mine-docs` | reference rules from doc examples | (mined, not guessed) |
| `vocabulary.docs.txt` | `mine-docs` | every word in the provider docs: what counts as public | (mined) |
| `imports.docs.json` | `mine-docs` | documented import IDs/identities per type | (mined) |
| `list_args.json` | `discover`'s suggestions | how to list types that need arguments | the schema, then the next discover lists them or prints the failure |
| `pack.json` | `discover`'s and `link`'s suggestions (params, excluded types and attributes); by hand (skips, `query_provider`) | scope parameter data sources and the rest | discover reads each param's data source and compares it with the discovered value |
| `inventory.json` | `discover`'s suggestions | data sources that enumerate resources; kind -> type and import ID | the schema; discover imports a sample of each new mapping, reads with each new partition, and compares each new per-parent source with a single read |
| `rules.llm.json` | `link`'s suggestions | rules judged from real infrastructure, and rejected ones | your judgment: a wrong rule still plans |
| `catalog.json` | by hand | what each resource type is on the map (see [Catalogs](#catalogs)) | the map: `desprawl unclassified` lists the types it doesn't cover |

## Catalogs

A catalog says what each resource type is on the map. Types it doesn't
list are wiring: hidden, with references passing through them. Only things
that run code, store data or take traffic get a box; labels are the
provider's own ("ECS service", "S3 bucket").

```json
{
  "provider": "cloudamqp",
  "resources": {
    "cloudamqp_instance": {"role": "primary", "category": "messaging", "label": "RabbitMQ instance"},
    "cloudamqp_alarm": {"role": "child", "parent_attr": "instance_id"}
  },
  "data_sources": {}
}
```

| role | on the map | e.g. |
|---|---|---|
| `primary` | a component of its own | a function, database, bucket, queue, load balancer |
| `child` | folded into the one component it configures (`parent_attr` names the reference to it) | a bucket's ACL, an API's stage |
| `connector` | an edge between the components in `from_attr` and `to_attr`, labelled `verb` | a function permission |
| `satellite` | plumbing folded into the component that uses it; shared, it's see-through, so each user gets its effects | an IAM role |
| `helper` | hidden, and see-through | `random_pet`, `archive_file` |
| `ignore` | hidden background that absorbs references, with its children | a VPC, a network |

`category` (`compute`, `database`, `storage`, `messaging`, `network`,
`integration`, `identity`, `security`, `observability`, `other`) picks the
icon and colour; `compute` also decides which component shared wiring
joins. The other fields (`attaches`, `inverse`, `inverse_attrs`) are in the
comment on `Rule` in `internal/tfmap/catalog.go`, with `aws/catalog.json` as
the fullest example.

To draw a provider desprawl doesn't know, write
`.desprawl/pack/<name>/catalog.json` in your workspace: every map
(`desprawl`, `desprawl map`, the MCP tools) uses it at once, no rebuild
needed. `desprawl unclassified` lists the types left. To share it, copy it
to `providers/<name>/catalog.json`.

## Building a pack for a new provider

```sh
bin/desprawl init -C ws/<x> -provider <ns>/<name> -config '…'
bin/desprawl -C ws/<x> mine-docs -provider <ns>/<name>   # into ws/<x>/.desprawl/pack/<name>/
bin/desprawl -C ws/<x> run
```

When the pack is good, copy `ws/<x>/.desprawl/pack/<name>/` to
`providers/<name>/` and rebuild.

Then work through what `run` prints: list args and inventory first (they
change what's discovered), then rules and scope params. The inventory is
how discovery finds types the provider can't list: a provider's "list
everything" data source (a resource search, an asset or tag index) and
listings of access grants. Resources it finds become parents, so their
children get listed too. Inventory sources are the hardest entries to
write: read the data source's schema (`.desprawl/state/schema.json`) for the list
attribute and the fields holding each entry's ID and kind.

## Privacy

Packs are shared, but parts are judged from a real account. A word from the
account's values (names, IDs, tags, account numbers) that the provider's
docs and schema never use is a leak; so is a rule whose path goes into a map
key the provider doesn't document (an environment variable you named).
`bin/desprawl -C ws/<x> check-pack` reports them for that workspace's pack:
run it in every workspace a pack was fixed from, before copying the fixes
into `providers/<name>/`.

## Checking a pack

Run the full workflow on a real account and look at what it prints and at
the desprawl diagram. Unlinked components with obvious connections point to
missing rules; `link` prints the candidates it found. If a suggestion is
wrong in a way the next run can't catch (a parent that lists the wrong
resources, a kind mapped to the wrong one of two valid types), improve
`internal/suggest`, not only the pack: the next provider will have the same
problem.
