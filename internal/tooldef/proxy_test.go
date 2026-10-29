package tooldef

import (
	"errors"
	"strings"
	"testing"
	"time"

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
		{"health_timeout zero", `health = "/health"`, "health = \"/health\"\nhealth_timeout = 0", "proxy.health_timeout"},
		{"health_timeout negative", `health = "/health"`, "health = \"/health\"\nhealth_timeout = -5", "proxy.health_timeout"},
		{"health_timeout over", `health = "/health"`, "health = \"/health\"\nhealth_timeout = 121", "proxy.health_timeout"},
		{"health_timeout huge", `health = "/health"`, "health = \"/health\"\nhealth_timeout = 9223372036854775807", "proxy.health_timeout"},
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

// TestProxyHealthTimeout (T6847): health_timeout is optional, whole
// seconds from 1 to 120, and 15 when not set; headroom's is 90.
func TestProxyHealthTimeout(t *testing.T) {
	with := func(line string) string {
		return base + strings.Replace(proxyPart, `health = "/health"`, "health = \"/health\"\n"+line, 1)
	}
	for _, c := range []struct {
		line string
		want time.Duration
	}{
		{"", 15 * time.Second},
		{"health_timeout = 1", time.Second},
		{"health_timeout = 90", 90 * time.Second},
		{"health_timeout = 120", 120 * time.Second},
	} {
		for _, builtIn := range []bool{true, false} {
			d, err := Parse("demo.toml", []byte(with(c.line)), builtIn)
			if err != nil {
				t.Fatalf("%q: %v", c.line, err)
			}
			if got := d.Proxy.Wait(); got != c.want {
				t.Errorf("%q: Wait = %s, want %s", c.line, got, c.want)
			}
		}
	}
	for _, line := range []string{`health_timeout = "90"`, "health_timeout = 1.5", "health_timeout = []"} {
		if _, err := Parse("demo.toml", []byte(with(line)), false); err == nil {
			t.Errorf("%s accepted", line)
		}
	}
	r, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	hr, _ := r.Get("headroom")
	if hr.Proxy == nil || hr.Proxy.Wait() != 90*time.Second {
		t.Errorf("headroom's proxy: %+v", hr.Proxy)
	}
}
