package cli

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
)

// runRun is `aggy run [--direct] <vendor>`: a new interactive session of
// that vendor under the supervisor, or with --direct on aggy's own
// terminal (runDirect).
//
// TODO(T6661): bare `aggy` resumes the latest parent or starts one, and
// `aggy new` and `aggy resume` record parents and children in the store.
func runRun(args []string, stdout, stderr io.Writer) int {
	direct := len(args) > 0 && args[0] == "--direct"
	if direct {
		args = args[1:]
	}
	if len(args) != 1 {
		fmt.Fprint(stderr, "usage: aggy run [--direct] <claude|codex|grok>\n")
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
	bin := map[adapter.Vendor]string{adapter.Claude: claude.Bin, adapter.Codex: codex.Bin, adapter.Grok: grok.Bin}[v]
	path, err := probe.LookPath(bin)
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 1
	}
	var a adapter.Adapter
	switch v {
	case adapter.Claude:
		a = claude.New()
	case adapter.Codex:
		// --no-alt-screen is unverified for Codex, so it goes on the
		// command line only when this CLI's help lists it.
		p, err := codex.Adapter{Path: path}.Probe(context.Background(), execrun.ExecRunner(probeTimeout))
		if err != nil {
			fmt.Fprintf(stderr, "aggy run: probing codex: %v; starting it without --no-alt-screen\n", err)
		}
		a = codex.Adapter{Caps: p.Capabilities}
	case adapter.Grok:
		a = grok.New()
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
	key, err := config.ParseKey(cfg.Switch.Key)
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 1
	}
	stateDir, err := home.Dir()
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 1
	}
	parentID, err := newParentID()
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 1
	}
	runDir := filepath.Join(stateDir, "run")
	if direct {
		code, err := runDirect(cmd, runDir, parentID, v)
		if err != nil {
			fmt.Fprintf(stderr, "aggy run: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
		return code
	}
	onHook, stopIndex := startIndex(parentID, v)
	defer stopIndex()
	code, err := supervisor.Run(supervisor.Options{
		In: os.Stdin, Out: os.Stdout, Command: cmd,
		Start:     startPTY,
		SwitchKey: key,
		Vendors:   pickerVendors(cfg.Switch.Order, v),
		RunDir:    runDir,
		ParentID:  parentID,
		OnHook:    onHook,
	})
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// pickerVendors lists the vendors in order, except on, that aggy setup
// recorded and did not find signed out. Codex and Grok have no verified
// sign-in check, so "unknown" counts. Without an index, the list is empty.
func pickerVendors(order []adapter.Vendor, on adapter.Vendor) []adapter.Vendor {
	path, err := store.DefaultPath()
	if err != nil {
		return nil
	}
	ctx := context.Background()
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return nil
	}
	defer func() { _ = s.Close() }()
	rows, err := s.VendorStatuses(ctx)
	if err != nil {
		return nil
	}
	ok := map[adapter.Vendor]bool{}
	for _, r := range rows {
		ok[adapter.Vendor(r.Vendor)] = r.Connected != string(adapter.SignedOut)
	}
	var out []adapter.Vendor
	for _, v := range order {
		if v != on && ok[v] {
			out = append(out, v)
		}
	}
	return out
}

// newParentID returns a random (version 4) UUID.
//
// TODO(T6661): record the parent in the store; for now the id only names
// the supervisor socket.
func newParentID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
