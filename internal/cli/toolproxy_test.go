package cli

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/tooldef"
)

// proxyState is headroom's (or name's) state for v in no repo.
func proxyState(t *testing.T, name string, v adapter.Vendor) switchboard.State {
	t.Helper()
	b, r, _, err := toolBoard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tl, ok := r.Get(name)
	if !ok {
		t.Fatalf("no tool %s", name)
	}
	return b.State(tl, v, "")
}

// TestToolOnProxyConfirm (T6780): a proxy is turned on only with --proxy,
// for a named vendor its definition supports, at a terminal, outside any
// aggy session or borrow child, after the warning and the typed
// confirmation. Without --proxy the tool's other parts turn on and its
// proxy stays off.
func TestToolOnProxyConfirm(t *testing.T) {
	isolate(t)
	t.Setenv("AGGY_SOCKET", "")
	t.Setenv("AGGY_PARENT", "")
	t.Chdir(t.TempDir())
	px := []string{"on", "headroom", "--vendor", "claude", "--proxy"}
	for _, c := range []struct {
		name string
		in   string
		tty  bool
		env  string
		args []string
		exit int
		want string
	}{
		{"no vendor", "headroom claude\n", true, "", []string{"on", "headroom", "--proxy"}, 1, "tool.proxy_vendor_needed"},
		{"unsupported vendor", "headroom codex\n", true, "", []string{"on", "headroom", "--vendor", "codex", "--proxy"}, 1, "tool.not_applicable"},
		{"a tool with no proxy", "context7 claude\n", true, "", []string{"on", "context7", "--vendor", "claude", "--proxy"}, 1, "tool.not_applicable"},
		{"no terminal", "headroom claude\n", false, "", px, 1, "tool.proxy_not_confirmed"},
		{"--yes", "", false, "", append(slices.Clone(px), "--yes"), 2, "cli.usage"},
		{"--proxy on off", "", true, "", []string{"off", "headroom", "--vendor", "claude", "--proxy"}, 2, "cli.usage"},
		{"wrong text", "y\n", true, "", px, 1, "tool.proxy_not_confirmed"},
		{"other vendor typed", "headroom codex\n", true, "", px, 1, "tool.proxy_not_confirmed"},
		{"no answer", "", true, "", px, 1, "tool.proxy_not_confirmed"},
		{"inside a session", "headroom claude\n", true, "AGGY_SOCKET", px, 1, "tool.proxy_not_confirmed"},
		{"inside a session's CLI", "headroom claude\n", true, "AGGY_PARENT", px, 1, "tool.proxy_not_confirmed"},
		{"borrow child", "headroom claude\n", true, borrow.Delegated, px, 1, "tool.delegated"},
		{"grok", "headroom grok\n", true, "", []string{"on", "headroom", "--vendor", "grok", "--proxy"}, 1, "tool.vendor_unverified"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.env != "" {
				t.Setenv(c.env, "1")
			}
			code, _, errOut := toolRun(c.in, c.tty, c.args...)
			if code != c.exit || !strings.Contains(errOut, c.want) {
				t.Errorf("exit %d %q, want %d %s", code, errOut, c.exit, c.want)
			}
			for _, v := range adapter.Vendors() {
				if st := proxyState(t, "headroom", v); st.On || st.Parts.Proxy != nil {
					t.Errorf("%s: turned on (%s)", v, st.Why)
				}
			}
		})
	}

	// Without --proxy: the MCP server, as before, and no question.
	code, out, errOut := toolRun("", false, "on", "headroom")
	if code != 0 || !strings.Contains(out, "Its proxy stays off; add --proxy --vendor") || strings.Contains(out, "WARNING") {
		t.Fatalf("plain on: exit %d %q %q", code, out, errOut)
	}
	for _, v := range adapter.Vendors()[:2] {
		if st := proxyState(t, "headroom", v); !st.On || st.Parts.Proxy != nil {
			t.Errorf("%s: on %v proxy %v", v, st.On, st.Parts.Proxy != nil)
		}
	}

	code, out, errOut = toolRun("headroom claude\n", true, px...)
	if code != 0 {
		t.Fatalf("exit %d %q", code, errOut)
	}
	for _, w := range []string{"WARNING", "including your login token", "third-party code with its own network access",
		"forwards that traffic", "rewrites what the model sees", "inherits ANTHROPIC_BASE_URL", "your own key", "stops working",
		"connects only to that loopback address", "127.0.0.1:8787", `Type "headroom claude"`, "proxy on",
		"This proxy is experimental: its flags and health check are confirmed from headroom's source, not a live run."} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if st := proxyState(t, "headroom", adapter.Claude); st.Parts.Proxy == nil {
		t.Errorf("claude: proxy off (%s)", st.Why)
	}
	_, out, _ = toolRun("", false, "status")
	if line := statusLine(t, out, "headroom"); !strings.Contains(line, "proxy on (experimental: not verified live)") || !strings.Contains(line, "no proxy for codex") {
		t.Errorf("status: %q", line)
	}
	// aggy doctor says so.
	found := false
	for _, f := range toolFindings(context.Background(), "") {
		found = found || f.Code == codeToolProxy && f.Vendor == adapter.Claude && strings.Contains(f.Message, "login token")
	}
	if !found {
		t.Error("doctor does not list the proxy")
	}
	// A running session is offered a resume for it (T6729).
	if set, err := sessionToolSet(context.Background(), adapter.Claude, nil, ".", nil); err != nil || set["headroom"] != "mcp+proxy" {
		t.Errorf("tool set %v, %v", set, err)
	}
	// A later plain on for claude replaces the setting: the proxy is off.
	if code, _, _ := toolRun("", false, "on", "headroom", "--vendor", "claude"); code != 0 || proxyState(t, "headroom", adapter.Claude).Parts.Proxy != nil {
		t.Error("a plain on kept the proxy")
	}
	if code, _, errOut := toolRun("headroom claude\n", true, px...); code != 0 {
		t.Fatal(errOut)
	}
	code, out, _ = toolRun("", false, "off", "headroom", "--vendor", "claude")
	if code != 0 || !strings.Contains(out, "keeps its proxy") || proxyState(t, "headroom", adapter.Claude).On {
		t.Errorf("off: exit %d %s", code, out)
	}
}

// TestProxyNoteChanged (HIGH-1, consent): status says why a confirmed
// proxy is off once its definition changed, and that --proxy asks again.
func TestProxyNoteChanged(t *testing.T) {
	r, err := tooldef.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	h, _ := r.Get("headroom")
	note := proxyNote(h, adapter.Claude, switchboard.State{On: true, ProxyChanged: true}, 0)
	if !strings.Contains(note, "definition changed since you confirmed") || !strings.Contains(note, "aggy tool on headroom --vendor claude --proxy") {
		t.Errorf("note = %q", note)
	}
	on := switchboard.State{On: true, Parts: tooldef.Parts{Proxy: h.Proxy}}
	if note := proxyNote(h, adapter.Claude, on, 2); !strings.Contains(note, "neither starts") {
		t.Errorf("two proxies: %q", note)
	}
}

// TestEnvProxyBlocks (MED-3): an HTTP proxy in the environment refuses
// the tool proxy unless NO_PROXY covers its address.
func TestEnvProxyBlocks(t *testing.T) {
	v4, v6 := netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")
	for _, c := range []struct {
		env  []string
		addr netip.Addr
		want string
	}{
		{nil, v4, ""},
		{[]string{"HTTP_PROXY=http://corp:3128"}, v4, "HTTP_PROXY"},
		{[]string{"all_proxy=socks5://x"}, v6, "all_proxy"},
		{[]string{"http_proxy=http://corp:3128", "NO_PROXY=localhost,127.0.0.1"}, v4, ""},
		{[]string{"http_proxy=http://corp:3128", "NO_PROXY=localhost,127.0.0.1"}, v6, "http_proxy"},
		{[]string{"HTTP_PROXY=http://corp:3128", "no_proxy=[::1], 127.0.0.0/8"}, v6, ""},
		{[]string{"HTTP_PROXY=http://corp:3128", "no_proxy=127.0.0.0/8"}, v4, ""},
		{[]string{"HTTP_PROXY=http://corp:3128", "NO_PROXY=*"}, v6, ""},
		{[]string{"HTTPS_PROXY=http://corp:3128"}, v4, ""},
	} {
		if got := envProxyBlocks(c.env, c.addr); got != c.want {
			t.Errorf("%q %s = %q, want %q", c.env, c.addr, got, c.want)
		}
	}
}

// pxTool is a user tool whose proxy is the fake proxy in mode, on port.
func pxTool(bin, mode string, port int, dir string) string {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	return fmt.Sprintf(`name = "px"
description = "A proxy"
version = "1.0.0"
scope = "global"

[install]
check = [%[1]q, "--version"]

[proxy]
vendors = ["claude"]
argv = [%[1]q, %[2]q, %[3]q, %[4]q]
listen = %[3]q
health = "/health"

[proxy.env]
claude = "ANTHROPIC_BASE_URL"

[cost]
method = "none"
`, bin, mode, addr, dir)
}

// TestProxyEdited (T6780): an edited user file is off, proxy and all.
func TestProxyEdited(t *testing.T) {
	isolate(t)
	t.Setenv("AGGY_SOCKET", "")
	t.Setenv("AGGY_PARENT", "")
	t.Chdir(t.TempDir())
	f := filepath.Join(os.Getenv("AGGY_HOME"), "tools", "px.toml")
	addedAt(t, f, pxTool("/bin/fake", "ok", 40000, "/tmp"))
	if code, _, errOut := toolRun("px claude\n", true, "on", "px", "--vendor", "claude", "--proxy"); code != 0 {
		t.Fatal(errOut)
	}
	if st := proxyState(t, "px", adapter.Claude); st.Parts.Proxy == nil {
		t.Fatal("not on")
	}
	writeAt(t, f, pxTool("/bin/fake", "ok", 40001, "/tmp"), 0o600)
	if st := proxyState(t, "px", adapter.Claude); st.On || st.Parts.Proxy != nil {
		t.Error("an edited file kept its proxy")
	}
}

// TestProxyExperimental (owner decision on #93): the experimental label
// follows the definition's unverified list, never the tool's name.
func TestProxyExperimental(t *testing.T) {
	var tl tooldef.Tool
	tl.Name, tl.Proxy = "headroom", &tooldef.Proxy{Listen: "127.0.0.1:8787", Argv: []string{"headroom"}, Env: map[adapter.Vendor]string{adapter.Claude: "ANTHROPIC_BASE_URL"}}
	st := switchboard.State{On: true}
	st.Parts.Proxy = tl.Proxy
	for _, tt := range []struct {
		unverified []string
		want       bool
	}{{nil, false}, {[]string{"mcp"}, false}, {[]string{"proxy"}, true}, {[]string{"mcp", "proxy"}, true}} {
		tl.Unverified = tt.unverified
		note := proxyNote(tl, adapter.Claude, st, 1)
		warn := proxyWarning(tl, adapter.Claude)
		if got := strings.Contains(note, "(experimental: not verified live)"); got != tt.want {
			t.Errorf("%v: status %q", tt.unverified, note)
		}
		if got := strings.Contains(warn, "This proxy is experimental"); got != tt.want {
			t.Errorf("%v: warning %q", tt.unverified, warn)
		}
	}
}
