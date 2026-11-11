package keyring

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func TestCheckName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"anthropic", true},
		{"openrouter-2", true},
		{"a", true},
		{"", false},
		{"-x", false},
		{"Anthropic", false},
		{"sk-ant-api03-ABCdef", false},
		{"two words", false},
		{"a/b", false},
		{strings.Repeat("a", 33), false},
	}
	for _, c := range cases {
		err := CheckName(c.name)
		if (err == nil) != c.ok {
			t.Errorf("CheckName(%q) = %v, want ok %v", c.name, err, c.ok)
		}
		// A key typed where the name goes is never printed back.
		if err != nil && c.name != "" && strings.Contains(err.Error(), c.name) {
			t.Errorf("CheckName(%q) quotes its input: %v", c.name, err)
		}
	}
}

func TestCheckProvider(t *testing.T) {
	cases := []struct {
		p  string
		ok bool
	}{
		{"anthropic", true}, {"openai", true}, {"amazon-bedrock", true}, {"x.ai", true},
		{"claude-agent", false}, {"opencode", false}, {"", false}, {"Anthropic", false}, {"sk-ABC", false},
	}
	for _, c := range cases {
		if err := CheckProvider(c.p); (err == nil) != c.ok {
			t.Errorf("CheckProvider(%q) = %v, want ok %v", c.p, err, c.ok)
		}
	}
}

func TestHelperFor(t *testing.T) {
	cases := []struct {
		goos string
		kind Kind
		path string
	}{
		{"darwin", Security, "/usr/bin/security"},
		{"linux", SecretTool, "/usr/bin/secret-tool"},
		{"windows", 0, ""},
		{"freebsd", 0, ""},
	}
	for _, c := range cases {
		h, err := HelperFor(c.goos)
		if c.kind == 0 {
			if err != ErrUnsupported {
				t.Errorf("%s: err = %v, want ErrUnsupported", c.goos, err)
			}
			continue
		}
		if err != nil || h.Kind != c.kind || h.Path != c.path {
			t.Errorf("%s: %+v, %v", c.goos, h, err)
		}
	}
}

// TestArgs: the argument lists name aggy's service and the key's name, never
// a value (no function takes one), and add asks for the value itself.
func TestArgs(t *testing.T) {
	sec := Helper{Security, "/usr/bin/security"}
	st := Helper{SecretTool, "/usr/bin/secret-tool"}
	add := sec.AddArgs("anthropic")
	if add[len(add)-1] != "-w" {
		t.Errorf("security add: -w is not last, so security would not ask: %q", add)
	}
	if i := slices.Index(add, "-T"); i < 0 || add[i+1] != "" {
		t.Errorf("security add: no -T \"\", so the item would trust security: %q", add)
	}
	if !slices.Contains(add, "-U") {
		t.Errorf("security add: no -U: %q", add)
	}
	for _, h := range []Helper{sec, st} {
		for _, args := range [][]string{h.AddArgs("k"), h.ReadArgs("k"), h.RemoveArgs("k")} {
			if !slices.Contains(args, Service) || !slices.Contains(args, "k") {
				t.Errorf("%s: %q does not name service %s and account k", h.Name(), args, Service)
			}
		}
	}
	if got := st.AddArgs("k"); !slices.Equal(got, []string{"store", "--label=aggy key k", "service", Service, "account", "k"}) {
		t.Errorf("secret-tool add = %q", got)
	}
	if got := sec.ReadArgs("k"); !slices.Equal(got, []string{"find-generic-password", "-s", Service, "-a", "k", "-w"}) {
		t.Errorf("security read = %q", got)
	}
	if args, ok := sec.PresenceArgs("k"); !ok || slices.Contains(args, "-w") || slices.Contains(args, "-g") {
		t.Errorf("security presence reads the value: %q", args)
	}
	if _, ok := st.PresenceArgs("k"); ok {
		t.Error("secret-tool has no presence check that does not print the value")
	}
	if !sec.NotFound(44) || sec.NotFound(1) || st.NotFound(44) || st.NotFound(1) {
		t.Error("NotFound")
	}
	// The provider is part of the item: a name and provider that pass their
	// checks make an account no other pair makes.
	if Account("anth", "anthropic") != "anth@anthropic" || Account("anth", "openai") == Account("anth", "anthropic") {
		t.Error("Account does not bind the provider")
	}
	if CheckName("a@b") == nil || CheckProvider("a@b") == nil {
		t.Error("a name or provider may hold @, so two pairs could share an account")
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"sk-ant-api03-abc_DEF\n", "sk-ant-api03-abc_DEF", true},
		{"sk-abc\r\n", "sk-abc", true},
		{"sk-abc", "sk-abc", true},
		{"a+b/c=", "a+b/c=", true},
		{"", "", false},
		{"\n", "", false},
		{"two words\n", "", false},
		{"tab\there", "", false},
		{"line\nbreak\n", "", false},
		{"caf\xc3\xa9", "", false},
		{strings.Repeat("x", MaxValue+1), "", false},
	}
	for _, c := range cases {
		raw := []byte(c.raw)
		v, err := Parse(raw)
		if (err == nil) != c.ok {
			t.Errorf("Parse(%q) err = %v, want ok %v", c.raw, err, c.ok)
			continue
		}
		if !c.ok {
			if strings.Contains(err.Error(), strings.TrimSpace(c.raw)) && strings.TrimSpace(c.raw) != "" {
				t.Errorf("Parse(%q) error quotes the value: %v", c.raw, err)
			}
			if !bytes.Equal(raw, make([]byte, len(raw))) {
				t.Errorf("Parse(%q) left the refused bytes in place", c.raw)
			}
			continue
		}
		if string(v.Bytes()) != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.raw, v.Bytes(), c.want)
		}
		v.Wipe()
		if v.Bytes() != nil || !bytes.Equal(raw[:len(c.want)], make([]byte, len(c.want))) {
			t.Errorf("Wipe left %q", raw)
		}
	}
}

// TestValueNeverPrints: every way to print a Value, or a struct holding one,
// shows the placeholder, never the bytes.
func TestValueNeverPrints(t *testing.T) {
	const secret = "sk-canary-0123456789"
	v, err := Parse([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	holder := struct {
		Name string
		Key  *Value
		Val  Value
	}{"x", v, *v}
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
		outs = append(outs, fmt.Sprintf(verb, v), fmt.Sprintf(verb, *v), fmt.Sprintf(verb, holder))
	}
	outs = append(outs, fmt.Sprint(v), fmt.Sprintln(holder), v.String())
	for _, x := range []any{v, *v, holder} {
		b, err := json.Marshal(x)
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, string(b))
	}
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("m", "key", v, "holder", holder)
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("m", "key", *v)
	outs = append(outs, logged.String())
	hex := fmt.Sprintf("%x", secret)
	for _, o := range outs {
		if strings.Contains(o, secret) || strings.Contains(o, hex) || strings.Contains(strings.ToLower(o), hex) {
			t.Errorf("printed the value: %q", o)
		}
	}
	if !strings.Contains(fmt.Sprintf("%v", holder), "[key]") {
		t.Errorf("no placeholder: %v", holder)
	}
}
