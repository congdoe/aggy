package supervisor

import (
	"encoding/json"
)

// The supervisor socket speaks newline-delimited JSON: one Request per
// line in, one Response per line out. A line longer than MaxMessage, or
// one that is not exactly one valid Request, gets an error Response and
// the connection is closed.

// MaxMessage caps one request line, newline excluded.
const MaxMessage = 64 << 10

// Request types.
const (
	ReqPing          = "ping"
	ReqHookEvent     = "hook-event"     // a vendor hook, forwarded by `aggy hook <event>`
	ReqSwitchRequest = "switch-request" // a linked skill asks for a switch
	// ReqToolsChanged says aggy tool on or off changed a setting (T6729).
	// It carries nothing: the supervisor works out itself whether its own
	// session's tools changed, and only offers a resume.
	ReqToolsChanged = "tools-changed"
	// ReqPromptCommand is a command a CLI's UserPromptSubmit hook saw
	// submitted (T6910): aggy hook blocks the prompt when the supervisor
	// takes it. It must come from the session, and the supervisor parses
	// the line again and never takes a keyboard-only command from it.
	ReqPromptCommand = "prompt-command"
	// ReqHermesPin asks the supervisor of a Hermes launch, with the launch's
	// token, whether a session id is its session, and for the decision plan's
	// path, SHA-256, and whether the policy is off (#134 review): the pin
	// lives in the supervisor's memory, never only in the index a tool can
	// write. Its payload is {"session_id": ...}.
	ReqHermesPin = "hermes-pin"
)

// Request is one message to the supervisor.
type Request struct {
	Type string `json:"type"`
	// Vendor is the vendor a hook-event comes from, or the vendor a
	// switch-request asks for.
	Vendor string `json:"vendor,omitempty"`
	// Event is the hook event name, such as Stop or StopFailure.
	Event string `json:"event,omitempty"`
	// Payload is the hook's JSON object, unchanged.
	Payload json.RawMessage `json:"payload,omitempty"`
	// FromSession is set by the server, never read from the wire: the
	// sender's process descends from the on-screen CLI (lineage.go). That
	// includes the CLI's own tool subprocesses, so it is a hint, not
	// proof that the CLI itself sent it.
	FromSession bool `json:"-"`
	// Chain is set with it: the sender's pid up to the on-screen CLI's, sender
	// first, as the lineage walk found them. It says how far below the CLI
	// the sender runs, which a session change is checked by (followsFrom).
	Chain []int `json:"-"`
	// New, Model, and Brief qualify a switch-request (T6665): a new child
	// instead of the latest, a model for that one launch, a brief.
	New   bool   `json:"new,omitempty"`
	Model string `json:"model,omitempty"`
	Brief bool   `json:"brief,omitempty"`
	// Tools are managed tools an agent asks to have on for the next
	// launch only (aggy switch --tool, T6708). The user sees them in the
	// switch's confirmation and confirms or cancels the whole switch.
	Tools []string `json:"tools,omitempty"`
	// Command is a prompt-command's line, as submitted.
	Command string `json:"command,omitempty"`
	// Token is a Hermes hook-event's launch token (T6933): Hermes runs its
	// hooks in its own daemon, outside the CLI's process tree, so aggy's hook
	// shows the token aggy run recorded for the launch instead
	// (Server.SetToken). Never logged.
	Token string `json:"token,omitempty"`
}

// Response answers one Request.
type Response struct {
	OK    bool   `json:"ok"`
	Type  string `json:"type,omitempty"` // "pong" for a ping
	Error string `json:"error,omitempty"`
	// Code is a refusal's reason code (docs/logging.md), so the sender
	// can print and record it too. The sender looks it up in the
	// registry and never trusts it further.
	Code string `json:"code,omitempty"`
	// Plan, PlanSum, and NoPolicy answer a hermes-pin: the launch's decision
	// plan and its SHA-256, or NoPolicy for a launch with --no-policy.
	Plan     string `json:"plan,omitempty"`
	PlanSum  string `json:"plan_sum,omitempty"`
	NoPolicy bool   `json:"no_policy,omitempty"`
}
