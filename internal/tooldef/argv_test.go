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
		// S63-3: {repo} as the program after a wrapper, as an
		// interpreter's script or module, or as git's exec path.
		{"nice", "{repo}/x"}, {"timeout", "10", "{repo}/x"}, {"xargs", "{repo}/x"},
		{"python3", "{repo}/x.py"}, {"python3", "-W", "ignore", "{repo}/x.py"}, {"node", "{repo}/x.js"},
		{"osascript", "{repo}/x"}, {"node", "--require", "{repo}/hook.js", "server.js"}, {"node", "--import={repo}/x.mjs", "s.js"},
		{"node", "-r", "{repo}/x.js"}, {"perl", "-I", "{repo}/lib", "x.pl"}, {"python3", "-m", "{repo}"},
		{"python3", "--", "{repo}/x.py"}, {"git", "--exec-path={repo}", "x"},
		// S63-4: pins that are not exact, and ways round them.
		{"npx", "demo@1"}, {"npx", "demo@1.2"}, {"npx", "demo@^1.2.3"}, {"npx", "demo@1.2.3 || 2"},
		{"npx", "git+https://example.com/x.git@1.2.3"}, {"npx", "https://example.com/x@1.2.3"},
		{"npx", "./local@1.2.3"}, {"npx", "--cache", "demo@1.0.0", "demo"}, {"npx", "--registry", "https://evil", "demo@1.0.0"},
		{"npm", "--yes", "exec", "demo@1.0.0"}, {"npm", "-y", "x", "demo@1.0.0"}, {"uvx", "-p", "3.12", "demo"},
		{"uvx", "--index-url", "https://evil", "demo==1.0.0"}, {"uvx", "--with-requirements", "r.txt", "demo==1.0.0"},
		{"pip", "install", "demo"}, {"npm", "install"}, {"npm", "i", "demo"}, {"npm", "run", "serve"}, {"npm", "start"},
		{"npm", "test"}, {"pnpm", "add", "x"}, {"yarn"}, {"yarn", "install"}, {"uv", "run", "x"}, {"cargo", "run"},
		{"cargo", "install", "x"}, {"go", "run", "."}, {"pipx", "install", "x"}, {"npx", "-p", "demo@1.0.0", "sh"},
		// S63-6: more runners.
		{"sandbox-exec", "-p", "x", "demo"}, {"expect", "-c", "x"}, {"xcrun", "demo"}, {"launchctl", "submit"},
		{"tmux", "new", "x"}, {"screen", "-dm", "x"},
		// S63-12: fetcher flags outside the known ones swallow the pin.
		{"npx", "--globalconfig", "demo@1.0.0", "demo"}, {"npx", "--node-options", "x", "demo@1.0.0"},
		{"npx", "--before", "2020-01-01", "demo@1.0.0"}, {"uvx", "--allow-insecure-host", "evil", "demo==1.0.0"},
		{"npx", "--cache", "/tmp/c", "demo@1.0.0"}, {"npm", "exec", "--userconfig", "/tmp/rc", "demo@1.0.0"},
		{"uvx", "--python", "{repo}/venv/bin/python", "demo==1.0.0"},
		// S63-13: package-manager subcommands outside the runners.
		{"uv", "pip", "install", "x"}, {"uv", "tool", "install", "x==1.0.0"}, {"uv", "sync"}, {"npm", "init", "x"},
		{"npm", "create", "x"}, {"pnpm", "create", "x"}, {"yarn", "create", "x"}, {"pip", "wheel", "x"},
		{"npm", "restart"}, {"cargo", "build"}, {"go", "generate"}, {"pnpm", "exec", "demo"},
		{"npm", "--prefix", "/tmp/p", "exec", "demo@1.0.0"},
		// S63-14: {repo} as code that runs, and relative scripts.
		{"deno", "run", "{repo}/x.ts"}, {"bun", "run", "{repo}/x.ts"}, {"ruby", "-C", "{repo}", "/opt/x.rb"},
		{"perl", "-I{repo}", "-MEvil", "/opt/x.pl"}, {"git", "--exec-path", "{repo}", "x"},
		{"npx", "-p", "demo@1.0.0", "--", "{repo}/evil"}, {"npx", "-p", "demo@1.0.0", "./evil"},
		{"node", "server.js"}, {"python3", "x.py"}, {"python3", "-m", "demo"}, {"bun", "x", "demo"},
		{"node", "--require", "hook", "/opt/s.js"}, {"node", "-r", "./hook.js", "/opt/s.js"}, {"deno", "run", "npm:demo"},
	} {
		if err := argv("argv", a, true); err == nil {
			t.Errorf("%q passed", a)
		}
	}
	for _, a := range [][]string{
		{"demo"}, {"shasum", "-a", "256"}, {"ashby"}, {"nudge"}, {"npx", "@demo/server@1.0.0"},
		{"npx", "demo@1.2.3-beta.1", "--stdio"}, {"uvx", "demo==1.2.3"}, {"uvx", "--from", "demo==1.2.0", "demo"},
		{"uvx", "-p", "3.12", "demo==1.2.3"}, {"npx", "-q", "demo@1.0.0"}, {"npm", "--version"},
		{"python3", "-Wignore", "-I", "-m", "demo"}, {"perl", "-MO=Deparse", "/opt/x.pl"}, {"ruby", "-I/opt/lib", "/opt/x.rb"},
		{"node", "/opt/server.js", "--root", "{repo}"}, {"python3", "-I", "-m", "demo", "{repo}"}, {"python3", "/opt/x.py", "{repo}"},
		{"git", "--exec-path=/opt/git", "log"},
		{"npm", "exec", "demo@1.0.0"}, {"find", ".", "-name", "x"}, {"git", "log"}, {"/usr/bin/arch"},
		{"node", "/opt/demo/server.js"}, {"python3", "-P", "-m", "demo"}, {"python3", "-IB", "-m", "demo"}, {"nice", "-n", "5", "demo"},
		{"deno", "run", "/opt/x.ts", "{repo}"}, {"node", "--require", "/opt/hook.js", "/opt/s.js"}, {"uvx", "--offline", "demo==1.2.3"},
		{"npx", "-p", "demo@1.0.0", "--", "demo-server", "{repo}"}, {"pipx", "run", "--spec", "demo==1.0.0", "demo"},
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
