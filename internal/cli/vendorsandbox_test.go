package cli

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/trustlist"
)

// sandboxCan makes Claude's sandbox able to start (missing "") or not.
func sandboxCan(t *testing.T, missing string) {
	t.Helper()
	old := claudeSandboxMissing
	claudeSandboxMissing = func() string { return missing }
	t.Cleanup(func() { claudeSandboxMissing = old })
}

// TestClaudeSandboxRow (T6980): the trust list offers Claude's sandbox in a
// folder never answered, unticked (PR #135 review: it costs network in daily
// use, so the person chooses it), saying what it costs, or why it cannot
// start; All does not tick it; a folder answered before the row existed is
// not asked again, keeps it off, and aggy trust shows it new and unticked.
func TestClaudeSandboxRow(t *testing.T) {
	t.Run("a new folder: offered unticked, saying what it costs", func(t *testing.T) {
		_, repo := trustWorld(t, "claude")
		sandboxCan(t, "")
		out, _ := startIn(repo, config.Default(), "1\n\n")
		if !strings.Contains(out, "How a harness runs here") || !strings.Contains(out, "[ ] Claude Code: run its shell commands in Claude's own sandbox") ||
			!strings.Contains(out, "file tools, hooks, and MCP servers stay outside it") || !strings.Contains(out, "npm install will raise Claude's own") ||
			!strings.Contains(out, "Claude still asks before each command") {
			t.Fatalf("the list: %q", out)
		}
		if a, _ := folderAnswer(t, repo); !slices.Equal(a.On, []string{"claude"}) {
			t.Errorf("All or the default ticked the sandbox: %+v", a)
		}
		if on, _ := claudeSandboxHere(repo); on {
			t.Error("on without being ticked")
		}
	})
	t.Run("a new folder: ticked by the person", func(t *testing.T) {
		_, repo := trustWorld(t, "claude")
		sandboxCan(t, "")
		startIn(repo, config.Default(), "3\n\n")
		if on, _ := claudeSandboxHere(repo); !on {
			t.Error("the launch does not see it ticked")
		}
	})
	t.Run("a new folder where it cannot start: unticked, saying why", func(t *testing.T) {
		_, repo := trustWorld(t, "claude")
		sandboxCan(t, "bwrap not found on PATH")
		out, _ := startIn(repo, config.Default(), "\n")
		if !strings.Contains(out, "[ ] Claude Code: run its shell") || !strings.Contains(out, "cannot start on this machine: bwrap not found") {
			t.Fatalf("the list: %q", out)
		}
		if a, _ := folderAnswer(t, repo); slices.Contains(a.On, trustRowClaudeSandbox) {
			t.Errorf("ticked where it cannot start: %+v", a)
		}
	})
	t.Run("not offered without Claude", func(t *testing.T) {
		_, repo := trustWorld(t, "opencode")
		sandboxCan(t, "")
		if out, _ := startIn(repo, config.Default(), "\n"); strings.Contains(out, "Claude's own sandbox") {
			t.Errorf("offered with Claude not installed: %q", out)
		}
	})
	t.Run("answered before: not asked again, off, new in aggy trust", func(t *testing.T) {
		_, repo := trustWorld(t, "claude")
		sandboxCan(t, "")
		tickTrustRows(t, repo, "claude")
		if out, note := startIn(repo, config.Default(), "\n"); out != "" || note != "" {
			t.Fatalf("asked again for the sandbox row: %q %q", out, note)
		}
		if on, _ := claudeSandboxHere(repo); on {
			t.Error("on in a folder that never ticked it")
		}
		tg, err := trustTargetFor(repo)
		if err != nil {
			t.Fatal(err)
		}
		l, err := openTrustList(tg, config.Default(), true)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(l.st.Rows, func(r trustlist.Row) bool { return r.ID == trustRowClaudeSandbox })
		if i < 0 || l.st.On[i] || !l.st.New[i] {
			t.Errorf("aggy trust's list: row %d, on %v, new %v", i, l.st.On, l.st.New)
		}
		if p := l.pending(); slices.Contains(p, trustRowClaudeSandbox) {
			t.Errorf("pending %q", p)
		}
	})
	t.Run("aggy trust ticks it in an answered folder", func(t *testing.T) {
		_, repo := trustWorld(t, "claude")
		sandboxCan(t, "")
		tickTrustRows(t, repo, "claude")
		atKeyboard(t, "6\n\n") // every harness is listed from the shell: Claude, OpenCode, Hermes, Antigravity, then the sandbox
		if code, out, errb := run("trust", repo); code != 0 {
			t.Fatalf("aggy trust: %d %q %q", code, out, errb)
		}
		if on, _ := claudeSandboxHere(repo); !on {
			t.Error("not on after aggy trust ticked it")
		}
	})
}

// TestClaudeSandboxLaunch (T6980): what an on-screen Claude launch does with
// the folder's choice: on where it can start, --no-policy or not (PR #135
// review); ticked where it cannot, asked in place, a yes starts without it and
// a no refuses, and with no one to ask it refuses too, naming the ways
// forward (PR #135 review); not ticked, the person's own settings.
func TestClaudeSandboxLaunch(t *testing.T) {
	cases := []struct {
		name     string
		ticked   bool
		noPolicy bool
		missing  string
		person   bool
		answer   string
		on       bool
		err      error
		line     string
		question bool
	}{
		{name: "ticked, can start", ticked: true, on: true, line: "Claude's sandbox: on for its shell commands"},
		{name: "ticked, can start, --no-policy", ticked: true, noPolicy: true, on: true, line: "still on with --no-policy, since this folder chose it; aggy trust"},
		{name: "not ticked", line: "not chosen for this folder, so your own Claude settings decide; aggy trust"},
		{name: "not ticked, --no-policy", noPolicy: true, line: "as your own Claude settings set it (--no-policy"},
		{name: "not ticked, cannot start", missing: "no bwrap", line: "not chosen for this folder"},
		{name: "ticked, cannot start, yes", ticked: true, missing: "no bwrap", person: true, answer: "y\n", line: "off this launch, as you said: no bwrap", question: true},
		{name: "ticked, cannot start, no", ticked: true, missing: "no bwrap", person: true, answer: "\n", err: errClaudeSandboxDeclined, question: true},
		{name: "ticked, cannot start, no one to ask", ticked: true, missing: "no bwrap", err: errClaudeSandboxNoOne},
		{name: "ticked, cannot start, no one to ask, --no-policy", ticked: true, noPolicy: true, missing: "no bwrap", err: errClaudeSandboxNoOne},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, repo := trustWorld(t, "claude")
			sandboxCan(t, c.missing)
			offerTerminal = func() bool { return c.person }
			ids := []string{"claude"}
			if c.ticked {
				ids = append(ids, trustRowClaudeSandbox)
			}
			tickTrustRows(t, repo, ids...)
			var req adapter.InteractiveRequest
			var out bytes.Buffer
			line, err := claudeSandboxLaunch(&req, repo, c.noPolicy, os.Getenv, strings.NewReader(c.answer), &out)
			if !errors.Is(err, c.err) || err == nil && c.err != nil {
				t.Fatalf("err %v, want %v", err, c.err)
			}
			if err != nil && (!strings.Contains(err.Error(), "aggy trust "+repo) || !strings.Contains(err.Error(), "bubblewrap and socat")) {
				t.Errorf("the refusal names no way forward: %v", err)
			}
			if req.ClaudeSandbox != c.on {
				t.Errorf("ClaudeSandbox = %v, want %v", req.ClaudeSandbox, c.on)
			}
			if !strings.Contains(line, c.line) {
				t.Errorf("line %q lacks %q", line, c.line)
			}
			if asked := strings.Contains(out.String(), "without its sandbox this time? [y/N]"); asked != c.question {
				t.Errorf("asked %v: %q", asked, out.String())
			}
		})
	}
	t.Run("home is no folder for it", func(t *testing.T) {
		h, _ := trustWorld(t, "claude")
		var req adapter.InteractiveRequest
		line, err := claudeSandboxLaunch(&req, h, false, os.Getenv, strings.NewReader(""), &bytes.Buffer{})
		if err != nil || req.ClaudeSandbox || !strings.Contains(line, "does not turn it on in this folder") {
			t.Errorf("%q %v %v", line, req.ClaudeSandbox, err)
		}
	})
}

// TestSandboxDoctor (T6980): aggy doctor's line for Claude's sandbox in the
// folder it runs in, a warning only when chosen and unable to start, with
// what is missing as detail, and Codex's line.
func TestSandboxDoctor(t *testing.T) {
	cases := []struct {
		name    string
		ticked  bool
		missing string
		level   checkup.Level
		msg     string
	}{
		{"on", true, "", checkup.LevelInfo, "on for this folder's sessions"},
		{"chosen, cannot start", true, "no socat", checkup.LevelWarn, "cannot start on this machine: aggy asks at a terminal"},
		{"not chosen", false, "", checkup.LevelInfo, "not chosen for this folder (aggy trust offers it)"},
		{"not chosen, cannot start", false, "no socat", checkup.LevelInfo, "not chosen for this folder, and it cannot start"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, repo := trustWorld(t, "claude")
			sandboxCan(t, c.missing)
			ids := []string{"claude"}
			if c.ticked {
				ids = append(ids, trustRowClaudeSandbox)
			}
			tickTrustRows(t, repo, ids...)
			f := claudeSandboxFinding(repo)
			if f.Level != c.level || !strings.Contains(f.Message, c.msg) || f.Detail != c.missing || f.Code != codeVendorSandbox {
				t.Errorf("finding %+v", f)
			}
		})
	}
	if f := codexSandboxFinding(); !strings.Contains(f.Message, "asks before danger-full-access") || f.Vendor != adapter.Codex {
		t.Errorf("codex %+v", f)
	}
}
