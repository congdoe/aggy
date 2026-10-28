package hook

import (
	"encoding/json"
	"testing"
)

func payload(t *testing.T, tool string, input map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": input})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPreToolUse(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		deny  bool
	}{
		{"claude -p", payload(t, "Bash", map[string]any{"command": "claude -p 'hi'"}), true},
		{"claude --print=x", payload(t, "Bash", map[string]any{"command": "claude --print=x"}), true},
		{"claude cluster", payload(t, "Bash", map[string]any{"command": "claude -pc"}), true},
		{"absolute path", payload(t, "Bash", map[string]any{"command": "/opt/bin/claude --model x -p q"}), true},
		{"codex exec", payload(t, "Bash", map[string]any{"command": "cd /x && codex exec --json -"}), true},
		{"codex e alias", payload(t, "Bash", map[string]any{"command": "codex e hi"}), true},
		{"grok -p", payload(t, "run_terminal_cmd", map[string]any{"command": "echo q | grok -p"}), true},
		{"aggy borrow", payload(t, "Bash", map[string]any{"command": "aggy borrow research 'x'"}), true},
		{"aggy image", payload(t, "Bash", map[string]any{"command": "aggy image 'a cat' --out x.png"}), true},
		{"aggy tool add", payload(t, "Bash", map[string]any{"command": "aggy tool add x.toml --yes"}), true},
		{"aggy tool remove", payload(t, "Bash", map[string]any{"command": "cd x && aggy tool remove demo"}), true},
		{"aggy tool list", payload(t, "Bash", map[string]any{"command": "aggy tool list"}), false},
		{"inside sh -c", payload(t, "Bash", map[string]any{"command": `sh -c "env A=1 claude -p hi"`}), true},
		{"cmd list", payload(t, "shell", map[string]any{"cmd": []any{"bash", "-lc", "codex exec x"}}), true},
		{"not JSON", []byte("claude -p"), true},
		{"plain ls", payload(t, "Bash", map[string]any{"command": "ls -la"}), false},
		{"grep for claude", payload(t, "Bash", map[string]any{"command": "grep -r claude ."}), false},
		{"claude interactive help", payload(t, "Bash", map[string]any{"command": "claude --help"}), false},
		{"codex review word", payload(t, "Bash", map[string]any{"command": "codex --version; echo exec"}), false},
		{"aggy inbox", payload(t, "Bash", map[string]any{"command": "aggy inbox"}), false},
		{"a file that mentions it", payload(t, "Write", map[string]any{"file_path": "/x", "content": "claude -p"}), false},
		{"no input", payload(t, "Read", nil), false},
		// Review finding #10-8.
		{"codex value flag before exec", payload(t, "Bash", map[string]any{"command": "codex -c k=v -m o3 --cd /x exec hi"}), true},
		{"grok --prompt-file", payload(t, "Bash", map[string]any{"command": "grok --prompt-file /tmp/p"}), true},
		{"grok --single", payload(t, "Bash", map[string]any{"command": "grok --single hi"}), true},
		{"npx claude-code", payload(t, "Bash", map[string]any{"command": "npx -y @anthropic-ai/claude-code@latest -p hi"}), true},
		{"npm exec codex", payload(t, "Bash", map[string]any{"command": "npm exec @openai/codex -- exec hi"}), true},
		{"pnpm dlx codex", payload(t, "Bash", map[string]any{"command": "pnpm dlx @openai/codex exec hi"}), true},
		{"IFS", payload(t, "Bash", map[string]any{"command": "claude${IFS}-p${IFS}hi"}), true},
		{"bare IFS", payload(t, "Bash", map[string]any{"command": "codex$IFS" + "exec x"}), true},
		{"env and timeout", payload(t, "Bash", map[string]any{"command": "env -u X A=1 timeout -s KILL 30 grok -p q"}), true},
		{"sudo -u", payload(t, "Bash", map[string]any{"command": "sudo -u me aggy borrow news x"}), true},
		{"find -print", payload(t, "Bash", map[string]any{"command": "find . -name claude -print"}), false},
		{"git log -S claude -p", payload(t, "Bash", map[string]any{"command": "git log -S claude -p"}), false},
		{"echo mention", payload(t, "Bash", map[string]any{"command": "echo use claude -p"}), false},
		// Review finding R-4: command position after reserved words and
		// in commands that run another.
		{"if then", payload(t, "Bash", map[string]any{"command": "if true; then claude -p hi; fi"}), true},
		{"if condition", payload(t, "Bash", map[string]any{"command": "if grok -p q; then :; fi"}), true},
		{"brace group", payload(t, "Bash", map[string]any{"command": "{ claude -p; }"}), true},
		{"bang", payload(t, "Bash", map[string]any{"command": "! claude -p"}), true},
		{"for do", payload(t, "Bash", map[string]any{"command": "for x in a b; do codex exec $x; done"}), true},
		{"while do", payload(t, "Bash", map[string]any{"command": "while :; do aggy borrow news x; done"}), true},
		{"else", payload(t, "Bash", map[string]any{"command": "if x; then y; else grok -p q; fi"}), true},
		{"find -exec", payload(t, "Bash", map[string]any{"command": "find . -exec claude -p {} +"}), true},
		{"find -execdir", payload(t, "Bash", map[string]any{"command": `find . -name x -execdir codex exec {} \;`}), true},
		{"watch", payload(t, "Bash", map[string]any{"command": "watch claude -p"}), true},
		{"watch -n", payload(t, "Bash", map[string]any{"command": "watch -n 5 grok -p q"}), true},
		{"ssh", payload(t, "Bash", map[string]any{"command": "ssh h claude -p"}), true},
		{"ssh flags, quoted", payload(t, "Bash", map[string]any{"command": "ssh -p 22 -i k me@h 'codex exec x'"}), true},
		{"find -exec grep", payload(t, "Bash", map[string]any{"command": "find . -exec grep -l claude {} +"}), false},
		{"ssh other command", payload(t, "Bash", map[string]any{"command": "ssh h ls claude -p"}), false},
		{"for over words", payload(t, "Bash", map[string]any{"command": "for x in claude -p; do echo $x; done"}), false},
		// C17-5.
		{"env -S", payload(t, "Bash", map[string]any{"command": "env -S 'claude -p hi'"}), true},
		{"env -S joined", payload(t, "Bash", map[string]any{"command": "env -S'claude -p hi'"}), true},
		{"env --split-string=", payload(t, "Bash", map[string]any{"command": "env --split-string='grok -p hi'"}), true},
		{"eval", payload(t, "Bash", map[string]any{"command": "eval 'claude -p hi'"}), true},
		{"parallel", payload(t, "Bash", map[string]any{"command": "parallel claude -p ::: a"}), true},
		{"parallel -j", payload(t, "Bash", map[string]any{"command": "parallel -j 2 codex exec ::: a b"}), true},
		{"busybox sh -c", payload(t, "Bash", map[string]any{"command": "busybox sh -c 'claude -p hi'"}), true},
		{"script bsd", payload(t, "Bash", map[string]any{"command": "script -q /dev/null claude -p hi"}), true},
		{"script -c", payload(t, "Bash", map[string]any{"command": "script -q -c 'grok -p hi' /dev/null"}), true},
		{"ssh --", payload(t, "Bash", map[string]any{"command": "ssh host -- claude -p hi"}), true},
		{"env --", payload(t, "Bash", map[string]any{"command": "env -- claude -p hi"}), true},
		{"script only a file", payload(t, "Bash", map[string]any{"command": "script -q out.log"}), false},
		{"eval other", payload(t, "Bash", map[string]any{"command": "eval 'echo claude -p'"}), false},
		{"null payload", []byte("null"), true},
		{"array payload", []byte("[]"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := PreToolUse(tt.input)
			if d.Deny != tt.deny || (d.Deny && d.Reason == "") {
				t.Errorf("decision %+v, want deny %v", d, tt.deny)
			}
		})
	}
}
