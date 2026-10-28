package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
)

func TestParseMediaArgs(t *testing.T) {
	flags := []string{"--out", "--ref", "--n", "--vendor"}
	tests := []struct {
		name    string
		args    []string
		want    mediaArgs
		wantErr string
	}{
		{"all flags", []string{"a hero", "--out", "p/h.png", "--ref=s.png", "--ref", "t.jpg", "--n", "3", "--vendor", "codex", "--allow-untested"},
			mediaArgs{brief: "a hero", out: "p/h.png", refs: []string{"s.png", "t.jpg"}, n: 3, vendor: "codex", allowUntested: true}, ""},
		{"dash brief", []string{"--out", "h.png", "--", "-dark"}, mediaArgs{brief: "-dark", out: "h.png"}, ""},
		{"help", []string{"--help"}, mediaArgs{help: true}, ""},
		{"n zero", []string{"x", "--n", "0"}, mediaArgs{}, "--n must be a number from 1 to 4"},
		{"n five", []string{"x", "--n", "5"}, mediaArgs{}, "--n must be"},
		{"n word", []string{"x", "--n", "two"}, mediaArgs{}, "--n must be"},
		{"unknown flag", []string{"x", "--path", "a"}, mediaArgs{}, "unknown flag --path"},
		{"missing value", []string{"x", "--out"}, mediaArgs{}, "needs a value"},
		{"two briefs", []string{"a", "b"}, mediaArgs{}, "want one quoted brief"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMediaArgs(tt.args, flags)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.brief != tt.want.brief || got.out != tt.want.out || got.n != tt.want.n || got.vendor != tt.want.vendor ||
				got.help != tt.want.help || got.allowUntested != tt.want.allowUntested || !slices.Equal(got.refs, tt.want.refs) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func (f *borrowFixture) media(t *testing.T, kind adapter.Kind, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := mediaWith(kind, args, f.env, &out, &errb)
	return code, out.String(), errb.String()
}

func TestImageCommandRefusals(t *testing.T) {
	f := newBorrowFixture(t, adapter.Codex)
	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no out", []string{"a cat"}, 2, "--out is required"},
		{"out outside the repo", []string{"a cat", "--out", "/elsewhere/cat.png"}, 1, "must be a file inside"},
		{"out in .git", []string{"a cat", "--out", ".git/cat.png"}, 1, "outside its .git"},
		{"wrong extension", []string{"a cat", "--out", "cat.txt"}, 1, "must end in .png"},
		{"both", []string{"a cat", "--out", "cat.png", "--vendor", "both"}, 1, "research only"},
		{"edit without a sidecar", []string{"--edit", "cat.png", "darker"}, 1, "no sidecar aggy wrote"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, errOut := f.media(t, adapter.Image, tt.args...)
			if code != tt.code || !strings.Contains(errOut, tt.want) {
				t.Errorf("code %d stderr %q", code, errOut)
			}
		})
	}
	if recs := fakecli.Records(t, f.record); slices.ContainsFunc(recs, func(r fakecli.Record) bool { return r.Mode == "headless" }) {
		t.Error("a refused image borrow ran a child")
	}
}

// TestImageCommand (T6634): aggy image copies the image into the repo
// and prints where it went.
func TestImageCommand(t *testing.T) {
	f := newBorrowFixture(t, adapter.Codex)
	f.scenario["codex"] = `{"images":1}`
	code, out, errOut := f.media(t, adapter.Image, "a cat", "--out", "pub/cat.png")
	if code != 0 {
		t.Fatalf("code %d stderr %q stdout %q", code, errOut, out)
	}
	p := filepath.Join(f.repo, "pub", "cat.png")
	for _, w := range []string{"image on codex", "1 files made, 1 kept", "image: " + p} {
		if !strings.Contains(out, w) {
			t.Errorf("stdout lacks %q:\n%s", w, out)
		}
	}
	for _, q := range []string{p, filepath.Join(f.repo, ".aggy", "assets", "pub", "cat.png.json")} {
		if _, err := os.Stat(q); err != nil {
			t.Error(err)
		}
	}
}
