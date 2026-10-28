package parity

import (
	"path/filepath"

	"github.com/congdoe/aggy/internal/safefs"
)

// CodexSystemDir is the folder of Codex's system config layer on Unix
// (config/src/loader/mod.rs, SYSTEM_CONFIG_TOML_FILE_UNIX, at
// rust-v0.164.0).
const CodexSystemDir = "/etc/codex"

// CodexLayerDir is the folder of one Codex config layer: its config.toml
// and its rules folder live there.
type CodexLayerDir struct {
	Dir     string
	Project bool // a .codex folder in the project tree, applied only when trusted
}

// CodexLayerDirs returns the config folders Codex 0.164.0 reads below the
// -c session flags, lowest precedence first: the system folder, the user
// folder ($CODEX_HOME), then a .codex folder in each directory from the
// project root (the nearest ancestor with .git, else cwd) down to cwd
// (config/src/loader/mod.rs, load_config_layers_state and
// discover_project_layers; state.rs, config_folder). A .codex folder that
// is the user folder is skipped, as Codex skips it. Cloud-managed config
// layers are not files aggy can read, and are not listed.
func CodexLayerDirs(cwd, root string) ([]CodexLayerDir, error) {
	cfg, err := CodexConfigPath()
	if err != nil {
		return nil, err
	}
	user := filepath.Dir(cfg)
	out := []CodexLayerDir{{Dir: CodexSystemDir}, {Dir: user}}
	var tree []string
	for d := filepath.Clean(cwd); ; d = filepath.Dir(d) {
		tree = append(tree, d)
		if d == filepath.Clean(root) || filepath.Dir(d) == d {
			break
		}
	}
	for i := len(tree) - 1; i >= 0; i-- {
		if dot := filepath.Join(tree[i], ".codex"); dot != user {
			out = append(out, CodexLayerDir{Dir: dot, Project: true})
		}
	}
	return out, nil
}

// OtherCodexRules lists the *.rules files Codex would load beside aggy's:
// every regular *.rules file in the rules folder of each layer folder of
// CodexLayerDirs, but aggy's own. Codex stops at the first file it cannot
// parse and then drops every rules file, aggy's included
// (core/src/exec_policy.rs, load_exec_policy and
// load_exec_policy_with_warning), so each of these can switch aggy's
// policy off, a trusted repo's among them. Unreadable folders are
// skipped.
func OtherCodexRules(cwd, root string) ([]string, error) {
	dirs, err := CodexLayerDirs(cwd, root)
	if err != nil {
		return nil, err
	}
	ours, err := CodexRulesPath()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		rules := filepath.Join(d.Dir, "rules")
		des, err := safefs.ReadDir(rules)
		if err != nil {
			continue
		}
		for _, de := range des {
			p := filepath.Join(rules, de.Name())
			if filepath.Ext(p) == ".rules" && de.Type().IsRegular() && p != ours {
				out = append(out, p)
			}
		}
	}
	return out, nil
}
