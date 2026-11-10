package adapter

import (
	"slices"
	"strings"
	"testing"
)

func TestValidResumeName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"auth-refactor", true},
		{"My Project", true},
		{"café 2", true},
		{"", false},
		{"   ", false},
		{"-r", false},
		{"--dangerously-skip-permissions", false},
		{"../x", false},
		{"/home/me/.claude/projects/p/s.jsonl", false},
		{`a\b`, false},
		{".hidden", false},
		{"~me", false},
		{"a\x1bb", false},
		{"a\u202eb", false},
		{"bad\xff", false},
		{strings.Repeat("n", MaxResumeName), true},
		{strings.Repeat("n", MaxResumeName+1), false},
	}
	for _, tt := range tests {
		if err := ValidResumeName(tt.name); (err == nil) != tt.ok {
			t.Errorf("ValidResumeName(%q) = %v, want ok %v", tt.name, err, tt.ok)
		}
	}
}

func TestResumeNameArgs(t *testing.T) {
	tests := []struct {
		req  InteractiveRequest
		want []string
	}{
		{InteractiveRequest{ResumeName: "auth"}, []string{"--resume", "auth"}},
		{InteractiveRequest{ResumeName: "auth", SessionID: "x"}, nil},
		{InteractiveRequest{ResumeName: "auth", Resume: "x"}, nil},
		{InteractiveRequest{ResumeName: "auth", Fork: true}, nil},
		{InteractiveRequest{ResumeName: "-x"}, nil},
	}
	for _, tt := range tests {
		got, err := ResumeNameArgs(tt.req)
		if (err == nil) != (tt.want != nil) || !slices.Equal(got, tt.want) {
			t.Errorf("ResumeNameArgs(%+v) = %q, %v; want %q", tt.req, got, err, tt.want)
		}
	}
}
