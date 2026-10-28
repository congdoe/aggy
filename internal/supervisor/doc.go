// Package supervisor runs the on-screen vendor CLI in a pseudo-terminal,
// passes it through unchanged, and performs vendor switches at turn end.
//
// It never parses what passes through: bytes go both ways untouched, so
// bracketed paste, the kitty keyboard protocol, mouse reports, OSC 52, 8,
// 9, and 777, truecolor, and wide characters work as the CLI and the
// terminal agree. It starts no process itself: the caller passes Start,
// which internal/cli builds from internal/execrun.
package supervisor
