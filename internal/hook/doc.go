// Package hook is the entry point vendor hooks call (aggy hook <event>).
// It parses JSON payloads and, from M2, forwards them to the supervisor
// over a local socket. Payloads never reach a shell.
//
// PreToolUse is the guard wired into delegated (borrow) children: it
// denies shell commands that would start another vendor CLI headless or
// another borrow. It is an accident guard, not a security boundary.
package hook
