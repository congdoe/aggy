# Fixtures

Real CLI outputs captured with `aggy dev capture` and cleaned with `aggy dev scrub`. Never commit a fixture that contains a home path, an email, a token, your user or host name, a real session UUID, or a binary file.

## Recording one

`aggy dev capture` runs the real vendor CLI once, so it may spend quota. It is for maintainers, runs only inside the aggy repo, and refuses to run without `--i-know-this-runs-the-real-cli`:

```sh
aggy dev capture codex --i-know-this-runs-the-real-cli --name image-two -- exec --json --sandbox read-only -- "draw a cat"
```

It runs the CLI through the probe runner (no stdin, a scrubbed environment, a fresh temp working dir, a time limit) and writes `testdata/fixtures/<vendor>/<name>/`. `--name` resolves against the repo root, and `--out` must also be under `testdata/fixtures`. Neither `testdata` nor `testdata/fixtures` may be a symlink.

| File | Holds |
| --- | --- |
| `stdout`, `stderr` | What the CLI printed (left empty if it was not text). |
| `meta.json` | Vendor, arguments, exit code, the files copied, and the files skipped with why. |
| `files/vendor_home/...` | Text files the run created or changed in the vendor home that belong to the run's session, matched against `devfix.SessionPaths`: `generated_images/<id>/*` and `sessions/*/*/*/rollout-*<id>.jsonl` for Codex, `sessions/*/<id>/...` for Grok, `projects/*/<id>.jsonl` for Claude (the transcript paths are unconfirmed). The session id comes from the vendor's structured field, read by the adapter's `ParseHeadless` (Claude `session_id`, Codex `thread.started`, Grok `sessionId`). Without one, no vendor-home file is copied: there is no fallback to a UUID found in the output, so a reply that mentions another session, even as the only UUID it holds, never copies its transcript. At most 200 files of 1 MiB each. |
| `files/cwd/...` | Text files the run wrote in its working dir. |

Binary files, such as generated images, are never copied; they are listed as skipped. The fixture is then scrubbed. Review it before committing.

## Scrubbing

`aggy dev scrub <dir>` rewrites every text file in a fixture under `testdata/fixtures` (anywhere else only with `--anywhere`). It reads every file before writing any, so a file it cannot read changes nothing.

| Found | Becomes |
| --- | --- |
| your home dir and `/Users/<you>`, `/home/<you>`, also URL-encoded or in Claude's dash form (every non-alphanumeric becomes `-`); `/Users/<name>`, `/home/<name>`, `/root`, `C:\Users\<name>`; their `%2F` forms, however often the `%` is itself escaped (`%252F`), and JSON-escaped `\/` forms; `-Users-<name>` and `-home-<name>` path elements | `$HOME` |
| emails (except `example.com`, `.org`, `.net`) | `user@example.com` |
| your user name as `you@host` or an email's local part, as `~you`, or as `/Volumes/you`, and your host name as a whole word, in any case. Never as a bare word: many account names are ordinary words, so a user name in prose is a known limit. Generic names (`runner`, `root`, `ubuntu`, `user`, `admin`, `vscode`, `codespace`, `build`, `localhost`) and names under 3 characters identify no one on their own; their home paths are still caught | `fixture-user`, `fixture-host` |
| UUIDs such as session ids | `00000000-0000-4000-8000-000000000001`, numbered by first appearance, so one id keeps one placeholder |
| token-shaped strings: `sk-` and `xai-` keys, GitHub, Slack, Stripe, and Google keys and tokens, AWS access key ids and secret keys, JWTs, PEM private keys, basic and bearer authorization, `token=` and `secret:` assignments | `REDACTED-TOKEN` |

Scrub then reports anything still present and exits 1.

## The check

`TestFixturesHoldNoSecrets` in `internal/devfix`, which `make test` and CI run, fails when any file under this directory holds, in its content or its path, a home path (decoded until stable from `%2F` forms, and from JSON's `\/`), an email, a token-shaped string, the current user name as `you@host`, an email local part, `~you`, or `/Volumes/you`, the current host name, or a UUID that is not a scrub placeholder. It uses the same detectors as scrub. A binary file fails on its own, since its bytes are neither scrubbed nor checked. Known limit: a token wrapped in base64 is not found.

A documented false positive goes in [scrub-allowlist.txt](scrub-allowlist.txt), one `<path glob><TAB><exact match>` per line with a comment saying why it is safe. The literal `(binary)` lets a binary file through. The allowlist itself is not checked.
