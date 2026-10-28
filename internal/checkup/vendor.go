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
	// ProbeErr is a failed probe of an installed CLI, and SignInErr a
	// sign-in check that did not run. They may hold CLI text: print them
	// through termsafe.
	ProbeErr  error
	SignInErr error
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
		v.ProbeErr = err
		return v
	}
	v.VersionWarning = adapter.VersionWarning(a, p.Version)
	v.SignIn, v.SignInErr = adapter.CheckSignIn(ctx, run, p.Path, a.SignIn())
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

// VendorFindings turns a vendor check into findings. name is the
// product name and steps the vendor's own sign-in step, for messages;
// home shows paths as ~. Messages may hold CLI text (the version, an
// error): print them through termsafe.
func VendorFindings(res Vendor, name, steps, home string) []Finding {
	f := func(code string, l Level, msg string) Finding {
		return Finding{Code: code, Level: l, Vendor: res.Vendor, Message: msg}
	}
	if !res.Installed {
		return []Finding{f(CodeCLIMissing, LevelInfo, name+" is not installed (not found on PATH)")}
	}
	version := res.Version
	if version == "" {
		version = "unreadable"
	}
	out := []Finding{f(CodeCLIFound, LevelInfo, ShowPath(res.Path, home)+", version "+version)}
	if res.VersionWarning != "" {
		out = append(out, f(CodeVersionUntested, LevelWarn, res.VersionWarning))
	}
	switch {
	case res.ProbeErr != nil:
		out = append(out, f(CodeProbeFailed, LevelError, name+" did not answer its version check: "+res.ProbeErr.Error()))
	case res.SignInErr != nil:
		out = append(out, f(CodeSignInFailed, LevelWarn, name+"'s sign-in check failed: "+res.SignInErr.Error()))
	case res.SignIn == adapter.SignedIn:
		out = append(out, f(CodeSignedIn, LevelInfo, name+" is signed in"))
	case res.SignIn == adapter.SignedOut:
		out = append(out, f(CodeSignedOut, LevelWarn, name+" is not signed in: "+steps))
	default:
		out = append(out, f(CodeSignInUnknown, LevelInfo, "aggy has no verified way to ask "+name+" whether it is signed in; if it is not: "+steps))
	}
	return out
}
