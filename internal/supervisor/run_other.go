//go:build !unix

package supervisor

import (
	"errors"
	"time"
)

// Run needs a Unix terminal; Windows needs ConPTY (after 1.0).
func Run(Options) (Exit, error) {
	return Exit{Code: 1}, errors.New("supervisor: not supported on this OS yet")
}

// PrivateDir is not supported here: nothing runs a supervisor.
func PrivateDir(string) error { return errors.New("supervisor: not supported on this OS yet") }

// Live is false here: no supervisor runs on this OS.
func Live(string, string) (bool, error) { return false, nil }

// HandOverGrace is unused here: no supervisor runs on this OS.
const HandOverGrace = 0

// HandOver does nothing here: no supervisor runs on this OS.
type HandOver struct{}

// HandOverSignals does nothing here.
func HandOverSignals() *HandOver { return &HandOver{} }

// Stop does nothing here.
func (*HandOver) Stop() {}

// StopAfter does nothing here.
func (*HandOver) StopAfter(time.Duration) {}
