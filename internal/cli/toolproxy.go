package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
)

// A tool's local proxy (T6780). It is on for a vendor only where the
// person ran aggy tool on <name> --vendor <vendor> at a terminal, read
// the warning, and typed the confirmation (store.ToolSwitch.Proxy); the
// switchboard keeps State.Parts.Proxy only there. A launch of an
// on-screen CLI starts it, health-checks it, points the CLI at it, and
// stops it when the CLI exits; a borrow child never gets one.

// sessionEnvs are set inside an aggy session's CLI: an agent there, or
// a command it runs, must not turn a proxy on.
var sessionEnvs = []string{"AGGY_SOCKET", "AGGY_PARENT"}

// proxyWarning is what aggy tool on shows before it asks.
func proxyWarning(t tooldef.Tool, v adapter.Vendor) string {
	p := t.Proxy
	return fmt.Sprintf(`WARNING: this turns on %[1]s's local proxy for %[2]s.
While it is on, every request %[2]s sends, including your login token and all
your prompts, code, and replies, passes through a local process aggy starts
for each on-screen %[2]s session: %[3]s, listening on %[4]s, which %[2]s
reaches through %[5]s. %[1]s rewrites what the model sees. If the proxy
stops or misbehaves, a session using it stops working until its CLI
restarts; aggy checks the proxy before each launch and starts %[2]s without
it when it does not answer. It never runs for borrow children.
Turn it off with: aggy tool off %[1]s --vendor %[2]s
`, t.Name, v, termsafe.Field(strings.Join(p.Argv, " ")), p.Listen, p.Env[v])
}

// confirmProxy shows the warning and asks the person to type
// "<name> <vendor>". Only a terminal, outside any aggy session, can.
func confirmProxy(t tooldef.Tool, v adapter.Vendor, in io.Reader, tty bool, stdout io.Writer) error {
	refused := func(why string) error {
		return withCode(diag.CodeProxyConfirm, fmt.Errorf("%s's proxy was not turned on: %s", t.Name, why))
	}
	for _, k := range sessionEnvs {
		if os.Getenv(k) != "" {
			return refused("this runs inside an aggy session (" + k + " is set); run it in your own terminal")
		}
	}
	if !tty {
		return refused("it needs you at a terminal to read the warning and type the confirmation")
	}
	fmt.Fprint(stdout, proxyWarning(t, v))
	want := t.Name + " " + string(v)
	fmt.Fprintf(stdout, "Type %q to turn it on for %s: ", want, v)
	line, _ := bufio.NewReader(io.LimitReader(in, maxAnswer)).ReadString('\n')
	if strings.TrimSpace(line) != want {
		return refused("the confirmation did not match; nothing changed")
	}
	return nil
}

// proxyNote says, for aggy tool status and on, whether t's proxy is on
// for v where st was decided.
func proxyNote(t tooldef.Tool, v adapter.Vendor, st switchboard.State) string {
	switch {
	case t.Proxy == nil:
		return ""
	case st.Parts.Proxy != nil:
		return "; proxy on: every request goes through " + t.Proxy.Listen
	case t.PartsFor(v).Proxy == nil:
		return "; no proxy for " + string(v)
	case st.On:
		return fmt.Sprintf("; proxy off: aggy tool on %s --vendor %s asks for it", t.Name, v)
	}
	return ""
}

// toolProxy is a launch's proxy: the variable for the CLI's env (empty
// when none runs), and stop, which the launch defers.
type toolProxy struct {
	env  string
	stop func()
}

// startToolProxy starts the proxy of the tool that has one on for
// vendor v in the repo holding cwd, in the launch's private folder dir,
// and returns it once healthy. Anything wrong is a warning, and the CLI
// starts without a proxy: a taken port (never a process aggy did not
// start), a failed health check, more than one proxy on. It prints to
// stderr as it goes, since the health check can take seconds.
func startToolProxy(ctx context.Context, v adapter.Vendor, cwd, dir string, session []string, stderr io.Writer, name string) toolProxy {
	none := toolProxy{stop: func() {}}
	repo, _ := toolRepo(cwd, false)
	b, _, _, err := toolBoard(ctx)
	if err != nil {
		return none // addTools warned already
	}
	if repo.root != "" {
		if b.RepoOff, err = repoToolsOff(repo); err != nil {
			return none
		}
	}
	b.Session = session
	var on []switchboard.State
	for _, st := range b.On(v, repo.key) {
		if st.Parts.Proxy != nil {
			on = append(on, st)
		}
	}
	if len(on) == 0 {
		return none
	}
	refuse := func(tool string, code diag.Code, class string, err error) toolProxy {
		dlog.Log(ctx, diag.Rec{Event: diag.EvToolProxyRefused, Vendor: string(v), Code: code, Msg: fixedMsg(code),
			Attrs: map[string]any{"tool": tool, "class": class}})
		fmt.Fprintf(stderr, "%s: tool %s's proxy not used: %s; %s starts without it, talking to its vendor directly %s\n",
			name, tool, termsafe.Line(err.Error()), v, dlog.Tag(code))
		return none
	}
	if len(on) > 1 {
		return refuse(on[0].Tool.Name, diag.CodeProxyConflict, "conflict", errors.New("more than one tool with a proxy is on for "+string(v)))
	}
	t := on[0].Tool
	// R80-3, as for its servers: a changed skill keeps the whole tool out.
	if err, ok := changedSkills(ctx, on)[t.Name]; ok {
		return refuse(t.Name, codeOf(err, diag.CodeToolSkillChanged), "skill_changed", err)
	}
	if v == adapter.Grok && !grokToolsVerified {
		return refuse(t.Name, diag.CodeToolUnverified, "unverified", errors.New(grokUnverified))
	}
	prog := t.Proxy.Argv[0]
	if !filepath.IsAbs(prog) {
		if prog, err = probe.LookPath(prog); err != nil {
			return refuse(t.Name, diag.CodeProxyFailed, "program", fmt.Errorf("%w. %s", err, installHowTo(t)))
		}
	}
	if _, class, err := launchDir(dir); err != nil {
		return refuse(t.Name, diag.CodeProxyFailed, class, err)
	}
	fmt.Fprintf(stderr, "%s: starting %s's proxy for %s on %s ...\n", name, t.Name, v, t.Proxy.Listen)
	p, err := execrun.StartProxy(execrun.ProxySpec{Vendor: v, Var: t.Proxy.Env[v], Path: prog, Args: t.Proxy.Argv[1:],
		Addr: t.Proxy.Addr(), Health: t.Proxy.Health, Dir: dir, Wait: proxyWait}, os.Environ())
	switch {
	case errors.Is(err, execrun.ErrProxyPortTaken):
		return refuse(t.Name, diag.CodeProxyPortTaken, "port", err)
	case errors.Is(err, execrun.ErrProxyUnhealthy), errors.Is(err, execrun.ErrProxyExited):
		return refuse(t.Name, diag.CodeProxyUnhealthy, "health", err)
	case err != nil:
		return refuse(t.Name, diag.CodeProxyFailed, "start", err)
	}
	dlog.Log(ctx, diag.Rec{Event: diag.EvToolProxyStarted, Vendor: string(v), Attrs: map[string]any{"tool": t.Name}})
	fmt.Fprintf(stderr, "%s: %s's proxy is on for this %s session: every request, your login token included, goes through it (%s). aggy tool off %s --vendor %s turns it off.\n",
		name, t.Name, v, termsafe.Line(p.Env()), t.Name, v)
	return toolProxy{env: p.Env(), stop: func() {
		p.Stop()
		dlog.Log(context.Background(), diag.Rec{Event: diag.EvToolProxyStopped, Vendor: string(v), Attrs: map[string]any{"tool": t.Name}})
	}}
}

// proxyWait is how long a launch waits for a proxy's health check.
var proxyWait = execrun.ProxyWait

// vendorList is vs as text.
func vendorList(vs []adapter.Vendor) string {
	var out []string
	for _, v := range vs {
		out = append(out, string(v))
	}
	return strings.Join(out, ", ")
}

// proxyVars are the variables t's proxy sets, in vendor order.
func proxyVars(t tooldef.Tool) []string {
	var out []string
	for _, v := range t.Proxy.Vendors {
		out = append(out, t.Proxy.Env[v])
	}
	return out
}
