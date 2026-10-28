package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/store"
)

func TestSessionsNoIndex(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", filepath.Join(t.TempDir(), "aggy"))
	for _, cmd := range []string{"sessions", "session"} {
		var out, errb bytes.Buffer
		if code := Run([]string{cmd}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Run aggy to start one") {
			t.Errorf("%s = %d, %q, %q", cmd, code, out.String(), errb.String())
		}
	}
	if _, err := os.Stat(os.Getenv("AGGY_HOME")); !os.IsNotExist(err) {
		t.Errorf("listing created the state dir: %v", err)
	}
}

func TestSessionsList(t *testing.T) {
	s := sessionStore(t)
	home := os.Getenv("HOME")
	here := filepath.Join(home, "work")
	if err := os.MkdirAll(here, 0o700); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	seedParent(t, s, "aaaa0000-0000-4000-8000-000000000000", other,
		store.Child{ID: "o1", Vendor: "grok", VendorSessionID: sidB})
	seedParent(t, s, "bbbb0000-0000-4000-8000-000000000000", here,
		store.Child{ID: "c1", Vendor: "claude", VendorSessionID: sidA, Model: "opus"},
		store.Child{ID: "c2", Vendor: "codex", Kind: "review", Title: "check \x1b[31mred"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(here)
	t.Setenv("AGGY_PARENT", "")

	var out, errb bytes.Buffer
	if code := Run([]string{"sessions"}, &out, &errb); code != 0 {
		t.Fatalf("sessions = %d: %s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"aaaa0000", "bbbb0000", "~/work", other} {
		if !strings.Contains(got, want) {
			t.Errorf("sessions output lacks %q:\n%s", want, got)
		}
	}
	for _, never := range []string{sidA, sidB} {
		if strings.Contains(got, never) {
			t.Errorf("sessions printed a vendor session id:\n%s", got)
		}
	}

	out.Reset()
	if code := Run([]string{"session"}, &out, &errb); code != 0 {
		t.Fatalf("session = %d: %s", code, errb.String())
	}
	got = out.String()
	for _, want := range []string{"bbbb0000", "claude", "interactive", "opus", "codex", "review", "default"} {
		if !strings.Contains(got, want) {
			t.Errorf("session output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") || strings.Contains(got, "grok") {
		t.Errorf("session output has an escape or another parent's child:\n%q", got)
	}

	// Inside a supervised CLI, AGGY_PARENT picks the parent.
	t.Setenv("AGGY_PARENT", "aaaa0000-0000-4000-8000-000000000000")
	out.Reset()
	if code := Run([]string{"session"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "grok") {
		t.Errorf("session with AGGY_PARENT = %d:\n%s", code, out.String())
	}
}

func TestSessionArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", t.TempDir())
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"resume", "zz"}, "is not a session id"},
		{[]string{"resume", "abc"}, "is not a session id"},
		{[]string{"resume", "abcd", "x"}, "usage: aggy resume"},
		{[]string{"resume", "\x1b[2Jabcd"}, "is not a session id"},
		{[]string{"new", "gpt"}, "unknown vendor"},
		{[]string{"new", "--brief"}, "usage: aggy new"},
		{[]string{"sessions", "x"}, "usage: aggy sessions"},
		{[]string{"session", "x"}, "usage: aggy session"},
	}
	for _, tt := range tests {
		var out, errb bytes.Buffer
		if code := Run(tt.args, &out, &errb); code != 2 || !strings.Contains(errb.String(), tt.want) {
			t.Errorf("%q = %d, %q; want 2 and %q", tt.args, code, errb.String(), tt.want)
		}
		if strings.Contains(errb.String(), "\x1b") {
			t.Errorf("%q echoed an escape: %q", tt.args, errb.String())
		}
	}
}
