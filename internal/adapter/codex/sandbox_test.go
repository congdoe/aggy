package codex

import (
	"errors"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestSandboxFlagMatrix (T6980): a sandbox level as tight as Codex's default
// or tighter passes unasked, danger-full-access is a loosening flag (asked in
// place), and any other value is refused outright, in every spelling the
// parser knows; none slips through unasked.
func TestSandboxFlagMatrix(t *testing.T) {
	type want int
	const (
		pass want = iota
		asks
		refused
	)
	p := Adapter{}.PassFlags()
	cases := []struct {
		name string
		args []string
		want want
	}{
		{"--sandbox read-only", []string{"--sandbox", "read-only"}, pass},
		{"--sandbox=read-only", []string{"--sandbox=read-only"}, pass},
		{"-s read-only", []string{"-s", "read-only"}, pass},
		{"-sread-only", []string{"-sread-only"}, pass},
		{"-s=workspace-write", []string{"-s=workspace-write"}, pass},
		{"--sandbox workspace-write", []string{"--sandbox", "workspace-write"}, pass},
		{"-c sandbox_mode=read-only", []string{"-c", "sandbox_mode=read-only"}, pass},
		{"-c quoted read-only", []string{"-c", `sandbox_mode="read-only"`}, pass},
		{"--config single-quoted workspace-write", []string{"--config", `sandbox_mode = 'workspace-write'`}, pass},
		{"--config=sandbox_mode=read-only", []string{"--config=sandbox_mode=read-only"}, pass},
		{"-csandbox_mode=read-only", []string{"-csandbox_mode=read-only"}, pass},

		{"--sandbox danger-full-access", []string{"--sandbox", "danger-full-access"}, asks},
		{"--sandbox=danger-full-access", []string{"--sandbox=danger-full-access"}, asks},
		{"-s danger-full-access", []string{"-s", "danger-full-access"}, asks},
		{"-sdanger-full-access", []string{"-sdanger-full-access"}, asks},
		{"-c sandbox_mode=danger-full-access", []string{"-c", "sandbox_mode=danger-full-access"}, asks},
		{"-c quoted key and value", []string{"--config", `"sandbox_mode"="danger-full-access"`}, asks},
		{"--yolo", []string{"--yolo"}, asks},
		{"--dangerously-bypass-approvals-and-sandbox", []string{"--dangerously-bypass-approvals-and-sandbox"}, asks},
		{"--approve-for-me", []string{"--approve-for-me"}, asks},
		{"-c a workspace-write table", []string{"-c", "sandbox_workspace_write.network_access=true"}, asks},
		{"-c a profile's level", []string{"-c", `profiles.x.sandbox_mode="danger-full-access"`}, asks},

		{"--sandbox an unknown level", []string{"--sandbox", "off"}, refused},
		{"-s an unknown level", []string{"-s", "strict"}, refused},
		{"-c an unknown level", []string{"-c", "sandbox_mode=none"}, refused},
		{"-c sandbox_mode with no value", []string{"-c", "sandbox_mode"}, refused},
		{"-s run on after another short flag", []string{"-hs", "danger-full-access"}, refused},
		{"-s run on, its value attached", []string{"-hsdanger-full-access"}, refused},
		{"--sandbox with no value", []string{"--sandbox"}, refused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := p.Parse(c.args, false)
			_, loosens := adapter.LoosenedFlag(err)
			switch {
			case c.want == pass && err != nil:
				t.Errorf("refused: %v", err)
			case c.want == asks && !loosens:
				t.Errorf("not asked as a loosening flag: %v", err)
			case c.want == refused && (err == nil || loosens):
				t.Errorf("not refused outright: %v", err)
			}
			if c.want == asks {
				// --no-policy lets it through, named.
				if r, err := p.Parse(c.args, true); err != nil || len(r.Loosens) == 0 {
					t.Errorf("--no-policy: %+v %v", r, err)
				}
			}
			if c.want == refused {
				if _, err := p.Parse(c.args, true); err == nil {
					t.Error("--no-policy let an unknown level through")
				}
			}
		})
	}
	if _, err := p.Parse([]string{"--sandbox", "off"}, false); !errors.Is(err, ErrSandboxValue) {
		t.Errorf("an unknown level: %v", err)
	}
}

// TestSandboxLevel (T6980): the level a launch line names, from the words the
// person gave, the last one winning, and Codex's own default with none.
func TestSandboxLevel(t *testing.T) {
	cases := []struct {
		name      string
		extra     []string
		level     string
		from      string
		lineWords string
	}{
		{"none", nil, "", "", "Codex's own default"},
		{"--sandbox", []string{"--sandbox", "read-only"}, SandboxReadOnly, "--sandbox", "read-only (from --sandbox)"},
		{"-s=", []string{"-s=workspace-write"}, SandboxWorkspaceWrite, "--sandbox", "workspace-write"},
		{"-sread-only", []string{"-sread-only"}, SandboxReadOnly, "--sandbox", "read-only"},
		{"-c quoted", []string{"-c", `sandbox_mode="read-only"`}, SandboxReadOnly, "-c sandbox_mode", "read-only (from -c sandbox_mode)"},
		{"the last -c wins", []string{"-c", "sandbox_mode=read-only", "--config=sandbox_mode=danger-full-access"}, SandboxFullAccess, "-c sandbox_mode", "off (danger-full-access"},
		{"--yolo", []string{"--yolo"}, SandboxFullAccess, "--yolo", "full access"},
		{"--approve-for-me", []string{"--approve-for-me"}, SandboxWorkspaceWrite, "--approve-for-me", "workspace-write"},
		{"a profile", []string{"--profile", "work"}, "", "--profile", "aggy does not read the profile"},
		{"a model value is no flag", []string{"-m", "-s", "x"}, "", "", "Codex's own default"},
		{"after -- is prompt", []string{"--", "--sandbox", "danger-full-access"}, "", "", "Codex's own default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			level, from := SandboxLevel(c.extra)
			if level != c.level || from != c.from {
				t.Errorf("SandboxLevel = %q %q, want %q %q", level, from, c.level, c.from)
			}
			if line := SandboxLine(level, from); !strings.Contains(line, c.lineWords) {
				t.Errorf("line %q lacks %q", line, c.lineWords)
			}
		})
	}
}
