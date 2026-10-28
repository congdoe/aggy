//go:build unix

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
// be FromSession; onSwitch, if set, takes it or says why not, and without
// it (aggy --direct) switching is refused.
func answer(line []byte, fromSession bool, onHook func(Request), onSwitch func(Request) error) Response {
	r, err := decodeRequest(line)
	if err != nil {
		return Response{Error: err.Error()}
	}
	r.FromSession = fromSession
	switch r.Type {
	case ReqPing:
		return Response{OK: true, Type: "pong"}
	case ReqHookEvent:
		// The supervisor acts on a turn end only when FromSession, and
		// even then only as a hint (Run, T6664).
		if onHook != nil {
			onHook(r)
		}
		return Response{OK: true}
	default: // ReqSwitchRequest
		if !fromSession {
			return Response{Error: "switch-request from outside this session"}
		}
		if onSwitch == nil {
			return Response{Error: "switching needs aggy's pass-through; this session cannot switch"}
		}
		if err := onSwitch(r); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	}
}
