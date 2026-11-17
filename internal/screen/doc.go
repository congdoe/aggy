// Package screen keeps a virtual copy of a terminal screen, fed with the bytes
// a CLI writes to its pseudo-terminal: a grid of cells (a rune, its width, and
// whether it was drawn faint), the cursor's place and visibility, and whether
// the alternate screen is on. The supervisor feeds it the output it already
// forwards, and an adapter's classifier reads a Snapshot of it (T7016).
//
// It models only what a CLI's prompt can be told by: it keeps no colour, no
// other attribute, no scrollback, and no title. What it does not model and
// could move the cursor or change cells (a scroll region that is not the
// whole screen, a character set other than ASCII, an origin mode, a resize,
// bytes it lost) sets Unsure, which only a full clear of the screen (ED 2, or
// ED 0 from home) or a reset (RIS) clears: it never guesses.
//
// The model starts with the cursor at the top left of a blank screen, but the
// CLI starts wherever the real cursor was. Until a full clear, rows are known
// only relative to the CLI's own output: a CLI that draws with relative moves
// (Claude Code does) is modelled exactly; an absolute row (CUP, VPA), a cursor
// position report, or a move past what the CLI has drawn there makes the
// screen Unsure. Columns are always absolute.
//
// Memory is bounded by the screen size (MaxRows by MaxCols, larger sizes are
// Unsure) and the parser's fixed buffers; each byte costs at most one
// operation on the grid. It starts no process and opens no file.
package screen
