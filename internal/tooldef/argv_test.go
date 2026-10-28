package tooldef

import (
	"strings"
	"testing"
)

// TestArgvRefusesRunners: no spelling of a shell, env, a wrapper around
// one, or an interpreter given inline code gets through (T59-1).
func TestArgvRefusesRunners(t *testing.T) {
	for _, a := range [][]string{
		{"sh", "-c", "x"}, {"SH", "-c", "x"}, {"/BIN/SH", "-c", "x"}, {"Bash", "-c", "x"}, {"/usr/bin/ENV", "x"},
		{"bash.exe", "-c", "x"}, {"zsh-5.9", "-c", "x"}, {"bash5", "-c", "x"}, {"sh5", "x"}, {"rbash", "-c", "x"},
		{"pwsh", "-Command", "x"}, {"fish", "-c", "x"}, {"cmd.exe", "/c", "x"},
		{"env", "demo"}, {"sudo", "demo"}, {"doas", "demo"}, {"ssh", "host", "demo"}, {"eval", "x"}, {"busybox", "sh"},
		{"watch", "demo"}, {"script", "-c", "x"},
		{"nice", "sh", "-c", "x"}, {"nice", "-n", "5", "/bin/BASH", "x"}, {"nohup", "env", "x"},
		{"timeout", "10", "bash", "-c", "x"}, {"xargs", "sh"}, {"time", "nice", "zsh"},
		{"npx", "-c", "demo serve"}, {"npx", "--call=demo serve"}, {"npx", "-y", "sh"},
		{"python3", "-c", "x"}, {"Python3.12", "-Ic", "x"}, {"osascript", "-e", "x"}, {"perl", "-we", "x"},
		{"perl", "-E", "x"}, {"node", "--eval", "x"}, {"node", "--eval=x"}, {"node", "-p", "x"}, {"ruby", "-e", "x"},
		{"deno", "eval", "x"}, {"php", "-r", "x"}, {"awk", "-e", "x"},
		{"nice", "python3", "-c", "x"},
		// T6776 F-1: case folding beyond ASCII, after a wrapper too.
		{"nice", "/bin/ſh", "-c", "x"}, {"nice", "/bin/KSH"}, {"/bin/ſh"},
		// T6776 F-4: more wrappers and runners.
		{"arch", "-arm64", "sh", "-c", "x"}, {"caffeinate", "-i", "bash"}, {"chrt", "-f", "10", "sh"},
		{"taskset", "0x3", "zsh"}, {"ionice", "-c", "3", "sh"}, {"flock", "/tmp/l", "-c", "x"},
		{"flock", "/tmp/l", "sh"}, {"unbuffer", "sh"}, {"toybox", "sh"},
		{"find", ".", "-exec", "demo", ";"}, {"find", ".", "-okdir", "x"},
		{"git", "-c", "core.sshCommand=x", "fetch"}, {"git", "-calias.x=!sh", "x"}, {"git", "--config-env=a=B", "x"},
		{"sed", "1e", "f"}, {"vim", "-c", "!x"}, {"lldb", "-o", "platform shell x"}, {"awk", "BEGIN{system(\"x\")}"},
		{"gdb", "-ex", "shell x"},
		// T6776 F-5: fetching unpinned code on a launch.
		{"npx", "-y", "@demo/server@1.0.0"}, {"npx", "--yes", "demo@1.0.0"}, {"npx", "demo@latest"},
		{"npx", "demo"}, {"npx", "@demo/server"}, {"npx", "-p", "demo@next", "demo"}, {"uvx", "demo"},
		{"uvx", "--from", "demo", "demo"}, {"uvx", "demo@latest"}, {"npm", "exec", "demo"},
		{"pnpm", "dlx", "demo@latest"}, {"uv", "tool", "run", "demo"}, {"pipx", "run", "demo"},
		{"nice", "npx", "-y", "demo@1.0.0"},
	} {
		if err := argv("argv", a, true); err == nil {
			t.Errorf("%q passed", a)
		}
	}
	for _, a := range [][]string{
		{"demo"}, {"shasum", "-a", "256"}, {"ashby"}, {"nudge"}, {"npx", "@demo/server@1.0.0"},
		{"npx", "demo@1.2.3-beta.1", "--stdio"}, {"uvx", "demo==1.2.3"}, {"uvx", "--from", "demo==1.2", "demo"},
		{"npm", "exec", "demo@1.0.0"}, {"find", ".", "-name", "x"}, {"git", "log"}, {"/usr/bin/arch"},
		{"node", "/opt/demo/server.js"}, {"python3", "-m", "demo"}, {"nice", "-n", "5", "demo"},
		{"/usr/local/bin/demo", "{repo}"},
	} {
		if err := argv("argv", a, true); err != nil {
			t.Errorf("%q: %v", a, err)
		}
	}
}

// TestEnvRefused: secrets, loader and runtime injection, and vendor and
// cloud settings are refused for every tool, and a user tool passes only
// its own variables (T59-8).
func TestEnvRefused(t *testing.T) {
	for _, n := range []string{
		"ANTHROPIC_BASE_URL", "DEMO_BASE_URL", "HTTPS_PROXY", "all_proxy", "NO_PROXY",
		"OPENAI_API_KEY", "CONTEXT7_API_KEY", "GITHUB_TOKEN", "DEMO_SECRET", "AWS_ACCESS_KEY_ID",
		"GITHUB_PAT", "GH_PAT", "SENTRY_DSN", "MYSQL_PWD", "PGPASS", "TLS_KEY_FILE", "PRIVATE_KEY_PEM",
		"SESSION_COOKIE", "JWT", "STRIPE_SK", "KUBECONFIG", "CLAUDECODE", "CLAUDE_CODE_USE_BEDROCK",
		"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "NODE_OPTIONS", "PYTHONPATH", "PERL5OPT", "BASH_ENV",
		"GIT_SSH_COMMAND", "AGGY_HOME", "GOOGLE_APPLICATION_CREDENTIALS", "PATH", "HOME", "1X", "A-B", "",
	} {
		if envRefused("demo", n, true) == "" {
			t.Errorf("built-in: %q passed", n)
		}
		if envRefused("demo", n, false) == "" {
			t.Errorf("user: %q passed", n)
		}
	}
	for _, n := range []string{"DEMO_HOME", "DEMO_DIR", "DEMO_MODE"} {
		if why := envRefused("demo", n, false); why != "" {
			t.Errorf("user %q: %s", n, why)
		}
	}
	if why := envRefused("my-tool", "MY_TOOL_DIR", false); why != "" {
		t.Errorf("a dashed name's prefix: %s", why)
	}
	for _, n := range []string{"CONTEXT_DIR", "MY_TOOL_DIR", "DEMOX_DIR"} {
		if why := envRefused("demo", n, false); !strings.Contains(why, "DEMO_") {
			t.Errorf("user %q: %q, want the prefix rule", n, why)
		}
	}
}
