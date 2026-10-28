//go:build !unix

package safefs

import "io/fs"

// oNoFollow and oNonBlock are zero where the flags do not exist; the
// identity check on the opened descriptor still applies.
const (
	oNoFollow = 0
	oNonBlock = 0
)

// linkCount is 1 where the link count is not available.
func linkCount(fs.FileInfo) uint64 { return 1 }

// canWrite reports false where access(2) is not available.
func canWrite(string) bool { return false }

// FileID reports no identity where device and inode are not available.
func FileID(fs.FileInfo) (dev, ino uint64, ok bool) { return 0, 0, false }
