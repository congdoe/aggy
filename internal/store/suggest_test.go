package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSuggest(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "aggy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	now := time.Now()

	f, err := s.SuggestFileState(ctx, "/f")
	if err != nil || f.Offset != 0 || f.Pending != "[]" {
		t.Fatalf("%+v, %v", f, err)
	}
	f.Offset, f.Mode = 10, "auto"
	saved, err := s.SaveSuggest(ctx, 0, f, []SuggestCount{{"/r", "git status", 2}, {"/r", "ls", 1}}, now)
	if err != nil || !saved {
		t.Fatalf("%v, %v", saved, err)
	}
	// A second aggy read the same stretch: nothing is added.
	if saved, err = s.SaveSuggest(ctx, 0, f, []SuggestCount{{"/r", "git status", 2}}, now); err != nil || saved {
		t.Fatalf("double count: %v, %v", saved, err)
	}
	got, err := s.SuggestFileState(ctx, "/f")
	if err != nil || got.Offset != 10 || got.Mode != "auto" {
		t.Fatalf("%+v, %v", got, err)
	}
	f.Offset = 20
	if saved, err = s.SaveSuggest(ctx, 10, f, []SuggestCount{{"/r", "git status", 1}}, now); err != nil || !saved {
		t.Fatalf("%v, %v", saved, err)
	}

	c, err := s.SuggestCandidates(ctx, "/r", 3)
	if err != nil || len(c) != 1 || c[0].Command != "git status" || c[0].Count != 3 {
		t.Fatalf("%+v, %v", c, err)
	}
	if n, err := s.AnnounceSuggest(ctx, "/r", 3); err != nil || n != 1 {
		t.Errorf("announce: %d, %v", n, err)
	}
	if n, err := s.AnnounceSuggest(ctx, "/r", 3); err != nil || n != 0 {
		t.Errorf("announce again: %d, %v", n, err)
	}
	if err := s.DismissSuggest(ctx, "/r", "git status"); err != nil {
		t.Fatal(err)
	}
	if c, err = s.SuggestCandidates(ctx, "", 1); err != nil || len(c) != 1 || c[0].Command != "ls" {
		t.Errorf("after dismissing: %+v, %v", c, err)
	}
}

func TestSuggestBound(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "aggy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	counts := make([]SuggestCount, maxSuggestPerRepo+3)
	for i := range counts {
		counts[i] = SuggestCount{"/r", "c" + string(rune('a'+i%26)) + time.Duration(i).String(), 3}
	}
	if _, err := s.SaveSuggest(ctx, 0, SuggestFile{Path: "/f", Offset: 1, Pending: "[]"}, counts, time.Now()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM suggest_commands`).Scan(&n); err != nil || n != maxSuggestPerRepo {
		t.Errorf("%d rows, %v", n, err)
	}
}
