//go:build !(darwin || freebsd || netbsd)

package safefs

import "io/fs"

// FileBirth reports no birth time. Linux has one through statx(2), which
// the standard library does not expose and aggy does not reach around it
// (x/sys/unix and unsafe are banned), so callers must not rely on it.
func FileBirth(fs.FileInfo) (nsec int64, ok bool) { return 0, false }
