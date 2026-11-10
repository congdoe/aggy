package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// ErrNoIdentity matches a binary BinaryID cannot name: one with no device
// and inode on this OS. Its probe is never cached.
var ErrNoIdentity = errors.New("probe: the binary has no file identity here")

// BinaryID is the identity of the file at path (T6964): a hash of its
// resolved path, size, modification time, device, and inode, read through
// safefs. A binary replaced, updated, or moved to another path gets another.
func BinaryID(path string) (string, error) {
	real, err := safefs.RealPath(path)
	if err != nil {
		return "", err
	}
	fi, err := safefs.Stat(real)
	if err != nil {
		return "", err
	}
	dev, ino, ok := safefs.FileID(fi)
	if !ok {
		return "", ErrNoIdentity
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%d\x00%d", real, fi.Size(), fi.ModTime().UnixNano(), dev, ino))
	return hex.EncodeToString(sum[:16]), nil
}

// valuePrefix marks a stored pseudo-capability that is one of the probe's
// ValueFlags, and launchStamp the rows a launch's probe recorded with them, so
// a row a borrow's probe recorded (no value flags) or an older parse of them
// is not taken for one.
const (
	valuePrefix = "value:"
	launchStamp = "launch:1"
)

// Launch probes vendor's CLI at path for a launch (T6964): the version, the
// capabilities, and the flags its help shows with a value. When the store
// holds a probe of the same binary (BinaryID) made with the same spec, it
// returns that one (cached is true) and starts no process. Otherwise it probes
// in full and records the result under the binary's identity, unless a help
// run or the version failed or the binary changed while it ran. A version or
// help that failed leaves nothing recorded, so the next launch probes again.
// With no store, or a binary with no identity, it probes and records nothing.
func Launch(ctx context.Context, s *store.Store, run adapter.Runner, vendor adapter.Vendor, path string) (res adapter.Probe, cached bool, err error) {
	spec, ok := Specs[vendor]
	if !ok {
		return adapter.Probe{}, false, fmt.Errorf("probe: no spec for vendor %q", vendor)
	}
	stamp := specStamp(spec)
	id, idErr := BinaryID(path)
	if s != nil && idErr == nil {
		row, err := s.ProbeByBinary(ctx, string(vendor), id)
		switch {
		case err == nil && slices.Contains(row.Capabilities, stamp) && slices.Contains(row.Capabilities, launchStamp):
			res := adapter.Probe{Vendor: vendor, Path: path, Version: row.Version, ProbedAt: row.ProbedAt,
				Capabilities: unstamped(row.Capabilities), ValueFlags: valueFlags(row.Capabilities)}
			return res, true, nil
		}
		// Otherwise (none, another spec, or a read error), probe in full.
	}
	res = adapter.Probe{Vendor: vendor, Path: path, Capabilities: []string{}}
	version, verr := Version(ctx, run, path, spec.Programs)
	if verr != nil && !unreadable(verr) {
		return res, false, verr
	}
	var ve *VersionError
	if errors.As(verr, &ve) {
		res.VersionLine = ve.Line
	}
	res.Version = version
	hs, herr := helps(ctx, run, spec, path)
	res.Capabilities = Capabilities(spec, hs)
	if len(hs) > 0 {
		res.ValueFlags = ValueFlags(hs[0])
	}
	res.ProbedAt = time.Now().UTC().Truncate(time.Millisecond)
	if s == nil || idErr != nil || verr != nil || herr != nil {
		return res, false, nil
	}
	if after, err := BinaryID(path); err != nil || after != id {
		// Replaced while it ran: what was probed is not what id names.
		return res, false, nil
	}
	caps := append(slices.Clone(res.Capabilities), stamp, launchStamp)
	for _, f := range res.ValueFlags {
		caps = append(caps, valuePrefix+f)
	}
	if err := s.InsertProbe(ctx, store.Probe{Vendor: string(vendor), CLIPath: path, Version: version,
		Capabilities: caps, ProbedAt: res.ProbedAt, BinaryID: id}); err != nil {
		return res, false, fmt.Errorf("%w: %w", ErrNotRecorded, err)
	}
	return res, false, nil
}

// ErrNotRecorded matches a Launch whose probe ran and whose result is usable,
// but which the store could not record.
var ErrNotRecorded = errors.New("probe: not recorded")

// valueFlags returns the value flags stored among caps.
func valueFlags(caps []string) []string {
	var out []string
	for _, c := range caps {
		if f, ok := strings.CutPrefix(c, valuePrefix); ok {
			out = append(out, f)
		}
	}
	return out
}
