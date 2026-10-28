package router

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// fakeStatuses is a vendors table.
type fakeStatuses struct {
	rows []store.VendorStatus
	err  error
}

func (f fakeStatuses) VendorStatuses(context.Context) ([]store.VendorStatus, error) {
	return f.rows, f.err
}

func statuses(kv ...string) fakeStatuses {
	var f fakeStatuses
	for i := 0; i+1 < len(kv); i += 2 {
		f.rows = append(f.rows, store.VendorStatus{Vendor: kv[i], Connected: kv[i+1]})
	}
	return f
}

func allConnected() fakeStatuses {
	return statuses("claude", "connected", "codex", "connected", "grok", "connected")
}

// fakeLimits reports the vendors in limited as at their limit, and
// those in hinted as at a limit only a hook reported.
type fakeLimits struct {
	limited map[adapter.Vendor]time.Time // zero time: limited, reset unknown
	hinted  map[adapter.Vendor]time.Time
	err     error
}

func (f fakeLimits) Limited(_ context.Context, v adapter.Vendor, _ time.Time) (Limit, bool, error) {
	if f.err != nil {
		return Limit{}, false, f.err
	}
	if until, ok := f.hinted[v]; ok {
		return Limit{Until: until, Hint: true}, true, nil
	}
	until, ok := f.limited[v]
	return Limit{Until: until}, ok, nil
}

func repoAllows(v ...adapter.Vendor) func(string) (config.RepoConfig, error) {
	return func(string) (config.RepoConfig, error) {
		return config.RepoConfig{AllowedVendors: append([]adapter.Vendor{}, v...)}, nil
	}
}

func TestSkipRules(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	later := now.Add(2 * time.Hour)
	tests := []struct {
		name      string
		routing   func(*config.Routing)
		req       Request
		deps      Deps
		want      []adapter.Vendor
		wantSkip  []Skip
		wantNotes []string
		wantWarn  []string
		wantErr   string // the whole no-vendor error, or a part of another error
	}{
		{
			name: "all usable",
			req:  Request{Kind: "image", Repo: "/r"},
			deps: Deps{Statuses: allConnected(), RepoConfig: repoAllows(codex, grok)},
			want: vs(codex, grok),
		},
		// Rule 1: the parent's own vendor.
		{
			name:     "parent skipped",
			req:      Request{Kind: "image", Parent: codex},
			deps:     Deps{Statuses: allConnected()},
			want:     vs(grok),
			wantSkip: []Skip{{codex, "the parent is on codex"}},
		},
		{
			name:    "parent is the only vendor",
			req:     Request{Kind: "news", Parent: grok},
			deps:    Deps{Statuses: allConnected()},
			wantErr: "no vendor available for news: grok: the parent is on grok",
		},
		{
			name:    "override to the parent is skipped too",
			req:     Request{Kind: "image", Parent: claude, Override: claude},
			deps:    Deps{Statuses: allConnected()},
			wantErr: "no vendor available for image: claude: the parent is on claude",
		},
		// T6693: the vendors the reviewed commits' trailers name.
		{
			name:     "review skips trailer authors",
			req:      Request{Kind: "review", Parent: claude, Authors: vs(codex)},
			deps:     Deps{Statuses: allConnected()},
			want:     vs(grok),
			wantSkip: []Skip{{codex, "an Aggy-Vendor trailer says it wrote part of the change"}, {claude, "the parent is on claude"}},
		},
		{
			name:     "trailer authors never empty the list",
			req:      Request{Kind: "review", Parent: claude, Authors: vs(codex, grok)},
			deps:     Deps{Statuses: allConnected()},
			want:     vs(codex, grok),
			wantSkip: []Skip{{claude, "the parent is on claude"}},
			wantWarn: []string{"every vendor left wrote part of the change (Aggy-Vendor trailers name codex, grok); reviewing with it anyway"},
		},
		{
			name:     "a not connected vendor stays skipped for its own reason",
			req:      Request{Kind: "review", Authors: vs(codex)},
			deps:     Deps{Statuses: statuses("codex", "connected", "grok", "not_connected", "claude", "connected")},
			want:     vs(claude),
			wantSkip: []Skip{{codex, "an Aggy-Vendor trailer says it wrote part of the change"}, {grok, "not connected"}},
		},
		{
			name: "authors only steer review",
			req:  Request{Kind: "image", Authors: vs(codex)},
			deps: Deps{Statuses: allConnected()},
			want: vs(codex, grok),
		},
		{
			name:     "review skips its author",
			req:      Request{Kind: "review", Parent: claude},
			deps:     Deps{Statuses: allConnected()},
			want:     vs(codex, grok),
			wantSkip: []Skip{{claude, "the parent is on claude"}},
		},
		// Rule 2: connection.
		{
			name:     "not connected skipped",
			req:      Request{Kind: "image"},
			deps:     Deps{Statuses: statuses("codex", "not_connected", "grok", "connected")},
			want:     vs(grok),
			wantSkip: []Skip{{codex, "not connected"}},
		},
		{
			name:     "no row means not set up",
			req:      Request{Kind: "image"},
			deps:     Deps{Statuses: statuses("grok", "connected")},
			want:     vs(grok),
			wantSkip: []Skip{{codex, "not set up (run aggy setup)"}},
		},
		{
			name:      "unknown is usable and reported",
			req:       Request{Kind: "image"},
			deps:      Deps{Statuses: statuses("codex", "unknown", "grok", "connected")},
			want:      vs(codex, grok),
			wantNotes: []string{"codex: sign-in status unknown; trying it anyway"},
		},
		{
			name:      "no store treats every vendor as unknown",
			req:       Request{Kind: "video"},
			deps:      Deps{},
			want:      vs(grok),
			wantNotes: []string{"grok: sign-in status unknown; trying it anyway"},
		},
		{
			name:     "odd status skipped",
			req:      Request{Kind: "image"},
			deps:     Deps{Statuses: statuses("codex", "maybe", "grok", "connected")},
			want:     vs(grok),
			wantSkip: []Skip{{codex, `sign-in status "maybe" not understood`}},
		},
		{
			name:    "store error fails closed",
			req:     Request{Kind: "image"},
			deps:    Deps{Statuses: fakeStatuses{err: errors.New("disk on fire")}},
			wantErr: "read vendor status: disk on fire",
		},
		// Rule 3: the repo's allowed_vendors.
		{
			name:     "outside allowed_vendors skipped",
			req:      Request{Kind: "image", Repo: "/r"},
			deps:     Deps{Statuses: allConnected(), RepoConfig: repoAllows(grok)},
			want:     vs(grok),
			wantSkip: []Skip{{codex, "not in this repo's allowed_vendors"}},
		},
		{
			name:    "empty allowed_vendors allows none",
			req:     Request{Kind: "image", Repo: "/r"},
			deps:    Deps{Statuses: allConnected(), RepoConfig: repoAllows()},
			wantErr: "no vendor available for image: codex: not in this repo's allowed_vendors; grok: not in this repo's allowed_vendors",
		},
		{
			name:    "override cannot pass allowed_vendors",
			req:     Request{Kind: "image", Repo: "/r", Override: codex},
			deps:    Deps{Statuses: allConnected(), RepoConfig: repoAllows(grok)},
			wantErr: "no vendor available for image: codex: not in this repo's allowed_vendors",
		},
		{
			name: "unset allowed_vendors allows all",
			req:  Request{Kind: "image", Repo: "/r"},
			deps: Deps{Statuses: allConnected(), RepoConfig: func(string) (config.RepoConfig, error) { return config.RepoConfig{}, nil }},
			want: vs(codex, grok),
		},
		{
			name: "no repo ignores the loader",
			req:  Request{Kind: "image"},
			deps: Deps{Statuses: allConnected(), RepoConfig: func(string) (config.RepoConfig, error) {
				return config.RepoConfig{}, errors.New("must not be called")
			}},
			want: vs(codex, grok),
		},
		{
			name: "repo config error fails closed",
			req:  Request{Kind: "image", Repo: "/r"},
			deps: Deps{Statuses: allConnected(), RepoConfig: func(string) (config.RepoConfig, error) {
				return config.RepoConfig{}, errors.New("bad toml")
			}},
			wantErr: "read repo config: bad toml",
		},
		// Rule 4: recent limits.
		{
			name:     "limited with a reset time",
			req:      Request{Kind: "image"},
			deps:     Deps{Statuses: allConnected(), Limits: fakeLimits{limited: map[adapter.Vendor]time.Time{codex: later}}, Now: func() time.Time { return now }},
			want:     vs(grok),
			wantSkip: []Skip{{codex, "hit its limit; expected back at " + later.Local().Format("Jan 2 15:04")}},
		},
		{
			name:     "limited, reset unknown",
			req:      Request{Kind: "image"},
			deps:     Deps{Statuses: allConnected(), Limits: fakeLimits{limited: map[adapter.Vendor]time.Time{grok: {}}}, Now: func() time.Time { return now }},
			want:     vs(codex),
			wantSkip: []Skip{{grok, "hit its limit recently"}},
		},
		{
			name:     "a hook-only limit skips while another vendor is left",
			req:      Request{Kind: "image"},
			deps:     Deps{Statuses: allConnected(), Limits: fakeLimits{hinted: map[adapter.Vendor]time.Time{codex: later}}, Now: func() time.Time { return now }},
			want:     vs(grok),
			wantSkip: []Skip{{codex, "a hook reported its limit (unconfirmed); expected back at " + later.Local().Format("Jan 2 15:04")}},
		},
		{
			name:     "a hook-only limit never disables the last vendor",
			req:      Request{Kind: "image", Parent: grok},
			deps:     Deps{Statuses: allConnected(), Limits: fakeLimits{hinted: map[adapter.Vendor]time.Time{codex: later}}, Now: func() time.Time { return now }},
			want:     vs(codex),
			wantSkip: []Skip{{grok, "the parent is on grok"}},
			wantWarn: []string{"codex: a hook reported its limit, which aggy cannot confirm; no other vendor can run this, so trying it anyway"},
		},
		{
			name: "a vendor that refuses the kind does not count as left",
			routing: func(r *config.Routing) {
				r.Research = config.Route{Vendors: vs(grok, codex)}
			},
			req:       Request{Kind: "research"},
			deps:      Deps{Statuses: allConnected(), Limits: fakeLimits{hinted: map[adapter.Vendor]time.Time{grok: later}}, Now: func() time.Time { return now }},
			want:      vs(grok, codex),
			wantNotes: []string{"codex: research: not supported"},
			wantWarn:  []string{"grok: a hook reported its limit, which aggy cannot confirm; no other vendor can run this, so trying it anyway"},
		},
		{
			name:     "IgnoreHints keeps hinted vendors",
			req:      Request{Kind: "image", IgnoreHints: true},
			deps:     Deps{Statuses: allConnected(), Limits: fakeLimits{hinted: map[adapter.Vendor]time.Time{grok: later}, limited: map[adapter.Vendor]time.Time{claude: {}}}, Now: func() time.Time { return now }},
			want:     vs(codex, grok),
			wantWarn: []string{"grok: a hook reported its limit, which aggy cannot confirm; no other vendor can run this, so trying it anyway"},
		},
		{
			name:    "a confirmed limit still disables the last vendor",
			req:     Request{Kind: "image", Parent: grok},
			deps:    Deps{Statuses: allConnected(), Limits: fakeLimits{limited: map[adapter.Vendor]time.Time{codex: {}}}, Now: func() time.Time { return now }},
			wantErr: "no vendor available for image: codex: hit its limit recently; grok: the parent is on grok",
		},
		{
			name:    "limit state error fails closed",
			req:     Request{Kind: "image"},
			deps:    Deps{Statuses: allConnected(), Limits: fakeLimits{err: errors.New("x")}},
			wantErr: "codex: read limit state: x",
		},
		// Combinations: each vendor reports the first rule that matched,
		// in rule order.
		{
			name: "every rule at once",
			routing: func(r *config.Routing) {
				r.Research = config.Route{Vendors: vs(grok, codex, claude)}
			},
			req:     Request{Kind: "research", Parent: grok, Repo: "/r"},
			deps:    Deps{Statuses: statuses("grok", "not_connected", "codex", "not_connected", "claude", "connected"), RepoConfig: repoAllows(grok, codex)},
			wantErr: "no vendor available for research: grok: the parent is on grok; codex: not connected; claude: not in this repo's allowed_vendors",
		},
		{
			name:    "connection beats allowed_vendors and limits",
			req:     Request{Kind: "video", Repo: "/r"},
			deps:    Deps{Statuses: statuses("grok", "not_connected"), RepoConfig: repoAllows(), Limits: fakeLimits{limited: map[adapter.Vendor]time.Time{grok: {}}}},
			wantErr: "no vendor available for video: grok: not connected",
		},
		{
			name:    "allowed_vendors beats limits",
			req:     Request{Kind: "video", Repo: "/r"},
			deps:    Deps{Statuses: allConnected(), RepoConfig: repoAllows(codex), Limits: fakeLimits{limited: map[adapter.Vendor]time.Time{grok: {}}}},
			wantErr: "no vendor available for video: grok: not in this repo's allowed_vendors",
		},
		{
			name: "codex research kept with its refusal noted, grok skipped",
			routing: func(r *config.Routing) {
				r.Research = config.Route{Vendors: vs(grok, codex)}
			},
			req:       Request{Kind: "research", Parent: grok},
			deps:      Deps{Statuses: statuses("codex", "unknown", "grok", "connected")},
			want:      vs(codex),
			wantSkip:  []Skip{{grok, "the parent is on grok"}},
			wantNotes: []string{"codex: sign-in status unknown; trying it anyway", "codex: research: not supported by this CLI until T6638"},
		},
		{
			name: "review mix keeps order",
			routing: func(r *config.Routing) {
				r.Review = config.Route{Vendors: vs(grok, claude, codex)}
			},
			req:      Request{Kind: "review", Parent: codex, Repo: "/r"},
			deps:     Deps{Statuses: allConnected(), RepoConfig: repoAllows(claude, codex, grok)},
			want:     vs(grok, claude),
			wantSkip: []Skip{{codex, "the parent is on codex"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			if tt.routing != nil {
				tt.routing(&cfg.Routing)
			}
			d, err := New(cfg, tt.deps).Decide(context.Background(), tt.req)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Decide = %+v, want error %q", d, tt.wantErr)
				}
				if errors.Is(err, ErrNoVendor) {
					if err.Error() != tt.wantErr {
						t.Fatalf("err = %q\nwant  %q", err, tt.wantErr)
					}
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(d.Candidates, tt.want) {
				t.Errorf("candidates = %v, want %v", d.Candidates, tt.want)
			}
			if !slices.Equal(d.Skipped, tt.wantSkip) {
				t.Errorf("skipped = %v, want %v", d.Skipped, tt.wantSkip)
			}
			if !slices.Equal(d.Warnings, tt.wantWarn) {
				t.Errorf("warnings = %q, want %q", d.Warnings, tt.wantWarn)
			}
			if len(d.Notes) != len(tt.wantNotes) {
				t.Fatalf("notes = %q, want %q", d.Notes, tt.wantNotes)
			}
			for i, n := range tt.wantNotes {
				if !strings.HasPrefix(d.Notes[i], n) {
					t.Errorf("note %d = %q, want prefix %q", i, d.Notes[i], n)
				}
			}
		})
	}
}

func TestNoVendorErrorShape(t *testing.T) {
	_, err := New(config.Default(), Deps{Statuses: statuses("grok", "not_connected")}).Choose(context.Background(), Request{Kind: "news"})
	var nv *NoVendorError
	if !errors.As(err, &nv) || !errors.Is(err, ErrNoVendor) {
		t.Fatalf("err = %v, want a *NoVendorError matching ErrNoVendor", err)
	}
	if nv.Kind != adapter.News || !slices.Equal(nv.Skipped, []Skip{{grok, "not connected"}}) {
		t.Errorf("err = %+v", nv)
	}
}

// TestStoreSatisfiesStatuses keeps *store.Store usable as Deps.Statuses.
func TestStoreSatisfiesStatuses(t *testing.T) {
	var _ Statuses = (*store.Store)(nil)
}

func TestNoLimits(t *testing.T) {
	if lim, l, err := (NoLimits{}).Limited(context.Background(), grok, time.Now()); l || lim != (Limit{}) || err != nil {
		t.Errorf("NoLimits = %v %v %v", lim, l, err)
	}
}

// TestDefaultRepoLoader reads a real repo config through config.LoadRepo
// in a temp HOME.
func TestDefaultRepoLoader(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	writeRepoConfig(t, repo, "allowed_vendors = [\"grok\"]\n")
	d, err := New(config.Default(), Deps{Statuses: allConnected()}).Decide(context.Background(), Request{Kind: "image", Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Candidates, vs(grok)) || !slices.Equal(d.Skipped, []Skip{{codex, "not in this repo's allowed_vendors"}}) {
		t.Errorf("decision = %+v", d)
	}
}

func writeRepoConfig(t *testing.T, repo, text string) {
	t.Helper()
	if err := safefs.MkdirAll(filepath.Join(repo, config.RepoDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := safefs.WriteFile(config.RepoPath(repo), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
