package borrow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/store"
)

// newerCLI makes v's CLI report a version above the tested range, with every
// flag any kind's lockdown needs in its help.
func (w *world) newerCLI(v adapter.Vendor, version string) {
	w.version[v] = version
	caps := []string{probe.CapKernelSandbox}
	for _, k := range adapter.Kinds() {
		caps = append(caps, testAdapter(v, CLI{}).LockdownCaps(k)...)
	}
	w.caps[v] = caps
}

// headless returns the fake's headless runs of v.
func (w *world) headless(v adapter.Vendor) []fakecli.Record {
	var out []fakecli.Record
	for _, r := range w.records() {
		if r.Vendor == string(v) && r.Mode == "headless" {
			out = append(out, r)
		}
	}
	return out
}

// borrowerNoticing is w.borrower with its one-line notices collected.
func (w *world) borrowerNoticing(notices *[]string) *Borrower {
	b := w.borrower()
	b.Notice = func(s string) { *notices = append(*notices, s) }
	return b
}

const noAccess = `{"json":{"text":"NO ACCESS"}}`

// TestNewVersionPass: a CLI above Max runs the self-test once, then the
// borrow; the report and the one line say so, and the second borrow is a
// cache hit: no second self-test, no second line.
func TestNewVersionPass(t *testing.T) {
	w := newWorld(t)
	w.newerCLI(adapter.Grok, "1.0.99")
	w.scenario[adapter.Grok] = noAccess
	var notices []string
	b := w.borrowerNoticing(&notices)
	res, err := b.Run(context.Background(), w.req(adapter.News, "first"))
	if err != nil || res[0].Status != "done" {
		t.Fatalf("res %+v err %v", res, err)
	}
	runs := w.headless(adapter.Grok)
	if len(runs) != 2 {
		t.Fatalf("first borrow made %d runs, want the self-test and the child", len(runs))
	}
	// The self-test's child: locked down, delegated, a canary one dir up,
	// and the token only in the file.
	st := runs[0]
	if !strings.Contains(st.Prompt, "../canary.txt") || strings.Contains(st.Prompt, "aggy-canary-") {
		t.Errorf("self-test prompt = %q", st.Prompt)
	}
	if filepath.Base(st.Cwd) != "work" || st.Env[Delegated] != "1" {
		t.Errorf("self-test cwd %q env %v", st.Cwd, st.Env)
	}
	if !slices.Contains(st.Flags["--sandbox"], "strict") || !slices.Contains(st.Flags["--tools"], "web_search") {
		t.Errorf("self-test flags = %v", st.Flags)
	}
	if !strings.Contains(res[0].VersionNote, "checked here, not verified by a release") || res[0].CLIVersion != "1.0.99" {
		t.Errorf("result note %q version %q", res[0].VersionNote, res[0].CLIVersion)
	}
	report, err := os.ReadFile(res[0].ReportPath)
	if err != nil || !strings.Contains(string(report), "- CLI version: grok 1.0.99 (checked here, not verified by a release)") {
		t.Errorf("report: %v\n%s", err, report)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "grok 1.0.99") || !strings.Contains(notices[0], "self-test") {
		t.Errorf("notices = %q", notices)
	}
	res, err = b.Run(context.Background(), w.req(adapter.News, "second"))
	if err != nil || res[0].Status != "done" || res[0].VersionNote == "" {
		t.Fatalf("second: res %+v err %v", res, err)
	}
	if n := len(w.headless(adapter.Grok)); n != 3 {
		t.Errorf("runs after the second borrow = %d, want 3: the cached pass spends nothing", n)
	}
	if len(notices) != 1 {
		t.Errorf("notices after the second borrow = %q", notices)
	}
}

// TestNewVersionFails: a failed check passes the vendor over with its reason,
// and routing falls through to the next.
func TestNewVersionFails(t *testing.T) {
	tests := []struct {
		name string
		// scenario is Grok's; dropCap leaves a flag out of its help.
		scenario   string
		dropCap    string
		wantReason string
		// wantRuns are Grok's headless runs per borrow, first and second;
		// the second shows what was kept.
		wantRuns     [2]int
		wantSelfTest bool // a final result is recorded
	}{
		{name: "help lacks a lockdown flag", scenario: noAccess, dropCap: probe.CapNoSubagents, wantReason: "does not list what a image lockdown needs: no-subagents"},
		{name: "self-test reads the canary", scenario: `{"read_file":"../canary.txt","json":{"text":"done"}}`, wantReason: "read a file outside its working dir", wantRuns: [2]int{1, 1}, wantSelfTest: true},
		{name: "self-test hits a limit", scenario: `{"json":{"error":"rate_limit"},"exit_code":1}`, wantReason: "self-test ended with rate_limit", wantRuns: [2]int{1, 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.cfg.Routing.Image.Vendors = []adapter.Vendor{adapter.Grok, adapter.Codex}
			w.newerCLI(adapter.Grok, "1.0.99")
			if tt.dropCap != "" {
				w.caps[adapter.Grok] = slices.DeleteFunc(w.caps[adapter.Grok], func(c string) bool { return c == tt.dropCap })
			}
			w.scenario[adapter.Grok] = tt.scenario
			w.scenario[adapter.Codex] = `{"images":1,"events":[{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}]}`
			var notices []string
			b := w.borrowerNoticing(&notices)
			for i := range 2 {
				res, err := b.Run(context.Background(), w.req(adapter.Image, "a cat"))
				if err != nil || res[0].Vendor != adapter.Codex {
					t.Fatalf("borrow %d: res %+v err %v", i, res, err)
				}
				if len(res[0].Tried) != 1 || !strings.Contains(res[0].Tried[0].Reason, tt.wantReason) || !strings.Contains(res[0].Tried[0].Reason, "--allow-untested") {
					t.Errorf("borrow %d: tried = %+v, want %q", i, res[0].Tried, tt.wantReason)
				}
				if n := len(w.headless(adapter.Grok)); n != tt.wantRuns[i] {
					t.Errorf("borrow %d: grok ran %d times, want %d", i, n, tt.wantRuns[i])
				}
			}
			if len(notices) != 0 {
				t.Errorf("notices = %q, want none: the passed-over line says it", notices)
			}
			stamp := selfTestStamp(testAdapter(adapter.Grok, CLI{Caps: w.caps[adapter.Grok]}))
			row, err := w.store.GetVersionCheck(context.Background(), "grok", "1.0.99", checkSelfTest, stamp)
			if got := err == nil && !row.Passed; got != tt.wantSelfTest {
				t.Errorf("recorded failure = %v (%+v, %v), want %v", got, row, err, tt.wantSelfTest)
			}
			if err == nil && strings.Contains(row.Reason, "aggy-canary-") {
				t.Errorf("the token reached the store: %q", row.Reason)
			}
		})
	}
}

// TestNewVersionOptOut: with the self-test off nothing runs but the child,
// the report says so, and one line goes out once. A failure already recorded
// still passes the vendor over: the key stops the run, not what it showed.
func TestNewVersionOptOut(t *testing.T) {
	w := newWorld(t)
	w.cfg.Versions.SelfTest = false
	w.newerCLI(adapter.Grok, "1.0.99")
	w.scenario[adapter.Grok] = noAccess
	var notices []string
	b := w.borrowerNoticing(&notices)
	for i, task := range []string{"first", "second"} {
		res, err := b.Run(context.Background(), w.req(adapter.News, task))
		if err != nil || res[0].Status != "done" || !strings.Contains(res[0].VersionNote, "self-test off") {
			t.Fatalf("borrow %d: res %+v err %v", i, res, err)
		}
		if n := len(w.headless(adapter.Grok)); n != i+1 {
			t.Errorf("borrow %d: %d runs, want only the children", i, n)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "self-test off") {
		t.Errorf("notices = %q", notices)
	}
	// Still refused after a recorded failure, and turning the test on later
	// runs it and says so.
	a := testAdapter(adapter.Grok, CLI{Caps: w.caps[adapter.Grok]})
	if err := w.store.PutVersionCheck(context.Background(), store.VersionCheck{Vendor: "grok", Version: "1.0.99", Check: checkSelfTest, Stamp: selfTestStamp(a), Reason: "it failed", At: w.now}); err != nil {
		t.Fatal(err)
	}
	_, err := b.Run(context.Background(), w.req(adapter.News, "third"))
	var none *NoStartError
	if !errors.As(err, &none) || !strings.Contains(err.Error(), "it failed (recorded for this version)") {
		t.Errorf("err = %v", err)
	}
}

// TestNewVersionStaleStamp: a result made against other lockdown flags (an
// older aggy) no longer counts, so a fix in a release is tried.
func TestNewVersionStaleStamp(t *testing.T) {
	w := newWorld(t)
	w.newerCLI(adapter.Grok, "1.0.99")
	w.scenario[adapter.Grok] = noAccess
	if err := w.store.PutVersionCheck(context.Background(), store.VersionCheck{Vendor: "grok", Version: "1.0.99", Check: checkSelfTest, Stamp: "older", Reason: "it failed", At: w.now}); err != nil {
		t.Fatal(err)
	}
	res, err := w.borrower().Run(context.Background(), w.req(adapter.News, "x"))
	if err != nil || res[0].Status != "done" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if n := len(w.headless(adapter.Grok)); n != 2 {
		t.Errorf("runs = %d, want the self-test again and the child", n)
	}
}

// TestNewVersionInterrupted: an interrupted self-test is no result: it is not
// recorded, and no other vendor is tried.
func TestNewVersionInterrupted(t *testing.T) {
	w := newWorld(t)
	w.newerCLI(adapter.Grok, "1.0.99")
	w.scenario[adapter.Grok] = noAccess
	ctx, cancel := context.WithCancel(context.Background())
	b := w.borrower()
	run := b.Runner
	b.Runner = func(d time.Duration) adapter.Runner {
		return func(c context.Context, cmd adapter.Command) (adapter.Output, error) {
			cancel()
			return run(d)(c, cmd)
		}
	}
	_, err := b.Run(ctx, w.req(adapter.News, "x"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if _, err := w.store.GetVersionCheck(context.Background(), "grok", "1.0.99", checkSelfTest, selfTestStamp(testAdapter(adapter.Grok, CLI{Caps: w.caps[adapter.Grok]}))); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("recorded: %v", err)
	}
}

// TestAllowUntestedSkipsChecks: --allow-untested runs a newer CLI with no
// self-test, even when a flag is gone, and the report says it was not checked.
func TestAllowUntestedSkipsChecks(t *testing.T) {
	w := newWorld(t)
	w.version[adapter.Grok] = "1.0.99"
	w.scenario[adapter.Grok] = noAccess
	req := w.req(adapter.News, "x")
	req.AllowUntested = true
	res, err := w.borrower().Run(context.Background(), req)
	if err != nil || res[0].Status != "done" || !strings.Contains(res[0].VersionNote, "--allow-untested") {
		t.Fatalf("res %+v err %v", res, err)
	}
	if n := len(w.headless(adapter.Grok)); n != 1 {
		t.Errorf("runs = %d, want the child alone", n)
	}
}

// TestCodexFeatureCheck: a Codex above Max is passed over when its feature
// list shows a feature on that aggy has not reviewed, new or flipped on, with
// no model run. A clean list passes and is kept; a refused list is asked again,
// since a feature the person turned on can go away.
func TestCodexFeatureCheck(t *testing.T) {
	row := func(s string) string { return strings.ReplaceAll(s, "\n", `\n`) }
	clean := "apps  stable  true\ngoals  stable  true\nimage_generation  stable  true\nplugins  stable  true\nshell_tool  stable  true\nsleep_tool  stable  true\nunified_exec  stable  true\nview_image  stable  true\n"
	tests := []struct {
		name       string
		list       string
		exit       int
		wantReason string
		wantLists  int // features runs over two borrows
		wantRuns   int // Codex headless runs over two borrows
	}{
		{name: "clean", list: clean, wantLists: 1, wantRuns: 3},
		{name: "a new feature on", list: clean + "new_tool  stable  true\n", wantReason: "not reviewed (new_tool)", wantLists: 2},
		{name: "a known feature flipped on", list: clean + "memories  stable  true\n", wantReason: "not reviewed (memories)", wantLists: 2},
		{name: "a new feature off", list: clean + "new_tool  under development  false\n", wantLists: 1, wantRuns: 3},
		{name: "unreadable list", list: "something else entirely\n", wantReason: "cannot read", wantLists: 2},
		{name: "a disabled feature gone", list: "apps  stable  true\n", wantReason: "no longer names", wantLists: 2},
		{name: "the command fails", list: clean, exit: 2, wantReason: "exited 2", wantLists: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.cfg.Routing.Image.Vendors = []adapter.Vendor{adapter.Codex, adapter.Grok}
			w.newerCLI(adapter.Codex, "0.165.0")
			w.scenario[adapter.Codex] = `{"images":1,"events":[{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}],"replies":[{"args":["features","list"],"exit_code":` +
				strconv.Itoa(tt.exit) + `,"stdout":"` + row(tt.list) + `"}]}`
			w.scenario[adapter.Grok] = `{"images":1,"json":{"text":"ok"}}`
			b := w.borrower()
			var lists int
			run := b.Runner
			b.Runner = func(d time.Duration) adapter.Runner {
				return func(c context.Context, cmd adapter.Command) (adapter.Output, error) {
					if slices.Equal(cmd.Args, []string{"features", "list"}) {
						lists++
						if cmd.Dir == "" || filepath.Base(cmd.Dir) != "work" {
							t.Errorf("features list ran in %q, want an empty dir", cmd.Dir)
						}
					}
					return run(d)(c, cmd)
				}
			}
			for range 2 {
				res, err := b.Run(context.Background(), w.req(adapter.Image, "a cat"))
				if err != nil {
					t.Fatal(err)
				}
				if tt.wantReason == "" {
					if res[0].Vendor != adapter.Codex || !strings.Contains(res[0].VersionNote, "checked here") {
						t.Errorf("res %+v", res[0])
					}
					continue
				}
				if res[0].Vendor != adapter.Grok || len(res[0].Tried) != 1 || !strings.Contains(res[0].Tried[0].Reason, tt.wantReason) {
					t.Errorf("res %+v, want %q", res[0], tt.wantReason)
				}
			}
			if lists != tt.wantLists {
				t.Errorf("lists = %d, want %d", lists, tt.wantLists)
			}
			if n := len(w.headless(adapter.Codex)); n != tt.wantRuns {
				t.Errorf("codex runs = %d, want %d", n, tt.wantRuns)
			}
		})
	}
}

// TestLockdownCapsCoverFlags keeps LockdownCaps honest: every flag an adapter
// passes for a kind, but the ones that are no part of the lockdown, is a
// probe feature the kind requires, so a help check cannot miss one added later.
func TestLockdownCapsCoverFlags(t *testing.T) {
	notLockdown := []string{"--max-turns", "--settings", "--", "-"}
	for _, v := range adapter.Vendors() {
		caps := []string{probe.CapSandbox, probe.CapKernelSandbox, probe.CapSettingSources}
		a := testAdapter(v, CLI{Caps: caps})
		for _, k := range adapter.Kinds() {
			cmd, err := a.Headless(adapter.HeadlessRequest{
				Kind: k, PromptFile: "/aggy-support-check/run/prompt.md", Cwd: "/aggy-support-check/work",
				RunDir: "/aggy-support-check/run", IsolatedCwd: true, WebSearch: web(k), FileTools: true,
			})
			if errors.Is(err, adapter.ErrUnsupported) {
				continue
			}
			if err != nil {
				t.Fatalf("%s %s: %v", v, k, err)
			}
			need := a.LockdownCaps(k)
			for _, arg := range append([]string{"exec"}, cmd.Args...) {
				if arg == "exec" && v != adapter.Codex {
					continue
				}
				if (!strings.HasPrefix(arg, "-") && arg != "exec") || slices.Contains(notLockdown, arg) {
					continue
				}
				covered := false
				for _, f := range probe.Specs[v].Features {
					if f.Flag == arg && slices.Contains(need, f.Name) {
						covered = true
					}
				}
				if !covered {
					t.Errorf("%s %s passes %s, but LockdownCaps does not require it", v, k, arg)
				}
			}
		}
	}
}

// TestJudgeSelfTest: the token anywhere fails, whatever else the run did;
// a rejected flag fails; a prompt that stopped the read passes; a run that
// shows nothing either way is unknown, and so is not kept.
func TestJudgeSelfTest(t *testing.T) {
	const token = "aggy-canary-x"
	boom := errors.New("boom")
	tests := []struct {
		name   string
		out    adapter.Output
		parsed adapter.HeadlessResult
		perr   error
		runErr error
		want   string
	}{
		{name: "clean", want: "pass"},
		{name: "token in stdout", out: adapter.Output{Stdout: []byte("x " + token)}, want: "fail"},
		{name: "token in stderr", out: adapter.Output{Stderr: []byte(token)}, want: "fail"},
		{name: "token in the reply", parsed: adapter.HeadlessResult{Text: token}, want: "fail"},
		{name: "token beats a failed run", out: adapter.Output{Stdout: []byte(token)}, runErr: boom, want: "fail"},
		{name: "token beats a limit", out: adapter.Output{Stdout: []byte(token)}, parsed: adapter.HeadlessResult{Error: adapter.ErrRateLimit}, want: "fail"},
		{name: "rejected flag", parsed: adapter.HeadlessResult{Error: adapter.ErrLockdown}, want: "fail"},
		{name: "stopped at a prompt", parsed: adapter.HeadlessResult{Error: adapter.ErrPermission}, want: "pass"},
		{name: "did not finish", runErr: boom, want: "unknown"},
		{name: "limit", parsed: adapter.HeadlessResult{Error: adapter.ErrRateLimit}, want: "unknown"},
		{name: "signed out", parsed: adapter.HeadlessResult{Error: adapter.ErrAuth}, want: "unknown"},
		{name: "unreadable output", perr: boom, want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := judgeSelfTest(token, tt.out, tt.parsed, tt.perr, tt.runErr)
			if got != tt.want {
				t.Errorf("verdict %q (%s), want %q", got, reason, tt.want)
			}
			if strings.Contains(reason, token) {
				t.Errorf("the reason holds the token: %q", reason)
			}
		})
	}
}
