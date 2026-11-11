//go:build unix

package supervisor

import (
	"encoding/json"
	"sync"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
)

// hermesPin answers a hermes-pin line (#134 review): ok is false for any
// other line, which the caller answers as before. Only a line with the
// launch's token reaches onPin; one without it, or with no onPin, is refused.
func hermesPin(line []byte, token string, onPin func(string) Response) (Response, bool) {
	var probe struct{ Type string }
	if json.Unmarshal(line, &probe) != nil || probe.Type != ReqHermesPin {
		return Response{}, false
	}
	r, err := decodeRequest(line)
	if err != nil {
		return Response{Error: err.Error(), Code: diag.CodeBadRequest.String()}, true
	}
	if onPin == nil || !hermesToken(line, token) {
		return Response{Error: "hermes-pin without this launch's token", Code: diag.CodeLineage.String()}, true
	}
	var p struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(r.Payload, &p) != nil || adapter.ValidSessionID(adapter.Hermes, p.SessionID) != nil {
		return Response{Error: "hermes-pin needs a Hermes session id", Code: diag.CodeBadRequest.String()}, true
	}
	return onPin(p.SessionID), true
}

// HermesClaim is a Hermes launch's session, as its supervisor knows it.
// Its methods are safe for concurrent use: the socket asks on its own
// goroutines, and the terminal loop marks the first Enter.
type HermesClaim struct {
	mu      sync.Mutex
	pin     HermesPin
	session string // the claimed session, "" until one is
	started bool   // a first turn was sent: the brief, or an Enter typed
}

// NewHermesClaim holds p for a launch's hook. Under aggy --direct, where
// aggy sees no keys, the caller sets p.Brief, so the first session a hook
// names is taken: the earliest a tool call can come is the first turn.
func NewHermesClaim(p HermesPin) *HermesClaim {
	return &HermesClaim{pin: p, session: p.Session, started: p.Brief}
}

// Entered records an Enter typed in the session's terminal.
func (h *HermesClaim) Entered() {
	h.mu.Lock()
	h.started = true
	h.mu.Unlock()
}

// Answer is the hermes-pin answer for id. The launch's session answers
// with the pin. A new id claims the launch only when it has none yet and a
// first turn was sent from its own terminal (the brief, or an Enter typed
// there), the earliest a tool call can come from the session on screen. Any
// other id is refused: a nested `hermes -p <profile>` the model ran, a cron
// job or a delegated child in the profile, or a session the person moved to
// with /new or /resume, which aggy does not follow (it cannot tell that move
// from the others).
func (h *HermesClaim) Answer(id string) Response {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.session == id:
	case h.session == "" && h.started:
		h.session = id
	default:
		return Response{Error: "this Hermes session is not the one this aggy launch started", Code: diag.CodeHermesNoLaunch.String()}
	}
	return Response{OK: true, Plan: h.pin.Plan, PlanSum: h.pin.PlanSum, NoPolicy: h.pin.NoPolicy}
}

// Claimed is the claimed session, "" for none yet.
func (h *HermesClaim) Claimed() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.session
}
