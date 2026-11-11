package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/congdoe/aggy/internal/adapter/hermes"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// A Hermes profile aggy made is recorded for aggy unwire (#134 review): a
// marker row in the index's changes, in the run that wrote aggy's block into
// it, naming the profile's folder. aggy unwire deletes the profile with
// `hermes profile delete <name> --yes`, through the Hermes runner, and only a
// profile such a row names: never one the person made, and never the
// default, which no aggy name can be (hermes.ValidProfile). Nothing of the
// profile's config.yaml, which Hermes seeded from the person's root config
// (hermes_cli/profiles.py:735-748), goes into the row, the log, or a message:
// only the folder's path and the profile's name.

// recordHermesProfile records that aggy made the profile at dir, in run.
func recordHermesProfile(ctx context.Context, run, dir string) error {
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	now := time.Now()
	return s.InsertChange(ctx, store.Change{ChangeID: manifest.NewRunID(), RunID: run, File: dir, HashBefore: "none", HashAfter: "none",
		Status: store.ChangeApplied, Kind: store.KindMarker, Reverse: hermes.ProfileReverse, PlannedAt: now, AppliedAt: now})
}

// hermesProfileMarkers are the applied hermes-profile markers sel names.
func hermesProfileMarkers(ctx context.Context, s *store.Store, sel manifest.Selection) ([]store.Change, error) {
	var cs []store.Change
	switch {
	case sel.ChangeID != "":
		c, err := s.GetChange(ctx, sel.ChangeID)
		if err != nil {
			return nil, nil
		}
		cs = []store.Change{c}
	default:
		all, err := s.Changes(ctx, sel.RunID)
		if err != nil {
			return nil, err
		}
		cs = all
	}
	var out []store.Change
	for _, c := range cs {
		if c.Kind == store.KindMarker && c.Reverse == hermes.ProfileReverse && c.Status == store.ChangeApplied {
			out = append(out, c)
		}
	}
	return out, nil
}

// unwireHermesProfiles deletes the profiles the markers name, newest first,
// or says what it would do on a dry run. It returns how many it deleted (or
// would) and whether any was left, with a line saying what is left and the
// command to run.
func unwireHermesProfiles(ctx context.Context, s *store.Store, markers []store.Change, dryRun bool, stdout, stderr io.Writer) (done int, left bool) {
	for i := len(markers) - 1; i >= 0; i-- {
		c := markers[i]
		dir, id := termsafe.Field(c.File), termsafe.Field(c.ChangeID)
		name := filepath.Base(c.File)
		manual := "hermes profile delete " + name + " --yes"
		leave := func(why string) {
			left = true
			fmt.Fprintf(stderr, "left aggy's Hermes profile %s at %s (change %s): %s; run %s to remove it %s\n", name, dir, id, why, manual, dlog.Tag(diag.CodeUnwireFailed))
		}
		if !hermes.ValidProfile(name) || filepath.Base(filepath.Dir(c.File)) != "profiles" {
			left = true
			fmt.Fprintf(stderr, "refused change %s: it does not name a Hermes profile aggy makes %s\n", id, dlog.Tag(diag.CodeUnwireRefused))
			continue
		}
		root, err := hermesRoot(os.Getenv)
		if err != nil || filepath.Join(root, "profiles", name) != c.File {
			leave("Hermes' home is not where it was when aggy made it (HERMES_HOME or HERMES_DATA_DIR_SUFFIX changed)")
			continue
		}
		if dryRun {
			done++
			fmt.Fprintf(stdout, "would be deleted: aggy's Hermes profile %s at %s (change %s), with %s\n", name, dir, id, manual)
			continue
		}
		path, err := probe.LookPath(hermes.Bin)
		if err != nil {
			leave("Hermes is not installed, so aggy cannot run its profile delete")
			continue
		}
		cmd, err := hermes.ProfileDeleteCommand(path, name)
		if err != nil {
			leave(err.Error())
			continue
		}
		o, err := hermesRun(ctx, cmd)
		switch {
		case err != nil:
			leave("hermes profile delete did not run")
			continue
		case o.ExitCode != 0:
			leave(fmt.Sprintf("hermes profile delete exited %d", o.ExitCode))
			continue
		}
		if err := s.MarkHermesProfileGone(ctx, c.ChangeID, time.Now()); err != nil {
			fmt.Fprintf(stderr, "warning: deleted aggy's Hermes profile %s, but could not record it: %s\n", name, termsafe.Field(err.Error()))
		}
		done++
		fmt.Fprintf(stdout, "deleted aggy's Hermes profile %s at %s (change %s)\n", name, dir, id)
	}
	return done, left
}
