package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/switchboard"
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

// TestToolOnProxyConfirm (T6780): a proxy is turned on only for a named
// vendor, at a terminal, outside any aggy session or borrow child, after
// the warning and the typed confirmation.
func TestToolOnProxyConfirm(t *testing.T) {
	isolate(t)
	t.Setenv("AGGY_SOCKET", "")
	t.Setenv("AGGY_PARENT", "")
	t.Chdir(t.TempDir())
	for _, c := range []struct {
		name string
		in   string
		tty  bool
		env  string
		args []string
		exit int
		want string
	}{
		{"no vendor", "headroom claude\n", true, "", []string{"on", "headroom"}, 1, "tool.proxy_vendor_needed"},
		{"no terminal", "headroom claude\n", false, "", []string{"on", "headroom", "--vendor", "claude"}, 1, "tool.proxy_not_confirmed"},
		{"--yes", "", false, "", []string{"on", "headroom", "--vendor", "claude", "--yes"}, 2, "cli.usage"},
		{"wrong text", "y\n", true, "", []string{"on", "headroom", "--vendor", "claude"}, 1, "tool.proxy_not_confirmed"},
		{"other vendor typed", "headroom codex\n", true, "", []string{"on", "headroom", "--vendor", "claude"}, 1, "tool.proxy_not_confirmed"},
		{"no answer", "", true, "", []string{"on", "headroom", "--vendor", "claude"}, 1, "tool.proxy_not_confirmed"},
		{"inside a session", "headroom claude\n", true, "AGGY_SOCKET", []string{"on", "headroom", "--vendor", "claude"}, 1, "tool.proxy_not_confirmed"},
		{"inside a session's CLI", "headroom claude\n", true, "AGGY_PARENT", []string{"on", "headroom", "--vendor", "claude"}, 1, "tool.proxy_not_confirmed"},
		{"borrow child", "headroom claude\n", true, borrow.Delegated, []string{"on", "headroom", "--vendor", "claude"}, 1, "tool.delegated"},
		{"grok", "headroom grok\n", true, "", []string{"on", "headroom", "--vendor", "grok"}, 1, "tool.vendor_unverified"},
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

	code, out, errOut := toolRun("headroom claude\n", true, "on", "headroom", "--vendor", "claude")
	if code != 0 {
		t.Fatalf("exit %d %q", code, errOut)
	}
	for _, w := range []string{"WARNING", "including your login token", "rewrites what the model sees", "stops working", "127.0.0.1:8787", "ANTHROPIC_BASE_URL", `Type "headroom claude"`, "proxy on"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if st := proxyState(t, "headroom", adapter.Claude); st.Parts.Proxy == nil {
		t.Errorf("claude: proxy off (%s)", st.Why)
	}
	// Never added silently: another vendor stays as it was.
	if st := proxyState(t, "headroom", adapter.Codex); st.On {
		t.Errorf("codex turned on too (%s)", st.Why)
	}
	// An unsupported vendor gets the other parts, with no question.
	code, out, _ = toolRun("", false, "on", "headroom", "--vendor", "codex")
	if st := proxyState(t, "headroom", adapter.Codex); code != 0 || !st.On || st.Parts.Proxy != nil || !strings.Contains(out, "supports only claude") {
		t.Errorf("codex: exit %d, on %v proxy %v: %s", code, st.On, st.Parts.Proxy != nil, out)
	}
	_, out, _ = toolRun("", false, "status")
	if line := statusLine(t, out, "headroom"); !strings.Contains(line, "proxy on") || !strings.Contains(line, "no proxy for codex") {
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
	code, out, _ = toolRun("", false, "off", "headroom", "--vendor", "claude")
	if code != 0 || !strings.Contains(out, "keeps its proxy") || proxyState(t, "headroom", adapter.Claude).On {
		t.Errorf("off: exit %d %s", code, out)
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
	writeAt(t, f, pxTool("/bin/fake", "ok", 40000, "/tmp"), 0o600)
	if code, _, errOut := toolRun("px claude\n", true, "on", "px", "--vendor", "claude"); code != 0 {
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
