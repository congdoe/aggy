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
