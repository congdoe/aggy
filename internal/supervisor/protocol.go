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
	// New, Model, and Brief qualify a switch-request (T6665): a new child
	// instead of the latest, a model for that one launch, a brief.
	New   bool   `json:"new,omitempty"`
	Model string `json:"model,omitempty"`
	Brief bool   `json:"brief,omitempty"`
}

// Response answers one Request.
type Response struct {
	OK    bool   `json:"ok"`
	Type  string `json:"type,omitempty"` // "pong" for a ping
	Error string `json:"error,omitempty"`
}
