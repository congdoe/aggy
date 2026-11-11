//go:build unix

package execrun

import (
	"context"
	"errors"
	"os"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/congdoe/aggy/internal/keyring"
)

// TestKeychainPromptRestoresTerminal (#138 review L1): a keychain tool that
// turns echo off and never comes back leaves the terminal as it found it,
// whether the timeout or a signal ends it; the signal comes back as
// ErrInterrupted, and aggy is not ended with echo off.
func TestKeychainPromptRestoresTerminal(t *testing.T) {
	bin, _ := keychainWorld(t)
	t.Setenv("AGGY_FAKE_KEYCHAIN_HANG", "1")
	h := keyring.Helper{Kind: keyring.Security, Path: bin}
	for _, c := range []struct {
		name   string
		signal bool
	}{{"timeout", false}, {"interrupt", true}} {
		t.Run(c.name, func(t *testing.T) {
			ptm, tty, err := pty.Open()
			if err != nil {
				t.Skipf("no pseudo-terminal: %v", err)
			}
			defer func() { _ = ptm.Close(); _ = tty.Close() }()
			fd := int(tty.Fd())
			before, err := term.GetState(fd)
			if err != nil {
				t.Fatal(err)
			}
			timeout := 2 * time.Second
			if c.signal {
				timeout = 30 * time.Second
				go func() {
					// Once the tool has turned echo off, the person presses ^C.
					for i := 0; i < 200; i++ {
						if st, err := term.GetState(fd); err == nil && !reflect.DeepEqual(st, before) {
							_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
							return
						}
						time.Sleep(25 * time.Millisecond)
					}
				}()
			}
			_, err = KeychainPrompt(context.Background(), h.Path, h.AddArgs("hang"), tty, timeout)
			switch {
			case c.signal && !errors.Is(err, ErrInterrupted):
				t.Errorf("err = %v, want ErrInterrupted", err)
			case !c.signal && !errors.Is(err, ErrTimeout):
				t.Errorf("err = %v, want ErrTimeout", err)
			}
			after, err := term.GetState(fd)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Error("the terminal was left as the tool set it (echo off)")
			}
		})
	}
}
