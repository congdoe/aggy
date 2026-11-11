//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Hermes runs its hooks in its gateway daemon, which launchd (or the init
// process) parents, not the CLI: live, a hook's cwd was the install folder,
// and the daemon passes no AGGY_* variable. fake-hermes does the same (#134
// review): it starts each hook from a detached process, in a session of its
// own, whose parent exits at once, so the hook descends from neither the
// fake CLI nor aggy. aggy's socket then sees no lineage, and only the launch
// token can carry a hook's word: an e2e test fails if that path breaks.
//
// The detached process runs in two stages: stage a (setsid) starts stage b
// and exits; stage b waits until it is reparented, runs the hook with the
// daemon's environment, and writes its output and exit code into the spawn
// folder, the code last, for the fake CLI to read.
const (
	hermesSpawnDir   = "AGGY_FAKE_HERMES_SPAWN"
	hermesSpawnStage = "AGGY_FAKE_HERMES_STAGE"
	hermesSpawnFrom  = "AGGY_FAKE_HERMES_FROM"
)

// hermesSpawnJob is what the fake CLI hands the detached process.
type hermesSpawnJob struct {
	Words   []string `json:"words"`
	Timeout int      `json:"timeout"`
	Env     []string `json:"env"`
	Dir     string   `json:"dir"`
}

// hermesSpawned runs a stage of the detached process, if this is one.
func hermesSpawned() (int, bool) {
	dir := os.Getenv(hermesSpawnDir)
	if dir == "" {
		return 0, false
	}
	switch os.Getenv(hermesSpawnStage) {
	case "a":
		self, err := os.Executable()
		if err != nil {
			return 1, true
		}
		b := exec.Command(self)
		b.Env = append(spawnEnv(), hermesSpawnDir+"="+dir, hermesSpawnStage+"=b", hermesSpawnFrom+"="+strconv.Itoa(os.Getpid()))
		if err := b.Start(); err != nil {
			return 1, true
		}
		return 0, true // exits at once: b is reparented
	case "b":
		return hermesSpawnRun(dir), true
	}
	return 1, true
}

// spawnEnv is the environment without the spawn variables.
func spawnEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "AGGY_FAKE_HERMES_SPAWN=") && !strings.HasPrefix(kv, "AGGY_FAKE_HERMES_STAGE=") && !strings.HasPrefix(kv, "AGGY_FAKE_HERMES_FROM=") {
			out = append(out, kv)
		}
	}
	return out
}

func hermesSpawnRun(dir string) int {
	from, _ := strconv.Atoi(os.Getenv(hermesSpawnFrom))
	for i := 0; i < 500 && os.Getppid() == from; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	var job hermesSpawnJob
	b, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil || json.Unmarshal(b, &job) != nil || len(job.Words) == 0 {
		return 1
	}
	// Each file goes in whole, by rename, so the CLI never reads half of one.
	write := func(name, s string) {
		p := filepath.Join(dir, name)
		if os.WriteFile(p+".tmp", []byte(s), 0o600) == nil {
			_ = os.Rename(p+".tmp", p)
		}
	}
	if os.Getppid() == from {
		write("code", "spawn: not reparented")
		return 1
	}
	in, _ := os.ReadFile(filepath.Join(dir, "in"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(job.Timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, job.Words[0], job.Words[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin = job.Dir, job.Env, strings.NewReader(string(in))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	write("out", stdout.String())
	write("err", stderr.String())
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		write("code", "timeout")
	case errors.As(err, &ee):
		write("code", strconv.Itoa(ee.ExitCode()))
	case err != nil:
		write("code", "start: "+err.Error())
	default:
		write("code", "0")
	}
	return 0
}

// runDetached runs a hook's words as Hermes' daemon does, detached from this
// process: stdin is payload; it returns stdout, stderr, the exit code, and a
// non-empty failure for a hook that did not run or timed out.
func runDetached(words []string, timeout int, env []string, dir string, payload []byte) (stdout, stderr string, code int, failure string) {
	spawn, err := os.MkdirTemp("", "fake-hermes-hook")
	if err != nil {
		return "", "", 0, err.Error()
	}
	defer func() { _ = os.RemoveAll(spawn) }()
	job, _ := json.Marshal(hermesSpawnJob{Words: words, Timeout: timeout, Env: env, Dir: dir})
	if err := os.WriteFile(filepath.Join(spawn, "job.json"), job, 0o600); err != nil {
		return "", "", 0, err.Error()
	}
	if err := os.WriteFile(filepath.Join(spawn, "in"), payload, 0o600); err != nil {
		return "", "", 0, err.Error()
	}
	self, err := os.Executable()
	if err != nil {
		return "", "", 0, err.Error()
	}
	a := exec.Command(self)
	a.Dir = dir
	a.Env = append(env, hermesSpawnDir+"="+spawn, hermesSpawnStage+"=a")
	a.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := a.Run(); err != nil {
		return "", "", 0, "spawn: " + err.Error()
	}
	deadline := time.Now().Add(time.Duration(timeout)*time.Second + 5*time.Second)
	for time.Now().Before(deadline) {
		c, err := os.ReadFile(filepath.Join(spawn, "code"))
		if err == nil {
			o, _ := os.ReadFile(filepath.Join(spawn, "out"))
			e, _ := os.ReadFile(filepath.Join(spawn, "err"))
			s := string(c)
			if n, err := strconv.Atoi(s); err == nil {
				return string(o), string(e), n, ""
			}
			if s == "timeout" {
				return string(o), string(e), 0, "timed out after " + strconv.Itoa(timeout) + "s"
			}
			return string(o), string(e), 0, s
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", "", 0, "timed out after " + strconv.Itoa(timeout) + "s"
}
