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
   weaker; also `https://docs.x.ai/build/...`. Citations to "grok-src
   559751fd" name the same tree: grok-src 2bdd1d6a is the monorepo's
   SOURCE_REV 559751fd. Claude from
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
| Codex project trust in an edit child (T6892) | `codex exec` under workspace-write writes a project entry to the user's config, whatever `--ignore-user-config` says, unless a trust level is set. aggy sets one for the session. | Write Source-verified; fix Source-verified, not run live |
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

Status of each kind under strict, and of an image edit and video, which run without it:

| Kind | Status |
| --- | --- |
| review | Live-verified: S2, S4, S5; `grok-review-untrusted` passed under strict (b972adf re-run below), and again with the Grok home deny (S8, 807f323). The S9 spelling rules are not yet run live. |
| research | Live-verified: `research-borrow-grok` passed under strict plus the denies (b972adf), and again with the Grok home deny (807f323). |
| news | **Not yet run live** under strict. Same argv as research. |
| image | **Live-verified under strict with `--allow image_gen`** (2026-10-29, grok 1.0.70). The smoke `image-borrow-grok` of 2026-10-28 (at 25b89ee and 807f323) failed under strict: `aggy image` exited 1 after about 5 s, "the child left no file that passed the artifact gate". The cause is a permission prompt, not the sandbox's write rules: `grok -p "<make an image>" --tools image_gen --sandbox strict --output-format streaming-json` printed `permission required: imagine: <prompt> was not run, as a headless run cannot ask for approval` on stderr, exited 0, and made no image. The same command with `--allow image_gen` made the image, `images/1.jpg` in the session folder, seen on disk. Without strict it works as before. So an image generation runs under `--sandbox strict` with the sandbox host check and one allow rule, `--allow image_gen`, a bare tool name, which is a rule for that one tool (T6847, `usesSandbox` and `strictAllow` in `internal/adapter/grok/grok.go`). Then aggy's own argv, the smoke's `GROK_IMAGE_ARGS` (strict, the allow rule, every read deny, `--tools image_gen`, the full `--disallowed-tools`, `--no-subagents`, `--output-format json`), run directly as `grok --prompt-file prompt.txt` (2026-10-29, grok 1.0.70): exit 0, empty stderr, reply `images/1.jpg`, and the file was written (81,609 bytes). So image generation is live-verified with aggy's own argv. It keeps the read denies (S4, S5, S8, S9). The full `aggy image` borrow path (`smoke.sh image-borrow-grok`) has not been re-run. |
| image `--edit` | **Refused by aggy since T6923**, below, so this stays open only if that changes. **Not run live under strict.** An image edit (`image_edit`) runs without `--sandbox strict`, with the read denies, until a live check (TODO(T6821)). An `--edit` resumes a session that was created under strict, and Grok stores `--rules` with a session, so it may store the sandbox or permission rules too; an edit might then hit the same headless permission prompt. That is a live check to run. Its tools are checked as `AccessKind::Tool`, so those denies match nothing of its today; for the same reason, a HOME or Grok home that cannot be named in a rule drops the denies for an image edit and video instead of refusing them (N54-4). |
| video | **Untested**: the test account has zero data retention, under which Grok's video tools are unavailable (b972adf re-run below); turning ZDR off, or a user-hosted bucket, is an owner item. Runs like an image edit: no strict, read denies kept (TODO(T6821)). `image_to_video` and `reference_to_video` were not run under strict. |

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

## Session changes inside a CLI (T6901)

- **Claude, doc-verified** (`hooks.md`, read 2026-11-07, not seen live):
  `SessionStart`'s `source` is "`startup` for new sessions, `resume` for
  resumed sessions, `clear` after `/clear`, `compact` after compaction, or
  `fork` for a new session forked from an existing one". `SessionEnd`'s reasons
  include `clear` ("Session cleared with `/clear` command") and `resume`
  ("Session switched via interactive `/resume`"). That `session_id` then names
  the new session is implied, not stated.
- **Codex, source only:** the same five values
  (`hooks/schema/generated/session-start.command.input.schema.json`). Which of
  `/clear`, `/new`, and `/resume` sends which is unverified.
- **Grok, unverified:** no `source` is recorded, nor whether `/clear` fires
  `SessionStart`.
- **Unverified on all three:** that one CLI spawns every hook from one process
  at one depth. To confirm (T6895, T6896, the owner's live checks): under aggy,
  one turn, `/clear`, one turn, then the switch key and a pick. The status line
  should come at that turn's end with no `hook.follow_refused` in `aggy log`,
  and `aggy resume` should open the session after the `/clear`. Do it again with
  `/resume`.

## Folder trust as `aggy doctor` reads it (T6899)

Codex only, source only: the `[projects."<path>"]` table's `trust_level` in
`config.toml` (see "Codex project trust in an edit child"). That the TUI's
trust dialog writes the same entry, and what each CLI exits with when its
dialog is declined, are unverified: T6895 and T6896 wait for the owner to
accept trust dialogs. To confirm: decline each CLI's dialog once under `aggy`,
note the exit code, then check `config.toml` after accepting Codex's. Grok's
stored answer is not recorded; Claude's is (below, "Claude Code's folder
trust"), though doctor does not read it yet. Doctor says nothing for either.

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
  inline text only; there is no file variant. It is stored with the
  session at the first launch: a resume keeps the original rules and
  ignores new ones (live, 2026-10-29, below). Grok reads `AGENTS.md`, `CLAUDE.md`, and `.grok/rules/*.md`
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

### Asking on screen (T6967)

Docs and source only, read 2026-11-10. No real CLI was run; every line
below is unverified live until the recipe at the end is run.

- **Claude.** "Rules are evaluated in order: deny, then ask, then allow.
  The first match in that order determines the outcome", and "a matching
  ask rule prompts even when a more specific allow rule also matches the
  same call" (`permissions.md`, "Manage permissions"). Deny and ask rules
  apply to every subcommand, and "An ask rule like `Bash(git clean *)`
  still prompts you ... even in auto mode" (same page, "Compound
  commands"). PreToolUse hooks cannot override an ask ("a matching ask rule
  still prompts even when the hook returned allow", "Precedence among
  rules, hooks, and mods"). "Yes, and don't ask again" saves to
  `.claude/settings.local.json` at the repository root; the page documents
  no setting that hides that option for one rule. Doc-verified. Whether the
  prompt for a command an ask rule matched offers "don't ask again" at all
  is not stated, and unverified.
- **Grok.** Its config has `ask` rules (`deny` beats `ask` beats `allow`,
  above), but its command line documents `--allow` and `--deny` only
  (`src/app/cli.rs` at `2bdd1d6a`); aggy writes no vendor config per
  session, so the policy stays deny there. Source-verified.
- **Codex.** `decision = "prompt"` is a documented decision
  (`execpolicy/README.md`). In `create_exec_approval_requirement_for_parsed_commands`,
  a prompt rule under `AskForApproval::Never` becomes `Forbidden` with
  `PROMPT_CONFLICT_REASON` ("approval required by policy, but
  AskForApproval is set to Never"; `prompt_is_rejected_by_policy`,
  `core/src/exec_policy.rs` at `rust-v0.164.0`), and `codex exec` sets
  approval to never (`exec/src/lib.rs#L591` at `6aaac69`, already cited by
  `internal/adapter/codex`). The strictest decision wins, so an allow saved
  at Codex's prompt (`default.rules`) cannot loosen a prompt rule.
  Source-verified.
- **OpenCode.** A later permission rule wins, and `--auto` answers an ask
  with once while denies hold (this file, "OpenCode 2.2.5 as an on-screen
  harness", fake-model and live). Whether a stored Always allow in
  `opencode.db` beats a later `ask` rule in the launch's config is
  unverified; aggy cannot see those rows, so the strict groups stay
  `tool.use` denies on OpenCode (TODO(T6895)), except where aggy's
  `evaluate` hook can turn such an allow back to ask (T6976, below).
- **Claude bypass mode.** A `defaultMode` such as `bypassPermissions` in
  the person's own Claude settings may skip the ask rules; aggy does not
  read that file, and which modes skip an ask rule is unverified
  (TODO(T6895)). The `permissions.md` line above says an ask rule still
  prompts in auto mode only.
- **No pre-approval.** Removing an ask for a narrow allow is unsound: an
  allow aggy does not read, or a mode changed after launch, can cover the
  rest of the rule (re-review HIGH-N1: `Bash(rm:*)` in
  `~/.claude/settings.json` plus a removed `Bash(rm -rf*)` ask let
  `rm -rf ~` run unasked; a unit test, not live). So no allow changes a
  rendered policy rule; exact per-command pre-approval is a hook decision
  since T6976 (below, "Claude PreToolUse decisions").

Live-check recipe (a maintainer, never in CI, no `--dangerously-*` flag,
in a scratch git repo with a `.env`, a `.env.example`, `server.pem`, and a
`dist` folder):

1. `aggy run claude`; ask the model to `rm -rf dist`. Expect Claude's own
   approval prompt, not a refusal. Record which options it offers.
   Answer "don't ask again" if offered; quit; relaunch and ask again:
   it must still ask (an ask rule beats the saved allow). `aggy
   permissions show` lists the saved allow under `rm-rf` as changing
   nothing.
2. Ask it to read `.env.example`: no prompt from aggy's rules. Ask it to
   read `.env`: a prompt.
3. `aggy permissions add allow 'Read(./server.pem)'`: one line says the
   command still asks every time; relaunch: reading `server.pem` still
   asks.
4. `aggy permissions policy rm-rf deny`; relaunch: `rm -rf dist` is refused.
5. `aggy policy wire` (replacing an older aggy file if there), `aggy run
   codex`, ask for `git clean -n`: expect Codex's approval prompt with
   aggy's justification. `aggy borrow edit` (or any `codex exec`) must
   still refuse it.
6. `aggy run grok`: `rm -rf dist` is refused with no prompt (deny), and
   the launch notes that an allow naming it does not go to Grok.
7. `aggy run opencode`: `rm -rf dist` asks; with `--auto` it is refused;
   `git push --force origin main` is refused in both.
8. `aggy resume <id>` of a finished Codex edit borrow: refused with
   `policy.unreachable`, and Codex does not start.

### Claude PreToolUse decisions (T6976)

**Docs, read 2026-11-10** (code.claude.com/docs `hooks.md`, "PreToolUse
decision control", and `permissions.md`, "Precedence among rules, hooks, and
mods"): a PreToolUse hook's output `hookSpecificOutput.permissionDecision` is
`allow`, `ask`, `deny`, or `defer`, with `permissionDecisionReason`; several
hooks combine as deny > defer > ask > allow. A matching static deny rule
blocks, and a matching static ask rule still prompts, even when a hook
returns allow, so a static ask can never be lifted by a hook. A hook failure
(timeout, a bad exit, bad JSON) goes on through the normal permission flow,
unless the hook entry has `onFailure: "block"`. `permissions.md` ("Read and
Edit") applies Read rules to the built-in tools that read files, Grep and
Glob best effort, and Edit rules to those that edit them, which is the list
aggy's matcher names.

**Live, 2026-11-10**: Claude Code 2.1.322, macOS, three one-turn runs in a
scratch folder, every prompt declined, no `--dangerously-*` flag, the real
Claude config untouched (the hooks were in a `--settings` layer):

| Check | Result |
| --- | --- |
| Default mode | Auto mode is the default permission mode at this version |
| A hook whose inner command exited 3, with a shell `\|\|` fallback printing an ask decision, in auto mode and in acceptEdits | A prompt: "Hook PreToolUse:Bash requires confirmation for this command: <reason>", options Yes and No only, no "don't ask again" |
| A hook that slept past its `timeout`, with `onFailure: "block"` | The call was blocked |

**Unverified, and the live check that would settle each** (a maintainer,
never in CI, no `--dangerously-*` flag, a scratch folder, every prompt
declined unless it says otherwise):

- Bypass mode (`--permission-mode bypassPermissions` with `--no-policy` and
  the hook in a `--settings` layer): whether a hook's deny and ask hold.
  aggy denies in that mode, as it does in `dontAsk`.
- A hook's allow against auto mode's own classifier: run `aggy claude` with
  `aggy permissions add allow "Bash(git push --force origin main)"` in a
  scratch repo with a local bare remote, and see whether the classifier
  still stops it.
- Plan mode: whether a hook's ask or deny is shown or overridden.
- The `Read` tool matcher, and the regular-expression form
  `^(Bash|Read|Edit|Write|MultiEdit|NotebookEdit|Grep|Glob)$`: ask the model
  to read `.env` and to run `ls`; expect a prompt for the first and none
  from aggy for the second, and no hook call for `WebFetch`.
- That a hook's ask beats a static allow rule in the same layer (`Bash(git
  push *)` beside the hook's ask on `git push --force origin dev`).
- That no other built-in tool reads files past the hook's list.
- Managed settings: aggy now reads the managed settings file at the path
  `settings.md` ("Settings files") gives per OS (macOS `/Library/Application
  Support/ClaudeCode/managed-settings.json`, Linux and WSL
  `/etc/claude-code/managed-settings.json`, Windows `C:\Program
  Files\ClaudeCode\managed-settings.json`), only its `disableAllHooks` and
  `allowManagedHooksOnly`, and keeps the static asks when either is set. The
  paths are the docs', not checked live at 2.1.322; a drop-in folder or
  server-managed settings, if Claude has them, are not read. Check: with
  `allowManagedHooksOnly: true` in that file, run `aggy claude` and see the
  launch note, then `/hooks` in Claude.

**Live, PR #129 review** (2026-11-10, a maintainer: Claude Code 2.1.322,
macOS, a test folder, five one-turn runs, every prompt declined, hooks in a
`--settings` layer):

| Check | Result |
| --- | --- |
| A PreToolUse entry matched to `^(Bash\|Monitor)$` | Fired for a Bash call and gave the ask prompt: the anchored form works |
| A hook that exits 0 printing two JSON objects back to back, with `onFailure: "block"` | Blocked: Claude reported the output could not be parsed and the entry's `onFailure` blocked the call. aggy's wrapper prints its answer only on exit 0 anyway |
| A `Monitor` call with a hook matched to `Bash` only | The hook ran; the prompt read "Hook PreToolUse:Monitor requires confirmation for this command", so the hook's input names `Monitor`. A Bash matcher covering Monitor is verified at 2.1.322 only; aggy matches `*` and judges Monitor's `command` itself |
| Launched in `<repo>/sub/` with `sub/.claude/settings.local.json` = `{"disableAllHooks":true}` and an ask hook in `--settings` | No hook ran; the command was "Allowed by auto mode". Claude reads project settings from the launch folder, and that file turns off a `--settings` hook (MEDIUM-1 confirmed) |
| A session started with the ask hook; the first command prompted; then `.claude/settings.local.json` in the launch folder was written with `{"disableAllHooks":true}` mid-session | The next command ran, "Allowed by auto mode", no hook: a mid-session write of that file removes the hook for the rest of the session (HIGH-4 confirmed) |

Because of the last row, aggy keeps static ask rules for writes of Claude's
project settings files beside the hook (design.md, section 10), as a hook
cannot ask once hooks are off.

**Still unverified after the PR #129 review** (each with the check that would
settle it; not checked live: bypass mode, plan mode, the Read matcher, managed
settings, `CLAUDE_CODE_SIMPLE`):

- Whether Claude reads a `--settings` *file* again during a session. aggy
  keeps the static asks for that launch, so it does not depend on it. Check:
  start `claude --settings <file>` with an ask rule, edit the file to drop it,
  and run the command.
- Whether Claude also reads `.claude/settings*.json` of folders between the
  launch folder and the repo root. aggy reads them all. Check: the file in
  `<repo>/a/`, launch in `<repo>/a/b/`.
- `CLAUDE_CODE_SIMPLE`: its name and meaning (perhaps what `--bare` sets) are
  unverified; aggy keeps the static asks when it is set. Check: `claude --bare
  -p 'env | grep CLAUDE_CODE'` in a scratch folder.
- That the matcher `*` sends an MCP tool and `WebFetch` to the hook (the
  anchored form is verified above; `*` is from `hooks.md`). Check: a
  recording hook matched to `*`, then a WebFetch.
- That the static `Bash(*.claude/settings*)` ask matches with a leading `*`
  (`permissions.md`, "Wildcard patterns", read 2026-11-10), and that
  `Edit(//**/.claude/settings.local.json)` covers Write. Check: ask the model
  to write the file both ways with the hook off.
- A hook's ask after a mid-session switch to `bypassPermissions` (Shift+Tab):
  aggy denies when the payload reports that mode; whether Claude shows the
  ask or runs the call is unverified. Check: start in default mode with
  `--allow-dangerously-skip-permissions`, switch, and run a policy match.
- Grep or Glob whose `path` is a folder (`Grep` over `certs/`): aggy has no
  opinion, as a folder names no key file; what the static ask rules did for
  the same call on main is unverified. Check: `Read(**/*.pem)` as an ask rule,
  then a Grep with `path` a folder holding a `.pem`, with and without the hook.

### OpenCode permission evaluate (T6976)

**Source, read 2026-11-10, not run**: anomalyco/opencode `dev` at `d4aeee5`,
`packages/core/src/permission.ts` lines 184-201 and 340, and
opencode.ai/docs/build/plugins/server: a plugin's
`ctx.permission.hook("evaluate", ...)` may change `effect` to allow, ask, or
deny; the event carries `action` and `resources`. An explicit configured
deny is final and does not invoke the hook; a saved Always allow is
evaluated before the hook, and the hook can turn it back to ask. At the
pinned 2.2.5, the fake-model run above ("Always allow") saw a plugin's
`evaluate` hook set an allow back to ask, and it held; nothing else of the
hook was tried there. **Unverified at 2.2.5**: the ask-to-allow direction;
that assigning `e.effect` is how the hook answers (the plugin assigns it);
what `resources` holds for a compound shell command (aggy allows only one
resource, the whole command); and what a throw or a plugin that fails to
load does (the guard has the same exposure). Live check: in a trusted repo,
`aggy permissions add allow "Bash(git push --force origin main)"`, then
`aggy run opencode` with a fake model that asks for that command, for it
joined with `&& echo x`, and for `git push --force origin dev` answered
Always allow and asked again: expect no prompt, a prompt, and a prompt.

**After the PR #129 review** (2026-11-10, no live run). The plugin now turns
an allow into an ask whenever aggy's answer does not come (a failure, a
timeout, a throw), and on a strict launch aggy's guard refuses a strict match
the `evaluate` hook did not clear in that process. `TestOpenCodePluginStrict`
(`test/e2e`) runs the real plugin under node with the real aggy and a stand-in
for OpenCode's plugin context, not OpenCode: with a saved allow, the hook API
absent and an `evaluate` that never ran both end in the guard's refusal, and
aggy failing, timing out, or its plan removed or changed all turn the effect
into an ask. Unverified at 2.2.5, each with its check (a fake model, a trusted
repo, no `--auto`):

- What a read's `resources` holds (aggy takes a path, relative to the session
  folder or absolute). Check: a private key read, then look at the clear the
  guard used (`aggy hook opencode-guard` exits 0) or the refusal.
- That `reply` is `reject` on `permission.replied` for a refusal (the plugin
  forgets every clear on it). Check: refuse a force push's prompt, then ask
  for it again with the evaluate hook made to fail.
- That a key read by its own name prompts, answered once, and reads; that a
  harmless name linked to a key is refused with no prompt. Check: both, with
  `aggy run opencode`.
- With the plugin not loaded at all, nothing of aggy's runs: a key read and a
  strict force push prompt (the owner accepted this), and a saved Always allow
  for either would run it (TODO(T6953)). Check: break the plugin's folder and
  watch for the launch's missing `GuardLoaded`.

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

Not checked: whether exec's default web search is on under this set.
`codex.EditLockdown` is this set.

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

## Codex project trust in an edit child (T6892)

Source-verified at commit 6aaac69 (adapters.md has the links): under the edit
lockdown, `thread/start` writes a project entry to the user's `config.toml`
unless a trust level is set for the cwd, so aggy sets one for the session with
`-C <cwd> -c projects={"<cwd>"={trust_level="untrusted"}}`. That is source
only: it has not been run live.

A maintainer's check, which never touches the real `~/.codex`:

1. Point `CODEX_HOME` at a new scratch dir and run `codex login` there, so the
   scratch home is signed in; copy no credential file. Set `AGGY_HOME` to a
   scratch dir outside temp dirs.
2. Make a never-trusted scratch repo with one committed file, an
   `AGENTS.override.md` saying to end every report with one made-up word, and
   an `AGENTS.md` saying to end it with another.
3. Record `shasum "$CODEX_HOME/config.toml"`, or that the file does not exist.
4. Run `aggy borrow edit "add a line saying hello to notes.txt" --vendor codex`.
5. All of these must hold:
   - `config.toml` is byte for byte as in step 3;
   - the borrow succeeded, which also shows `--strict-config` takes the override;
   - the child's change landed: `git -C "$AGGY_HOME/worktrees/<borrow id>" status
     --short` lists `notes.txt` and it holds the line, since a child the sandbox
     refused can still report success;
   - the report ends with the `AGENTS.override.md` word, not the `AGENTS.md` one.
6. On macOS, repeat with `AGGY_HOME` spelled through `/System/Volumes/Data`, then
   in another case. Record whether the child's change landed in each: the first
   should since aggy removes the prefix, and the second is unverified.
7. Record the scrubbed results here (no paths, ids, or tokens), then remove the
   scratch `CODEX_HOME`, `AGGY_HOME`, and repo.

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
  `notification_type`; per the docs (read 2026-11-07), `idle_prompt` is
  "Claude finished responding about 60 seconds ago and you haven't typed
  since" and `permission_prompt` is "Claude needs you to approve a tool use
  or a sandboxed command's network request, and the prompt has waited about
  six seconds". Doc-verified; not seen live. A command hook that exits with anything
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
  (`user_interrupt`, ...); `Notification` adds `notificationType`.
  Live (2026-10-29, grok 1.0.70, below): an extra `stop` with
  `"reason":"shutdown"` fires at every exit, and the payload's event
  names are snake_case. Every
  hook gets `GROK_HOOK_EVENT`, `GROK_HOOK_NAME`, `GROK_SESSION_ID`,
  `GROK_WORKSPACE_ROOT`, and `CLAUDE_PROJECT_DIR`. Grok also reads the
  hooks in `~/.claude/settings.json` with the same camelCase envelope
  (plus `hook_event_name`), unless `[compat.<vendor>] hooks = false` or
  "the corresponding environment variable"; the doc at this revision
  does not name the variable, so `GROK_CLAUDE_HOOKS_ENABLED` (from
  `docs.x.ai`) is Doc-verified only there. Whether Grok's
  `SessionStart` payload has a source field is not stated: unverified,
  and aggy does not read one.

## Commands inside a session (T6908 to T6911)

Docs and source only, not run live (read 2026-11-09). What aggy relies on:

- **Claude `UserPromptSubmit`** (`code.claude.com/docs/en/hooks.md`): its input
  adds `prompt`, "the submitted text"; output `decision: "block"` "stops the
  prompt before it reaches Claude", `reason` is "Shown to the user ... Not added
  to context", and `suppressOriginalPrompt: true` leaves the text out of the
  block message. Exit 2 blocks too, but aggy's hook command ends in `|| true`.
  `UserPromptExpansion` fires "When a user-typed command expands into a prompt",
  with `command_name`, `command_args`, and the original `prompt`, and blocks the
  same way. The docs do not say outright whether a blocked prompt's text goes
  back to the input, nor which of the two hooks a skill picked from the `/` menu
  sends: unverified. Nor is it recorded that Claude at the tested version
  accepts a `UserPromptExpansion` entry in `settings.json`, so `aggy hooks
  install` does not write one (R121); a live check would be: add it by hand to
  a scratch `CLAUDE_CONFIG_DIR`'s settings, start `claude --version` and one
  session, and see no settings error.
- **Claude `disable-model-invocation: true`** (`code.claude.com/docs/en/skills.md`):
  "Description not in context, full skill loads when invoked"; `argument-hint`
  is the autocomplete hint.
- **Grok** (grok-build `2bdd1d6a`, `crates/codegen/xai-grok-pager/docs/user-guide/10-hooks.md`):
  `UserPromptSubmit` ("You submit a prompt") can block; `{"decision": "block",
  "reason": "..."}` on stdout blocks on any exit code, the reason is shown to the
  person and not added to the model's context; only prompts the person types can
  be blocked; its default timeout is 30 seconds, and "a timed-out or crashed hook
  fails open and the prompt proceeds". The payload's field for the text is not
  named there: aggy reads `prompt`, unverified.
- **Grok skills** (`user-guide/08-skills.md` at `2bdd1d6a`): the frontmatter takes
  `disable-model-invocation` ("When `true`, only your slash command runs the skill
  -- the model cannot invoke it automatically") and `argument-hint`.
- **Codex**: no frontmatter key that keeps a skill out of the model's context is
  recorded, nor whether it ignores unknown keys such as
  `disable-model-invocation`: unverified. No hook path (design.md, section 5).
- **OpenCode**: only that it loads `~/.claude/skills` and `~/.agents/skills`
  (fake-model, below); whether the command skills show in its menu, and any
  prompt hook, are unverified, so aggy uses the keystroke catch alone there.
- **Claude skill precedence** (`code.claude.com/docs/en/skills.md`, "Resolve
  skills that share a name", read 2026-11-09): "Enterprise over personal, and
  personal over project"; so aggy's skills, linked in `~/.claude/skills`, win
  over a project's skill of the same name.
- **OpenCode built-in commands** (`opencode.ai/docs/tui`, read 2026-11-09,
  current docs, not pinned to a version): `/connect`, `/compact`
  (`/summarize`), `/editor`, `/exit` (`/quit`, `/q`), `/export`, `/help`,
  `/init`, `/models`, `/new` (`/clear`), `/redo`, `/sessions` (`/resume`,
  `/continue`), `/themes`, `/thinking`, `/undo`, `/update`: none is `switch`,
  `borrow`, `inbox`, or `aggy`.
- **Every CLI, unverified**: that Ctrl+U (or its CSI u form) clears the composer
  line, which the keystroke catch types in place of the held Enter.

To confirm (owner's live check): under aggy on each CLI, type `/switch` and
press Enter (the picker, and no turn in the transcript); type some text, then
Ctrl+U, then Enter, and see no message sent: with that recorded here for a CLI,
`clearLineChecked` in `internal/adapter` flips to true for it, and a typed
`/switch <vendor>` there skips the status line (R121); type `/inbox`; pick
`/switch` from the CLI's own menu with a vendor (Claude and Grok: the prompt is
blocked and the status line asks); and check `aggy log` and `aggy diag` for
`supervisor.command.caught`. For the typed-prefix rule (T6975, design.md
section 5), on Claude and Grok: type `/s`, press Down until `/switch` is
highlighted, and Enter (the picker opens, `via: hook` in `aggy diag -n 20`);
type `/sw`, Tab, `grok`, Enter (the status line asks `/switch via hook`), and
note what Tab put in the composer (`/switch ` with its space, or not); type
`/in`, Tab, Enter (the inbox page); type `/`, Down to `/aggy`, Enter (refused,
`command.no_enter`, and the skill tells the model to change nothing); and with
`/s` typed, press Up and see whether the menu moves or a history line comes
back. Record each, and whether a menu pick fires `UserPromptSubmit` with the
command as `prompt`. For the hook path's known limits (TODO(T6975)): send a
prompt the hook blocks and see whether the CLI puts its text back in the
composer; and in each CLI's shell mode (Claude's `!`) run `test -t 0 && echo
tty` to see whether stdin is a terminal there. After Claude's `/model`, with
its picker closed by Esc, type `/switch` and Enter: today it goes to the CLI
until a turn end (only `/clear` frees the composer at once); a check that the
prompt has the keys again after each local command would let it be caught.

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
`aggy video` refuses by default, naming the key (Grok `aggy image --edit` has
been refused outright since T6923); a new Grok image gets `image_gen` only. `reference_to_video` is never
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
  data appended (a PNG trailer too), goes up whole. Any real image the
  model names goes up too, with its EXIF metadata such as GPS position
  when it is sent as is; that is what the opt-in's text warns about.

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
  aggy sets both unless the user config allows telemetry (T6822).
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
  `init` if that fails. Neither `init` nor `index` asks anything with no
  stdin and no terminal, and `init` writes only `.codegraph/` (live,
  2026-10-29, below), so aggy passes no `--yes`.
  Under WSL on a Windows drive the folder may be `.codegraph-wsl`
  (`directory.d.ts`); aggy launches nothing there (TODO(T6711)).
  Unverified (TODO(T6711)): the name of the lock file (`FileLock`),
  which a worktree copy leaves out by copying only `codegraph.db` and
  `codegraph.db-wal`.
- **Context7** works without an API key (anonymous calls to its API);
  `--api-key` or `CONTEXT7_API_KEY` raises its rate limits.
- **Playwright MCP** downloads no browser with `--ignore-scripts`; the
  first real use needs `npx playwright install`.
- **Headroom.** Homepage <https://docs.headroomlabs.ai>, source
  <https://github.com/headroomlabs-ai/headroom>. Its full value needs
  `headroom proxy` and `ANTHROPIC_BASE_URL`, which aggy sets only for a
  vendor the person turned Headroom's proxy on for (T6780); the MCP
  server alone offers compress, retrieve, and stats on demand.
  `headroom mcp install` edits every agent config it finds: never run it.
- **Headroom proxy (T6780).** The flags, `/health`, and the beacon's
  `DO_NOT_TRACK` opt-out are now live-verified (2026-10-29, below); one
  Claude turn through the proxy is not, so the definition still lists
  `proxy` in `unverified` (TODO(T6780)). The earlier record,
  from docs.headroomlabs.ai/docs/proxy (latest, not pinned) and the
  README, and source-read by a #93 reviewer:
  `headroom/cli/proxy.py:296,334` defines `--host` (default
  `127.0.0.1`) and `--port` (default `8787`);
  `headroom/proxy/server.py:4274` serves `GET /health`, which answers
  JSON with `"status": "healthy"`; `headroom/telemetry/beacon.py:60-95`
  turns the beacon off when `DO_NOT_TRACK` is set; Claude Code is pointed at it
  with `ANTHROPIC_BASE_URL=http://localhost:8787`; Codex and Grok are
  routed by `headroom wrap` or a config edit, with no environment
  variable documented, so aggy's definition supports claude only; an
  anonymous usage beacon is on by default, and `DO_NOT_TRACK=1`,
  `HEADROOM_TELEMETRY=off`, or `--no-telemetry` turns it off. To check
  at 0.42.0 by hand (never `headroom wrap`, `init`, or `mcp install`):
  `headroom proxy --help` for the flags; `headroom proxy --host
  127.0.0.1 --port 8787`, then `curl -sS -o /dev/null -w '%{http_code}'
  http://127.0.0.1:8787/health` for the health path; whether it starts
  with only `HOME`, `USER`, `LOGNAME`, `PATH`, `TMPDIR`, the locale, and
  `DO_NOT_TRACK=1`, and sends no beacon then; and one Claude turn with
  `ANTHROPIC_BASE_URL=http://127.0.0.1:8787`. The flags, the health path, and the
  beacon were run on 2026-10-29; aggy's scrubbed environment and the
  Claude turn were not.
- **Claude and an HTTP proxy in the environment (T6780).** Live
  (2026-10-29, below): Claude sends a request for a loopback
  `ANTHROPIC_BASE_URL` through `HTTP_PROXY` unless `NO_PROXY` lists the
  address, and goes direct when it does. So aggy refuses to start a tool
  proxy when one of `HTTP_PROXY`, `http_proxy`, `ALL_PROXY`, `all_proxy`
  is set and `NO_PROXY`/`no_proxy` does not cover the address
  (`tool.proxy_env_proxy`). `ALL_PROXY` was not run; it is treated the
  same way.
- **Caveman.** Its installers (`npx @caveman-ai/cli`, `install.sh`) find
  the agents and their logins, edit vendor configs, add a proxy, and
  send telemetry by default. aggy uses only the `skills/caveman/` folder
  at the pinned tag (T6715).
- **Claude matches permission rules against the hook's `updatedInput`,
  not the original input (H78-1; live, 2026-10-29, below).** So when a
  wired hook declares `rewrites` (only RTK's does), the
  launch adds every safety-policy Bash rule again with the hook's
  `rewrite_prefix` (`"rtk "`) in front, and for each key file a rule
  matching any prefixed command that names it (`Bash(rtk *.env*)`),
  since which file commands Claude's Read rules recognize in Bash is not
  documented. `aggy hook-tool` passes a rewrite only when its `command`
  is exactly `rewrite_prefix` plus the original command and every other
  key is the original's (R80-1); any other rewrite is dropped
  (`updatedInput_unexpected`). So the prefixed deny copies are what
  stops a rewritten command: a Bash rule that matches the original
  matches the rewrite with the prefix in front, and a key file named in
  the original is named in the rewrite. A hook that
  does not declare `rewrites` has its `updatedInput` dropped. That RTK
  1.1.0 rewrites only in that whole-command form is unverified: a
  rewrite in another form (of `cat` to another subcommand, or of each
  part of `a && b`) is dropped, and the command runs as the model asked.
- **Unverified: whether `headroom mcp serve` reaches the network.** Its
  definition says `network = true`, so an edit borrow's child does not
  get it without `routing.edit_network_tools` (M78-2). Context7 calls its
  API, and Playwright drives a browser; CodeGraph is local.

## Live checks, 2026-10-29 (T6847)

Run by the owner with claude 2.1.312, grok 1.0.70, codegraph 1.6.2, and
headroom-ai[proxy] 0.42.0. The Grok image result is in the strict status
table above.

- **Grok hooks.** Grok fires an extra `stop` hook with
  `"reason":"shutdown"` at every exit, interactive and headless. A
  headless SIGINT gave no `stop_cancelled`, only that shutdown stop.
  `sessionId` was set on `stop`, `stop_failure`, and `stop_cancelled`, so
  `grokStopNamesSession` stays true (T6807-R2). The payload's event
  names and `GROK_HOOK_EVENT` are snake_case (`stop`, `stop_failure`,
  `stop_cancelled`); aggy takes the event from the installed command's
  argv (`internal/hook/install.go`), so that does not matter to it. The
  supervisor ignores a Grok `Stop` whose `reason` is `shutdown`: it ends
  no turn and is recorded as `hook.grok_shutdown`.
- **Grok resume.** `--agent <file>` must be passed again on
  `grok -r <id>`: without it the agent's MCP server was gone, and with it
  a fresh server worked. `--rules` is stored with the session at the
  first launch: a resume without `--rules` kept the original rules (the
  model recalled a number given only there), and new `--rules` passed on
  resume were ignored (the reply followed the old rule).
- **Claude and an HTTP proxy.** With `HTTP_PROXY` set and no `NO_PROXY`,
  the POST to a loopback `ANTHROPIC_BASE_URL` went through `HTTP_PROXY`,
  with credentials. With `NO_PROXY=127.0.0.1,localhost,::1` it went
  direct. So `tool.proxy_env_proxy` is needed as built.
- **Claude `--`.** `claude --mcp-config <file> -- "<prompt>"` works
  interactively.
- **Claude permission rules and a hook's `updatedInput` (H78-1).** Claude
  matches its rules against the rewritten command: a deny rule on the
  rewritten command blocked it, and the denial named the rewritten
  command; a deny rule on the original command let the rewritten one
  run. So `RewriteDeny`'s prefixed deny copies are required.
- **CodeGraph 1.6.2.** With no stdin and no terminal, `codegraph init .`
  exited 0 without asking anything and wrote only `.codegraph/`
  (`.gitignore` and `codegraph.db`). `codegraph index .` did not ask
  either. A second `init` printed "Already initialized" and exited 0.
- **Headroom 0.42.0 proxy.** `headroom proxy` needs the `[proxy]` extra;
  without it, it fails with "Proxy dependencies not installed. Run: pip
  install headroom-ai[proxy]". `GET /health` answers 200 JSON
  (`status: healthy`, `ready: true`), and it listens only on the
  `--host 127.0.0.1 --port` given; it also serves `/livez`, `/readyz`,
  `/stats`, and `/metrics`. Its log says "Anonymous upload beacon:
  ENABLED (default — session summaries are sent to Headroom Labs)", and
  with `DO_NOT_TRACK=1` "DISABLED (DO_NOT_TRACK=1)". A subscription
  tracker, on by default, polls `GET
  https://api.anthropic.com/api/oauth/usage` every 300 s with the Claude
  OAuth token, learned from a loopback caller's traffic or read from
  `CLAUDE_CODE_OAUTH_TOKEN` or `~/.claude/.credentials.json`
  (`headroom/subscription/client.py`, `tracker.py`,
  `credential_policy.py`); `--no-subscription-tracking` (env
  `HEADROOM_NO_SUBSCRIPTION_TRACKING`) turns it off. Its Codex tracker,
  which uses the bearer token from traffic, stays on, but aggy's proxy
  is Claude only. An update check contacts PyPI; `HEADROOM_UPDATE_CHECK=off`
  turns it off, and `--stateless` skips it. The first use downloads a
  model from Hugging Face; with `HF_HUB_OFFLINE=1` it never became
  healthy within 90 s, so aggy does not set it. It writes `~/.headroom`
  (`compression_cache.db`, `logs/proxy-<port>.log`, `update_check.json`,
  `subscription_state.json`) and `~/.cache/huggingface`. With
  `DO_NOT_TRACK=1`, `--no-subscription-tracking`, and
  `HEADROOM_UPDATE_CHECK=off`, the log showed the beacon, subscription
  tracking, and Claude quota tracking all disabled; one TLS connection
  to an Anthropic address stayed open, purpose unknown, with no usage
  polls. Time to a healthy `/health` was 24.6 s, 56.8 s, and 80.6 s on a
  machine with a load average of 100 to 200; the first start after
  install was not healthy after 60 s, and SIGTERM did not stop that cold
  start within 5 s (SIGKILL did; aggy's group kill covers it). So
  headroom's definition sets `--no-subscription-tracking`,
  `HEADROOM_UPDATE_CHECK=off` (`telemetry_env`), and
  `health_timeout = 90`. Not run: one Claude turn through the proxy, so
  it stays `unverified = ["proxy"]`.

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

## OpenCode with a SuperGrok plan (2026-11-06)

The owner asked for this check, installed OpenCode, and signed in themselves. It is one turn, for the planned open-harness work (M8). OpenCode 2.2.5, on macOS.

Docs: xAI's [Use Grok in OpenCode](https://x.ai/news/grok-opencode) (May 21, 2026, read 2026-11-06) says to use a SuperGrok or X Premium subscription inside OpenCode through `/connect`, with a browser and a headless sign-in. It lists no terms specific to this. OpenCode's own [providers page](https://opencode.ai/docs/providers/) has no xAI sign-in section.

Live, in an empty temporary git folder, with `--standalone` and without `--auto`:

| Check | Result |
|---|---|
| `opencode auth list` | Lists xAI as `stored`; the credential file was not opened |
| `opencode models` | Lists `xai/grok-build-0.1` and other `xai/grok-*` models |
| `opencode debug config` | Shows the folder's `opencode.json`, which denied every action |
| `opencode run --format json -m xai/grok-build-0.1 '<reply with one word>'` | Exit 0, the expected reply, no tool call, no 403 |
| `grok --version` afterwards | Still runs; no Grok turn was run, so its login was not exercised |

Not established:

- **What the reported cost means.** The turn's `step_finish` event reported a non-zero `cost`. The owner signed in with the SuperGrok option, and their account showed usage and no charge. No API-key variable was set in the shell that ran it. Read later in the source (T6930, below): a cost the endpoint reports is used first, otherwise the model's price table times the tokens, and OpenAI's plan methods empty that table while xAI's does not. So the figure is most likely a table estimate, though whether xAI's endpoint reports its own cost was not checked. It is not proof of billing either way.
- **Whether the deny rule is enforced.** The config loaded, but the turn called no tool.
- **Limits, longer sessions, and tiers other than the owner's.** xAI's page doesn't say how use counts against a plan's limits, and Hermes's docs report that xAI can return 403 on some tiers.

## OpenCode 2.2.5 as an on-screen harness (2026-11-06, spike T6930)

**Who ran these.** Agents ran the checks here, except the two marked as the owner's. On 2026-11-06 the owner approved agents running OpenCode live for this spike only, which is an exception to the rule in `CLAUDE.md` that agents never run real vendor CLIs. It covers OpenCode alone: the real Claude, Codex, and Grok CLIs were not run.

Runs were on macOS, with OpenCode 2.2.5 (MIT, source at tag `v2.2.5`, commit `9bbb8ce`; paths below are under `packages/`). Every command used `--standalone` or a test server of its own, and no vendor credential file was opened. In the first round none used `--auto` and 17 runs tried to reach a model on the owner's sign-ins, 3 of which reached none (a dead port and two bad model names). A second round, below, tested `--auto`. A third round, after review, used no real model.

Each fact is marked by how it was established:

- **Live**: the real OpenCode, either on the owner's xAI or OpenAI sign-in or in a temporary `HOME`, where no model was needed.
- **Fake-model**: the real OpenCode against a loopback test server, in a temporary `HOME` with no credentials and no key. It shows what OpenCode does, not what a vendor does.
- **Source**: read at the tag, not run.

**Exception to the temp-folder rule.** Run 17 (`opencode mini`, xAI, project config on) was meant for a temporary folder, but mini takes its location from `$PWD`, which the test script had not updated, so it ran in an aggy checkout.

- **Evidence that no file changed.** `git status --short` was empty straight after, and the session export shows one tool call, `echo -n "ready"`. That is all the evidence: `git status` does not cover ignored files, and no file listing was taken before the run.
- **What was sent to the model.** The prompt "Reply with exactly the word ready", then the `echo` result. The request itself was not captured. By source and the fake-model checks below, it also held OpenCode's own instructions, the tool definitions, skill descriptions, and an environment block with the date, the checkout's full path, whether it is a git repo, and the platform (`core/src/instructions/builtins.ts`). The export shows no file read. The checkout has no `AGENTS.md`, and `CLAUDE.md` is not read.
- **What it left.** A session row and a snapshot folder in OpenCode's data directory, kept for the owner to decide on. The folder's creation time matches the run; only its name and times were looked at. **It may hold checkout content:** snapshots capture the working tree into a git store there, with ignored files left out and untracked files up to 2 MiB (source, `core/src/snapshot.ts`).

### Repo config and safety

| Check | Evidence | Result |
|---|---|---|
| Trust prompt | Source, live | None in the source; none on interactive launches |
| Repo plugin in `.opencode/plugins` | Live (`debug config`) | Its code ran with no prompt |
| Repo local MCP server | Live (`debug agents`) | Its command ran with no prompt |
| `{file:../outside}` and `{env:NAME}` in repo config | Live (`debug config`) | Both substituted |
| Repo `providers.xai.settings.baseURL` | Live (signed-in xAI, a port with no listener) | `provider.transport`, exit 1: the request went to the repo's address. Whether the sign-in header goes with it was not checked |
| `OPENCODE_DISABLE_PROJECT_CONFIG=1` | Live, fake-model | Repo config, **server** plugin, MCP server, agents, base URL, and repo skills all have no effect; `AGENTS.md` is dropped too (fake-model). **A repo's `tui.js` is not covered:** it still runs on the full screen (third round, below) |
| `OPENCODE_CONFIG_PROJECT_DISABLE` | Source (`cli/src/server-process.ts`), fake-model | Beats `OPENCODE_DISABLE_PROJECT_CONFIG`: a stale `=0` turned project config back on while the other was `1` |
| Load order | Source (`core/src/config.ts`) | Managed policies, integrations, global, `OPENCODE_CONFIG`, repo files, `.opencode`, `OPENCODE_CONFIG_CONTENT`, managed settings |
| `policies` in `OPENCODE_CONFIG` against a repo's allow | Live | The per-launch deny holds; a repo's agent with allow-all is still denied; the repo's plugin and MCP server did not run |
| `policies` in `OPENCODE_CONFIG_CONTENT` against a repo's allow | Live (`models`) | The repo wins: policies take the earliest loaded |
| Global config against `OPENCODE_CONFIG` | Source, fake-model | **The global layer beats aggy.** A global `policies` allow on `shell:*` cancelled aggy's `shell:rm *` deny and the file was removed. A global `plugins` folder loads, both server and TUI entries |
| `policies` allow | Fake-model | **Policies only ever deny.** A `shell:*` allow with `permissions` ask-all was still rejected, so allows have to live in `permissions`; a policy allow can only cancel a later policy's deny |
| Repo `permissions` allow-all, launch `permissions` ask-all | Fake-model | Once project config is on, as it would be after a user trusts a repo, **the repo's allow-all wins** and the command ran. With project config off it was rejected |
| `permissions` in `OPENCODE_CONFIG_CONTENT` against a repo agent | Live (`debug agents`) | The repo agent keeps its allow-all rule |

### Permissions

| Check | Evidence | Result |
|---|---|---|
| No config | Live (xAI) | Reads inside and outside the folder, a write, and shell all ran, with no ask, exit 0 |
| `permissions` deny-all | Live (xAI) | No canary reached, exit 0. Tools are hidden from the model, and it made 170 failed `skill` calls in 127 s before answering |
| `permissions` ask-all | Live (xAI) | Every call rejected, exit 0, one stderr line per request |
| `tool.use` deny on `read`, `edit`, `shell`, `external_directory` | Live (xAI) | All blocked, error type `permission.rejected`, but `grep` still returned an inside file's lines; `glob` and `execute` also ran |
| `execute` | Source (`core/src/tool.ts`, `core/src/codemode/tool.ts`), fake-model | **No permission assert of its own:** it ran under ask-all and under a `tool.use` `execute:*` policy deny. Only a `permissions` deny on `execute` removed it. Its inner calls do get `execute.before` (third round, below) |
| Path deny on `read` and `external_directory` | Live (xAI) | The read tool was blocked; `cat`, `cd` then `cat`, and `python3` all read the file. OpenCode has no OS sandbox (source search) |
| `--auto` | Source (`core/src/permission.ts`, `cli/src/run/noninteractive.ts`, `web/src/content/docs/permissions.mdx`) | Answers `ask` with once; denies still hold. `--yolo` and `--dangerously-skip-permissions` are hidden aliases. Run in the second round, below |
| "Always allow" | Fake-model, source | Saved in the `permission` table of `opencode.db` per project, for example `shell` `echo *`; a later session used it. Configured denies are checked first (source). The docs page saying "current session" is out of date |

### On-screen

| Check | Evidence | Result |
|---|---|---|
| First screen, no sign-in, new folder | Live | No dialog; first output under 0.5 s |
| Alternate screen | Live | `opencode` uses it, with mouse reporting; `opencode mini` uses neither |
| Changing model in `opencode mini` | Owner's try (2026-11-06), fake-model, source (`tui/src/mini/footer.command.tsx`) | The owner found no `/model` and set a model only at launch with `-m`. Rechecked: `/model` gives "No matching items". **But the Ctrl-P menu has "Switch model"**, and after choosing a second model the next turn reached the fake server on it. The owner then used Ctrl-P "Switch model" on a real session (2026-11-06). So mini lacks the slash command, not the ability |
| `/exit` with nothing open | Live | Exit 0 |
| `/exit` then Enter over a permission dialog | Fake-model | The letters were dropped and **Enter chose Allow once: the command ran** |
| Ctrl-C over a permission dialog | Fake-model | Declined the call; the app stayed open |
| Ctrl-C on an empty prompt | Live | Exit 0 |
| `--session <new id>` | Live, fake-model | The session takes that id; the exit screen prints it |
| Location | Live (`mini`), source (`run`, `cli/src/run/run.ts`) | Both use `$PWD`, so a launch must set it |

### Models

| Check | Evidence | Result |
|---|---|---|
| Top-level `--model` | Live (help), source | None; `run` and `mini` have `-m` |
| `model` in `OPENCODE_CONFIG_CONTENT`, new session | Live (OpenAI) | Session on that model; export confirms |
| Same, resuming | Fake-model | The session's own model wins |
| `POST /api/session/:id/model` | Fake-model (own `opencode serve`; also `opencode api session.switchModel`) | HTTP 204; a running screen says it switched; `session.model.selected` names old and new |
| xAI to OpenAI, and back, after a tool call | Live (4 runs), source (`core/src/session/runner/to-llm-message.ts`) | Exit 0 both ways; the word and the file's contents survived. Provider-bound metadata is dropped on a change |
| `opencode models` | Live | 0.5 s, works with `--standalone`, one `provider/model` per line, respects `provider.use` policies. Help documents the command, not the format as stable |
| API-key variables in the environment | Fake-model (dummy values, sent nowhere) | `ANTHROPIC_API_KEY` adds 20 `anthropic/*`; `XAI_API_KEY` adds xAI as an environment connection |
| Claude route | Source (`core/src/plugin/provider/claude-agent`) | `claude-agent` runs the local `claude` through the Agent SDK once connected; not connected here, so not listed |

### MCP config

Format, from source (`schema/src/mcp.ts`, `schema/src/config/mcp.ts`). The key is `mcp.servers.<name>`, and `mcp.timeout` sets a default:

| `type` | Fields |
|---|---|
| `local` | `command` (array, required), `cwd`, `environment`, `disabled`, `codemode`, `timeout`, `protocol` |
| `remote` | `url` (required), `headers`, `oauth` (object or `false`), `disabled`, `codemode`, `timeout`, `protocol` |

| Check | Evidence | Result |
|---|---|---|
| A `local` entry in repo config starts | Live | Ran with no prompt |
| `mcp.use` deny in `OPENCODE_CONFIG` | Live | The entry did not start |
| An entry in per-launch config | Source (`core/src/config.ts`) | Read like any config document. No per-launch server was run, so aggy's copied servers are unverified |
| `opencode mcp list --standalone` | Live | Printed none while the repo's entry had started. Not explained, so it is not a reliable check |

### OpenAI sign-in methods

Source only (`core/src/plugin/provider/chatgpt.ts`, `core/src/plugin/provider/openai.ts`). No method other than the owner's existing one was run. Which one the owner used is a source inference, not a check: `openai/gpt-5.5` is listed on the owner's sign-in, and the legacy methods disable it, which implies Sign in with ChatGPT.

| | Sign in with ChatGPT | Codex browser and Codex device code, both labelled legacy |
|---|---|---|
| Method id | `chatgpt-token-sharing` | `chatgpt-browser`, `chatgpt-headless` |
| Flow | Browser only, with a local callback | Browser with a local callback, or a device code |
| Client | User-owned: registered at first sign-in, its id kept in the credential's metadata | One fixed id in the source |
| Scope and token | `chatgpt.tokens.use.direct`; the ID token is verified; a sign-in without token sharing fails | `openid profile email offline_access`; no ID-token check found |
| Endpoint | `https://api.openai.com/v1` | `https://chatgpt.com/backend-api/codex` |
| Transport | HTTP | WebSocket by default |
| Compaction | Summary | `x-codex-beta-features: remote_compaction_v2` header |
| Models | A fixed list of 18, including `gpt-5.5` | Only above `gpt-5.5`, without bare `gpt-5.6`; image limited to `gpt-image-2` |
| Price table | Emptied | Emptied |
| Limit | `subscription_sharing_usage_limit_exceeded` becomes `provider.chatgpt-limit` and is not retried | Same mapping if that body appears; no retry exception and no legacy-specific limit shape found |

- **Storage.** Each is an `oauth` row in the `credential` table of `opencode.db` under the `openai` integration, apart from xAI's row. An older `chatgpt-browser` row holding a client id is rewritten to `chatgpt-token-sharing` at load.
- **Telling them apart.** `auth list` shows integration, label, and `stored`. Its JSON connections have only `id`, `label`, `method`, and `type` (keys checked live), and `method` is `oauth` for all three. `auth export` prints secrets and was not run. So no safe command names the method. By source only, `openai/gpt-5.5` in `opencode models` means the legacy methods are not in use, because they disable it. It is listed on the owner's sign-in; no legacy sign-in was run to confirm the contrast.

### Signals, transcripts, instructions

| Check | Evidence | Result |
|---|---|---|
| Turn end | Fake-model | `session.execution.succeeded`, with `sessionID`, on `/api/event` (401 without password) and to a plugin. `.failed` and `.interrupted` are in `schema/src/session-event.ts` (source) |
| Plugin from per-launch config, project config off | Fake-model | Loaded; the repo's server plugin did not |
| Plugin given as a file path | Fake-model | **Silently never loads**: exit 0, no error, with or without `file://`. Only the directory form, holding a `package.json`, worked |
| Plugin slash command | Fake-model | Ran from the screen with no model request |
| `opencode api` | Source (`cli/src/commands/handlers/api.ts`) | Buffers the response, so it cannot stream events |
| Limit type | Fake-model, source (`core/src/session/to-session-error.ts`, `core/src/model-retry.ts`) | `provider.chatgpt-limit` from the ChatGPT code; `provider.rate-limit` from a plain 429, in `session.retry.scheduled`, up to 10 retries (about 84 s). No real limit was hit, and no xAI-specific type exists |
| Event format | Live | The same event types and fields on xAI and OpenAI; no model or provider in them |
| `session export` | Live | `info.model`, per-message model, and `model-switched` entries. It calls an `experimental` API |
| `session list --format json` | Live | `id`, `title`, `directory`, `projectId`, `created`, `updated`; no model |
| Storage | Source, file names only | SQLite `opencode.db`, which also holds credentials, so transcripts must come through `opencode session`, never the file |
| `CLAUDE.md`, repo and `~/.claude` | Fake-model | **Not read**, including with no `AGENTS.md`. The `rules` docs page saying it is read is out of date |
| `instructions` in per-launch config | Live | Reaches the model, including with project config off |
| `AGENTS.md` | Fake-model | Reaches the model with project config on |
| Skills | Fake-model | `~/.claude/skills`, `~/.agents/skills` always; repo `.claude`, `.agents`, `.opencode` skills only with project config on |
| `step_finish.cost` | Live, source (`core/src/session/usage.ts`) | xAI non-zero, OpenAI 0. A cost the endpoint reports is used first, otherwise the price table. OpenAI's plan methods empty that table; xAI's does not |
| Standalone cost | Fake-model | About 0.6 s per launch; two at once both finished |
| Service and stale config | Source (`cli/src/server-process.ts`) | Config variables are read at server start, so per-launch config cannot reach a running service. Never run, per the owner's rule |
| Headless exit | Live | 0 on success, including denied tools; 1 on a transport error or bad model (`provider.no-route`, 0.5 s) |

### `--auto` under the proposed launch (second round)

The owner lifted the `--auto` ban for this round only. It added 8 real runs, 25 in all, and every launch first asserted that its real working folder and `PWD` were inside the temporary folder. The launch is `OPENCODE_DISABLE_PROJECT_CONFIG=1` with `OPENCODE_CONFIG=<file>` holding `policies`, in a folder with a hostile `opencode.json` and plugin, which had no effect in any run. Shell test commands were harmless, with a loopback port nothing listens on and planted canaries; the "database" was a planted decoy.

**What `--auto` does**

| Check | Evidence | Result |
|---|---|---|
| Mechanism | Source (`tui/src/routes/session/index.tsx`, `cli/src/run/noninteractive.ts`) | The client answers each `ask` with once, so nothing is saved (fake-model: no `permission` row) |
| Headless | Live (xAI, OpenAI), fake-model | Undenied shell and write ran with no prompt, exit 0; without the flag, `ask` rules reject |
| Hard denies | Live (xAI, OpenAI), fake-model | `shell:rm *` held live and in the fake runs, a `read:` deny in the fake runs; exit 0, and the model saw the error |
| `opencode --auto` on screen | Live (OpenAI), fake-model | No dialog; the command ran; a denied command did not |
| `--auto` resuming with `--session` | Live, fake-model | Works, including a headless resume from xAI to OpenAI. The flag is per launch: resuming without it shows the dialog again |
| `session.permissions: "autoaccept"` in `OPENCODE_CLI_CONFIG_CONTENT` | Fake-model | Same as the flag on the full screen; ignored by `mini` |
| `opencode mini --auto` | Fake-model | No such flag: exit 1 |
| `mini` with no `ask` rules | Live (xAI), fake-model | No dialog, since the default is allow, and `permissions` allow-all in `OPENCODE_CONFIG` gave the same; a `policies` deny held. An `ask` rule shows a dialog with no flag to skip it |
| `OPENCODE_CONFIG_CONTENT` `model` with `OPENCODE_CONFIG` policies and `--auto` | Live (OpenAI), fake-model | Works together, headless and on screen |
| That `model` on a provider the policy denies | Fake-model | **Falls to another allowed model, exit 0, with no error**, so `/switch` must check the model it got |

**Text-matching denies on shell.** All 26 command shapes are fake-model, under `--auto`; none ran on a vendor. The rule's text is matched against each simple command, not the whole line (source, `core/src/shell/parse.ts`), so a pipe is never part of what it sees.

| Rule | Blocked | Ran |
|---|---|---|
| `shell:curl * \| *` | None | All but one, including a plain `curl … \| cat`: **it matches nothing** |
| `shell:curl *` | Pipe, `&&`, `;`, variable URL, subshell, `$()`, backticks, extra spaces, newline, function | `sh -c`, `bash -c`, variable command, script file, `"curl"`, `c'ur'l`, `\curl`, absolute path, `env`, `command`, `eval`, `xargs`, `base64 \| sh`, `${IFS}`, heredoc |
| `shell:*curl*` | All of those shapes where the word appears literally | Variable command, `c'ur'l`, `base64 \| sh` |

Under all three rules, one `python3 -c` shape was refused by the shell scanner, before any rule, so it is not a deny: other `python3 -c` commands ran in the first round. A script that already exists under another name was not tried; the script-file case was only caught by `*curl*` because the command that wrote it held the word.

**Shell read of a planted decoy database, by path.** The list below is fake-model. Denies on `read`, `grep`, and `external_directory`, plus `shell:*decoy*` and `shell:*opencode.db*`.

- Blocked: read tool, grep tool, `cat <path>`, `cd <path> && cat`, a command holding the literal path.
- **Read the decoy:** `cat ../dec*/open*.db`, `(cd ../dec*; cat open*)`, a variable-built path, `find … -exec cat`, a script, and `cp` then `cat`.
- The one live case, on both plans (xAI, OpenAI): with only a `read:` deny, `cat` read it. The file was a planted decoy, not a real database.

So a deny cannot protect a file from the shell. OpenCode has no OS sandbox.

**Plugin guard.** A plugin from `OPENCODE_CONFIG` registers `execute.before` and runs a stand-in script with the tool name and input; a non-zero result throws.

| Check | Evidence | Result |
|---|---|---|
| Stand-in refuses | Live (xAI), fake-model | The tool did not run, under `--auto`; exit 0; the model saw the stand-in's text as the tool's error, and quoted it |
| Stand-in crashes, hangs past its limit, or is missing | Fake-model | All refused: the plugin's throw fails closed (source: `plugin/src/promise/adapter.ts`) |
| Other tools | Fake-model | `write` was refused the same way. Subagents and `execute`'s inner calls: third round, below |
| Repo plugin, or repo config removing the guard, with project config off | Fake-model | No effect; the guard ran and the repo's plugin did not |
| The same with project config on | Fake-model | **The repo disabled the guard** and the command ran |
| **Guard plugin has a syntax error, or its folder is missing** | Fake-model | **Fails open: no error, exit 0, and the command ran**, so aggy must confirm the guard loaded before trusting a session |

**Tampering from the model's own shell.** All fake-model, own server. The model runs as the same user.

- Overwriting the `OPENCODE_CONFIG` file **took effect in 3 s and removed a deny**: config reloads when the file changes.
- Overwriting the plugin file **reloads it and removes the guard**. One run still refused after 3 s, a later one no longer did at 3 s, and the review saw it within 6 s, so the timing varies and there is no safe window.
- Removing the plugin from the `OPENCODE_CONFIG` file **unloaded the guard**, by 3 s.

**Key file.** Fake-model, dummy values, `{file:}` in per-launch config, own server. The dummy key did reach the request header.

| Step | Key sent |
|---|---|
| First turn | The file's value |
| File rewritten, then a new turn | The old value: read once, not per request |
| File deleted, then new turns, including 70 s later | The old value still works |
| File deleted, turn from a **new folder** | **None, and no error from OpenCode** |
| File deleted, config file changed | **None** |
| File recreated, config file changed | The new value |

So the value is read at config load, per folder. Deleting the file after load works only until the session moves to another folder or config reloads.

| Exposure | Evidence | Result |
|---|---|---|
| Tool shell's environment, `ps eww`, and files under `HOME` | Fake-model | The key value was in none of them |
| `/api/config`, `/api/provider`, `/api/model` | Fake-model | **All return the key to anyone with the server password** |
| `OPENCODE_PASSWORD` in tool shells | Fake-model | **Set under `opencode serve`**, and the model's shell used it to fetch the key from `/api/config`. Absent from the tool's environment under `--standalone`, but see the next row |
| `--standalone` child's password | Fake-model | **Visible to the same user**: `ps -E` on the child shows it (`ps -Eaxww` lists every process), and with `lsof` for the port, `/api/config` returned the dummy key. So `--standalone` does not keep the key from a same-user shell |
| A plugin deleting it in `shell.create.before` | Fake-model | Removed from the tool shell, but `ps eww` still shows the password to the same user |

### Follow-up after review (third round)

No real model and no `--auto` in the checks below: a fake model, a temporary `HOME` and XDG dirs, no credentials, and a dummy key where one is named. Every launch first asserted that its working folder and `PWD` were inside the temporary folder.

**1. A repo's `.opencode/plugins/<dir>/tui.js`, with project config off.** Fake-model. It was marked by a file written from the module's top level, so this shows import only: its `tui()` never fired, even in the baseline.

| Launch | `tui.js` ran |
|---|---|
| `opencode --standalone`, repo as working folder | **Yes** |
| Plus `OPENCODE_CLI_CONFIG_CONTENT` `plugins` `-demo.tui`, or `-*` | **Yes** |
| Plus `OPENCODE_CONFIG_DIR` elsewhere | **Yes** |
| Plus `plugin.use` deny and `-` ids in `OPENCODE_CONFIG` | **Yes** |
| Working folder in a repo subfolder | **Yes** |
| Empty working folder, repo as the directory argument (it changes directory first) | **Yes** |
| `opencode mini --standalone`, with and without a turn | No |
| Empty working folder, no session | No |
| Empty working folder, `--session <id>` located at the repo | No, and the turn's shell ran in the repo |

So no variable or config key stops it on the full screen (source: `tui/src/app.tsx`, `tui/src/plugin/discovery.ts` have no project-config check). Only the launch shape does. That shape has a cost: new sessions default to the launch folder (source, `tui/src/config/new-session-location.ts`), and `session.new_location: inherit` may fix that (not run).

**2. `OPENCODE_CONFIG_DIR` pointed at an aggy folder.**

| Check | Evidence | Result |
|---|---|---|
| Global policies and plugins | Fake-model | Removed: aggy's deny held, and neither the server nor the TUI plugin ran |
| Sign-in | Fake-model (dummy credential) | Survives: `auth list` shows it, and `opencode.db` stays in the data directory |
| What it does not remove | Source (`core/src/config/discovery.ts`) | The system directory, macOS managed preferences, `~/.claude`, `~/.agents` |
| What it loses | Source | The user's global config and `cli.json` |
| Read-only folder and files | Fake-model | OpenCode launches. A plain write from the model's shell fails, but **`chmod` then write succeeded**, including rewriting a deny out of the `OPENCODE_CONFIG` file |
| `OPENCODE_DISABLE_FILEWATCHER=1` | Fake-model | The deny held after tampering in the same folder, but **a new folder on that server read the tampered file**. Aggy would have to rewrite the file at each launch |

**3. Scrubbing `OPENCODE_*` at launch.** Fake-model for precedence. It closes the stale-variable hole only if `OPENCODE_CONFIG_PROJECT_DISABLE` ends up removed or `1`. A repo's `.env` was ignored (also source: `autoloadDotenv: false` in `cli/script/build.ts`).

Variables that change config, plugins, permissions, or what runs (source): `OPENCODE_CONFIG`, `_CONFIG_CONTENT`, `_CONFIG_DIR`, `_CONFIG_PROJECT_DISABLE`, `_DISABLE_PROJECT_CONFIG`, `_DISABLE_CLAUDE_CODE`, `_DISABLE_CLAUDE_CODE_SKILLS`, `_CLI_CONFIG_CONTENT` (autoaccept, TUI plugins), `_TEST_HOME`, `_DB`, `_DISABLE_CHANNEL_DB`, `_FILEWATCHER_DISABLE`, `_DISABLE_FILEWATCHER`, `_MODELS_URL`, `_MODELS_PATH`, `_CLAUDE_PATH`, `_PTY_BIN`, `_NODE_PTY_PATH`, `_PARCEL_WATCHER_PATH`, `_PASSWORD`, `_SERVER_PASSWORD`; also `HOME` and `XDG_*`.

**4. Exact rules in `permissions`.** Fake-model, no `--auto`, so a rejection means it would prompt. Source for matching: `core/src/util/wildcard.ts`.

| Rule | Prompts | Runs |
|---|---|---|
| `git status`, `git diff`, `ls` (no `*`) | `git status -s`, `git  status`, `ls -la`, `ls > x`, `ls>x`, `ls 2> x`, `git status > x`, `git diff --output=x`, `git diff --no-index a b`, `git diff --ext-diff`, `FOO=1 git status`, `git status; touch x`, `git status \| tee x`, `$()`, backticks, `<()`, `"git" status`, `command git status` | The exact commands; `(git status)`, `{ git status; }`, `git status #c`, `git status &`; **`cd <anywhere> && git status`, including outside the project** |
| `git diff *`, `ls *`, `git status *` | `ls>x` | `git diff --output=x` and `ls > x` **ran and wrote files**; `git status > x` too; `git diff --no-index a b` |

Scripts named in a repo's `.git/config`, all under exact rules:

| Setting | Command | Script ran | What stopped it |
|---|---|---|---|
| `core.fsmonitor` | `git status`, also after `cd` into a nested repo | **Yes** | `git -c core.fsmonitor=false status`, or launch env `GIT_CONFIG_COUNT=1`, `GIT_CONFIG_KEY_0=core.fsmonitor`, `GIT_CONFIG_VALUE_0=false`; a model's `GIT_CONFIG_COUNT=0` prefix then prompts, but `export GIT_CONFIG_VALUE_0=<command>; git status` or `PATH=<dir>:$PATH; git status` ran with no prompt, since declarations aren't checked (`core/src/shell/parse.ts`) |
| `diff.external` | `git diff` | **Yes** | Launch env `GIT_EXTERNAL_DIFF=true`, or `diff.external` emptied the same way; rule `git diff --no-ext-diff --no-textconv` |
| `diff.<driver>.command` with `.gitattributes` | `git diff` | **Yes** | Rule `git diff --no-ext-diff --no-textconv` |
| `filter.<name>.clean` with `.gitattributes` | `git diff` | **Yes**. `git status` did not in this one test, but did in the later test below once a tracked file's time changed | Not found; that rule was not tried against it |
| `core.pager` | `git log`, no terminal | No | |
| `post-index-change` hook | `git status` | Yes, once a tracked file's time changed; no in the first test, which changed none | |

**5. The `execute.before` guard.** Fake-model, allow-all rules, so only the guard decides. It sees the raw tool input only, so it can't reliably see redirects or flags. A guard that refuses text naming a protected path:

- Caught: a literal `.git/config` in redirect, absolute path, or `tee`; the write tool, relative and absolute; a literal plugin-folder path, including with `cd`; a literal config file name; `git config core.fsmonitor`.
- **Changed the target anyway:** `cd .git && echo x > config`, `sed -i` after `cd`, variable, quote, and glob splits, `base64 \| sh`, a script file, `git config core.$k`, and `cd` plus a glob on the config file.
- `python3 -c` was refused by the shell scanner (`dynamic-execution`), not the guard.
- **Subagents:** seen, both the `subagent` call and the shell call inside it.
- **`execute`:** inner calls are seen with assembled input (`'GUARD'+'ME'` arrived joined) and a refusal holds. Shell, read, and write are not in the code-mode catalog, which here held `opencode` tools only, so `execute` could not reach them.

**6. "Always allow".** Fake-model; no setting to turn off or limit saving was found in the config schema (source search).

| Check | Result |
|---|---|
| Saved rows | `python3 x.py` saved `python3 *`, `echo hi > r.txt` saved `echo *`, `git push nowhere main` saved `git push *` |
| Under an `ask` rule | Saved rows beat it: `python3 y.py` ran, and saved `echo *` let **`echo x >> .git/config` run with no prompt** |
| Config deny, or policy deny, over a saved row | The deny held |
| A plugin's `permission` `evaluate` hook setting allow back to ask | Held. It would fail open if the plugin breaks, as the guard does |
| Other project | Not covered by the row |
| Deleting the rows (temp database only) | Back to asking. `/api/permission/saved` also has a DELETE (source, not run) |

Not established: a real plan limit on either plan; which sign-in method the owner used for OpenAI, beyond the source inference above; whether a provider key from `{file:}` reaches a real vendor's request header (the fake model received it); SIGTERM and SIGHUP behaviour; the service path.

## OpenCode, second review (2026-11-06, fake model)

Real OpenCode 2.2.5 against a loopback fake model in a temp `HOME`, with no credentials.

- With only exact rules, declarations before an allowed command ran arbitrary code with no prompt, as the `core.fsmonitor` row says. Substitutions, function definitions, heredocs, `trap`, and an `if` wrapper still prompted.
- `*` and `?` in a rule keep their wildcard meaning, and `{file:}` in a rule is expanded: `rm -f *.tmp` allowed `rm -f keep a.tmp`.
- `/open` on the full screen moved a `--auto` session to another known project, where a command ran with no prompt. That project's screen plugin should not load (source only).
- A plugin or a policy planted in the `OPENCODE_CONFIG_DIR` folder took effect in a later launch in another repo.
- With a `read` allow on `*`, a read through a repo symlink sent a canary to the model with no prompt, past a deny on the target's name. The direct outside path prompted.
- `run -m` with a denied provider, a resume after its provider was denied, and no allowed provider each exited 1 with no request. `mini -m` is untested.

## Native sessions (T6965, 2026-11-10)

Docs and source only; no vendor CLI was run and no real session file was read.
The tests build session stores by hand under a temp HOME.

- **Claude, doc-verified** (code.claude.com/docs/en/sessions, read
  2026-11-10): transcripts are JSONL at
  `~/.claude/projects/<project>/<session-id>.jsonl`, `<project>` being the
  working directory "with non-alphanumeric characters replaced by `-`"; a
  converted name over 200 characters is cut to 200 and "a hash of the full
  path" added; `CLAUDE_CONFIG_DIR` moves the store, and with it set,
  `CLAUDE_CODE_PROJECT_DIR_NAME` (1 to 64 letters, digits, hyphens, or
  underscores) names the folder instead. `claude --resume <name>` resumes a
  named session, an ambiguous name opens the picker, `claude --resume` alone
  opens the picker, `--resume` also takes a transcript's absolute path, and an
  id is searched for in every project. The picker and `--continue` leave out
  `claude -p` and Agent SDK sessions. The entry format "is internal to Claude
  Code and changes between versions". cli-reference (same day): `-r` "Resume a
  specific session by ID or name, or show an interactive picker".
  **Unverified:** the hash (so aggy lists nothing for such a folder), whether
  a non-ASCII letter counts as alphanumeric (likewise), and which field marks
  a `-p` session (so aggy's list may show one). The folder name is not
  one-to-one (`my_app`, `my-app`, and `my.app` share one; so do `/a/b-c` and
  `/a/b/c`), so aggy also requires a file's first `cwd` to name the folder:
  session lines carry `cwd` ("Claude Code session file fields" below, read
  live, not documented), and a file whose head records none is listed for no
  folder. cli-reference (same day): `--no-session-persistence`, "Disable
  session persistence so sessions are not saved to disk and cannot be resumed.
  Print mode only"; aggy's `-p` runs pass it when help lists it (help listing
  unverified live). Parsers, for `--resume=<name>`: commander's README
  ("Options", the option-argument can "follow an `=` for a long option") and
  clap's tutorial ("Options", `--name=bob`), read 2026-11-10; that Claude
  parses with commander and Grok with clap is as recorded in
  docs/adapters.md and the fakes. This settles the folder name
  `internal/transcript` and `sessionFiles` took from grok-build, for a short
  ASCII path.
- **Codex, source-verified**: as "Session file formats" above, plus
  `session_meta`'s `source` and `cwd` (`rolloutInDir`, protocol.rs#L3275 at
  6aaac694). Whether `codex resume` takes a thread name is not recorded, so
  aggy passes Codex only ids.
- **Grok**: the session folder as above; `-r`/`--resume
  [<SESSION_ID_OR_TITLE>]` from `grok --help` at 1.0.77 ("Grok's flags"
  below). The cwd encoding is `grok.EncodeCwd`'s reading, unconfirmed. No
  recorded source names a cwd field in Grok's session files, so aggy checks
  none: the url-encoded folder name is one-to-one, and a miss lists nothing.
  Which session a `--resume <title>` reopened comes from Grok's
  `SessionStart` hook (`sessionId`, "Hook payloads and config" above), as for Claude's
  `session_id`.
- To confirm live (owner): in a scratch repo, one turn in plain `claude`,
  then `aggy -r` lists it with its first prompt and reopens it; one turn in
  `claude -n demo`, then `aggy claude -r demo` and, after quitting, `aggy
  claude -c` reopens the same session.

## Claude Code session file fields (2026-11-06)

Read on the owner's machine, by key and count only, from 39 session files of the last 30 days, at the installed Claude Code. Not a documented format. Shell calls appear as `tool_use` blocks named `Bash` with `input.command`; session lines carry `permissionMode` and `cwd`; `tool_result` blocks carry `tool_use_id` and `is_error` when a call failed. A full scan of 730 MB in Python took 2.7 seconds. `is_error` does not separate a denied command from a failed one. Not every line carries `permissionMode`, so `internal/suggest` keeps the last one a file named. Its fixture, `internal/suggest/testdata/session.jsonl`, is built by hand from this list, not from a real session; a live check should compare it with a current session file's keys.

## Shared permissions (T6912 to T6916)

Docs and source only, read 2026-11-07. No real CLI was run, and none of this has been checked live. A maintainer's live check should settle each of these:

1. Claude honours `allow`, `ask`, and `defaultMode` in an inline `--settings` layer. `permission-modes.md` names `--settings` for `permissions.defaultMode`, and `settings.md` says lists merge; a fake CLI only proves the argv.
2. Grok on screen with `--allow <rule>` and `--permission-mode <mode>` together with its `--deny` flags, and on a resume. `--allow` is in the T6952 help; a deny outranks an allow in source (`decision_rank`, `policy.rs:791`, grok-build `2bdd1d6a`). `grok --help` may show a flag for ask rules, which is unverified, so none is passed.
3. Codex's `trust_level` under `[projects."<dir>"]` trusts a repo, and whether a worktree goes by its main checkout. Where Claude and Grok keep a folder's trust is T6895's.
4. Claude's project folder name under `projects/` for `aggy permissions suggest` (`transcript.ClaudeProjectDir`, already marked unverified), and that the fixture in `internal/suggest/testdata` still fits a current session file.

Facts used, with sources: Claude drops "Blanket `Bash(*)` or `PowerShell(*)`", "Wildcarded interpreters like `Bash(python*)`", "Package-manager run commands", "`Agent` allow rules", and `Monitor` rules on entering auto mode, while "Narrow rules like `Bash(npm test)` stay in effect" (`permission-modes.md`, "How the classifier evaluates actions"). `Bash(ls:*)` means `Bash(ls *)`, and "A `*` at the end, with a space before it, also matches the bare command" (`permissions.md`, "Wildcard patterns"). Grok reads `~/.claude/settings.json` and a project's `settings.json` and `settings.local.json`, and `defaultMode` from them; `--allow RULE` and `--deny RULE` repeat; `grok --permission-mode auto` sets the mode (`user-guide/22-permissions-and-safety.md`, grok-build `2bdd1d6a`). Grok matches a Bash rule by `matches_command_prefix`, `cmd == pattern` or `cmd` starting with `pattern` and a space, so a prefix with a word boundary, or else as a `glob::Pattern` over the whole command, where `?` and `[...]` are wildcards (`bash_command_matches_pattern`, `policy.rs:648`, same version); a guide line that reads otherwise was corrected against this source. Both flags split on commas (`value_delimiter`, `src/app/cli.rs`). `Task` as another name for `Agent` is from Claude's docs; the extra names in aggy's interpreter and runner lists are its own reading and not vendor facts. ripgrep's `--pre` and `--hostname-bin` run a program (`rg --help`, 15.2.0). Unverified: whether a Grok Bash rule stops at a redirect. A live run of `grok --allow 'Bash(cat *)'` where the model tries `cat x > y` settles it, and until then aggy sends Grok no wildcard Bash rule. Claude's rule matching of a tool name with other capitals, such as `bash(*)`, is unverified, so aggy treats it as the same tool for dropping and never as read-only.

## Approvals across CLIs (T6914)

Source only, read 2026-11-07. No real CLI was run for this task, and none of this has been checked live.

- **Grok.** An `allowed_bash_commands` entry holding `*`, `?`, or `[` is a literal to Grok (`state.rs`) and would be a wildcard in Claude's rules, so aggy names and skips it. Grants are in `sessions/<encoded root>/permission.toml` under its home (`user-guide/22-permissions-and-safety.md`, "Persistence Is Per Project", grok-build `2bdd1d6a`). The folder is the repo root, or the launch folder outside a repo, and each worktree has its own (`permission_scope_root`, `xai-grok-workspace/src/permission/state.rs`). The name is the root URL-encoded by the `urlencoding` crate, or a blake3 form past 255 bytes (`encode_cwd_dirname`, `xai-grok-config/src/paths.rs`); aggy computes only the first and names a path too long. Fields, from `PermissionState` in `state.rs`: `allow_bash_execute`, `allowed_bash_commands` (literal prefixes), `disallowed_bash_commands`, `allowed_bash_globs`, `allowed_web_fetch_domains`, `disallowed_web_fetch_domains`, `allowed_mcp_tools`, `allowed_mcp_servers`, `disallowed_mcp_tools`. The guide says dangerous commands never honor a remembered prefix, and aggy does not hold that list, so an approved command carries as the exact command. How Grok's MCP names map to Claude's is unverified, so those are named and not carried. A `permission_<client>.toml` variant is not read. A live check should make one grant of each kind, read the file, and confirm the folder name and fields.
- **Codex.** Approvals append to `rules/default.rules` (`DEFAULT_POLICY_FILE`, `core/src/exec_policy.rs` at `rust-v0.164.0`) as `prefix_rule(pattern=[...], decision="allow")`, its tokens JSON strings (`blocking_append_allow_prefix_rule`, `execpolicy/src/amend.rs`). aggy parses that one-line shape as text and never runs it; a rule written by hand in another shape is named. A live check should approve a command with "don't ask again" and read the line back.
- **Claude.** "Yes, and don't ask again" saves to `.claude/settings.local.json` at the git repository root, "resolved through worktrees to the main checkout" (`permissions.md`, read 2026-11-07).
- **OpenCode.** Always allow rows are in `opencode.db`, beside its sign-ins, which aggy never opens.

## Switching, known limits left as they are (T6895, batch 4 review, 2026-11-07)

From the adversarial review of T6897 to T6902. All fail on the safe side or stay
small; each has a `TODO(T6895)` or is noted here, and none has been run against a
real CLI.

- Unknown terminal replies (XTQMODKEYS, XTSMGRAPHICS) and a whole reply at the head
  of a burst past `maxHeld` count as typed: the prompt stays in doubt.
- A click counts as input and types nothing, so a click that fills the composer or
  starts a turn is seen only through `inputSinceTurn`.
- A CLI that exits 130 on its own after Ctrl+C reads as killed by a signal, so a
  declined dialog closed that way ends aggy rather than going back.
- A session file expired before aggy recorded `file_seen` reads as never started.
- A resumed session's "unchanged since launch" still compares file time with
  aggy's clock, so skew can read as never started. That only goes back: a resumed
  row is never removed.

- A refused move leaves the row on the id it had, since `settle` needs a followed
  id: a session reached that way is not what a later resume opens.
- A paste that spans a prompt of aggy's: `strip` and the key filter each follow the
  markers, so the first read's bytes are seen twice; harmless so far.
- The doubt note's words, and its second pick, have not been tried on a real CLI.
- Claude's local commands (`adapter.LocalCommand`) come from `commands.md`, read
  2026-11-07: rows not marked Skill, each describing a dialog or a display. That
  none starts a turn, and that a user's skill cannot take a name, is not checked live.
  That Claude's `/compact` ends a turn (so no doubt follows) is unverified.
- An interrupted turn (T6968): Claude's `Stop` "does not run if the stoppage
  occurred due to a user interrupt" (`hooks.md`, read 2026-11-10), and the docs
  name no other hook for it; Grok's `StopCancelled` carries `reason`
  `user_interrupt` (`user-guide/10-hooks.md` at `2bdd1d6a`); nothing is recorded
  for Codex or OpenCode. aggy takes a lone Esc during a turn, then 2 seconds of
  quiet, as a turn that may have ended, for a waiting switch only, in doubt.
  Live check (owner): under `aggy run claude`, send a message that runs long,
  press Esc while it runs, press `Ctrl+\` and pick another vendor. Expect,
  about two seconds after Claude goes quiet, the status line with `can't tell if
  the prompt is empty`, and no `/exit` until the switch key. Then the same with
  the first turn of a fresh session: expect the list's quit-by-hand words and no
  status line. On Grok, confirm that Esc sends `StopCancelled` (aggy's diag log
  shows the counted turn end). Also check that Claude prints nothing for 2
  seconds after its interrupt, and that a long silent tool call with no Esc
  never brings the line.

Read from the Codex source at 6aaac694, not seen live: `SessionStart` hooks run
from `run_turn` (`core/src/session/turn.rs#L340`); idle, Tab does not submit a `!`
command, and during a turn it queues one (`chat_composer.rs#L3531`, tests at L10170
and L9893); the backtrack (`tui/src/app_backtrack.rs`, header) and its preview keys
(`app_backtrack/legacy_input.rs#L195`, from the reviewer's reading, not rechecked);
`SessionMeta.cwd` (`protocol/src/protocol.rs#L3275`). A `$` popup taking Tab
(`chat_composer.rs#L2255`) comes from the reviewer's reading and was not rechecked.

## Still open

1. Codex limit event, captured live from an exhausted account: the
   rollout lines and the exit code.
2. Codex hook firing and payload, after trusting a hook by hand.
3. Answered 2026-10-29 (above): Grok resume needs `--agent` again and
   keeps the first launch's `--rules`.
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
9. The Codex edit child's `projects` override (T6892): the config file
   stays the same across a real edit borrow.
10. aggy's installed hooks firing live in each CLI (T6616 for Codex).
   How each runs the command is source- or doc-verified (Claude `sh -c`,
   Codex `$SHELL -lc`, Grok "an inline shell command"); what Grok does
   with a hook that cannot start is not stated (aggy's command exits 0
   either way).

## Surviving vendor updates (T6929)

Source only, at Codex `6aaac69`. No real CLI was run for this task, and none of this has been checked live.

- `codex features list` loads config and prints each entry of `FEATURES` as `name  stage  enabled`, sorted, and makes no model request on that path ([main.rs#L1921](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/cli/src/main.rs#L1921) to #L1953). Stages include the two-word `under development`, and one key holds a dot (`guardianv2.thread_context`), both in [features/src/lib.rs](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/features/src/lib.rs). `codex features --help` is one of the help runs above, so the subcommand is confirmed; its output is not.
- That load goes through [cli/src/cloud_config.rs](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/cli/src/cloud_config.rs), which builds a cloud config loader from the CLI's own auth, so a signed-in CLI may contact its vendor. Whether it does for a given account was not traced. Only the vendor's CLI does this, and aggy passes it no credentials.
- The command loads config with default loader overrides and has no `--ignore-user-config`, which is `exec`-only ([exec/src/cli.rs#L45](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/exec/src/cli.rs#L45)), so its `enabled` column is not exactly what a child with that flag gets. aggy cannot point it at an empty `CODEX_HOME`: `execrun.Env` refuses to override a vendor home. It runs in an empty dir, so no project config counts.
- `default_enabled: true` at this commit, 62 of the 182 entries, is `codex.featuresOn`; `secret_auth_storage` is on only on Windows. Lockdown's eight are all among them.
- The daemon has its own `codex app-server daemon version` ([main.rs#L2441](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/cli/src/main.rs#L2441)), and `--no-daemon` keeps an on-screen session off it.

A maintainer's live check should settle, on the current CLIs:

1. `codex features list` from a clean config: every row parses, and nothing on is outside `codex.featuresOn`.
2. `claude --help` and `grok --help` list every flag in `LockdownCaps`.
3. The self-test prompt under Claude's review flags: does it refuse `../canary.txt` and reply `NO ACCESS`? Until this is known, a new Claude version may be passed over on every update. Then again with a `Read` allow rule, or `permissions.additionalDirectories`, for that dir in a test config's `settings.json`: does the child read it?
4. Under the same prompt on Codex and Grok: the reply, the item types in Codex's stream, and whether Grok's `chat_history.jsonl` is there with `tool_calls` as expected.
5. Whether a refused patch under the read-only lockdown can report a Codex `file_change` as `completed`, which would fail every new Codex's self-test for good on that version. Source says no at `rust-v0.164.0`, not checked live: a patch that lockdown refuses emits no item at all, since the safety check rejects it (`core/src/safety.rs:113-114`, `core/src/apply_patch.rs:58`) in the handler (`tools/handlers/apply_patch.rs:524`) before `emitter.begin` (`:543`), and a declined patch folds to `failed` (`exec/src/event_processor_with_jsonl_output.rs:198-203`). aggy counts only `completed` as written.
6. Which folder under `sessions/` holds Grok's history when the cwd is a macOS temp dir, and how a refused read looks in its `tool_result` lines. Source at grok-build `2bdd1d6a` wraps the policy's reason as a message naming the tool and saying it was not executed (`xai-grok-shell/src/session/acp_session_impl/tool_calls.rs:1925-1926`); aggy's `RunNotes` takes that and the bare reason, and 1.0.77's exact text is unverified.

## OpenCode under aggy (2026-11-07, T6932)

An agent ran these under the owner's approval for this work (at most 40 real OpenCode runs across T6930 and T6932; 25 were used before it). The real OpenCode 2.2.5 ran on the owner's xAI sign-in, model `xai/grok-build-0.1`, in temporary git repos, with aggy's state in a temporary folder. The driver checked before each launch that the cwd and `PWD` were inside the temporary folder, and read aggy's and its child's real cwd with `lsof` during each session. Real Claude, Codex, and Grok CLIs were not on `PATH`: the other side of each switch is the fake Claude.

**Runs: 10 of the 15 allowed here.** Counted: one `aggy setup`, six on-screen launches, three `session export`. Not counted separately: the `--version` and `--help` probes `aggy setup` and a launch make. `--auto` was used in one launch only, with harmless commands.

| Check | Result |
| --- | --- |
| `aggy setup` reads the real `--version` and `--help` | 2.2.5 recorded; every launch after it passed the capability check |
| Untrusted repo | `opencode mini`, and the note pointing to Claude Code |
| A real turn | The answer came from `AGENTS.md`, passed as a copy in `instructions` with project config off |
| Guard report | Reached the socket from the plugin through `aggy hook`, in each launch, in `mini` and on the full screen |
| Turn end | `session.execution.succeeded` reached the supervisor as a turn end naming the session |
| Switch away (mini) | `/exit` typed by aggy, exit 0, a brief holding both turns from `session export`, the fake Claude started |
| Switch to OpenCode | From the fake Claude; the session answered from the brief's task file with no tool |
| Guard refusal | `FOO=1; git status` refused (`hook.guard.refused`), in `mini` and under `--auto` |
| Policy deny | A read of `.env` was denied, and the decoy's text never reached the screen, in `mini` and under `--auto` |
| Permission | `touch` off the list asked; nothing ran; Reject left no file |
| Switch over a dialog | With a permission open, a chosen switch offered nothing for 10 s; after the Reject it was offered and went through |
| Trusted repo | `aggy trust --opencode` asked, recorded through the manifest; the alternate screen and mouse were on; a real turn |
| Switch away (full screen) | Picker over the alternate screen, `/exit`, a brief, the fake Claude |
| Full auto | `touch` ran with no key pressed |
| Closed terminal | aggy killed the session after the hang-up |

**Found live, and fixed in T6932:**

- On the full screen, `/exit` and Enter in one write leaves `/exit` in the composer with the slash menu open, and the switch times out. Sent 0.5 s apart it runs. `mini` takes either. aggy now pauses on OpenCode.
- An Esc or arrow key answering a dialog counted as composer text, so a switch then waited on "text you have not sent". Keys sent while a dialog is open now do not count.
- On the full screen, SIGTERM and SIGHUP did not end OpenCode within 10 s, and it ran on with no terminal after the driver's terminal closed, for over 6 minutes before it was killed by hand. aggy now sends SIGKILL to the group 10 s after a hang-up, on OpenCode only. A SIGTERM to aggy is still only forwarded.

**Other live facts:**

- A rejected permission ends the turn with `session.execution.interrupted`.
- In `mini`, Esc on a dialog opens "Tell OpenCode what to do differently", and the dialog stays open until that is answered: typed text goes there, even `/exit`. On the full screen the same Esc rejected at once.
- Under `--auto`, `permission.asked` still fires and is answered at once with no key, so a dialog hold clears itself.

**Not run live:** the guard's `opencode auth export` refusal (never run against a real sign-in; unit tests and a process test with the fake's environment), a tool call outside the launch's folder and `/open`, the guard failing to load (unit test only), a real question form, real plan limits, a real Kitty-protocol terminal: the driver's terminal does not answer the keyboard query, so Ctrl+\ and Ctrl+U as CSI-u are for the owner's terminal to check.

### Review fixes (2026-11-07, T6932)

**Runs: 5 more, on the same terms, for 40 of 40.** Four `opencode run` launches on a fake model in a temporary `HOME` with no sign-in (the first two failed with `provider.no-route` on a config key of the driver's: `providers`, not `provider`), and one on-screen launch on the owner's xAI sign-in.

- **Live, fake model.** With an ask on `*` and an allow on exactly `git status`, a model's `for PATH in ./bin; do git status; done` ran the planted `./bin/git`, exit 0, with no prompt. With aggy's plugin added, the guard refused it, and the planted git did not run.
- **Source, run directly.** `packages/core/src/shell/scan.ts` at `v2.2.5`, run under Node with its first line removed and no vendor CLI, gives these only `git status` as a resource: bare assignments anywhere, including in `$(...)`, `( )`, `{ }`, `if`, `case`, and a `while` or `until` condition; `PATH[0]=`, `FOO=(a b)`; a declaration word; a `for` or `select` variable; `for (( ))`; and `(( ))` in the bash and zsh dialects. `read`, `printf -v`, `getopts`, `FOO=1 export X=1`, `command export`, and `${X:=1}` are resources, so they ask. `alias` and `foreach` are opaque, so they ask.
- **Real shells.** `TestGuardAgainstRealShells` runs each shape through bash, zsh, sh, and dash where present, with a planted git; any that runs it must be refused.
- **Live, the owner's sign-in.** After these fixes, on the full screen in a trusted temporary repo: a real turn, the guard's report, and a switch with no brief: `/exit` and its later Enter came through the session's loop, no exit timeout, and the fake Claude started.

### Guard scan and last review fixes (2026-11-07, T6932)

**Runs: none.** The 40 real runs were used. Nothing below has run against the real OpenCode.

- **The guard's raw-text scan** (`internal/hook/opencode.go`) replaced the word parser. It is checked by unit tests and by `TestGuardAgainstRealShells` only, against bash, zsh, sh, and dash with a planted git. The live refusal recorded above was by the parser it replaced, so **the scan's refusal reaching the model through the plugin is unverified**. So is the plugin's new `auto` option reaching the guard on a real launch; the process test runs the guard with the fake's environment.
- **Source, for skipping the scan under `--auto`.** `tui/src/context/permission.tsx` at `v2.2.5` returns `autoaccept` whenever `args.auto` is set, so the flag cannot be turned off during a session; `component/dialog-config.tsx` offers `session.permissions` as a setting, so a session started without the flag can be put on auto, and then keeps the scan. Reasoned from `core/src/shell/scan.ts`, not run: denies match each simple command's text, which a declaration does not change.
- **`aggy opencode`** ran against the fake CLI only (`TestOpenCodeCommand`, in `test/e2e`).
- **Unverified, for the owner's terminal.** The backspaces that take `/exit` back when keys are typed or Esc is pressed in the 0.5 s gap have never run on the real screen, only against the fake. On a real session: start a switch, type a letter in the gap, and check that the prompt holds only that letter; then again with Esc on the list.
- **`--direct` cannot signal the group.** The CLI shares aggy's process group there, for the terminal's job control, so the kill 10 s after a signal reaches the CLI alone, and its tool shells may run on. The pass-through's kill reaches the group.

### The scan runs only beside an allow rule (2026-11-07, T6932)

**Runs: none.** Unit tests, real shells, and source at `v2.2.5` only.

- **Source.** A resource is a raw slice of the command (`core/src/shell/scan.ts`), matched by `Wildcard.match` with no whitespace rule (`core/src/util/wildcard.ts`), which only turns `\` into `/`. So only a command spelled exactly as a rule matches it.
- **Source.** Always allow saves the request's `save` patterns per project (`core/src/permission.ts`, in `opencode.db`), and they apply to later sessions. `permission.replied` carries `reply` (`schema/src/permission.ts`).
- **Unverified.** That `permission.replied` reaches the plugin with `reply: "always"`, and that the plugin's `always` state reaches the guard on a real session. The mark and the guard reading it are tested through `aggy hook`; `server.js` has not run since this change. A next command in the moment after the click may run before the plugin hears of it.
- **Source.** On `opencode mini`, `-m` sets the model of a resumed session too (`cli/src/mini.ts`), so `aggy opencode --continue --model` does what `aggy claude --continue --model` does there. Not run.
- **Reviewer's check.** `export DEVELOPER_DIR=./dev; git status` ran a planted `dev/usr/bin/xcrun` through stock macOS `/usr/bin/git`, in bash and zsh. `BASH_CMDS[git]=` follows the bash manual for bash 4 and later; macOS's 3.2 cannot confirm it. Neither needs the real OpenCode.

## OpenCode's free models (T6956, 2026-11-10)

**Runs: none.** No real OpenCode ran for this task: unit tests, the fake CLIs, and OpenCode's public docs only. `[opencode] free_models` (docs/design.md, section 15) adds `opencode` to a session's `provider.use` allow-list.

- **Docs** ([opencode.ai/docs/zen](https://opencode.ai/docs/zen), read 2026-11-10; the page is titled "Models" and calls the service OpenCode Console). Models are named `opencode/<model-id>`. The page lists free models by id and says some are free "for a limited time". On data: "Our providers follow a zero-retention policy and do not use your data for model training", with exceptions named per model, such as "During its free period, collected data may be used to improve the model" and, for one, "Its provider retains your data and may use it for model training". It also says "To hide free models in OpenCode, set `OPENCODE_DISABLE_FREE_MODELS=1`", which aggy does not use. The page links no OpenCode terms of service; it names each model's provider and links some of their terms and policies. It says to "Sign in, add credits, and get an API key" for its models. aggy's note says only that the setting allows the whole provider, so paid models become selectable too and spend OpenCode credit if the person has any; that prompts and code go to OpenCode's own service under OpenCode's and each model provider's terms; and that some free models' data may be kept or used to improve or train the model, pointing to this page.
- **Unverified, live check owed:** which free models a real OpenCode offers (the docs' list changes, and some are limited by region).
- **Unverified, live check owed:** that `opencode/*` free models appear in `opencode models` under aggy's per-launch config with `free_models` on, and not with it off. `opencode models` respecting `provider.use` was live for other providers (this file, "OpenCode 2.2.5 as an on-screen harness").
- **Unverified, live check owed:** whether OpenCode's small or title model goes to the `opencode` provider when the setting is on while the session uses another provider.
- **Unverified:** whether paid `opencode/*` models show without OpenCode credit or a Console key; the setting allows the whole provider. OpenCode's general terms of service and privacy policy were not read.

## Claude Code's folder trust (T6977, live, 2026-11-10)

**Run: the owner, Claude Code 2.1.322, macOS, a throwaway config folder** (`CLAUDE_CONFIG_DIR` set to an empty temporary folder; the real config was not read or changed; no permission- or trust-skipping flag).

- **Live:** Claude keeps a folder's trust in `.claude.json` at the top of its config folder (`$CLAUDE_CONFIG_DIR/.claude.json` when that is set; `~/.claude.json` otherwise, as for MCP servers), under `projects["<absolute folder>"].hasTrustDialogAccepted`. With that key written `true` ahead of the run, Claude did not show its trust screen in that folder; with it `false`, it showed it; cancelling the screen recorded nothing.
- **What aggy does with it:** the trust list's Claude Code row (docs/design.md, section 10, "Trust") sets that one key for the repo's work-tree root, by its real path, through the manifest: a minimal text edit, re-read and hash-checked just before the write, checked to decode to the old document plus that key, with no backup of the file (it holds Claude's history and account), and never the file's content in a preview or log. aggy unwire puts back just that key and keeps what Claude wrote since. A file that is not there is not made, and one that does not parse is left as it is.
- **Unverified, live check owed:** that a signed-in Claude honours the key written ahead the same way (the check was signed out, in a fresh config folder); whether a trusted folder covers its subfolders (aggy writes the work-tree root only, so a start in a subfolder may still show Claude's screen once); how Claude spells the folder (aggy writes the real path, so `/tmp` and `/private/tmp` on macOS, or a folder reached through a link, may differ); and whether a running Claude overwrites a key written meanwhile when it next saves the file (aggy writes before the CLI it starts, and another Claude already running may still rewrite the file).
- **Unverified: whether Claude locks `.claude.json`, or re-reads it before it writes** (PR #130 review). No lock file was seen beside it in the run above, which proves nothing either way. aggy takes no lock of Claude's, so a Claude write between aggy's re-read and its rename is lost. aggy therefore never writes the key while a session it runs is on screen: `/aggy trust` records aggy's own answer at once and leaves Claude's key for the next start in that folder, before the CLI starts (docs/design.md, section 10, "Trust"). A Claude started outside aggy may still be running then. A live check would be: with a scratch `CLAUDE_CONFIG_DIR`, run Claude, change an unrelated key in the file from outside, make Claude save its config (change a setting), and see whether the outside change survives.

## Grok reviews under `$HOME` (T6922)

From the agent audit of 2026-11-06, run under the owner's M7 approval (finding F9), grok 1.0.77, in a repo under `<HOME>`: `read_file` and `list_dir` were refused by Grok's permission policy, which named aggy's `~*/**` deny rule. The review still came back, from the diff, and its report did not mention the denial. It fails closed. This task did not repeat the run and took these results from the finding. Unverified: that the run used aggy's exact review argv. `grep` was not seen; it has the same rule. aggy keeps the rules, since they stop `~` spellings of the Grok home (source: `policy/path_match.rs:91-112`, `policy.rs:2221`), and the report now says so. S4 shows Grok prints a rule on `<HOME>/Library/**` in its `~` form, which fits, but why a physical repo path matches `~*/**` was not traced in the source. No live run has shown the new report line.

## Grok image `--edit` (T6923)

From the agent audit of 2026-11-06, run under the owner's M7 approval (finding F10), grok 1.0.77: `aggy image --edit` of a Grok image failed in about a second on 0 tokens, because the session's folder no longer exists, and Codex `--edit` worked. This task did not repeat the run and took these results from the finding. Unverified: that `grok_file_tools` was on (inferred, since aggy refuses such an edit without it), and the exact cause beyond that, that the session's folder is kept under the first run's cwd and stops resolving from a new temp dir (inferred from the finding and `sessions/<encoded cwd>/`; not traced in the source). aggy now refuses before any child (`borrow.grok_edit`). Unverified, and not tried: `aggy resume` remakes the same temp path for an on-screen session, and a headless edit might work the same way; that would change what the borrow's temp dir promises, so it is not done.

## A video that made no file (T6924)

From finding F21, passed on in this task's brief and not repeated here: on an account with zero data retention, `image_to_video` fails, and aggy reported outcome unknown and that the child left no file that passed the artifact gate; the reason was only in the child's text. aggy now shows the start of that text, scrubbed, in the short report and the inbox report. Fakes only: no live run has shown the new lines.

## Grok's flags (T6952)

Live, 2026-11-07, `grok --version` and `grok --help` only, in an empty temp folder, no turn: `grok 1.0.77 (50ca579fa996) [stable]`, exit 0, no path in the output. The options it lists:

`--add-dir <DIR>`, `--agent <NAME>`, `--agents <JSON>`, `--allow <RULE>` (aliases `--allowedTools`, `--allowed-tools`), `--always-approve` ("Auto-approve all tool executions"), `-c`/`--continue`, `--cwd <CWD>`, `--debug`, `--debug-file <FILE>`, `--deny <RULE>` (alias `--disallowedTools`), `--disable-web-search`, `--disallowed-tools <TOOLS>`, `--fork-session`, `--fullscreen`, `-h`/`--help`, `--include-partial-messages`, `--json-schema <SCHEMA>`, `--leader-socket <PATH>`, `-m`/`--model <MODEL>`, `--max-turns <N>`, `--minimal`, `--no-alt-screen`, `--no-plan`, `--no-subagents`, `--oauth`, `--output-format <OUTPUT_FORMAT>` (plain, json, streaming-json, streaming-messages-json), `-p`/`--single <PROMPT>`, `--permission-mode <MODE>` (default, acceptEdits, auto, dontAsk, bypassPermissions, plan), `--prompt-file <PATH>`, `--prompt-json <JSON>`, `-r`/`--resume [<SESSION_ID_OR_TITLE>]`, `--reasoning-effort <EFFORT>` (alias `--effort`), `--restore-code`, `--rules <RULES>`, `-s`/`--session-id <SESSION_ID>`, `--sandbox <PROFILE>` (env `GROK_SANDBOX`), `--system-prompt-override <PROMPT>` (alias `--system-prompt`), `--tools <TOOLS>`, `-v`/`--version`, `--verbatim`, `-w`/`--worktree [<WORKTREE>]`, `--worktree-ref <WORKTREE_REF>` (alias `--ref`).

Hidden flags and aliases, from `PagerArgs` in `crates/codegen/xai-grok-pager/src/app/cli.rs` at grok-build `2bdd1d6a` (source, not live): `--trust` with alias `--trust-folder` (hidden; "persist the decision to the trust store"), `--always-approve` with aliases `--yolo` and `--dangerously-skip-permissions`, `--single` with alias `--print`, `--rules` with alias `--append-system-prompt`, `--load` (hidden, an alias of `--resume`), and hidden `--installer` (sets a field in `config.toml`), `--fs-read`, `--fs-write`, `--no-ask-user`, `--storage-mode`, `--compaction-mode`, `--compaction-detail`, `--hunk-tracker-mode`, `--client-identifier`, `--background-wait-timeout`, and more. The repo publishes no tags and does not have commit `50ca579fa996`, the one `grok --version` names, and `2bdd1d6a` (2026-09-29) has no `--add-dir`, which help at 1.0.77 lists. So the binary is newer than that tree, and a hidden flag added since is not known.

What it settles: `--auto-approve` is not listed, and `--always-approve` is, so the earlier "unverified guess" on it is closed; `--yolo` is an alias of it in source and is left out of help. `-c`, `-m`, and `-s` are `--continue`, `--model`, and `--session-id`, so `aggy grok -c` used to bypass aggy's index and now does not. Not run: whether the CLI refuses an unlisted flag (only `--help` and `--version` were allowed here; `PagerArgs` is a clap parser, which would); what `--sandbox` profiles exist beyond `strict`; what `--agents` with a `permissionMode` does.

Sources read for the same task, not live: Claude's `--safe-mode` says hooks, MCP servers, and CLAUDE.md do not load and "permissions work normally" (cli-reference, 2026-11-07), which does not say whether hooks given with `--settings` count; `manual` is an alias of `default` (permission-modes); "Deny rules block in every mode" (permission-modes) and "An allow rule can't carve an exception out of a deny rule" (permissions). Codex: `--disable` "translates to `-c features.<name>=false`" (developer-commands), `hooks` with the old name `codex_hooks` ([features/src/legacy.rs](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/features/src/legacy.rs)), and `--ask-for-approval` takes only `on-request` and `never` at that commit ([approval_mode_cli_arg.rs](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/utils/cli/src/approval_mode_cli_arg.rs)), so `-a untrusted` passes aggy and the CLI refuses it there.

A maintainer's live check should settle: under `claude --safe-mode` with aggy's settings file, do its hooks fire? Under `codex --profile`, and in an `--add-dir` folder, does aggy's rules file still forbid a command? In a resumed edit child's worktree, which of `CLAUDE.md`, `AGENTS.md`, and `.claude/settings.json` load for each CLI?

## Hermes Agent 0.23.1 (T6933, live 2026-11-10)

**Runs.** On a personal Mac (macOS 15.5), Hermes Agent 0.23.1 (source at hermes-agent `v0.23.1`, commit 3b42157858), signed in with xAI OAuth, as handed to T6933 in its fact sheet; not on the machine aggy is built on, where Hermes, `uv`, and its Python were never run. No credential file and no `state.db` was opened. Below, only what was seen, scrubbed: no session id, path with a user name, or model output. aggy itself did not run against the real Hermes: everything aggy does with it is tested against fake-hermes (`testdata/fakes/fake/hermes.go`), built from the source.

- **Live:** `hermes --version`'s first line is `Hermes Agent v0.23.1 (2026.9.24) · stable · local <hash>`, followed by the install folder, install method, Python and OpenAI SDK versions, and `Up to date` (a network update check; `updates.check: false` turns the launch check off, `hermes_cli/source_check.py:570`, source).
- **Live:** `hermes --help` lists `-z`, `--usage-file`, `--cpu-profile`, `-m`, `--provider`, `--reasoning`, `-t`, `--resume`, `--no-restore-cwd`, `--in`, `--continue`, `--worktree`, `--accept-hooks`, `--skills`, `--yolo`, `--pass-session-id`, `--checkpoints`, `--ignore-user-config`, `--ignore-rules`, `--safe-mode`, `--tui`, `--native`, `--cli`, and `--dev`, and the subcommands `chat`, `model`, `auth`, `config`, `profile`, `sessions`, `hooks`, and `gateway`, among others.
- **Live:** `hermes chat -q "<text>" -Q` prints the answer, a blank line, and `session_id: <id>`, and exits 0. After it exited, the gateway daemon (`python -m hermes_cli.main --profile default gateway run --quiet --idle...`) was still running, its parent under launchd with ppid 1; it idles out after 600 s (`gateway/run_idle_exit.py`, source). Creating a second profile did not start a second daemon: one serves every profile.
- **Live:** `hermes profile create <name> --no-alias --no-skills` made a profile that saw the root's OAuth sign-in and ran a query; `hermes profile delete <name> --yes` removed it. A plain profile's defaults: `approvals.mode: smart`, `cron_mode: deny`, `single_query_mode: deny`, `unattended_mode: deny`.
- **Live:** `hermes config get <key>` prints the value, and `hermes config set auth.adopt_external_logins false` works. `--json` on `config get` is from source (`hermes_cli/subcommands/config.py:20-25`), not run.
- **Live:** `hermes auth list` shows the provider with one credential; `hermes sessions list`'s columns are Preview, Workspace, Last Active, Src (`oneshot`, `cli`), and ID.
- **Live (one `-q` query that used the terminal tool, with four logging hooks):** `pre_tool_call`, `post_tool_call`, `post_llm_call`, and `on_session_end` each fired once, in that order. `tool_name` was `terminal` and `tool_input` had the key `command`. Every payload had the same `session_id` and a `profile`. `cwd` was Hermes' install folder, not the session's: the hook runs in the daemon. `post_llm_call`'s `extra` carries the reply and the conversation; `on_session_end`'s has `completed`, `failed`, `interrupted`, and `turn_exit`.

**Unverified, live checks owed** (on the Mac mini, the owner's to run; aggy is built for them but has not met a real Hermes):

- `aggy run hermes` end to end: the classic `--cli` screen in aggy's pseudo-terminal (escape sequences, Ctrl-Z, resize), Hermes' first-run screens (the telemetry consent, `hermes_cli/observability/shared_metrics_consent.py`; the setup guard with no provider, `main.py:1617-1619`), and `/exit` at the prompt.
- The hook consent prompt without `--accept-hooks`, for aggy's two hooks, and that the answer holds at the next launch in the same repo.
- That hooks fire for a `--cli` session's tool calls in the daemon, as for `-q`, and that `on_session_end` fires once per interactive turn.
- A `pre_tool_call` block (`{"action":"block"}`), `fail_closed` on a hook that fails or hangs, and `{"action":"approve"}` with a `rule_key` reaching the approval prompt, and what Always does with that key.
- `approvals.deny` refusing a matching terminal command under `approvals.mode: manual`.
- `hermes config get <key> --json` output for a string, a boolean, and `hooks`.
- `hermes sessions export - --session-id <id> --format jsonl --redact` on a real session (the test fixture is hand-built from the source).
- Whether `--query-file` and `HERMES_EPHEMERAL_SYSTEM_PROMPT` reach a resumed session.
- `HERMES_HOME` set to a profile, and `HERMES_DATA_DIR_SUFFIX`, as `safefs` reads them.
