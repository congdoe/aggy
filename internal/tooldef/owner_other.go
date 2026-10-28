//go:build !unix

package tooldef

import "io/fs"

// notMine cannot check owners here, so no user definition is trusted
// until the Windows port.
func notMine(fs.FileInfo) string { return "user tools are not supported on this OS yet" }
