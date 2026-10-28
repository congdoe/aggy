package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/termsafe"
)

const switchUsage = "usage: aggy switch [--brief] <claude|codex|grok>\n"

// runSwitch is `aggy switch <vendor>`, which a linked skill runs inside a
// CLI aggy supervises: it asks the supervisor to switch to vendor at the
// turn end (T6668). The supervisor accepts it only from the CLI's own
// process tree, checks the vendor against the picker's list, the repo's
// allowed_vendors, and PATH, and still asks the user before it types
// anything. An agent asks for a vendor and a brief only (O56-1): a new
// session or a model for the launch is the user's choice, in the picker,
// and the supervisor refuses them too.
func runSwitch(args []string, stdout, stderr io.Writer) int {
	return switchWith(args, os.Getenv, stdout, stderr)
}

func switchWith(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	ctx := context.Background()
	req := supervisor.Request{Type: supervisor.ReqSwitchRequest}
	// refused records a refusal under code with msg, and prints it with
	// shown, which may quote the agent's own arguments; the record never
	// does. This process's trace is the supervisor's (AGGY_TRACE), so
	// its record joins the session's.
	refused := func(code diag.Code, msg, shown string, v adapter.Vendor) {
		dlog.Log(ctx, diag.Rec{Event: diag.EvSwitchRefused, Code: code, Msg: msg, Vendor: string(v)})
		fmt.Fprintf(stderr, "aggy switch: %s %s\n", termsafe.Line(shown), dlog.Tag(code))
	}
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--brief":
			req.Brief = true
		case a == "--new" || a == "--model":
			msg := a + " is the user's choice, in aggy's picker; an agent may ask for a vendor and --brief only"
			refused(diag.CodeAgentOptions, msg, msg, "")
			return 2
		case strings.HasPrefix(a, "-"):
			refused(diag.CodeUsage, "an unknown flag", "an unknown flag", "")
			fmt.Fprint(stderr, switchUsage)
			return 2
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		refused(diag.CodeUsage, "one vendor is needed", "one vendor is needed", "")
		fmt.Fprint(stderr, switchUsage)
		return 2
	}
	v, err := adapter.ParseVendor(rest[0])
	if err != nil {
		refused(diag.CodeUsage, "an unknown vendor", err.Error(), "")
		return 2
	}
	sock := getenv("AGGY_SOCKET")
	if sock == "" {
		msg := "not inside a CLI aggy runs (no AGGY_SOCKET); press the switch key in aggy instead"
		refused(diag.CodeNotSupervised, msg, msg, v)
		return 1
	}
	dir, err := home.Dir()
	if err != nil {
		refused(diag.CodeStore, err.Error(), err.Error(), v)
		return 1
	}
	req.Vendor = string(v)
	resp, err := supervisor.Send(filepath.Join(dir, "run"), sock, req, hookTimeout)
	switch {
	case err != nil:
		refused(diag.CodeUnreachable, err.Error(), err.Error(), v)
		return 1
	case !resp.OK:
		// The supervisor recorded it too; its code is taken only when
		// the registry has it.
		code := diag.CodeSwitchVendor
		if c, ok := diag.CodeNamed(resp.Code); ok {
			code = c.Name
		}
		refused(code, "refused: "+resp.Error, "refused: "+resp.Error, v)
		return 1
	}
	fmt.Fprintf(stdout, "aggy will offer the user a switch to %s when this turn ends. End your turn now; the user decides.\n", v)
	return 0
}
