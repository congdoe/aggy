package tooldef

import (
	"errors"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// proxyPart is a valid [proxy] part, appended to base.
const proxyPart = `[proxy]
vendors = ["claude"]
argv = ["demo", "proxy", "--port", "8787"]
listen = "127.0.0.1:8787"
health = "/health"

[proxy.env]
claude = "ANTHROPIC_BASE_URL"
`

// TestParseProxy (T6780): built-ins and user tools may declare a proxy;
// the validator keeps it on loopback and to the allowlisted variable.
func TestParseProxy(t *testing.T) {
	grok := strings.NewReplacer(`vendors = ["claude"]`, `vendors = ["claude", "grok"]`,
		`claude = "ANTHROPIC_BASE_URL"`, "claude = \"ANTHROPIC_BASE_URL\"\ngrok = \"GROK_MODELS_BASE_URL\"")
	for _, c := range []struct {
		name, text string
		builtIn    bool
	}{
		{"user", base + proxyPart, false},
		{"built-in", base + proxyPart, true},
		{"ipv6 loopback", base + strings.Replace(proxyPart, `"127.0.0.1:8787"`, `"[::1]:8787"`, 1), false},
		{"grok too", base + grok.Replace(proxyPart), false},
		{"proxy only", "name = \"px\"\ndescription = \"p\"\nversion = \"1\"\nscope = \"global\"\n[install]\ncheck = [\"demo\"]\n[cost]\nmethod = \"none\"\n" + proxyPart, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, err := Parse("demo.toml", []byte(c.text), c.builtIn)
			if err != nil {
				t.Fatal(err)
			}
			if d.Proxy == nil || d.PartsFor(adapter.Claude).Proxy == nil || d.PartsFor(adapter.Codex).Proxy != nil || d.SkillOnly() {
				t.Errorf("proxy parts wrong: %+v", d.Proxy)
			}
			if a := d.Proxy.Addr(); !a.Addr().IsLoopback() || a.Port() != 8787 {
				t.Errorf("addr %v", a)
			}
		})
	}
	for _, c := range []struct{ name, old, new, key string }{
		{"any address", `"127.0.0.1:8787"`, `"0.0.0.0:8787"`, "proxy.listen"},
		{"lan address", `"127.0.0.1:8787"`, `"192.168.1.5:8787"`, "proxy.listen"},
		{"other loopback", `"127.0.0.1:8787"`, `"127.0.0.2:8787"`, "proxy.listen"},
		{"host name", `"127.0.0.1:8787"`, `"localhost:8787"`, "proxy.listen"},
		{"no port", `"127.0.0.1:8787"`, `"127.0.0.1"`, "proxy.listen"},
		{"low port", `"127.0.0.1:8787"`, `"127.0.0.1:80"`, "proxy.listen"},
		{"mapped v4", `"127.0.0.1:8787"`, `"[::ffff:127.0.0.1]:8787"`, "proxy.listen"},
		{"zone", `"127.0.0.1:8787"`, `"[::1%lo0]:8787"`, "proxy.listen"},
		{"var not allowed", `claude = "ANTHROPIC_BASE_URL"`, `claude = "ANTHROPIC_API_KEY"`, "proxy.env.claude"},
		{"another vendor's var", `claude = "ANTHROPIC_BASE_URL"`, `claude = "GROK_MODELS_BASE_URL"`, "proxy.env.claude"},
		{"var missing", `claude = "ANTHROPIC_BASE_URL"`, ``, "proxy.env.claude"},
		{"env for an unlisted vendor", `claude = "ANTHROPIC_BASE_URL"`, "claude = \"ANTHROPIC_BASE_URL\"\ngrok = \"GROK_MODELS_BASE_URL\"", "proxy.env"},
		{"codex has no var", `vendors = ["claude"]`, `vendors = ["claude", "codex"]`, "proxy.vendors"},
		{"no vendors", `vendors = ["claude"]`, `vendors = []`, "proxy.vendors"},
		{"repo placeholder", `"--port", "8787"]`, `"--dir", "{repo}"]`, "proxy.argv"},
		{"shell", `argv = ["demo", "proxy", "--port", "8787"]`, `argv = ["sh", "-c", "demo proxy"]`, "proxy.argv"},
		{"no argv", `argv = ["demo", "proxy", "--port", "8787"]`, ``, "proxy.argv"},
		{"health url", `"/health"`, `"http://evil.example/health"`, "proxy.health"},
		{"health dotdot", `"/health"`, `"/../health"`, "proxy.health"},
		{"health query", `"/health"`, `"/health?x=1"`, "proxy.health"},
		{"health missing", `"/health"`, `""`, "proxy.health"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(proxyPart, c.old) {
				t.Fatalf("proxyPart lacks %q", c.old)
			}
			text := base + strings.Replace(proxyPart, c.old, c.new, 1)
			for _, builtIn := range []bool{true, false} {
				_, err := Parse("demo.toml", []byte(text), builtIn)
				var e *Error
				if !errors.As(err, &e) || e.Key != c.key {
					t.Errorf("built-in %v: err = %v, want one naming %s", builtIn, err, c.key)
				}
			}
		})
	}
}
