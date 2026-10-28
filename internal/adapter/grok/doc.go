// Package grok adapts the official Grok Build CLI (grok): headless grok -p
// --output-format json, whose reply carries sessionId, sessions through
// --session-id, --resume, and --fork-session, a tool allowlist per borrow
// kind through --tools, and images collected from
// sessions/<url-encoded cwd>/<sessionId>/images/. Facts and the
// unconfirmed fallbacks are in docs/adapters.md.
package grok
