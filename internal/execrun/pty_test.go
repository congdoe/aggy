//go:build unix

package execrun

import (
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestPTYEnv(t *testing.T) {
	base := []string{"HOME=/h", "TERM=xterm", "ANTHROPIC_BASE_URL=user-set", "AGGY_VENDOR=old"}
	tests := []struct {
		name    string
		extra   []string
		want    []string
		wantErr string
	}{
		{"user env kept, AGGY_ replaced", []string{"AGGY_VENDOR=codex"},
			[]string{"HOME=/h", "TERM=xterm", "ANTHROPIC_BASE_URL=user-set", "AGGY_VENDOR=codex"}, ""},
		{"no extra", nil, base, ""},
		{"base url refused", []string{"OPENAI_BASE_URL=x"}, nil, "only AGGY_"},
		{"api key refused", []string{"ANTHROPIC_API_KEY=x"}, nil, "only AGGY_"},
		{"lowercase aggy refused", []string{"aggy_x=1"}, nil, "only AGGY_"},
		{"no equals", []string{"AGGY_X"}, nil, "bad environment entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PTYEnv(base, tt.extra)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tt.want) {
				t.Errorf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestStartPTYRefuses(t *testing.T) {
	tests := []struct {
		name string
		c    adapter.Command
	}{
		{"relative path", adapter.Command{Path: "codex", Dir: "/", Stdin: adapter.StdinTerminal}},
		{"unclean path", adapter.Command{Path: "/bin/../bin/sh", Dir: "/", Stdin: adapter.StdinTerminal}},
		{"not interactive", adapter.Command{Path: "/bin/sh", Dir: "/"}},
		{"relative dir", adapter.Command{Path: "/bin/sh", Dir: "x", Stdin: adapter.StdinTerminal}},
		{"bad env", adapter.Command{Path: "/bin/sh", Dir: "/", Stdin: adapter.StdinTerminal, Env: []string{"XAI_API_KEY=k"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if p, err := StartPTY(tt.c, 24, 80); err == nil {
				_ = p.Close()
				t.Fatal("started")
			}
		})
	}
}
