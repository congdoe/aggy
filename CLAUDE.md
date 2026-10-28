# CLAUDE.md

Guidance for AI coding agents and humans working in this repo.

## What aggy is

aggy is a Go CLI that runs the official Claude Code, Codex, and Grok Build CLIs on the user's own subscriptions. It keeps a parent session with real vendor child sessions, switches vendors at turn end with a brief built without a model, borrows bounded tasks (image, video, research, news, review, edit) to the vendor best suited to each, and keeps instructions, memory, and safety rules in parity across the CLIs. The design is in [docs/design.md](docs/design.md); the adapter contract is in [docs/adapters.md](docs/adapters.md).

## Layout

- `cmd/aggy`: the entry point; it only calls `internal/cli`.
- `internal/cli`: argument parsing and command dispatch.
- `internal/adapter`: the vendor `Adapter` interface. `claude/`, `codex/`, `grok/` are stubs today, not implementations.
- `internal/supervisor`, `hook`, `brief`, `borrow`, `router`, `parity`: stubs today, holding only their package docs. In order: the pseudo-terminal pass-through and switches; `aggy hook <event>`, which forwards vendor hook JSON over a local socket; the handoff brief; headless borrows (isolation, budgets, artifacts, inbox); vendor choice per borrow kind; and rendering shared config into each CLI.
- `internal/probe`: probe specs, version and help parsing, the probe cache, and `LookPath`. It starts nothing.
- `internal/execrun`: the only runner that starts vendor CLI processes (no stdin, a scrubbed `AGGY_*`-only env addition, a time limit). Only `internal/cli`, `internal/devfix`, and tests may import it.
- `internal/patch`: guarded, reversible config changes recorded for `aggy unwire`.
- `internal/safefs`: the only file I/O layer; refuses vendor credential files by path and file identity.
- `internal/store`: the SQLite index at `~/.aggy/aggy.db` and its migrations; the only package that may call `sql.Open`.
- `internal/config`: user and repo config with generic defaults.
- `internal/home`: locates the state directory (`$AGGY_HOME` or `~/.aggy`).
- `docs/`: the design and the adapter guide.
- `testdata/fakes`: fake vendor CLIs driven by scenario files. `testdata/fixtures`: scrubbed real outputs.
- `test/e2e`: the supervisor in a pseudo-terminal against the fakes. `test/smoke`: opt-in real-vendor runs.

## Hard rules

- **Never read, copy, or store vendor credentials.** All file I/O goes through `internal/safefs`. Lint bans the main file-system entry points (`os`, `io/ioutil`, `io/fs`, `filepath` walks, `syscall` opens) outside `internal/safefs` and test files, and `sql.Open` outside `internal/store`. A `nolint` comment must name the linter and give a reason. Processes start only in `internal/execrun` (and the planned supervisor and borrow runner): lint bans `os/exec` elsewhere and keeps `internal/execrun` out of adapters, which run commands only through the `Runner` their caller passes. Other packages that open files are covered by review, not lint. Tests set `HOME` to a temp dir.
- **No telemetry, no model requests, no vendor API calls, no proxy.** Only the vendor CLIs talk to their vendors. Never set `*_BASE_URL` or API-key variables.
- **Every config change goes through `internal/patch`** and must be undoable with `aggy unwire`.

## Commands

```sh
make build          # builds ./bin/aggy
make test           # go vet and go test -race
make lint-install   # run once to get the golangci-lint version CI uses
make lint           # golangci-lint
```

## Testing

- CI never signs in to a vendor. Test adapters against the fake CLIs and the recorded fixtures.
- Scrub fixtures of home paths, emails, and tokens before committing them.
- Real-vendor smoke tests are opt-in and spend quota. Never run them in CI.

## Conventions

- Standard library first. A new dependency needs a written justification in the PR.
- Every package has a package doc. Tests are table-driven.
- Before pushing: `gofmt -l .` prints nothing, `make test` passes, and `make lint` passes.
- Sign off every commit: `git commit -s`.
- Commit subjects: maintainer task commits start with the task code, e.g. `T6589: add safefs wrapper`: one task commit, plus any fix commits from review, which carry the same task code. Everything else uses a short imperative subject.

## Contributing without agents

Open a PR. CI runs. A maintainer reviews it and, once it is approved, merges it. That is the whole path for outside contributors.

## Maintainers' agent workflow

Maintainers build their own tracked tasks with this loop. The orchestrator is the maintainer's main agent session: it batches tasks, opens PRs, and runs the loop.

1. **Batch.** Group ready tasks from the maintainer's task tracker (all blockers merged) into as few PRs as dependencies allow. Run independent batches in parallel when they touch different files. One branch per batch; mark its tasks in progress with the branch.
2. **Build.** One builder agent (Opus, medium effort) works in an isolated git worktree, makes one commit per task, and pushes the branch. The orchestrator opens the PR and moves the tasks to review with a PR comment.
3. **Adversarial review.** A separate reviewer agent (Opus, high effort) gets the code, the invariants, and places to attack, but not the builder's conclusions. Every finding has a severity, `file:line`, a concrete failure scenario, and a verdict: CONFIRMED with proof, or PLAUSIBLE with what would confirm it.
4. **Triage.** Each finding is fixed in this PR or deferred to a named later task, with a `TODO(#<github-issue>)` or `TODO(<task code>)` in the code.
5. **Fix.** Fixes go back to the same builder agent, so it keeps its context. Every confirmed finding gets a regression test that fails on the old code.
6. **Re-review.** The reviewer checks the full diff since the last reviewed commit.
7. **Merge.** Rebase-merge, so task commits and their fix commits survive on `main` as written, then mark the tasks done.

**Merge rule.** Merging after green CI and a re-review with no blocking findings, without further approval, applies only to branches a maintainer created for their own tracked tasks. A PR from a fork or an outside author needs a maintainer's explicit approval before merge, whatever the agents say.
