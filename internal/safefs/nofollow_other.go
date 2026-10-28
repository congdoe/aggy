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

// fileID reports no identity where device and inode are not available;
// the identity history then has nothing to record.
func fileID(fs.FileInfo) (id, bool) { return id{}, false }
