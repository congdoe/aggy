package config

import "testing"

// TestVersionsSelfTest: the self-test is on by default, and the user's config
// turns it off.
func TestVersionsSelfTest(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"", true},
		{"[versions]\nself_test = true\n", true},
		{"[versions]\nself_test = false\n", false},
	}
	for _, tt := range tests {
		c, err := Parse(tt.text)
		if err != nil {
			t.Fatal(err)
		}
		if c.Versions.SelfTest != tt.want {
			t.Errorf("%q: SelfTest = %v, want %v", tt.text, c.Versions.SelfTest, tt.want)
		}
	}
}
