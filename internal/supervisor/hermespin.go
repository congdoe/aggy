//go:build unix

package supervisor

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
)

// hermesPin answers a hermes-pin line (#134 review): ok is false for any
// other line, which the caller answers as before. Only a line with the
// launch's token reaches onPin; one without it, or with no onPin, is refused.
func hermesPin(line []byte, token string, onPin func(string, bool) Response) (Response, bool) {
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
		Shared    bool   `json:"shared"`
	}
	if json.Unmarshal(r.Payload, &p) != nil || adapter.ValidSessionID(adapter.Hermes, p.SessionID) != nil {
		return Response{Error: "hermes-pin needs a Hermes session id", Code: diag.CodeBadRequest.String()}, true
	}
	return onPin(p.SessionID, p.Shared), true
}

// HermesClaimWindow is how long after an Enter typed in an unclaimed
// launch's terminal a new session may claim it while another aggy Hermes
// launch in the same profile is live (T6954).
const HermesClaimWindow = 60 * time.Second

// HermesClaim is a Hermes launch's session, as its supervisor knows it.
// Its methods are safe for concurrent use: the socket asks on its own
// goroutines, and the terminal loop marks each Enter.
type HermesClaim struct {
	mu      sync.Mutex
	pin     HermesPin
	session string    // the claimed session, "" until one is
	started bool      // a first turn was sent: the brief, or an Enter typed
	entered time.Time // the last Enter typed (or the brief sent), zero for none
	now     func() time.Time
}

// NewHermesClaim holds p for a launch's hook. Under aggy --direct, where
// aggy sees no keys, the caller sets p.Direct, so the first session a hook
// names is taken while the launch is the only one in its profile: the
// earliest a tool call can come is the first turn.
func NewHermesClaim(p HermesPin) *HermesClaim {
	h := &HermesClaim{pin: p, session: p.Session, started: p.Brief || p.Direct, now: time.Now}
	if p.Brief && !p.Direct {
		h.entered = h.now() // the brief is the first turn, sent now
	}
	return h
}

// Entered records an Enter typed in the session's terminal.
func (h *HermesClaim) Entered() {
	h.mu.Lock()
	h.started = true
	h.entered = h.now()
	h.mu.Unlock()
}

// Answer is the hermes-pin answer for id; shared says the hook found
// another live aggy Hermes launch in the profile. The launch's session
// answers with the pin. A new id claims the launch only when it has none yet
// and a first turn was sent from its own terminal (the brief, or an Enter
// typed there), the earliest a tool call can come from the session on
// screen. Any other id is refused: a nested `hermes -p <profile>` the model
// ran, a cron job or a delegated child in the profile, or a session the
// person moved to with /new or /resume, which aggy does not follow (it
// cannot tell that move from the others).
//
// With another launch live in the profile (T6954), a session that other
// launch starts (/new there, or a nested hermes, a delegate, or a cron job
// the person approved there) looks just like this launch's first one: both
// are new ids, and the hook runs in Hermes' daemon, where nothing ties a
// call to a terminal. So then a new id claims the launch only within
// HermesClaimWindow of the last Enter typed here (or the brief), and never
// under --direct, where aggy sees no keys; a refused first call blocks, and
// typing the message again opens the window again. One launch alone in its
// profile, the common case, claims as before, with no window.
func (h *HermesClaim) Answer(id string, shared bool) Response {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.session == id:
	case h.session == "" && h.started && shared && (h.pin.Direct || h.entered.IsZero() || h.now().Sub(h.entered) > HermesClaimWindow):
		return Response{Error: fmt.Sprintf("another aggy Hermes session in this repo is open, and this session's call did not come within %s of an Enter typed in this one", HermesClaimWindow), Code: diag.CodeHermesClaimLate.String()}
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
