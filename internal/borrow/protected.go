package borrow

import "strings"

// protectedNames are files and dirs, at any depth, that hold code a tool
// runs or text an agent loads, beyond the trust sources (TrustMarker):
// git's hook dirs that core.hooksPath commonly names, git attribute and
// submodule files, pre-commit, other git hook managers' config (lefthook),
// package manager config that runs scripts or names a registry (npm,
// yarn, pnpm), editor tasks, CI workflows, and other agents' config and
// instructions. A dir protects everything under it.
var protectedNames = []string{
	".husky", ".githooks", ".gitattributes", ".gitmodules", ".pre-commit-config.yaml",
	"lefthook.yml", "lefthook.yaml", "lefthook-local.yml", "lefthook-local.yaml",
	".lefthook.yml", ".lefthook.yaml", ".lefthook-local.yml", ".lefthook-local.yaml",
	".npmrc", ".yarnrc", ".yarnrc.yml", ".pnpmfile.cjs",
	".claude", ".grok", ".codex", ".cursor", ".gemini", ".vscode", ".github", ".aggy",
	"GEMINI.md", ".cursorrules",
}

// ProtectedPath reports whether an edit's diff may not write the
// repo-relative, slash-separated path rel, compared case-insensitively:
//
//   - any path with a .git part;
//   - any path under a top-level dot dir (".x/..."), with no exceptions:
//     tools keep hooks, CI, and agent config in such dirs, and an edit that
//     needs one is applied by hand;
//   - a protectedNames file or dir, or a TrustMarker source, at any depth.
//
// A hook a child writes would run unsandboxed on the person's next commit
// or session. The repo's own core.hooksPath is checked by the caller,
// which can ask git for it.
func ProtectedPath(rel string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) > 1 && strings.HasPrefix(parts[0], ".") {
		return true
	}
	for i, c := range parts {
		if strings.EqualFold(c, ".git") {
			return true
		}
		sub := strings.ToLower(strings.Join(parts[i:], "/"))
		for _, s := range protectedNames {
			s = strings.ToLower(s)
			if sub == s || strings.HasPrefix(sub, s+"/") {
				return true
			}
		}
	}
	return TrustMarker(rel)
}
