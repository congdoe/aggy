# aggy

One command for Claude Code, Codex, and Grok Build.

aggy runs the official CLIs you already pay for and adds three things:

- **Switch.** When one vendor hits its limit, continue in another with a handoff brief. Come back later with only what changed.
- **Borrow.** Hand one task to the vendor best suited to it (image, video, research, news, review, or edit) and get a short report back.
- **Parity.** Every CLI sees the same project rules, memory, task state, and safety settings.

> **Status: pre-alpha.** Nothing works yet. The first usable release is v0.1.0, which adds read-only borrows.

## What aggy never does

- It never reads, copies, or stores credentials. Each CLI signs in with its own flow.
- It never sends model requests or proxies vendor traffic. Only the official CLIs talk to their vendors.
- It collects no telemetry. Everything aggy writes stays on your machine.

## Known limits

- The shared memory (`.aggy/memory.md`) reaches Codex and Grok on their command line, which other local users can read with `ps`. So does the payload Codex passes to its legacy `notify` hook. Keep secrets out of the memory; see [docs/design.md](docs/design.md), section 13.
- The safety policy is a guardrail against an agent's mistakes, not a sandbox.

aggy is not affiliated with Anthropic, OpenAI, or xAI. You are responsible for following each vendor's terms.

## Development

```sh
make test    # go vet and go test -race
make build   # builds ./bin/aggy
```

## License

[MIT](LICENSE)
