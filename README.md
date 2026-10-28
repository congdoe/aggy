# aggy

One command for the coding agents you already use: Claude Code, Codex, and Grok Build.

aggy runs the official CLIs, signed in with your own accounts, and lets them work together. From any of them you can hand one bounded task to another (an image, a short video, a web research question, a news check, or a code review) and get a short report back. Each task runs headless in the other vendor's CLI, locked down to the tools that task needs.

> **Status: pre-release, heading for v0.1.0.** Read-only borrows work today. Switching a session between vendors, with a handoff brief and a parent session over the vendor sessions, is **in progress** (milestone M2) and not shipped. Its first pieces are merged: `aggy hooks install` (hooks that tell aggy when a session starts, ends a turn, or hits a limit; `aggy unwire` takes them out) and `aggy brief --show` (a preview of the brief, built without a model; writing one comes with switching). Edit borrows come in M3.

## What aggy never does

- It never reads, copies, or stores vendor credentials. Each CLI signs in with its own flow. `aggy setup` records each CLI's version and when it checked; no CLI has a verified way for aggy to ask whether it is signed in, so aggy records that as unknown and tells you how to sign in.
- It never sends model requests or calls a vendor API. Only the official CLIs talk to their vendors.
- It runs no proxy and collects no telemetry. aggy's own network use is a local Unix socket in `~/.aggy/run`.
- It never sets `*_BASE_URL` or API-key variables. Borrow children run with those removed from their environment, and `aggy doctor` warns when such settings could bill an API instead of your subscription.

What aggy does change, it records: every config change goes through one guarded layer and `aggy unwire` undoes it.

What a task contains does leave your machine, through the vendor CLI that runs it, as with any prompt you type into that CLI.

## Install

aggy runs on macOS and Linux and needs at least one of the vendor CLIs installed and signed in.

Homebrew (from v0.1.0):

```sh
brew install congdoe/aggy/aggy
```

The macOS binaries are not notarized yet, so Gatekeeper may block the first run; `go install` builds locally and avoids that.

Go (the version in `go.mod`; no cgo):

```sh
go install github.com/congdoe/aggy/cmd/aggy@latest
```

From source:

```sh
git clone https://github.com/congdoe/aggy && cd aggy && make build   # ./bin/aggy
```

## Quickstart

```sh
aggy setup             # find each CLI and check its version
aggy doctor            # API keys, base URLs, untested versions, config drift

aggy borrow research "What changed in Go 1.27's toolchain?"
aggy borrow news "Latest stable release of SQLite"
aggy borrow review "Check this change for bugs"          # run inside a git work tree
aggy image "Hero image, 1200x630, the text 'aggy'" --out docs/hero.png
aggy inbox             # unread borrow reports

aggy run claude        # start a vendor CLI under aggy (pass-through; switching is M2)
aggy skill link        # let each CLI call aggy itself
aggy policy wire       # write aggy's safety policy into Codex (Claude and Grok get it per launch)
aggy memory import --from claude   # copy Claude's memory into .aggy/memory.md, after a diff
aggy log               # borrows, vendors, models, files changed
aggy stats             # borrows per kind and vendor: runs, tokens, time, redo rate
```

`aggy <command> --help` gives the details. Routing picks a vendor for each kind and says why it skipped the others; `--vendor` picks one yourself.

## Vendor support

What each borrow kind does on each CLI. "Live" means checked against the real CLI and recorded, with commands and evidence, in [docs/verification.md](docs/verification.md); everything else is built from vendor docs or source and tested against fake CLIs only. Tested versions: Claude Code 2.1.311 to 2.1.312, codex-cli 0.164.0, grok 1.0.70. Borrows refuse other versions unless you pass `--allow-untested`.

| Kind | Claude Code | Codex | Grok Build |
| --- | --- | --- | --- |
| research, news | Not offered. | **Unsupported** for now (T6638): even fully locked down, Codex's `apply_patch` tool can confirm guesses of a local file's contents. Live. | Supported, with web search as the only tool. The tool list was checked live as the model reported it; the exact flag set aggy passes has not yet been run live. |
| image | Not offered. | Supported. Lockdown flags checked live; the end-to-end borrow is not yet verified live. | Supported with `image_gen` only, which takes no local path (source). Not yet verified live. |
| image `--edit` | Not offered. | Supported (resumes the same thread). Not yet verified live. | **Off by default.** Needs `[media] grok_file_tools = true` in `~/.aggy/config.toml` (a repo's config cannot set it). Grok's `image_edit` reads a local image at any path the model names and uploads it to xAI, so a brief that names a private image, or a child steered to one, sends it off your machine. Not yet verified live. |
| video | Not offered. | Not offered. | **Off by default**, behind the same `grok_file_tools` opt-in and with the same upload risk (`image_to_video`). Unverified headless. |
| review | Supported when the CLI has `--setting-sources` (probed), read-only in plan mode. Its tool names are unverified. | Supported, with the Codex lockdown flags, which leave no tool that reads a file, so the diff and files go in the prompt. That lockdown is not leak-proof: `apply_patch`, which no flag removes, can confirm a guessed line of a file at a known path (Live; the same reason research is off on Codex). | Supported, read-only in plan mode; the tool list was checked live as the model reported it. Refused in a repo holding config Grok may load and run (such as project hooks), because aggy does not rely on Grok's folder trust. |
| edit | M3, not shipped. | M3, not shipped. | M3, not shipped. |

## Shared memory and safety policy

Every session aggy starts gets the repo's `.aggy/memory.md` (at most 4 KB, framed as notes, not instructions) at session start: Claude through a private file, Codex and Grok on their command line. `aggy memory import --from claude` fills it from Claude's own memory after showing the diff, and `aggy unwire` takes it back.

Every session also gets aggy's safety policy: never read `.env` or key files, never push to main, no `rm -rf`, `git reset --hard`, `git clean`, or force push. Claude and Grok get deny rules on their command line; Codex reads a rules file that `aggy policy wire` writes once. The policy only adds denials and never loosens your own settings. If a CLI cannot get it (its help lacks the flag, or Codex's rules file is not wired), aggy refuses to start that CLI. To start it anyway, pass `--no-policy` (`aggy --no-policy`, `aggy run --no-policy codex`, `aggy new --no-policy`, `aggy resume --no-policy <id>`), or set this in `~/.aggy/config.toml` (a repo's config cannot):

```toml
[policy]
required = false   # start with a warning instead of refusing
```

`aggy doctor` shows, per CLI, whether it gets the memory and the policy.

## Known limits

- The shared memory (`.aggy/memory.md`) reaches Codex and Grok on their command line, which other local users can read with `ps`. So does the payload Codex passes to a legacy `notify` hook you set yourself (aggy installs none). Keep secrets out of the memory; see [docs/design.md](docs/design.md), section 13.
- The safety policy is a guardrail against an agent's mistakes, not a sandbox.

## Not affiliated

aggy is an independent open-source project. It is not affiliated with, endorsed by, or sponsored by Anthropic, OpenAI, or xAI. Claude Code, Codex, and Grok Build are their respective owners' products and trademarks, named here only to say what aggy works with.

The vendor CLIs and subscriptions are yours: aggy runs the CLIs you installed, signed in with your own accounts, and every task it starts uses your plan. You are responsible for following each vendor's terms.

## Development

```sh
make test    # go vet and go test -race
make lint    # golangci-lint (make lint-install once)
make build   # builds ./bin/aggy
```

See [CONTRIBUTING.md](CONTRIBUTING.md), [docs/design.md](docs/design.md), and [docs/adapters.md](docs/adapters.md). Releases: [docs/release.md](docs/release.md). Opt-in live checks: [test/smoke](test/smoke/README.md).

## License

[MIT](LICENSE)
