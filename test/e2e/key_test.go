//go:build unix

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/fakecli"
)

// TestKeyCycle (T6931): a full aggy key add, the on-screen launches of Claude
// Code, Codex, and Grok (and Claude under --direct), an OpenCode launch, and
// aggy key remove, against the fake keychain tool. The key reaches the fake
// keychain and OpenCode's key file, and nothing else: no other CLI's
// environment, arguments, stdin, or files; not the terminal; not any file
// under the temp HOME (the index, the diagnostic log, the run folder, the
// vendor homes, the fakes' records) once the cycle is done.
func TestKeyCycle(t *testing.T) {
	w := openCodeWorldE2E(t)
	chain := t.TempDir() // the fake keychain, outside HOME
	w.env = append(w.env, "AGGY_FAKE_KEYCHAIN="+chain)
	// A shape the redactor does not know (#138 review M2), so a leak into the
	// diagnostic log is not hidden by redact dropping it; searched for plain
	// and encoded (holdsKey).
	const canary = "gsk_e2eCanary12AB34cd56EF78gh90IJkl"
	var shown []string
	finish := func(s *session) {
		t.Helper()
		shown = append(shown, s.screen())
	}

	// aggy key add: the fake tool asks on the terminal, echo off.
	s := w.start(t, map[string]any{}, "key", "add", "anth", "--provider", "anthropic")
	if runtime.GOOS == "darwin" {
		s.waitWithin([]byte("password data for new item: "), offerTimeout)
		s.send([]byte(canary + "\r"))
		s.waitFor([]byte("retype password for new item: "))
		s.send([]byte(canary + "\r"))
	} else {
		s.waitWithin([]byte("Password: "), offerTimeout)
		s.send([]byte(canary + "\r"))
	}
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy key add exit %d: %s", code, s.screen())
	}
	finish(s)
	if b, err := os.ReadFile(filepath.Join(chain, "item.aggy-key.anth@anthropic")); err != nil || string(b) != canary {
		t.Fatalf("keychain item = %q, %v", b, err)
	}
	// The add's own calls: a look (macOS) or a clear (Linux), then the add.
	addCalls := 0
	if calls, err := os.ReadFile(filepath.Join(chain, "calls.jsonl")); err == nil {
		addCalls = bytes.Count(calls, []byte("\n"))
	}
	if addCalls != 2 {
		t.Errorf("aggy key add made %d keychain calls, want 2", addCalls)
	}

	// Claude Code, Codex, and Grok on screen, and Claude under --direct.
	for _, args := range [][]string{{"run", "claude"}, {"run", "codex"}, {"run", "grok"}, {"run", "--direct", "claude"}} {
		s := w.start(t, map[string]any{"stdout": "READY>", "canary": canary}, args...)
		s.waitFor([]byte("READY>"))
		s.send([]byte("/exit\r"))
		if code := s.exit(); code != 0 {
			t.Fatalf("%q exit %d: %s", args, code, s.screen())
		}
		finish(s)
	}
	runs := interactiveRuns(t, w)
	if len(runs) != 4 {
		t.Fatalf("%d interactive runs, want 4", len(runs))
	}
	for _, r := range runs {
		if len(r.CanaryIn) > 0 {
			t.Errorf("the key reached %s: %q", r.Vendor, r.CanaryIn)
		}
		if len(r.OpenCodeKeys) > 0 {
			t.Errorf("%s got key files: %v", r.Vendor, r.OpenCodeKeys)
		}
	}
	if calls, _ := os.ReadFile(filepath.Join(chain, "calls.jsonl")); bytes.Count(calls, []byte("\n")) != addCalls {
		t.Errorf("the keychain tool ran for a launch that is not OpenCode's:\n%s", calls)
	}
	_ = os.Remove(w.record)

	// OpenCode gets it, as a file its config names.
	s = w.start(t, map[string]any{"stdout": "READY>", "canary": canary}, "run", "opencode")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("opencode exit %d: %s", code, s.screen())
	}
	finish(s)
	if !strings.Contains(s.screen(), "OpenCode gets your key anth for anthropic") || !strings.Contains(s.screen(), "can read a key") {
		t.Errorf("the launch did not say it hands the key over: %s", s.screen())
	}
	oc := interactiveRuns(t, w)
	if len(oc) != 1 {
		t.Fatalf("%d OpenCode runs", len(oc))
	}
	sum := sha256.Sum256([]byte(canary))
	if got := oc[0].OpenCodeKeys["anthropic"]; got != hex.EncodeToString(sum[:]) {
		t.Errorf("OpenCode's anthropic key file: %q", got)
	}
	if len(oc[0].CanaryIn) > 0 {
		t.Errorf("the key reached OpenCode outside its key file: %q", oc[0].CanaryIn)
	}
	if !slices.Contains(ocPolicies(t, oc[0]), "anthropic=allow") {
		t.Errorf("anthropic is not allowed: %v", ocPolicies(t, oc[0]))
	}

	// From a session's tool shell (its AGGY_* variables), no key is read.
	_ = os.Remove(w.record)
	shell := *w
	shell.env = append(slices.Clone(w.env), "AGGY_VENDOR=claude")
	s = shell.start(t, map[string]any{"stdout": "READY>", "canary": canary}, "run", "opencode")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte("/exit\r"))
	s.exit()
	finish(s)
	if !strings.Contains(s.screen(), "not handed to OpenCode here") {
		t.Errorf("no line saying the keys were not handed over: %s", s.screen())
	}
	if oc := interactiveRuns(t, w); len(oc) != 1 || len(oc[0].OpenCodeKeys) > 0 || slices.Contains(ocPolicies(t, oc[0]), "anthropic=allow") {
		t.Errorf("a session shell's launch got the key: %+v", oc)
	}
	if calls, _ := os.ReadFile(filepath.Join(chain, "calls.jsonl")); bytes.Count(calls, []byte("\n")) != addCalls+1 {
		t.Errorf("want the add and one read only:\n%s", calls)
	}

	// What is meant to be pasted publicly, and the names-only list.
	for _, args := range [][]string{{"doctor", "--report"}, {"doctor"}, {"diag", "bundle"}, {"key", "list"}} {
		s = w.start(t, map[string]any{}, args...)
		s.exit()
		finish(s)
	}
	if !strings.Contains(shown[len(shown)-1], "anthropic") {
		t.Errorf("aggy key list: %s", shown[len(shown)-1])
	}

	// aggy key remove.
	s = w.start(t, map[string]any{}, "key", "remove", "anth")
	s.waitWithin([]byte("Delete it? [y/N]"), offerTimeout)
	s.answerUntil("y\r", []byte("Deleted key anth"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy key remove exit %d: %s", code, s.screen())
	}
	finish(s)
	if _, err := os.Stat(filepath.Join(chain, "item.aggy-key.anth@anthropic")); err == nil {
		t.Error("the keychain item outlived remove")
	}

	for _, out := range shown {
		if holdsKey([]byte(out), canary) {
			t.Errorf("the terminal showed the key:\n%s", out)
		}
	}
	// scenario.json is the test's own input, which names the canary.
	scenario := filepath.Join(w.home, "scenario.json")
	err := filepath.WalkDir(w.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() || p == scenario {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && holdsKey(b, canary) {
			t.Errorf("the key is in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls, _ := os.ReadFile(filepath.Join(chain, "calls.jsonl")); holdsKey(calls, canary) {
		t.Error("the key reached the keychain tool's arguments")
	}
}

// holdsKey reports whether b holds key plain, or as hex, base64, or base64url.
func holdsKey(b []byte, key string) bool {
	k := []byte(key)
	for _, f := range [][]byte{k, []byte(hex.EncodeToString(k)), []byte(base64.StdEncoding.EncodeToString(k)),
		[]byte(base64.RawStdEncoding.EncodeToString(k)), []byte(base64.URLEncoding.EncodeToString(k)), []byte(base64.RawURLEncoding.EncodeToString(k))} {
		if bytes.Contains(b, f) {
			return true
		}
	}
	return false
}

// TestKeyControlCanary: the fakes report a canary that does reach them, so
// TestKeyCycle's empty canary_in means something.
func TestKeyControlCanary(t *testing.T) {
	w := newWorld(t)
	w.env = append(w.env, "MY_CONTROL=control-canary-2")
	s := w.start(t, map[string]any{"stdout": "READY>", "canary": "control-canary-2"}, "run", "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte("/exit\r"))
	s.exit()
	r := interactiveRecord(t, w)
	if !slices.Contains(r.CanaryIn, "env:MY_CONTROL") {
		t.Errorf("canary_in = %q", r.CanaryIn)
	}
}

// ocPolicies are an OpenCode run's provider.use rules as provider=effect.
func ocPolicies(t *testing.T, r fakecli.Record) []string {
	t.Helper()
	var out []string
	for _, p := range ocConfig(t, r).Policies {
		if p.Action == "provider.use" {
			out = append(out, p.Resource+"="+p.Effect)
		}
	}
	return out
}
