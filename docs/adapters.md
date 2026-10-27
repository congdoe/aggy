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
  `--fork-session`. Use `--no-alt-screen` to keep scrollback.

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

## Testing an adapter

CI has no vendor logins. Test adapters against the fake CLIs in
`testdata/fakes` and the recorded output in `testdata/fixtures`. Scrub
fixtures of personal content before you commit them. Tests that touch home
paths set `HOME` to a temp dir.
