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

// runRun is `aggy run <vendor>`: a new parent in the current directory
// with an interactive session of that vendor, under the supervisor.
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
	return launch("aggy run", target{New: true, Vendor: v}, stderr)
}

// launch opens the parent t names and puts its child on screen under the
// supervisor: the child last on screen resumed with its CLI's resume
// flag and no brief, or a new child. name prefixes errors.
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
	var a adapter.Adapter
	switch v {
	case adapter.Claude:
		a = claude.New()
	case adapter.Codex:
		// --no-alt-screen is unverified for Codex, so it goes on the
		// command line only when this CLI's help lists it.
		p, err := codex.Adapter{Path: path}.Probe(context.Background(), execrun.ExecRunner(probeTimeout))
		if err != nil {
			fmt.Fprintf(stderr, "%s: probing codex: %v; starting it without --no-alt-screen\n", name, err)
		}
		a = codex.Adapter{Caps: p.Capabilities}
	case adapter.Grok:
		a = grok.New()
	}
	cmd, err := a.Interactive(pl.interactiveRequest())
	if err != nil {
		return fail(err)
	}
	cmd.Path = path
	key, err := config.ParseKey(cfg.Switch.Key)
	if err != nil {
		return fail(err)
	}
	stateDir, err := home.Dir()
	if err != nil {
		return fail(err)
	}
	lease, err := pl.commit(ctx, s, time.Now())
	if err != nil {
		return fail(err)
	}
	if lease != nil {
		defer func() { _ = lease.Release(ctx) }()
	}
	parentID := pl.parent.ID
	code, err := supervisor.Run(supervisor.Options{
		In: os.Stdin, Out: os.Stdout, Command: cmd,
		Start:     startPTY,
		SwitchKey: key,
		Vendors:   pickerVendors(cfg.Switch.Order, v),
		RunDir:    filepath.Join(stateDir, "run"),
		ParentID:  parentID,
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
