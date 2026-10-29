# Built-in tools

What each of aggy's built-in tool definitions (`internal/tooldef/builtin`) wires, per vendor. How tools are defined, switched, and wired is in [design.md](design.md), section 9; the vendor facts behind each one are in [verification.md](verification.md), "Managed tools (2026-10-28)".

Common to all of them:

- **Off by default.** A tool starts only after `aggy tool on <name>`. Off leaves no MCP entry, hook, skill link, or line in memory or a brief (`TestToolOffLeavesNoTrace`).
- **aggy never installs.** Each definition pins a version and carries an `install.hint`. `aggy tool on` and `aggy doctor` show the hint when the program is not on `PATH`. A user tool's hint is quoted and attributed to its file, never presented as aggy's advice, and is shown only once the file is the one `aggy tool add` showed. aggy never runs a tool's own `init`, `install`, or `wrap` command, and never writes vendor config for a tool. So uninstalling a tool is the job of the package manager you installed it with (`npm uninstall -g`, `uv tool uninstall`, `brew uninstall`); `aggy tool off` only unwires it, and `aggy tool status` shows whether it is still installed.
- **Per vendor.** An MCP server reaches Claude through `--mcp-config` and Codex through `-c mcp_servers.<name>.*`, for that session only. A hook reaches Claude only, in the session's inline `--settings` layer. Codex hooks live in global config, so they are refused (`tool.no_launch_flag`). Grok gets no MCP server or hook until its per-session form is verified: `aggy tool on --vendor grok` refuses (`tool.vendor_unverified`). A skill is linked for every vendor that has it on, Grok included.
- **Cost.** `aggy tool list` and `aggy doctor` estimate it offline. `skill-md` counts the installed `SKILL.md`. `mcp-schema` and `none` show as `unknown`, because aggy never starts a server to count it.

## RTK (T6712)

- **What it does.** Compresses shell command output before the model sees it.
- **What aggy wires.** Claude only: a `PreToolUse` hook on `Bash`, `rtk hook claude`, run through `aggy hook-tool`. The hook rewrites a command to `rtk <command>` (`rewrites = true`, `rewrite_prefix = "rtk "`). aggy passes a rewrite only in that exact form, and adds the safety policy's Bash deny rules again with the `rtk ` prefix.
- **What it never wires:**
  - **Codex.** `rtk hook codex` answers `permissionDecision: "allow"`, which would approve every command it rewrites (S63-9).
  - **Grok.** RTK has no Grok subcommand, and Grok hooks are gated.
  - **`rtk init`.** It edits vendor config that aggy cannot undo through its manifest.
- **Version.** `rtk 1.1.0` (verified from Homebrew).
- **Network.** None.
- **Cost.** `none`, which shows as `unknown`. The hook adds no text to the context of its own. What it saves depends on each command's output, and nobody has measured it (TODO(T6712)).
- **Limits.**
  - RTK chooses one compressor per command, and decides which output, a failing command's for example, it leaves whole. That is RTK's own behaviour. It is not in verification.md, so aggy makes no claim about it, and aggy cannot tell before a command runs whether it will fail.
  - A rewrite in another form, such as a different subcommand or each part of `a && b`, is dropped, and the command runs as the model asked it.

## Headroom (T6713)

- **What it does.** Compresses a large blob the agent already holds. The original stays local and can be retrieved.
- **What aggy wires.** The MCP server `headroom mcp serve`, which offers compress, retrieve, and stats, for Claude and Codex.
- **Its proxy (T6780), off by default.** `aggy tool on headroom --vendor claude --proxy`, at a terminal, with the typed confirmation, also starts `headroom proxy --host 127.0.0.1 --port 8787` for each on-screen Claude session, health-checks `GET /health`, sets `ANTHROPIC_BASE_URL=http://127.0.0.1:8787` for that CLI only, and stops it when the CLI exits. Every Claude request, the login token included, then passes through Headroom, and so do the requests of every process Claude starts, your own API-key scripts included. A plain `aggy tool on headroom` turns on only the MCP server and says how to add the proxy. If Headroom's definition changes (another argv or port), the proxy turns off until you confirm again. Claude only: Codex and Grok are unsupported. The flags, the health path, and the `DO_NOT_TRACK` opt-out of its anonymous beacon (on by default) were read in Headroom's source, not run at 0.42.0; aggy sets `DO_NOT_TRACK=1` in its environment (TODO(T6780), docs/verification.md).
- **What it never runs:**
  - `headroom wrap` or `headroom init`, which wrap or edit the agents themselves.
  - `headroom mcp install`, which edits every agent config it finds.
- **Version.** `headroom-ai` 0.42.0. Install it with `uv tool install headroom-ai==0.42.0`, which needs Python 3.10 or later. It takes about 7 s to start cold.
- **Network.** Marked `true`, because whether the MCP server reaches the network is unverified. So an edit borrow gets it only with `routing.edit_network_tools`.
- **Cost.** `mcp-schema`, which shows as `unknown`.
- **Limits.** The MCP server works only on demand, on what the agent passes it. Most of Headroom's value comes through the proxy, which stays off until you turn it on for a vendor.

## Context7 (T6714)

- **What it does.** Fetches current docs for the library version in the repo.
- **What aggy wires.** The MCP server `context7-mcp` (stdio), for Claude and Codex.
- **Version.** `@upstash/context7-mcp` 4.4.4. Install it with `npm i -g --ignore-scripts`.
- **Network.** Yes. The docs query goes to Context7's API. Source code stays local.
- **Cost.** `mcp-schema`, which shows as `unknown`.
- **Limits.** It runs without an API key, so its calls are anonymous and rate limited. aggy's shim passes no `CONTEXT7_API_KEY`: tool definitions may not pass keys.

## Caveman (T6715)

- **What it does.** A skill for shorter assistant replies.
- **What aggy wires.** Caveman is only a skill. aggy links `~/.aggy/skills/caveman` into `~/.claude/skills`, `~/.agents/skills`, and `~/.grok/skills` for each vendor that has it on.
  - **Grok.** While Grok has it off but a Claude or Codex link stays, the folder goes on Grok's `[skills] ignore`.
  - **Invoking it.** Use `/caveman` in Claude and Grok, and `$caveman` in Codex. That follows the skill folder conventions aggy's own skill uses (`/aggy`, `$aggy`). The command name comes from the skill's frontmatter.
- **Version.** Tag v4.0.1 (d262ab3). aggy never fetches it, and never runs Caveman's installers, which edit vendor configs, add a proxy, and send telemetry by default. Instead, copy `skills/caveman/` from that tag into `~/.aggy/skills/caveman` yourself, or run `aggy skill add <that folder> --replace`, which shows the files and their sha256 and stores the folder for the built-in (no user definition). `aggy tool on caveman` refuses until the folder is in place.
- **Network.** None.
- **Cost.** `skill-md`: the installed `SKILL.md`'s bytes / 4.
- **Limits.**
  - A skill is linked for every repo and every session of that CLI, aggy's or not. `--repo` and a repo's `tools_off` cannot narrow it.
  - aggy links a tool's skill only from `~/.aggy/skills/<name>`. A built-in skill stored elsewhere under home is not supported yet (TODO(T6715)).

## Playwright MCP (T6716)

- **What it does.** Browser automation that reads the page's accessibility tree instead of a screenshot.
- **What aggy wires.** The MCP server `playwright-mcp`, for Claude and Codex.
- **Scope.** `scope = "repo"`, so it is turned on per repo, with `aggy tool on playwright --repo DIR`. It is not in the default set.
  - It is meant for repos with a UI, but aggy does not guess which repos those are. `when = "has-ui"` is a label that `aggy tool add` and `aggy tool on` show, and nothing checks it.
- **Version.** `@playwright/mcp` 0.0.83. Its binary is `playwright-mcp`, not `mcp-server-playwright`. Install it with `npm i -g --ignore-scripts`.
- **Network.** Yes, because it drives a browser.
- **Cost.** `mcp-schema`, which shows as `unknown`.
- **Limits.** With `--ignore-scripts` no browser is downloaded. Run `npx playwright install` before its first use.

## CodeGraph (T6711)

A local code index the agent queries instead of grepping, per repo. See design.md, section 9.
