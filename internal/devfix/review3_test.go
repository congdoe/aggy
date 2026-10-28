package devfix

import (
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// Regression tests for T6732. Each fails on 45fa3fa.

// TestSessionIDsNoUUIDFallback: without the vendor's structured session
// id, no id is taken from the output, even when it holds one UUID, which
// may be another session's.
func TestSessionIDsNoUUIDFallback(t *testing.T) {
	const id = "0f8a3c1e-5b2d-4e6f-9a7b-1c2d3e4f5a6b"
	const other = "11111111-2222-4333-8444-555555555555"
	tests := []struct {
		name   string
		vendor adapter.Vendor
		stdout string
		stderr string
		want   []string
	}{
		{"codex structured", adapter.Codex, `{"type":"thread.started","thread_id":"` + id + `"}` + "\n", "", []string{id}},
		{"one UUID in prose", adapter.Codex, "see session " + other + "\n", "", nil},
		{"one UUID on stderr", adapter.Claude, "", "resuming " + other + "\n", nil},
		{"one UUID, grok", adapter.Grok, "done, " + other + "\n", "", nil},
		{"no UUID", adapter.Claude, "hello\n", "", nil},
		{"two UUIDs", adapter.Codex, id + " " + other + "\n", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := sessionIDs(tt.vendor, adapter.Output{Stdout: []byte(tt.stdout), Stderr: []byte(tt.stderr)})
			if !slices.Equal(got, tt.want) {
				t.Errorf("sessionIDs = %v (%s), want %v", got, why, tt.want)
			}
			if got == nil && why == "" {
				t.Error("no id and no reason")
			}
		})
	}
}

// TestUserNamePathForms: the user name is found and scrubbed as ~user and
// /Volumes/<user>, as well as user@host; prose and generic names are not.
func TestUserNamePathForms(t *testing.T) {
	id := Identity{User: "annb"}
	tests := []struct {
		line, want string
		found      bool
	}{
		{"cd ~annb/proj", "cd ~fixture-user/proj", true},
		{"~annb", "~fixture-user", true},
		{"%7Eannb%2Fproj", "%7Efixture-user%2Fproj", true},
		{`"dir":"~ANNB"`, `"dir":"~fixture-user"`, true},
		{"/Volumes/annb/src", "/Volumes/fixture-user/src", true},
		{"/Volumes/annb", "/Volumes/fixture-user", true},
		{"%2FVolumes%2Fannb%2Fsrc", "%2FVolumes%2Ffixture-user%2Fsrc", true},
		{"%252FVolumes%252Fannb", "%252FVolumes%252Ffixture-user", true},
		{`"\/Volumes\/annb\/x"`, `"\/Volumes\/fixture-user\/x"`, true},
		{"annb@laptop:~$", "fixture-user@laptop:~$", true},
		{"~annb ~annb", "~fixture-user ~fixture-user", true},
		{"see ~annb.", "see ~fixture-user.", true},
		{"/Volumes/annb.", "/Volumes/fixture-user.", true},
		{"(~annb.)", "(~fixture-user.)", true},
		{"~annb...", "~fixture-user...", true},
		{"~annb.doe/x", "~annb.doe/x", false}, // another, dotted name
		{"/Volumes/annb.backup", "/Volumes/annb.backup", false},
		// Not the user name: a longer name, another volume, prose.
		{"~annbx/proj", "~annbx/proj", false},
		{"x~annb", "x~annb", false},
		{"/Volumes/annbackup", "/Volumes/annbackup", false},
		{"/Volumes/Data/annb", "/Volumes/Data/annb", false},
		{"annb wrote this", "annb wrote this", false},
		{"~/annb", "~/annb", false},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			found := slices.Contains(kinds(DetectText("f", tt.line, id, nil)), KindUser)
			if found != tt.found {
				t.Errorf("found = %v, want %v", found, tt.found)
			}
			if got := NewScrubber(id).String(tt.line); got != tt.want {
				t.Errorf("scrub = %q, want %q", got, tt.want)
			}
		})
	}
	// Generic names stay unmatched in every form.
	for _, u := range genericNames {
		for _, line := range []string{"~" + u + "/x", "/Volumes/" + u + "/x", u + "@ci"} {
			if slices.Contains(kinds(DetectText("f", line, Identity{User: u}, nil)), KindUser) {
				t.Errorf("generic %q flagged in %q", u, line)
			}
		}
	}
}
