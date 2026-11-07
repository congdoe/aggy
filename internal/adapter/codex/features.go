package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
)

// A Codex lockdown is a --disable list, so a tool behind a feature new in a
// later Codex would be on in a borrow child, and a canary cannot show it.
// So a Codex above the tested range must also pass this check, which needs
// no model call: codex features list prints every feature in Codex's table,
// one "name  stage  enabled" row, after loading config only
// (cli/src/main.rs#L1921 to #L1953 at 6aaac69; the load is cli/src/cloud_config.rs,
// which also builds Codex's cloud config loader, so a signed-in CLI may talk
// to its vendor there; no model request is on this path). Source only: never
// run live (docs/verification.md, T6929).
//
// What it cannot see: a tool behind no feature, like apply_patch. And the
// command takes no --ignore-user-config, so the enabled column is the user's
// config over Codex's defaults, while the child ignores that config. A feature
// the user turned on fails the check (with a reason that says so), and a new
// on-by-default feature the user turned off is missed. Both are stated in
// design section 7.

// featuresOn are the features on by default at rust-v0.164.0
// (features/src/lib.rs FEATURES at 6aaac69): the live lockdown checks ran
// with these on, so they count as reviewed. Any other feature that reports on
// is a change aggy has not reviewed, whether it is new or was off by default
// at that tag, which is how a new tool usually rolls out.
var featuresOn = []string{
	"api_key_cyber_access_programs", "api_key_model_discovery", "apps",
	"auth_elicitation", "bedrock_setup_wizard", "browser_annotation_api", "browser_use",
	"browser_use_external", "browser_use_full_cdp_access", "cli_daybreak",
	"code_mode_host", "code_mode_interrupt", "collaboration_modes",
	"compaction_image_budget", "computer_use", "content_item_kinds", "daemon_auto_start",
	"enable_request_compression", "fast_mode", "goals", "guardian_approval",
	"guardian_reuse_parent_compaction", "hooks", "image_generation", "in_app_browser",
	"in_app_chat", "in_app_dictation", "in_app_local_automation", "in_app_updates",
	"in_app_voice", "incremental_tools", "instant_interrupt", "item_ids",
	"mcp_oauth_refresh_coordination", "mentions_v2", "multi_agent", "plugin_sharing",
	"plugins", "realtime_conversation", "remote_plugin", "resize_all_images",
	"shell_snapshot", "shell_tool", "skill_mcp_dependency_install", "skill_search",
	"sleep_tool", "sqlite", "steer", "system_proxy_fallback", "terminal_resize_reflow",
	"tool_call_mcp_elicitation", "tool_search_always_defer_mcp_tools", "tool_suggest",
	"tui_app_server", "ultrafast_mode", "unbounded_connection_retries", "unified_exec",
	"unified_exec_tty", "view_image", "workspace_dependencies", "worktrees",
	"write_stdin_approval",
}

// FeatureCommand is codex features list.
func (Adapter) FeatureCommand() adapter.Command {
	return adapter.Command{Args: []string{"features", "list"}, Stdin: adapter.StdinNull}
}

// FeatureBaseline is a hash of the features reviewed as on, and of the
// lockdown's own.
func (Adapter) FeatureBaseline() string {
	sum := sha256.Sum256([]byte(strings.Join(featuresOn, ",") + "|" + strings.Join(Lockdown, ",")))
	return hex.EncodeToString(sum[:8])
}

// featureName is what a Codex feature key looks like (one has a dot).
var featureName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.]*$`)

// NewFeatures returns the features out reports as on that are not in
// featuresOn, sorted. A row aggy cannot read, an empty list, a failed
// command, or a feature the lockdown turns off that is gone from the list is
// an error: the list then says nothing either way. --strict-config would
// refuse such a --disable at run time anyway.
func (Adapter) NewFeatures(out adapter.Output) ([]string, error) {
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("codex features list exited %d", out.ExitCode)
	}
	listed := map[string]bool{}
	var on []string
	for _, line := range strings.Split(string(out.Stdout), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		last := f[len(f)-1]
		if len(f) < 3 || !featureName.MatchString(f[0]) || (last != "true" && last != "false") {
			return nil, errors.New("codex features list printed a row aggy cannot read")
		}
		listed[f[0]] = true
		if last == "true" && !slices.Contains(featuresOn, f[0]) {
			on = append(on, f[0])
		}
	}
	if len(listed) == 0 {
		return nil, errors.New("codex features list printed no features")
	}
	for i, a := range Lockdown {
		if a == "--disable" && !listed[Lockdown[i+1]] {
			return nil, fmt.Errorf("codex features list no longer names %s, which the lockdown turns off", Lockdown[i+1])
		}
	}
	slices.Sort(on)
	return on, nil
}

// UserConfigFile is the user's config.toml, which an edit child's sandbox
// could reach (projectOverride).
func (a Adapter) UserConfigFile() string {
	return filepath.Join(a.Home(), "config.toml")
}
