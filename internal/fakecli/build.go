// Package fakecli builds the fake vendor CLIs in testdata/fakes for tests:
// fake-claude, fake-codex, fake-grok, and fake-hook. Only tests import it.
// The scenario format is documented in testdata/fakes/README.md.
package fakecli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

// Env variables the fakes read.
const (
	// EnvScenario is the path of the scenario JSON file.
	EnvScenario = "AGGY_FAKE_SCENARIO"
	// EnvRecord is a file each run appends one JSON line to.
	EnvRecord = "AGGY_FAKE_RECORD"
	// EnvVendor overrides the vendor taken from the binary's name.
	EnvVendor = "AGGY_FAKE_VENDOR"
)

// Fakes holds the paths of the built binaries.
type Fakes struct {
	Dir    string // the directory holding them all
	Claude string
	Codex  string
	Grok   string
	Hook   string
}

// For returns the fake for vendor ("claude", "codex", "grok", or "hook").
func (f Fakes) For(vendor string) string {
	return filepath.Join(f.Dir, "fake-"+vendor)
}

// buildEnv is the environment at process start, before any test changes
// HOME, so the go tool keeps using its real build cache and never runs
// with a test's temp HOME.
var buildEnv = os.Environ()

// Build compiles the fakes into a temp dir for t. It runs the go tool, never
// a vendor CLI.
func Build(t testing.TB) Fakes {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("fakecli: cannot locate the source tree")
	}
	src := filepath.Join(filepath.Dir(file), "..", "..", "testdata", "fakes", "fake")
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-claude")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = src
	cmd.Env = append(append([]string{}, buildEnv...), "CGO_ENABLED=0", "GOTOOLCHAIN=local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fakecli: go build: %v\n%s", err, out)
	}
	data, err := safefs.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"codex", "grok", "hook"} {
		if err := safefs.WriteFile(filepath.Join(dir, "fake-"+v), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := Fakes{Dir: dir}
	f.Claude, f.Codex, f.Grok, f.Hook = f.For("claude"), f.For("codex"), f.For("grok"), f.For("hook")
	return f
}

// WriteScenario writes a scenario JSON document to a temp file and returns
// its path.
func WriteScenario(t testing.TB, json string) string {
	t.Helper()
	if strings.TrimSpace(json) == "" {
		t.Fatal(errors.New("fakecli: empty scenario"))
	}
	p := filepath.Join(t.TempDir(), "scenario.json")
	if err := safefs.WriteFile(p, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Record is one run as a fake recorded it in the EnvRecord file. Env holds
// the AGGY_* variables; EnvKeys names every variable the run saw.
type Record struct {
	Vendor        string              `json:"vendor"`
	Mode          string              `json:"mode"` // version, help, headless, interactive, error
	Args          []string            `json:"args"`
	Cwd           string              `json:"cwd"`
	Env           map[string]string   `json:"env"`
	EnvKeys       []string            `json:"env_keys"`
	StdinTerminal bool                `json:"stdin_terminal"`
	Stdin         string              `json:"stdin"`
	SessionID     string              `json:"session_id"`
	Flags         map[string][]string `json:"flags"`
	Positional    []string            `json:"positional"`
}

// Records reads the runs recorded at path, oldest first. A missing file
// means no runs.
func Records(t testing.TB, path string) []Record {
	t.Helper()
	b, err := safefs.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []Record
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("fakecli: record %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}
