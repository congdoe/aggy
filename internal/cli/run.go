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
	"github.com/congdoe/aggy/internal/termsafe"
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
	bin := map[adapter.Vendor]string{adapter.Claude: claude.Bin, adapter.Codex: codex.Bin, adapter.Grok: grok.Bin}[v]
	path, err := probe.LookPath(bin)
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 1
	}
	// The probe finds the flags whose presence is only known from help:
	// Codex --no-alt-screen and -c, Grok --rules.
	var caps []string
	probed, err := map[adapter.Vendor]adapter.Adapter{adapter.Claude: claude.Adapter{Path: path},
		adapter.Codex: codex.Adapter{Path: path}, adapter.Grok: grok.Adapter{Path: path}}[v].Probe(context.Background(), execrun.ExecRunner(probeTimeout))
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: probing %s: %s; starting it without the flags help must list\n", v, termsafe.Line(err.Error()))
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
	req := adapter.InteractiveRequest{
		Cwd:   cwd,
		Model: cfg.Vendors.For(v).Model,
		Env:   []string{"AGGY_VENDOR=" + string(v)},
	}
	root, _ := repoRoot(cwd)
	cleanup, warns := addMemory(v, caps, root, &req)
	defer cleanup()
	for _, w := range warns {
		fmt.Fprintf(stderr, "aggy run: %s\n", termsafe.Line(w))
	}
	cmd, err := a.Interactive(req)
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
	code, err := supervisor.Run(supervisor.Options{
		In: os.Stdin, Out: os.Stdout, Command: cmd,
		Start:     startPTY,
		SwitchKey: key,
		Vendors:   pickerVendors(cfg.Switch.Order, v),
		RunDir:    filepath.Join(stateDir, "run"),
		ParentID:  parentID,
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
