package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/probe"
)

// installPage is where each vendor says how to install its CLI (T6979), for
// the refusal when it is not on PATH. Sources in docs/adapters.md, "Install
// pages": the Codex and Grok Build pages are the repositories aggy's adapters
// cite; the Claude Code and OpenCode pages are on the docs sites aggy cites,
// but that they are the install pages is unverified.
func installPage(v adapter.Vendor) string {
	switch v {
	case adapter.Claude:
		return "https://code.claude.com/docs/en/setup"
	case adapter.Codex:
		return "https://github.com/openai/codex"
	case adapter.Grok:
		return "https://github.com/xai-org/grok-build"
	case adapter.OpenCode:
		return "https://opencode.ai/docs"
	}
	return ""
}

// lookPathFor finds a vendor's CLI; a test sets it.
var lookPathFor = func(v adapter.Vendor) bool {
	_, err := probe.LookPath(vendorBin(v))
	return err == nil
}

// cliMissing is the refusal for a vendor whose CLI is not on PATH: it says the
// CLI is not installed and where to get it, and names another vendor to start.
func cliMissing(v adapter.Vendor, others []adapter.Vendor) error {
	msg := fmt.Sprintf("%s is not installed: aggy finds no %s on PATH", v, vendorBin(v))
	if p := installPage(v); p != "" {
		msg += "; install it from " + p
	}
	if len(others) > 0 {
		msg += fmt.Sprintf(", or start another, such as aggy %s", others[0])
	}
	return fmt.Errorf("%s", msg)
}

// installedElse are the vendors besides v whose CLI is on PATH and that may
// start in dir: not turned off, and allowed by the repo.
func installedElse(cfg config.Config, dir string, v adapter.Vendor) []adapter.Vendor {
	var out []adapter.Vendor
	for _, o := range adapter.Known() {
		if o == v || !lookPathFor(o) {
			continue
		}
		if _, err := allowedIn(cfg, dir, o); err != nil {
			continue
		}
		out = append(out, o)
	}
	return out
}

// pickInstead offers, at a terminal, the installed vendors to start instead of
// v, whose CLI is not on PATH (T6979), and returns the one picked, or "".
func pickInstead(_ context.Context, v adapter.Vendor, others []adapter.Vendor, in io.Reader, out io.Writer) adapter.Vendor {
	if len(others) == 0 {
		return ""
	}
	names := make([]string, len(others))
	for i, o := range others {
		names[i] = string(o)
	}
	i, _ := pickInPlace(os.Getenv, in, out, cliMissing(v, nil).Error()+".", names)
	if i < 0 {
		return ""
	}
	return others[i]
}
