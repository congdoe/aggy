//go:build unix

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/congdoe/aggy/internal/fakecli"
)

const timeout = 10 * time.Second

// buildEnv is the environment before any test changes HOME, so the go
// tool keeps its real build cache.
var buildEnv = os.Environ()

var (
	buildOnce sync.Once
	aggyBin   string
	buildErr  error
	buildOut  []byte
)

// buildAggy builds cmd/aggy once per test binary.
func buildAggy(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		dir, err := os.MkdirTemp("", "aggy-e2e-bin")
		if err != nil {
			buildErr = err
			return
		}
		aggyBin = filepath.Join(dir, "aggy")
		cmd := exec.Command("go", "build", "-o", aggyBin, "./cmd/aggy")
		cmd.Dir = filepath.Join(filepath.Dir(file), "..", "..")
		cmd.Env = append(append([]string{}, buildEnv...), "CGO_ENABLED=0", "GOTOOLCHAIN=local")
		buildOut, buildErr = cmd.CombinedOutput()
	})
	if buildErr != nil {
		t.Fatalf("go build aggy: %v\n%s", buildErr, buildOut)
	}
	return aggyBin
}

// world is a temp HOME with the fakes installed under their vendor names
// on PATH.
type world struct {
	home, bin, cwd, record string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	fakes := fakecli.Build(t)
	// A short dir: a Unix socket path must fit in about 100 bytes.
	home, err := os.MkdirTemp("/tmp", "aggy-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	home, _ = filepath.EvalSymlinks(home)
	w := &world{home: home, bin: filepath.Join(home, "bin"), cwd: filepath.Join(home, "work"), record: filepath.Join(home, "record.jsonl")}
	for _, d := range []string{w.bin, w.cwd} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"claude", "codex", "grok"} {
		b, err := os.ReadFile(fakes.For(v))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(w.bin, v), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

// session is aggy running on the slave side of a test pseudo-terminal;
// the test is the user's terminal on the master side.
type session struct {
	t      *testing.T
	master *os.File
	slave  *os.File
	cmd    *exec.Cmd
	mu     sync.Mutex
	out    bytes.Buffer
	before *term.State
}

func (w *world) start(t *testing.T, scenario map[string]any, args ...string) *session {
	t.Helper()
	sc, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	scPath := filepath.Join(w.home, "scenario.json")
	if err := os.WriteFile(scPath, sc, 0o600); err != nil {
		t.Fatal(err)
	}
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	if err := pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	s := &session{t: t, master: master, slave: slave}
	if s.before, err = term.GetState(int(master.Fd())); err != nil {
		t.Fatal(err)
	}
	s.cmd = exec.Command(buildAggy(t), args...)
	s.cmd.Dir = w.cwd
	s.cmd.Env = []string{
		"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"),
		"PATH=" + w.bin + ":/usr/bin:/bin", "TERM=xterm-256color",
		"USER_OWN_SETTING=kept",
		fakecli.EnvScenario + "=" + scPath, fakecli.EnvRecord + "=" + w.record,
	}
	s.cmd.Stdin, s.cmd.Stdout, s.cmd.Stderr = slave, slave, slave
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s.cmd.ProcessState == nil {
			_ = s.cmd.Process.Kill()
			_ = s.cmd.Wait()
		}
	})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			s.mu.Lock()
			s.out.Write(buf[:n])
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return s
}

// waitFor waits until the terminal has shown want, byte for byte.
func (s *session) waitFor(want []byte) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		ok := bytes.Contains(s.out.Bytes(), want)
		s.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t.Fatalf("terminal never showed %q; it showed %q", want, s.out.Bytes())
}

func (s *session) send(b []byte) {
	s.t.Helper()
	if _, err := s.master.Write(b); err != nil {
		s.t.Fatal(err)
	}
}

// exit waits for aggy to exit and returns its exit code.
func (s *session) exit() int {
	s.t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			s.t.Fatal(err)
		}
	case <-time.After(timeout):
		s.t.Fatal("aggy did not exit")
	}
	return s.cmd.ProcessState.ExitCode()
}

// restored fails unless the terminal is back in the state it had before
// aggy started. It reads the state through the master: darwin revokes the
// slave when aggy, its session leader, exits.
func (s *session) restored(when string) {
	s.t.Helper()
	now, err := term.GetState(int(s.master.Fd()))
	if err != nil {
		s.t.Fatal(err)
	}
	if !reflect.DeepEqual(now, s.before) {
		s.t.Errorf("%s: the terminal was not restored", when)
	}
}

// sample holds what a pass-through must never touch: bracketed paste
// (with a Ctrl+\ inside it), kitty keyboard reports, SGR mouse, OSC 52,
// 8, 9, and 777, truecolor, wide and combining characters, Ctrl+C, and
// bare CR and LF.
var sample = []byte("\x1b[200~pasted \x1c text\r\nline2\x1b[201~" +
	"\x1b[>1u\x1b[97;5u\x1b[13;2u" +
	"\x1b[<0;10;5M\x1b[<0;10;5m" +
	"\x1b]52;c;aGVsbG8=\x07" +
	"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\" +
	"\x1b]9;done\x07\x1b]777;notify;t;b\x1b\\" +
	"\x1b[38;2;255;100;0mtrue\x1b[0m" +
	"漢字🙂é\x03\r\n")

func TestPassThrough(t *testing.T) {
	w := newWorld(t)
	banner := "\x1b[?2004h\x1b[38;2;1;2;3mREADY 漢🙂\x1b]8;;https://x\x1b\\l\x1b]8;;\x1b\\\x1b[0m>"
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": banner, "exit_code": 7}, "run", "codex")
	s.waitFor([]byte(banner))
	s.send(sample)
	s.waitFor(sample)

	if err := pty.Setsize(s.master, &pty.Winsize{Rows: 40, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	s.waitFor([]byte("[winch 40x100]"))

	s.send([]byte{0x04})
	if code := s.exit(); code != 7 {
		t.Errorf("exit code = %d, want the CLI's 7", code)
	}
	s.restored("after exit")

	recs := fakecli.Records(t, w.record)
	var rec *fakecli.Record
	for i := range recs {
		if recs[i].Mode == "interactive" {
			rec = &recs[i]
		}
	}
	if rec == nil {
		t.Fatalf("no interactive run recorded: %+v", recs)
	}
	if !rec.StdinTerminal {
		t.Error("the CLI's stdin was not a terminal")
	}
	if rec.Env["AGGY_VENDOR"] != "codex" {
		t.Errorf("AGGY_VENDOR = %q", rec.Env["AGGY_VENDOR"])
	}
	if !slices.Contains(rec.EnvKeys, "USER_OWN_SETTING") || !slices.Contains(rec.EnvKeys, "TERM") {
		t.Errorf("the user's environment did not reach the CLI: %v", rec.EnvKeys)
	}
	if rec.Cwd != w.cwd {
		t.Errorf("cwd = %q, want %q", rec.Cwd, w.cwd)
	}
}

func TestSuspendResume(t *testing.T) {
	w := newWorld(t)
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", "codex")
	s.waitFor([]byte("READY>"))
	s.send([]byte("before\x1a"))
	s.waitFor([]byte("before"))

	// The fake stops itself with SIGSTOP on Ctrl+Z (a SIGTSTP would be
	// discarded: it leads an orphaned process group). aggy must then stop
	// too, with the terminal restored, so the shell gets it back.
	stopped := make(chan syscall.WaitStatus, 1)
	go func() {
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(s.cmd.Process.Pid, &ws, syscall.WUNTRACED, nil)
		stopped <- ws
	}()
	select {
	case ws := <-stopped:
		if ws&0xff != 0x7f { // WIFSTOPPED; ws.Stopped is false for SIGSTOP on darwin
			t.Fatalf("aggy did not stop: %v", ws)
		}
	case <-time.After(timeout):
		t.Fatal("aggy did not stop after the CLI stopped")
	}
	s.restored("while stopped")

	// fg: aggy resumes the CLI, which reports it, and raw mode is back.
	if err := syscall.Kill(s.cmd.Process.Pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	s.waitFor([]byte("[continued]"))
	s.send([]byte("after\r"))
	s.waitFor([]byte("after\r"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Errorf("exit code = %d", code)
	}
	s.restored("after exit")
}

func TestSignalForwarded(t *testing.T) {
	tests := []struct {
		name string
		sig  syscall.Signal
	}{
		{"SIGTERM", syscall.SIGTERM},
		{"SIGHUP", syscall.SIGHUP},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", "codex")
			s.waitFor([]byte("READY>"))
			if err := s.cmd.Process.Signal(tt.sig); err != nil {
				t.Fatal(err)
			}
			if code := s.exit(); code != 128+int(tt.sig) {
				t.Errorf("exit code = %d, want %d (the CLI died of the signal)", code, 128+int(tt.sig))
			}
			s.restored("after the signal")
		})
	}
}
