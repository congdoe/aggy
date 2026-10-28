// Package parity keeps instructions, shared memory, and the safety policy
// the same across the vendor CLIs. What fits on a CLI's command line goes
// there at launch; anything else is a vendor config change, made through
// a manifest that aggy unwire can undo.
package parity
