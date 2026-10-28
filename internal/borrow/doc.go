// Package borrow runs one bounded task headless in another vendor's CLI:
// aggy borrow <kind> "<task>". It builds the prompt (the task, the
// caller's paths and constraints, and today's date, never the parent
// transcript), records the child and the borrow in the store, asks a
// Router for the vendor, and runs the adapter's command through the
// adapter.Runner the caller passes: this package never starts a process
// itself. Isolation, budgets, reports, and the inbox live here too.
package borrow
