// Package permissions reads the rules a person approved, in Claude Code's
// permissions format, from the places aggy trusts as theirs, and filters
// them before any reach a CLI (docs/design.md, section 10). Committed rules
// and a repo's .aggy/config.toml never become allows. It starts nothing:
// whether git tracks a file is the caller's answer (Input.Tracked).
package permissions
