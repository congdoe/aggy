# Fake CLIs

`fake-claude`, `fake-codex`, and `fake-grok` stand in for the vendor CLIs, so tests never run a real vendor CLI or need a login. `fake-hook` records hook calls. All four are one program, `fake/`, named by its binary name (or `AGGY_FAKE_VENDOR`).

Tests build them with `fakecli.Build(t)` from `internal/fakecli`, which runs `go build` into a temp dir. They are not built by `go build ./...`.

## Rules

- A fake refuses to run (exit 97) when `HOME` is unset, relative, or the real user's home, or when any vendor home (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `GROK_HOME`, else `$HOME/.<vendor>`) is relative or inside the real home. Paths are compared after resolving symlinks, and case-insensitively on darwin. Tests set `HOME` to a temp dir and clear or set the three vendor variables.
- It accepts only the flags recorded in [docs/adapters.md](../../docs/adapters.md). Any other flag fails with exit 2 and `unknown option`, as the real CLIs do, so an adapter that invents a flag fails its test. A scenario can allow more with `extra_flags` and `extra_switches`.
- It never reads anything except the scenario file and its stdin.

## Environment

| Variable | Meaning |
| --- | --- |
| `AGGY_FAKE_SCENARIO` | Path of the scenario JSON file. Unset means the defaults: success, no output. |
| `AGGY_FAKE_RECORD` | A file each run appends one JSON line to: vendor, mode, args, parsed flags, positionals, cwd, stdin, whether stdin was a terminal, the session id, the names of every environment variable, and the values of `AGGY_*` ones. `fakecli.Records` reads it. |
| `AGGY_FAKE_VENDOR` | Overrides the vendor taken from the binary name. |

## Flags honored

| Command | Value flags | Switches |
| --- | --- | --- |
| `claude` | `--output-format`, `--permission-mode`, `--session-id`, `--resume` | `-p`, `--fork-session` |
| `grok` | `--output-format`, `--permission-mode`, `--session-id`, `--resume`, `--tools`, `--disallowed-tools`, `--max-turns` | `-p`, `--fork-session`, `--no-alt-screen`, `--continue` |
| `codex` | `-c` | `--no-alt-screen` |
| `codex exec`, `codex exec resume <id>` | `-c`, `--disable` (repeatable), `--sandbox`, `-i` | `--json` |
| `codex resume <id>` | `-c` | `--no-alt-screen` |

Every command also takes `--version` and `--help` (`-h`). `--flag=value` works too. Grok's `--continue` is recorded in the proposal (§3) and in docs/adapters.md.

Values are checked where docs/adapters.md records them, with exit 2 otherwise:

- `--session-id` must be a UUID.
- `--output-format` must be `json`, `--permission-mode` must be `plan`, and `--sandbox` must be `read-only`. `extra_values` adds values.
- `claude -p`, `codex exec`, and `codex exec resume <id>` need a prompt, as an argument or on stdin.
- An interactive run needs a terminal on stdin, unless `interactive_without_tty` is set.
- Rules that are not recorded are opt-in through `strict`: `resume-with-session-id` refuses `--resume` with `--session-id` unless `--fork-session` is given.

Headless means `-p` (Claude, Grok) or `codex exec`. A headless run reads stdin to EOF when it is not a terminal: `codex exec` waits on such a stdin, which is why aggy runs it with `/dev/null`. An interactive run prints `stdout`, calls the hooks, then reads lines until `/exit` or EOF.

Session ids: `--session-id` sets a new one (Claude, Grok); `--resume <id>` keeps `<id>` unless `--fork-session` is given; Codex never takes an id in advance, and resumes with `codex [exec] resume <id>`. Otherwise the id is the scenario's `session_id`, or a random UUID.

## Scenario format

A JSON object. Every field is optional, and unknown fields are an error.

| Field | Type | Meaning |
| --- | --- | --- |
| `version` | string | Version for `--version`. Defaults: claude 2.1.311, codex 0.164.0, grok 1.0.70. |
| `version_output` | string | Exact `--version` text, replacing the default `<vendor> <version> (fake)`. |
| `help` | string | Exact `--help` text, replacing the generated list of honored flags. |
| `extra_flags`, `extra_switches` | strings | More value flags and switches to accept, for flags not yet recorded (such as a model flag). |
| `extra_values` | object | More accepted values per flag, e.g. `{"--sandbox": ["workspace-write"]}`. |
| `strict` | strings | Opt-in rules that are not recorded: `resume-with-session-id`. |
| `interactive_without_tty` | bool | Let an interactive run start without a terminal, for tests that pipe stdin. |
| `session_id` | string | Id of a new session. |
| `stdout`, `stderr` | string | Text printed. `stdout` is the terminal output of an interactive run, or the reply of a headless run without JSON output. |
| `json` | object | Claude and Grok `--output-format json` reply. Grok's always gets `sessionId`. |
| `events` | objects | Codex `exec --json` events printed, one per line, after `thread.started`. |
| `error` | string | A limit error: `rate_limit` (Claude, Grok) or `usage_limit_reached` (Codex). Prints the limit text to stderr and exits 1 unless `exit_code` is set. |
| `error_text` | string | Limit text, replacing the default. |
| `error_event` | object | Codex `exec --json` line printed for the error, after `events`. |
| `exit_code` | int | Exit status. |
| `hook_command` | strings | The hook program and its leading arguments. The event name is appended. |
| `hook_payload_in` | string | `stdin` (default) or `arg`, for where the payload goes. |
| `hooks` | objects | `{"event": "Stop", "payload": {...}}`, called in order at turn end. With `error: rate_limit` and no hooks, Claude and Grok call `StopFailure` with `{"error":"rate_limit"}`. Codex has no failure hook. |
| `images` | int | Number of images to write where the CLI does: `$CODEX_HOME/generated_images/<thread-id>/N.png`, or `$GROK_HOME/sessions/<url-encoded cwd>/<sessionId>/images/N.jpg`. Claude's image path is not recorded, so Claude refuses. |
| `session_files` | objects | `{"path": "...", "content": "..."}` files written under the vendor's home. The path must stay inside it. |
| `sleep_ms` | int | Delay before output, for timeout tests. |

Strings in `stdout`, `stderr`, `json`, `events`, `error_event`, hook payloads, `session_files`, and `version_output` expand `{session_id}`, `{cwd}`, `{home}`, `{vendor_home}`, and, on Grok, `{session_dir}`.

Example, a Codex image borrow that hits its limit:

```json
{
  "session_id": "0199-thread",
  "images": 2,
  "events": [{"type": "item.completed", "item": {"text": "made {session_id}"}}],
  "error": "usage_limit_reached",
  "error_event": {"type": "error", "code": "usage_limit_reached"}
}
```

## What is not recorded, so is configurable

The fakes do not invent vendor formats. These are unknown, so the scenario supplies them, and tests that rely on them must say so:

- `--version` and `--help` text of every CLI.
- The Claude and Grok JSON reply beyond Grok's `sessionId`.
- Every Codex `--json` event after `thread.started`, including the shape of the `usage_limit_reached` error event.
- Session transcript paths and formats, for every vendor.
- How each CLI passes a hook its payload (stdin or argument), and where its hook config lives. The fakes take the command from the scenario, not from vendor config.
- Grok's exact cwd encoding in its session path. The fake uses `url.QueryEscape` (`/` becomes `%2F`).
- Claude's limit text beyond the fragment "hit your limit … resets in", Grok's limit text, and the `StopFailure` error value Claude sends at a plan limit.
- Model flags. Use `extra_flags`.
