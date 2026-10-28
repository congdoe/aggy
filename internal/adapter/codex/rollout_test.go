package codex

import (
	"testing"
	"time"
)

func TestRolloutThreadID(t *testing.T) {
	const id = "5973b6c0-94b8-487b-a530-2aeb6098ae0e"
	tests := []struct {
		name string
		want string
		ok   bool
	}{
		{"rollout-2025-05-07T17-24-21-" + id + ".jsonl", id, true}, // recorder.rs doc comment
		{"rollout-" + id + ".jsonl", "", false},
		{"rollout-2025-05-07T17-24-21-" + id + ".json", "", false},
		{"other-2025-05-07T17-24-21-" + id + ".jsonl", "", false},
		{"rollout-2025-05-07T17-24-21-5973b6c0-94b8-487b-a530-2aeb6098ae0Z.jsonl", "", false},
	}
	for _, tt := range tests {
		if got, ok := RolloutThreadID(tt.name); got != tt.want && tt.ok || ok != tt.ok {
			t.Errorf("%s: %q %v", tt.name, got, ok)
		}
	}
}

func TestRolloutDirs(t *testing.T) {
	start := time.Date(2026, 10, 28, 23, 59, 0, 0, time.UTC)
	dirs := RolloutDirs("/s", start, start.Add(2*time.Minute))
	want := map[string]bool{"/s/2026/10/27": true, "/s/2026/10/28": true, "/s/2026/10/29": true, "/s/2026/10/30": true}
	for _, d := range dirs {
		delete(want, d)
	}
	if len(want) > 0 {
		t.Errorf("missing %v in %v", want, dirs)
	}
}
