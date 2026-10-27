package home

import (
	"path/filepath"
	"testing"
)

func TestDir(t *testing.T) {
	h := t.TempDir()
	override := t.TempDir()
	tests := []struct {
		name, aggyHome, want string
	}{
		{"default", "", filepath.Join(h, ".aggy")},
		{"override", override, override},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", h)
			t.Setenv(Env, tt.aggyHome)
			got, err := Dir()
			if err != nil || got != tt.want {
				t.Errorf("Dir() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}
