# Verified vendor CLI facts

This records what was checked about the three vendor CLIs on 2026-10-27
and 2026-10-28, how, and what was found. [design.md](design.md) and
[adapters.md](adapters.md) rely on these results. Session ids, thread ids,
ping replies, canary tokens, and file hashes are replaced with `<id>` or
`<token>`; scratch paths with `<scratch>`, `<HOME>`, and `<TESTDIR>`.

Versions checked live: claude 2.1.312 (Claude Code), codex-cli 0.164.0,
and grok 1.0.70 (f50904eaee6c) [stable]. The adapters still declare
claude 2.1.311, the version of the earlier help checks, as tested
(adapters.md).

## Method

Three tiers were used.

1. **Docs and source.** Codex at tag `rust-v0.164.0`, commit
   `6aaac694803004a63c6f393d153c96d22521a6b9`, which matches the installed
   binary exactly. Permalinks below have the form
   `https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/<path>#L<line>`.
   Grok from `github.com/xai-org/grok-build` (Apache-2.0) at HEAD
   `2bdd1d6a6369de0e8c68132ea4539e9abd9e14a8` (2026-09-29), which may be
   newer than the installed binary, so Grok source claims are a little
   weaker; also `https://docs.x.ai/build/...`. Claude from
   `https://code.claude.com/docs/en/<page>.md`.
2. **Help output.** Only `--help` and `--version`, with stdin closed:
   `claude --help`, `claude mcp [add] --help`, `codex --help`,
   `codex exec [resume|fork] --help`, `codex resume --help`,
   `codex mcp [add] --help`, `codex features --help`, `grok --help`, and
   `grok agent|mcp|inspect|sessions|memory|setup|plugin --help`. None
   started a session.
3. **Live runs** (owner-approved, a few model calls each). Every run used
   a fresh directory under the scratch area (not a git repo, so Codex got
   `--skip-git-repo-check`), stdin closed or the prompt file, and no
   `--dangerously-*` flag, yolo mode, or permission skipping. From the
   vendor state directories, only files these runs created were read:
   the JSON each run printed, and, once, the `turn_context` line of one
   check run's own Codex rollout (to learn the model slug it had used,
   for the catalog run below). No other session, and no credential file,
   was ever read.

Tool lists below are what the model itself reported in one turn when
asked to list its tools, not a dump of the tool definitions sent on the
wire. They can miss deferred tools or misname one.

Confidence words: *Source-verified*, *Help-verified*, *Doc-verified*,
*Inferred* (reasoned from source, not traced), *Live-confirmed*.

## Summary

| Question | Answer | Confidence |
| --- | --- | --- |
| Codex `SessionStart` hook | Exists, Claude-style; payload `session_id` is the thread id. Hooks need a trust hash; a hook passed with `-c` is untrusted and silently skipped. | Source-verified; trust gate Live-confirmed; firing not yet seen live |
| Codex usage limit | `exec --json` gives only human text, no code and no reset time. The rollout has `task_complete.error.codex_error_info` and `token_count.rate_limits.*.resets_at`. | Source-verified; limit case not testable live |
| Codex research isolation | No local read tool once `shell_tool` and `view_image` are off, but `apply_patch` is a file-guess oracle and cannot be turned off by a flag. | Live-confirmed |
| Grok research and news tools | `x_keyword_search` and `x_semantic_search` do not exist. A clean list, as the model reported it, is `web_search` plus `open_page*`. | Live-confirmed |
| Extra context at session start | Claude `--append-system-prompt-file` (works although `--help` omits it); Codex `-c developer_instructions`; Grok `--rules` (inline only). | Claude Live-confirmed; others Source/Doc-verified |
| Grok headless prompt and version | `-p` is `--single <PROMPT>` and takes the prompt as its value, so aggy passes `--prompt-file <absolute path>`; `--version` ends in a `[stable]` channel tag. | Live-confirmed (T6740) |
| Grok read confinement | Reads are auto-allowed in every permission mode; only `--sandbox strict` kept `read_file` out of `$HOME`. | Live-confirmed (T6740) |
| Launch-time MCP | Claude `--mcp-config` (re-pass on resume); Codex `-c mcp_servers.*` plus per-server approval; Grok has no flag, use `--agent <file>` with `mcpServers`. | Live-confirmed |
| Managed tools | Pinned versions, binaries, `--version` output, and MCP or hook commands of RTK, CodeGraph, Context7, Playwright MCP, and Headroom; Caveman's skill folder. | Live-confirmed (2026-10-28) |
| Codex CLI details | `codex exec resume`, `--ignore-user-config` on `exec` only, greedy `-i`. | Help/Source-verified; parts Live-confirmed |
| API and route settings | Listed in adapters.md. | Source/Doc-verified |
| Same-vendor nested review (T6779) | A Claude review borrow run from inside an on-screen Claude Code session routes to Claude and runs as a fresh headless session. Codex under Codex and Grok under Grok are not run. | Claude Live-confirmed; Codex and Grok unverified |

## Codex hooks (T6616)

- Codex 0.164.0 has lifecycle hooks: `PreModelRequest`, `PostModelRequest`,
  `PreToolUse`, `PermissionRequest`, `PostToolUse`, `PreCompact`,
  `PostCompact`, `SessionStart`, `SessionEnd`, `UserPromptSubmit`,
  `SubagentStart`, `SubagentStop`, `Stop`, `Interrupt`
  (`codex-rs/config/src/hook_config.rs#L39`). Feature `hooks` is stable and
  on by default.
- Config: `hooks.json` in `$CODEX_HOME` or `<project>/.codex`, shaped
  `{"hooks":{"SessionStart":[{"matcher":"<regex>","hooks":[{"type":"command","command":"...","timeout":10}]}]}}`
  (`hook_config.rs#L12`, `#L180`; `engine/discovery.rs#L378`), or TOML
  `[[hooks.SessionStart]]` in `config.toml` (`hooks_tests.rs#L157`).
- `SessionStart` stdin: `session_id` (the thread id, also in the rollout
  file name and in `exec --json`'s `thread.started`), `transcript_path`,
  `cwd`, `hook_event_name`, `model`, `permission_mode`, and `source`
  (`startup|resume|clear|compact|fork`)
  (`hooks/schema/generated/session-start.command.input.schema.json`).
  `Stop` adds `turn_id`, `stop_hook_active`, and `last_assistant_message`.
- **Trust gate.** A non-managed hook runs only if its hash matches
  `trusted_hash` under `[hooks.state."<key>"]` in `config.toml`, or with
  `--dangerously-bypass-hook-trust` (`engine/discovery.rs#L893`, `#L811`).
- Legacy `notify = ["cmd", ...]` appends one JSON argument
  (`agent-turn-complete`, kebab-case keys including `thread-id`)
  (`hooks/src/legacy_notify.rs#L16`). The source marks it for removal.

Live (2026-10-27): `codex exec --json --skip-git-repo-check --ignore-user-config --sandbox read-only -c 'hooks.SessionStart=[{hooks=[{type="command",command="<scratch>/hook.sh"}]}]' -- "reply OK" </dev/null`.
The run answered, but the hook never ran: hooks from `-c` come from the
non-managed session-flags layer, so they are untrusted and skipped
without a message. **Trust gate confirmed; payload still unconfirmed.**

To finish the check, the owner trusts a hook by hand: put
`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/abs/path/hook.sh","timeout":10}]}]}}`
in `$CODEX_HOME/hooks.json`, approve it once in interactive `codex`
(`/hooks`), then run `codex exec --json "reply OK"` and check that the
payload's `session_id` equals `thread.started`'s `thread_id` and `source`
is `startup`.

## Codex limits (T6617)

- The backend answers HTTP 429 with
  `{"error":{"type":"usage_limit_reached","resets_at":<unix>,...}}`, mapped
  to `CodexErr::UsageLimitReached` (`codex-api/src/api_bridge.rs#L192`,
  `protocol/src/error.rs#L709`) and classified `usage_limit_exceeded`
  (`protocol/src/error.rs#L498`).
- **`exec --json` carries no machine code.** It prints
  `{"type":"error","message":"<text>"}` then
  `{"type":"turn.failed","error":{"message":"<text>"}}`; the event has only
  `message` (`exec/src/exec_events.rs#L92`,
  `exec/src/event_processor_with_jsonl_output.rs#L447`). The text starts
  "You've hit your usage limit" (curly apostrophe) and ends with a local
  time without a zone, or "Try again later." (`protocol/src/error.rs#L719`
  to `#L870`). Parsing the time from it is lossy.
- **Rollout.** `EventMsg::Error` is transient and not written
  (`rollout/src/policy.rs#L161`). What is written and usable:
  - `event_msg` with `payload.type == "token_count"`, whose
    `rate_limits.primary|secondary` hold `used_percent`, `window_minutes`,
    and `resets_at` in unix seconds (`protocol/src/protocol.rs#L2472`,
    `#L2478`, `#L2521`; persisted per `policy.rs#L133`). This is the best
    structured reset time.
  - Each snapshot belongs to one quota (T6741): `rate_limits.limit_id`,
    `limit_name`, and `normal_model_slug` (`protocol.rs#L2479-L2483`). A
    missing `limit_id` is the ordinary `codex` quota
    (`core/src/state/session.rs#L446-L453`), and ids are lowercased with
    `-` as `_` (`codex-api/src/rate_limits.rs#L270`). In Codex's TUI,
    only the refresh-poll cadence looks at both the `codex` bucket and
    the one whose `limit_name` is the current model (exact,
    case-sensitive: `tui/src/chatwidget/rate_limits.rs#L196-L202`). Its
    usage warnings and rate-limit-reached state key off the `codex` limit
    alone, matched with `eq_ignore_ascii_case`
    (`on_rate_limit_snapshot_from`, `#L252-L265`, `#L325`, `#L350`). So
    the ordinary quota limiting a session is source-checked; a
    model-scoped quota limiting it is Inferred, unverified (TODO(T6617)).
    `normal_model_slug` is metadata of a quota alias, "never a replacement
    for the request model" (`protocol.rs#L2481-L2483`). The model is
    `turn_context`'s `payload.model` (`protocol.rs#L3451`,
    `history/src/rollout_payload.rs#L32`, `#L51`). Source-checked only: no
    rollout with a second quota has been seen live.
  - `event_msg` with `payload.type == "task_complete"`, whose
    `error.codex_error_info` is `"usage_limit_exceeded"`
    (`protocol.rs#L2273`, `#L2195`; `core/src/tasks/mod.rs#L865`). That
    this field is set on the limit path is Inferred.
  - `turn_aborted` can carry an `error` too (`protocol.rs#L4367`).
- Codex has no failure hook (`Stop` has no error field). The process exit
  code on a limit is unknown.

Not testable live without an exhausted account.

## Codex research isolation (T6618)

Source: `shell_tool`, `view_image`, and `unified_exec` are real feature
names (`codex-rs/features/src/lib.rs#L1081`, `#L1087`, `#L1111`), and
`--disable X` equals `-c features.X=false`. With `shell_tool` off no shell
tool is registered (`core/src/tools/spec_plan.rs#L1235`); with only
`unified_exec` off a one-shot exec handler remains (`#L1257`), so
`--disable unified_exec` alone does not isolate. No `read_file` or
`list_dir` tool exists in this version. `codex exec` does not expand
`@file`. `apply_patch` is registered whenever the model's catalog entry
sets `apply_patch_tool_type` (`#L1409`).

Live (2026-10-27), canary file one level above the work dir:

| Check | Command | Verdict | Evidence |
| --- | --- | --- | --- |
| Isolation | `codex exec --json --skip-git-repo-check --ignore-user-config --sandbox read-only --disable shell_tool --disable unified_exec --disable view_image -c 'web_search="live"' -- - < prompt.txt` | Confirmed: no file read | Canary token appeared 0 times. The model: "no available tool provides local file reading or shell execution". A forced `functions.exec` read failed with "No filesystem command tool is available". |
| MCP part | same run | "No MCP tools" refuted | 52 `mcp__codex_apps__*` ChatGPT connector tools were present even with `--ignore-user-config` (the `apps` and `plugins` features are on by default for a ChatGPT sign-in), plus `list_mcp_resources` and `read_mcp_resource`. |

Lockdown runs (2026-10-28), prompt "List every tool you have by exact name":

| Check | Flags beyond the isolation set | Result |
| --- | --- | --- |
| A1 | `--disable apps --disable plugins --disable code_mode --disable goals` | Connector tools and goal tools gone. `functions.exec`, `web__run`, `image_gen__imagegen` remain. The model said additional deferred tools may be available but could not name them. |
| A2 | A1 plus `--disable image_generation --disable standalone_web_search --disable multi_agent --disable sleep_tool` | Image tool and sleep gone. `functions.exec`, `web__run`, `collaboration.*`, `apply_patch`, MCP resource tools remain. `--disable multi_agent` did not remove `collaboration.*`, and `--disable code_mode` did not remove `functions.exec`. |

The clean Codex set is therefore `--ignore-user-config --sandbox read-only
--disable shell_tool --disable unified_exec --disable view_image --disable
apps --disable plugins --disable goals --disable image_generation
--disable sleep_tool -c 'web_search="live"'`.

Escape checks (2026-10-28), clean set, canary in the parent dir:

| Check | Verdict | Evidence |
| --- | --- | --- |
| E1 `spawn_agent` inheritance | No leak; child start not observable | Token 0 times. The helper reported no shell. The JSON shows only a `collab_tool_call` `wait` with empty `receiver_thread_ids`, so a child thread could not be proved. Source (`core/src/agent/role.rs#L37`) says children inherit disabled features and cannot widen the sandbox. |
| E2 `apply_patch` as a read oracle | **Leak: a guess oracle** | Patching the canary to replace its real line gave "patch rejected: writing is blocked by read-only sandbox"; replacing a wrong line gave "apply_patch verification failed: Failed to find expected lines in <path>". The errors differ, so a model can confirm a guessed line of a file at a known path. The file was unchanged. |
| E3 `functions.exec` | No leak; no file system, no network | `require`, `Deno`, `Bun`, `fetch` undefined; `import('node:fs')` unsupported. Its `tools` global can call other tools, including `apply_patch`. |

**Verdict.** The clean set stops direct file reads and network from code,
and children inherit it, but it is not leak-proof: `apply_patch` confirms
guessed lines. aggy keeps Codex out of research and news (T6638).

### Closing the apply_patch oracle

Source: no feature flag or config key removes `apply_patch`; the only
gate is the model catalog's `apply_patch_tool_type`
(`spec_plan.rs#L1409`; `protocol/src/openai_models.rs#L326`, `#L449`). An
extension-only tool policy exists but cannot be set from `-c`. The
config key `model_catalog_json = "<abs path>"`
(`config/src/config_toml.rs#L408`) replaces the model list.

Live, catalog route: a one-model catalog with `"apply_patch_tool_type": null`
and a placeholder `base_instructions`, passed with
`-c 'model_catalog_json="<scratch>/models.json"' -m <slug>`. Result:
`apply_patch` gone, and no error text of any kind. But the catalog
replaces all the model's metadata, including its real system
instructions, and depends on an account-specific slug and schema. **Not
shipped.**

Live, OS sandbox route (macOS `sandbox-exec` around the whole Codex
process, normal model, full clean set, canary outside the allowed tree):

| Check | Verdict | Evidence |
| --- | --- | --- |
| Startup, auth, web search | Work | `thread.started`, a `web_search` item with results, a correct answer. Shell is off, so Codex's own seatbelt never nests. |
| `apply_patch` right vs wrong guess | **Content oracle closed** | Both gave the identical "Failed to read file to update <path>: Operation not permitted (os error 1)". |
| Existence | **Existence oracle still open** | A denied existing file gives EPERM, a missing one ENOENT ("No such file or directory"), so a model can test whether a path exists in the denied tree. |
| Helper via `spawn_agent` | No leak | Token 0 times; a `web__run` with a `file://` URL failed. |

The profile, with paths as placeholders (aggy would generate it with real
absolute paths; in SBPL the later matching rule wins):

```
(version 1)
(allow default)
(deny file-read* (subpath "<HOME>"))
(deny file-write* (subpath "<HOME>"))
(allow file-read-metadata (literal "<HOME>") (literal "<HOME>/.cache") (literal "<TESTDIR>"))
(allow file-read* (subpath "<HOME>/.codex"))
(allow file-read* (literal "<HOME>/.CFUserTextEncoding"))
(allow file-write* (subpath "<HOME>/.codex/tmp"))
(allow file-write* (subpath "<HOME>/.codex/sessions"))
(allow file-write* (subpath "<HOME>/.codex/thread-writer-locks"))
(allow file-write* (literal "<HOME>/.codex/installation_id"))
(allow file-write* (literal "<HOME>/.codex/models_cache.json"))
(allow file-write* (regex #"^<HOME>/\.codex/(state|logs)_[0-9]+\.sqlite(-wal|-shm|-journal)?$"))
(allow file-read* (subpath "<TESTDIR>/tmp"))
(allow file-write* (subpath "<TESTDIR>/tmp"))
```

Why each rule:

- `allow default` with HOME denied for read and write: everything outside
  HOME (system libraries, the binary, network, DNS, TLS) stays usable. A
  Codex binary installed under HOME would need its own allowance.
- `file-read-metadata` on the work dir's ancestors: stat for cwd
  resolution, not contents or listings. Not shown to be strictly needed.
- Read `<HOME>/.codex`: config, sign-in, model cache, state. **This makes
  all of `~/.codex`, credentials and past rollouts included, readable to
  the process and so to its tools**; nothing secret may be placed there.
- Read `.CFUserTextEncoding`: macOS CoreFoundation reads it at start.
- Write `.codex/tmp`: Codex's argv[0] aliases; without it the in-process
  app server fails to start.
- Write `.codex/sessions`: rollouts, needed for resume.
- Write `.codex/thread-writer-locks`, `installation_id`, and
  `models_cache.json`: startup writes; the lock and id are required.
- Write the `state_N` and `logs_N` SQLite files: required, or session
  creation fails with a thread-store error.
- Read and write `<TESTDIR>/tmp`: the child's scratch dir.

Left denied without harm: goals and memories databases, shell snapshots,
and Keychain metadata. Not tested: `codex exec resume` under the wrapper,
sign-in token refresh (would need a write to `.codex/auth.json`), and a
HOME-installed binary. `(allow default)` leaves everything outside HOME
open, so other secret roots need their own deny rules.

**Verdict.** The wrapper closes the content oracle and keeps web search
and sign-in working, with path existence and the readable `~/.codex` as
residual leaks. It is backlogged, not shipped: an OS sandbox around
research children would re-enable Codex research. Until then Codex
research stays off in the defaults.

## Grok research and news tools

| Check | Command | Verdict | Evidence |
| --- | --- | --- | --- |
| C1, design flags as written | `grok -p "<prompt>" --tools web_search,x_keyword_search,x_semantic_search --disallowed-tools "read_file,grep,list_dir,write,search_replace,run_terminal_cmd,web_fetch,Agent" --output-format json` | **Not clean** | stderr: `warning: --tools "x_keyword_search", "x_semantic_search" name no tool, so other tools stay available`. The allowlist was ignored, and image, video, workflow, and the MCP meta-tools `search_tool` and `use_tool` stayed reachable. No tool by either X name exists in grok-build's source or docs. |
| C2, cleaned | `grok -p "<prompt>" --tools web_search --disallowed-tools "read_file,grep,list_dir,write,search_replace,run_terminal_cmd,web_fetch,Agent,search_tool,use_tool,image_gen,image_edit,image_to_video,reference_to_video,send_feedback,monitor,workflow,todo_write,ask_user_question,enter_plan_mode,exit_plan_mode,open_page,open_page_with_find" --no-subagents --output-format json` | **Clean, as the model reported it** | The model reported only `web_search`, `open_page`, `open_page_with_find` (a one-turn self-report, not a wire dump). stderr: `warning: --disallowed-tools "open_page", "open_page_with_find" name no tool, so they remove nothing`: the two `open_page*` tools are browse helpers that come with web search and cannot be named in `--disallowed-tools` (`--disable-web-search` removes all three). The flag set aggy's adapter will use, which drops those two names, has not been run live yet. |
| C3, review argv (2026-10-28, grok 1.0.70) | `grok -p "<prompt>" --tools read_file,grep,list_dir --disallowed-tools "write,search_replace,run_terminal_cmd,web_fetch,Agent,search_tool,use_tool,image_gen,image_edit,image_to_video,reference_to_video,send_feedback,monitor,workflow,todo_write,ask_user_question,enter_plan_mode,exit_plan_mode,web_search" --no-subagents --permission-mode plan --output-format json` | **Clean, as the model reported it** | The model reported only `read_file`, `grep`, and `list_dir` (a one-turn self-report, not a wire dump). `open_page` and `open_page_with_find` did not survive: denying `web_search` takes them along. No `name no tool` warning; stderr was empty. |

Lessons: an unknown name in `--tools` makes Grok ignore the whole
allowlist, with only a warning; and `search_tool` and `use_tool` must be
denied by name to cut off MCP servers.

## Grok headless prompt, version, and sandbox (T6740)

Live diagnostics, 2026-10-28, grok 1.0.70, after the smoke run below
found every Grok borrow failing. Minimal runs in a scratch directory.

| Check | Command | Verdict | Evidence |
| --- | --- | --- | --- |
| G1, aggy's old argv | `grok -p --output-format json ... < prompt.md` | **Fails** | `error: a value is required for '--single <PROMPT>' but none was supplied`. `-p` is clap's `--single <PROMPT>` and needs a value; stdin is not read. |
| G2, prompt last | `grok -p --output-format json ... "<prompt>"` | **Fails** | `the argument '--single <PROMPT>' cannot be used with '[PROMPT]'`. |
| G3, prompt file | `grok --prompt-file <abs path> --output-format json --tools read_file,grep,list_dir --disallowed-tools <review deny list> --no-subagents --permission-mode plan` | **Works** | Exit 0; the model reported only `read_file`, `list_dir`, and `grep`. aggy's argv since T6740. |
| G4, inline | `grok -p "<text>" --output-format json --max-turns 1` | Works | Puts the prompt in argv, so aggy does not use it. |
| G6, JSON reply | G3 | Recorded | `--output-format json` prints one pretty-printed object (29 lines in the sample), keys `text`, `stopReason` (`end_turn`), `sessionId`, `requestId`, `cwd`, `thought`, `usage` {`input_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `output_tokens`, `reasoning_tokens`, `total_tokens`}, `num_turns`, `total_cost_usd`, `total_cost_usd_ticks`, `modelUsage` {per model}. The answer is `text`; `thought` is the model's reasoning. aggy's parser before T6740's fix read single lines only and failed every successful borrow with "unrecognized headless output" (7c86f6c re-run below). |
| G5, version | `grok --version` | Recorded | Prints exactly `grok 1.0.70 (f50904eaee6c) [stable]`. aggy's parser before T6740 did not allow the `[stable]` tag, read no version, and refused every Grok borrow as untested. |
| S1, read confinement | G3 plus a prompt asking to read `./README.md`, then a canary file outside the cwd by absolute path, with `--permission-mode plan`; `--permission-mode dontAsk --allow 'Read(./**)'`; `--permission-mode plan --allow 'Read(./**)'` | **Not confined** | All three exited 0 and returned the canary: Grok auto-allows reads in every mode, so allow rules do not confine `read_file`. |
| S2, strict sandbox | the same with `--permission-mode plan --sandbox strict` | **Confined under `$HOME`** | README was read; the canary under `$HOME` was refused, `Permission denied: <HOME>/.../canary.txt`. |
| S3, `~/Library` (G32-3) | S2 with the canary at `<HOME>/Library/Caches/aggy-canary.*/canary.txt` | **Not confined** | `read_file` returned the canary: strict allows `~/Library`. |
| S5, firmlink (G32-9) | S4, with `read_file /System/Volumes/Data<HOME>/Library/Caches/.../c.txt` | **Not confined**, then **Confined** with aggy's firmlink deny | Before the deny: returned the canary, since Grok matches deny rules on the lexical path and a firmlink is not a symlink. The same spelling of a home folder outside `~/Library` was refused by Seatbelt (`Permission denied`). Re-check, live-verified (2026-10-28, grok 1.0.70): with `--deny "Read(/System/Volumes/Data/**)"` and the matching `Grep` rule, both `read_file` and `grep` on `/System/Volumes/Data<HOME>/Library/...` were refused (`Denied by permission policy`), and the canary did not leak. aggy's darwin argv since G32-9. |
| S6, nested sandbox (G32-8) | the review argv with `--sandbox strict`, run inside `sandbox-exec -p '(version 1)(allow default)'` | **Refuses to start** | Exit 1; stderr `sandbox initialization failed: Operation not permitted` / `error: could not apply the 'strict' sandbox profile; see the warning above for the cause. Refusing to start with its protections missing.` So a profile that cannot be applied fails closed here; aggy maps it to `ErrLockdownRejected`. |
| S7, sandbox logging | a successful sandboxed run with `RUST_LOG=xai_grok_sandbox=info` | Recorded | No sandbox line on stderr: headless tracing does not reach stderr, so the source's "continuing without sandbox" warnings are a weak guard. aggy does not set `RUST_LOG`. |
| S4, `~/Library` denied | S3 plus `--deny "Read(<HOME>/Library/**)" --deny "Grep(<HOME>/Library/**)"`, `$HOME` expanded | **Confined** | `read_file` was refused, `Denied by permission policy: deny rule on read matching "~/Library/**"`; the in-repo README still read. aggy's argv since G32-3. |
| S8, Grok home denied (T6742) | every sandboxed kind, and image and video, adds `--deny "Read(<Grok home>/**)" --deny "Grep(<Grok home>/**)"`: `$GROK_HOME`, else `<HOME>/.grok`, and `<HOME>/.grok` as well when `GROK_HOME` points elsewhere | **Grok works with it; the deny itself is inferred** | Strict leaves the Grok home readable, and it holds `auth.json`. Live smoke (2026-10-28, grok 1.0.70, 807f323): Grok starts and works with the deny for review (`grok-review-untrusted`, `aggy-grok-review-refused`) and research (`research-borrow-grok`, canary 0). Whether it refuses a read is not checked directly: no canary is planted in the real `~/.grok`. It is the same mechanism as S4 and S5, which are live-verified, so it is inferred to refuse `read_file` and `grep` there. On darwin, the `/System/Volumes/Data/**` deny (S5) already covers the firmlink spelling of the Grok home. Read and Grep only: Grok's media tools write there in their own code, and `image_edit` and `image_to_video`, which take their input still from the session folder there, are checked as `AccessKind::Tool`, which Read and Grep rules do not match (grok-src 559751fd, `xai-grok-tools/src/permission_access.rs:38-41`, `xai-grok-permission-rules/src/policy.rs:571`). So the deny does not get in their way, and it does not confine them either. |
| S9, other spellings (N54-1, N54-3) | the denies of S4, S5 and S8, plus: `Read`/`Grep` of `~*` and `~*/**` (every `~` path); of `**/[<quotes and white space>]*` and `**/[...]*/**` (every path with a part that starts with a quote or Unicode white space); on darwin a case-folded copy of each dir rule (`/[Uu][Ss]...`); and the physical form of `~/Library` and the Grok home when a symlink makes it differ | **Source only; the owner's live canary check is pending** | From grok-src 559751fd. Grok's gate matches a `~` path literally and unnormalized, and the tool expands `~` after it (`xai-grok-permission-rules/src/policy/path_match.rs:91-112`, test `policy.rs:2221`). So `~/.grok/x`, `~//.grok/x` and `~/a/../.grok/x` would match no absolute rule, and a `~/.grok/**` rule would still miss the last two. Hence every `~` path is denied. Under strict nothing else in `$HOME` is readable anyway. A quote- or space-wrapped path is relative to the gate, which joins it to the cwd, and the tool strips the quotes and `str::trim` white space afterwards (`xai-grok-tools/src/types/resources.rs:404-445`). So `"~/.grok/auth.json"` or `" ../.grok/x"` match no path rule, and only a rule on the leading character catches them. Grok's glob match is case-sensitive (`policy.rs:825-835`) and APFS is not, hence the folded copies. Grok re-checks a symlink it follows (`policy.rs:222-263`), but a path already in its physical form has nothing to follow, hence the physical rules. Not covered: other Unicode spellings of a non-ASCII home path (APFS also ignores normalization). Nothing here was run live. |

The strict profile (grok-src `crates/codegen/xai-grok-sandbox/src/profiles.rs:463-488`,
kernel-enforced) allows reads of system dirs (`/usr`, `/etc`, `/tmp`,
`/private`, `/var`, `/System`, `/Library`, ...), `~/Library`, `~/.grok`,
and the workspace; writes only to the workspace, Grok's sessions dir,
and the temp dirs (`xai-grok-sandbox/src/paths.rs:74-78`,
`essential_writable_paths_strict`); and blocks child
networking. Gaps: files under `/tmp` and `/private` (`$TMPDIR`, aggy's
run dirs) stay readable, `~/.grok` (Grok's own home) is readable
(aggy's deny rules close it for `read_file` and `grep`, S8),
`~/Library` is readable (S3; aggy's deny rules close it for `read_file`
and `grep`, S4), and only vendor homes and other paths under `$HOME` are
denied, and the `~/Library` deny is lexical, so on darwin the
`/System/Volumes/Data` firmlink is denied too (S5), as are `~`, quoted,
case-varied and physical spellings (S9, pending a canary check). Those
two dirs are guarded by permission rules only. When the profile
cannot be applied, grok 1.0.70 refused to start (S6); the source also has
paths, `xai-grok-sandbox/src/lib.rs:206-216` and `:236-247`, that log
"continuing without sandbox" and return Ok when the platform has none
(source only, 2bdd1d6a), and those logs do not reach stderr by default
(S7). aggy passes `--sandbox strict` on review, research, and news,
refuses them when the probed `--help` does not list `--sandbox` or the
host has no kernel sandbox (macOS, or Linux with `landlock` in
`/sys/kernel/security/lsm`), and discards a run whose stderr shows the
refusal or, a weak extra check, one of those log lines.

Status of each kind under strict, and of image and video, which run without it:

| Kind | Status |
| --- | --- |
| review | Live-verified: S2, S4, S5; `grok-review-untrusted` passed under strict (b972adf re-run below), and again with the Grok home deny (S8, 807f323). The S9 spelling rules are not yet run live. |
| research | Live-verified: `research-borrow-grok` passed under strict plus the denies (b972adf), and again with the Grok home deny (807f323). |
| news | **Not yet run live** under strict. Same argv as research. |
| image | **Live FAIL under strict** (smoke `image-borrow-grok`, 2026-10-28, grok 1.0.70, at 25b89ee and again at 807f323): `aggy image` exited 1 after about 5 s, "the child left no file that passed the artifact gate"; the child only said it would generate the image (about 3.3K tokens). The same check passed on main without strict. Cause not determined (TODO(T6751)). From source the output write should be allowed: `image_gen` saves to `<Grok home>/sessions/<encoded cwd>/<id>/images/<n>.jpg` (grok-src `559751fd`, `xai-grok-tools/src/implementations/grok_build/image_gen/mod.rs:13`, `storage.rs:54-69`), which strict keeps writable (`paths.rs:74-78`; `profiles.rs:463-488`). So `strictMedia` in `internal/adapter/grok/grok.go` is off: image runs without `--sandbox strict` and without the sandbox host check, as on main, but keeps the read denies (S4, S5, S8, S9), which are permission rules, not part of the sandbox. Its tools (`image_gen`, `image_edit`) are checked as `AccessKind::Tool`, so those denies match nothing of image's today. For the same reason, a HOME or Grok home that cannot be named in a rule drops the denies for image and video instead of refusing them (N54-4). |
| video | **Untested**: the test account has zero data retention, under which Grok's video tools are unavailable (b972adf re-run below); turning ZDR off, or a user-hosted bucket, is an owner item. Runs like image: no strict while `strictMedia` is off, read denies kept. |

## Smoke run, 2026-10-28 (main 2383c13)

`AGGY_SMOKE=1 test/smoke/smoke.sh`, live, with claude 2.1.312, codex-cli
0.164.0, and grok 1.0.70. Runs used: claude 2, codex 4, grok 8. Every
Grok row failed on 2383c13 for the two causes above (G1 and G5); two
refusal checks reported "not refused" only because aggy had already
stopped at the version check, with exit 1. The smoke script now passes
the prompt with `--prompt-file`, requires each refusal's own reason, and
prints a scrubbed stderr tail on a FAIL.

| Check | Result on 2383c13 | Status |
| --- | --- | --- |
| `aggy-setup` | PASS | |
| `claude-append-prompt` | PASS | |
| `codex-hook-trust-gate` | PASS (the `-c` hook was skipped) | |
| `codex-research-isolation` | PASS (canary 0 times) | |
| `mcp-launch-claude` | PASS | |
| `mcp-launch-codex` | FAIL, exit 143 (the 300 s timeout) | intermittent: passed on 7c86f6c, cause unknown (below) |
| `mcp-launch-grok` | FAIL, exit 1 | the agent file's `mcpServers` layout (below) |
| `grok-review-untrusted` | FAIL, exit 2 (G1) | failed on 2383c13, fixed by T6740, re-run pending |
| `aggy-grok-review-refused` | FAIL, not refused (exit 1: the version refusal, G5) | failed on 2383c13, fixed by T6740, re-run pending |
| `grok-image-tool-list` | FAIL, exit 2 (G1) | failed on 2383c13, fixed by T6740, re-run pending |
| `aggy-codex-research-refused` | PASS | |
| `image-borrow-codex` | PASS | |
| `image-borrow-grok` | FAIL, exit 1 (G5) | failed on 2383c13, fixed by T6740, re-run pending |
| `research-borrow-grok` | FAIL, exit 1 (G5) | failed on 2383c13, fixed by T6740, re-run pending |
| `aggy-video-default-refused` | FAIL, not refused (exit 1: the version refusal, G5) | failed on 2383c13, fixed by T6740, re-run pending |
| `video-borrow-grok` | FAIL, exit 1 (G5) | failed on 2383c13, fixed by T6740, re-run pending |

### Re-run on 7c86f6c (T6740, 2026-10-28)

`AGGY_SMOKE_KEEP=1`, live. Grok alone (`AGGY_SMOKE_VENDORS=grok`, 7 grok
runs), then Codex alone (4 codex runs).

| Check | Result on 7c86f6c | Cause, and fix |
| --- | --- | --- |
| `aggy-setup` | PASS | the version now parses |
| `aggy-grok-review-refused` | PASS | |
| `grok-image-tool-list` | PASS | |
| `aggy-video-default-refused` | PASS | |
| `image-borrow-grok`, `research-borrow-grok`, `video-borrow-grok` | FAIL: "grok: unrecognized headless output" (the CLI exited 0) | the multi-line JSON reply (G6); `ParseHeadless` now decodes all of stdout. Re-run pending. |
| `mcp-launch-grok` | FAIL, exit 1 | stderr: `failed to load agent profile '<scratch>/agent.md': failed to parse agent definition: its frontmatter is not a valid definition at line 5, column 3`. The smoke agent file gave `mcpServers` as a map; Grok wants a YAML list whose entries are a name, a one-key `{name: config}` map, or an object with `name` (grok-src `crates/codegen/xai-grok-agent/src/config.rs:1121-1163`, `McpServerRef`; test `crates/codegen/xai-grok-shell/src/session/agent_mcp_tests.rs:429`). A stdio entry is `{command, args}` with no `type`: `McpServerTransportConfig` is untagged (`crates/codegen/xai-grok-config/src/mcp_server_config.rs:27-41`). The file is now `- aggy: {command: python3, args: [...]}`, and `--max-turns 3` is gone (MCP tools are deferred). Re-run pending. |
| `grok-review-untrusted` | INCONCLUSIVE: no hook ran, none fired in the control either | `--sandbox strict` allows writes only to Grok's sessions dir, so the control's hook marker writes failed. The control now runs without `--sandbox`; the untrusted run keeps aggy's exact argv. Re-run pending. |
| Codex: all four checks, including `mcp-launch-codex` | PASS ("the server's token came back") | The 2383c13 timeout did not recur: intermittent, seen once, cause unknown. No code change. |

### Re-run on b972adf (T6740, 2026-10-28)

Live, Grok alone (7 grok runs), with the review argv's `--sandbox strict`
and `~/Library` denies.

| Check | Result |
| --- | --- |
| `aggy-setup` | PASS |
| `mcp-launch-grok` | PASS (the list-form agent file loads) |
| `grok-review-untrusted` | PASS: no hook ran untrusted; the control fired `claude-SessionStart`, `claude-PreToolUse`, `grok-SessionStart`, `grok-PreToolUse` |
| `aggy-grok-review-refused` | PASS |
| `grok-image-tool-list` | PASS |
| `image-borrow-grok` | PASS |
| `research-borrow-grok` | PASS: report back, canary 0 times, under strict plus the deny |
| `aggy-video-default-refused` | PASS |
| `video-borrow-grok` | FAIL, environment: the account has zero data retention, and Grok's `image_to_video` reported "Video generation tools are unavailable under zero data retention (ZDR)"; aggy's artifact gate correctly found no file. Testing Grok video needs ZDR off or a user-hosted bucket (owner item). |

## Grok project config and folder trust (source only)

From grok-build source at `2bdd1d6`, not run live. Grok loads project
hooks and other code-running config whenever folder trust resolves
Trusted:

- Project hook sources at the git root: `.claude/settings.json`,
  `.claude/settings.local.json`, `.grok/hooks`, `.cursor/hooks.json`
  (`xai-grok-hooks/src/discovery.rs:385-401`); they load only when the
  trust allows project sources (`:299-314`).
- `decide` is Trusted when folder trust is off (the feature flag, set by
  `GROK_FOLDER_TRUST`, `[folder_trust] enabled`, managed or remote
  config), when the folder is durably trusted (an earlier interactive
  grant or `--trust`), when the key cannot be recorded (`$HOME`, `/`),
  or when no repo config is present
  (`xai-grok-permission-rules/src/folder_trust.rs:58-77`). A headless run
  is Untrusted only when none of these holds.
- The repo config folder trust gates
  (`xai-grok-workspace/src/folder_trust.rs:433-551`): `.mcp.json` and
  `.grok/config.toml` from the cwd up to the git root; Claude settings on
  the same walk (`xai-grok-permission-rules/src/claude_settings.rs:354-384`);
  `.grok/plugins`, `.claude/plugins`, `.grok/agents`, `.claude/agents`
  on that walk; `.grok/lsp.json`, `.cursor/mcp.json`, `.envrc`,
  `.grok/roles`, `.grok/personas` in the cwd; `.grok/hooks`,
  `.cursor/hooks.json`, `.grok/workflows` at the git root; project
  instructions and skills; and `~/.claude.json` project MCP servers.
- `.envrc` runs through `direnv export json`, or, without direnv, in bash
  with direnv stubs (`xai-grok-workspace/src/envrc.rs:147-188`).

So a repo the user once trusted in interactive Grok, on a hostile
branch, would run that branch's hooks in a Grok review. aggy does not
rely on folder trust: a Grok review is refused, and Grok passed over,
when the review root holds any of the code-running files or dirs above
(`internal/borrow/grok_trust.go`).

Project instructions and skills no longer refuse it (C17-10, relaxed by
T6742). They run no code, but Grok's `read_file` keeps an absolute or
`~` path as is, with no check that it is under the cwd
(`xai-grok-tools/src/types/resources.rs:404-430`, `resolve_model_path`;
the tool's own description says an absolute path "will be preserved as
is", `implementations/grok_build/read_file/mod.rs:135`), so a hostile
repo's instructions could once have had a reviewer read and echo a file
outside the repo. A Grok review now runs under `--sandbox strict` with
the `~/Library` and `/System/Volumes/Data` denies, which kept
`read_file` and `grep` out of `$HOME` whoever asked (S2, S4, S5 above,
all live), the Grok home deny (S8: Grok works with it live; the
refusal itself is inferred), and the other spellings (S9, pending).
What strict still leaves readable (system dirs, the temp dirs) a hostile
diff, which is attacker text in every review, could ask for just the
same, so a gate on instruction files never closed it. aggy still keeps an edit diff off them
(`borrow.TrustMarker`).

One case stays refused (N54-2). Grok loads instruction files with no
permission check and follows symlinks to do it
(`xai-grok-agent/src/prompt/agents_md.rs:46-79`, grok-src 559751fd). So
a committed `AGENTS.md -> ../../.grok/auth.json` would put the token in
the prompt: the sandbox allows that read, and the deny rules never see
it. Source only, not run live. A Grok review is refused, and Grok
passed over, when any instruction, rules, or skill path below, in a dir
from the cwd to the repo root:
- is a symlink, or has one on its way down (a linked `.claude`);
- holds one anywhere inside a rules or skill dir;
- is neither a regular file nor a dir, or cannot be checked
  (`grokInstructionLinks`).
Plain files pass. Not covered: Grok also reads `.cursor/rules` under any
dir above a file it reads, anywhere in the repo, and follows a linked
`.cursor` or `rules` dir there, though not a linked rule file
(`xai-grok-tools/src/implementations/cursor_rules_on_read.rs:191-272`).
Only `.md` and `.mdc` files load that way. Those are not scanned for
across the repo. The instruction names are `INSTRUCTION_FILENAMES`
and `.claude/CLAUDE*.md` (`xai-grok-config/src/compat.rs:311-318`,
`:431-438`) and the `.grok`, `.claude`, and `.cursor` `rules` dirs
(`:418-427`); skills are `skills/` or `commands/` under `.grok`,
`.agents`, `.claude`, or `.cursor` (`compat.rs:405-414`,
`skills/discovery.rs:26-27`). Grok 1.0.70's folder trust held in one
live check (`grok-review-untrusted`, b972adf re-run above: no hook ran
untrusted), but aggy still does not rely on it.

Grok's CLI parses its flags with clap (`xai-grok-pager/src/app/cli.rs:2`),
so its rejection lines have clap's form.

## Extra context at session start (T6619)

- **Claude.** `--append-system-prompt-file <path>` is documented for
  interactive and print modes (`cli-reference.md`) but missing from the
  2.1.312 `--help`. Live: `claude -p --append-system-prompt-file ../extra.txt --output-format json "What is the codeword?"`
  answered with the codeword from the file, `is_error:false`. **Works.**
  The docs ("System prompt flags in resumed conversations") say the
  prompt is snapshotted on the first request: a changed value is ignored
  on `--resume` until compaction, unless `--system-prompt-snapshot off`.
- **Codex.** `-c developer_instructions="<text>"` adds a developer
  message; also `instructions` and `model_instructions_file` (which
  replaces the built-in prompt). `experimental_instructions_file` does
  not exist in 0.164.0. `-c` works on `codex`, `codex resume`,
  `codex exec`, and `codex exec resume` (`config/src/config_toml.rs#L250`,
  `#L254`, `#L275`). Whether a resumed thread takes new instructions is
  unknown. AGENTS.md discovery: `AGENTS.override.md`, then `AGENTS.md`,
  then `project_doc_fallback_filenames` (a TOML array of bare file
  names, `config_toml.rs#L340`), walked from the project root to cwd.
- **Grok.** `--rules "<text>"` (alias `--append-system-prompt`) appends
  inline text only; there is no file variant. Behavior on `--resume` is
  unknown. Grok reads `AGENTS.md`, `CLAUDE.md`, and `.grok/rules/*.md`
  (also `.claude/rules/`) from the repo root to cwd.

## Safety policy rendering (T6678)

Docs and source only, not run live (2026-10-28). `aggy run` and
`aggy policy wire` rely on these:

- **Claude.** `--settings <file-or-json>` takes inline JSON and applies
  for the session only, above the user, project, and local files; list
  keys such as `permissions.deny` merge across layers instead of
  replacing (`settings.md`, "Change a setting for one session", "Lists
  merge instead of overriding"). A deny wins over any allow; `Read`
  rules use gitignore patterns, so `Read(**/.env)` matches at any depth
  under the working directory and `Read(//**/.env)`, anchored at the
  file-system root, matches a `.env` anywhere; a `*` in a Bash rule matches any text,
  and deny rules apply to every subcommand of a compound command but not
  to the same program written another way (`permissions.md`). Doc-verified.
- **Grok.** `--deny RULE` (alias `--disallowedTools`) with
  `value_delimiter = ','` and `--allow` (`crates/codegen/xai-grok-pager/src/app/cli.rs`
  at `2bdd1d6a`). Rules from flags and every config source merge, `deny`
  beats `ask` beats `allow`; `Read(**/.env)` matches anywhere, and there
  is no `//` anchor (a leading `//` is literal text); a Bash
  pattern matches as a prefix or a whole-command glob, against every
  segment of a chained command; Read deny rules also cover file paths
  that shell commands such as `cat` name
  (`user-guide/22-permissions-and-safety.md`). Source/Doc-verified;
  `--allow` was live-confirmed with an MCP rule (T6734), `--deny` was not
  run.
- **Codex.** Every regular `*.rules` file in `<config folder>/rules` of
  each config layer is loaded, the user layer's folder being
  `$CODEX_HOME` (`core/src/exec_policy.rs`, `load_exec_policy`,
  `collect_policy_files`). `prefix_rule(pattern, decision,
  justification)` with `decision = "forbidden"`; a list in a pattern is
  a set of alternatives; the strictest decision of all matching rules
  wins (`execpolicy/README.md`, `execpolicy/src/parser.rs`). A file that
  fails to parse makes Codex fall back to no user rules at all
  (`load_exec_policy_with_warning`). `-s`/`-a` replace the configured
  sandbox and approval policy. Source-verified at `6aaac69`.

## Launch-time MCP (T6734)

Live (2026-10-27), with a 25-line stdlib stdio MCP server exposing
`aggy_ping`:

| Check | Command | Verdict | Evidence |
| --- | --- | --- | --- |
| Claude fresh | `claude -p --mcp-config ../mcp.json --strict-mcp-config --allowedTools mcp__aggy__aggy_ping --output-format json "Call the aggy_ping tool..."` | Confirmed | Returned `<token>`. |
| Claude resume, flags re-passed | `claude -p --resume <id> --mcp-config ../mcp.json --strict-mcp-config ...` | Confirmed | Same session id, same `<token>`. |
| Claude resume, no `--mcp-config` | `claude -p --resume <id> --strict-mcp-config ...` | Confirmed: tool gone | "The `aggy` MCP server has been removed from this session's configuration". **Re-pass `--mcp-config` on every resume.** |
| Codex, default approval | `codex exec --json --skip-git-repo-check --ignore-user-config -c 'mcp_servers.aggy.command="python3"' -c 'mcp_servers.aggy.args=["<scratch>/server.py"]' --sandbox read-only -- - < p.txt` | Key path confirmed; call blocked | "MCP tool call requires approval, but approval policy is never". |
| Codex, per-server approval | same plus `-c 'mcp_servers.aggy.default_tools_approval_mode="approve"'` | Confirmed | Returned `<token>`. Values: `auto`, `prompt`, `writes`, `approve`. A per-server allow, not a global bypass. |
| Codex resume with sandbox | `codex exec --json ... -c ... --sandbox workspace-write resume <id> -- - < p2.txt` | Confirmed | Same thread id; MCP call worked; a write succeeded on a thread started read-only, so `--sandbox` before `resume` applies. |
| Grok via agent file | `grok -p "Call aggy_ping..." --agent ../agent.md --allow 'MCPTool(aggy__aggy_ping)' --output-format json`, with `mcpServers` in the agent file's frontmatter | Confirmed | Returned `<token>`. Grok has no per-session MCP flag. Resume with the same agent file was not tested. The frontmatter `mcpServers` is a YAML list (see "Smoke run" below): a map there fails to load. |

Also (Doc/Source-verified): Claude subagents can declare inline
`mcpServers` that connect only while they run; Grok user agents can too,
with `mcpInheritance`; Codex roles cannot carry MCP servers and children
inherit the parent's. Claude and Grok defer MCP tool definitions (names
only, or `search_tool`/`use_tool`); Codex defers only when the model
supports tool search.

A Claude run with `--strict-mcp-config --mcp-config` on
`{"mcpServers":{}}` had no MCP or claude.ai connector tools at all
(2026-10-28); built-in file tools stay unless `--tools` trims them.

## MCP server config (T6679)

Docs and source only, not run live (2026-10-28). `aggy mcp wire` and
the brief's missing-tools list rely on these:

- **Claude.** User-scope servers are kept in `~/.claude.json`, local-scope
  ones in the same file under the project's path (`mcp.md`, "MCP
  installation scopes"). An entry has `type` (stdio when absent; `http`,
  its alias `streamable-http`, `sse`, `ws`, `sdk`), `command`, `args`,
  `env`, `url`, `headers`, `headersHelper`, and `oauth` (same page).
  Doc-verified. The key names (a top-level `mcpServers` object, and
  `projects.<path>.mcpServers`) are those Grok reads from that file
  (grok-build `2bdd1d6a`, `xai-grok-config/src/mcp_servers/json_config.rs:235-244`,
  `:399-402`). The page does not say whether `CLAUDE_CONFIG_DIR` moves
  the file: unverified, so aggy reads nothing when it is set.
- **Codex.** `mcp_servers` is a table of servers in `config.toml`
  (`config/src/config_toml.rs#L299`); an entry with `command` and `args`
  is stdio, one with `url` streamable HTTP, and there is no SSE
  transport (`config/src/mcp_types.rs#L373-L389` and its `TryFrom`);
  `enabled = false` skips a server, default true
  (`mcp_types.rs#L240-L242`). `codex mcp login <name>` signs in to a
  server with OAuth (`cli/src/mcp_cmd.rs#L62`, `#L79`; also in
  `codex mcp --help`). Source-verified at `6aaac69`.
- **Grok.** `[mcp_servers.<name>]` in `~/.grok/config.toml` with
  `command`/`args`/`env`, or `url`/`headers`, and `enabled` (default
  true)
  (`xai-grok-pager/docs/user-guide/07-mcp-servers.md`); `type = "sse"`,
  or a `url` ending in `/sse`, is SSE
  (`xai-grok-config/src/mcp_server_config.rs:28-59`, `:499-502`). Grok
  also loads `~/.claude.json`'s servers unless `[compat.claude] mcps =
  false` or `GROK_CLAUDE_MCPS_ENABLED` turns that off, and `config.toml`
  wins on a name clash (same guide, "Compatibility"). How Grok parses the
  variable's value is unverified; when listing Grok's servers aggy
  counts any value but `1` or `true` as off, and for `aggy doctor`'s
  warning any value but `0` or `false` as on. Source/Doc-verified at
  `2bdd1d6a`.

## Codex edit lockdown (T6687, W-1)

Live (2026-10-28, codex-cli 0.164.0), in a scratch git repo with `env -i`
and only `HOME` and `PATH` set:

| Check | Command | Verdict | Evidence |
| --- | --- | --- | --- |
| Edit lockdown | `codex exec --strict-config --ignore-user-config --sandbox workspace-write --disable view_image --disable apps --disable plugins --disable goals --disable image_generation --disable sleep_tool "<prompt>"` | Confirmed | Exit 0. The shell tool created `./inside.txt` holding "OK" in the cwd. A write to `$HOME/<scratch canary dir>/out.txt`, outside the cwd, failed with `operation not permitted`, and the file was not created. |

Not checked: whether exec's default web search is on under this set, and
whether Codex resolves a linked worktree to its main repo for project
trust (with `--ignore-user-config` no trust entry loads). `codex.EditLockdown` is this set.

Temp dirs (K46-2), live 2026-10-28, codex-cli 0.164.0, scratch repo, `TMPDIR`
set to a scratch dir:

| Check | Command | Verdict | Evidence |
| --- | --- | --- | --- |
| Run A: temp dirs under workspace-write | the edit lockdown above, unchanged | Writable | The shell tool wrote a file in `/private/tmp/<scratch>`, in `$TMPDIR`, and in the repo, each with exit 0. |
| Run B: exclude keys | the same plus `-c sandbox_workspace_write.exclude_slash_tmp=true -c sandbox_workspace_write.exclude_tmpdir_env_var=true` | Confirmed | `--strict-config` accepted both keys. The `/private/tmp` and `$TMPDIR` writes failed with `operation not permitted`; the repo write succeeded. |

The key names are fields of the binary's `SandboxWorkspaceWrite` config
struct (`writable_roots`, `network_access`, `exclude_tmpdir_env_var`,
`exclude_slash_tmp`). `codex.EditLockdown` passes both exclude keys. aggy's
check gate still refuses a main checkout under `/tmp`, `/private/tmp`, or
`$TMPDIR`, and accept keeps its patch file in its own `run` dir, as defense
in depth for other temp roots and older Codex versions.

## Codex CLI details

- `codex exec resume [SESSION_ID] [PROMPT]` with `--last` and `--all`
  (Help-verified). With `--last`, a lone positional is the prompt
  (`exec/src/cli.rs#L256`). It has no `-s/--sandbox`, `-C`, `--add-dir`,
  or `-p`; flags such as `--sandbox` go before `resume` (Live-confirmed
  above).
- `codex resume` hides exec sessions from the picker and `--last` unless
  `--include-non-interactive`.
- `--ignore-user-config` ("Do not load `$CODEX_HOME/config.toml`; auth
  still uses `CODEX_HOME`") exists on `exec`, `exec resume`, and
  `exec fork`, not on the root command (`config/src/loader/mod.rs#L203`).
- `-i/--image` on root `exec` takes several values
  (`utils/cli/src/shared_options.rs#L12`), so `codex exec -i a.png -` may
  take `-` as a second image (Inferred, not run). Live: `codex exec -i <scratch>/red.png --json --skip-git-repo-check --sandbox read-only -- - < p.txt`
  attached the image and read the prompt from stdin. Use `--`.

## API and route settings

Source and docs only; adapters.md lists each setting and its source. In
short: Codex reads `openai_base_url` and `[model_providers.<id>]` from
config, not `OPENAI_BASE_URL`, and `CODEX_API_KEY` takes precedence in
`codex exec` (API billing). Claude honors `ANTHROPIC_BASE_URL`,
`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `apiKeyHelper`, Bedrock and
Vertex switches, and managed settings. Grok's media tools use
`endpoints.xai_api_base_url` (`GROK_XAI_API_BASE_URL`);
`toolset.image_gen` and `toolset.video_gen` keys do not exist;
`GROK_MODELS_BASE_URL` and `[skills] ignore` are real.

Grok hooks: `~/.grok/hooks/*.json` and `<project>/.grok/hooks/*.json`,
Claude format, with `sessionId` in the payload and `GROK_SESSION_ID`,
`GROK_HOOK_EVENT` in the environment. **Grok also runs the hooks in
Claude's `settings.json` by default** (`[compat.claude] hooks = false` or
`GROK_CLAUDE_HOOKS_ENABLED=0` turns that off), so a hook aggy installs
for Claude fires under Grok too (`docs.x.ai/build/features/hooks.md`;
`user-guide/10-hooks.md`).

## Hook payloads and config (M2 hooks)

Docs and source only, not run live (2026-10-28). `aggy hook` and
`aggy hooks install` rely on these; `internal/hook/event.go` keeps only
the fields named here.

- **Claude** (`code.claude.com/docs/en/hooks.md`). Config: `"hooks"` maps
  an event to matcher groups `{"matcher": ..., "hooks": [{"type":
  "command", "command": ..., "timeout": <seconds>}]}`; `matcher` "`*`,
  `""`, or omitted" matches all, and `Stop` has no matcher support. A
  command without `args` runs with `sh -c`; with `args` it is spawned
  without a shell (not used: when `args` arrived is not stated, so a
  2.1.312 that ignored it would run bare `aggy`). Common input:
  `session_id`, `transcript_path`, `cwd`, `permission_mode`,
  `hook_event_name`. `SessionStart` adds `source`
  (`startup|resume|clear|compact|fork`) and an optional `model`; its
  plain stdout on exit 0 is added to the model's context. `Stop` adds
  `stop_hook_active` and `last_assistant_message`. `StopFailure` "runs
  instead of Stop when the turn ends due to an API error" with `error`
  (`rate_limit`, `overloaded`, `authentication_failed`, ..., `unknown`),
  optional `error_details` and `last_assistant_message`; its output and
  exit code are ignored. `Notification` adds `message`, `title`, and
  `notification_type`. A command hook that exits with anything
  but 0 or 2, including one that cannot start (the shell's 127), is a
  non-blocking error: a "`<hook> hook error`" notice, and the action goes
  on ("Other exit codes"; `onFailure: "block"` changes that, and has no
  effect on `Stop`).
- **Codex** 0.164.0. `Stop` input, all required:
  `cwd`, `hook_event_name`, `last_assistant_message`, `model`,
  `permission_mode`, `session_id`, `stop_hook_active`, `transcript_path`,
  `turn_id` (`hooks/schema/generated/stop.command.input.schema.json`).
  Legacy `notify` (`hooks/src/legacy_notify.rs#L13`-`#L68`): the configured
  argv plus one final argument, `{"type":"agent-turn-complete",
  "thread-id", "turn-id", "cwd", "client"?, "input-messages",
  "last-assistant-message"}`; stdin, stdout, and stderr are null, and
  Codex does not wait for it. aggy does not install it: the prompts and
  reply in that argument would be visible in the process table.
  Hook commands (`hooks/src/engine/command_runner.rs`, `build_command`):
  `$SHELL -lc <command>` from the session environment, `/bin/sh -lc`
  when `SHELL` is unset, in a new session; a spawn failure is recorded as
  outcome `spawn_error` and a non-zero exit as `completed` with its exit
  code, neither stopping the session.
  Rollouts (`rollout/src/recorder.rs`, `precompute_new_rollout_path`):
  `sessions/<YYYY>/<MM>/<DD>/rollout-<timestamp>-<thread id>.jsonl`
  under the Codex home, **created lazily** at the first persist, not at
  session start.
- **Grok** (grok-build `2bdd1d6a`,
  `crates/codegen/xai-grok-pager/docs/user-guide/10-hooks.md`). Config:
  `~/.grok/hooks/*.json` (Claude format, a top-level `"hooks"` object);
  `command` is a path or "an inline shell command"; `timeout` in seconds
  (default 5, 600 for `Stop`). Events include `SessionStart`, `Stop`,
  `StopFailure` ("a turn ends because of an API error"), `StopCancelled`
  ("instead of `Stop` when a turn ends without completing"), and
  `Notification`. The envelope is camelCase: `hookEventName`,
  `sessionId`, `cwd`, `workspaceRoot`, `timestamp`, `permissionMode`;
  `StopFailure` adds `error` (`rate_limit`, ...) and `errorDetails`
  (clipped at 1000 characters); `StopCancelled` adds `reason`
  (`user_interrupt`, ...); `Notification` adds `notificationType`. Every
  hook gets `GROK_HOOK_EVENT`, `GROK_HOOK_NAME`, `GROK_SESSION_ID`,
  `GROK_WORKSPACE_ROOT`, and `CLAUDE_PROJECT_DIR`. Grok also reads the
  hooks in `~/.claude/settings.json` with the same camelCase envelope
  (plus `hook_event_name`), unless `[compat.<vendor>] hooks = false` or
  "the corresponding environment variable"; the doc at this revision
  does not name the variable, so `GROK_CLAUDE_HOOKS_ENABLED` (from
  `docs.x.ai`) is Doc-verified only there. Whether Grok's
  `SessionStart` payload has a source field is not stated: unverified,
  and aggy does not read one.

## Skill folders

Source and docs only, not run live (2026-10-28). `aggy skill link`
relies on these:

- Claude Code loads personal skills from `~/.claude/skills/<name>/SKILL.md`,
  follows a symlinked skill folder, and "loads the skill once even if
  several locations point at the same target"; only the description is
  loaded every turn, the body on invoke; invoked as `/<name>`
  (code.claude.com/docs/en/skills). Whether `CLAUDE_CONFIG_DIR` moves the
  folder is not stated: unverified, so aggy uses `~/.claude/skills`.
- Codex reads user skills from `$HOME/.agents/skills` (repo
  `.agents/skills`, `/etc/codex/skills`, and bundled ones too; not
  `~/.codex/skills`), follows symlinked skill folders, requires `name`
  and `description`, and is invoked with `$<name>`; two skills with one
  name are both kept (learn.chatgpt.com/docs/build-skills, formerly
  developers.openai.com/codex/skills).
- Grok scans `<grok home>/skills`, `~/.agents/skills` (always), and
  `~/.claude/skills` (unless `[compat.claude] skills = false`), plus
  project folders; it drops a skill whose canonical path it has already
  seen, then dedupes by name; `[skills] ignore` takes paths (with `~`
  expansion), canonicalizes them, and hides every skill whose canonical
  path starts with one; `disabled` takes names and keeps the skill
  listed (`crates/codegen/xai-grok-agent/src/prompt/skills.rs`:
  `collect_config_dirs`, `dedupe_skills`, `filter_skills`;
  `crates/codegen/xai-grok-pager/docs/user-guide/08-skills.md`, at
  commit `2bdd1d6a`). Invoked as `/<name>`.

## Grok media tools and local files (2026-10-28, source)

Source-verified against grok-build `2bdd1d6a` (see Method; possibly
newer than the tested grok 1.0.70). Paths below are under
`https://github.com/xai-org/grok-build/blob/2bdd1d6a6369de0e8c68132ea4539e9abd9e14a8/crates/codegen/xai-grok-tools/src/implementations/grok_build`. Not run live yet; `test/smoke/smoke.sh` runs the video path
(`video-borrow-grok`) and the image tool list (`grok-image-tool-list`).

| Tool | Takes a local path? | Evidence |
| --- | --- | --- |
| `image_gen` | No. Input is `prompt` and `aspect_ratio` only. | `image_gen/mod.rs#L366-L374` (`ImageGenInput`) |
| `image_edit` | Yes. Each `image` may be an attachment token, an absolute path, or a data URL; a path (or `file://` URI) is read with `tokio::fs::read`, with no check that it is under the cwd. Small PNG/JPEG pass as is; other bytes must decode as an image. | `image_edit/mod.rs#L113-L161` (`resolve_to_data_url`, read at `#L146`), `#L47-L75` (`compress_reference`), `#L241` (the parameter's description) |
| `image_to_video` | Yes. `image` is "an absolute filesystem path, HTTPS URL, or data URL"; a path is read with `tokio::fs::read` and must validate as an image. No cwd check. | `video_gen/mod.rs#L1010-L1020` (`ImageToVideoInput`), `#L880-L926` (`resolve_image_reference`, read at `#L909`, `validate_image_bytes` at `#L921`), `#L1223` |
| `reference_to_video` | Yes. `images` (up to 14), `first_frame`, `last_frame`, and `keyframes[].image` each take an absolute path, read the same way. | `video_gen/mod.rs#L1053-L1079`, `#L1376-L1389` |
| text-to-video | **None exists.** `reference_to_video` needs at least one of `images`, `voices`, `first_frame`, `last_frame`, or `keyframes`. | `video_gen/mod.rs#L1330-L1338` |

Every path-taking tool uploads the bytes to xAI as a data URL, so a
brief that names a private image (or text that steers the child to one)
sends it off the machine. Only real images are read (the tools reject
other bytes), but that is no comfort for a photo of a passport. Videos
are saved as `<session folder>/videos/<n>.mp4` (`video_gen/mod.rs#L14`,
`#L48`).

Consequence in aggy: `image_edit`, `image_to_video`, and
`reference_to_video` are off unless the user sets `[media]
grok_file_tools = true` in `~/.aggy/config.toml` (never repo config). So
`aggy video` and Grok `aggy image --edit` refuse by default, naming the
key; a new Grok image gets `image_gen` only. `reference_to_video` is never
given, opt-in or not.

**Confining them to images (T6751).** Source only, grok-build `2bdd1d6a`,
paths under `crates/codegen/`; not run live.

- No path rule reaches them. Both are checked as `AccessKind::Tool` with
  the tool's name and no path (`xai-grok-tools/src/permission_access.rs:40-41`).
  A rule's tool prefix is one of `Bash`, `Read`, `Edit`/`Write`,
  `MCPTool`, `Grep`/`Glob`, `WebFetch`, `WebSearch`, `AgentMessage`
  (`xai-grok-permission-rules/src/rules.rs:244-257`); `image_edit(<path>)`
  is a parse error (`UnknownToolPrefix`, `rules.rs:150-166`), and a bare
  `image_edit` is a tool-wide `Any` rule (`rules.rs:229-240`). A `Tool`
  access is reached only by `Any` rules and tool-wide `Edit` deny or ask
  (`policy.rs:547-577`), and matched against the tool's name
  (`policy.rs:749-755`). So `--deny` can remove either tool whole, never
  one path, and `Read`/`Grep` denies (S8, S9) do not reach them.
- A file that is not an image is refused before upload. The extension
  and any MIME a caller claims are not looked at; the bytes are.
  `image_edit` reads the path (`implementations/grok_build/image_edit/mod.rs:146`)
  and passes it as is only when it is at most the size cap and `infer`
  sniffs `image/jpeg` or `image/png` magic (`:52-60`); anything else must
  be detected and decoded by the `image` crate and is re-encoded
  (`:63-104`), else `failed to decode image reference` (`:84-86`); only
  then is the data URL built (`:159-161`) and sent (`:356`).
  `image_to_video` reads the path (`video_gen/mod.rs:909`) and calls
  `validate_image_bytes` (`:921`): magic-byte sniff, a full decode, a
  complete JPEG marker walk, and an allow-list of PNG, JPEG, GIF, WebP,
  BMP, and TIFF (`xai-grok-image/src/lib.rs:38-92`); it uploads the raw
  bytes only after that (`video_gen/mod.rs:924-925`). The refusal texts
  quote the path, not the bytes. The Grok home's `auth.json` is JSON, so
  either tool refuses it.
- Not covered: `image_edit`'s fast path checks only the magic bytes, and
  `image_to_video` ignores bytes after a JPEG's end marker
  (`lib.rs:113-114`), so a file that starts as an image, or is one with
  data appended, goes up whole. Any real image the model names goes up
  too; that is what the opt-in's text warns about.

Consequence in aggy: no rule is added to the media argv; the opt-in
stays off by default and unchanged.

## Session file formats (T6670)

Source only; no session file was read. `internal/transcript` follows
these, and adapters.md lists what it reads.

- **Codex** (rust-v0.164.0): a rollout is
  `sessions/YYYY/MM/DD/rollout-YYYY-MM-DDThh-mm-ss-<uuid>.jsonl`
  (`rollout/src/list.rs#L423`). Each line is `{"timestamp", "type",
  "payload"}` (`history/src/lib.rs#L369`, the `RolloutLine`;
  `history/src/rollout_payload.rs#L32`, tag `type` in snake case). A
  `response_item` payload is a `ResponseItem`, whose `message` has `role`
  and `content` (`protocol/src/models.rs#L1052`, `#L1062`), and content
  items are `input_text` or `output_text` with `text` (`models.rs#L883`).
  Messages are always persisted (`rollout/src/policy.rs#L82`); the
  `event_msg` `user_message` and `agent_message` events are persisted
  only in the legacy history mode (`policy.rs#L143`), so they are not
  read.
  Codex also writes context as `user` messages: AGENTS.md text
  (`# AGENTS.md instructions` ... `</INSTRUCTIONS>`,
  `core/src/context/user_instructions.rs#L31`), the environment context,
  turn notes (`<turn_aborted>`, `context/turn_aborted.rs#L36`), and a
  shell command the user ran, as `<user_shell_command>` holding
  `<command>` and a `<result>` with the full output
  (`context/user_shell_command.rs#L46`, `#L51`). Codex itself treats a
  user message as context when any text item, trimmed, starts with a
  fragment's start marker and ends with its end marker
  (`core/src/event_mapping.rs#L64`,
  `context/contextual_user_message.rs#L24`,
  `context-fragments/src/fragment.rs#L163`); aggy matches the same
  markers. The compaction summary is a `user` message with empty markers
  (`context/compaction_summary.rs#L34`) whose text starts with
  `prompts/templates/compact/summary_prefix.md` and a newline
  (`core/src/compact.rs#L403`, `#L618`); aggy labels it a summary.
- **Grok** (grok-build `2bdd1d6a`): a session folder holds
  `chat_history.jsonl` ("raw chat messages sent to the model") and
  `updates.jsonl` (`xai-grok-pager/docs/user-guide/17-sessions.md`,
  "Storage Layout"). `chat_history.jsonl` gets one `ConversationItem` per
  line (`xai-grok-shell/src/session/storage/jsonl/mod.rs#L1333`), tagged
  `type` in snake case: `system`, `user` (`content` a list of `text` or
  `image` parts, `synthetic_reason` set only on injected messages),
  `assistant` (`content` a string, plus `tool_calls`), `tool_result`,
  `backend_tool_call`, `reasoning`
  (`xai-grok-sampling-types/src/conversation.rs#L71`, `#L239`, `#L266`,
  `#L379`). The reader skips lines it cannot parse, as Grok's own loader
  does (`jsonl/mod.rs#L954`).
- **Claude**: no Claude doc gives the transcript format, so it is partly
  unverified. Grok-build (`2bdd1d6a`) lists Claude sessions as
  `projects/<cwd with every character but an ASCII letter or digit as
  ->/<id>.jsonl`, mapped per character
  (`xai-grok-foreign-sessions/src/claude/projects.rs#L44`), and skips a
  session whose first line has `isSidechain` true (`claude.rs#L372`). Its
  session-title heuristic (`first_prompt`, `claude.rs#L428`, which reads
  only the head of a file) takes lines with `type` `user` and
  `message.content` as a string or blocks of `type` `text`, skips
  `isMeta` and `isCompactSummary` lines, takes `! <cmd>` from
  `<bash-input>` (`#L464`), and skips text that starts with `<` and a
  lowercase letter or with "[Request interrupted by user" (`#L482`).
  aggy reads every line that way and skips `isSidechain` per line.
  **Unverified:** assistant lines (read the same way with `type`
  `assistant`); the bash-mode output tags `<bash-stdout>` and
  `<bash-stderr>` and `<local-command-stdout>`, which aggy drops; and how
  Claude names the folder of a long path.

Grok's auto-compaction threshold (T6673): `[session]
auto_compact_threshold_percent` in the user `config.toml`, also per model
as `[model.<id>].auto_compact_threshold_percent` and the environment
variable `GROK_AUTO_COMPACT_THRESHOLD_PERCENT`, a percent (0 to 100) of
the model's context window, default 85
(`xai-grok-shell/src/util/config/resolve/compaction.rs#L2`, `#L36`,
`#L38`). It is source-only at a revision possibly newer than 1.0.70, and
the context window of the user's Grok model is not recorded, so no
percent can be computed that keeps a session under 200,000 tokens. aggy
does not set it yet (TODO(T6673)).

## Managed tools (2026-10-28)

Live, by a maintainer with the owner's approval to install, on macOS
arm64 with `DO_NOT_TRACK=1`. No vendor config was edited, and no tool's
`init` or `install` subcommand was run. The built-in definitions in
`internal/tooldef/builtin` use these facts.

| Tool | Package, pinned | Binary | `--version` prints | MCP server or hook |
| --- | --- | --- | --- | --- |
| RTK | 1.1.0 (Homebrew) | `rtk` | `rtk 1.1.0` | hooks `rtk hook claude` (Claude `PreToolUse`) and `rtk hook codex` (Codex `PreToolUse`, which approves, so aggy does not use it); there is no Grok subcommand |
| CodeGraph | npm `@colbymchenry/codegraph@1.6.2` | `codegraph` | `1.6.2` | `codegraph serve --mcp` (stdio; takes `--path`, `--no-watch`) |
| Context7 | npm `@upstash/context7-mcp@4.4.4` (`--ignore-scripts`) | `context7-mcp` | `4.4.4` | `context7-mcp` (stdio by default; `--transport stdio\|http`) |
| Playwright MCP | npm `@playwright/mcp@0.0.83` (`--ignore-scripts`) | `playwright-mcp` (not `mcp-server-playwright`) | `Version 0.0.83` | `playwright-mcp` |
| Headroom | `uv tool install headroom-ai==0.42.0` (Python 3.10 or later) | `headroom` | `headroom, version 0.42.0` (about 7 s cold) | `headroom mcp serve` (not `headroom mcp`) |
| Caveman | git tag v4.0.1 (d262ab3), read only | none | n/a | skill `skills/caveman/SKILL.md` (4.7 KB) |

- **RTK's Claude hook does not approve (#59 F-6).** `rtk hook claude`
  answers a command it rewrites with `hookSpecificOutput` holding
  `permissionDecisionReason: "RTK auto-rewrite"` and `updatedInput`, and
  no `permissionDecision`, so the CLI still asks as it would have. It
  prints nothing for a command it does not rewrite.
- **RTK's Codex hook approves (S63-9).** `rtk hook codex` answers
  `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecisionReason":"RTK auto-rewrite","updatedInput":{"command":"rtk git status"},"permissionDecision":"allow"}}`:
  it approves every command it rewrites. aggy does not wire it; RTK is
  a Claude-only built-in.
- **CodeGraph** `codegraph init [path]` writes `.codegraph/` inside the
  repo, and `uninit` deletes it.
- **CodeGraph's index and refresh (T6711).** Read from the installed
  1.6.2 package (its `README.md` and `dist/*.d.ts`), not run: the index
  is `.codegraph/codegraph.db`, SQLite with `-wal` and `-shm` sidecars
  (`index.d.ts`); a repo counts as indexed once `codegraph.db` holds its
  schema (`directory.d.ts`, `isInitialized`); the running server keeps
  `.codegraph/daemon.pid` (`directory.d.ts`); `CODEGRAPH_DIR` can rename
  the folder (aggy does not pass it, so it stays `.codegraph`). The
  server refreshes the index itself: a file watcher syncs changes, and a
  `(size, mtime)` and content-hash catch-up runs when an MCP client
  connects (README, "How auto-syncing works"); `codegraph sync [path]`
  is the manual incremental update. Telemetry is on unless
  `CODEGRAPH_TELEMETRY=0` or `DO_NOT_TRACK=1` (README, "Telemetry");
  aggy passes the person's own values through and sets neither.
  Install: `npm install -g @colbymchenry/codegraph@1.6.2`. Its launcher
  (`npm-shim.js`) runs the bundle the npm package installed, and when
  that is missing downloads one from GitHub Releases into
  `~/.codegraph/bundles` (`CODEGRAPH_INSTALL_DIR`), checked against
  `SHA256SUMS`. Init (the platform bundle's `dist/directory.js`,
  `createDirectory`) makes `.codegraph/` and writes its own
  `.codegraph/.gitignore` (a header, `*`, and `!.gitignore`), which
  leaves that file itself untracked-but-visible, so aggy adds
  `.codegraph/` to the repo's `info/exclude`. The `init` command
  (`dist/bin/codegraph.js`, `runInit`) logs "Already initialized" and
  exits 0 when `codegraph.db` holds the schema, a half-built one an
  interrupted init left included, and rebuilds a database without the
  schema; `codegraph index [path]` rebuilds the full index from scratch
  and exits 1 when the path is not initialized. So after an interrupted
  init aggy runs `codegraph index {repo}` (`install.rebuild`), then
  `init` if that fails. Whether `index` asks anything without a terminal
  is unverified (TODO(T6711)).
  Under WSL on a Windows drive the folder may be `.codegraph-wsl`
  (`directory.d.ts`); aggy launches nothing there (TODO(T6711)).
  Unverified, for a maintainer's live check (TODO(T6711)): whether
  `codegraph init <path>` with no stdin and no `--yes` builds the index
  without asking anything (the README says `--yes` skips every prompt,
  so aggy does not pass it: a yes it cannot read is not given), and
  whether it writes anything outside `.codegraph/` (an agent's
  instructions file); and the name of the lock file (`FileLock`), which
  a worktree copy leaves out by copying only `codegraph.db` and
  `codegraph.db-wal`.
- **Context7** works without an API key (anonymous calls to its API);
  `--api-key` or `CONTEXT7_API_KEY` raises its rate limits.
- **Playwright MCP** downloads no browser with `--ignore-scripts`; the
  first real use needs `npx playwright install`.
- **Headroom.** Homepage <https://docs.headroomlabs.ai>, source
  <https://github.com/headroomlabs-ai/headroom>. Its full value needs
  `headroom proxy` and `ANTHROPIC_BASE_URL`, which aggy never uses; the
  MCP server alone offers compress, retrieve, and stats on demand.
  `headroom mcp install` edits every agent config it finds: never run it.
- **Caveman.** Its installers (`npx @caveman-ai/cli`, `install.sh`) find
  the agents and their logins, edit vendor configs, add a proxy, and
  send telemetry by default. aggy uses only the `skills/caveman/` folder
  at the pinned tag (T6715).
- **Unverified: whether Claude matches permission rules against the
  original input or the hook's `updatedInput` (H78-1).** aggy covers
  both: when a wired hook declares `rewrites` (only RTK's does), the
  launch adds every safety-policy Bash rule again with the hook's
  `rewrite_prefix` (`"rtk "`) in front, and for each key file a rule
  matching any prefixed command that names it (`Bash(rtk *.env*)`),
  since which file commands Claude's Read rules recognize in Bash is not
  documented. `aggy hook-tool` passes a rewrite only when its `command`
  is exactly `rewrite_prefix` plus the original command and every other
  key is the original's (R80-1); any other rewrite is dropped
  (`updatedInput_unexpected`). So whichever input Claude matches its
  rules against, the prefixed deny copies cover it: a Bash rule that
  matches the original matches the rewrite with the prefix in front, and
  a key file named in the original is named in the rewrite. A hook that
  does not declare `rewrites` has its `updatedInput` dropped. That RTK
  1.1.0 rewrites only in that whole-command form is unverified: a
  rewrite in another form (of `cat` to another subcommand, or of each
  part of `a && b`) is dropped, and the command runs as the model asked.
- **Unverified: whether `headroom mcp serve` reaches the network.** Its
  definition says `network = true`, so an edit borrow's child does not
  get it without `routing.edit_network_tools` (M78-2). Context7 calls its
  API, and Playwright drives a browser; CodeGraph is local.

## Same-vendor nested review (T6779)

Live-confirmed on 2026-10-28. The `review-route` binary at 3ef4753, with a
scratch `AGGY_HOME` and `aggy setup --only claude`, run from inside a
Claude Code 2.1.312 session (`CLAUDECODE=1` in the environment).
`aggy borrow review "…" --base main` with the default routing routed to
Claude and ran a fresh headless session: "ok in 12s, 432 tokens", and the
report found the bug planted in the diff. So a Claude review nested under
an on-screen Claude works.

Not run: Codex under Codex and Grok under Grok. The default route reaches
them only when Claude is limited, disconnected, or not allowed, or when a
user lists them. Grok's `--sandbox strict`, which a Grok review uses,
refuses to start inside another sandbox (S6 above), so a Grok review
nested under a sandboxed Grok session may fail closed. The smoke script
has no check for either yet (TODO(T6779)).

## Tool reload (T6729)

Not checked: whether any of the three CLIs reloads MCP servers, hooks,
or skills in a running session. aggy knows no in-place reload, so a tool
switched on or off for a running session takes a resume of that session
(design.md, "Applying a switch to a running session").

| Question | Verdict | What aggy does |
| --- | --- | --- |
| Claude: an MCP server or hook added to a running session | Launch flags only (`--mcp-config`, `--settings`, "Launch-time MCP" above: not stored with the session, re-passed on every resume) | Resume. |
| Codex: an MCP server added to a running session | Launch flags only (`-c mcp_servers.<name>.*`) | Resume. |
| Any CLI: a skill linked into its user skill folder while it runs | Unverified | Counted as needing a resume. |
| Any CLI: a command or API that reloads MCP config in place | Unverified; none known | None used (TODO(T6729)). |
| Grok | Its tools are gated (W71-4) | No offer. |

A live check would start each CLI, link a skill or change its MCP config,
and ask the agent for the new skill or tool in the next turn.

## Still open

1. Codex limit event, captured live from an exhausted account: the
   rollout lines and the exit code.
2. Codex hook firing and payload, after trusting a hook by hand.
3. Grok resume (`-r`) with an `--agent` file, and `--rules` on resume.
4. Whether a Codex `spawn_agent` child really starts and inherits the
   clean set (not observable in `exec --json`).
5. Backlogged: an OS sandbox around research children, which would
   re-enable Codex research (open: the existence oracle and the readable
   `~/.codex`).
6. Greedy `codex exec -i a.png -` without `--`.
7. Grok 1.0.70 folder trust in a headless review (`test/smoke/smoke.sh`,
   `grok-review-untrusted`; failed on 2383c13 on the argv, re-run
   pending): a scratch
   repo whose `.claude/settings.json` and `.grok/hooks` hold SessionStart
   and PreToolUse hooks that touch a marker, never trusted, run with
   aggy's exact Grok review argv; the marker must stay absent.
8. Grok's `auto_compact_threshold_percent` at 1.0.70, and the context
   window of the user's model, before aggy sets Grok's compaction below
   200,000 tokens (T6673).
9. aggy's installed hooks firing live in each CLI (T6616 for Codex).
   How each runs the command is source- or doc-verified (Claude `sh -c`,
   Codex `$SHELL -lc`, Grok "an inline shell command"); what Grok does
   with a hook that cannot start is not stated (aggy's command exits 0
   either way).
