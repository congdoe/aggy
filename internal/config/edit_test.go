package config

import (
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestSetValue(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		s         Setting
		value     string
		want      string
		err       bool
	}{
		{name: "new top level, empty", s: Setting{Key: "default_vendor"}, value: `"claude"`, want: "default_vendor = \"claude\"\n"},
		{name: "new top level goes before tables", src: "# mine\n[switch]\nkey = \"k\"\n", s: Setting{Key: "default_vendor"}, value: `"claude"`,
			want: "# mine\ndefault_vendor = \"claude\"\n\n[switch]\nkey = \"k\"\n"},
		{name: "replace keeps comments", src: "# a\ndefault_vendor = \"codex\" # old\n# b\n", s: Setting{Key: "default_vendor"}, value: `"grok"`,
			want: "# a\ndefault_vendor = \"grok\"\n# b\n"},
		{name: "remove", src: "default_vendor = \"codex\"\n# b\n", s: Setting{Key: "default_vendor"}, want: "# b\n"},
		{name: "remove what is not there", src: "# b\n", s: Setting{Key: "default_vendor"}, want: "# b\n"},
		{name: "new table", src: "# a\n", s: Setting{"vendors.claude", "model"}, value: `"m"`, want: "# a\n\n[vendors.claude]\nmodel = \"m\"\n"},
		{name: "no final newline", src: "# a", s: Setting{"permissions", "mode"}, value: `"auto"`, want: "# a\n\n[permissions]\nmode = \"auto\"\n"},
		{name: "existing table", src: "[vendors.claude]\nmodel = \"m\"\n\n[switch]\nkey = \"k\"\n", s: Setting{"vendors.claude", "off"}, value: "true",
			want: "[vendors.claude]\noff = true\nmodel = \"m\"\n\n[switch]\nkey = \"k\"\n"},
		{name: "same key in another table", src: "[vendors.codex]\nmodel = \"x\"\n", s: Setting{"vendors.claude", "model"}, value: `"m"`,
			want: "[vendors.codex]\nmodel = \"x\"\n\n[vendors.claude]\nmodel = \"m\"\n"},
		{name: "written inline", src: "vendors = { claude = { model = \"x\" } }\n", s: Setting{"vendors.claude", "model"}, value: `"m"`, err: true},
		{name: "bad file", src: "= nope", s: Setting{Key: "default_vendor"}, value: `"m"`, err: true},
		{name: "bad key", s: Setting{Key: "a b"}, value: "1", err: true},
		{name: "multi-line value holds a line like the key", src: "x = \"\"\"\ndefault_vendor = 1\n\"\"\"\n", s: Setting{Key: "default_vendor"}, value: `"m"`, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SetValue([]byte(tc.src), tc.s, tc.value)
			if (err != nil) != tc.err {
				t.Fatalf("err %v, got %q", err, got)
			}
			if !tc.err && string(got) != tc.want {
				t.Errorf("got %q", got)
			}
		})
	}
}

func TestCheckFile(t *testing.T) {
	if got := CheckFile("default_vendor = \"claude\"\n"); len(got) != 0 {
		t.Errorf("%+v", got)
	}
	got := CheckFile("# a\nbad line\n")
	if len(got) != 1 || got[0].Line != 2 {
		t.Errorf("syntax: %+v", got)
	}
	got = CheckFile("# a\n\n[switch]\nkey = \"ctrl+\\\\\"\nnope = 1\n")
	if len(got) != 1 || got[0].Line != 5 || !strings.Contains(got[0].Msg, "nope") {
		t.Errorf("unknown key: %+v", got)
	}
	got = CheckFile("[permissions]\nmode = \"bypassPermissions\"\n")
	if len(got) != 1 || !strings.Contains(got[0].Msg, "permissions.mode") {
		t.Errorf("bad mode: %+v", got)
	}
}

func TestVendorOff(t *testing.T) {
	cfg, err := Parse("default_vendor = \"grok\"\n[vendors.grok]\noff = true\n")
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(cfg, RepoConfig{})
	if e.Allows(adapter.Grok) || !e.Allows(adapter.Claude) {
		t.Errorf("allowed: %v", e.Allowed)
	}
	if e.DefaultVendor != "" {
		t.Errorf("default %s", e.DefaultVendor)
	}
	for _, v := range e.Switch.Order {
		if v == adapter.Grok {
			t.Error("still in the switch order")
		}
	}
	for _, v := range e.Routing.For(adapter.News).Vendors {
		if v == adapter.Grok {
			t.Error("still on a route")
		}
	}
	// A repo cannot turn it back on.
	e = Merge(cfg, RepoConfig{AllowedVendors: []adapter.Vendor{adapter.Grok}})
	if e.Allows(adapter.Grok) {
		t.Error("a repo turned a harness back on")
	}
}
