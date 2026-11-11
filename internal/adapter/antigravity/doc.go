// Package antigravity builds Google's Antigravity CLI (agy) commands for an
// on-screen session and renders the two files aggy hands it: the per-run
// --settings file with aggy's deny and ask rules, and aggy's one named entry
// in the workspace hooks file, <repo>/.agents/hooks.json. agy is closed
// source, so every flag, key, payload field, and message here is cited as a
// live check recorded in docs/verification.md ("Antigravity CLI"), as
// Google's docs read 2026-11-11 and not run, or marked unverified. It starts
// no process and writes no file.
//
// Antigravity runs on screen only (docs/design.md, section 15,
// "Antigravity"): Headless refuses, and print mode is refused at the flag
// table, since Google's FAQ names wrapping agy headless as not supported
// under consumer plans.
package antigravity
