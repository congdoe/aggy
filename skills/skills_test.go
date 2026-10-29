package skills

import "testing"

// The ceilings the aggy skill must stay under (T6646). The frontmatter is
// paid on every turn of every session that has the skill linked.
const (
	maxAlways = 150
	maxBody   = 1500
)

func TestAggySkillCost(t *testing.T) {
	always, body := Estimate(Aggy)
	t.Logf("aggy skill: about %d tokens every turn, %d on invoke", always, body)
	if always == 0 || always > maxAlways {
		t.Errorf("always-loaded part = %d tokens, want 1..%d", always, maxAlways)
	}
	if body == 0 || body > maxBody {
		t.Errorf("body = %d tokens, want 1..%d", body, maxBody)
	}
}

func TestEstimate(t *testing.T) {
	tests := []struct {
		in           string
		always, body int
	}{
		{"", 0, 0},
		{"no frontmatter here", 0, 5},
		{"---\nname: a\n---\nbody", 4, 1},
		{"---\nunclosed\nbody", 0, 5},
	}
	for _, tt := range tests {
		a, b := Estimate([]byte(tt.in))
		if a != tt.always || b != tt.body {
			t.Errorf("Estimate(%q) = %d, %d, want %d, %d", tt.in, a, b, tt.always, tt.body)
		}
	}
}

func TestFront(t *testing.T) {
	tests := []struct{ in, name, desc string }{
		{"no frontmatter", "", ""},
		{"---\nname: demo\ndescription: \"Does a thing\"\nmeta:\n  name: inner\n---\nbody", "demo", "Does a thing"},
		{"---\nname: 'q'\n---\n", "q", ""},
		{"---\r\nname: crlf\r\ndescription: d\r\n---\r\nbody", "crlf", "d"},
		{"---\nname: b\ndescription: >-\n  folded\n---\n", "b", ""},
		{"---\nname: b\ndescription: |\n  literal\n---\n", "b", ""},
	}
	for _, tt := range tests {
		if n, d := Front([]byte(tt.in)); n != tt.name || d != tt.desc {
			t.Errorf("Front(%q) = %q, %q, want %q, %q", tt.in, n, d, tt.name, tt.desc)
		}
	}
}
