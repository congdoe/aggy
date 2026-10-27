# Contributing

Thanks for helping. aggy is early, and the design and adapter guides will land in `docs/`.

## Ground rules

- aggy never reads vendor credentials, proxies vendor traffic, or sends model requests. Changes that would are out of scope.
- Every change aggy makes to another tool's config goes through the manifest, so `aggy unwire` can undo it.
- Tests never need a vendor login. Use the fake CLIs in `testdata/fakes`.
- Scrub recorded fixtures of home paths, emails, and tokens before committing them.

## Sign-off

Commits need a Developer Certificate of Origin sign-off (`git commit -s`). There is no CLA.

## Development

```sh
make test    # go vet and go test -race
make build   # builds ./bin/aggy
```
