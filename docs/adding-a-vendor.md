# Adding a vendor adapter

An adapter is how aggy talks to one vendor CLI. Every flag, file path,
output format, and error string aggy depends on for that CLI lives in its
adapter, so a vendor change is fixed in one package. This guide follows
the three adapters that exist, `internal/adapter/claude`,
`internal/adapter/codex`, and `internal/adapter/grok`. Read
[adapters.md](adapters.md) (the contract and every recorded fact) and
[verification.md](verification.md) (how each fact was checked) first.

## The hard rules

These are not style. A PR that breaks one is closed, however useful the
rest of it is.

- **No credentials.** An adapter never reads, copies, or stores a vendor
  credential file, a token, or an API key. Every file access goes through
  `internal/safefs`, and a new vendor's credential files are added to its
  deny list (`internal/safefs/safefs.go`) in the same PR, by path and under
  the vendor's relocation variable, so safefs refuses them by path and by
  file identity. Lint bans the `os` and `syscall` file calls outside
  safefs.
- **No model requests.** aggy sends nothing to any vendor: no API calls,
  no proxy, no `*_BASE_URL`, no API-key variable in a command's `Env`.
  Only the vendor CLI talks to its vendor, on the user's own sign-in.
  Lint bans `net` outside the supervisor's local socket.
- **No `--dangerously-*`.** No flag that skips permissions, trust, or a
  sandbox (`--dangerously-*`, yolo modes, `--permission-mode
  bypassPermissions` and the like), in an adapter, a test, a fixture, or a
  live check.
- **execrun only.** Adapters build commands and parse output; they never
  start a process. `Interactive` and `Headless` return an
  `adapter.Command`, and `Probe` runs commands only through the
  `adapter.Runner` its caller passes. The processes themselves start in
  `internal/execrun`, which lint keeps out of adapters; lint bans
  `os/exec` elsewhere.
- **Vendor facts need a source.** See [below](#every-vendor-fact-needs-a-verificationmd-entry).

## The interface

`adapter.Adapter` is in `internal/adapter/adapter.go`; the method table is
in [adapters.md, "Contract"](adapters.md#contract). A new adapter is a
package `internal/adapter/<vendor>` with:

- `doc.go`: the package doc.
- `<vendor>.go`: `const Bin` (the binary name), `type Adapter struct`
  (the zero value ready to use; `Path` and `Caps` as in the others),
  `func New() Adapter`, compile-time checks
  (`var _ adapter.Adapter = Adapter{}`, plus `adapter.ChildFiler` if
  headless runs read per-run files), and `var Tested =
  adapter.VersionRange{...}`, returned by `TestedVersions()`.
- `testdata/`: recorded, scrubbed output that `ParseHeadless` is tested
  on (Claude's `success.json`, `limit.json`, `max_turns.json`, and a
  `garbage.json` it must not guess at).
- Tests: `<vendor>_test.go` for argv and parsing, `probe_test.go`,
  `review_test.go`, and `lockdown_test.go` where the CLI has lockdown
  flags.

Then the vendor is wired in where the other three are named. Search for
`adapter.Grok` to find them; today they include the `Vendor` constant
and `Vendors()` in `internal/adapter/adapter.go`, the probe spec
(`probe.Specs` in `internal/probe/probe.go`), the transcript reader
(`internal/transcript`), the API-key and route checks
(`internal/checkup/routes.go`), the PreToolUse guard's list of vendor
headless commands (`internal/hook/pretooluse.go`), the routing order
(`internal/router`), and the adapter choice in `internal/cli`
(`run.go`, `borrow.go`, `setup.go`).

The rules every method follows are in
[adapters.md, "Contract"](adapters.md#contract): prompts go in files,
never argv; session ids pass `adapter.ValidSessionID` and models
`adapter.ValidModel`; an empty model means no model flag; tools are off by
default; an unrecognized format returns `ErrNoTranscript` or
`ErrUnknown` rather than a guess; and every vendor error maps onto an
`adapter.ErrorClass`, with every plan-limit signal mapped to
`rate_limit`.

## Lockdown flags

Every headless child except review runs in an empty temp directory
(`HeadlessRequest.IsolatedCwd`; review runs read-only at the work-tree
root), with its vendor's lockdown flags. They take away whatever the
borrow's kind does not need: file and shell tools, MCP servers, user
config, subagents, and web search except for research and news. Look at
how each adapter does it:

- Claude: `--strict-mcp-config` with an empty `--mcp-config` written to
  `RunDir` through `ChildFiles`, `--permission-mode plan` for review,
  and a `--tools` allowlist.
- Codex: `codex.Lockdown` (`--strict-config`, `--ignore-user-config`,
  `--sandbox read-only`, and the `--disable` features), and
  `codex.EditLockdown` for edit.
- Grok: a per-kind `--tools` allowlist, `grok.Lockdown` minus the kind's
  tools as `--disallowed-tools`, `--no-subagents`, and
  `--sandbox strict`.

**Fail closed.** If the CLI rejects a lockdown flag, the run is
discarded, not retried without it. `ParseHeadless` calls
`adapter.CheckLockdown(vendor, out)` first, which matches only the CLI's
own rejection formats (`internal/adapter/lockdown_check_test.go` lists
them); add the new CLI's format there. If the CLI ignores a whole
allowlist on one bad name, as Grok does, detect that warning and fail
too. A kind the CLI cannot lock down is refused (see `codex.Refusal`
for research and news), not run open.

Each lockdown flag needs a live check that the child really lost the
tool, recorded in verification.md, before its kind is enabled.

## Every vendor fact needs a verification.md entry

A vendor fact is any flag, subcommand, config key, tool name, hook
payload, output field, error string, or file path of the vendor CLI.
Each one aggy relies on cites:

- the vendor's source or docs at the tested version (a permalink at a
  commit or tag), or
- a check recorded in [verification.md](verification.md): a `--help` or
  `--version` output, or a minimal live run, with the version, the date,
  the exact argv, and the scrubbed result.

Put the entry in verification.md under its own section, and the fact in
[adapters.md, "Verified vendor facts"](adapters.md#verified-vendor-facts),
citing it. A fact you could not check is marked unverified, in a code
comment where it is used and in adapters.md's "Unconfirmed facts and
their fallbacks" table, with the fallback the code uses until it is
settled. Do not invent tool names or flags: guessed Grok tool names were
wrong before, and a wrong name in an allowlist can unlock the child.

Live checks are run by a maintainer, never by CI or an agent, on the
maintainer's own sign-in: help and version output, or minimal one-turn
runs in a fresh scratch directory, with stdin closed, no permission- or
trust-skipping flag, and no edit to the real vendor config. Scrub session
ids, paths, and tokens before recording the result.

`Tested` is the version range those facts were checked on. Outside it,
`aggy doctor` warns and `aggy borrow` refuses unless `--allow-untested`
is passed (`adapter.VersionWarning`).

## Fakes and tests

CI never signs in to a vendor, and tests never run a real vendor CLI.

- **The fake CLI.** `testdata/fakes/fake` is one program that stands in
  for every vendor, named by its binary name; `fakecli.Build(t)` from
  `internal/fakecli` builds it into a temp dir. Add the new vendor there:
  its binary name, its honored flags and values (only those recorded in
  adapters.md, so an adapter that invents a flag fails its test), its
  rejection message format, its `--version` output, and its session-id
  rules. Document them in
  [testdata/fakes/README.md](../testdata/fakes/README.md).
- **Fixtures.** Record real output with `aggy dev capture` and clean it
  with `aggy dev scrub` (see
  [testdata/fixtures/README.md](../testdata/fixtures/README.md)); the
  fixture check in `make test` fails on any home path, email, or token
  left in one.
- **Unit tests** check the argv each kind builds (the lockdown flags
  included), that every refusal fails closed, and `ParseHeadless` on
  each fixture. They are table-driven.
- **End to end.** `test/e2e` drives the supervisor in a pseudo-terminal
  against the fakes ([test/e2e/README.md](../test/e2e/README.md)); add
  the new vendor's interactive and switch cases. `test/smoke` holds the
  opt-in real-vendor runs, which spend quota and never run in CI.
- **Home safety.** Every test that runs the CLI or opens the store sets
  `HOME`, `AGGY_HOME`, and `PATH` to temp dirs first, and sets or clears
  the vendor home variables. The fakes refuse to run against the real
  home.

Before you push: `gofmt -l .` prints nothing, and `make test`,
`make lint`, `make nocgo`, and `make noasm` pass.
