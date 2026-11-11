package store

import (
	"context"
	"testing"
	"time"
)

// TestKeys (T6931): the index keeps a key's name and provider, one key per
// provider, and a replace or delete reads back.
func TestKeys(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	now := time.UnixMilli(1_700_000_000_000).UTC()
	steps := []struct {
		name    string
		put     *Key
		del     string
		wantErr bool
		want    []Key
	}{
		{name: "empty", want: nil},
		{name: "add", put: &Key{"anthropic", "anthropic", now}, want: []Key{{"anthropic", "anthropic", now}}},
		{name: "second", put: &Key{"or", "openrouter", now}, want: []Key{{"anthropic", "anthropic", now}, {"or", "openrouter", now}}},
		{name: "provider taken", put: &Key{"other", "openrouter", now}, wantErr: true, want: []Key{{"anthropic", "anthropic", now}, {"or", "openrouter", now}}},
		{name: "replace", put: &Key{"or", "groq", now}, want: []Key{{"anthropic", "anthropic", now}, {"or", "groq", now}}},
		{name: "no provider", put: &Key{"x", "", now}, wantErr: true, want: []Key{{"anthropic", "anthropic", now}, {"or", "groq", now}}},
		{name: "delete", del: "anthropic", want: []Key{{"or", "groq", now}}},
		{name: "delete none", del: "anthropic", want: []Key{{"or", "groq", now}}},
	}
	for _, st := range steps {
		var err error
		switch {
		case st.put != nil:
			err = s.PutKey(ctx, *st.put)
		case st.del != "":
			err = s.DeleteKey(ctx, st.del)
		}
		if (err != nil) != st.wantErr {
			t.Errorf("%s: err = %v, want error %v", st.name, err, st.wantErr)
		}
		got, err := s.Keys(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(st.want) {
			t.Errorf("%s: keys = %+v, want %+v", st.name, got, st.want)
			continue
		}
		for i := range got {
			if got[i] != st.want[i] {
				t.Errorf("%s: keys = %+v, want %+v", st.name, got, st.want)
			}
		}
	}
	k, ok, err := s.KeyNamed(ctx, "or")
	if err != nil || !ok || k.Provider != "groq" {
		t.Errorf("KeyNamed = %+v %v %v", k, ok, err)
	}
	if _, ok, err := s.KeyNamed(ctx, "none"); ok || err != nil {
		t.Errorf("KeyNamed(none) = %v %v", ok, err)
	}
}
