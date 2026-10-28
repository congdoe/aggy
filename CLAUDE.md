# CLAUDE.md

Guidance for AI coding agents and humans working in this repo.

## What aggy is

aggy is a Go CLI that runs the official Claude Code, Codex, and Grok Build CLIs on the user's own subscriptions. It keeps a parent session with real vendor child sessions, switches vendors at turn end with a brief built without a model, borrows bounded tasks (image, video, research, news, review, edit) to the vendor best suited to each, and keeps instructions, memory, and safety rules in parity across the CLIs. The design is in [docs/design.md](docs/design.md); the adapter contract is in [docs/adapters.md](docs/adapters.md); what has been checked against the real CLIs, and how, is in [docs/verification.md](docs/verification.md).

## Layout

- `cmd/aggy`: the entry point; it only calls `internal/cli`.
- `internal/cli`: argument parsing and command dispatch.
- `internal/adapter`: the vendor `Adapter` interface, and `claude/`, `codex/`, `grok/`, which build each CLI's commands and parse its output. Adapters never start processes.
- `internal/router`: the routing table, which picks the vendors a borrow kind may use and says why each other vendor was skipped.
- `internal/supervisor`: the pseudo-terminal pass-through behind `aggy run <vendor>` (switches come later).
- `internal/parity`: shared config for each CLI: the Codex `CLAUDE.md` fallback, the git exclude line, the shared memory (`.aggy/memory.md`), and the safety policy (deny rules on the Claude and Grok command line, a Codex rules file), and the public MCP servers copied from Claude into Codex and Grok (`aggy mcp wire`).
- `internal/brief`: the task file (`.aggy/task.md`) and the handoff brief, built without a model from the task file, the transcript tail, and git state that `internal/cli` gathers (`aggy brief --show`), written to `.aggy/briefs/<child-id>.md` for `aggy resume --brief`; a return to a child gets only the turns other CLIs added since it was last closed.
- `internal/redact`: secret-shaped text (keys, tokens, private keys, secret settings): the fixture scrubber replaces it, and a brief drops every line that holds it.
- `internal/transcript`: reads the tail of a Claude, Codex, or Grok session file for a brief.
- `internal/borrow`: headless borrows (routing with fall-through, isolation, budgets and leases, reports, the per-borrow log). `internal/hook`: the PreToolUse guard that stops a delegated child from starting another vendor CLI.
- `internal/limits`: plan-limit signals (StopFailure hints, the Codex rollout tail, borrow failures), recorded as `limit` events and read back for the router's skip rule.
- `internal/notify`: desktop notifications (osascript, notify-send), off unless the user config turns them on; the text goes as argv, never as script or markup.
- `internal/checkup`: the checks `aggy setup` and `aggy doctor` share: API-key variables and proxy routes, by name only.
- `internal/probe`: probe specs, version and help parsing, the probe cache, and `LookPath`. It starts nothing.
- `internal/execrun`: the only runner that starts vendor CLI processes (no stdin, or a borrow's prompt file opened through `safefs.OpenRegular`; a scrubbed env whose only additions are `AGGY_DELEGATED`, `AGGY_VENDOR`, `AGGY_MODEL`, and the fakes' `AGGY_FAKE_*`; a time limit that kills the process group). `NotifyRunner` runs the desktop notifier the same way, plus the session bus and display variables. Separately, `StartPTY` starts the on-screen CLI in a pseudo-terminal (and `RunDirect`, for `aggy --direct`, on aggy's own terminal) with the user's own environment plus `AGGY_*` variables only; it never uses or loosens the borrow allowlist. Only the files of `internal/cli` and `internal/devfix` (not their subpackages) and tests may import it.
- `internal/patch`: guarded, reversible config changes recorded for `aggy unwire`.
- `internal/safefs`: the only file I/O layer; refuses vendor credential files by path and file identity.
- `internal/aggydir`: the one rule for a repo's `.aggy` folder: no symlink anywhere under it, directories made through descriptors, and nothing read from it trusted as aggy's record unless the store confirms it.
- `internal/store`: the SQLite index at `~/.aggy/aggy.db` and its migrations; the only package that may call `sql.Open`.
- `internal/config`: user and repo config with generic defaults.
- `internal/home`: locates the state directory (`$AGGY_HOME` or `~/.aggy`).
- `docs/`: the design, the adapter guide, and the verification record.
- `testdata/fakes`: fake vendor CLIs driven by scenario files. `testdata/fixtures`: scrubbed real outputs.
- `test/e2e`: the supervisor in a pseudo-terminal against the fakes. `test/smoke`: opt-in real-vendor runs.

## Hard rules

- **Never read, copy, or store vendor credentials.** All file I/O goes through `internal/safefs`. Lint bans the main file-system entry points (`os`, `io/ioutil`, `io/fs`, `filepath` walks, `syscall` opens) outside `internal/safefs` and test files, and `sql.Open` outside `internal/store`. A `nolint` comment must name the linter and give a reason. Processes start only in `internal/execrun` (and the planned supervisor): lint bans `os/exec` elsewhere and keeps `internal/execrun` out of adapters, which run commands only through the `Runner` their caller passes. Other packages that open files are covered by review, not lint. Tests set `HOME` to a temp dir.
- **No telemetry, no model requests, no vendor API calls, no proxy.** Only the vendor CLIs talk to their vendors. Never set `*_BASE_URL` or API-key variables. Lint bans `net` except in the supervisor socket's files (`internal/supervisor/socket.go` and `peercred_*.go`), which only listen on a Unix socket in `~/.aggy/run`, and its client (`client_unix.go`), which only dials that socket with `net.DialUnix`, and `golang.org/x/sys/unix` except for the peer-uid calls there.
- **Every config change goes through `internal/patch`** and must be undoable with `aggy unwire`.
- **Vendor facts need a source.** Any claim about a vendor CLI (a flag, config key, tool name, hook payload, or error format) cites the vendor's source or docs at the tested version, or a live check recorded in `docs/verification.md`. Anything else is marked unverified in code and docs. Guessed names have been wrong before: Grok's X-search tools and its toolset media keys did not exist.
- **Borrow children are locked down.** Every headless child except review runs in an empty temp directory (review runs read-only at the git work-tree root) with its vendor's lockdown flags from `docs/adapters.md`. The adapter fails closed when a CLI rejects a flag, or when Grok reports a tool name it does not know (it then ignores the whole allowlist). Research and news children never get file-reading or MCP tools.
- **Anything meant to be pasted publicly** (`aggy doctor --report`, fixtures, `docs/verification.md`) carries paths as structured fields and never includes env values, vendor error text, config ids the user chose, session ids, or tokens.

## Commands

```sh
make build          # builds ./bin/aggy
make test           # go vet and go test -race
make lint-install   # run once to get the golangci-lint version CI uses
make lint           # golangci-lint
make nocgo          # fails if any package has cgo files, per OS
make noasm          # fails on Go assembly or .syso objects
```

## Testing

- CI never signs in to a vendor. Test adapters against the fake CLIs and the recorded fixtures.
- Scrub fixtures of home paths, emails, and tokens before committing them.
- Real-vendor smoke tests are opt-in and spend quota. Never run them in CI.
- Agents never run the real vendor CLIs. A maintainer may run opt-in live checks: help and version output, or minimal one-turn runs. Live checks never edit the real vendor config and never use `--dangerously-*` or other permission- or trust-skipping flags. Record the scrubbed results in `docs/verification.md`.
- Tests must not depend on map iteration order: build rows and inserts from slices. A package whose tests were flaky runs with `-count=20` before a push.
- A dry run has no side effects: no lock files, no directories, no store writes.

## Conventions

- Standard library first. A new dependency needs a written justification in the PR.
- Every package has a package doc. Tests are table-driven.
- Before pushing: `gofmt -l .` prints nothing, and `make test`, `make lint`, `make nocgo`, and `make noasm` pass.
- Sign off every commit: `git commit -s`.
- Commit subjects: maintainer task commits start with the task code, e.g. `T6589: add safefs wrapper`: one task commit, plus any fix commits from review, which carry the same task code. Everything else uses a short imperative subject.
- This repo is public. Commits and PR descriptions carry no agent session links or `Claude-Session` trailers.

## Contributing without agents

Open a PR. CI runs. A maintainer reviews it and, once it is approved, merges it. That is the whole path for outside contributors.

## Maintainers' agent workflow

Maintainers build their own tracked tasks with this loop. The orchestrator is the maintainer's main agent session: it batches tasks, opens PRs, and runs the loop.

1. **Batch.** Group ready tasks from the maintainer's task tracker (all blockers merged) into as few PRs as dependencies allow. Run independent batches in parallel when they touch different files. One branch per batch; mark its tasks in progress with the branch.
2. **Build.** One builder agent (Opus, medium effort) works in an isolated git worktree, makes one commit per task, and pushes the branch. The orchestrator opens the PR and moves the tasks to review with a PR comment.
3. **Adversarial review.** A separate reviewer agent (Opus, high effort) gets the code, the invariants, and places to attack, but not the builder's conclusions. Batches built in parallel share one reviewer per wave, which reports findings per PR and also checks how the PRs fit together. Every finding has a severity, `file:line`, a concrete failure scenario, and a verdict: CONFIRMED with proof, or PLAUSIBLE with what would confirm it.
4. **Triage.** Each finding is fixed in this PR or deferred to a named later task, with a `TODO(#<github-issue>)` or `TODO(<task code>)` in the code.
5. **Fix.** Fixes go back to the same builder agent, so it keeps its context. Every confirmed finding gets a regression test that fails on the old code. Fix commits are fast-forward: never force-push a PR branch. Test a rebase onto `main` on a throwaway local branch; if it conflicts, push the rebased branch under a new name.
6. **Re-review.** The reviewer checks the full diff since the last reviewed commit. Rounds are capped: after the first re-review, only HIGH or BLOCKER findings start another round. MEDIUM and LOW findings go to a named follow-up task, and the orchestrator checks a small final fix diff itself.
7. **Merge.** Rebase-merge, so task commits and their fix commits survive on `main` as written, then mark the tasks done.

**Merge rule.** Merging after green CI and a re-review with no blocking findings, without further approval, applies only to branches a maintainer created for their own tracked tasks. A PR from a fork or an outside author needs a maintainer's explicit approval before merge, whatever the agents say.
