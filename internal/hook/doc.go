// Package hook is the entry point vendor hooks call (aggy hook <event>).
// It parses JSON payloads and forwards them to the supervisor over a local
// socket. Payloads never reach a shell.
package hook
