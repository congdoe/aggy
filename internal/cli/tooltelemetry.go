package cli

import (
	"path/filepath"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/tooldef"
)

// telemetryOff is what aggy sets in every process of t's it starts (the
// shim's server, a hook, a prebuild): DO_NOT_TRACK=1 and t's own
// telemetry_env, over the person's own values (T6822). The user config
// in state ("" is the state dir) can allow telemetry with [tools]
// allow_telemetry; a repo config cannot. A config that cannot be read
// sets them all the same.
func telemetryOff(state string, t tooldef.Tool) []string {
	off := append([]string{execrun.DoNotTrack}, t.TelemetryEnv...)
	if state == "" {
		var err error
		if state, err = home.Dir(); err != nil {
			return off
		}
	}
	cfg, err := config.Load(filepath.Join(state, config.FileName))
	if err != nil || !cfg.Tools.AllowTelemetry {
		return off
	}
	return nil
}
