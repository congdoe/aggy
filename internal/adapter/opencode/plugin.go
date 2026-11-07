package opencode

import "embed"

// PluginFiles is the shipped plugin, written to the launch's folder in
// directory form: a plugin given as a file path silently never loads
// (fake model, T6930). The format is schema/src/config/plugin.ts and
// packages/plugin/src/promise at v2.2.5; a package.json export named
// ./server loaded from a per-launch config with project config off.
//
//go:embed plugin/package.json plugin/server.js
var pluginFS embed.FS

// PluginFile is one file of the plugin.
type PluginFile struct {
	Name string
	Data []byte
}

// Plugin returns the plugin's files.
func Plugin() ([]PluginFile, error) {
	out := make([]PluginFile, 0, 2)
	for _, n := range []string{"package.json", "server.js"} {
		b, err := pluginFS.ReadFile("plugin/" + n)
		if err != nil {
			return nil, err
		}
		out = append(out, PluginFile{Name: n, Data: b})
	}
	return out, nil
}
