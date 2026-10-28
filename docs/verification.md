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
| Launch-time MCP | Claude `--mcp-config` (re-pass on resume); Codex `-c mcp_servers.*` plus per-server approval; Grok has no flag, use `--agent <file>` with `mcpServers`. | Live-confirmed |
| Codex CLI details | `codex exec resume`, `--ignore-user-config` on `exec` only, greedy `-i`. | Help/Source-verified; parts Live-confirmed |
| API and route settings | Listed in adapters.md. | Source/Doc-verified |

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
when the review root holds any of these files or dirs except the
instruction and skill ones, which are prompt text
(`internal/borrow/grok_trust.go`). Grok 1.0.70's folder-trust behaviour
is unverified; the live check is TODO(T6649).

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
| Grok via agent file | `grok -p "Call aggy_ping..." --agent ../agent.md --allow 'MCPTool(aggy__aggy_ping)' --output-format json`, with `mcpServers` in the agent file's frontmatter | Confirmed | Returned `<token>`. Grok has no per-session MCP flag. Resume with the same agent file was not tested. |

Also (Doc/Source-verified): Claude subagents can declare inline
`mcpServers` that connect only while they run; Grok user agents can too,
with `mcpInheritance`; Codex roles cannot carry MCP servers and children
inherit the parent's. Claude and Grok defer MCP tool definitions (names
only, or `search_tool`/`use_tool`); Codex defers only when the model
supports tool search.

A Claude run with `--strict-mcp-config --mcp-config` on
`{"mcpServers":{}}` had no MCP or claude.ai connector tools at all
(2026-10-28); built-in file tools stay unless `--tools` trims them.

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
7. Grok 1.0.70 folder trust in a headless review (TODO(T6649)): a scratch
   repo whose `.claude/settings.json` and `.grok/hooks` hold SessionStart
   and PreToolUse hooks that touch a marker, never trusted, run with
   aggy's exact Grok review argv; the marker must stay absent.
