# Provider packs

Everything provider-specific lives in `providers/<name>/` as data. `<name>`
is the provider's local name (`aws`, `google`, `azurerm`, `cloudflare`).
These shipped packs are embedded into the binary at build time: rebuild
after changing them. A workspace's fixes go into its
`.desprawl/pack/<name>/`, where a file replaces the shipped file of the same
name (the first fix copies the shipped one there). To contribute fixes,
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
