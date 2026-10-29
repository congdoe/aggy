package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/toolwire"
)

// borrowToolsSuffix names a borrow child's launch folder in the run dir:
// borrow-<nonce>.tools. sweepMemoryDirs leaves it alone.
// TODO(T6709): a borrow killed before its cleanup leaves this empty
// folder behind; nothing sweeps it yet.
const borrowToolsSuffix = ".tools"

// editTools resolves an edit borrow's tools (T6709): the --tool names,
// each of which routing.edit_tools must list, since an agent runs aggy
// borrow; with none given, routing.edit_tools itself. Any other kind
// gets none, and --tool for one is refused: aggy cannot tell which tools
// read files or write, so a review stays read-only.
func editTools(kind adapter.Kind, asked []string, r config.Routing) ([]string, diag.Code, error) {
	if kind != adapter.Edit {
		if len(asked) > 0 {
			return nil, diag.CodeToolBorrowKind, fmt.Errorf("--tool is for an edit borrow only: a %s child gets no tool, since aggy cannot tell which tools read files or write", kind)
		}
		return nil, diag.Code{}, nil
	}
	if len(asked) == 0 {
		return slices.Clone(r.EditTools), diag.Code{}, nil
	}
	for _, n := range asked {
		if !slices.Contains(r.EditTools, n) {
			return nil, diag.CodeToolUnlisted, fmt.Errorf("--tool %s: routing.edit_tools in your config does not list it, and only the tools listed there reach an edit child", n)
		}
	}
	return asked, diag.Code{}, nil
}

// borrowTools is borrow.Deps.Tools: it wires the named tools into one
// edit child on vendor v. Each tool's MCP server is started through aggy
// tool exec, with {repo} set to cwd (the child's worktree), in a private
// launch folder under stateDir/run that done removes. A tool that cannot
// be wired refuses the vendor (a *borrow.ToolError, so the next one may
// try) rather than run the child without it. The checks are the
// launch's: the Grok gate, Codex's -c, the S63-16 file check, the repo's
// tools_off and a per-repo tool's scope, and for Codex the fail-closed
// name clash check; for Claude, which reads no MCP config but aggy's
// (--strict-mcp-config), the managed-mcp.json only (M78-3). A tool that
// reaches the network (network = true) is refused unless network, the
// user's routing.edit_network_tools, is set (M78-2).
func borrowTools(stateDir string, network bool) func(context.Context, adapter.Vendor, borrow.CLI, []string, string) ([]adapter.MCPServer, func(), error) {
	return func(ctx context.Context, v adapter.Vendor, cli borrow.CLI, names []string, cwd string) ([]adapter.MCPServer, func(), error) {
		refused := func(tool string, code diag.Code, why string) error {
			attrs := map[string]any{"class": code.String(), "borrow": true}
			if tool != "" {
				attrs["tool"] = tool
			}
			dlog.Log(ctx, diag.Rec{Event: diag.EvToolWireRefused, Vendor: string(v), Code: code, Msg: fixedMsg(code), Attrs: attrs})
			if tool != "" {
				why = "tool " + tool + ": " + why
			}
			return &borrow.ToolError{Code: code, Err: fmt.Errorf("%s: %s (%s)", v, why, code)}
		}
		switch {
		case v == adapter.Grok && !grokToolsVerified:
			return nil, nil, refused("", diag.CodeToolUnverified, grokUnverified)
		case v == adapter.Codex && !slices.Contains(cli.Caps, probe.CapConfigOverride):
			return nil, nil, refused("", diag.CodeToolNoFlag, "codex --help does not list -c")
		}
		b, _, _, err := toolBoard(ctx)
		if err != nil {
			return nil, nil, refused("", diag.CodeToolWireFailed, "the tool settings could not be read: "+err.Error())
		}
		repo, _ := toolRepo(cwd, false)
		if repo.root == "" {
			return nil, nil, refused("", diag.CodeToolWireFailed, "the child's worktree is in no git work tree")
		}
		if b.RepoOff, err = repoToolsOff(repo); err != nil {
			return nil, nil, refused("", diag.CodeToolWireFailed, "the repo's config cannot be read, so every tool is off here: "+err.Error())
		}
		// The launch's per-name checks, any refusal of which refuses the
		// vendor here.
		var first error
		keep, _ := sessionTools(ctx, b, v, repo.key, names, func(tool string, code diag.Code, _, why string) string {
			if first == nil {
				first = refused(tool, code, why)
			}
			return ""
		})
		if first != nil {
			return nil, nil, first
		}
		b.Session = keep
		var on []switchboard.State
		named := slices.DeleteFunc(b.On(v, repo.key), func(st switchboard.State) bool {
			return !slices.Contains(keep, st.Tool.Name) // on for sessions, not named for this child
		})
		changed := changedSkills(ctx, named) // R80-3
		for _, st := range named {
			if err := changed[st.Tool.Name]; err != nil {
				return nil, nil, refused(st.Tool.Name, codeOf(err, diag.CodeToolSkillChanged), err.Error())
			}
			if st.Parts.MCP == nil {
				return nil, nil, refused(st.Tool.Name, diag.CodeToolScope, "it has no MCP server for "+string(v)+", and a borrow child gets MCP servers only")
			}
			if st.Tool.Network && !network {
				return nil, nil, refused(st.Tool.Name, diag.CodeToolNetwork, "it reaches the network, and routing.edit_network_tools is off")
			}
			on = append(on, st)
		}
		var taken map[string]string
		switch v {
		case adapter.Claude:
			// The child reads no MCP config but aggy's, except an
			// administrator's managed one (M78-3).
			if taken, err = claudeManagedNames(); err != nil {
				return nil, nil, refused("", diag.CodeToolWireFailed, "the CLI's managed MCP servers could not be listed to check names: "+err.Error())
			}
		case adapter.Codex:
			// TODO(L78-9): every layer counts, the user's too, which
			// --ignore-user-config drops: an over-refusal. Which project
			// layers codex exec still reads then is unverified.
			if taken, err = takenMCPNames(v, cwd, repo.root); err != nil {
				return nil, nil, refused("", diag.CodeToolWireFailed, "the CLI's own MCP servers could not be listed to check names: "+err.Error())
			}
		}
		bin, err := aggyBin()
		if err != nil {
			return nil, nil, refused("", diag.CodeToolWireFailed, err.Error())
		}
		runDir := filepath.Join(stateDir, "run")
		if err := supervisor.PrivateDir(runDir); err != nil {
			return nil, nil, refused("", diag.CodeToolWireFailed, err.Error())
		}
		n, err := newUUID()
		if err != nil {
			return nil, nil, refused("", diag.CodeToolWireFailed, err.Error())
		}
		dir := filepath.Join(runDir, "borrow-"+n[:8]+borrowToolsSuffix)
		if err := safefs.Mkdir(dir, 0o700); err != nil {
			return nil, nil, refused("", diag.CodeToolWireFailed, err.Error())
		}
		done := func() { _ = safefs.RemoveAll(dir) }
		if fi, err := safefs.Lstat(dir); err != nil || !ownPrivateDir(fi) {
			done()
			return nil, nil, refused("", diag.CodeToolWireFailed, errors.Join(err, fmt.Errorf("%s is not a private (0700) directory of this user", dir)).Error())
		}
		servers, no := toolwire.MCP(on, repo.root, dir, bin, taken)
		if len(no) > 0 {
			done()
			return nil, nil, refused(no[0].Tool, no[0].Code, no[0].Why)
		}
		// T6711: the child's worktree gets a copy of the main checkout's
		// index; a refused copy is recorded, and the child runs without it.
		_ = copyIndexes(ctx, on, toolwire.Names(servers), repo)
		dlog.Log(ctx, diag.Rec{Event: diag.EvToolWired, Vendor: string(v), Attrs: map[string]any{"tools": toolwire.Names(servers), "borrow": true}})
		return servers, done, nil
	}
}
