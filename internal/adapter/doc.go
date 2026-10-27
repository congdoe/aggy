// Package adapter defines the interface every vendor CLI implements.
// aggy never calls a vendor CLI except through an Adapter. Adapters build
// commands and parse output; they never start processes. The contract is
// documented in docs/adapters.md.
//
// The interface is unstable until M1. Known gaps, to be added as the vendor
// adapters are built:
//
//   - parsing hook payloads: Stop, StopFailure, and the Codex
//     agent-turn-complete notify
//   - discovering the session id of an interactive launch (Codex cannot
//     pre-assign one)
//   - detecting a plan limit in an interactive session
//   - the exit sequence the supervisor types to end a session at turn end
//   - the inputs ConfigPatches needs: tool toggles, the safety policy, and
//     the MCP server list
//   - patches that create symlinks or delete files
package adapter
