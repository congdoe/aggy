package adapter

import (
	"context"
	"fmt"
)

// SignInStatus is what aggy knows about a CLI's sign-in. aggy only ever
// learns it from the CLI itself; it never reads credential files.
type SignInStatus string

// Sign-in statuses. These are also the values the store records.
const (
	// SignInUnknown: the adapter has no verified way to ask the CLI.
	SignInUnknown SignInStatus = "unknown"
	SignedIn      SignInStatus = "connected"
	SignedOut     SignInStatus = "not_connected"
)

// ParseSignInStatus returns the status named s.
func ParseSignInStatus(s string) (SignInStatus, error) {
	switch st := SignInStatus(s); st {
	case SignInUnknown, SignedIn, SignedOut:
		return st, nil
	}
	return "", fmt.Errorf("adapter: unknown sign-in status %q", s)
}

// SignIn says how a vendor's CLI signs in.
type SignIn struct {
	// Steps is the vendor's own sign-in step, as text for the person to
	// follow, such as "run `codex login`". aggy prints it; it never signs
	// in for the person.
	Steps string
	// Check is a verified command that reports sign-in status without a
	// terminal. Nil when none is verified in docs/adapters.md: the status
	// is then SignInUnknown.
	Check *SignInCheck
}

// SignInCheck is a sign-in status command and how to read its output.
type SignInCheck struct {
	Args  []string                  // run on the probed CLI path, stdin /dev/null
	Parse func(Output) SignInStatus // must not keep or return any of the output
}

// CheckSignIn asks the CLI at path for its sign-in status through run,
// using si.Check. With no check it returns SignInUnknown and runs
// nothing. A runner error is returned with SignInUnknown.
func CheckSignIn(ctx context.Context, run Runner, path string, si SignIn) (SignInStatus, error) {
	if si.Check == nil || si.Check.Parse == nil || path == "" {
		return SignInUnknown, nil
	}
	out, err := run(ctx, Command{Path: path, Args: si.Check.Args})
	if err != nil {
		return SignInUnknown, err
	}
	switch st := si.Check.Parse(out); st {
	case SignedIn, SignedOut:
		return st, nil
	}
	return SignInUnknown, nil
}
