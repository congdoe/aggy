package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
)

// A tool's local proxy (T6780). It is on for a vendor only where the
// person ran aggy tool on <name> --proxy --vendor <vendor> at a terminal,
// read the warning, and typed the confirmation; the setting keeps
// tooldef.ProxyDigest of what they confirmed, and the switchboard keeps
// State.Parts.Proxy only while the definition still hashes to it. A
// launch of an on-screen CLI starts it, health-checks it, checks the
// listener is its own, points the CLI at it, watches it, and stops it
// when the CLI exits; a borrow child never gets one.
//
// The typed confirmation guards against mistakes, not against an agent
// running as the same user: such an agent can give aggy a pseudo-terminal
// (script, expect) or edit aggy's index. The skill forbids both.

// sessionEnvs are set inside an aggy session's CLI: an agent there, or
// a command it runs, must not turn a proxy on.
var sessionEnvs = []string{"AGGY_SOCKET", "AGGY_PARENT"}

// proxyWarning is what aggy tool on --proxy shows before it asks.
func proxyWarning(t tooldef.Tool, v adapter.Vendor) string {
	p := t.Proxy
	return fmt.Sprintf(`WARNING: this turns on %[1]s's local proxy for %[2]s.
- While it is on, every request %[2]s sends, including your login token and all your prompts, code, and replies, passes through a local process aggy starts for each on-screen %[2]s session: %[3]s
- aggy points %[2]s at %[4]s through %[5]s and connects only to that loopback address; the program's own flags decide where it binds.
- It is third-party code with its own network access, and it forwards that traffic on to the vendor.
- %[1]s rewrites what the model sees.
- Every program %[2]s starts (its shell commands, your MCP servers and hooks) inherits %[5]s too, so a script of yours that calls the API with your own key would go through the proxy as well.
- If the proxy stops or misbehaves, a session using it stops working until its CLI restarts. aggy checks the proxy before each launch and starts %[2]s without it when it does not answer.
- It never runs for borrow children.
%[6]sTurn it off with: aggy tool off %[1]s --vendor %[2]s
`, t.Name, v, termsafe.Field(strings.Join(p.Argv, " ")), p.Listen, p.Env[v], experimentalLine(t))
}

// proxyExperimental is " (experimental: not verified live)" for a tool
// whose definition lists "proxy" in unverified (the owner's decision on
// #93), else "". It is driven by the definition only, so it goes when the
// live check (docs/verification.md) removes "proxy" from that list.
func proxyExperimental(t tooldef.Tool) string {
	if slices.Contains(t.Unverified, "proxy") {
		return " (experimental: not verified live)"
	}
	return ""
}

// experimentalLine is the warning's line for such a tool, else "".
func experimentalLine(t tooldef.Tool) string {
	if proxyExperimental(t) == "" {
		return ""
	}
	return fmt.Sprintf("This proxy is experimental: its flags and health check are confirmed from %s's source, not a live run.\n", t.Name)
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

// proxiesOn counts the tools whose proxy is on for v in repo.
func proxiesOn(b switchboard.Board, v adapter.Vendor, repo string) int {
	n := 0
	for _, st := range b.On(v, repo) {
		if st.Parts.Proxy != nil {
			n++
		}
	}
	return n
}

// proxyNote says, for aggy tool status and on, whether t's proxy is on
// for v where st was decided. others is how many proxies are on for v.
func proxyNote(t tooldef.Tool, v adapter.Vendor, st switchboard.State, others int) string {
	switch {
	case t.Proxy == nil:
		return ""
	case st.Parts.Proxy != nil && others > 1:
		return "; proxy on" + proxyExperimental(t) + ", but another tool's proxy is on for " + string(v) + " too, so neither starts"
	case st.Parts.Proxy != nil && resolveProgram(t.Proxy.Argv[0]) != nil:
		return "; proxy on" + proxyExperimental(t) + ", but its program is not an executable on PATH, so it cannot start"
	case st.Parts.Proxy != nil:
		return "; proxy on" + proxyExperimental(t) + ": every request goes through " + t.Proxy.Listen
	case t.PartsFor(v).Proxy == nil:
		return "; no proxy for " + string(v)
	case st.ProxyChanged:
		return fmt.Sprintf("; proxy off: its definition changed since you confirmed; aggy tool on %s --vendor %s --proxy asks again", t.Name, v)
	case st.On:
		return fmt.Sprintf("; proxy off: aggy tool on %s --vendor %s --proxy asks for it", t.Name, v)
	}
	return ""
}

// envProxyBlocks is the HTTP proxy variable in env that would send a
// CLI's requests for addr somewhere else first, or "" (MED-3): any of
// HTTP_PROXY, http_proxy, ALL_PROXY, all_proxy set, unless NO_PROXY or
// no_proxy lists the address, its loopback range, or "*".
func envProxyBlocks(env []string, addr netip.Addr) string {
	get := func(k string) string {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, k+"="); ok {
				return v
			}
		}
		return ""
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		for _, e := range strings.Split(get(k), ",") {
			e = strings.Trim(strings.TrimSpace(e), "[]")
			if e == "*" || e == addr.String() {
				return ""
			}
			if pfx, err := netip.ParsePrefix(e); err == nil && pfx.Contains(addr) {
				return ""
			}
		}
	}
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if get(k) != "" {
			return k
		}
	}
	return ""
}

// toolProxy is a launch's proxy: the variable for the CLI's env (empty
// when none runs); stop, which the launch defers; and died, closed when
// it exits on its own mid-session (nil when none runs).
type toolProxy struct {
	env    string
	stop   func()
	died   <-chan struct{}
	lost   <-chan struct{} // the CLI was killed; died follows within execrun's retries
	onDied func()          // records it, once; call once died is closed
	dead   atomic.Bool
	tool   string
	vendor adapter.Vendor
	held   error // why the placeholder could not take the address
	// guard names the CLI the proxy serves (execrun.Proxy.Guard): it is
	// killed at once if the proxy dies and aggy cannot hold its address.
	guard func(pid int, group bool)
	// interrupted: a signal arrived while the proxy came up, so nothing
	// starts.
	interrupted bool
}

// startToolProxy starts the proxy of the tool that has one on for
// vendor v in the repo holding cwd, in the launch's private folder dir,
// and returns it once healthy and its own. Anything wrong is a warning,
// and the CLI starts without a proxy: a taken port (never a process aggy
// did not start), a failed health check, a listener outside the proxy's
// process group, an HTTP proxy in the environment, more than one proxy
// on. A signal while it waits returns one marked interrupted, and the
// launch ends with nothing started. It prints to stderr as it goes, since
// the health check can take seconds.
func startToolProxy(ctx context.Context, v adapter.Vendor, cwd, dir string, session []string, stderr io.Writer, name string) *toolProxy {
	none := &toolProxy{stop: func() {}, guard: func(int, bool) {}}
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
	refuse := func(tool string, code diag.Code, class string, err error) *toolProxy {
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
	// MED-3: the CLI would send its requests to an HTTP proxy first.
	// TODO(T6780): whether Claude honors NO_PROXY for its base URL is
	// unverified (docs/verification.md), so a match there is trusted.
	if k := envProxyBlocks(os.Environ(), t.Proxy.Addr().Addr()); k != "" {
		return refuse(t.Name, diag.CodeProxyEnvProxy, "env_proxy",
			fmt.Errorf("%s is set and NO_PROXY does not list %s, so %s would send its requests to that HTTP proxy first", k, t.Proxy.Addr().Addr(), v))
	}
	// F74-2: no PATH entry from the repo picks the proxy's program or
	// reaches it. aggy's own PATH is left alone: the CLI starts with it.
	path, err := toolPath(cwd)
	if err != nil {
		return refuse(t.Name, diag.CodeProxyFailed, "path", err)
	}
	base := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "PATH=") })
	if path != "" {
		base = append(base, "PATH="+path)
	}
	prog := t.Proxy.Argv[0]
	if !filepath.IsAbs(prog) {
		if prog, err = probe.LookPathIn(prog, path); err != nil {
			return refuse(t.Name, diag.CodeProxyFailed, "program", fmt.Errorf("%w. %s", err, installHowTo(t)))
		}
	}
	if _, class, err := launchDir(dir); err != nil {
		return refuse(t.Name, diag.CodeProxyFailed, class, err)
	}
	fmt.Fprintf(stderr, "%s: starting %s's proxy for %s on %s ...\n", name, t.Name, v, t.Proxy.Listen)
	// MED-1: a signal while it comes up stops it and goes on without it.
	sctx, cancel := execrun.NotifyContext(ctx)
	pass, set := toolTelemetry("", t) // T6822
	p, err := execrun.StartProxy(sctx, execrun.ProxySpec{Vendor: v, Var: t.Proxy.Env[v], Path: prog, Args: t.Proxy.Argv[1:],
		Addr: t.Proxy.Addr(), Health: t.Proxy.Health, Dir: dir, Wait: proxyWait, Pass: pass, Set: set}, base)
	cancel()
	switch {
	case errors.Is(err, execrun.ErrProxyPortTaken):
		return refuse(t.Name, diag.CodeProxyPortTaken, "port", err)
	case errors.Is(err, execrun.ErrProxyNotOwned):
		return refuse(t.Name, diag.CodeProxyNotOwned, "owner", err)
	case errors.Is(err, execrun.ErrProxyUnhealthy), errors.Is(err, execrun.ErrProxyExited):
		return refuse(t.Name, diag.CodeProxyUnhealthy, "health", err)
	case errors.Is(err, execrun.ErrProxyInterrupted):
		// Ctrl-C while it came up ends aggy: nothing starts.
		dlog.Log(ctx, diag.Rec{Event: diag.EvToolProxyRefused, Vendor: string(v), Code: diag.CodeProxyFailed, Msg: fixedMsg(diag.CodeProxyFailed),
			Attrs: map[string]any{"tool": t.Name, "class": "interrupted"}})
		return &toolProxy{stop: func() {}, guard: func(int, bool) {}, interrupted: true}
	case err != nil:
		return refuse(t.Name, diag.CodeProxyFailed, "start", err)
	}
	dlog.Log(ctx, diag.Rec{Event: diag.EvToolProxyStarted, Vendor: string(v), Attrs: map[string]any{"tool": t.Name}})
	fmt.Fprintf(stderr, "%s: %s's proxy is on for this %s session: every request, your login token included, goes through it (%s). aggy tool off %s --vendor %s turns it off.\n",
		name, t.Name, v, termsafe.Line(p.Env()), t.Name, v)
	// Round 2 MED: the user's own base URL (a local gateway) gives way
	// to the proxy's for this session; say so once.
	if own := os.Getenv(t.Proxy.Env[v]); own != "" && t.Proxy.Env[v]+"="+own != p.Env() {
		fmt.Fprintf(stderr, "%s: your own %s is set; %s's proxy replaces it for this %s session.\n", name, t.Proxy.Env[v], t.Name, v)
	}
	px := &toolProxy{env: p.Env(), died: p.Died(), lost: p.Lost(), tool: t.Name, vendor: v, guard: p.Guard}
	px.stop = func() {
		p.Stop()
		dlog.Log(context.Background(), diag.Rec{Event: diag.EvToolProxyStopped, Vendor: string(v), Attrs: map[string]any{"tool": t.Name}})
	}
	var once sync.Once
	px.onDied = func() {
		once.Do(func() {
			r := diag.Rec{Event: diag.EvToolProxyExited, Vendor: string(v), Attrs: map[string]any{"tool": t.Name, "held": true}}
			if px.held = p.HoldErr(); px.held != nil {
				r.Code, r.Msg, r.Attrs["held"] = diag.CodeProxyHoldFailed, fixedMsg(diag.CodeProxyHoldFailed), false
			}
			px.dead.Store(true) // after held: diedNote reads it once dead
			dlog.Log(context.Background(), r)
		})
	}
	return px
}

// diedNote is what aggy says once the CLI has exited, after its proxy
// died mid-session; "" when it did not.
func (px *toolProxy) diedNote(name string) string {
	if px.died != nil {
		select {
		case <-px.lost:
			// The CLI was killed at the first failed take; Died comes
			// once execrun stops retrying (at most half a second).
			select {
			case <-px.died:
			case <-time.After(2 * time.Second):
			}
		default:
		}
		select {
		case <-px.died:
			px.onDied() // the watch may not have run yet
		default:
		}
	}
	if !px.dead.Load() {
		return ""
	}
	if px.held != nil {
		return fmt.Sprintf("%s: WARNING: %s's proxy stopped and another process took its address (%s); aggy stopped %s so it would not send your login there. Resume with aggy resume. %s",
			name, px.tool, termsafe.Line(px.held.Error()), px.vendor, dlog.Tag(diag.CodeProxyHoldFailed))
	}
	return fmt.Sprintf("%s: %s's proxy exited during the %s session; aggy held its address, so its requests failed rather than going elsewhere. The next launch starts it again, or starts %s without it.", name, px.tool, px.vendor, px.vendor)
}

// withoutProxy drops the proxy part from a session's tool set once its
// proxy died, so the supervisor offers a resume (T6729).
func (px *toolProxy) withoutProxy(set map[string]string) {
	if !px.dead.Load() {
		return
	}
	parts := strings.Split(set[px.tool], "+")
	kept := parts[:0]
	for _, p := range parts {
		if p != "proxy" && p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		delete(set, px.tool)
	} else {
		set[px.tool] = strings.Join(kept, "+")
	}
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
