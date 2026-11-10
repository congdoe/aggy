package store

import (
	"context"
	"testing"
	"time"
)

// TestNotes (T6966): a note reads back as last set, and none reads as absent.
func TestNotes(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	steps := []struct {
		set, want string
		ok        bool
	}{
		{"", "", false},
		{"declined", "declined", true},
		{"accepted", "accepted", true},
	}
	for _, st := range steps {
		if st.set != "" {
			if err := s.SetNote(ctx, "setup_offer", st.set, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		v, ok, err := s.Note(ctx, "setup_offer")
		if err != nil || ok != st.ok || v != st.want {
			t.Errorf("after %q: %q %v %v, want %q %v", st.set, v, ok, err, st.want, st.ok)
		}
	}
	if err := s.SetNote(ctx, "", "x", time.Now()); err == nil {
		t.Error("a note with no name was set")
	}
}
