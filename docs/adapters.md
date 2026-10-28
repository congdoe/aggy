# Vendor adapters

Every flag, file path, hook format, and transcript format aggy depends on goes
through one adapter per vendor. The vendor CLIs update often, so a CLI
change should be fixed in one package. Adapters are also how a new vendor is
added.

The interface is `adapter.Adapter` in `internal/adapter/adapter.go`.
Implementations live in `internal/adapter/<vendor>`.

## Stability

**The interface is unstable until M1.** It will change as the first vendor
adapters are built. These gaps are known:

- Parsing hook payloads: `Stop`, `StopFailure`, and the Codex
  `agent-turn-complete` notify.
- Discovering the session id of an interactive launch. Codex cannot be
  given an id in advance.
- Detecting a plan limit in an interactive session.
- The exit sequence the supervisor types to end a session at turn end.
- The inputs `ConfigPatches` needs: tool toggles, the safety policy, and
  the MCP server list.
- Patches that create symlinks or delete files.

## Contract

**Adapters build commands and parse output. They never start processes.**
`Interactive` and `Headless` return an `adapter.Command` (program, args, env,
dir, stdin). The supervisor runs interactive commands in a pseudo-terminal,
and the borrow runner runs headless ones. `Probe` gets a `Runner` from the
caller and runs commands only through it. Because of this split, adapters
can be tested against recorded output with no vendor installed.

| Method | Does |
| --- | --- |
| `Vendor()` | Names the vendor: `claude`, `codex`, or `grok`. |
| `TestedVersions()` | The CLI version range the adapter was tested on. |
| `Probe(ctx, run)` | Reports the CLI's path, version, sign-in status, and capabilities. It records connection status only and never tokens. |
| `Interactive(req)` | Builds the command for an on-screen session: new session, resume, or fork. It sets a model only when one is given, and points to a brief file when there is one. |
| `Headless(req)` | Builds the command for one borrow. |
| `ParseHeadless(req, out)` | Turns stdout, stderr, and the exit code into a `HeadlessResult`: vendor session id, final text, artifacts, error class, and tokens. |
| `Artifacts(req, res)` | Lists every file the run produced in the vendor's own store, not only the file the reply names. |
| `TranscriptTail(ref, n)` | Returns the latest `n` turns and the transcript path, or `ErrNoTranscript`. |
| `ConfigPatches(cc)` | Returns the parity changes as `[]patch.Patch`, each guarded by the file's current hash. |

Rules every adapter follows:

- **No credentials.** Never read, copy, or store `~/.claude/.credentials.json`,
  `~/.codex/auth.json`, `~/.codex/.credentials.json` (Codex's MCP OAuth
  store), `~/.grok/auth.json`, `~/.grok/mcp_credentials.json`, or their
  relocated forms under `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, and `GROK_HOME`.
  All file access goes through `internal/safefs`, which refuses those files
  by path and by file identity, and lint enforces it.
- **No proxies or API keys.** Never set `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`,
  `GROK_MODELS_BASE_URL`, or any API-key variable.
- **Prompts go in files.** `HeadlessRequest.PromptFile` and
  `InteractiveRequest.BriefFile` are paths. Never put prompt text in argv,
  because macOS `ARG_MAX` is 1MB including the environment.
- **Tools are off by default.** A nil `AllowedTools` means the child gets no
  tools beyond what its kind needs. Research and news run in an empty temp
  directory, with file and shell tools removed where the CLI allows it.
- **Read-only kinds stay read-only.** `image`, `video`, `research`, `news`,
  and `review` never write the repo. `review` runs in plan mode or a
  read-only sandbox.
- **Degrade instead of failing.** When a format is not recognized, return
  `ErrNoTranscript` or `ErrUnknown`. Do not guess.
- **Map errors onto aggy's classes.** Use `ErrorClass`: `rate_limit`, `auth`,
  `billing`, `server`, `invalid_request`, `max_turns`, `timeout`, and
  `unknown`. `rate_limit` triggers the switch picker and rerouting, so map
  every plan-limit signal to it.
- **Pass the model through unchanged.** An empty model means no model flag.

## Verified vendor facts

These were checked on 2026-10-27 with claude 2.1.311, codex-cli 0.164.0, and
grok 1.0.70. Each adapter states the versions it was tested against.

### Codex

- Headless is `codex exec --json`. The first JSON event is
  `thread.started` with `thread_id`, which is the session id.
- Run it with stdin from `/dev/null` (`StdinNull`). `codex exec` waits on a
  stdin that is not a terminal.
- Web search: `-c 'web_search="live"'`. `exec` has no `--search` flag. Each
  query appears as a `web_search` item in the JSON stream.
- Isolation: `--disable shell_tool --disable unified_exec --disable view_image`,
  plus `--sandbox read-only` in an empty temp dir. Whether this fully stops
  file reads still has to be checked live.
- Images land in `~/.codex/generated_images/<thread-id>/*.png`, which is
  `$CODEX_HOME/generated_images/...` when `CODEX_HOME` is set. The image tool
  call does not appear in the JSON stream, and one request can produce more
  than one image, so collect every file in the folder.
- Reference images: `codex exec -i <file>`.
- Resume: `codex resume <id>` and `codex exec resume <id>`. A new session
  cannot be given an id in advance.
- Limit: there is no failure hook. The binary has a `usage_limit_reached`
  error code and the text "You've hit your usage limit".

### Grok

- Headless is `grok -p --output-format json`, and the JSON includes
  `sessionId`.
- Flags: `--tools`, `--disallowed-tools`, `--max-turns`, and
  `--permission-mode plan`.
- An isolated news run worked with `--disallowed-tools
  "read_file,grep,list_dir,write,search_replace,run_terminal_cmd,web_fetch,Agent"`.
  It kept `web_search`, `x_keyword_search`, and `x_semantic_search`.
- Images land in `~/.grok/sessions/<url-encoded cwd>/<sessionId>/images/*.jpg`,
  or the same path under `$GROK_HOME`. The reply gives the path relative to
  the session folder (`images/1.jpg`).
- Sessions: `--session-id <uuid>`, `--resume <id>` (also with `-p`), and
  `--fork-session`. `--continue` takes the latest session in the current
  directory. Use `--no-alt-screen` to keep scrollback.

### Claude

- Headless is `claude -p --output-format json`. Read-only uses
  `--permission-mode plan`.
- Sessions: `--session-id <uuid>`, `--resume <id>`, and `--fork-session`.

### Hooks and limits

- Claude and Grok both have a `StopFailure` hook. Its error classes include
  `rate_limit`, and Grok says its classes mirror Claude Code's. Claude's
  payload also carries `error_details`.
- Turn end: Claude uses `Stop`. Codex uses `Stop` and the
  `agent-turn-complete` notify. Grok uses `Stop` and `StopCancelled`.

## The adapters as built

Each adapter lives in `internal/adapter/<vendor>` and is tested against
its fake with golden argv per kind and parsing tests driven by scenario
files in its `testdata/`.

**Prompts.** A headless prompt is never in argv. Every adapter returns
`Stdin: StdinFile` with `StdinPath` set to the prompt file. For Codex the
prompt argument is `-`: `codex exec` reads a non-terminal stdin to EOF,
and a regular file reaches EOF at once, so it never waits as it would on
an open pipe. (The recorded rule is "never an open stdin"; `/dev/null`
also satisfies it but leaves no way to pass a prompt from a file.)

**Briefs.** An interactive session with a brief gets one short first
message, `adapter.BriefPrompt(path)`, that tells the agent to read the
file. The brief itself never goes in argv.

**Probes.** `Probe` finds the binary on aggy's absolute `PATH` entries
(`probe.LookPath`) and runs `probe.Detect` with the vendor's `Spec`.
`--version` is parsed keyed on the Spec's program names (`Claude Code`,
`claude`; `codex-cli`, `codex`; `grok`), with an optional `v`; with no
named line, one unambiguous version line is taken, and several are an
error. Codex `exec resume` counts as present only when the `exec` help
lists it under `Commands:` or its own help names a session-id argument; a
usage error is a cached absence.

Headless argv per kind (`<p>` is the prompt file on stdin, `<id>` a
session id; `--model <m>` or `-c model="<m>"` is added only when a model
is given):

| Kind | Claude | Codex | Grok |
| --- | --- | --- | --- |
| research | unsupported | `codex exec --json --sandbox read-only --disable shell_tool --disable unified_exec --disable view_image -c web_search="live" -` | `grok -p --output-format json --tools web_search,x_keyword_search,x_semantic_search [--max-turns N]` |
| news | unsupported | same as research | same as research |
| image | unsupported | `codex exec --json --sandbox read-only [-i <ref>]... -` | `grok -p --output-format json --tools image_gen` |
| image `--edit` | unsupported | `codex exec resume <id> --json --sandbox read-only -` | `grok -p --output-format json --resume <id> --tools image_gen,image_edit` |
| video | unsupported | unsupported | `grok -p --output-format json --tools video_gen` |
| review | `claude -p --output-format json --permission-mode plan` | `codex exec --json --sandbox read-only -` | `grok -p --output-format json --permission-mode plan --tools read_file,grep,list_dir` |
| edit | `claude -p --output-format json --permission-mode acceptEdits` | `codex exec --json --sandbox workspace-write -` | `grok -p --output-format json --tools read_file,grep,list_dir,write,search_replace,run_terminal_cmd` |

`WebSearch` adds `-c web_search="live"` on Codex and `web_search` to the
Grok tool list; Claude refuses it. `SessionID` adds `--session-id` on
Claude and Grok; Codex refuses it. `Resume` adds `--resume <id>` on
Claude and Grok, and `exec resume <id>` on Codex. Grok research and news
refuse any file, shell, or fetch tool in `AllowedTools`, and other
read-only kinds refuse write tools.

Interactive argv: Claude `claude [--session-id <uuid> | --resume <id>
[--fork-session [--session-id <uuid>]]] [<brief pointer>]`; Codex `codex
--no-alt-screen [<brief pointer>]` or `codex resume --no-alt-screen <id>
[<brief pointer>]`; Grok `grok --no-alt-screen` with the same session
flags as Claude.

Artifacts are paths only; copying them is the borrow runner's job, through
`internal/safefs`. Codex: every `*.png` in
`$CODEX_HOME/generated_images/<thread-id>/` (else `~/.codex/...`). Grok:
every `*.jpg` in `$GROK_HOME/sessions/<encoded cwd>/<sessionId>/images/`
(else `~/.grok/...`), falling back to the single `sessions/*/<sessionId>/
images/` folder; several are an error. Claude has none.
`TranscriptTail` returns `ErrNoTranscript` for every vendor: no transcript
path or format is recorded. `ConfigPatches` returns nothing yet; the Codex
`CLAUDE.md` fallback is built in `internal/parity`.

### Tested versions

Each adapter declares the CLI versions it was tested on, from the verified
versions above: claude 2.1.311, codex-cli 0.164.0, and grok 1.0.70.
Each declares it as `Tested` and through `TestedVersions()`.
`adapter.VersionWarning(a, version)` returns a warning when a probed
version falls outside that range or cannot be read, and "" inside it, for
`aggy doctor` to show.

### Unconfirmed facts and their fallbacks

These are implemented behind named fallbacks and marked `unconfirmed` in
the code until a live check settles them.

| Fact | Fallback in code |
| --- | --- |
| Codex `SessionStart` hook carries the session id (T6616) | Not used; the session folder watch stays the plan. |
| Headless Codex reports `usage_limit_reached` in `--json`, and interactive Codex writes it to its rollout file (T6617) | `codex.LimitCode` and `codex.LimitText` are matched in any `error` or `turn.failed` event and in stderr. |
| `--disable shell_tool --disable unified_exec --disable view_image` blocks file reads (T6618) | `codex.Isolation`, always with `--sandbox read-only` in an empty dir. |
| How Codex and Grok take extra context at session start (T6619) | A positional first message pointing to the brief file (`adapter.BriefPrompt`). |
| `codex exec -` reads the prompt from stdin | Codex headless passes `-` with the prompt file on stdin. |
| `claude -p` and `grok -p` read the prompt from stdin | Both pass the prompt file on stdin with no prompt argument. |
| Model flags | Claude and Grok `--model <m>`, Codex `-c model="<m>"`. |
| `--permission-mode acceptEdits` (Claude edit) and `--sandbox workspace-write` (Codex edit) | `claude.PermissionEdit`, `codex.SandboxEdit`. |
| `grok --tools` alone removes every other tool | `grok.SearchTools` and the other per-kind allowlists; the verified isolation run used `--disallowed-tools`. |
| `image_edit` and `video_gen` work headless on Grok | `grok.ImageTools` plus `image_edit` on resume, `grok.VideoTools`. |
| Grok's cwd encoding under `sessions/` | `grok.EncodeCwd` (`url.QueryEscape`), then the scan fallback. |
| Claude's JSON reply fields (`result`, `session_id`, `is_error`, `subtype`, `usage`) | Read when present; a reply with none of them is `ErrUnparsed`. |
| Codex events after `thread.started` (`item.completed` agent messages, `turn.completed` usage, `error`, `turn.failed`) | Read when present; output with no `thread.started` is `ErrUnparsed`. |
| Grok's reply text field and error field | `grok.TextFields` (`result`, `text`, `response`); an `error` string mapped with `adapter.ClassifyName`. |
| Error class names other than `rate_limit` | `adapter.ClassifyName` also accepts Claude Code's likely names (`authentication_failed`, `billing_error`, `server_error`, `error_max_turns`). |

## Testing an adapter

CI has no vendor logins. Test adapters against the fake CLIs in
`testdata/fakes` and the recorded output in `testdata/fixtures`. Record a
fixture with `aggy dev capture` and clean it with `aggy dev scrub`
(see `testdata/fixtures/README.md`); the fixture check in `make test`
fails on any home path, email, or token left in one. Tests that touch home
paths set `HOME` to a temp dir.
