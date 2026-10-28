# Smoke tests

Opt-in checks that run the real vendor CLIs. They spend quota, so they never run in CI or `make test`; `smoke.sh` refuses to run unless `AGGY_SMOKE=1` is set, and refuses a live run when `CI` is set.

```sh
AGGY_SMOKE=1 make smoke                   # live
AGGY_SMOKE=1 test/smoke/smoke.sh --fake   # against testdata/fakes, no quota
```

`AGGY_SMOKE_VENDORS=claude,codex` limits the vendors (default all three); a vendor not selected or not on `PATH` is skipped. The Grok checks run only on grok 1.0.70, the version every Grok fact here was checked on; on another version they are skipped, unless `AGGY_SMOKE_ANY_VERSION=1` runs them with a warning.

**Bounds.** Wall time is the real bound. Each direct vendor run is killed after `AGGY_SMOKE_TIMEOUT` seconds (default 300); each aggy borrow stops at its kind's budget (image 5 minutes, video 10, research 10 with 30 turns) and the script kills it after 1200 seconds anyway. Turn limits exist only where the CLI has one: Grok's `--max-turns` (docs/adapters.md, Grok flags) is not passed to the direct Grok MCP run (MCP tools are deferred, so it needs several turns; 3 were too few to rule out), nor to the review and image runs, which must use aggy's exact argv (`--prompt-file <file>` and the rest; grok 1.0.70's `-p` is `--single <PROMPT>` and takes the prompt as its value); Claude's turn flag is not recorded and Codex `exec` has none.

## Safety

- Everything runs in a fresh temp dir, deleted at exit (kept, and its path printed, with `AGGY_SMOKE_KEEP=1`, so each check's stdout and stderr can be read), with a temp `AGGY_HOME`. `aggy setup` runs only against that temp state (it runs each CLI's `--version` and `--help`, no model call).
- Every vendor CLI and aggy run gets a fresh environment (`env -i`) with only `HOME`, `PATH`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_*`, `TMPDIR`, `TERM=dumb`, `NO_COLOR=1`, `AGGY_HOME`, and `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `GROK_HOME` when you set them, as aggy gives a borrow child. Proxies, a surrounding Claude Code session's variables, and Grok's folder-trust and hook switches do not leak in.
- The script refuses to start while an API-key or route variable is set (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `OPENAI_API_KEY`, `CODEX_API_KEY`, `XAI_API_KEY`, or any `*_BASE_URL`), and names it.
- No vendor config is edited, no folder is trusted, and no `--dangerously-*`, yolo, or other permission- or trust-skipping flag is passed. The CLIs use the user's own sign-in in their usual homes, as the 2026-10-27 checks did, and record their sessions there as any run does.
- Output is one line per check: its name, PASS, FAIL, SKIP, or INCONCLUSIVE, and a note the script wrote. The exit status is 0 only when nothing failed and nothing was inconclusive. A FAIL also prints the last 5 lines of that check's stderr, indented under it, with terminal escapes and control characters removed and key-shaped text replaced (`Authorization` values, Bearer/Basic/Token credentials, the values of secret-named flags and keys in any quoting and of any length, known key prefixes, runs of 24 or more key characters; `scrub_test.go` checks it), the home directory shown as `~`, and the temp dir as `$WORK`; that is vendor or aggy error text, so read it before pasting the log publicly. Nothing else from the vendors is printed. The CLI versions are printed as numbers only.
- The fake mode builds the fakes, puts them first on `PATH`, and sets `HOME` to a temp dir, so a real CLI is never reached.

## Checks and cost

Vendor runs are one-turn, minimal prompts. A borrow counts as one run, though the child may take several agent turns inside its budget.

| Check | What it shows | Source | Runs |
| --- | --- | --- | --- |
| `aggy-setup` | setup records sign-in status in the temp state | | 0 |
| `claude-append-prompt` | `--append-system-prompt-file` reaches the model | verification.md T6619 | claude 1 |
| `codex-hook-trust-gate` | a `SessionStart` hook passed with `-c` is skipped | verification.md T6616 | codex 1 |
| `codex-research-isolation` | with the clean lockdown set, a canary one level up is not read | verification.md T6618 | codex 1 |
| `mcp-launch-claude`, `-codex`, `-grok` | launch-time MCP returns a stdio server's token (needs `python3`) | verification.md T6734 | 1 each |
| `grok-review-untrusted` | in a never-trusted repo whose `.claude/settings.json` and `.grok/hooks` hold `SessionStart` and `PreToolUse` hooks, aggy's exact Grok review argv runs no hook, while the model is shown to call `list_dir`; a control repo run with `GROK_FOLDER_TRUST=0` (trust off, so Trusted, and no trust-store write; sources in smoke.sh) must fire them. PASS needs both; a control that fires nothing is INCONCLUSIVE | open item in grok_trust.go | grok 2 |
| `aggy-grok-review-refused` | aggy refuses a Grok review in that repo, with the folder-trust reason (any other error is a FAIL) | grok_trust.go | 0 (grok 1 if it was not refused) |
| `grok-image-tool-list` | with aggy's image argv the model reports `image_gen` and no tool that reads a path | open item in grok.go | grok 1 |
| `aggy-codex-research-refused` | Codex research is refused with the T6638 reason | | 0 |
| `image-borrow-codex`, `image-borrow-grok` | `aggy image` round trip into a scratch repo | | 1 each |
| `research-borrow-grok` | `aggy borrow research` on Grok with planted page text that asks for a canary file in the repo; the canary must not come back | open item in research_isolation_test.go | grok 1 |
| `aggy-video-default-refused` | `aggy video` is refused without `[media] grok_file_tools`, with that hint (any other error is a FAIL) | | 0 |
| `video-borrow-grok` | `aggy video` round trip with the opt-in in the temp `config.toml` | open item in video file tools | grok 1 |

Totals, all vendors selected: claude 2, codex 4, grok 7. The script prints the count it made at the end (an upper bound: a run aggy refuses before starting is still counted).

Limits: the Grok tool list is the model's own one-turn report, not a wire dump. The research check's "page" is text in the task, not a hosted page. The Grok agent file gives `mcpServers` as a YAML list (`- aggy: {command: python3, args: [...]}`), the layout grok-src parses (verification.md, re-run on 7c86f6c); a map fails to load. If the user's Grok has folder trust turned off, `grok-review-untrusted` fails by design: that is what aggy's refusal guards against. The run timeout kills the CLI process, not its children. Whether Grok fires `SessionStart` in headless mode is unverified; the control shows which hooks fired. The review argv carries `--sandbox strict`, which allows writes only to Grok's sessions dir, so hooks inside it cannot write their markers (the 7c86f6c run read INCONCLUSIVE that way). The control therefore runs the same argv without `--sandbox strict`, to show a hook would fire; the untrusted run keeps aggy's exact argv, sandbox included. The image tool-list check reads only the reply's `text` field (with `python3`), since `thought` may name tools the model does not have.

Record the scrubbed results in `docs/verification.md`.
