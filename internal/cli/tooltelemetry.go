package cli

import (
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/tooldef"
)

// telemetrySets says what aggy sets in the processes of d's it starts
// (#97 HIGH-2), or "" for a tool that runs none (a skill).
func telemetrySets(d tooldef.Def) string {
	if len(d.MCP) == 0 && len(d.Hooks) == 0 && len(d.Install.Prebuild) == 0 {
		return ""
	}
	return "aggy sets in every process it starts for this tool (unless [tools] allow_telemetry = true): " +
		strings.Join(append([]string{execrun.DoNotTrack}, d.TelemetryEnv...), ", ")
}

// toolTelemetry is the telemetry part of the environment of every
// process of t's that aggy starts (the shim's server, a hook, a
// prebuild), T6822. By default set is DO_NOT_TRACK=1 and t's own
// telemetry_env (a built-in's), put over the person's own values. With
// [tools] allow_telemetry in the user config in state ("" is the state
// dir; a repo config has no such key), set is empty and pass names
// DO_NOT_TRACK, so the person's own value still reaches the tool. A
// config that cannot be read keeps the default.
func toolTelemetry(state string, t tooldef.Tool) (pass, set []string) {
	off := append([]string{execrun.DoNotTrack}, t.TelemetryEnv...)
	if state == "" {
		var err error
		if state, err = home.Dir(); err != nil {
			return nil, off
		}
	}
	cfg, err := config.Load(filepath.Join(state, config.FileName))
	if err != nil || !cfg.Tools.AllowTelemetry {
		return nil, off
	}
	return []string{"DO_NOT_TRACK"}, nil
}
