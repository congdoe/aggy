package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter/hermes"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/store"
)

var hermesCaps = []string{probe.CapClassicScreen, probe.CapResume, probe.CapQueryFile}

// hermesWorld is openCodeWorld with fake-hermes for hermesRun: Hermes' home
// is the temp HOME's .hermes.
func hermesWorld(t *testing.T) (repo, dir, bin string, s *store.Store) {
	t.Helper()
	repo, dir = openCodeWorld(t)
	t.Setenv("HERMES_HOME", "")
	t.Setenv("HERMES_DATA_DIR_SUFFIX", "")
	fakes := fakecli.Build(t)
	t.Setenv(fakecli.EnvRecord, filepath.Join(t.TempDir(), "rec.jsonl"))
	old := hermesRun
	hermesRun = execrun.HermesRunner(30 * time.Second)
	t.Cleanup(func() { hermesRun = old })
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return repo, dir, fakes.Hermes, s
}

func hermesCode(err error) diag.Code {
	var he *hermesErr
	if errors.As(err, &he) {
		return he.code
	}
	return diag.Code{}
}

// TestPrepareHermes (T6933): the launch is refused until Hermes is ticked;
// then it makes aggy's profile with hermes profile create, writes its block,
// and checks Hermes reads it back; a second launch writes nothing.
func TestPrepareHermes(t *testing.T) {
	repo, dir, bin, s := hermesWorld(t)
	in := hermesInput{s: s, caps: hermesCaps, path: bin, cwd: filepath.Join(repo, "sub"), dir: dir, runDir: filepath.Join(os.Getenv("AGGY_HOME"), "run"),
		plan: parity.PolicyPlan{OnScreen: true}, version: "0.23.1"}
	ctx := context.Background()
	if _, err := prepareHermes(ctx, in); hermesCode(err) != diag.CodeHermesUntrusted {
		t.Fatalf("unticked: %v", err)
	}
	tickTrustRows(t, repo, trustRowHermes)
	out, err := prepareHermes(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !hermes.ValidProfile(out.launch.Profile) || out.root != repo || out.token == "" || out.planPath == "" {
		t.Fatalf("setup = %+v", out)
	}
	cfg, err := os.ReadFile(filepath.Join(out.profileDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(cfg), "model:\n") || !strings.Contains(string(cfg), hermes.BlockStart) || !strings.Contains(string(cfg), "--profile "+out.launch.Profile+" --event pre_tool_call") {
		t.Fatalf("config.yaml:\n%s", cfg)
	}
	warns := strings.Join(out.warns, "\n")
	for _, w := range []string{"made aggy's Hermes profile", "asks you once", "not aggy's", "adopt_external_logins"} {
		if !strings.Contains(warns, w) {
			t.Errorf("warns lack %q:\n%s", w, warns)
		}
	}
	// The plan the hook reads is the launch's, and holds the profile as protected.
	plan, err := readDecidePlan(out.planPath, out.planSum)
	if err != nil || plan.Root != repo || !strings.Contains(strings.Join(plan.Protected, "\n"), out.profileDir) {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	// Again: nothing made or written; the block reads back.
	again, err := prepareHermes(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(again.warns, "\n"), "made aggy's") || again.token == out.token {
		t.Fatalf("second launch: %+v", again)
	}
	if b, _ := os.ReadFile(filepath.Join(out.profileDir, "config.yaml")); !bytes.Equal(b, cfg) {
		t.Fatalf("rewritten:\n%s", b)
	}
	// --no-policy: no plan, so the hook has no opinion; the block stays.
	in.noPolicy = true
	np, err := prepareHermes(ctx, in)
	if err != nil || np.planPath != "" {
		t.Fatalf("--no-policy: %+v, %v", np, err)
	}

	// A key aggy owns outside its block refuses the launch, and the file stays.
	bad := append([]byte("approvals:\n  mode: off\n"), cfg...)
	if err := os.WriteFile(filepath.Join(out.profileDir, "config.yaml"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	in.noPolicy = false
	if _, err := prepareHermes(ctx, in); hermesCode(err) != diag.CodeHermesProfile {
		t.Fatalf("owned key outside: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(out.profileDir, "config.yaml")); !bytes.Equal(b, bad) {
		t.Fatal("a refused launch wrote")
	}
	if err := os.WriteFile(filepath.Join(out.profileDir, "config.yaml"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	// A live launch with no session id yet refuses a second one in the repo.
	if _, err := s.PutHermesLaunch(ctx, store.HermesLaunch{ParentID: "p", ChildID: "c", Profile: out.launch.Profile, Token: "t", Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareHermes(ctx, in); hermesCode(err) != diag.CodeHermesUnclaimed {
		t.Fatalf("unclaimed: %v", err)
	}
}

// TestHermesTickInPlace (T6979): at a terminal, a launch in a folder whose
// trust list does not tick Hermes asks to tick it, and only it; a yes goes on,
// a no or no terminal refuses naming aggy trust.
func TestHermesTickInPlace(t *testing.T) {
	for _, c := range []struct {
		name  string
		typed string
		term  bool
		yes   bool
	}{
		{"yes", "y\n", true, true},
		{"no", "n\n", true, false},
		{"no terminal", "y\n", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo, dir, bin, s := hermesWorld(t)
			// The folder answered before, Claude ticked, Hermes not shown, and
			// "never ask again" set (#134 review: the tick must keep it).
			tickTrustRows(t, repo, trustRowClaude)
			setTrustNever(t, repo)
			inPlaceWorld(t, c.typed, c.term, false)
			var out bytes.Buffer
			in := hermesInput{s: s, caps: hermesCaps, path: bin, cwd: repo, dir: dir, runDir: filepath.Join(os.Getenv("AGGY_HOME"), "run"),
				plan: parity.PolicyPlan{OnScreen: true}, getenv: os.Getenv, in: offerIn, out: &out}
			_, err := prepareHermes(context.Background(), in)
			if c.yes {
				if err != nil || !hermesTicked(repo) || !strings.Contains(out.String(), "Tick Hermes Agent here") {
					t.Fatalf("err %v, ticked %v, out %q", err, hermesTicked(repo), out.String())
				}
				f, _ := readTrustRecord(filepath.Join(os.Getenv("AGGY_HOME"), "trust.json"))
				for _, a := range f.Folders {
					if !slices.Contains(a.On, trustRowClaude) {
						t.Errorf("Claude's tick was lost: %+v", a)
					}
					if !a.Never {
						t.Errorf("the folder's never answer was cleared: %+v", a)
					}
				}
				return
			}
			if hermesCode(err) != diag.CodeHermesUntrusted || !strings.Contains(err.Error(), "aggy trust") || hermesTicked(repo) {
				t.Fatalf("err %v, ticked %v", err, hermesTicked(repo))
			}
		})
	}
}

// TestPrepareHermesUnreachable: a CLI whose help lacks --cli is refused.
func TestPrepareHermesUnreachable(t *testing.T) {
	repo, dir, bin, s := hermesWorld(t)
	tickTrustRows(t, repo, trustRowHermes)
	_, err := prepareHermes(context.Background(), hermesInput{s: s, caps: []string{probe.CapResume}, path: bin, cwd: repo, dir: dir, runDir: t.TempDir()})
	if hermesCode(err) != diag.CodeHermesProfile || !strings.Contains(err.Error(), "--cli") {
		t.Fatalf("err = %v", err)
	}
}

func TestHermesHookArgs(t *testing.T) {
	ok := []string{"--home", "/h/.aggy", "--profile", "aggy-0123456789ab", "--event", "pre_tool_call"}
	if _, _, _, good := hermesHookArgs(ok); !good {
		t.Fatal("refused good args")
	}
	for _, bad := range [][]string{
		nil,
		{"--home", "rel", "--profile", "aggy-0123456789ab", "--event", "pre_tool_call"},
		{"--home", "/h/../x", "--profile", "aggy-0123456789ab", "--event", "pre_tool_call"},
		{"--home", "/h", "--profile", "default", "--event", "pre_tool_call"},
		{"--home", "/h", "--profile", "aggy-0123456789ab", "--event", "post_llm_call"},
		{"--profile", "aggy-0123456789ab", "--home", "/h", "--event", "pre_tool_call"},
	} {
		if _, _, _, good := hermesHookArgs(bad); good {
			t.Errorf("took %q", bad)
		}
	}
}
