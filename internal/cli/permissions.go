package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/transcript"
)

// permissionsFile is ~/.aggy/permissions.json.
func permissionsFile() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, permissions.FileName), nil
}

// trustFile keeps the repos the person trusted with aggy trust <dir>, in
// aggy's state folder, so a repo can never name itself.
func trustFile() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "trust.json"), nil
}

// repoTrusted reports whether the repo with this key is trusted. A file that
// cannot be read trusts nothing.
func repoTrusted(key string) bool {
	if key == "" {
		return false
	}
	path, err := trustFile()
	if err != nil {
		return false
	}
	b, err := safefs.ReadConfig(path, config.MaxFileSize)
	if err != nil {
		return false
	}
	var t openCodeTrust
	if err := json.Unmarshal(b, &t); err != nil {
		return false
	}
	return slices.Contains(t.Repos, key)
}

// permissionInput says where the rules for dir come from.
func permissionInput(dir string) permissions.Input {
	var in permissions.Input
	in.ClaudeDir, _ = transcript.Home(adapter.Claude, os.Getenv)
	in.AggyFile, _ = permissionsFile()
	if repo, err := toolRepo(dir, false); err == nil && repo.main != "" {
		in.Main, in.RepoKey = repo.main, repo.key
		in.Trusted = repoTrusted(repo.key)
	}
	return in
}

// gitTracks asks git whether it tracks path, under main. git's own config
// cannot run anything here (gitSafe), and an error counts as tracked.
func gitTracks(ctx context.Context, main string) func(string) (bool, error) {
	return func(path string) (bool, error) {
		rel, err := filepath.Rel(main, path)
		if err != nil {
			return true, err
		}
		out, _, err := defaultBorrowEnv().git(ctx, main, "ls-files", "-z", "--", rel)
		if err != nil {
			return true, err
		}
		return out != "", nil
	}
}

// loadPermissions reads the rules in force for dir, and the mode the person
// set in aggy's config, which comes first.
func loadPermissions(ctx context.Context, dir string, cfg config.Config) permissions.Set {
	in := permissionInput(dir)
	if in.Main != "" {
		in.Tracked = gitTracks(ctx, in.Main)
	}
	s := permissions.Load(in)
	if mode, err := permissions.CheckMode(cfg.Permissions.Mode); err == nil {
		s.Mode, s.ModeFrom = mode, permissions.FromConfig
	}
	return s
}

// addPermissions puts the shared rules on req for vendor v, and returns the
// launch's notes. A rule that cannot reach a CLI never stops the launch: it
// only means that CLI asks.
func addPermissions(v adapter.Vendor, caps []string, s permissions.Set, req *adapter.InteractiveRequest) []string {
	var notes []string
	for _, p := range s.Problems {
		notes = append(notes, fmt.Sprintf("a permissions file is not used: %s: %s", p.Path, p.Why))
	}
	if v != adapter.Claude && v != adapter.Grok {
		return notes
	}
	r := s.Render(v)
	skipped := len(r.Skipped)
	for _, k := range s.Skipped {
		// What a CLI reads itself is not aggy's to pass on, so it is no note.
		if k.Origin != permissions.FromClaudeUser && k.Origin != permissions.FromClaudeLocal {
			skipped++
		}
	}
	if skipped > 0 {
		notes = append(notes, skippedNote(skipped, vendorNames[v]))
	}
	switch v {
	case adapter.Claude:
		if !slices.Contains(caps, probe.CapSettings) {
			if len(r.Allow)+len(r.Ask)+len(r.Deny) > 0 || r.Mode != "" {
				notes = append(notes, "claude --help does not list --settings, so your rules from aggy do not reach this session")
			}
			return notes
		}
	case adapter.Grok:
		if len(r.Allow) > 0 && !slices.Contains(caps, probe.CapAllow) {
			notes = append(notes, "grok --help does not list --allow, so your allow rules from aggy do not reach this session")
			r.Allow = nil
		}
		if len(r.Deny) > 0 && !slices.Contains(caps, probe.CapDeny) {
			notes = append(notes, "grok --help does not list --deny, so your deny rules from aggy do not reach this session")
			r.Deny = nil
		}
		if r.Mode != "" && !slices.Contains(caps, probe.CapPermissionMode) {
			notes = append(notes, "grok --help does not list --permission-mode, so the mode set in aggy does not reach this session")
			r.Mode = ""
		}
		if slices.ContainsFunc(req.Extra, func(a string) bool {
			return a == "--permission-mode" || strings.HasPrefix(a, "--permission-mode=")
		}) {
			r.Mode = "" // the person's own flag, for this launch
		}
	}
	req.Rules = adapter.SharedRules{Allow: r.Allow, Ask: r.Ask, Deny: r.Deny, Mode: r.Mode}
	return notes
}

// skippedNote is the launch's one line about rules that do not apply.
func skippedNote(n int, name string) string {
	if n == 1 {
		return fmt.Sprintf("1 of your permission rules does not apply on %s; aggy permissions show says why", name)
	}
	return fmt.Sprintf("%d of your permission rules do not apply on %s; aggy permissions show says why", n, name)
}

// guardRules are the person's exact rules the OpenCode guard must count, for
// the repo in its payload. Its gate only decides when to scan, so it errs wide:
// it does not ask git or trust, and it adds what the running launch rendered, as
// a rule removed since is still in OpenCode's config. It starts nothing, and a
// payload it cannot read gives none: the guard then refuses or scans anyway.
func guardRules(payload []byte, getenv func(string) string) []string {
	var in struct {
		Root string `json:"root"`
	}
	if err := json.Unmarshal(payload, &in); err != nil || in.Root == "" {
		return nil
	}
	pi := permissionInput(in.Root)
	pi.Trusted = true
	pi.Tracked = func(string) (bool, error) { return false, nil }
	shared, _ := parity.OpenCodeShared(permissions.Load(pi))
	return append(shared.Allow, renderedOpenCodeAllows(getenv("OPENCODE_CONFIG"))...)
}

// renderedOpenCodeAllows reads the shell allows of a rendered config.
func renderedOpenCodeAllows(path string) []string {
	if path == "" {
		return nil
	}
	b, err := safefs.ReadConfig(path, config.MaxFileSize)
	if err != nil {
		return nil
	}
	return parity.OpenCodeRenderedAllows(b)
}
