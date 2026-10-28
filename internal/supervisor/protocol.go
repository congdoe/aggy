package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/congdoe/aggy/internal/adapter"
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
}

// Response answers one Request.
type Response struct {
	OK    bool   `json:"ok"`
	Type  string `json:"type,omitempty"` // "pong" for a ping
	Error string `json:"error,omitempty"`
}

var eventRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// decodeRequest parses and checks one request line.
func decodeRequest(line []byte) (Request, error) {
	var r Request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("bad request: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return r, errors.New("bad request: more than one JSON value on the line")
	}
	switch r.Type {
	case ReqPing:
		if r.Vendor != "" || r.Event != "" || r.Payload != nil {
			return r, errors.New("bad request: ping takes no fields")
		}
	case ReqHookEvent:
		if _, err := adapter.ParseVendor(r.Vendor); err != nil {
			return r, err
		}
		if !eventRE.MatchString(r.Event) {
			return r, fmt.Errorf("bad request: hook event name %q", r.Event)
		}
		if p := bytes.TrimSpace(r.Payload); len(p) == 0 || p[0] != '{' {
			return r, errors.New("bad request: a hook payload must be a JSON object")
		}
	case ReqSwitchRequest:
		if _, err := adapter.ParseVendor(r.Vendor); err != nil {
			return r, err
		}
		if r.Event != "" || r.Payload != nil {
			return r, errors.New("bad request: switch-request takes only vendor")
		}
	default:
		return r, fmt.Errorf("bad request: unknown type %q", r.Type)
	}
	return r, nil
}

// answer handles one request line. onHook, if set, gets each valid
// hook-event, with FromSession set to fromSession. A switch-request must
// be FromSession.
func answer(line []byte, fromSession bool, onHook func(Request)) Response {
	r, err := decodeRequest(line)
	if err != nil {
		return Response{Error: err.Error()}
	}
	switch r.Type {
	case ReqPing:
		return Response{OK: true, Type: "pong"}
	case ReqHookEvent:
		// TODO(T6664): turn-end hooks gate a pending switch, and a
		// rate_limit StopFailure opens the picker, both only when
		// FromSession (fail closed). Until then onHook keeps the index
		// (session ids) and records limits.
		if onHook != nil {
			r.FromSession = fromSession
			onHook(r)
		}
		return Response{OK: true}
	default: // ReqSwitchRequest
		if !fromSession {
			return Response{Error: "switch-request from outside this session"}
		}
		// TODO(T6664): perform the switch at the CLI's turn end.
		return Response{Error: "switching is not implemented yet"}
	}
}
