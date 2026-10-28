package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// runNew is `aggy new [vendor]`: another parent in the current directory.
// The old ones stay listed.
func runNew(args []string, _, stderr io.Writer) int {
	noPolicy, args := cutNoPolicy(args)
	if len(args) > 1 || (len(args) == 1 && strings.HasPrefix(args[0], "-")) {
		fmt.Fprint(stderr, "usage: aggy new [--no-policy] [claude|codex|grok]\n")
		return 2
	}
	var v adapter.Vendor
	if len(args) == 1 {
		var err error
		if v, err = adapter.ParseVendor(args[0]); err != nil {
			fmt.Fprintf(stderr, "aggy new: %v\n", err)
			return 2
		}
	}
	return launch("aggy new", target{New: true, Vendor: v, NoPolicy: noPolicy}, stderr)
}

// runResume is `aggy resume [id]`: the latest parent in the current
// directory, or the one whose id starts with id, or, when no parent's id
// does, the parent of the interactive child whose id starts with id, with
// that child on screen (aggy session lists child ids). Flags may come
// before or after the id.
func runResume(args []string, stdout, stderr io.Writer) int {
	const use = "usage: aggy resume [--no-policy] [--brief [--show]] [id]\n"
	var t target
	var show bool
	var ids []string
	for _, a := range args {
		switch {
		case a == "--no-policy":
			t.NoPolicy = true
		case a == "--brief":
			t.Brief = true
		case a == "--show":
			show = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprint(stderr, use)
			return 2
		default:
			ids = append(ids, a)
		}
	}
	args = ids
	if len(args) > 1 || show && !t.Brief {
		fmt.Fprint(stderr, use)
		return 2
	}
	if len(args) == 1 {
		id := strings.ToLower(args[0])
		if len(id) < 4 || len(id) > 36 || strings.Trim(id, "0123456789abcdef-") != "" {
			fmt.Fprintf(stderr, "aggy resume: %q is not a session id (at least 4 hex digits; aggy sessions lists them)\n", termsafe.Field(args[0]))
			return 2
		}
		t.ParentID = id
	}
	if show {
		return showBrief("aggy resume", t, stdout, stderr)
	}
	return launch("aggy resume", t, stderr)
}

// openIndex opens the index, or returns nil when there is none yet.
func openIndex(ctx context.Context) (*store.Store, error) {
	path, err := store.DefaultPath()
	if err != nil {
		return nil, err
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return store.OpenDefault(ctx)
}

// runSessions is `aggy sessions`: every parent, most recently active
// first, with its directory, last vendor, and last-active time. Paths
// are shown to the local user only; no env value or vendor session id is
// printed.
func runSessions(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprint(stderr, "usage: aggy sessions\n")
		return 2
	}
	ctx := context.Background()
	s, err := openIndex(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aggy sessions: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	if s == nil {
		fmt.Fprintln(stdout, "No sessions yet. Run aggy to start one.")
		return 0
	}
	defer func() { _ = s.Close() }()
	ps, err := s.Parents(ctx)
	if err == nil {
		// Last active comes from the vendors' session files, so it holds
		// after a crash or sleep; then the order may change.
		files := &sessionFiles{}
		for _, p := range ps {
			if _, err = refreshChildren(ctx, s, p.ID, files); err != nil {
				break
			}
		}
		if err == nil {
			ps, err = s.Parents(ctx)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "aggy sessions: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	if len(ps) == 0 {
		fmt.Fprintln(stdout, "No sessions yet. Run aggy to start one.")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLAST ACTIVE\tVENDOR\tDIRECTORY\tTITLE")
	for _, p := range ps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", termsafe.Field(shortID(p.ID)), when(p.LastActive),
			orDash(termsafe.Field(p.LastVendor)), termsafe.Field(tildePath(p.Dir)), termsafe.Field(p.Title))
	}
	_ = tw.Flush()
	fmt.Fprintln(stdout, "aggy resume <id> reopens one.")
	return 0
}

// runSession is `aggy session`: the children of the open parent (the
// supervisor's, from AGGY_PARENT, else the latest in the current
// directory), with vendor, kind, title, model, and last-active time.
// Borrows are children too.
func runSession(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprint(stderr, "usage: aggy session\n")
		return 2
	}
	ctx := context.Background()
	s, err := openIndex(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aggy session: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	if s == nil {
		fmt.Fprintln(stdout, "No session here. Run aggy to start one.")
		return 0
	}
	defer func() { _ = s.Close() }()
	p, err := openParent(ctx, s)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintln(stdout, "No session here. Run aggy to start one.")
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "aggy session: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	cs, err := refreshChildren(ctx, s, p.ID, &sessionFiles{})
	if err != nil {
		fmt.Fprintf(stderr, "aggy session: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	fmt.Fprintf(stdout, "Session %s in %s\n", shortID(p.ID), termsafe.Line(tildePath(p.Dir)))
	if len(cs) == 0 {
		fmt.Fprintln(stdout, "No children yet.")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tVENDOR\tKIND\tTITLE\tMODEL\tLAST ACTIVE\tSTATUS")
	for _, c := range cs {
		kind := c.Kind
		if kind == "" {
			kind = "interactive"
		}
		model := c.Model
		if model == "" {
			model = "default"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", termsafe.Field(shortID(c.ID)), termsafe.Field(c.Vendor), termsafe.Field(kind),
			orDash(termsafe.Field(c.Title)), termsafe.Field(model), when(c.LastActive), termsafe.Field(c.Status))
	}
	_ = tw.Flush()
	return 0
}

// openParent is the parent named by AGGY_PARENT (set by the supervisor
// for the CLI on screen) when it is a UUID the index knows, else the
// latest parent in the current directory.
func openParent(ctx context.Context, s *store.Store) (store.Parent, error) {
	if id := os.Getenv("AGGY_PARENT"); adapter.IsUUID(id) {
		p, err := s.GetParent(ctx, id)
		if !errors.Is(err, store.ErrNotFound) {
			return p, err
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return store.Parent{}, err
	}
	key, err := safefs.CanonicalPath(cwd)
	if err != nil {
		return store.Parent{}, err
	}
	return s.LatestParentInDir(ctx, key, cwd)
}

// when is a local time as aggy's lists show it.
func when(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// tildePath shows a path under HOME as ~/...
func tildePath(p string) string {
	h := os.Getenv("HOME")
	if h == "" || !filepath.IsAbs(h) {
		return p
	}
	if p == h {
		return "~"
	}
	if rel, ok := strings.CutPrefix(p, strings.TrimSuffix(h, "/")+"/"); ok {
		return "~/" + rel
	}
	return p
}

// cutNoPolicy takes a leading --no-policy off args.
func cutNoPolicy(args []string) (bool, []string) {
	if len(args) > 0 && args[0] == "--no-policy" {
		return true, args[1:]
	}
	return false, args
}
