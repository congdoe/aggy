// Package screen keeps a virtual copy of a terminal screen, fed with the bytes
// a CLI writes to its pseudo-terminal: a grid of cells (a rune, its width, and
// whether it was drawn faint), the cursor's place and visibility, and whether
// the alternate screen is on. The supervisor feeds it the output it already
// forwards, and an adapter's classifier reads a Snapshot of it (T7016).
//
// It models only what a CLI's prompt can be told by: it keeps no colour, no
// other attribute, no scrollback, and no title. It never guesses; what it does
// not follow makes the Snapshot Unsure, in three kinds (#149 review):
//
//   - Content that may differ from the real screen's (a resize, invalid
//     UTF-8, a move past what the CLI drew), per buffer, main or alternate,
//     until a full clear of that buffer: ED 2, ED 0 from home, or a redraw from
//     home that erases every row with EL 2 before it draws anything.
//   - A mode it does not model but knows the end of (insert mode, newline
//     mode, origin mode, a scroll region that is not the whole screen), until
//     it is turned off: a clear blanks cells, not modes. What was drawn while
//     it was on, and the cursor's place, stay in doubt as content after it,
//     and under a scroll region or origin mode no clear counts.
//   - Anything else (a mode it does not know, a character set switch it cannot
//     follow, a sixel, bytes its owner lost and never fed): lost, until a
//     reset (RIS). Lost bytes may have hidden the cursor or ended faint text,
//     which no clear restores.
//
// The model starts with the cursor at the top left of a blank screen, but the
// CLI starts wherever the real cursor was. Until a full clear, rows are known
// only relative to the CLI's own output: a CLI that draws with relative moves
// (Claude Code does) is modelled exactly; an absolute row (CUP, VPA) followed
// by a change of cells, a cursor position report, or a move past what the CLI
// has drawn there makes the screen Unsure. A full clear with the row known, or
// an absolute row on a buffer blank since a full clear, makes that buffer's
// rows real. Columns are always absolute.
//
// Memory is bounded by the screen size (MaxRows by MaxCols, larger sizes are
// Unsure) and the parser's fixed buffers. Work is bounded per Feed call: one
// short sequence can touch the whole screen (a clear, a reset, a scroll of
// every row), so each call may touch at most 256 cells per byte plus four
// whole screens; past that the rest of the call is not followed and the
// screen is lost until a reset. It starts no process and opens no file.
package screen
