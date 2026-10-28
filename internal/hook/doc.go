// Package hook is the logic behind aggy hook <event>, which vendor hooks
// call. Payloads are parsed as JSON and never reach a shell.
//
// Forwarded events (event.go) keep only the payload fields aggy uses
// and go to the supervisor over its socket; Forward decides which
// installed hook speaks for the vendor on screen. Install (install.go)
// merges aggy's hook entries into a vendor's hook config.
//
// PreToolUse is the guard wired into delegated (borrow) children: it
// denies shell commands that would start another vendor CLI headless or
// another borrow. It is an accident guard, not a security boundary.
package hook
