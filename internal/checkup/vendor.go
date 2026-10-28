package checkup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/probe"
)

// Vendor is what a check found about one vendor's CLI.
type Vendor struct {
	Vendor       adapter.Vendor
	Installed    bool
	Path         string // the CLI's absolute path, "" when not installed
	Version      string // "" when not installed or unreadable
	Capabilities []string
	SignIn       adapter.SignInStatus
	// VersionWarning is adapter.VersionWarning for an installed CLI:
	// outside the tested range, or unreadable. Its version part comes
	// from the CLI, so print it through termsafe.
	VersionWarning string
	// Err is a failed probe of an installed CLI, or a failed sign-in
	// check. It may hold CLI output: print it through termsafe.
	Err error
}

// CheckVendor probes a's CLI through run (path, version, capabilities)
// and asks for its sign-in status. A CLI that is not on PATH is not
// installed, which is not an error.
func CheckVendor(ctx context.Context, a adapter.Adapter, run adapter.Runner) Vendor {
	v := Vendor{Vendor: a.Vendor(), SignIn: adapter.SignInUnknown}
	p, err := a.Probe(ctx, run)
	if errors.Is(err, probe.ErrNotFound) {
		return v
	}
	v.Installed, v.Path, v.Version, v.Capabilities = true, p.Path, p.Version, p.Capabilities
	if err != nil {
		v.Err = err
		if v.Path == "" {
			return v
		}
	}
	v.VersionWarning = adapter.VersionWarning(a, p.Version)
	if v.Err != nil {
		return v
	}
	v.SignIn, v.Err = adapter.CheckSignIn(ctx, run, p.Path, a.SignIn())
	return v
}

// Connected is the store's value for v: not_connected when the CLI is
// missing, else its sign-in status.
func (v Vendor) Connected() adapter.SignInStatus {
	if !v.Installed {
		return adapter.SignedOut
	}
	return v.SignIn
}

// ShowPath returns p with the home directory shown as ~, for output.
func ShowPath(p, home string) string {
	home = strings.TrimRight(home, string(filepath.Separator))
	switch {
	case home == "" || !filepath.IsAbs(home):
		return p
	case p == home:
		return "~"
	case strings.HasPrefix(p, home+string(filepath.Separator)):
		return "~" + p[len(home):]
	}
	return p
}
