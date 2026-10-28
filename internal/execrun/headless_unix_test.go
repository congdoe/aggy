//go:build unix

package execrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
)

// TestHeadlessRunnerStdin (T6620): the prompt file is the child's stdin,
// and only a regular file reached without a symlink is accepted.
func TestHeadlessRunnerStdin(t *testing.T) {
	f := setup(t)
	dir := t.TempDir()
	prompt := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(prompt, []byte("the prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(prompt, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	run := HeadlessRunner(10 * time.Second)
	cmd := func(stdin string) adapter.Command {
		return adapter.Command{Path: f.fakes.Claude, Args: []string{"-p"}, Dir: dir,
			Env:   []string{fakecli.EnvRecord + "=" + f.record},
			Stdin: adapter.StdinFile, StdinPath: stdin}
	}
	tests := []struct {
		name    string
		c       adapter.Command
		wantErr string
	}{
		{"regular file", cmd(prompt), ""},
		{"symlink", cmd(link), "not a regular file"},
		{"fifo", cmd(fifo), "not a regular file"},
		{"relative", cmd("prompt.md"), "not absolute"},
		{"no dir", adapter.Command{Path: f.fakes.Claude, Args: []string{"-p", "x"}}, "absolute working dir"},
		{"terminal", adapter.Command{Path: f.fakes.Claude, Dir: dir, Stdin: adapter.StdinTerminal}, "no terminal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := run(context.Background(), tt.c)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || out.ExitCode != 0 {
				t.Fatalf("run: %v, exit %d %s", err, out.ExitCode, out.Stderr)
			}
			recs := fakecli.Records(t, f.record)
			if len(recs) == 0 || recs[len(recs)-1].Stdin != "the prompt\n" {
				t.Errorf("stdin not passed: %+v", recs)
			}
		})
	}
}
