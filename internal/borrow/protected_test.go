package borrow

import "testing"

// TestProtectedPath (W-2): hooks, CI, editor tasks, and agent config are
// protected at any depth, and every top-level dot dir is.
func TestProtectedPath(t *testing.T) {
	for _, p := range []string{
		".husky/pre-commit", ".githooks/pre-push", ".claude/hooks/h.sh", ".vscode/tasks.json",
		".github/workflows/ci.yml", ".pre-commit-config.yaml", "GEMINI.md", ".gemini/settings.json",
		".cursorrules", ".aggy/config.toml", ".codex/rules/x.rules", ".gitattributes", "sub/.gitmodules",
		"a/.git/config", "sub/.claude/settings.json", "sub/.husky/pre-commit", ".anything/x",
		"AGENTS.md", "pkg/CLAUDE.md", ".mcp.json", ".envrc", ".Husky/pre-commit",
		// T6748: top-level hook and package manager config, outside a dot dir.
		".npmrc", "pkg/.npmrc", "lefthook.yml", "Lefthook.YAML", "lefthook-local.yml", ".lefthook.yml",
		".yarnrc.yml", ".pnpmfile.cjs",
	} {
		if !ProtectedPath(p) {
			t.Errorf("%s is not protected", p)
		}
	}
	for _, p := range []string{"a.go", "internal/x/y.go", ".gitignore", "docs/README.md", "pkg/.eslintrc"} {
		if ProtectedPath(p) {
			t.Errorf("%s is protected", p)
		}
	}
}
