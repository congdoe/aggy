package adapter

import (
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.70", "1.0.70", 0},
		{"1.0.70", "1.0.9", 1},
		{"0.164.0", "0.165.0", -1},
		{"v2.1.311", "2.1.311", 0},
		{"1.0.70-beta.1", "1.0.70", -1},
		{"1.0.70+build.5", "1.0.70", 0},
		{"10.0.0", "9.99.99", 1},
	}
	for _, tt := range tests {
		if got, err := CompareVersions(tt.a, tt.b); err != nil || got != tt.want {
			t.Errorf("CompareVersions(%s, %s) = %d, %v, want %d", tt.a, tt.b, got, err, tt.want)
		}
	}
	if _, err := CompareVersions("1.0", "1.0.0"); err == nil {
		t.Error("1.0 accepted")
	}
}

func TestVersionWarning(t *testing.T) {
	var a Adapter = stub{} // tested on 1.0.70
	tests := []struct {
		version, want string
	}{
		{"1.0.70", ""},
		{"1.0.71", "outside the versions"},
		{"1.0.69", "outside the versions"},
		{"1.0.70-rc.1", "outside the versions"},
		{"weird", "cannot read"},
	}
	for _, tt := range tests {
		got := VersionWarning(a, tt.version)
		if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
			t.Errorf("VersionWarning(%q) = %q, want %q", tt.version, got, tt.want)
		}
	}
	r := VersionRange{Min: "1.0.0", Max: "1.2.0"}
	if ok, _ := r.Contains("1.1.5"); !ok {
		t.Error("1.1.5 not in 1.0.0 to 1.2.0")
	}
}
