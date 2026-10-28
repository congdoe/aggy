package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/store"
)

// seedInbox records two finished borrows with reports and one running
// borrow without.
func seedInbox(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", filepath.Join(t.TempDir(), "aggy"))
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	dir, _ := home.Dir()
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	p, err := s.ParentForDir(ctx, "/repo", "borrows", "p", now)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"aaaa000000000001", "bbbb000000000002", "bbbb000000000003"} {
		at := now.Add(time.Duration(i) * time.Minute)
		l, err := s.StartBorrow(ctx,
			store.Child{ID: "c" + id, ParentID: p.ID, Vendor: "grok", Kind: "news", Title: "task \x1b[2J" + id[:4], Cwd: "/w", CreatedAt: at, LastActive: at},
			store.Borrow{ID: id, Kind: "news", Vendor: "grok", StartedAt: at}, store.BorrowCaps{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			continue // still running, no report
		}
		rp := filepath.Join(dir, "inbox", id+".md")
		mustWrite(t, rp, "# Borrow "+id+"\n\nreport \x1b]0;pwned\x07 body\n", 0o600)
		if err := l.Finish(ctx, store.BorrowEnd{Status: "done", Outcome: "ok", ReportPath: rp, EndedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInbox(t *testing.T) {
	seedInbox(t)
	steps := []struct {
		name    string
		args    []string
		code    int
		want    []string
		notWant []string
	}{
		{"unread", nil, 0, []string{"aaaa000000000001", "bbbb000000000002", `task \x1b[2Jaaaa`}, []string{"bbbb000000000003", "\x1b"}},
		{"ambiguous prefix", []string{"bbbb"}, 1, nil, nil},
		{"show", []string{"aaaa"}, 0, []string{"# Borrow aaaa000000000001", `report \x1b]0;pwned\x07 body`}, []string{"\x1b", "\x07"}},
		{"read one gone from unread", nil, 0, []string{"bbbb000000000002"}, []string{"aaaa000000000001"}},
		{"all", []string{"--all"}, 0, []string{"aaaa000000000001", "bbbb000000000002 *", "* unread"}, nil},
		{"no report yet", []string{"bbbb000000000003"}, 1, nil, nil},
		{"not hex", []string{"../../etc"}, 2, nil, nil},
		{"too short", []string{"aa"}, 2, nil, nil},
		{"unknown", []string{"cccc"}, 1, nil, nil},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := inboxWith(st.args, time.Now, &out, &errb)
			if code != st.code {
				t.Fatalf("code %d, stderr %q", code, errb.String())
			}
			all := out.String() + errb.String()
			for _, w := range st.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("stdout lacks %q:\n%s", w, out.String())
				}
			}
			for _, w := range st.notWant {
				if strings.Contains(all, w) {
					t.Errorf("output has %q:\n%s", w, all)
				}
			}
		})
	}
}

// TestInboxReadsOnlyTheInboxPath: a report path in the row that points
// elsewhere is never read; the id names the file.
func TestInboxReadsOnlyTheInboxPath(t *testing.T) {
	seedInbox(t)
	other := filepath.Join(t.TempDir(), "secret.md")
	mustWrite(t, other, "SECRET\n", 0o600)
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE borrows SET report_path = ? WHERE id = 'aaaa000000000001'`, other); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	var out, errb bytes.Buffer
	if code := inboxWith([]string{"aaaa"}, time.Now, &out, &errb); code != 0 || strings.Contains(out.String(), "SECRET") {
		t.Errorf("code %d out %q", code, out.String())
	}
}

func TestInboxNoStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", filepath.Join(t.TempDir(), "none"))
	var out, errb bytes.Buffer
	if code := inboxWith(nil, time.Now, &out, &errb); code != 0 || !strings.Contains(out.String(), "No reports yet") {
		t.Errorf("code %d out %q", code, out.String())
	}
}
