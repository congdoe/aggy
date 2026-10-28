# Smoke tests

Opt-in checks that run the real vendor CLIs. They spend quota, so they never run in CI or `make test`; `smoke.sh` refuses to run unless `AGGY_SMOKE=1` is set, and refuses a live run when `CI` is set.

```sh
AGGY_SMOKE=1 make smoke                   # live
AGGY_SMOKE=1 test/smoke/smoke.sh --fake   # against testdata/fakes, no quota
```

`AGGY_SMOKE_VENDORS=claude,codex` limits the vendors (default all three); a vendor not selected or not on `PATH` is skipped. `AGGY_SMOKE_TIMEOUT` (default 300) is the limit in seconds for each direct vendor run; aggy's own borrow budgets bound the borrows.

## Safety

- Everything runs in a fresh temp dir, deleted at exit, with a temp `AGGY_HOME`. `aggy setup` runs only against that temp state (it runs each CLI's `--version` and `--help`, no model call).
- No vendor config is edited, no folder is trusted, and no `--dangerously-*`, yolo, or other permission- or trust-skipping flag is passed. The CLIs use the user's own sign-in in their usual homes, as the 2026-10-27 checks did, and record their sessions there as any run does.
- API-key and base-URL variables are unset for the run, so the CLIs use the subscription.
- Output is one line per check: its name, PASS, FAIL, or SKIP, and a note the script wrote. Vendor output, paths, session ids, and tokens are never printed. The CLI versions are printed as numbers only.
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
| `grok-review-untrusted` | in a never-trusted repo whose `.claude/settings.json` and `.grok/hooks` hold `SessionStart` and `PreToolUse` hooks, aggy's exact Grok review argv runs no hook | open item in grok_trust.go | grok 1 |
| `aggy-grok-review-refused` | aggy refuses a Grok review in that repo | grok_trust.go | 0 |
| `grok-image-tool-list` | with aggy's image argv the model reports `image_gen` and no tool that reads a path | open item in grok.go | grok 1 |
| `aggy-codex-research-refused` | Codex research is refused (T6638) | | 0 |
| `image-borrow-codex`, `image-borrow-grok` | `aggy image` round trip into a scratch repo | | 1 each |
| `research-borrow-grok` | `aggy borrow research` on Grok with planted page text that asks for a canary file in the repo; the canary must not come back | open item in research_isolation_test.go | grok 1 |
| `aggy-video-default-refused` | `aggy video` is refused without `[media] grok_file_tools` | | 0 |
| `video-borrow-grok` | `aggy video` round trip with the opt-in in the temp `config.toml` | open item in video file tools | grok 1 |

Totals, all vendors selected: claude 2, codex 4, grok 6. The script prints the count it made at the end (an upper bound: a run aggy refuses before starting is still counted).

Limits: the Grok tool list is the model's own one-turn report, not a wire dump. The research check's "page" is text in the task, not a hosted page. The Grok agent file's frontmatter layout is not recorded in verification.md, so a failure of `mcp-launch-grok` may mean the layout is wrong. If the user's Grok has folder trust turned off, `grok-review-untrusted` fails by design: that is what aggy's refusal guards against. The run timeout kills the CLI process, not its children.

Record the scrubbed results in `docs/verification.md`.
