package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/parity"
)

// inPlaceWorld is a person at a terminal (term) about to be asked, answering
// with typed; shell puts them in a session's own tool shell.
func inPlaceWorld(t *testing.T, typed string, term, shell bool) {
	t.Helper()
	oldT, oldIn, oldA := offerTerminal, offerIn, typeAhead
	offerTerminal, offerIn, typeAhead = func() bool { return term }, strings.NewReader(typed), 0
	t.Cleanup(func() { offerTerminal, offerIn, typeAhead = oldT, oldIn, oldA })
	for _, v := range []string{"AGGY_DELEGATED", "AGGY_SOCKET", "AGGY_PARENT", "AGGY_VENDOR"} {
		t.Setenv(v, "")
	}
	if shell {
		t.Setenv("AGGY_SOCKET", "/tmp/x.sock")
	}
}

// TestAskInPlace (T6979): the one question every in-place offer asks: a
// person at a terminal answers it; y or yes is yes, anything else no; with no
// terminal, or in a session's own tool shell, it asks nothing; a line typed
// ahead of it is dropped; a paste answers nothing.
func TestAskInPlace(t *testing.T) {
	q := inPlace{change: "aggy can do X.", undo: "aggy unwire undoes it.", ask: "Do X?"}
	for _, c := range []struct {
		name        string
		typed       string
		term, shell bool
		yes, asked  bool
	}{
		{"yes", "y\n", true, false, true, true},
		{"yes, spelled out", "Yes\n", true, false, true, true},
		{"no", "n\n", true, false, false, true},
		{"empty is no", "\n", true, false, false, true},
		{"end of input is no", "", true, false, false, true},
		{"no terminal", "y\n", false, false, false, false},
		{"a session's tool shell", "y\n", true, true, false, false},
		{"a paste", "\x1b[200~y\n\x1b[201~\n", true, false, false, true},
		{"a paste over lines", "\x1b[200~y\nmore\nend\x1b[201~\n", true, false, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			inPlaceWorld(t, c.typed, c.term, c.shell)
			var out bytes.Buffer
			yes, asked := askInPlace(os.Getenv, offerIn, &out, q)
			if yes != c.yes || asked != c.asked {
				t.Fatalf("yes %v asked %v, want %v %v; %q", yes, asked, c.yes, c.asked, out.String())
			}
			if asked && (!strings.Contains(out.String(), "aggy can do X.") || !strings.Contains(out.String(), "aggy unwire undoes it.") || !strings.Contains(out.String(), "Do X? [y/N]")) {
				t.Errorf("the question: %q", out.String())
			}
			if !asked && out.Len() > 0 {
				t.Errorf("printed with no one to ask: %q", out.String())
			}
		})
	}
	t.Run("typed ahead", func(t *testing.T) {
		inPlaceWorld(t, "", true, false)
		typeAhead = 20 * time.Millisecond
		var out bytes.Buffer
		if yes, _ := askInPlace(os.Getenv, &slowReader{lines: []string{"y\n"}, pause: 60 * time.Millisecond}, &out, q); yes {
			t.Error("a y typed before the question answered it")
		}
		if yes, _ := askInPlace(os.Getenv, &slowReader{lines: []string{"x\n", "y\n"}, pause: 60 * time.Millisecond}, &out, q); !yes {
			t.Error("the answer typed after the question was not taken")
		}
	})
}

// TestCodexRulesInPlace (T6979): a launch of Codex with no rules file offers
// to write it, as aggy policy wire does: yes writes it and the policy reaches
// Codex; no, or no one to ask, leaves the refusal, which names the command.
func TestCodexRulesInPlace(t *testing.T) {
	for _, c := range []struct {
		name, typed string
		term        bool
		wrote       bool
	}{
		{"yes", "y\n", true, true},
		{"no", "n\n", true, false},
		{"no terminal", "y\n", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			inPlaceWorld(t, c.typed, c.term, false)
			path, err := parity.CodexRulesPath()
			if err != nil {
				t.Fatal(err)
			}
			if why, _ := policyDelivery(adapter.Codex, nil); why != whyRulesMissed {
				t.Fatalf("before: %q", why)
			}
			var out bytes.Buffer
			wrote, err := wireCodexRulesInPlace(context.Background(), os.Getenv, offerIn, &out)
			if err != nil || wrote != c.wrote {
				t.Fatalf("wrote %v, %v: %q", wrote, err, out.String())
			}
			_, statErr := os.Stat(path)
			if (statErr == nil) != c.wrote {
				t.Errorf("rules file there: %v", statErr == nil)
			}
			why, _ := policyDelivery(adapter.Codex, nil)
			if c.wrote && why != "" || !c.wrote && !strings.Contains(why, "aggy policy wire") {
				t.Errorf("after: %q", why)
			}
		})
	}
}

// TestSwitchFix (T6979): the switch picker's refusal for the missing Codex
// rules file names aggy policy wire as its page's fix; any other refusal has
// none.
func TestSwitchFix(t *testing.T) {
	isolate(t)
	if lines, confirm, ok := switchFix(adapter.Codex, errors.New("the safety policy cannot be applied to codex: "+whyRulesMissed)); !ok || strings.Join(confirm, " ") != "policy wire" || !strings.Contains(strings.Join(lines, "\n"), "aggy unwire undoes it") {
		t.Errorf("rules missing: %v %q %q", ok, confirm, lines)
	}
	for _, c := range []struct {
		v   adapter.Vendor
		err error
	}{
		{adapter.Codex, errors.New("the safety policy cannot be applied to codex: " + whyRulesOther)},
		{adapter.Grok, errors.New(whyRulesMissed)},
		{adapter.Codex, nil},
	} {
		if _, _, ok := switchFix(c.v, c.err); ok {
			t.Errorf("%s %v: a fix offered", c.v, c.err)
		}
	}
	// The page runs exactly the fix, and only on its y.
	if out := sessionAggy([]string{"policy", "wire"}, false); !strings.Contains(out, "not a command for inside a session") {
		t.Errorf("without the y: %q", out)
	}
	if out := sessionAggy([]string{"policy", "wire", "--dry-run"}, true); !strings.Contains(out, "not a command for inside a session") {
		t.Errorf("another form: %q", out)
	}
	if out := sessionAggy([]string{"policy", "wire"}, true); !strings.Contains(out, "Done (run ") {
		t.Errorf("the y: %q", out)
	}
	if out := sessionAggy([]string{"hooks", "uninstall", "--vendor", "claude"}, true); !strings.Contains(out, "not a command for inside a session") {
		t.Errorf("hooks uninstall: %q", out)
	}
}

// TestHarnessOnInPlace (T6979): a launch of a harness turned off offers to
// turn it on in the person's own config, as aggy config set does.
func TestHarnessOnInPlace(t *testing.T) {
	for _, c := range []struct {
		name, typed string
		term, shell bool
		on          bool
	}{
		{"yes", "y\n", true, false, true},
		{"no", "n\n", true, false, false},
		{"no terminal", "y\n", false, false, false},
		{"a session's tool shell", "y\n", true, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			path, _ := config.Path()
			writeAt(t, path, "[vendors.grok]\noff = true\n", 0o600)
			inPlaceWorld(t, c.typed, c.term, c.shell)
			var out bytes.Buffer
			on, err := harnessOnInPlace(context.Background(), adapter.Grok, os.Getenv, offerIn, &out)
			if err != nil || on != c.on {
				t.Fatalf("on %v, %v: %q", on, err, out.String())
			}
			cfg, err := config.LoadDefault()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Vendors.For(adapter.Grok).Off == c.on {
				t.Errorf("grok off = %v after %s", cfg.Vendors.For(adapter.Grok).Off, c.name)
			}
			if c.on && !strings.Contains(out.String(), "aggy unwire --run") {
				t.Errorf("no undo said: %q", out.String())
			}
		})
	}
}

// TestLoosenInPlace (T6979): a loosening flag is asked about alone, naming
// what it does to the policy on that CLI; a yes lets that flag through for
// this launch only, a no or no one to ask keeps the refusal, and --no-policy
// still lets every such flag through unasked.
func TestLoosenInPlace(t *testing.T) {
	words := []string{"--permission-mode", "bypassPermissions", "--dangerously-skip-permissions"}
	for _, c := range []struct {
		name    string
		answers []bool
		want    []string
		err     bool
	}{
		{"both yes", []bool{true, true}, []string{"--permission-mode bypassPermissions", "--dangerously-skip-permissions"}, false},
		{"first yes, second no", []bool{true, false}, nil, true},
		{"no", []bool{false}, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked []string
			ask := func(flag, effect string) bool {
				if effect == "" {
					t.Errorf("%s asked with no effect", flag)
				}
				asked = append(asked, flag)
				return c.answers[len(asked)-1]
			}
			var tg target
			p, err := parseAsking(adapter.Claude, &tg, words, adapter.ParseOpts{}, ask)
			if (err != nil) != c.err {
				t.Fatalf("err %v", err)
			}
			if !c.err && strings.Join(p.Loosens, ",") != "--permission-mode,--dangerously-skip-permissions" || len(p.Safe) != 0 && !c.err {
				t.Errorf("loosens %q safe %q", p.Loosens, p.Safe)
			}
			if !c.err && strings.Join(tg.Allowed, ",") != strings.Join(c.want, ",") {
				t.Errorf("allowed %q", tg.Allowed)
			}
			// Sorted again (the launch after aggy <vendor> sorted them), nothing
			// already answered yes is asked again.
			n := len(asked)
			if !c.err {
				if _, err := parseAsking(adapter.Claude, &tg, words, adapter.ParseOpts{}, ask); err != nil || len(asked) != n {
					t.Errorf("asked again: %q, %v", asked, err)
				}
			}
		})
	}
	t.Run("the question says what it does", func(t *testing.T) {
		inPlaceWorld(t, "y\n", true, false)
		var out bytes.Buffer
		if !askLoosen(os.Getenv, offerIn, &out)("--permission-mode bypassPermissions", passFlags(adapter.Claude).LoosenEffect("--permission-mode bypassPermissions")) {
			t.Fatal("no")
		}
		for _, w := range []string{"Claude asks nothing in this mode", "refuses what the policy would ask", "this launch only", "--no-policy"} {
			if !strings.Contains(out.String(), w) {
				t.Errorf("lacks %q: %q", w, out.String())
			}
		}
	})
	t.Run("no terminal keeps the refusal", func(t *testing.T) {
		inPlaceWorld(t, "y\n", false, false)
		var tg target
		if _, err := parseAsking(adapter.Codex, &tg, []string{"--yolo"}, adapter.ParseOpts{}, askLoosen(os.Getenv, offerIn, io.Discard)); !errors.Is(err, adapter.ErrLoosens) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("a session's tool shell keeps the refusal", func(t *testing.T) {
		inPlaceWorld(t, "y\n", true, true)
		var tg target
		if _, err := parseAsking(adapter.Grok, &tg, []string{"--always-approve"}, adapter.ParseOpts{}, askLoosen(os.Getenv, offerIn, io.Discard)); !errors.Is(err, adapter.ErrLoosens) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("--no-policy asks nothing", func(t *testing.T) {
		var tg target
		p, err := parseAsking(adapter.Codex, &tg, []string{"--yolo"}, adapter.ParseOpts{NoPolicy: true}, func(string, string) bool {
			t.Error("asked")
			return false
		})
		if err != nil || strings.Join(p.Loosens, ",") != "--yolo" {
			t.Errorf("%q %v", p.Loosens, err)
		}
	})
}

// TestUntestedOnly (T6979): a borrow no vendor could start offers to run the
// first vendor passed over only for its version, never one passed over for
// its lockdown.
func TestUntestedOnly(t *testing.T) {
	for _, c := range []struct {
		err  error
		want adapter.Vendor
		ok   bool
	}{
		{&borrow.NoStartError{Tried: []borrow.Attempt{{Vendor: adapter.Codex, Code: diag.CodeCLIMissing}, {Vendor: adapter.Grok, Code: diag.CodeUntested}}}, adapter.Grok, true},
		{&borrow.NoStartError{Tried: []borrow.Attempt{{Vendor: adapter.Grok, Code: diag.CodeVersionCheck}}}, "", false},
		{errors.New("other"), "", false},
		{nil, "", false},
	} {
		if v, ok := untestedOnly(c.err); v != c.want || ok != c.ok {
			t.Errorf("%v: %q %v", c.err, v, ok)
		}
	}
}
