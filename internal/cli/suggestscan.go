package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/suggest"
	"github.com/congdoe/aggy/internal/transcript"
)

const (
	// scanChunk is how much of a file one transaction covers.
	scanChunk = 8 << 20
	// scanBudget is how much one scan reads in all; the next goes on from there.
	scanBudget = 512 << 20
	// scanStopWait bounds how long a session's end waits for the job to stop.
	scanStopWait = 2 * time.Second
)

// suggestAtLaunch is the scan when aggy opens: a background job in this process,
// not a daemon, that never delays the launch and fails quietly. It returns what
// stops it, and a line if earlier scans found new candidates. Candidates never
// become allows on their own.
func suggestAtLaunch(ctx context.Context, s *store.Store, cwd string) (stop func(), note string) {
	if repo, err := toolRepo(cwd, false); err == nil && repo.key != "" {
		if n, err := s.AnnounceSuggest(ctx, repo.key, suggest.MinCount); err == nil && n > 0 {
			note = fmt.Sprintf("%d commands you run often could be allowed here: aggy permissions suggest", n)
			if n == 1 {
				note = "1 command you run often could be allowed here: aggy permissions suggest"
			}
		}
	}
	scanCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }() // a bad line must never reach a session
		js, err := store.OpenDefault(scanCtx)
		if err != nil {
			return
		}
		defer func() { _ = js.Close() }()
		scanRepo(scanCtx, js, cwd)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(scanStopWait):
		}
	}, note
}

// scanRepo reads this repo's Claude session files: those of the main checkout
// and of the work tree it is in.
func scanRepo(ctx context.Context, s *store.Store, cwd string) {
	repo, err := toolRepo(cwd, false)
	if err != nil || repo.key == "" {
		return
	}
	home, err := transcript.Home(adapter.Claude, os.Getenv)
	if err != nil {
		return
	}
	accept := func(line string) (string, bool) {
		// Each line says where it ran, as different folders can share a
		// session folder's name.
		if line != "" && (pathWithin(line, repo.root) || pathWithin(line, repo.main)) {
			return repo.key, true
		}
		return "", false
	}
	names := []string{transcript.ClaudeProjectDir(repo.main)}
	if repo.root != repo.main {
		names = append(names, transcript.ClaudeProjectDir(repo.root))
	}
	budget := int64(scanBudget)
	for _, name := range names {
		scanFolder(ctx, s, filepath.Join(home, "projects", name), accept, &budget)
	}
}

// scanAllRepos reads every project's session files, each line going to the repo
// its cwd is in. That cwd is what the file says, so a forged line can name a
// repo; it can only make a candidate, which the person sees with its repo.
func scanAllRepos(ctx context.Context, s *store.Store) {
	home, err := transcript.Home(adapter.Claude, os.Getenv)
	if err != nil {
		return
	}
	root := filepath.Join(home, "projects")
	if !plainDir(root) {
		return
	}
	entries, err := safefs.ReadDir(root)
	if err != nil {
		return
	}
	known := map[string]string{}
	accept := func(line string) (string, bool) {
		key, seen := known[line]
		if !seen {
			if repo, err := toolRepo(line, false); err == nil && plainDir(line) {
				key = repo.key
			}
			known[line] = key
		}
		return key, key != ""
	}
	budget := int64(scanBudget)
	for _, e := range entries {
		if e.IsDir() {
			scanFolder(ctx, s, filepath.Join(root, e.Name()), accept, &budget)
		}
	}
}

// pathWithin reports a path at or under dir. Both are as the CLI spelled them.
func pathWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// plainDir is a folder that is not a symlink.
func plainDir(p string) bool {
	fi, err := safefs.Lstat(p)
	return err == nil && fi.IsDir()
}

func scanFolder(ctx context.Context, s *store.Store, dir string, accept suggest.Accept, budget *int64) {
	if !plainDir(dir) {
		return
	}
	entries, err := safefs.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil || *budget <= 0 {
			return
		}
		if !suggest.ProjectFile(e.Name()) {
			continue
		}
		scanFile(ctx, s, filepath.Join(dir, e.Name()), accept, budget)
	}
}

// scanFile reads new lines of one file, in small transactions that hold the read
// position, so a stop loses nothing and a second aggy cannot count a line twice.
func scanFile(ctx context.Context, s *store.Store, path string, accept suggest.Accept, budget *int64) {
	f, err := safefs.OpenRegular(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	for ctx.Err() == nil && *budget > 0 {
		row, err := s.SuggestFileState(ctx, path)
		if err != nil {
			return
		}
		old := row.Offset
		st := suggest.State{Offset: row.Offset, Mode: row.Mode}
		if json.Unmarshal([]byte(row.Pending), &st.Pending) != nil {
			st.Pending = nil
		}
		if fi.Size() < st.Offset {
			// A file that shrank is another file: start it again.
			st = suggest.State{}
		}
		if fi.Size() == st.Offset {
			return
		}
		if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
			return
		}
		res, err := suggest.Scan(f, st, accept, scanChunk)
		if err != nil {
			return
		}
		read := res.State.Offset - st.Offset
		pending, err := json.Marshal(res.State.Pending)
		if err != nil {
			return
		}
		next := store.SuggestFile{Path: path, Offset: res.State.Offset, Mode: res.State.Mode, Pending: string(pending),
			Shells: row.Shells + int64(res.Shells), WithMode: row.WithMode + int64(res.WithMode)}
		counts := make([]store.SuggestCount, 0, len(res.Counts))
		for _, c := range res.Counts {
			counts = append(counts, store.SuggestCount{Repo: c.Repo, Command: c.Command, N: c.N})
		}
		saved, err := s.SaveSuggest(ctx, old, next, counts, time.Now())
		if err != nil || !saved {
			return
		}
		*budget -= read
		if res.EOF || read == 0 {
			return
		}
	}
}

// suggestFormatMoved says the scans see shell calls but no permission mode, so
// the fields they read have probably changed (aggy doctor).
func suggestFormatMoved(ctx context.Context) (bool, error) {
	// aggy doctor only reads: no index yet is no finding, and an older one
	// has no such table, which is an error here and no finding there.
	path, err := store.DefaultPath()
	if err != nil {
		return false, err
	}
	s, err := store.OpenExisting(ctx, path)
	if err != nil {
		return false, err
	}
	defer func() { _ = s.Close() }()
	shells, withMode, err := s.SuggestSeen(ctx)
	if err != nil {
		return false, err
	}
	return shells >= 20 && withMode == 0, nil
}
