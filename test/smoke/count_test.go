package smoke

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestAggyGrokCallsCarryAllow (T6894): every aggy borrow that can start Grok
// passes the flag, or the version refusal fails the check without starting it.
func TestAggyGrokCallsCarryAllow(t *testing.T) {
	script, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	var lines int
	for _, l := range strings.Split(string(script), "\n") {
		if !strings.Contains(l, `"$AGGY" `) || !regexp.MustCompile(`--vendor (grok|"\$v")`).MatchString(l) {
			continue
		}
		lines++
		if !strings.Contains(l, `${AGGY_GROK_ALLOW[@]+`) && !strings.Contains(l, `${allow[@]+`) {
			t.Errorf("no --allow-untested path: %s", l)
		}
	}
	if lines != 5 { // review, image, research, video twice
		t.Errorf("%d aggy borrows that can start grok, want 5", lines)
	}
	if !strings.Contains(string(script), "AGGY_SMOKE_ANY_VERSION=1)\" >&2\n\t\t\tAGGY_GROK_ALLOW=(--allow-untested)") {
		t.Error("the flag is not set under AGGY_SMOKE_ANY_VERSION=1")
	}
}

// TestCallIfStarted (T6894): a borrow aggy refused before starting a child
// counts no vendor run. Only the helpers run: never the script, or a vendor.
func TestCallIfStarted(t *testing.T) {
	script, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	var fns strings.Builder
	for _, re := range []string{`(?m)^call\(\) \{.*\}$`, `(?ms)^call_if_started\(\) \{\n.*?^\}\n`} {
		fn := regexp.MustCompile(re).Find(script)
		if fn == nil {
			t.Fatalf("no %s in smoke.sh", re)
		}
		fns.Write(fn)
		fns.WriteString("\n")
	}
	for _, tt := range []struct {
		name, out, rc, want string
	}{
		{"a refusal prints no report", "", "1", "0"},
		{"a child that succeeded", "borrow b1: image on grok (default model): ok in 3s, 0 tokens\n", "0", "1"},
		{"a child that failed", "borrow b1: research on grok (default model): limit in 3s, 0 tokens [x]\n", "1", "1"},
		{"a cached report", "borrow b1: research on grok (default model): ok, cached from 2026-01-01 (--fresh asks again)\n", "0", "0"},
		{"another vendor's report", "borrow b1: image on codex (default model): ok in 3s, 0 tokens\n", "0", "0"},
		{"killed by the watchdog", "", "143", "1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := t.TempDir() + "/out"
			if err := os.WriteFile(f, []byte(tt.out), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", "set -u\nCALLS_grok=0\n"+fns.String()+`call_if_started grok "$1" "$2"; printf %s "$CALLS_grok"`, "bash", f, tt.rc)
			got, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("counted %s, want %s", got, tt.want)
			}
		})
	}
}

// TestEmptyAllowExpands (T6894): the empty flag array expands to nothing under
// set -u, on the bash macOS ships (3.2) where available.
func TestEmptyAllowExpands(t *testing.T) {
	bash := "/bin/bash"
	if _, err := os.Stat(bash); err != nil {
		bash = "bash"
	}
	out, err := exec.Command(bash, "-c", `set -u; A=(); printf '<%s>' x ${A[@]+"${A[@]}"}; A=(--allow-untested); printf '<%s>' x ${A[@]+"${A[@]}"}`).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if string(out) != "<x><x><--allow-untested>" {
		t.Errorf("got %s", out)
	}
}
