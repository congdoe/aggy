// Package borrow runs one bounded task headless in a vendor's CLI (for a
// review, possibly the one on screen, in a fresh session):
// aggy borrow <kind> "<task>". It builds the prompt (the task, the
// caller's paths and constraints, and today's date, never the parent
// transcript), records the child and the borrow in the store, asks a
// Router for the vendor, and runs the adapter's command through the
// adapter.Runner the caller passes: this package never starts a process
// itself. Isolation, budgets, reports, and the inbox live here too.
//
// # The PreToolUse guard
//
// Each child runs with AGGY_DELEGATED=1, so its own aggy borrow refuses
// (one hop). A child with a shell could still run claude -p, codex exec,
// or grok -p directly, so where a CLI takes a per-run hook the child also
// gets the guard (aggy hook pretooluse), which denies those commands. It
// is an accident guard, not a security boundary.
//
//   - Claude: a settings layer in the run dir, passed with --settings,
//     when the probe found that flag.
//   - Codex: none. Hooks need hash trust, and hooks passed with -c do not
//     run untrusted. TODO(M2 hooks).
//   - Grok: none: no per-run hook flag is known. Grok does load Claude's
//     own settings.json hooks by default. TODO(M2 hooks).
//
// In M1 no kind keeps a shell: Claude review keeps Read, Grep, and Glob;
// Codex disables shell_tool and unified_exec for every kind; Grok denies
// run_terminal_cmd for every kind. The guard matters only if a lockdown
// flag fails open, and for M3's edit.
package borrow
