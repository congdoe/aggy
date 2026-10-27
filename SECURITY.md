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
