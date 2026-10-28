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
| `Probe(ctx, run)` | Reports the CLI's path, version, and capabilities. It never reads tokens; `Connected` stays false (sign-in is `SignIn`). |
| `SignIn()` | The vendor's own sign-in step, as text, and a verified sign-in status check if there is one. `adapter.CheckSignIn` runs the check through the caller's runner. |
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
  directory (`IsolatedCwd`), with file and shell tools removed where the
  CLI allows it. Web search is for research and news only.
- **Ids are never flags.** Every session id in argv or parsed from output
  passes `adapter.ValidSessionID`.
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
grok 1.0.70 (help and source). The live checks of 2026-10-27 and
2026-10-28 ran claude 2.1.312, codex-cli 0.164.0, and grok 1.0.70, and
the Claude adapter declares 2.1.311 to 2.1.312 as tested. They are recorded, with commands and evidence, in
[verification.md](verification.md). Each adapter states the versions it
was tested against.

### Codex

- Headless is `codex exec --json`. The first JSON event is
  `thread.started` with `thread_id`, which is the session id.
- Never run it with an open pipe on stdin: `codex exec` reads a stdin that
  is not a terminal to EOF. aggy gives it the prompt file, which ends at
  once (see "Prompts" below).
- Web search: `-c 'web_search="live"'`. `exec` has no `--search` flag. Each
  query appears as a `web_search` item in the JSON stream.
- Isolation: `--ignore-user-config --sandbox read-only --disable shell_tool
  --disable unified_exec --disable view_image --disable apps --disable
  plugins --disable goals --disable image_generation --disable sleep_tool`
  in an empty temp dir leaves no tool that reads a file (checked live).
  `--disable unified_exec` alone does not remove the shell; `shell_tool`
  does. `apps` and `plugins` must go too, or 52 ChatGPT connector tools
  stay even with `--ignore-user-config`. But `apply_patch`, which no flag
  removes, is a file-guess oracle: its errors differ for a right and a
  wrong guess of a line. So Codex research and news are unsupported for
  now (T6638).
- MCP at launch: `-c mcp_servers.<name>.command=…` and `.args=…`, plus
  `-c mcp_servers.<name>.default_tools_approval_mode="approve"`, because
  `exec` denies MCP calls otherwise. Pass them again on resume. Flags such
  as `--sandbox` go before `resume`, and they apply to the resumed thread.
- Extra context: `-c developer_instructions="…"`, on every subcommand.
- Images land in `~/.codex/generated_images/<thread-id>/*.png`, which is
  `$CODEX_HOME/generated_images/...` when `CODEX_HOME` is set. The image tool
  call does not appear in the JSON stream, and one request can produce more
  than one image, so collect every file in the folder.
- Reference images: `codex exec -i <file>`.
- Resume: `codex resume <id>` and `codex exec resume <id>`. A new session
  cannot be given an id in advance.
- Limit: there is no failure hook, and `exec --json` has no machine code:
  the `error` and `turn.failed` events carry only the text "You've hit
  your usage limit … try again at <local time>". (`usage_limit_reached`
  is only the backend's HTTP body type.) The rollout file has
  `task_complete` with `error.codex_error_info: "usage_limit_exceeded"` and
  `token_count` with `rate_limits.primary|secondary.resets_at` (unix
  seconds); it does not keep the `error` event.

### Grok

- Headless is `grok -p --output-format json`, and the JSON includes
  `sessionId`.
- Flags: `--tools`, `--disallowed-tools`, `--max-turns`, and
  `--permission-mode plan`.
- Research and news: `--tools web_search --no-subagents --disallowed-tools
  "read_file,grep,list_dir,write,search_replace,run_terminal_cmd,web_fetch,Agent,search_tool,use_tool,image_gen,image_edit,image_to_video,reference_to_video,send_feedback,monitor,workflow,todo_write,ask_user_question,enter_plan_mode,exit_plan_mode"`
  should leave only `web_search`, `open_page`, and `open_page_with_find`.
  The live run (the same list plus `open_page,open_page_with_find`, which
  Grok ignored with a warning) got that list as the model's own one-turn
  report, not a wire dump; this exact set has not been run live yet. There is no `x_keyword_search` or `x_semantic_search`
  tool: naming them made Grok warn and ignore the whole `--tools`
  allowlist. `open_page*` cannot be named in `--disallowed-tools`. Every
  other kind (review, image, video) denies `web_search`, which takes
  `open_page*` with it: a live review run listed only its read tools
  (`docs/verification.md` C3).
- MCP at launch: no flag. `--agent <file>` whose frontmatter lists
  `mcpServers` works, with `--allow 'MCPTool(<server>__<tool>)'`.
- Extra context: `--rules "<text>"`, inline only.
- Images land in `~/.grok/sessions/<url-encoded cwd>/<sessionId>/images/*.jpg`,
  or the same path under `$GROK_HOME`. The reply gives the path relative to
  the session folder (`images/1.jpg`). Videos land in the same session
  folder as `videos/<n>.mp4` (source-verified, grok-build 2bdd1d6a,
  `xai-grok-tools/src/implementations/grok_build/video_gen/mod.rs`; not
  seen live).
- Sessions: `--session-id <uuid>`, `--resume <id>` (also with `-p`), and
  `--fork-session`. `--continue` takes the latest session in the current
  directory. Use `--no-alt-screen` to keep scrollback.

### Claude

- Headless is `claude -p --output-format json`. Read-only uses
  `--permission-mode plan`.
- Sessions: `--session-id <uuid>`, `--resume <id>`, and `--fork-session`.
- MCP at launch: `--mcp-config <file> --strict-mcp-config`; it is not kept
  with the session, so pass it again on resume. With an empty
  `{"mcpServers":{}}` no MCP or connector tool is left.
- Extra context: `--append-system-prompt-file <path>` works although
  `--help` omits it. The system prompt is snapshotted on the first
  request, so a new value is ignored on resume.

### Hooks and limits

- Claude and Grok both have a `StopFailure` hook. Its error classes include
  `rate_limit`, and Grok says its classes mirror Claude Code's. Claude's
  payload also carries `error_details`.
- Turn end: Claude uses `Stop`. Codex uses `Stop` and the
  `agent-turn-complete` notify. Grok uses `Stop` and `StopCancelled`.
- Codex has Claude-style hooks (`hooks.json` or `[[hooks.<Event>]]`), and
  `SessionStart` carries `session_id` (the thread id) and `source`. A
  hook runs only once its hash is trusted; one passed with `-c` is
  skipped silently (checked live). Whether it fires in `exec` is still
  open.
- Grok runs the hooks in Claude's `settings.json` by default. Its payload
  has `sessionId` and its environment `GROK_SESSION_ID`, which is how
  aggy tells a Grok call from a Claude one.

## The adapters as built

Each adapter lives in `internal/adapter/<vendor>` and is tested against
its fake with golden argv per kind and parsing tests driven by scenario
files in its `testdata/`.

**Prompts.** A headless prompt is never in argv. Every adapter returns
`Stdin: StdinFile` with `StdinPath` set to the prompt file. For Codex the
prompt argument is `-` after `--`: `codex exec` reads a non-terminal stdin
to EOF, and a regular file reaches EOF at once, so it never waits as it
would on an open pipe. (The recorded rule is "never an open stdin";
`/dev/null` also satisfies it but leaves no way to pass a prompt from a
file.) The borrow runner (`execrun.HeadlessRunner`) opens `StdinPath`
with `safefs.OpenRegular`: no-follow and non-blocking flags, and anything
but a regular file refused before and after the open (a FIFO or device
could block or feed the CLI forever). O_NONBLOCK has no effect on reads of
a regular file, so the descriptor goes to the child as is.

**Session ids and models.** Every session id an adapter puts in argv,
and every id `ParseHeadless` returns, passes `adapter.ValidSessionID`: it
may not be empty or start with `-`, holds only `[A-Za-z0-9-]`, and is a
UUID (Claude and Grok take UUIDs; the Codex thread-id format is not
recorded, so it is held to a UUID too). So an id can never be read as a
flag. Ids are passed as plain arguments (`--resume <id>`, `resume <id>`),
since `--resume=<id>` and `--` before an id are not recorded. When aggy
pre-assigned an id (`SessionID`) or resumes one (`Resume`), `ParseHeadless`
keeps it and returns an error if the CLI reports another. Models pass `adapter.ValidModel`.

**Web search and isolation.** Web search is only for research and news:
any other kind with `WebSearch` is `ErrUnsupported`, because a child that
can read files must not also have web egress. Every kind but review runs
in a fresh, empty temp dir from the OS temp location, which the borrow
runner makes and vouches for with `HeadlessRequest.IsolatedCwd` (research
and news refuse to build without it); review runs read-only at the root of
the git work tree. Per-run files an adapter needs (`adapter.ChildFiler`)
go in `HeadlessRequest.RunDir`, a separate private dir; they are not vendor
config, live only for the run, and do not go through the manifest.

Every child gets its CLI's lockdown flag set (the table below), checked
live on 2026-10-28. A CLI that rejects any of these flags, Codex under
`--strict-config` meeting an unknown feature, or Grok warning `name no
tool` (it then ignores the whole `--tools` allowlist, or removes nothing
for a `--disallowed-tools` name) fails the borrow closed with the error
class `lockdown` (`adapter.CheckLockdown`, `adapter.ErrLockdownRejected`);
aggy never retries without the flag. A CLI whose version is outside the
adapter's tested range, or unreadable, is refused for borrows unless
`--allow-untested` is passed, since the lockdown was verified only there.

**Review input.** Codex keeps the full lockdown for review, which leaves it
no tool that reads a file, so every vendor's reviewer gets its input in
the prompt: `internal/cli` runs `git diff` of the work tree against the
merge-base of `HEAD` with the default branch (`origin/HEAD`, else `main`,
else `master`), or against `--base <ref>`. That covers committed changes
and staged or unstaged changes to tracked files; an untracked new file is
not in the diff unless it is passed with `--path`. git runs through
`execrun` with no fsmonitor, hooks, pager, external diff, or textconv from
the repo's config, and with every filter driver git's config defines
(`git config -z --get-regexp '^filter\.'`) switched off by `-c
filter.<name>.clean= -c filter.<name>.smudge= -c filter.<name>.process= -c
filter.<name>.required=false`, since attributes (`.gitattributes`,
`.git/info/attributes`) would otherwise run a driver on a stat-dirty
file. A driver name `-c` cannot carry is refused. The `--path` files
inside the repo are read through `safefs`. The material is capped at 256 KiB and says
where it was cut. It sits under a "Material" heading in fences longer than
any backtick run inside, marked as data to review, not instructions.
Claude and Grok keep their read tools, at the work tree root. A review
refuses a directory outside a git work tree, `$HOME`, `/`, and a root
that holds or is inside aggy's state dir.

**Errors.** The error class comes from structured fields only: Claude's
`error`, `subtype`, and `is_error`; Grok's JSON `error`; Codex's `error`
and `turn.failed` events. The CLI's own text is read only where the CLI
wrote it: Claude's "hit your limit" in a `result` marked `is_error`, and
stderr on a non-zero exit. A Codex `error` event that a `turn.completed`
follows is a success with a warning in `ErrorDetail`. `Text` and
`ErrorDetail` are raw vendor output that a model or web page may have
shaped: callers pass them through `termsafe` before printing (M1).

**Briefs.** An interactive session with a brief gets one short first
message, `adapter.BriefPrompt(path)`, that tells the agent to read the
file. The brief itself never goes in argv.

**Probes.** `Probe` finds the binary on aggy's absolute `PATH` entries
(`probe.LookPath`) and runs `probe.Detect` with the vendor's `Spec`.
`--version` is parsed keyed on the Spec's program names (`Claude Code`,
`claude`; `codex-cli`, `codex`; `grok`), with an optional `v`; with no
named line, one unambiguous version line is taken, and several are an
error, never a guess. Codex `exec resume` counts as present only when the `exec` help
lists it under `Commands:` or its own help names a session-id argument; a
usage error is a cached absence.

Headless argv per kind, as built (the prompt file is stdin; `<id>` a
session id; `--model <m>`, or `-c model="<m>"` on Codex, only when a model
is given; Grok's `[--max-turns N]` when the kind has a turn budget, which
by default is news 15 and research 30; `<run>` is `HeadlessRequest.RunDir`):

| Kind | Claude | Codex | Grok |
| --- | --- | --- | --- |
| research, news | unsupported | unsupported until T6638 (`codex.Refusal`: apply_patch can confirm guessed file contents) | `grok -p --output-format json --tools web_search --disallowed-tools <D> --no-subagents` |
| image | unsupported | `codex exec [-i <ref>]... --json <L without --disable image_generation> --skip-git-repo-check -- -` | `grok -p --output-format json [--session-id <uuid>] --tools image_gen --disallowed-tools <D> --no-subagents` |
| image `--edit` | unsupported | `codex exec [-i <ref>]... --json <L without image_generation> --skip-git-repo-check resume <id> -- -` | `grok -p --output-format json --resume <id> --tools image_gen,image_edit --disallowed-tools <D> --no-subagents` |
| video | unsupported | unsupported | `grok -p --output-format json --tools image_gen,image_to_video --disallowed-tools <D> --no-subagents` (unverified headless) |
| review | `claude -p --output-format json --permission-mode plan --strict-mcp-config --mcp-config <run>/mcp.json --tools Read,Grep,Glob [--setting-sources user] [--settings <run>/settings.json] [--session-id <uuid> \| --resume <id>]` | `codex exec --json <L> -- -` (in a git work tree, so no `--skip-git-repo-check`) | `grok -p --output-format json --permission-mode plan --tools read_file,grep,list_dir --disallowed-tools <D> --no-subagents` |
| edit (M3; refused by `aggy borrow`) | `claude -p --output-format json --permission-mode acceptEdits --strict-mcp-config --mcp-config <run>/mcp.json` | `codex exec --json --sandbox workspace-write -- -` | `grok -p --output-format json --tools read_file,grep,list_dir,write,search_replace --disallowed-tools <D> --no-subagents` |

`L` is `codex.Lockdown`: `--strict-config --ignore-user-config --sandbox
read-only --disable shell_tool --disable unified_exec --disable view_image
--disable apps --disable plugins --disable goals --disable image_generation
--disable sleep_tool`. `--strict-config` makes an unknown `--disable`
feature an error instead of a silent no-op. `D` is `grok.Lockdown` minus
the kind's own tools, with `search_tool`, `use_tool`, and `Agent` always
in it: `read_file, grep, list_dir, write, search_replace,
run_terminal_cmd, web_fetch, Agent, search_tool, use_tool, image_gen,
image_edit, image_to_video, reference_to_video, send_feedback, monitor,
workflow, todo_write, ask_user_question, enter_plan_mode, exit_plan_mode,
web_search`. `open_page` and `open_page_with_find` are not in it: they
cannot be removed by name. Denying `web_search` takes them along: the
review argv, run live with grok 1.0.70 (2026-10-28,
`docs/verification.md` C3), left the model reporting only `read_file`,
`grep`, and `list_dir`, with empty stderr; image and video deny
`web_search` the same way. `--disable-web-search` (not in a recorded
help) is unverified and not needed (TODO(T6737)). Claude's `<run>/mcp.json` is `{"mcpServers":{}}`; the
`--tools` names are unverified. `--setting-sources user` (so a review does
not run the reviewed repo's own `.claude/settings.json` hooks) and
`--settings` (the PreToolUse guard) are passed only when the probe found
them in `claude --help` (capabilities `setting-sources` and `settings`);
without the first, that gap is open (TODO(T6643)).

Codex puts every exec flag before the `resume` subcommand, which takes
only its own flags; `-i` takes one or more values, so the references come
right after `exec` and `--json` ends them. `-i` also splits values on
commas, so a reference path holding a comma is refused. `SessionID` adds `--session-id`
on Claude and Grok; Codex refuses it. On Grok, `AllowedTools` may only
name tools the kind already permits, compared case-insensitively, and
never the MCP meta-tools: research and news get `web_search`, review the
three read tools, edit no shell (`aggy check` runs the tests).

**The PreToolUse guard.** Each child runs with `AGGY_DELEGATED=1`, so its
own `aggy borrow` refuses (one hop). Where a CLI takes a per-run hook,
the child also gets `aggy hook pretooluse`, which denies a shell command
that runs `claude -p`, `codex exec`, `grok -p`, or `aggy borrow` at
command position. It is an accident guard, not a security boundary.
Claude gets it through `--settings`; Codex (hooks need a trusted hash,
and `-c` hooks are skipped) and Grok (no per-run hook flag; it does load
Claude's `settings.json` hooks) do not: TODO(M2 hooks). No M1 kind keeps
a shell, so it is a backstop.

Interactive argv: Claude `claude [--session-id <uuid> | --resume <id>
[--fork-session [--session-id <uuid>]]] [<brief pointer>]`; Codex `codex
[--no-alt-screen] [<brief pointer>]` or `codex resume [--no-alt-screen]
<id> [<brief pointer>]`; Grok `grok --no-alt-screen` with the same session
flags as Claude. `--no-alt-screen` is unverified for both: neither help
text nor a live check in `docs/verification.md` records it. Codex gets it
only when the launch probe finds it in `codex --help` (capability
`no-alt-screen`); Grok always gets it, from the Grok session notes above.

Artifacts are paths only; copying them is the borrow runner's job, through
`internal/safefs` and the artifact gate (one link, made during the run,
PNG or JPEG or MP4 by magic bytes, under a size cap; design section 13). Codex: every file (not
only `*.png`) in `$CODEX_HOME/generated_images/<thread-id>/` (else
`~/.codex/...`), all counted as made; the gate keeps the real images. Grok:
every file in `$GROK_HOME/sessions/<encoded cwd>/<sessionId>/images/`
(`videos/` for the video kind)
(else `~/.grok/...`), falling back to the single `sessions/*/<sessionId>/
images/` folder; several are an error. Claude has none.
`TranscriptTail` returns `ErrNoTranscript` for every vendor: no transcript
path or format is recorded. `ConfigPatches` returns nothing yet; the Codex
`CLAUDE.md` fallback is built in `internal/parity`.

### Sign-in

No sign-in status command is recorded above, so no adapter has a
`SignIn().Check`: `adapter.CheckSignIn` returns `unknown` for every
installed CLI and runs nothing, and `aggy setup` records `unknown`. The
sign-in steps each adapter prints come from docs/design.md section 2
(`claude` then `/login`, `codex login`, `grok login`). They are not
verified here, so aggy only prints them and never launches them. When a
status command is verified (its arguments and how its output or exit
status reads), it goes in the adapter's `SignIn().Check`, whose `Parse`
returns a status and keeps none of the output. The fakes answer such a
command through a scenario `replies` entry.

### Proxy and API-key routes

`internal/checkup` flags what can take a CLI off its subscription, by
variable name, file, and key, never by value. `aggy setup` shows these as
warnings and `aggy doctor` reports them:

Each is marked by where it comes from. **Source** is the vendor's source
at a pinned revision: Codex at tag `rust-v0.164.0` (commit `6aaac69`),
which matches the tested 0.164.0; Grok at `xai-org/grok-build` commit
`2bdd1d6a` (2026-09-29), possibly newer than the tested 1.0.70. **Docs**
is the vendor's published docs (Claude Code: env-vars, settings).
**Design** is docs/design.md section 2 only.

| What | Vendor | Source |
| --- | --- | --- |
| `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `ANTHROPIC_BASE_URL` | Claude | docs (env-vars) |
| `CODEX_API_KEY`: `codex exec` prefers it over any stored login, and it bills the API | Codex | source: `login/src/auth/manager.rs` (`load_auth`), `exec/src/lib.rs` |
| `OPENAI_API_KEY` | Codex? | design; whether Codex reads it is unknown |
| `OPENAI_BASE_URL`, and any `OPENAI_*_BASE_URL` | none | source: **Codex does not read it** (only its network-proxy code mentions it). Still flagged, under no vendor, since other OpenAI tools may |
| `CODEX_*_BASE_URL` | Codex | a catch-all; no such variable is in the source |
| `XAI_API_KEY` | Grok | design; docs |
| `GROK_MODELS_BASE_URL` | Grok | source/docs: custom inference base URL |
| `GROK_XAI_API_BASE_URL` (by the `GROK_*_BASE_URL` rule) | Grok | source/docs: overrides `endpoints.xai_api_base_url` |
| Codex `config.toml` (in `$CODEX_HOME`, else `~/.codex`): top-level `openai_base_url`, or `base_url` in any `[model_providers.<id>]` | Codex | source: `config/src/config_toml.rs`, `model-provider-info/src/lib.rs` |
| Claude `settings.json` and `settings.local.json` (in `$CLAUDE_CONFIG_DIR`, else `~/.claude`): an `env` block setting any variable above, or an `apiKeyHelper` | Claude | docs (settings) |
| Grok `config.toml` (in `$GROK_HOME`, else `~/.grok`): `endpoints.xai_api_base_url` (the base URL Grok's image and video tools use), `endpoints.models_base_url`, and `base_url`, `env_key`, or `api_key` in any `[model.<id>]` (a bring-your-own-key model) | Grok | source: `xai-grok-shell/src/agent/media_tool_config.rs`; docs: config reference |

Grok's `toolset.image_gen` and `toolset.video_gen` `base_url` and
`env_key`, which docs/design.md once named, **do not exist** in Grok's
source or docs (the documented `[toolset.*]` tables are `bash`,
`ask_user_question`, `web_fetch`, `web_search`, and `file_toolset`), so
they are not checked. Not checked yet: Claude's managed settings
(`/Library/Application Support/ClaudeCode/managed-settings.json`,
`/etc/claude-code/`), Grok's `managed_config.toml`, and
`CODEX_ACCESS_TOKEN`.

A key under a table the person named, such as `[model_providers.<id>]` or `[model.<id>]`, is reported as `model_providers.<id>.base_url`; the id itself, often a company or person's name, is shown on screen but left out of `aggy doctor --report`. A key of an unexpected shape (such as Grok's top-level `model = "grok-4"`) is skipped, never a reason to stop checking the file.

Config files are read through `safefs`. A symlinked file (as dotfile
managers make) is resolved, its target must pass the credential check,
and the target is read with `safefs.ReadFileLimit`, which refuses a
link, a non-regular file, and anything over 1 MiB. A file that cannot be
read or parsed, or a link to a credential file or to nothing, is a
`config-unreadable` warning naming the file. The process runner never
passes any of these variables to a CLI (`execrun.Env`).

### Tested versions

Each adapter declares the CLI versions it was tested on, from the verified
versions above: claude 2.1.311 to 2.1.312, codex-cli 0.164.0, and grok
1.0.70.
Each declares it as `Tested` and through `TestedVersions()`.
`adapter.VersionWarning(a, version)` returns a warning when a probed
version falls outside that range or cannot be read, and "" inside it, for
`aggy doctor` to show, and `aggy borrow` refuses such a CLI unless
`--allow-untested` is passed. Versions compare the semver way (pre-release
identifiers numerically where both are numbers, build metadata ignored),
and `x.y` means `x.y.0`. A `--version` aggy cannot read does not fail the
probe: `Version` stays empty, nothing is cached, and the warning reports
it.

### Unconfirmed facts and their fallbacks

These are implemented behind named fallbacks and marked `unconfirmed` in
the code until a live check settles them.

| Fact | Fallback in code |
| --- | --- |
| Codex `SessionStart` hook fires in `exec` with `session_id` (T6616; source-verified, and hooks need a trusted hash) | Not used; the session folder watch stays the plan. |
| Codex limit signals (T6617). Source shows `--json` has no limit code, only the text; the rollout has `task_complete.error.codex_error_info` and `token_count.rate_limits`. A real limit was not captured live | `codex.LimitCode` and `codex.LimitText` are matched in the code and message of `error` and `turn.failed` events, and in stderr only on a non-zero exit with no error event. |
| Settled (T6618): those flags leave no file-read tool, but `apply_patch` is a file-guess oracle, and `apps`, `plugins`, `goals`, `image_generation`, and `sleep_tool` must be disabled too; Codex research and news are refused until T6638 | `codex.Lockdown` for image and review; research and news refused by `codex.Refusal`. |
| Extra context at session start (T6619): settled as Claude `--append-system-prompt-file`, Codex `-c developer_instructions`, Grok `--rules`; resume behavior of the last two is open | A positional first message pointing to the brief file (`adapter.BriefPrompt`). |
| `codex exec -` reads the prompt from stdin | Codex headless passes `-- -` with the prompt file on stdin. Confirmed live with `codex exec -i <png> ... -- - < prompt`. |
| Codex exec flags go before `resume`, which takes only its own flags; `-i` takes several values | `codex exec [-i <ref>]... --json ... resume <id> -- -`. Confirmed by help and live: flags before `resume` apply to the resumed thread. |
| `codex exec --ignore-user-config` keeps user MCP servers out (confirmed live; ChatGPT connector tools stay unless `--disable apps --disable plugins`) | Always passed, in `codex.Lockdown`; a CLI that rejects it fails the borrow. |
| The Codex thread-id format | Held to a UUID by `adapter.ValidSessionID`. |
| `claude -p` and `grok -p` read the prompt from stdin | Both pass the prompt file on stdin with no prompt argument. |
| Model flags | Claude and Grok `--model <m>`, Codex `-c model="<m>"`, all after `adapter.ValidModel`. |
| `--permission-mode acceptEdits` (Claude edit) and `--sandbox workspace-write` (Codex edit) | `claude.PermissionEdit`, `codex.SandboxEdit`. |
| `grok --tools` alone removes every other tool, and it combines with `--disallowed-tools` | The per-kind `--tools` allowlists, plus `grok.Lockdown` minus the kind's tools as `--disallowed-tools`, and `--no-subagents`; a `name no tool` warning fails the borrow. |
| `image_edit`, `image_gen` plus `image_to_video` for video, work headless on Grok (there is no `video_gen`) | `grok.ImageTools` plus `image_edit` on resume, `grok.VideoTools`. |
| Claude `--tools Read,Grep,Glob` names, and whether `--setting-sources user` keeps the `--settings` layer | `claude.ReviewTools`; `--setting-sources` only with the probed capability. |
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
