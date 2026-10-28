# Releasing aggy

Releases are cut by the owner only. The config is in place, and
publishing stays off until the owner turns it on.

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
  every archive and `checksums.txt` with Sigstore build provenance. It is
  disabled: it runs only by hand and its job has `if: ${{ false }}`.
- CI runs `goreleaser check` and a snapshot build on every pull request.
  Locally: `make release-check` and `make snapshot` (the snapshot writes
  the cask to `dist/homebrew/Casks/aggy.rb` and pushes nothing).

## Owner checklist for v0.1.0

1. Make the repo public (`go install` and the release URLs need it).
2. Create the tap repo `congdoe/homebrew-aggy` (public, default branch
   `main`, any README). Homebrew finds it as `congdoe/aggy`.
3. Create a fine-grained token with **Contents: read and write** on
   `congdoe/homebrew-aggy` only, and add it to `congdoe/aggy` as the
   Actions secret `HOMEBREW_TAP_GITHUB_TOKEN`.
4. Run the live smoke checks and record the results in
   `docs/verification.md`: `AGGY_SMOKE=1 make smoke`
   ([test/smoke/README.md](../test/smoke/README.md) lists what it spends).
5. Enable the release workflow: in `release.yml`, change the trigger to
   `push: tags: ["v*"]` and remove the job's `if: ${{ false }}`. Merge
   that change.
6. Tag and push: `git tag -s v0.1.0 -m v0.1.0 && git push origin v0.1.0`.
7. The workflow makes a draft release and pushes the cask to the tap.
   The cask's download URLs work only once the release is published, so
   check the draft's assets and attestations
   (`gh attestation verify <file> --repo congdoe/aggy`) and publish it
   right away.
8. Check both install paths on a clean machine:
   `brew install congdoe/aggy/aggy` and
   `go install github.com/congdoe/aggy/cmd/aggy@v0.1.0`, then
   `aggy version`.

The binaries are not code-signed or notarized. Homebrew may quarantine
a cask's binary on macOS; signing, or a cask `postflight` hook that
removes the quarantine attribute, is an owner decision not made here.

## go install

`go install github.com/congdoe/aggy/cmd/aggy@latest` works once the repo
is public: the module path is `github.com/congdoe/aggy`, the module has
no `replace` directives, and no package uses cgo (`make nocgo` checks
it), so it builds with any C toolchain or none. It needs the Go version
in `go.mod`. A binary built this way reports the module version Go
records (`aggy version`), since it has no release ldflags.
