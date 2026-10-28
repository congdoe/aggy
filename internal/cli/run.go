package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/supervisor"
)

// runRun is `aggy run <vendor>`: a new interactive session of that vendor
// under the supervisor.
//
// TODO(T6661): bare `aggy` resumes the latest parent or starts one, and
// `aggy new` and `aggy resume` record parents and children in the store.
func runRun(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, "usage: aggy run <claude|codex|grok>\n")
		return 2
	}
	v, err := adapter.ParseVendor(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 2
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 1
	}
	var a adapter.Adapter
	var bin string
	switch v {
	case adapter.Claude:
		a, bin = claude.New(), claude.Bin
	case adapter.Codex:
		a, bin = codex.New(), codex.Bin
	case adapter.Grok:
		a, bin = grok.New(), grok.Bin
	}
	path, err := probe.LookPath(bin)
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 1
	}
	cmd, err := a.Interactive(adapter.InteractiveRequest{
		Cwd:   cwd,
		Model: cfg.Vendors.For(v).Model,
		Env:   []string{"AGGY_VENDOR=" + string(v)},
	})
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 1
	}
	cmd.Path = path
	code, err := supervisor.Run(supervisor.Options{
		In: os.Stdin, Out: os.Stdout, Command: cmd,
		Start: startPTY,
	})
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}
