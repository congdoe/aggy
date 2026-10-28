# aggy design

This is the public design of aggy. It says what aggy is, why it exists, and how it works. For the vendor adapter contract, see [adapters.md](adapters.md).

**Status.** This is the target design, and aggy is pre-alpha. Today the code has the foundation packages (`safefs`, `patch`, `store`, `config`, `home`) and the vendor `Adapter` interface, and the `aggy` binary implements only `version` and `help`. Everything else described here is planned. Section 18 shows which milestone delivers each part and its status.

## 1. What aggy is

aggy is one command that opens a real Claude Code, Codex, or Grok Build session on that vendor's subscription. You run `aggy` instead of `claude`, `codex`, or `grok`. The program on screen is always the official CLI, with its own tools, permissions, slash commands, and model picker.

aggy adds three things around those CLIs:

- **Switch.** Move the on-screen work to another vendor, for example when one hits its plan limit, with a handoff that does not depend on the vendor being left.
- **Borrow.** Send one bounded task to the vendor best suited to it (image, video, research, news, review, or edit) and get a short report back, without leaving the current session.
- **Parity.** Make every CLI see the same project rules, lessons, task state, and safety rules, so a switch lands in the same project instead of a stranger's.

Why: people who pay for more than one coding agent usually live in one of them. When that one runs out for the week, the others sit idle, and moving work by hand loses context. Some work also suits another vendor better: image and video generation, web research, and current news about tools. aggy lets that work go where it fits, using only the CLIs you already run.

## 2. Constraints

These are requirements, not preferences.

- **Official binaries only.** aggy launches the unmodified installed `claude`, `codex`, and `grok`.
- **Vendor sign-in only.** Each CLI signs in with its own flow: `claude` then `/login`, `codex login`, `grok login`.
- **No credentials.** aggy stores conversation ids, connection status, and its own index. Its goal is to never read, copy, or store OAuth tokens, session tokens, passwords, or credential files. A file-access guard enforces this for the vendor credential files (section 13).
- **No model requests.** Only the vendor CLIs talk to their vendors. aggy never calls a model to classify a task, summarize a transcript, or write a brief.
- **No proxy.** aggy never sits between a CLI and its vendor. Anything that sets `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, or `GROK_MODELS_BASE_URL`, and any compressor proxy in front of a CLI, is out. Those paths can bill an API key or sit in the middle of plan traffic.
- **Subscription, not API.** If `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, or `XAI_API_KEY` is set, setup warns that the CLI may bill the API instead of the subscription. Settings that route a CLI to an API key or another endpoint, such as Grok's `endpoints.xai_api_base_url` (which its image and video tools use) and Codex's `openai_base_url`, are flagged by setup and `aggy doctor`; aggy never sets them. [adapters.md](adapters.md) lists every one checked and its source.
- **No service, no telemetry.** The project runs no hosted service, relay, or account system, and aggy sends nothing to the project. Each user runs aggy on their own machine against their own subscriptions. aggy makes no model or vendor API calls of its own. The only network surfaces aggy itself opens are planned, opt-in ones (section 13).
- **Any subset of vendors.** Setup works with one, two, or three vendors. A skipped vendor is absent from switching, borrows, usage, and tool wiring. The default vendor must be a connected one.
- **Per-repo allowlist.** A repo can list the vendors allowed to see it. A brief, borrow, or switch never sends that repo's content to a vendor outside the list.
- **Turn boundaries.** Switches happen after the current turn finishes. An in-flight shell or test belongs to the vendor process and stops when that process exits.
- **Vendor state stays put.** Hidden reasoning, tool-call ids, permission prompts, and the vendor's own todo list stay in the vendor session that created them. They come back when that session is resumed. They do not move to another vendor.
- **One hop.** aggy refuses a borrow from inside a borrow; a child with a shell can still bypass it (see section 7). Native same-vendor subagents still run.
- **One writer.** Only one process edits the main checkout. Edit borrows and parallel editors get their own git worktrees. A worktree is not a sandbox: isolation from the rest of the disk comes from each CLI's own sandbox and permission settings.
- **macOS and Linux first.** Windows needs ConPTY and different sandbox behavior, so it comes after 1.0.

## 3. Concepts

| Term | Meaning |
| --- | --- |
| Vendor | `claude`, `codex`, or `grok`. Each has its own subscription, tools, and session store. |
| Parent | An aggy session. It has a title, a directory, a last-active time, and the vendor that was last on screen. Many parents can exist, and several can share a directory. |
| Child | A real vendor session started inside a parent, interactive or a borrow. aggy stores the vendor's session id and its last-active time, taken from the modified time of the vendor's session file so a crash or sleep does not lose it. |
| Task file | `.aggy/task.md` in the working directory: goal, done, next, decisions, files touched. The on-screen agent keeps it current at step boundaries. It is the main input to a brief. |
| Brief | Optional handoff text, written to a file the new child is told to read. Never a command-line argument. |
| Borrow | One bounded task run headless in another vendor. The on-screen agent stays put and gets a short report. |
| Kind | What a borrow is for: `image`, `video`, `research`, `news`, `review`, or `edit`. |
| Routing table | A per-kind ordered list of vendors in config. It is aggy's only routing logic. |

## 4. Parents and children

`aggy sessions` lists parents with their directory, last vendor, and last-active time.

- `aggy` with no arguments resumes the latest parent, or starts one when none exists.
- `aggy resume` reopens the latest parent. `aggy resume <id>` reopens that one.
- `aggy new` starts another parent in the current directory. The old one stays listed.
- Resume launches the child that was last on screen with that CLI's resume flag, and injects no brief unless `--brief` is set.
- If the vendor no longer has that session, aggy says so and keeps the parent's other children.
- Claude sessions are tied to their directory, so a child whose worktree was removed is marked closed, not resumable.

`aggy session` lists the children of the open parent: vendor, kind, title, model, last active. Borrows are children too. The same list is the picker when one vendor has several children.

Inside a harness, a linked skill prints either list. Each vendor owns its slash commands, so aggy does not add slash commands inside them.

Session ids per vendor, checked against claude 2.1.311, codex-cli 0.164.0, and grok 1.0.70 unless marked unconfirmed:

| Vendor | New session | Resume | Notes |
| --- | --- | --- | --- |
| Claude | `--session-id <uuid>` | `--resume <id>` | `--fork-session` branches instead of reusing the id. |
| Codex | No pre-assigned id | `codex resume <id>`, `codex exec resume <id>` | Headless, the first `codex exec --json` event is `thread.started` with the `thread_id`. Interactive, aggy watches `~/.codex/sessions/` for the new session file, which races when two Codex sessions start at once. A `SessionStart` hook reports the id as `session_id` (source-verified), which would avoid the race, but Codex runs a hook only once its hash is trusted, and a hook passed with `-c` is never trusted (section 11), so the folder watch stays as the fallback. `codex exec` reads stdin to EOF when it is not a terminal, so aggy never gives it an open pipe: headless Codex gets the prompt file as stdin, which ends at once (see adapters.md). |
| Grok | `--session-id <uuid>` | `--resume <id>`, also with `-p` | `--fork-session` branches. `grok -p --output-format json` returns `sessionId`. |

Only one process holds a vendor session at a time. The holder is recorded as a lease in the store. A borrow or event that targets a busy session waits for its turn to end, or uses `--fork` to branch instead. Two writers on one transcript corrupt it.

## 5. The supervisor and switching

aggy stays running as the parent process. The supervisor holds the vendor CLI in a pseudo-terminal and passes it straight through to your terminal. It draws no screen of its own. `exec` cannot work: once aggy replaces itself with the CLI, nothing is left to perform a switch, and a skill alone cannot cleanly end the CLI it runs in.

**The switch key.** `Ctrl+\` opens the switch menu. It is configurable, and pressing it twice sends it through to the CLI. It is not a known shortcut in any of the three CLIs; this is to be confirmed in M2. Claude already uses `Ctrl+B` and `Ctrl+]`, which rules out tmux's default prefix. It is set as `[switch] key = "ctrl+<k>"`, where `<k>` is a letter or one of `\ ] ^ _`; Ctrl+I, J, M (Tab and Enter) and Ctrl+[ (Esc) are refused. aggy recognizes it as its control byte or, once a CLI turns on the kitty keyboard protocol, as its CSI-u press report (`ESC [ 92 ; 5 u` for Ctrl+\, with or without Caps or Num Lock; a report split across two reads is passed through). Inside a bracketed paste it is data. A second press within 400 ms sends one switch key to the CLI; other input within that time opens the picker and goes to it. The picker pauses the pass-through both ways and draws on the terminal's alternate screen, so closing it puts the vendor's screen back without aggy redrawing it. It lists the vendors in `switch.order` that `aggy setup` recorded and did not find signed out, except the one on screen; Esc, Ctrl+C, or the switch key closes it. Picking a vendor does not switch yet (T6664).

**A switch:**

1. You press the switch key, a linked skill requests a switch, or a vendor reports its limit.
2. The supervisor waits for that vendor's end-of-turn hook, then types `/exit` into the session.
3. With `--brief`, the brief builder writes `.aggy/briefs/<child-id>.md`.
4. The adapter launches the next CLI inline and, with a brief, tells it to read the file.

`/vendor` is the switch command, used from the menu or a skill. It changes harness, not model.

- `/vendor grok` resumes the latest interactive Grok child in this parent, or starts one, and injects nothing.
- `/vendor grok --new` starts another Grok child and keeps the others listed.
- `/vendor grok --model <id>` overrides the model for one launch and does not write config. Inside a running harness, its own `/model` still changes models.
- Several children of one vendor open a picker limited to this parent.

**Scrollback.** Claude renders inline, and aggy launches Codex and Grok with `--no-alt-screen` (verified for Grok; for Codex, unverified, so passed only when a probe at launch finds it in `codex --help`), so the previous vendor's output stays in your terminal's scrollback after a switch. aggy does not redraw the thread. The transcript stays on disk for `aggy log` and later briefs.

**Terminal transparency.** The pass-through must carry window resize, bracketed paste, image paste, Shift+Enter and the kitty keyboard protocol, mouse, clipboard (OSC 52), links (OSC 8), notifications (OSC 9 and 777), truecolor and wide characters, and Ctrl+Z. A checklist covering these runs before each release in common terminals, with and without tmux. `aggy --direct` launches a CLI unwrapped for when the pass-through breaks something; the index is still updated through hooks.

**How the pass-through works.** `aggy run <vendor>` starts the CLI through `internal/execrun` (`StartPTY`) in a new pseudo-terminal at the user's terminal size, puts the user's terminal in raw mode, and copies bytes both ways unchanged. aggy parses nothing that passes through except the switch key. SIGWINCH passes the new size on; SIGINT, SIGQUIT, SIGTERM, and SIGHUP sent to aggy are forwarded to the CLI's process group; the CLI's exit code (128 plus the signal number when a signal ended it) becomes aggy's. The terminal is restored on every exit, on a panic, and around a suspend. Ctrl+Z is a byte for the CLI: the CLI leads its own session in the pseudo-terminal, so its process group is orphaned and the kernel discards a SIGTSTP it sends itself, as it does for a CLI run as a tmux pane's command. If the CLI is stopped anyway (SIGSTOP), or aggy gets SIGTSTP, aggy restores the terminal and stops, and on `fg` re-enters raw mode, passes on the size, and resumes the CLI. What each CLI shows on Ctrl+Z is on the release checklist (T6684).

The CLI gets the user's whole environment, since it is the user's own session; aggy adds only `AGGY_VENDOR` (the vendor on screen), `AGGY_SOCKET` (the supervisor socket), and `AGGY_PARENT` (the parent id, which names the socket), and `StartPTY` refuses any other variable. Its allowlist is separate from the borrow runner's: an on-screen CLI never gets `AGGY_DELEGATED`, and a borrow never gets the socket. aggy never adds a `*_BASE_URL`, an API key, or anything else.

**Dependencies.** The stdlib cannot open a pseudo-terminal or set raw mode without `ioctl` calls that the `syscall` and `unsafe` bans keep out of aggy, so two pinned modules do it. `github.com/creack/pty` v1.1.24 (MIT, no dependencies, no cgo on darwin or linux, the most used Go PTY package) opens the pseudo-terminal, starts an `exec.Cmd` in it, and sets its size. It opens `/dev/ptmx` itself, outside `safefs` (section 13). `golang.org/x/term` v0.47.0 (BSD-3-Clause, Go project) sets raw mode and reads the size. `github.com/aymanbagabas/go-pty` was rejected: it wraps creack/pty on Unix and pulls in `golang.org/x/crypto` and `u-root` for its SSH support. The Windows port (after 1.0) adds a ConPTY `StartPTY` behind a build tag, through creack/pty's Windows support or a small ConPTY file; the supervisor only sees the `Child` interface.

**The supervisor socket.** Hooks (`aggy hook <event>`) and linked skills reach the supervisor at `run/<parent-id>.sock` in the state directory: a Unix socket, mode 0600, in a directory aggy keeps at 0700 and owned by the user (a looser one is tightened, another owner's refused). It speaks newline-delimited JSON, one request per line and at most 64 KiB: `ping`, `hook-event` (vendor, event name, and the hook's JSON object), and `switch-request` (a stub until T6664). Unknown fields, a second value on a line, or a bad value get an error reply and the connection is closed; so does a connection idle for 10 seconds. Each connection's peer uid is read (`LOCAL_PEERCRED` on darwin, `SO_PEERCRED` on Linux) and anything but aggy's own uid is closed unanswered; elsewhere every connection is refused. While the supervisor runs it holds an flock on `run/<parent-id>.lock`; a new supervisor that gets the lock knows the old one is dead (an flock cannot be fooled by pid reuse) and removes its socket, and one that cannot is refused. Anything at the socket path that is not a socket is left alone. aggy removes both files when it exits.

**Limit handoff.** When the on-screen vendor reports its limit, aggy opens the switch picker with the first vendor in `[switch] order` that is connected and has quota left preselected. One key accepts, with a brief.

## 6. The brief

A brief is opt-in (`--brief`). It is built without a model, because the vendor being left may be out of quota. It holds:

- the task file, if present
- the latest turns verbatim, read from the vendor's transcript on disk
- `git status` and `git diff --stat`
- tools the previous harness had that this one lacks
- the path to the full transcript, which the new child reads on its own quota if it needs more

The first visit to a child gets all of that. A return to a child gets the task file plus only the turns other vendors added since it was last closed.

Lines matching secret patterns are removed. `--brief --show` previews it. The brief is a file, never an argument: 200,000 tokens is about 800 KB, and macOS `ARG_MAX` is 1 MB including the environment.

Grok bills a whole prompt at the long-context rate once it reaches 200,000 tokens. aggy keeps a Grok brief well under that by dropping older turns (not summarizing them) and pointing to the transcript. There is no local Grok tokenizer, so the count is a conservative character estimate. A session can still grow past the threshold after launch, so the lasting guard is Grok's own compaction setting.

When an adapter cannot read a transcript, for example after a format change, the brief degrades to the task file alone.

Two common uses:

- **Quota handoff.** `/vendor codex --new --brief` continues the same work on Codex. The Claude child stays listed.
- **Side trip.** `/vendor grok --new` opens a blank child for a small task. `/vendor claude --brief` returns to Claude and adds only what Grok did.

## 7. Borrows

From the on-screen agent, a linked skill runs `aggy borrow <kind> "<task>"` in the agent's own shell, in the background where the harness supports it. It is a shell command, not an MCP tool: long headless runs would hit MCP tool-call timeouts, and a shell command works the same in all three harnesses.

aggy routes the kind (section 8), runs that vendor's CLI headless (`claude -p`, `codex exec`, `grok -p`) with `AGGY_DELEGATED=1`, and returns a short report to the conversation and to `aggy inbox`.

- The borrow prompt is the task, the paths, the constraints, and today's date. It does not include the parent transcript. The child's exploration stays in that child.
- A borrow is a child, so it appears in `aggy session` and can be resumed with `/vendor`.
- Read-only kinds (`image`, `video`, `research`, `news`, `review`) never write the repo. Every kind but `review` runs in a fresh, empty temp directory with its CLI's lockdown flags (adapters.md). `review` runs at the root of the git work tree in plan mode: `--permission-mode plan` on Claude and Grok, `--sandbox read-only` with the full Codex lockdown on Codex, which leaves Codex no tool that reads a file. So every reviewer gets its input in the prompt: aggy runs `git diff` against the merge-base with the default branch (or `--base <ref>`) and reads the `--path` files, caps the total at 256 KiB, and fences it as data to review, not instructions. Claude and Grok keep their read tools at the work tree root. A review refuses `$HOME`, `/`, a directory outside a git work tree, and a root around aggy's state dir.
- `edit` gets its own git worktree and never writes the main checkout. Its diff passes `aggy check` before you see it.
- Each kind has a time limit and, where the CLI supports one, a turn limit. aggy kills a borrow's whole process group past its time limit, or when `aggy borrow` gets SIGINT, SIGTERM, or SIGHUP (recorded as `interrupted`), and runs at most 2 borrows per vendor and 4 in total, counted across processes through store leases.
- `--vendor <name>` replaces the routing list for one borrow; the skip rules of section 8 still apply to it. `--vendor both` asks two vendors, for research only.
- A CLI whose version is outside the versions the adapter was tested on is refused for borrows unless `--allow-untested` is passed: the lockdown flags were verified only there.

**One hop.** The parent's `aggy borrow` works and the child's copy refuses. A child with a shell could still run `claude -p`, `codex exec`, or `grok -p` directly. Delegated children get a `PreToolUse` hook that denies commands calling those or `aggy borrow`. It catches accidents. It is documented as not a security boundary.

**Image and video.** `aggy image` and `aggy video` are `aggy borrow image|video` with file arguments:

```
aggy image "<brief>" --out public/hero.png [--vendor codex|grok] [--ref style.png] [--n 3]
aggy image --edit public/hero.png "darker background, keep the logo"
aggy video "<brief>" --out public/intro.mp4
```

The on-screen agent writes the brief (purpose, size, exact text, style references, target path). aggy runs the vendor read-only with any reference images, copies the result to `--out`, and writes a sidecar with the prompt, vendor, model, and child session id. The agent checks the image and accepts it or asks for an edit. `--edit` resumes the same child, so the vendor keeps its context. Image MCP servers are out, because nearly all of them call an API with a key.

Facts established against codex-cli 0.164.0 and grok 1.0.70:

- Codex: `codex exec --json --sandbox read-only` generates images on the plan. They land in `~/.codex/generated_images/<thread-id>/`. The image tool call is not in the JSON stream, and one request can produce more than one image, so aggy collects and counts every file in that folder and reports the count. Reference images go in with `codex exec -i`.
- Grok: `image_gen` is verified headless on the plan with `grok -p --tools image_gen --output-format json`. `image_edit` exists but is unverified headless. There is no `video_gen` tool: a video borrow gets `image_gen` and `image_to_video`, unverified headless (batch 6 owns video). All three sit behind `features.image_gen` and `features.video_gen`. Files land in `~/.grok/sessions/<url-encoded cwd>/<sessionId>/images/`, and the reply gives the path relative to the session folder.

**Research and news.** The child gets the question, today's date, and any versions that matter. The report goes to `.aggy/research/<date>-<slug>.md` with sources, publish dates, an "as of" time, and open questions. The parent gets about ten lines and the path. The same question reuses a cached report for a day (news) or one to two weeks (research). `--fresh` skips the cache. The report is data, and the parent treats it as information, not instructions.

Research takes in untrusted web content and sends queries out. If it could also read files, a planted instruction on a page could put a secret into a query. So research and news children run in an empty temp directory with file and shell tools removed:

- Grok runs with `--tools web_search --no-subagents` and `--disallowed-tools` naming the file, shell, and fetch tools (`read_file,grep,list_dir,write,search_replace,run_terminal_cmd,web_fetch,Agent`), the media and workflow tools, and the MCP meta-tools `search_tool` and `use_tool`. In a live check (2026-10-28) the model then reported only `web_search` and its browse helpers `open_page` and `open_page_with_find`, for research and news alike; that is the model's own one-turn tool list, not a wire dump, and the run also named the two `open_page*` tools in `--disallowed-tools`, which Grok ignored with a warning, so the exact set above has not been run live yet. Grok has no `x_keyword_search` or `x_semantic_search` tool; news uses `web_search`. An unknown name in `--tools` makes Grok ignore the whole allowlist with only a warning, so the list must name real tools.
- Codex research and news are unsupported for now. With `--ignore-user-config --sandbox read-only --disable shell_tool --disable unified_exec --disable view_image --disable apps --disable plugins --disable goals --disable image_generation --disable sleep_tool -c 'web_search="live"'`, a Codex child has no tool that reads a file, and live checks found no direct read. But its `apply_patch` tool, which no flag removes, answers a right guess of a line in a file at a known path differently from a wrong one, so a child can confirm guessed contents. The Codex adapter refuses both kinds until that is closed (T6638); an OS sandbox around the child closes the content oracle but not path existence, and is backlogged (section 21). Details are in [verification.md](verification.md).

Tool deny-lists only cover the vendor's built-in tools. On Grok, denying `search_tool` and `use_tool` cuts off MCP servers too; on Claude, `--strict-mcp-config` with an empty `--mcp-config` leaves no MCP or connector tool (verified live).

## 8. Routing

The on-screen agent is a model already in a turn, so it makes the judgment call: which kind of task this is. The skill lists the kinds and when to use each, for example that anything that may have changed since the model's training data goes to `research` or `news`. aggy then makes the policy call from the routing table. Changing which vendor handles a kind is a config edit; the skill does not change. aggy has no model-based router.

Defaults, chosen from what each CLI has built in, not from benchmarks:

| Kind | Order | Why | Time limit |
| --- | --- | --- | --- |
| `news` | Grok | Web search built in, and a flag set that leaves only web tools (section 7). | 5 min, 15 turns |
| `research` | Grok | Web search with sources, with the same clean flag set. | 10 min, 30 turns |
| `image` | Codex, Grok | Built-in image generation. | 5 min |
| `video` | Grok | Only Grok has video tools. | 10 min |
| `review` | Not the author: Codex, Grok, Claude | Any connected vendor except the one that wrote the change (the parent's vendor). | 15 min |
| `edit` | Codex, Grok | Moves edit work off the on-screen vendor. Accepted in config; borrows refuse `edit` until M3. | 45 min |

Codex is deliberately not in the `news` and `research` defaults. Its `apply_patch` tool cannot be turned off, and its error text tells a right guess of a line in a file at a known path from a wrong one, so a research child could confirm file contents by guessing ([verification.md](verification.md), 2026-10-28). Until that oracle is closed (T6638), the Codex adapter refuses both kinds. A user config may still list `codex` for them: the router keeps it in the list with a note naming the reason, and the adapter refuses it with `ErrUnsupported`.

aggy walks the list for the kind (or the one `--vendor` named) and skips, checking in this order:

- the vendor the parent is on, which should use its own tools instead (for `review`, it wrote the change)
- vendors that are not connected, or that `aggy setup` never recorded, in the `vendors` table; an `unknown` sign-in status counts as usable, and aggy says so
- vendors outside the repo's `allowed_vendors`
- vendors that hit their limit recently (the limit signal comes with M2)

The rest are tried in order. A candidate whose CLI is missing, whose adapter refuses the kind, or whose version the lockdown was not verified on is passed over before anything runs, and the report names it and why; a borrow that started and then failed is the result, with no second vendor. When none is left, the error names each vendor and why it was skipped, for example `no vendor available for research: grok: the parent is on grok; codex: not connected`.

Every borrow is logged with its kind, vendor, model, tokens, time, and outcome. A redo, or the same question re-asked of another vendor, counts against the first. `aggy stats` shows results per kind, and you reorder the table. The switch picker uses the same signals.

## 9. Configuration

User config is `~/.aggy/config.toml` (or `$AGGY_HOME/config.toml`). Unset keys take the defaults, and unknown keys are an error.

An illustrative override (the values are examples, not recommendations):

```toml
default_vendor = "codex"    # unset means the first connected vendor

[vendors.grok]
model = "<model-id>"        # passed to the CLI unchanged

[switch]
order = ["grok", "codex", "claude"]

[routing]
image = ["grok", "codex"]
review = ["grok", "claude"]  # or "not-author": codex, grok, claude minus the author

[budgets]
research = { minutes = 15, turns = 40 }
```

Per repo, `<repo>/.aggy/config.toml` holds `allowed_vendors`, `tools_off` (tools this repo turns off; a repo cannot turn any on), the commands `aggy check` runs, the ignored files a new worktree copies (such as `.env`), and a worktree setup command.

Defaults are generic. No model is set for any vendor, so each CLI uses its own default. `switch.order` defaults to claude, codex, grok, and the picker skips the vendor on screen. Routing and budgets default to the table in section 8, and no tool is wired in.

`[routing]` takes one key per kind (`news`, `research`, `image`, `video`, `review`, `edit`), each an ordered vendor list; an empty list turns the kind off, and `review` also takes `"not-author"`. An unknown kind or vendor is an error that names the key, and a vendor listed twice keeps its first place. A setting replaces that kind's default list; it is not merged with it. Listing `codex` for `research` or `news` is accepted, but the Codex adapter refuses those kinds for now (section 8).

**Tools.** Every tool, plugin, skill, and MCP server aggy manages, aggy's own skill included, can be turned off cleanly per vendor, and off costs zero tokens: an off tool leaves no MCP entry, no skill link, no hook that injects text, and no line in memory, the brief, or the missing-tools list, and a CLI that scans shared skill folders (Grok scans `~/.claude/skills` and `~/.agents/skills`) gets the skill on its ignore list. A test proves it per vendor. `aggy tool list` and `aggy doctor` show each tool's estimated token cost per vendor. Tools are installed once at a pinned version, with any index built ahead of time, and stay ready while off, so `on` and `off` only rewire: the next session aggy launches or resumes gets the new set on its command line, and a config edit through the manifest is made only for a CLI without a launch flag (below). A switch takes effect at the next turn end: aggy resumes that harness on the same session where the CLI allows it, and never interrupts a turn.

**aggy's own skill.** `aggy skill link [--vendor v]` writes the skill once, to `~/.aggy/skills/aggy/SKILL.md`, and links that folder into each vendor's user skill folder: `~/.claude/skills/aggy` for Claude, `~/.agents/skills/aggy` for Codex (its documented user folder), and `~/.grok/skills/aggy` (or under `$GROK_HOME`) for Grok. It is `/aggy` in Claude and Grok and `$aggy` in Codex. Every write and link is a manifest change, so `aggy unwire` undoes it; `aggy setup` does not link it, so linking stays an explicit step. The layout loads the skill exactly once per linked CLI: Claude scans only `~/.claude/skills` and Codex only `~/.agents/skills` among these, so each sees one link. Grok scans all three folders, so with every vendor linked it reaches the skill three times, but all three links resolve to the one installed folder and Grok drops a skill whose canonical path it has already seen (verification.md, "Skill folders"), so it loads once. A per-vendor link is what keeps vendors independent: unlinking Codex must not take the skill from Grok, which a Grok that relied on `~/.agents/skills` would lose. `aggy skill unlink [--vendor v]` removes that vendor's link (a removal change, also undone by unwire). For Grok it also adds the installed folder to `[skills] ignore` in Grok's `config.toml` whenever Claude's or Codex's link is still in a folder Grok scans; Grok compares ignore entries with canonical paths, so the one entry hides the skill through every link. Linking Grok again takes the entry out, and with no other link left none is needed. A test proves, per vendor, that after unlink no folder that vendor scans gives it the skill. The skill's cost is estimated offline as bytes / 4: about 60 tokens every turn for the frontmatter and about 640 on invoke, held by a test under 150 and 1,500 and shown by `aggy skill status`. Limits: the ignore entry is written only when a Grok home exists, so a Grok installed later sees a Claude or Codex link until `aggy skill unlink --vendor grok` is run (`aggy skill status` says so); and an existing `[skills] ignore` list is not extended automatically.

Whether a tool is on is set per vendor and per repo, in aggy's state in `~/.aggy`. A repo can never turn a tool on: a checked-in `.aggy/config.toml` may only turn tools off, so cloning a repo cannot start an MCP server. Every session aggy launches (the on-screen session, a handoff or resume, a borrow child) gets the tools that are on for its vendor and repo on its command line, verified live: Claude `--mcp-config` (with `--strict-mcp-config`), which is not stored with the session and must be passed again on every resume; Codex `-c mcp_servers.<name>.command=…` and `.args=…`, plus `-c mcp_servers.<name>.default_tools_approval_mode="approve"`, because `codex exec` otherwise denies every MCP call; and Grok, which has no per-session MCP flag, through `--agent <file>` whose frontmatter lists `mcpServers` (whether it must be passed again on resume is open, section 21). A CLI without any of these falls back to a config edit through the manifest. `aggy switch <vendor> --tool <name>` (or a toggle in the switch picker) and `aggy borrow <kind> "<task>" --tool <name>` turn a tool on for that one session without changing any setting, and routing can give each borrow kind default tools; research, news, image, and video borrows refuse file-reading tools such as CodeGraph. Subagents a vendor CLI starts itself get whatever tools their parent session has. Claude subagents and Grok user agents can declare inline `mcpServers` of their own, and Codex roles cannot; aggy does not use subagent-only servers yet.

The CodeGraph index is built lazily the first time CodeGraph is turned on for a repo, then refreshed incrementally. Worktrees do not share it: a worktree gets a copy of the main checkout's index only when a session aggy launches there has CodeGraph on, the copy is refreshed for its branch, and `aggy gc` removes it with the worktree.

## 10. Parity

Without parity, a switch lands in an agent that does not know the project's rules, lessons, or safety settings. Codex reads only `AGENTS.md`, while many repos have only `CLAUDE.md`. Claude's auto-memory is invisible to the other two. A repo's `.claude/` permission rules apply only to Claude.

- **Instructions.** aggy adds `CLAUDE.md` to Codex's `project_doc_fallback_filenames` in `~/.codex/config.toml` (or `$CODEX_HOME/config.toml`). That is one config line and no repo changes. Grok already reads `CLAUDE.md`. The key is taken to be a top-level array of file names; that shape is assumed, not yet checked against a live Codex. The change is a minimal text edit through the manifest, not a re-serialization: an existing list keeps its entries, comments, and line endings and gains `"CLAUDE.md"`; a missing key gets one top-level line. The result is decoded and compared with the original before anything is written, and `aggy unwire` undoes it.
- **Memory.** A shared project memory at `<repo>/.aggy/memory.md`, capped at 4 KB and delivered at session start. Claude takes it with `--append-system-prompt-file <path>`, which works interactively and with `-p` although `--help` does not list it (verified live); Claude snapshots the system prompt on the first request, so a changed file is ignored on `--resume` until compaction unless `--system-prompt-snapshot off` is passed. Codex takes it with `-c developer_instructions="…"` on every subcommand. Grok takes it with `--rules "<text>"`, inline text only. How Codex and Grok treat new text on resume is open (section 21). `aggy memory import --from claude` copies chosen entries from Claude's memory folder, which is plain markdown, after a preview. It is a separate, explicit step. To share memory across machines, remove its git exclude line and commit it.
- **Task file.** The skill tells every harness to keep `.aggy/task.md` current at step boundaries, under fixed headings and at most 60 lines. When a vendor is cut off mid-turn, the file is at most one step behind. A `Stop` hook nudges when it is 5 or more turns stale and files changed.
- **Connected tools.** Claude.ai connectors exist only inside Claude, and aggy cannot copy their logins. Where the same server is public, aggy adds it to Codex (whose own `codex mcp login` signs in) and to Grok. The brief lists tools the new harness lacks.
- **Safety rules.** One small policy rendered into each CLI's own format (Claude permissions, Codex sandbox and approval policy, Grok permission rules and sandbox profile): never read `.env` or key files, never push to main, no destructive commands. `aggy doctor` reports drift.

The `.aggy/` project folder is hidden from git through `.git/info/exclude`, never by editing `.gitignore`. aggy finds the exclude file the way `git rev-parse --git-path info/exclude` does, without running git: in a linked worktree or a submodule, `.git` is a file naming the git dir, and a worktree's exclude file is the shared one in the common dir. Because a `.git` file can name any directory, the git dir must hold a `HEAD` file and the common dir `HEAD` and `objects/`, and neither `info` nor `info/exclude` may be a symlink; otherwise nothing is planned. The line `.aggy/` is added through the manifest, once (a file that has it is left alone), with the file's own line ending, so `aggy unwire` removes it again. To commit the shared memory, delete that line, or `git add -f .aggy/memory.md`: exclude rules never apply to a tracked file.

## 11. Hooks and limit detection

Each vendor calls `aggy hook <event>` from its own hook config. Hooks call a fixed binary path. The payload is parsed as JSON and forwarded to the supervisor over a local socket. It never reaches a shell.

Hooks, checked against claude 2.1.311, codex-cli 0.164.0, and grok 1.0.70:

| | Turn end | Limit signal |
| --- | --- | --- |
| Claude | `Stop` | `StopFailure` with a classified `error` and `error_details`. The limit text reads "hit your limit … resets in". |
| Codex | `Stop` and the `agent-turn-complete` notify | No failure hook, and no machine code in `exec --json`: its `error` and `turn.failed` events carry only the text "You've hit your usage limit … try again at <local time>". The structured signal is in the rollout file: a `task_complete` event whose `error.codex_error_info` is `usage_limit_exceeded`, and `token_count` events whose `rate_limits.primary` and `.secondary` give `used_percent` and `resets_at` (unix seconds). The rollout does not keep the `error` event itself. aggy reads the rollout for both modes, with the text as fallback. |
| Grok | `Stop`, `StopCancelled` | `StopFailure` with `error: rate_limit`. Grok's error classes mirror Claude Code's. |

Adapters map every plan-limit signal to one class, `rate_limit`, which opens the switch picker and reroutes borrows. Codex detection is weaker without a failure hook; the rollout fields above are source-verified, and a live capture of a real limit is still open (section 21).

Two hook facts shape the wiring:

- **Codex trusts hooks by hash.** A hook in `hooks.json` or `config.toml` runs only once its hash is recorded as trusted (`[hooks.state."<key>"] trusted_hash`), which the user approves in interactive Codex, or with `--dangerously-bypass-hook-trust`, which aggy never passes. A hook given with `-c` comes from an untrusted layer and is silently skipped (verified live). So aggy cannot add a Codex hook per launch; the user must approve aggy's hook once.
- **Grok runs Claude's hooks.** By default Grok also loads the hooks in Claude's `settings.json` (turned off with `[compat.claude] hooks = false` or `GROK_CLAUDE_HOOKS_ENABLED=0`). A hook aggy installs for Claude therefore fires under Grok too, and `aggy hook` must tell the two apart: Grok sets `GROK_SESSION_ID` in the hook's environment and sends `sessionId` (camel case) in the payload, where Claude sends `session_id`.

## 12. Config changes: the manifest, `--dry-run`, and `aggy unwire`

Parity and tool wiring edit other tools' config: `~/.codex/config.toml`, Claude and Grok settings, skill folders, and MCP entries. Every such change is:

- **Planned.** `--dry-run` shows each file and the change before anything is written.
- **Guarded.** A patch names the file and the hash it expects. If the file has changed since, the patch refuses and writes nothing. Applying a patch twice is a no-op.
- **Backed up.** The old bytes are copied under `~/.aggy` before the write, and the write is atomic.
- **Recorded.** Each change goes into the manifest in the store: file, hash before, hash after, backup path.
- **Undoable.** `aggy unwire` restores every file byte for byte, or removes a file aggy created. It refuses to overwrite a file someone changed after aggy did.

No other code path writes another tool's config.

A patch is one of three kinds. A **content** patch rewrites a file through a transform (deleting a TOML or JSON entry is a content patch whose transform drops it; undo restores the bytes, so it needs no kind of its own). A **link** patch makes the path a symlink: it creates one, or replaces the file or link there. A **removal** patch removes a file or a symlink. Link and removal patches never follow a symlink at the path: they read it with lstat, record a symlink by its target (`link-` and the target's sha256, so a link's hash never equals a file's), back up a replaced file's bytes or a replaced link's target under `backups/<change-id>/<hash>.bak`, and act only on a path that still has the device and inode they checked. A link whose target is, or resolves to, a vendor credential file is refused, and so is replacing or removing one. Undo puts back the same file and mode, or the same link, or removes the link aggy made, under the same drift, superseded, blocked, and sweep rules as a content change. For these kinds, "the same file" is the link's own path (its directory made canonical, its name kept), not what it points to, so two links to one target are two paths. A content patch on a path that is a symlink still reads and writes through it (the limit below).

`aggy unwire (--change ID | --run ID | --all) [--dry-run]` undoes changes newest first (reverse id order), because a later change to a file was planned against the earlier one's result. Each undo runs under the target's lock, restores the old bytes and mode (or removes a created file and any directories made for it that are now empty), and marks the row undone. `--dry-run` prints the diff of each undo, escaped for the terminal. No-op rows and the marker row of a run that changed nothing are skipped.

A row is `pending` until its write is recorded, and `failed` when the write failed after the row was written. A `failed` row never wrote: the target write is an atomic rename, and the only error after it, a failed directory sync, is recorded as applied. Drift found by the re-check just before the write also leaves a failed row, with no backup. A failed row is *cleaned* whatever came after it (the file is not even read): the file is left as it is, only the empty directories its own Apply made are removed, and the row is marked undone. A failed row records only the directories its Apply made itself (one another process made between the plan and the mkdir is not aggy's), each with its identity; a directory it has no identity for is never removed by path alone. A pending row may have been written or not, so unwire compares the file with the row's hashes: still the hash before means nothing was written, and only empty directories made for a new file are removed (by path: a pending row stopped before it could record their identities); the hash after means the write happened before the process stopped, and it is undone from `backups/<change-id>` like an applied change, or fails loudly when that backup is gone; anything else is drift. Either way the change leaves nothing behind.

Before acting on a row, unwire checks that it looks like a change aggy could have made: the file is an absolute, clean path, the backup is named `<change-id>/<hash>.bak`, and every directory recorded as created is an ancestor of the file and below the home directory (never the home directory, an ancestor of it, or `/`). Any other row is refused untouched. This is a sanity check against a damaged or mistaken row, not a security boundary: whoever can write `~/.aggy` can plant a well-formed row and a matching backup together, and so can make unwire write or remove any file the person can. The backup is always read from the current `backups/` (the state directory resolved now), whatever prefix the row recorded, so a state directory spelled differently than when the change was made (`/tmp` and `/private/tmp`, or `~/.aggy` moved into a dotfiles directory and linked back) still undoes.

Any other change is undone only after every newer change to the same file (by canonical path) that is still in effect: applied, pending, or failed. `--change` refuses, forced or not, while such a newer change exists, and names it: undo it first, or use `--all`. In a `--run` or `--all` undo, an applied change behind such a newer change outside the selection is refused the same way. So is a pending change that a newer change was planned on (the newer change's hash before is this change's hash after): its write may have happened and is under the newer one, and `--all`, newest first, undoes the newer one before it. A pending change behind a newer change is *superseded* only when some newer change to the same file, still in effect, was planned on this change's hash before, under the target lock: that newer change read the file under the lock and found this change's write had not happened (a change planned where the file system cannot lock is recorded as `unlocked` and proves nothing, since its plan may have raced a live write), and while it is in effect nothing older can have been undone, so the file holds the newer changes' writes. It is left alone, only empty directories made for the old change are removed, and the row is marked undone. An undone newer change proves nothing, because undoing the changes between can bring the old write back. Any other pending change behind a newer change is refused the same way as above, because its write may have happened and been edited by hand before the newer change was planned on the edit; once the newer change is undone, the file no longer matches either of its hashes and it is reported as drifted, still pending, with its backup in place, for the person to see and undo by hand or with `--change <id> --force`. Nothing is marked undone while its line may still be in the file.

After any undo, unwire also sweeps the directories every undone change created and removes those now empty, unless a change still in place created them too. A created directory's identity is its device and inode, plus its birth time where the file system reports one (macOS and the BSDs; Linux has it only through `statx`, which the standard library does not expose), recorded once it exists. Device and inode alone do not prove a directory is aggy's: ext4 hands a freed inode number straight to the next directory made, so a directory the person removed and made again can match. So the sweep removes a directory only while its identity still matches, birth time included when one was recorded, and either a birth time was recorded (which on its own tells aggy's directory from a new one), or the unwire is related to it: it undid the change that created the directory, or itself removed a file aggy created inside it. A file found already gone relates the unwire to nothing, because the person may have removed it along with the directory and made the directory again. An unrelated later unwire never sweeps a directory known only by device and inode. APFS can rewrite a directory's birth time (a restore, or a tool that sets it) but never reuses an inode number, so during the undo of the change that made a directory, a device and inode match is enough; any other unwire still needs the birth time to match. Once aggy sees a recorded directory gone, or with another identity, it forgets the identity. Known limits, without birth times: if the person removes aggy's directory with aggy's file in it and makes it again, the unwire of that file's change removes the new directory while it is empty; and if an undo removed aggy's file and stopped before recording it, the next unwire finds the file gone and leaves the now-empty directory of an earlier undone change, rather than risk removing one the person made.

Two hand edits count as undoing aggy's change, not as drift: a file aggy created that now holds only white space is removed (with the directories made for it), and a file that is its old content plus one line ending, which aggy added before its own line, gets its old content back exactly.

A file changed after aggy changed it in any other way is reported and left alone, and unwire goes on with the rest. `--force` undoes one such change anyway: it needs `--change` and `--yes`, without `--yes` it only shows what it would overwrite, and the edited bytes are saved under `backups/<change-id>` first.

Known limits of a content patch (TODO(T6607) follow-up): a file that is a symlink is restored through the link, a hard link to a changed file is not kept (the restore writes a new inode), and extended attributes are not restored.

## 13. Security model

aggy runs with your privileges next to agents that can run shell commands. Every surface that carries outside text or writes config has a named control.

**What aggy never reads.** The goal is that aggy never reads the vendor credential files: `~/.claude/.credentials.json`, `~/.codex/auth.json`, `~/.codex/.credentials.json` (Codex's MCP token store), `~/.grok/auth.json`, `~/.grok/mcp_credentials.json`, and the same files under a relocated `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, or `GROK_HOME`. A guard enforces it: all file access goes through one wrapper, `internal/safefs`, which on every call:

- checks the path first, resolved through symlinks and compared in Unicode NFC and, on macOS, with full Unicode case folding and then the `/System/Volumes/Data` firmlink prefix removed (in any spelling APFS takes for it) (so `ſ`, `ß`, and the Kelvin sign match the way APFS matches them), and then acts on the resolved path
- refuses the credential files by file identity (device and inode, read from metadata only), so a hard link or a different spelling of the same file is caught
- reads through an opened descriptor, opened with `O_NOFOLLOW`, whose identity is checked before any byte is returned
- refuses, on macOS, any path in `/.vol`, which names a file by device and inode around every path check
- refuses a `RemoveAll` whose tree holds a credential path, or any entry with a credential's identity, before removing anything
- fails closed: when it cannot find an absolute home directory, or a relocation variable is a relative path, every call is refused
- refuses a regular file with more than one hard link, since its other name could be a credential file

Known gap: a hard link made to a credential before the vendor rotates its token keeps the old inode, now with one link and the old token, and safefs reads it like any other file. Remembering old identities was tried and removed: ext4 reuses inode numbers at once, so aggy refused its own new files, and the history was easy to delete or redirect. The gap is closed where such a file could leave the machine: from M1, a borrow's artifacts must be real image files (checked by magic bytes), created during the child's run, with one link (TODO(T6724)). Hardening is also still in progress for check-then-act races in the path-based calls (`Chmod`, `Remove`, `MkdirAll`).

Lint bans the main file-system entry points (`os`, `io/ioutil`, `io/fs`, `filepath` walks) outside `internal/safefs` and test files; every `syscall` name except an explicit allowlist (errno values, the open flags safefs needs, `Stat_t`, flock, and process attributes), in safefs too; signals and process attributes outside `internal/execrun` and `internal/store` (`internal/supervisor` may send signals); `os/exec` outside `internal/execrun`, `internal/supervisor`, and `internal/borrow`; every `net/` package except `net/url` and `net/netip` (and `net` itself, except in the supervisor socket's files, which only listen on a Unix socket; `net.Dial` stays banned), `log/syslog`, `debug/*`, `go/parser`, `go/build`, `unsafe` (and so `//go:linkname`), and cgo (`make nocgo` asks `go list` for each OS's cgo files, which lint cannot see when cross-compiling); `golang.org/x/sys/unix`, except for the socket's peer-uid calls in two supervisor files (the stdlib has no `getpeereid` on darwin); Go assembly and `.syso` objects, which make raw system calls past every ban (`make noasm`); `toml.DecodeFile` and `toml.DecodeFS`; the SQLite driver and `sql.Open` outside `internal/store`; the process-starting runner, which is its own package (`internal/execrun`) that only the files of `internal/cli` and `internal/devfix` (not their subpackages) and tests may import, so no other package can re-export it to the adapters, which never start processes, run commands only through the runner the caller passes, and may not import `internal/execrun`, `internal/cli`, `internal/devfix`, or `os/exec`; and requires every `nolint` comment to name the linter and give a reason. Lint runs for darwin/arm64, darwin/amd64, linux/amd64, and linux/arm64. Other packages that open files are covered by review. The SQLite driver is the main exception to the wrapper: it opens the database and its `-wal` and `-shm` files itself, so the store checks those paths through `safefs` after opening. The others are fixed paths: `creack/pty` opens `/dev/ptmx`, and the supervisor's listener creates its socket (and sets its mode) in `run/`, after `safefs` has checked the directory and any stale socket. The read-only handle that `aggy log`, `aggy doctor`, and the `--dry-run` preview use carries a SQLite authorizer that allows only SELECTs and the `user_version` read, so it cannot `ATTACH` another file, change a PRAGMA, or `VACUUM`.

**What aggy never sends.** aggy collects no telemetry, sends nothing to the project, makes no model or vendor API calls of its own, and never proxies vendor traffic. The network surfaces aggy itself will open are all planned and opt-in: the remote client and chat connection, local webhooks, `aggy pr`, which opens a pull request on your git host, and `aggy usage`, whose ccusage reader may fetch pricing data unless it runs offline (section 21). What a vendor CLI receives from aggy is limited to:

- a brief, which has lines matching secret patterns removed, can be previewed with `--brief --show`, and only goes to a vendor in the repo's `allowed_vendors`
- a borrow prompt: the task, paths, constraints, and date, never the parent transcript
- for research and news, only what the task needs; those children run in an empty temp directory, never see copied secret files, and have built-in file and shell tools removed (Codex research and news are refused until its `apply_patch` oracle is closed; see section 7)

**What aggy stores.** Ids, paths, status, CLI versions, and connection status. Never tokens.

**Threats and controls:**

| Threat | Control |
| --- | --- |
| A planted instruction on a web page puts a secret into a search query | Research and news run in an empty temp dir with built-in file and shell tools removed, and get only what the task needs. Grok's flag set also removes MCP meta-tools. Codex is refused for both kinds, because `apply_patch` can confirm guessed file contents (section 7). |
| Cloning a repo starts an MCP server or other tool | Tools are turned on only in `~/.aggy`, per vendor and repo; a checked-in `.aggy/config.toml` may only turn tools off (section 9). Research, news, image, and video borrows refuse file-reading tools. |
| A brief carries secrets to another vendor | The credential-file guard, redaction of lines matching secret patterns, `allowed_vendors` per repo, and a preview. |
| aggy breaks a user's own tool config | Dry run, hash guard, backup, manifest, `aggy unwire` (section 12). |
| A delegated child spends another subscription through its shell | The `PreToolUse` guard, documented as not a security boundary. |
| A hook payload runs as shell input | Fixed binary path, JSON parsing, never a shell. |
| Webhook text steers an agent with a shell | Localhost only, a required shared-secret signature, the payload wrapped as data, and a watcher child that is read-only or in its own worktree. |
| Someone else drives your terminal | Remote client pairing with a one-time code, per-device tokens, LAN by default, no project-run relay. |
| A tampered release | Signed checksums, build provenance, pinned dependencies. |

Report vulnerabilities privately; see [SECURITY.md](../SECURITY.md).

## 14. The store

Per-user state lives in `~/.aggy` (or `$AGGY_HOME`):

```
~/.aggy/
  config.toml                 user config
  aggy.db                     SQLite index
  backups/                    copies taken before any config change
  inbox/<borrow-id>.md        finished borrow reports
  recipes/<name>.toml         saved segues (M6)
  run/<parent-id>.sock        supervisor socket for hooks and borrows
  logs/aggy.log
```

Per-project state lives in `<repo>/.aggy`, excluded from git:

```
<repo>/.aggy/
  config.toml                 allowed_vendors, tools_off, check commands, worktree copy list, setup command
  task.md                     current task state
  memory.md                   shared lessons, 4 KB cap
  briefs/<child-id>.md        briefs handed to children
  research/<date>-<slug>.md   research and news reports
  assets/<path>.json          sidecars for generated images and video
```

The index is SQLite in WAL mode, with the file at mode 0600, in a directory created 0700 if it does not exist, using a pure-Go driver. One index covers every directory. The driver opens the database, `-wal`, and `-shm` files itself, outside `safefs`, so the store checks those paths through `safefs` after opening them.

| Table | Holds |
| --- | --- |
| `parents` | id, title, directory, last vendor, created, last active |
| `children` | id, parent, vendor, vendor session id, kind, model, cwd, worktree, transcript path, status, process lease, last active |
| `borrows` | id, child, kind, vendor, model, task hash, budget, status, outcome, artifacts, image count, tokens, report path, start, end |
| `changes` | the config-change manifest: file, hash before, hash after, backup path, applied, undone |
| `probes` | per vendor: CLI path, version, connected, capabilities, probed at |
| `vendors` | what `aggy setup` last found, one row per vendor: connected (`connected`, `not_connected`, or `unknown`), CLI version, checked at |
| `events` | append-only log behind `aggy log` |

## 15. Vendor adapters

Every flag, file path, hook format, and transcript format aggy depends on goes through one adapter per vendor. All three CLIs update often, so a CLI change is fixed in one package, and adding a vendor is one adapter plus its routing entries.

Adapters build commands and parse output; they never start processes. They probe each CLI's version and capabilities, re-check them when the version changes, and degrade instead of failing. Each adapter declares the CLI versions it is tested on, and `aggy doctor` warns outside that range.

The interface is documented in [adapters.md](adapters.md). The interface is unstable until M1, while the first vendor adapters are built; adapters.md lists the known gaps.

## 16. Other commands

- `aggy setup` checks Claude, Codex, and Grok in turn: it finds each CLI on `PATH` and probes it through its adapter for version, capabilities, and sign-in status. You can skip any and rerun setup later; `--only claude,codex` picks vendors, and `--yes`, or a stdin that is not a terminal (`/dev/null` counts as none), checks without asking, and input that ends at a prompt switches to that for the rest, so every vendor not skipped is recorded. A CLI that is not signed in gets the vendor's own sign-in step printed (`claude` then `/login`, `codex login`, `grok login`), and on a terminal setup waits and checks again. aggy never runs a login itself: no adapter has a verified login command. Nor has any a verified sign-in status command yet, so today every installed CLI's status is `unknown`. Setup records only, per vendor, connected or not, the CLI version, and when it checked (the `vendors` table). It writes no vendor config; applying the parity layer comes with M2.
- `aggy doctor` checks, per vendor, install, the CLI version against the adapter's tested range (`adapter.VersionWarning`), and sign-in status; the API-key variables, the base-URL variables, and the vendor config settings that route a CLI to an API key or another endpoint (named by variable, file, and key, never by value); and config drift, which is an `aggy unwire --all` dry run that takes no locks and writes nothing (`manifest.Drift`), so doctor and unwire cannot disagree: a change unwire would refuse without `--force` is drift (warning); one it cannot check, such as a row naming a credential file, which is reported and never read, or an invalid row, is an error; one waiting on a newer change to the same file that is still in place, which must be undone first, is info naming that change; one unwire would undo cleanly (restored, removed, cleaned, superseded) is not reported. Skill links, MCP entries, and tool toggles come with the features that make them (M2 and later). Each finding has a stable code and a level: **error** when aggy cannot work as set up (no vendor CLI, a CLI that fails its version check, a changed file that cannot be checked, an unreadable index), **warning** when something may cost money or surprise you (API keys, base URLs, config routes, an untested or unreadable version, not signed in, drift), **info** otherwise. It exits 1 when any finding is an error, writes nothing, and never creates the index. `aggy doctor --report` prints the same findings for a public issue: aggy's version, OS and architecture, each CLI's version, and each finding's level, code, and message, scrubbed: a finding's path is kept apart from its message and shown as `~/…` under home and as `<path>/<base name>` anywhere else (so spaces in a path cannot leak it), error text from a CLI or the system is left out, any other absolute path in the text also becomes `<path>/<base name>`, the user name is replaced wherever it stands as a word (unless generic, such as `root`), and then the fixture scrubber (`internal/devfix`) removes home paths, host names, emails, UUIDs, and token-shaped text; it holds no environment value, file content, or session id.
- `aggy check` runs the repo's typecheck, lint, and tests from repo config.
- `aggy log` records which harness and model were on screen, the switches, the borrows, and the files changed. `aggy inbox` holds finished borrows.
- `aggy usage` reads each connected CLI's local usage logs (through ccusage and `grok usage`) and shows consumption. ccusage may fetch pricing data unless run offline (section 21). Remaining allowance appears only where a vendor writes its limit state locally.
- `aggy gc` prunes finished worktrees, closed children, stray processes, old inbox items, and expired research.
- Attribution: aggy sets `AGGY_VENDOR` and `AGGY_MODEL` for each child, and a commit hook adds them as a trailer.
- Desktop notifications when a child waits on approval, a borrow finishes, or a limit is hit.

## 17. Testing

CI never signs in to a vendor.

- Unit tests cover routing, briefs, budgets, config patches, and `aggy unwire` restoring files byte for byte. Every patch aggy can make has golden diffs (`internal/parity/testdata`). The M0 gate (`TestM0Gate`) builds a temp HOME with realistic vendor config (TOML and JSON, comments, CRLF, no trailing newline, a fake credential), applies setup-style patches through the manifest over several runs (including a create with an explicit mode, a file at a non-default mode under a 077 umask, a failed run that made directories, a pending change whose write happened, and a mode another tool changed afterwards), checks the dry run's diff lines, runs `aggy unwire --all`, and requires every file byte-identical with the same mode and no extra file or directory. It runs on macOS and Linux in CI.
- Fake CLIs (`fake-claude`, `fake-codex`, `fake-grok`) follow scenario files. They honor flags, print terminal output, call hooks with recorded payloads, write session files and images where the real CLI does, and raise limit errors.
- End-to-end tests drive the supervisor in a pseudo-terminal against the fakes, on macOS and Linux.
- Fixtures are real outputs scrubbed of home paths, emails, and tokens. CI fails if a fixture holds one.
- Tests that touch home paths set `HOME` to a temp dir.
- Real-vendor smoke tests are opt-in and run locally, because they spend quota.
- Before each release, a handoff evaluation checks that a new child states the goal and next step correctly in 9 of 10 recorded handoffs.

## 18. Milestones

1.0 is M0 through M4.

| Milestone | Delivers | Done when | Status |
| --- | --- | --- | --- |
| M0 Foundation | Repo skeleton, CI, fake CLIs, store, config, adapter interface and probes, the patch manifest and `aggy unwire`, and the open vendor checks | `aggy unwire` restores files byte for byte, and the unconfirmed vendor facts are answered | In progress: `safefs`, `patch`, `store`, `config`, `home`, and the `Adapter` interface exist |
| M1 Read-only borrows | `image`, `video`, `research`, `news`, `review`; routing, budgets, research isolation, inbox, log, the skill, and the Codex `CLAUDE.md` fallback | A research borrow costs the on-screen agent under 2,000 tokens, and (batch 6, with `aggy image` and `aggy video`) an image lands at `--out` with its sidecar. Released as v0.1.0. | In progress: `aggy borrow` with isolation, lockdown, budgets, the guard, reports, `aggy inbox`, and the log |
| M2 Switching | Setup, doctor, the supervisor, the switch key and picker, resume, task file, brief, limit hooks, parity, desktop notifications | The handoff evaluation passes 9 of 10, and the terminal checklist passes | Not started |
| M3 Edit borrows | Worktrees, `aggy check`, attribution trailers, `aggy gc`, review routed to a non-author vendor | An edit diff reaches you only after the repo checks pass | Not started |
| M4 Usage and stats | `aggy usage`, `aggy stats` per kind, a near-limit warning where data exists | Routing is tuned from real stats. Released as 1.0. | Not started |
| M5 Tool wiring | Skill links, shared MCP entries, a per-vendor off switch; CodeGraph, RTK, Headroom MCP, Context7, Caveman, all off by default | An off tool leaves no schema or skill text in that vendor | Not started |
| M6 Parallel work | One task to two vendors in separate worktrees, you pick one diff; diff comments, `aggy pr` (opens, never merges), one recipe | Nothing lands on the main checkout until you accept a diff | Not started |

After M6: the remote surfaces (a Telegram-style chat, a browser client served by the supervisor, and local webhooks bound to a session), then Windows.

M1 ships before switching because it needs no terminal layer and moves work off the on-screen vendor right away.

## 19. What stays outside

- A runtime aggy owns, with its own tool loop and stored vendor credentials
- One live cross-vendor session: shared hidden reasoning, shared tool-call ids, or a command that survives `/vendor`
- Mid-turn handoff of a running test
- Team logins and unattended fleets
- An LLM router inside aggy
- Image or research MCP servers that call a vendor API with a key
- An aggy-drawn screen that redraws the thread
- An aggy model picker; each vendor has one
- Searching across all vendors' transcripts
- Any proxy or compressor in front of `claude`, `codex`, or `grok`

## 20. Decisions

- **Pass-through, not a screen.** aggy stays the parent process and relays a pseudo-terminal unchanged. Drawing its own screen would duplicate each vendor's UI and break with every release; `exec` leaves nothing to perform a switch.
- **Brief without a model, as a file.** The vendor being left may be out of quota, and a brief must not cost a model call. A file avoids `ARG_MAX`. The task file, kept during the work, is a better source than a summary written at handoff.
- **Key plus turn-end hooks.** A reserved key works the same in every CLI, and hooks say reliably when a turn has ended. A skill can also request a switch.
- **Borrows are a shell command.** MCP tool calls time out on long headless runs, and a shell command behaves the same in all three harnesses.
- **The agent picks the kind, aggy picks the vendor.** The agent already understands the task; a static table makes the vendor choice predictable, cheap, and editable.
- **Go.** A single static binary, mature pseudo-terminal support, pure-Go SQLite with no cgo, and simple macOS and Linux builds.
- **SQLite index in `~/.aggy`.** One index for every directory, safe across processes, with leases for busy sessions.
- **`Ctrl+\` as the switch key.** Not a known shortcut in any of the three CLIs (to be confirmed in M2), configurable, and sent through when pressed twice.
- **MIT license.** The shortest and best-known license. It gives up only Apache-2.0's patent grant, which rarely matters for a CLI wrapper.
- **Windows after 1.0**, on a pseudo-terminal library that supports ConPTY, so it is a port rather than a rewrite.
- **Managed tools switch off cleanly, at zero tokens, and are ready to switch on** (2026-10-27). Every tool, plugin, skill, and MCP server aggy manages, aggy's own skill included, can be turned off per vendor, and off leaves no trace in what any agent loads: no MCP entry, no skill link, the skill on the ignore list of a CLI that scans shared skill folders, no text-injecting hook, and no line in memory, the brief, or the missing-tools list, proved per vendor by a test. `aggy tool list` and `aggy doctor` show each tool's estimated token cost per vendor, so the cost is visible before it is paid. Tools are installed once at a pinned version with any index prebuilt and stay ready while off; `on` and `off` only rewire, on the command line of the next launch or resume (a manifest config edit only for a CLI without a launch flag), and take effect at the next turn end, resuming the same session where the CLI allows it, never mid-turn.
- **Tool scope is per repo, set in aggy's state, and wired at launch** (2026-10-27). A tool is on per vendor and per repo, stored in `~/.aggy`; a repo can never turn a tool on, only off (`tools_off` in its `.aggy/config.toml`), so cloning a repo cannot start an MCP server. Each session aggy launches gets its tools on its command line (Claude `--mcp-config`, Codex `-c mcp_servers…`, Grok an `--agent` file with `mcpServers`), with a manifest config edit as the fallback for a CLI without a launch flag. `--tool` on `aggy switch` and `aggy borrow` turns a tool on for one session only; research, news, image, and video borrows refuse file-reading tools. The CodeGraph index is built lazily per repo and copied to a worktree only when a session there uses it. A CLI's own subagents get their parent session's tools until a CLI can scope an MCP server to a subagent.

## 21. Open questions

- Whether aggy runs ccusage offline, so `aggy usage` makes no network call for pricing data.
- How reliably an interactive Codex session can report its id and its limit without a failure hook, and whether to ask upstream for one.
- A Codex limit event captured live from an exhausted account: which rollout lines appear (`task_complete.error.codex_error_info`, `token_count.rate_limits`) and the exit code of `codex exec`.
- The Codex hook live check: after the user trusts a `SessionStart` hook by hand, that it fires in `codex exec` and its `session_id` matches `thread.started`.
- Grok resume with agent files: whether `--agent <file>` (and its `mcpServers`) and `--rules` must be passed again on `grok -r`. Likewise whether resumed Codex threads take new `developer_instructions`.
- `spawn_agent` inheritance: whether a Codex child agent really starts and inherits the research flag set; `codex exec --json` does not show it.
- Backlogged: an OS sandbox around research children (on macOS, `sandbox-exec` around the whole Codex process), which would re-enable Codex research. A live check closed the `apply_patch` content oracle with it, but a model could still test whether a path exists (EPERM against ENOENT), and `~/.codex` must stay readable to the process ([verification.md](verification.md)).
- How much keeping the task file current costs per session (target: under 3% of tokens), and which cadence and nudge settings to expose.
- The recipe file format beyond a list of steps (switch, new, brief, borrow).
- The remote surfaces and Windows, which come after M6 and are designed only in outline here.
