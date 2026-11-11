package parity

import (
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/permissions"
)

func TestHermesDeny(t *testing.T) {
	d := func(text string, k permissions.Kind) PolicyDecision {
		return PolicyDecision{PolicyRule: PolicyRule{Group: "g", Text: text}, Kind: k}
	}
	got := HermesDeny([]PolicyDecision{
		d("Bash(rm -rf*)", permissions.Deny),
		d("Bash(git push --force*main*)", permissions.Deny),
		d("Bash(Git Reset --hard)", permissions.Deny),
		d("Bash(rm:*)", permissions.Deny),
		d("Bash(ls [a]?)", permissions.Deny),
		d("Bash(curl *)", permissions.Ask),
		d("Read(**/.env)", permissions.Deny),
	})
	want := []string{"*rm -rf*", "*git push --force*main*", "*git reset --hard*", "*rm*", "*ls [[]a][?]*"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}
