package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/termsafe"
)

const switchUsage = "usage: aggy switch [--new] [--model M] [--brief] <claude|codex|grok>\n"

// runSwitch is `aggy switch <vendor>`, which a linked skill runs inside a
// CLI aggy supervises: it asks the supervisor to switch to vendor at the
// turn end (T6668). The supervisor accepts it only from the CLI's own
// process tree, checks the vendor against the picker's list, the repo's
// allowed_vendors, and PATH, and still asks the user before it types
// anything.
func runSwitch(args []string, stdout, stderr io.Writer) int {
	return switchWith(args, os.Getenv, stdout, stderr)
}

func switchWith(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	req := supervisor.Request{Type: supervisor.ReqSwitchRequest}
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--new":
			req.New = true
		case a == "--brief":
			req.Brief = true
		case a == "--model" && i+1 < len(args):
			i++
			req.Model = args[i]
		case strings.HasPrefix(a, "-"):
			fmt.Fprint(stderr, switchUsage)
			return 2
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		fmt.Fprint(stderr, switchUsage)
		return 2
	}
	if err := adapter.ValidModel(req.Model); err != nil {
		fmt.Fprintf(stderr, "aggy switch: %s\n", termsafe.Line(err.Error()))
		return 2
	}
	v, err := adapter.ParseVendor(rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "aggy switch: %v\n", err)
		return 2
	}
	sock := getenv("AGGY_SOCKET")
	if sock == "" {
		fmt.Fprintln(stderr, "aggy switch: not inside a CLI aggy runs (no AGGY_SOCKET); press the switch key in aggy instead")
		return 1
	}
	dir, err := home.Dir()
	if err != nil {
		fmt.Fprintf(stderr, "aggy switch: %v\n", err)
		return 1
	}
	req.Vendor = string(v)
	resp, err := supervisor.Send(filepath.Join(dir, "run"), sock, req, hookTimeout)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "aggy switch: %s\n", termsafe.Line(err.Error()))
		return 1
	case !resp.OK:
		// TODO(T6753): log switch.refused.
		fmt.Fprintf(stderr, "aggy switch: refused: %s\n", termsafe.Line(resp.Error))
		return 1
	}
	fmt.Fprintf(stdout, "aggy will offer the user a switch to %s when this turn ends. End your turn now; the user decides.\n", v)
	return 0
}
