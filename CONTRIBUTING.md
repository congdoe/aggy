# Contributing

Thanks for helping. The design is in [docs/design.md](docs/design.md), the adapter contract in [docs/adapters.md](docs/adapters.md), and what has been checked against the real vendor CLIs in [docs/verification.md](docs/verification.md).

## Ground rules

- aggy never reads vendor credentials, proxies vendor traffic, or sends model requests. Changes that would are out of scope.
- Every change aggy makes to another tool's config goes through the manifest, so `aggy unwire` can undo it.
- Tests never need a vendor login. Use the fake CLIs in `testdata/fakes`.
- Scrub recorded fixtures of home paths, emails, and tokens before committing them.

## Add a vendor adapter

A new vendor CLI gets its own package in `internal/adapter/`. [docs/adding-a-vendor.md](docs/adding-a-vendor.md) walks through it: the interface, the lockdown flags, the verification.md entry every vendor fact needs, the fakes and tests, and the security rules a PR must keep. Adapter changes for a CLI update follow the same rules.

## Report a bug or a vendor change

Use the issue templates. A bug report asks for `aggy diag bundle` ([docs/logging.md](docs/logging.md)), and a vendor change for `aggy doctor --report`; both are scrubbed for a public issue. Never paste credentials, tokens, or anything from `~/.claude`, `~/.codex`, or `~/.grok`.

## Propose a tool for the default set

aggy's managed tools (CodeGraph, RTK, Headroom, Context7, Caveman, Playwright) are TOML definitions in `internal/tooldef/builtin/`. The format and its trust rules are in [docs/design.md](docs/design.md), section 9, "Tool definitions". To propose one:

1. Write the definition and try it as your own: `aggy tool add demo.toml` validates it and shows what it would run and wire per vendor before copying it to `~/.aggy/tools/`.
2. Open a PR that adds it as `internal/tooldef/builtin/<name>.toml`. It must pass the same validator (`go test ./internal/tooldef` fails otherwise), pin a `version`, keep `default = false`, and say in the PR what the tool does, its license, and what it sends off the machine, if anything.
3. Cite every vendor fact (a command, flag, hook event, or config key) from the tool's or the vendor's docs at that version, or a live check recorded in `docs/verification.md`. Anything else goes in `unverified`.
4. A tool that needs a proxy, a base URL, an API key in the environment, or a shell string is out of scope.

## Sign-off

Commits need a Developer Certificate of Origin sign-off (`git commit -s`). There is no CLA.

## Development

```sh
make test    # go vet and go test -race
make build   # builds ./bin/aggy
```
