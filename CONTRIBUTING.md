# Contributing to desprawl

Thanks for helping. Bug reports, new provider catalog entries, pack fixes and code are all
welcome.

## Before you start

- For anything bigger than a small fix, open an issue first so we can agree on the approach.
- Read [DEVELOPMENT.md](DEVELOPMENT.md): how the code is laid out and how a configuration
  becomes a map. [README.md](README.md) describes what users do.
- desprawl tries to stay simple: prefer a catalog or pack change over new code, and new code
  over new concepts.

## Build and test

You need the Go version in `go.mod`. Reverse-engineering tests also use Terraform.

```sh
go build -o bin/desprawl ./cmd/desprawl
go test ./...
```

- Golden files live in `testdata/` directories. If your change alters one, check that the
  diff makes sense, then accept it with `go test ./internal/<pkg> -update`.
- After changing the map page (`cmd/desprawl/*.html`), run `scripts/check-ui.sh`, which
  renders every example in headless Chrome.
- CI runs `go test -race`, [golangci-lint](https://golangci-lint.run) (`.golangci.yml`) and
  `govulncheck` on every pull request. Running `golangci-lint run` locally first saves a
  round trip.

## Where changes go

- **A resource type is drawn wrong or "not in catalog"**: fix it in `catalog/<provider>.json`.
- **Reverse-engineering misses or mislinks something**: fix it in the provider pack,
  `providers/<provider>/`. Code in `internal/` must stay provider-agnostic: no provider,
  resource type or cloud API names outside tests.
- **Packs hold no account data.** If your fix came from a real account, run
  `desprawl -C <workspace> check-pack` before copying it into `providers/`.

## Pull requests

- Keep each pull request to one change, with tests where they make sense.
- Describe what changed and why; for map changes, a before/after screenshot helps.
- Commit messages: a short imperative summary line, then the why if it isn't obvious.

## License

desprawl is licensed under the [Apache License 2.0](LICENSE). By submitting a contribution
you agree that it is licensed under the same terms (Section 5 of the license), and that you
have the right to submit it. No separate agreement to sign.
