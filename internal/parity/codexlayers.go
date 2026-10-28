package parity

import (
	"path/filepath"
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
