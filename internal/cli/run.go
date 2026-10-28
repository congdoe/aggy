package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

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
	"github.com/congdoe/aggy/internal/termsafe"
)

// runRun is `aggy run [--direct] [--no-policy] <vendor>`: a new parent in
// the current directory with an interactive session of that vendor, under
// the supervisor, or with --direct on aggy's own terminal (runDirect).
// --no-policy starts it without the safety policy (launch).
func runRun(args []string, stdout, stderr io.Writer) int {
	var direct, noPolicy bool
	for len(args) > 0 && (args[0] == "--direct" || args[0] == "--no-policy") {
		direct = direct || args[0] == "--direct"
		noPolicy = noPolicy || args[0] == "--no-policy"
		args = args[1:]
	}
	if len(args) != 1 {
		fmt.Fprint(stderr, "usage: aggy run [--direct] [--no-policy] <claude|codex|grok>\n")
		return 2
	}
	v, err := adapter.ParseVendor(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 2
	}
	return launch("aggy run", target{New: true, Vendor: v, Direct: direct, NoPolicy: noPolicy}, stderr)
}

// launch opens the parent t names and puts its child on screen under the
// supervisor: the child last on screen resumed with its CLI's resume
// flag and no brief, or a new child. name prefixes errors. It gives the
// child the shared memory of the child's own repo and the safety
// policy, and refuses to start a CLI the policy cannot reach unless
// t.NoPolicy is set or the user's config sets [policy] required = false.
func launch(name string, t target, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %s\n", name, termsafe.Line(err.Error()))
		return 1
	}
	// Refused before anything is written to the index.
	if !isTerminal(os.Stdin) {
		return fail(errors.New("needs a terminal on stdin"))
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		return fail(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = s.Close() }()
	pl, err := plan(ctx, s, cfg, cwd, t, time.Now(), stderr)
	if err != nil {
		return fail(err)
	}
	v := adapter.Vendor(pl.child.Vendor)
	path, err := probe.LookPath(vendorBin(v))
	if err != nil {
		return fail(err)
	}
	// The probe finds the flags whose presence is only known from help:
	// Codex --no-alt-screen and -c, Claude --settings, Grok --rules and
	// --deny.
	var caps []string
	probed, err := map[adapter.Vendor]adapter.Adapter{adapter.Claude: claude.Adapter{Path: path},
		adapter.Codex: codex.Adapter{Path: path}, adapter.Grok: grok.Adapter{Path: path}}[v].Probe(ctx, execrun.ExecRunner(probeTimeout))
	if err != nil {
		fmt.Fprintf(stderr, "%s: probing %s: %s; starting it without the flags help must list\n", name, v, termsafe.Line(err.Error()))
	} else {
		caps = probed.Capabilities
	}
	var a adapter.Adapter
	switch v {
	case adapter.Claude:
		a = claude.Adapter{Caps: caps}
	case adapter.Codex:
		a = codex.Adapter{Caps: caps}
	case adapter.Grok:
		a = grok.New()
	}
	key, err := config.ParseKey(cfg.Switch.Key)
	if err != nil {
		return fail(err)
	}
	stateDir, err := home.Dir()
	if err != nil {
		return fail(err)
	}
	parentID := pl.parent.ID
	runDir := filepath.Join(stateDir, "run")
	if err := supervisor.PrivateDir(runDir); err != nil {
		return fail(err)
	}
	sweepMemoryDirs(runDir, time.Now())
	req := pl.interactiveRequest()
	// The policy is checked before the launch is recorded (commit), so a
	// refused launch adds no parent, child, or lease. plan's refresh may
	// already have raised last-active times from session files (D19-9).
	var warns []string
	if t.NoPolicy {
		warns = append(warns, "starting without the safety policy (--no-policy)")
	} else {
		pw, err := addPolicy(v, caps, cfg.Policy.Required, &req)
		if err != nil {
			return fail(err)
		}
		warns = append(warns, pw...)
	}
	// Memory comes from the child's own directory, never the current one:
	// aggy resume <id> works from anywhere.
	// A crashed launch's folder is left to sweepMemoryDirs above.
	memDir, err := memoryDir(runDir, parentID)
	if err != nil {
		return fail(err)
	}
	cleanup, mw := addMemory(v, caps, pl.child.Cwd, memDir, &req)
	defer cleanup()
	for _, w := range append(warns, mw...) {
		fmt.Fprintf(stderr, "%s: %s\n", name, termsafe.Line(w))
	}
	cmd, err := a.Interactive(req)
	if err != nil {
		return fail(err)
	}
	cmd.Path = path
	lease, err := pl.commit(ctx, s, time.Now())
	if err != nil {
		return fail(err)
	}
	defer func() {
		if lease != nil {
			_ = lease.Release(ctx)
		}
		// Record the session file's time now the CLI has exited.
		_, _ = refreshChildren(ctx, s, pl.parent.ID, &sessionFiles{})
	}()
	// The child's id when known at launch: Claude's and Grok's
	// pre-assigned or resumed one (the only id the index records for
	// them), or a resumed Codex thread. A new Codex child's id comes from
	// the session index, which ids hands to the child row.
	assigned := pl.child.VendorSessionID
	ids := &childIDs{s: s, child: pl.child}
	defer ids.release(ctx)
	if t.Direct {
		code, err := runDirect(cmd, runDir, parentID, v, assigned, ids.set)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", name, err)
			if code == 0 {
				code = 1
			}
		}
		return code
	}
	onHook, stopIndex := startIndex(parentID, v, assigned, ids.set)
	defer stopIndex()
	onHook, stopLimits := startLimits(parentID, v, onHook)
	defer stopLimits()
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
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
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

// vendorBin is the vendor CLI's command name.
func vendorBin(v adapter.Vendor) string {
	return map[adapter.Vendor]string{adapter.Claude: claude.Bin, adapter.Codex: codex.Bin, adapter.Grok: grok.Bin}[v]
}

// newUUID returns a random (version 4) UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
