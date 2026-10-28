package cli

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
)

// maxCodexConfig caps the Codex config files read to merge
// developer_instructions.
const maxCodexConfig = 1 << 20

// memoryDelivery says how vendor v takes the shared memory, given its
// probed capabilities: ok, or why not. Claude's flag is live-verified
// but missing from --help, so it needs no capability.
func memoryDelivery(v adapter.Vendor, caps []string) (bool, string) {
	switch v {
	case adapter.Codex:
		if !slices.Contains(caps, probe.CapConfigOverride) {
			return false, "codex --help does not list -c"
		}
	case adapter.Grok:
		if !slices.Contains(caps, probe.CapRules) {
			return false, "grok --help does not list --rules"
		}
	}
	return true, ""
}

// addMemory puts the repo's shared memory on req for vendor v. For
// Claude it writes the framed text to a private temp file; cleanup
// removes it after the session. Warnings say what was not delivered and
// why; none of them stops the launch.
func addMemory(v adapter.Vendor, caps []string, root string, req *adapter.InteractiveRequest) (cleanup func(), warns []string) {
	cleanup = func() {}
	text, err := parity.LoadMemory(root)
	if err != nil {
		return cleanup, []string{"shared memory not delivered: " + err.Error()}
	}
	if text == "" {
		return cleanup, nil
	}
	if ok, why := memoryDelivery(v, caps); !ok {
		return cleanup, []string{"shared memory not delivered: " + why}
	}
	framed := parity.FrameMemory(text)
	switch v {
	case adapter.Claude:
		dir, err := safefs.MkdirTemp("", "aggy-memory-")
		if err != nil {
			return cleanup, []string{"shared memory not delivered: " + err.Error()}
		}
		cleanup = func() { _ = safefs.RemoveAll(dir) }
		f := filepath.Join(dir, "memory.md")
		if err := safefs.WriteFile(f, []byte(framed), 0o600); err != nil {
			cleanup()
			return func() {}, []string{"shared memory not delivered: " + err.Error()}
		}
		req.MemoryFile = f
	case adapter.Codex:
		user, project, err := codexConfigs(root)
		if err == nil {
			framed, err = parity.CodexInstructions(user, project, framed)
		}
		if err != nil {
			return cleanup, []string{"shared memory not delivered: " + err.Error()}
		}
		req.Memory = framed
	default:
		req.Memory = framed
	}
	return cleanup, nil
}

// codexConfigs reads Codex's user config and the repo's project config
// (<root>/.codex/config.toml), nil when absent.
func codexConfigs(root string) (user, project []byte, err error) {
	path, err := parity.CodexConfigPath()
	if err != nil {
		return nil, nil, err
	}
	read := func(p string) ([]byte, error) {
		b, err := safefs.ReadFileLimit(p, maxCodexConfig)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return b, err
	}
	if user, err = read(path); err != nil {
		return nil, nil, err
	}
	project, err = read(filepath.Join(root, ".codex", "config.toml"))
	return user, project, err
}
