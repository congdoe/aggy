# Security policy

aggy runs next to coding agents that can execute shell commands, so security reports are welcome.

## Reporting a vulnerability

Report privately through GitHub: **Security → Report a vulnerability** on this repository. Please do not open a public issue.

Include the aggy version (`aggy version`), the vendor CLI versions, your OS, and steps to reproduce.

## Of particular interest

- Anything that makes aggy read a vendor credential file
- Hook payloads or webhook text reaching a shell
- A borrowed session reading files or reaching the network beyond its kind's limits
- A config change that `aggy unwire` cannot undo
- A key stored with `aggy key` leaving the OS keychain other than to the OpenCode launch a person started: in an argv or process listing, an environment variable, a log, an error, a panic, a core dump, a brief, a file other than that launch's key file, or a key file left behind after aggy exits normally (a crash leaves it until the next aggy launch); reaching Claude Code, Codex, Grok, Hermes, or a borrow; or a repo or a model's shell getting aggy to read one or hand it to anything else
