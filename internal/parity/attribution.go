package parity

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
)

// Attribution trailers: the commit hook adds them, and a review reads
// them back to route away from the vendors that wrote the change.
const (
	TrailerVendor = "Aggy-Vendor"
	TrailerModel  = "Aggy-Model"
)

// AttributionHookName is the git hook aggy installs.
const AttributionHookName = "commit-msg"

// AttributionHook is the commit-msg hook. It is fixed text: nothing from
// the repo or the user is written into it. At commit time it adds the
// trailers only for a commit made under a CLI aggy runs (AGGY_VENDOR is
// set), and only values of 1 to 64 characters from [A-Za-z0-9._:-]; any
// other value is left out. A failure never blocks the commit.
const AttributionHook = `#!/bin/sh
# aggy attribution hook: adds Aggy-Vendor and Aggy-Model trailers to a
# commit made under a CLI aggy runs. Installed by aggy attribution wire;
# aggy unwire removes it.
ok() {
	case $1 in
	'' | *[!ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._:-]*) return 1 ;;
	esac
	[ ${#1} -le 64 ]
}
if ok "${AGGY_VENDOR-}"; then
	git interpret-trailers --in-place --if-exists addIfDifferent --trailer "Aggy-Vendor: $AGGY_VENDOR" "$1" || :
	if ok "${AGGY_MODEL-}"; then
		git interpret-trailers --in-place --if-exists addIfDifferent --trailer "Aggy-Model: $AGGY_MODEL" "$1" || :
	fi
fi
exit 0
`

// AttributionHookPatch returns the patch that creates the hook at path
// (<git common dir>/hooks/commit-msg), to apply through a manifest so
// aggy unwire removes it. It never replaces anything: a path already
// holding AttributionHook gives a patch that changes nothing, and any
// other file, link, or directory there is refused, as is a hooks folder
// that is a symlink.
func AttributionHookPatch(path string) (patch.Patch, error) {
	dir := filepath.Dir(path)
	if fi, err := safefs.Lstat(dir); err == nil && (fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir()) {
		return patch.Patch{}, fmt.Errorf("parity: %s is not a real directory", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return patch.Patch{}, err
	}
	h := patch.NoFile
	fi, err := safefs.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return patch.Patch{}, err
	case !fi.Mode().IsRegular():
		return patch.Patch{}, fmt.Errorf("parity: %s exists and is not aggy's hook; aggy never replaces a hook (chaining hooks is not supported yet)", path)
	default:
		b, err := safefs.ReadFileLimit(path, 64<<10)
		if err != nil && !errors.Is(err, safefs.ErrTooLarge) {
			return patch.Patch{}, err
		}
		if err != nil || !bytes.Equal(b, []byte(AttributionHook)) {
			return patch.Patch{}, fmt.Errorf("parity: %s exists and is not aggy's hook; aggy never replaces a hook (chaining hooks is not supported yet)", path)
		}
		h = patch.Hash(b)
	}
	return patch.Patch{File: path, ExpectedHash: h, Mode: 0o755, Transform: func(src []byte) ([]byte, error) {
		if src != nil && !bytes.Equal(src, []byte(AttributionHook)) {
			return nil, fmt.Errorf("parity: %s is not aggy's", path)
		}
		return []byte(AttributionHook), nil
	}}, nil
}
