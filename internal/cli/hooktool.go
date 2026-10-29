package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/tooldef"
	"github.com/congdoe/aggy/internal/toolwire"
)

// maxHookInput caps the hook payload aggy hook-tool hands a tool.
const maxHookInput = 1 << 20

// hookTools loads the tool definitions for aggy hook-tool; a test swaps
// it.
var hookTools = func() ([]tooldef.Tool, error) {
	r, _, err := tooldef.LoadDefault()
	if err != nil {
		return nil, err
	}
	return r.List(), nil
}

// runHookTool is aggy hook-tool --vendor V <tool> <hook index> <digest>,
// the command a managed tool's hook entry runs (toolwire.HookCommand):
// it runs the hook's argv through execrun, with the payload from stdin as
// a file, a private empty working directory, an environment built from
// scratch with no vendor home (execrun.ToolHookRunner, L78-7), the
// hook's timeout, and capped output, and prints only what
// hook.FilterToolOutput passes: never an "allow", and an updatedInput
// only from a hook whose definition declares rewrites (H78-1), and only
// as its rewrite_prefix in front of the payload's own command (R80-1).
// What it
// drops is recorded by fixed names only (L78-5). On any failure it
// prints nothing and exits 0, which leaves the action as the CLI would
// have handled it without the hook. It always exits 0.
func runHookTool(args []string, stdout, stderr io.Writer) int {
	hookToolWith(context.Background(), args, os.Stdin, stdout)
	return 0
}

func hookToolWith(ctx context.Context, args []string, in io.Reader, stdout io.Writer) {
	tool := ""
	fail := func(code diag.Code) {
		dlog.Log(ctx, diag.Rec{Event: diag.EvHookToolFailed, Code: code, Msg: fixedMsg(code),
			Attrs: map[string]any{"tool": tool}})
	}
	if len(args) != 5 || args[0] != "--vendor" || args[1] != string(adapter.Claude) || !tooldef.ValidName(args[2]) {
		fail(diag.CodeUsage)
		return
	}
	tool = args[2]
	i, err := strconv.Atoi(args[3])
	digest := args[4]
	if err != nil || len(digest) != 16 {
		fail(diag.CodeUsage)
		return
	}
	payload, err := io.ReadAll(io.LimitReader(in, maxHookInput+1))
	if err != nil || len(payload) > maxHookInput {
		fail(diag.CodeToolHookFailed)
		return
	}
	tools, err := hookTools()
	if err != nil {
		fail(diag.CodeToolHookFailed)
		return
	}
	j := slices.IndexFunc(tools, func(t tooldef.Tool) bool { return t.Name == tool })
	// The definition must still be the one the session was wired from.
	if j < 0 || !strings.HasPrefix(tools[j].Digest, digest) || i < 0 || i >= len(tools[j].Hooks) {
		fail(diag.CodeToolHookFailed)
		return
	}
	h := tools[j].Hooks[i]
	if !slices.Contains(h.Vendors, adapter.Claude) || h.Approves {
		fail(diag.CodeToolHookApproves)
		return
	}
	// F74-2: no PATH entry from the repo the CLI runs in picks the hook's
	// program or reaches it; a folder that cannot be placed runs nothing.
	cwd, err := os.Getwd()
	if err == nil {
		var p string
		if p, err = toolPath(cwd); err == nil {
			err = os.Setenv("PATH", p)
		}
	}
	if err != nil {
		fail(diag.CodeToolHookFailed)
		return
	}
	path := h.Argv[0]
	if !filepath.IsAbs(path) {
		if path, err = probe.LookPath(path); err != nil {
			fail(diag.CodeToolHookFailed)
			return
		}
	}
	dir, err := safefs.MkdirTemp("", "aggy-hook-")
	if err != nil {
		fail(diag.CodeToolHookFailed)
		return
	}
	defer func() { _ = safefs.RemoveAll(dir) }()
	file := filepath.Join(dir, "payload.json")
	if err := safefs.WriteFile(file, payload, 0o600); err != nil {
		fail(diag.CodeToolHookFailed)
		return
	}
	timeout := h.Timeout
	if timeout == 0 {
		timeout = toolwire.ToolHookTimeout
	}
	pass, set := toolTelemetry("", tools[j])
	out, err := execrun.ToolHookRunner(time.Duration(timeout)*time.Second, pass, set)(ctx, adapter.Command{
		Path: path, Args: h.Argv[1:], Dir: dir, Stdin: adapter.StdinFile, StdinPath: file})
	if err != nil || out.ExitCode != 0 {
		fail(diag.CodeToolHookFailed)
		return
	}
	prefix := ""
	if h.Rewrites {
		prefix = h.RewritePrefix
	}
	filtered, dropped, err := hook.FilterToolOutput(h.Event, out.Stdout, payload, prefix)
	if err != nil {
		fail(diag.CodeToolHookOutput)
		return
	}
	if len(dropped) > 0 {
		dlog.Log(ctx, diag.Rec{Event: diag.EvHookToolFiltered, Attrs: map[string]any{"tool": tool, "dropped": strings.Join(dropped, ",")}})
	}
	_, _ = stdout.Write(filtered)
}
