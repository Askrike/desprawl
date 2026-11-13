# Security policy

desprawl reads Terraform and, when reverse-engineering, live cloud accounts, so we take
security reports seriously.

## Reporting a vulnerability

Please **don't open a public issue**. Report it privately through GitHub:
[Report a vulnerability](https://github.com/askrike/desprawl/security/advisories/new).

Include what you found, how to reproduce it and which version (`desprawl version`) you used.
We aim to acknowledge reports within three working days and will keep you updated until a
fix is released. We're happy to credit you in the advisory.

## Supported versions

Fixes go into the latest release. desprawl releases often, so please upgrade before
reporting.

## Scope

Especially relevant:

- Secrets or sensitive values from a cloud account written to disk or output (desprawl
  scrubs values the provider marks sensitive).
- Account data leaking into provider packs.
- Anything that makes desprawl change cloud resources: it is meant to be read-only.
- The local map server (`desprawl` with no command) or MCP server being reachable or
  controllable by other sites or users.
