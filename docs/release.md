# Releasing aggy

Releases are cut by the owner only. Pushing a `v*` tag runs the release
workflow, which makes a draft release; nothing is public until the owner
publishes the draft.

## What is set up

- `.goreleaser.yaml` builds `aggy` for darwin and linux, amd64 and arm64,
  with `CGO_ENABLED=0` and `-trimpath`, archives them with `LICENSE` and
  `README.md`, writes `checksums.txt`, and makes a **draft** GitHub
  release.
- Its `homebrew_casks` section renders a cask for the tap
  `congdoe/homebrew-aggy` (`Casks/aggy.rb`). goreleaser v2 deprecated
  `brews` in v2.10 in favour of `homebrew_casks`
  (<https://goreleaser.com/deprecations/#brews>).
- `.github/workflows/release.yml` runs goreleaser v2.19.0 and attests
  every archive and `checksums.txt` with Sigstore build provenance. It
  runs on a pushed `v*` tag.
- CI runs `goreleaser check` and a snapshot build on every pull request.
  Locally: `make release-check` and `make snapshot` (the snapshot writes
  the cask to `dist/homebrew/Casks/aggy.rb` and pushes nothing).

## Owner checklist for v0.1.0

1. **macOS Gatekeeper: decided (owner, 2026-10-29).** The binaries are
   not code-signed or notarized, and the cask does nothing about
   quarantine, so on macOS Gatekeeper blocks the first run of a
   `brew install`. v0.1.0 ships as is: the README documents the manual
   step (`xattr -d com.apple.quarantine "$(brew --prefix)/bin/aggy"`) and
   points macOS users at `go install`, which builds locally and is not
   quarantined. Signing and notarizing (goreleaser v2's `notarize`
   section, with an Apple Developer ID certificate and API key as
   secrets) is revisited for 1.0. A cask hook that strips the quarantine
   flag was not chosen: it would skip Gatekeeper for every user without
   them deciding to.
2. Create the tap repo `congdoe/homebrew-aggy` (public, default branch
   `main`, any README). Homebrew finds it as `congdoe/aggy`.
3. Create a fine-grained token with **Contents: read and write** on
   `congdoe/homebrew-aggy` only, and add it to `congdoe/aggy` as the
   Actions secret `HOMEBREW_TAP_GITHUB_TOKEN`.
4. Run the live smoke checks and record the results in
   `docs/verification.md`: `AGGY_SMOKE=1 make smoke`
   ([test/smoke/README.md](../test/smoke/README.md) lists what it spends).
   `make release-pinned` must pass: every built-in tool pinned to a
   version. The release workflow runs it before goreleaser and stops on
   a failure.
5. The release workflow is enabled: it runs on a pushed `v*` tag. Push
   no `v*` tag before steps 2 to 4 are done.
6. Test a release candidate first:
   `git tag -a v0.1.0-rc.1 -m v0.1.0-rc.1 && git push origin v0.1.0-rc.1`.
   (Use `-s` instead of `-a` for a signed tag once a signing key is set
   up, such as `git config gpg.format ssh` and `user.signingkey`.)
   The workflow makes a draft release and pushes the cask to the tap.
   Check the draft's assets and attestations
   (`gh attestation verify <file> --repo congdoe/aggy`), then publish it
   **as a pre-release** (the cask's download URLs work only once it is
   published; a pre-release is not shown as the latest release).
7. On a clean Mac and a Linux machine, install the candidate both ways,
   `brew install congdoe/aggy/aggy` and
   `go install github.com/congdoe/aggy/cmd/aggy@v0.1.0-rc.1`, and run
   `aggy version` and `aggy setup`. On the Mac, confirm the step 1
   decision works. Fix and repeat with `-rc.2` as needed. The tap holds
   the candidate's cask until the final tag replaces it.
8. Tag the release: `git tag -a v0.1.0 -m v0.1.0 && git push origin v0.1.0`,
   check the draft as in step 6, and publish it as the latest release.
9. Check `brew upgrade congdoe/aggy/aggy` and
   `go install github.com/congdoe/aggy/cmd/aggy@latest` pick up v0.1.0.

## go install

`go install github.com/congdoe/aggy/cmd/aggy@latest` works: the module path is `github.com/congdoe/aggy`, the module has
no `replace` directives, and no package uses cgo (`make nocgo` checks
it), so it builds with any C toolchain or none. It needs the Go version
in `go.mod`. A binary built this way has no release ldflags, so `aggy version`
reports the module version Go recorded.

## Release notes

Notes for the first release's changelog, and for maintainers' own dev
installs before it:

- **Dev indexes at schema version 12.** Migration `0012_worktrees.sql`
  changed during review (it gained `worktree_tree`) after some maintainer
  dev indexes had already applied it, so such an index is at version 12
  without that column and edit borrows fail on it. Migrations are not
  re-run by content, so move or delete that dev `~/.aggy/aggy.db` (and its
  `-wal` and `-shm` files) and let aggy make a fresh one. No released
  build has shipped version 12, so users are not affected.
- **Self-updaters are killed with the borrow.** aggy kills a probe's or
  borrow's whole process group when the vendor CLI exits. A CLI that
  starts a self-updater in its own process group without detaching it
  (`setsid`) during a probe or borrow has that updater killed too; the
  update then runs on the CLI's next interactive start.
