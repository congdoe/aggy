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
	"github.com/congdoe/aggy/internal/diag"
)

// maxModel caps a model name a switch carries, from the socket or typed
// in the picker.
const maxModel = 128

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
	if r.Type != ReqSwitchRequest && (r.New || r.Model != "" || r.Brief) {
		return r, errors.New("bad request: new, model, and brief are for switch-request")
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
			return r, errors.New("bad request: switch-request takes only vendor, new, model, and brief")
		}
		if len(r.Model) > maxModel {
			return r, fmt.Errorf("bad request: a model name is at most %d bytes", maxModel)
		}
		if err := adapter.ValidModel(r.Model); err != nil {
			return r, err
		}
	default:
		return r, fmt.Errorf("bad request: unknown type %q", r.Type)
	}
	return r, nil
}

// answer handles one request line. onHook, if set, gets each valid
// hook-event, with FromSession set to fromSession. A switch-request must
// be FromSession; onSwitch, if set, takes it or says why not, and without
// it (aggy --direct) switching is refused. log, if set, gets a record of
// each message refused or not acted on; a hook-event from the session is
// left to onHook to record.
func answer(line []byte, fromSession bool, onHook func(Request), onSwitch func(Request) error, log func(diag.Rec)) Response {
	if log == nil {
		log = func(diag.Rec) {}
	}
	refuse := func(ev diag.Event, c diag.Code, err error, attrs map[string]any) Response {
		log(diag.Rec{Event: ev, Code: c, Msg: err.Error(), Attrs: attrs})
		return Response{Error: err.Error(), Code: c.String()}
	}
	r, err := decodeRequest(line)
	if err != nil {
		// Nothing of the message is recorded: it is not aggy's.
		log(diag.Rec{Event: diag.EvHookRejected, Code: diag.CodeBadRequest, Msg: "a message aggy does not send", Attrs: map[string]any{"from_session": fromSession}})
		return Response{Error: err.Error(), Code: diag.CodeBadRequest.String()}
	}
	r.FromSession = fromSession
	switch r.Type {
	case ReqPing:
		return Response{OK: true, Type: "pong"}
	case ReqHookEvent:
		// The supervisor acts on a turn end only when FromSession, and
		// even then only as a hint (Run, T6664).
		if !fromSession {
			log(diag.Rec{Event: diag.EvHookRejected, Code: diag.CodeLineage, Msg: "a hook event from outside the CLI's process tree",
				Attrs: map[string]any{"event": hookName(r.Event), "from": r.Vendor}})
		}
		if onHook != nil {
			onHook(r)
		}
		return Response{OK: true}
	default: // ReqSwitchRequest
		attrs := map[string]any{"to": r.Vendor, "brief": r.Brief, "new": r.New, "model_set": r.Model != "", "origin": "agent"}
		if !fromSession {
			return refuse(diag.EvSwitchRefused, diag.CodeLineage, errors.New("switch-request from outside this session"), attrs)
		}
		if onSwitch == nil {
			return refuse(diag.EvSwitchRefused, diag.CodeNotSupervised, errors.New("switching needs aggy's pass-through; this session cannot switch"), attrs)
		}
		if err := onSwitch(r); err != nil {
			return refuse(diag.EvSwitchRefused, codeOf(err, diag.CodeSwitchVendor), err, attrs)
		}
		return Response{OK: true}
	}
}
