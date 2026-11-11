//go:build !unix

package supervisor

import (
	"errors"
	"time"
)

// Send needs a Unix socket; there is no supervisor on this OS yet.
func Send(string, string, Request, time.Duration) (Response, error) {
	return Response{}, errors.New("supervisor: not supported on this OS yet")
}

// ErrUnproven is a hermes-pin answer that came without the proof that its
// sender holds the supervisor's lock.
var ErrUnproven = errors.New("supervisor: the socket's peer did not show it holds the supervisor's lock")

// SendHermesPin cannot check the peer here, so it never answers: the hook
// asks (T6954).
func SendHermesPin(string, string, Request, time.Duration) (Response, error) {
	return Response{}, ErrUnproven
}
