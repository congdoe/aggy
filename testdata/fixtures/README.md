# Fixtures

Real CLI outputs captured with `aggy dev capture` and cleaned with `aggy dev scrub`. Never commit a fixture that contains a home path, an email, or a token.

## Recording one

`aggy dev capture` runs the real vendor CLI once, so it may spend quota. It is for maintainers and refuses to run without `--i-know-this-runs-the-real-cli`:

```sh
aggy dev capture codex --i-know-this-runs-the-real-cli --name image-two -- exec --json --sandbox read-only "draw a cat"
```

It runs the CLI through the probe runner (no stdin, a scrubbed environment, a fresh temp working dir, a time limit) and writes `testdata/fixtures/<vendor>/<name>/`:

| File | Holds |
| --- | --- |
| `stdout`, `stderr` | What the CLI printed. |
| `meta.json` | Vendor, arguments, exit code, the files copied, and the files skipped with why. |
| `files/vendor_home/...` | Files the run created or changed in the vendor home (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `GROK_HOME`, else `~/.<vendor>`), up to 200 files of 1 MiB each. Credential files are refused by `safefs` and listed as skipped. |
| `files/cwd/...` | Files the run wrote in its working dir. |

The fixture is then scrubbed. Review it before committing.

## Scrubbing

`aggy dev scrub <dir>` rewrites every text file in place:

| Found | Becomes |
| --- | --- |
| your home dir, `/Users/<name>`, `/home/<name>`, `/root`, `C:\Users\<name>` | `$HOME` |
| emails (except `example.com`, `.org`, `.net`) | `user@example.com` |
| your user name and host name, as whole words | `fixture-user`, `fixture-host` |
| UUIDs such as session ids | `00000000-0000-4000-8000-000000000001`, numbered by first appearance, so one id keeps one placeholder |
| token-shaped strings (`sk-`, `xai-`, GitHub, Slack, and AWS keys, JWTs, bearer tokens, `token=`/`secret:` assignments) | `REDACTED-TOKEN` |

Binary files are not rewritten. Scrub then reports anything still present and exits 1.

## The check

`TestFixturesHoldNoSecrets` in `internal/devfix`, which `make test` and CI run, fails when any file under this directory holds a home path, an email, or a token-shaped string in its content or its path. It uses the same detectors as scrub. A documented false positive goes in [scrub-allowlist.txt](scrub-allowlist.txt), one `<path glob><TAB><exact match>` per line with a comment saying why it is safe.
