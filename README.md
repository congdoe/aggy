# aggy

One command for the coding agents you already use: Claude Code, Codex, and Grok Build.

aggy runs the official CLIs, signed in with your own accounts, and lets them work together. From any of them you can hand one bounded task to another (an image, a short video, a web research question, a news check, or a code review) and get a short report back. Each task runs headless in the other vendor's CLI, locked down to the tools that task needs.

> **Status: pre-release, heading for v0.1.0.** Read-only borrows, switching a session between vendors at the turn end (`Ctrl+\`, with an optional handoff brief, under one aggy session over the vendor sessions), edit borrows on Codex, shared permissions, and OpenCode on screen work today. Commands typed inside a session, such as `/switch`, and the `/aggy` helper are planned, not shipped: for now you switch with `Ctrl+\`, or the agent asks with `aggy switch`. See [docs/design.md](docs/design.md), section 18.

## What aggy never does

- It never reads, copies, or stores vendor credentials. Each CLI signs in with its own flow. `aggy setup` records each CLI's version and when it checked; no CLI has a verified way for aggy to ask whether it is signed in, so aggy records that as unknown and tells you how to sign in.
- It never sends model requests or calls a vendor API. Only the official CLIs talk to their vendors.
- It collects no telemetry, and routes vendor traffic through nothing unless you turn on a local proxy tool for a vendor (such as Headroom's: off by default, with a warning and a typed confirmation). aggy's own network use is a local Unix socket in `~/.aggy/run`, and a loopback health check of a proxy you turned on.
- It never sets API-key variables, and sets a `*_BASE_URL` only to point an on-screen CLI at a local proxy you turned on for it. Borrow children run with those removed from their environment, and `aggy doctor` warns when such settings could bill an API instead of your subscription.

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

aggy                   # start a new session here on your default vendor, as claude does
                       # inside it, Ctrl+\ switches to another vendor at the turn end
aggy borrow edit "Rename Foo to Bar in pkg/x"   # Codex edits in its own git worktree
aggy accept <borrow-id>                         # see its diff stat, run the checks, apply it
aggy -c                # reopen the latest session here (aggy -r <id> for another)
aggy claude --model opus --permission-mode plan   # a vendor CLI with its own flags
aggy --model opus "explain this repo"             # the default vendor's flags and first prompt
git diff | aggy -p "review this"                  # print mode: the CLI's own output, aggy's policy, nothing recorded
aggy opencode --model xai/grok-4                  # OpenCode, on the plans and keys you set up
aggy claude mcp list                              # a vendor's own management command, run as typed
aggy skill link        # let each CLI call aggy itself
aggy policy wire       # write aggy's safety policy into Codex (Claude and Grok get it per launch)
aggy memory import --from claude   # copy Claude's memory into .aggy/memory.md, after a diff
aggy log               # what was on screen, switches, borrows, files an accept changed
aggy stats             # borrows per kind and vendor: runs, tokens, time, redo rate
aggy usage             # what Codex says is left; what aggy's borrows used
aggy diag show <id>    # what happened: the run id every error ends with, or a borrow id
aggy diag bundle > aggy-bundle.md   # scrubbed, to hand to an agent or attach to an issue
```

`aggy <command> --help` gives the details. Every refusal or error ends with `[<code> · run <id>]`: `aggy diag codes <code>` says what it means and what to check next, and `aggy diag` filters and queries the diagnostic log (`--since 1h`, `--borrow`, `--json`, `aggy diag sql`); see [docs/logging.md](docs/logging.md). Routing picks a vendor for each kind and says why it skipped the others; `--vendor` picks one yourself.

aggy's policy, memory, and switching apply only to what starts through `aggy`. A session in an IDE panel, the vendor's desktop app, or its web sessions starts without aggy.

## Vendor support

What each borrow kind does on each CLI. "Live" means checked against the real CLI and recorded, with commands and evidence, in [docs/verification.md](docs/verification.md); everything else is built from vendor docs or source and tested against fake CLIs only. Tested versions: Claude Code 2.1.311 to 2.1.312, codex-cli 0.164.0, grok 1.0.70.

These CLIs update constantly, so a newer version in the same major version is not refused. On first use aggy checks it, once per version: the flags the lockdown needs must be in its help, Codex's feature list must show nothing on that aggy has not reviewed, and in one small real run, part of your borrow, a child must report that it could not read a planted file, with nothing in the output that says otherwise (a child that lies leaves Claude no record to catch it in). It prints one line, and the report says the version was checked here, not verified by a release. A failure passes that vendor over with the reason. `[versions] self_test = false` in `~/.aggy/config.toml` turns the run off (a repo's config cannot). Older versions, a new major version, and a version aggy cannot read are refused. `--allow-untested` skips all of this. On-screen sessions run any version; `aggy doctor` warns. A new Codex tool behind no feature flag is a risk these checks do not cover; see [design section 7](docs/design.md).

| Kind | Claude Code | Codex | Grok Build |
| --- | --- | --- | --- |
| research, news | Not offered. | **Unsupported** for now (T6638): even fully locked down, Codex's `apply_patch` tool can confirm guesses of a local file's contents. Live. | Supported, with web search as the only tool. Research is live: a borrow with aggy's flags under `--sandbox strict` came back, and a planted file was never read. News takes the same flags and has not been run live. |
| image | Not offered. | Supported. Live: an image borrow landed at `--out`. | Supported with `image_gen` only, which takes no local path (source), under `--sandbox strict` with `--allow image_gen`. aggy's own Grok arguments made an image when run directly (live); the full `aggy image` borrow path has not been re-run under strict. |
| image `--edit` | Not offered. | Supported (resumes the same thread). Not yet verified live. | **Refused**, before any child starts: a Grok session lives under its working directory, and the borrow's temp dir is gone by then, so it could only fail. Make a new image, or make it on Codex, whose images `--edit` can change. |
| video | Not offered. | Not offered. | **Off by default**, behind the same `grok_file_tools` opt-in and with the same upload risk (`image_to_video`). Unverified headless. |
| review | Supported when the CLI has `--setting-sources` (probed), read-only in plan mode. Its tool names are unverified. | Supported, with the Codex lockdown flags, which leave no tool that reads a file, so the diff and files go in the prompt. That lockdown is not leak-proof: `apply_patch`, which no flag removes, can confirm a guessed line of a file at a known path (Live; the same reason research is off on Codex). | Supported, read-only in plan mode, under `--sandbox strict`: plan mode alone does not confine its reads, which Grok allows in every mode (live). The tool list was checked live as the model reported it. In a repo under your home folder its read tools are denied, so it works from the diff and files aggy gives it, and the report says so. Refused in a repo holding config Grok may load and run (such as project hooks), because aggy does not rely on Grok's folder trust. |
| edit | **Refused**: its write mode and sandbox under aggy are unverified. | Supported, in its own git worktree with the Codex edit lockdown (flags checked live); `aggy check` runs your repo's checks and `aggy accept` applies the diff after you see it. The end-to-end borrow is not yet verified live. | **Refused**: its write mode and sandbox under aggy are unverified. |

## Shared memory and safety policy

Every session aggy starts gets the repo's `.aggy/memory.md` (at most 4 KB, framed as notes, not instructions) at session start: Claude through a private file, Codex and Grok on their command line. `aggy memory import --from claude` fills it from Claude's own memory after showing the diff, and `aggy unwire` takes it back.

Every session also gets aggy's safety policy: reading `.env` files (on screen, the usual names, not templates such as `.env.example`; an unusual name such as `.env.foo` is not covered there) or private keys, a push to main, `rm -rf`, `git reset --hard`, `git clean`, and force push. On screen the CLI asks you first, as it would without aggy: Claude gets ask rules on its command line, Codex a rules file that `aggy policy wire` writes once (it asks there, and refuses under `codex exec`), and OpenCode asks before the shell commands. Grok's command line has no way to say ask, so there they stay refused, and so do OpenCode's key file reads and force pushes to a protected branch. A borrow child is always refused, and a Codex borrow child is not reopened on screen. No allow changes these rules: on Claude Code an ask rule beats every allow, so a command they cover asks every time, whatever you allowed in Claude or with `aggy permissions add` (which says so); exact pre-approval of one command is planned (T6976). The strict ones (a force push to main, master, trunk, or your default branch, and private keys) are refused on OpenCode. Unusual secret-looking names such as `.env.foo`, `.envrc`, `.npmrc`, and `.netrc` are not covered on screen. A bypass mode in your own Claude settings may skip the asks. `aggy permissions policy rm-rf deny` brings back the old refusal for a group. The policy never allows anything, and beyond it aggy adds only the allows you list yourself. If a CLI cannot get it (its help lacks the flag, or Codex's rules file is not wired), aggy refuses to start that CLI. To start it anyway, pass `--no-policy` (`aggy --no-policy`, `aggy run --no-policy codex`, `aggy new --no-policy`, `aggy resume --no-policy <id>`), or set this in `~/.aggy/config.toml` (a repo's config cannot):

```toml
[policy]
required = false   # start with a warning instead of refusing
```

`aggy doctor` shows, per CLI, whether it gets the memory and the policy.

## One set of permissions

Rules use Claude Code's format, so an approval given once holds on Claude Code, Grok, and OpenCode. aggy reads them from `~/.claude/settings.json`, your repo's untracked `.claude/settings.local.json`, and its own `~/.aggy/permissions.json`, and it picks up what you approved in Grok and Codex at each launch and switch. It adds only the allows you list yourself: a repo's committed rules never become allows. Each allow aggy passes on goes through a filter modelled on the one Claude applies entering auto mode (no blanket shell access, no wildcarded interpreters, no wildcarded run commands). In a repo you have not trusted, only known read-only forms apply, such as `git status --short` and `ls`, and anything else, a repo's `settings.local.json` included, is held and named. The filter covers what aggy passes on: a rule in a CLI's own file is applied by that CLI, so `Bash(*)` in `~/.claude/settings.json` still holds on Claude, and `aggy permissions` says so beside it. Deny always wins, and nothing is written to a vendor's config for a session. A mistake in `~/.aggy/permissions.json` refuses the launch, by line, as a deny there must not be lost.

```sh
aggy permissions show                              # the rules in force, where each came from, what was skipped and why
aggy permissions add allow 'Bash(gh pr list)'      # for this repo; --everywhere for all of them (needs aggy trust)
aggy permissions suggest                           # propose commands you ran in Claude Code; you confirm each
aggy trust .                                       # let allows beyond the read-only ones apply here
aggy config                                        # default harness and model, permission mode, harness on or off
```

Changes are for you at a terminal, apply at the next launch or switch, and `aggy unwire` undoes them. "At a terminal" holds against mistakes, not a model that tries: a model's shell can give itself a terminal with `script` and pipe `y`. Codex gets none of these rules yet; it keeps asking in its own way. OpenCode takes exact commands only, so a rule with a wildcard is named and skipped.

## Known limits

- The shared memory (`.aggy/memory.md`) reaches Codex and Grok on their command line, which other local users can read with `ps`. So does the payload Codex passes to a legacy `notify` hook you set yourself (aggy installs none). Keep secrets out of the memory; see [docs/design.md](docs/design.md), section 13.
- The safety policy is a guardrail against an agent's mistakes, not a sandbox.
- OpenCode (`aggy opencode`) gets guardrails against mistakes, not protection from a hostile repo or a tricked model: a model's shell runs as you with no OS sandbox, so it can reach OpenCode's logins and aggy's config. A repo you have not trusted with `aggy trust --opencode <dir>` opens in `opencode mini`; a trusted one opens the full screen, and its screen plugins run before any guard. See [docs/design.md](docs/design.md), section 15.

## Not affiliated

aggy is an independent open-source project. It is not affiliated with, endorsed by, or sponsored by Anthropic, OpenAI, xAI, or the makers of OpenCode. Claude Code, Codex, Grok Build, and OpenCode are their respective owners' products and trademarks, named here only to say what aggy works with.

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
