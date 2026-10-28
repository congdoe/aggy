// Package checkup holds the checks aggy setup and aggy doctor share: each
// vendor CLI's install, version, and sign-in status, asked of the CLI
// through its adapter and a caller-supplied runner. It starts no process
// itself, reads files only through internal/safefs, and never reads,
// prints, or stores a credential or an environment value.
package checkup
