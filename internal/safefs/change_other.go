//go:build !(darwin || freebsd || netbsd || linux)

package safefs

import "io/fs"

// FileChange reports no change time on this OS, so callers must not rely on
// it.
func FileChange(fs.FileInfo) (nsec int64, ok bool) { return 0, false }
