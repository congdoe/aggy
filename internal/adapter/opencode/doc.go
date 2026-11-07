// Package opencode builds OpenCode's commands and reads its output. It
// holds every OpenCode flag, variable, output format, and session id rule
// aggy depends on, each checked at 2.2.5 (docs/verification.md, "OpenCode
// 2.2.5 as an on-screen harness"), and the shipped plugin, which it only
// hands over as bytes. It starts no process and writes no file.
//
// OpenCode runs on screen only (docs/design.md, section 15): Headless
// refuses, until a lockdown passes the same read test as the others.
package opencode
