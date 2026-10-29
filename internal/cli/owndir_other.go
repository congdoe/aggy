//go:build !unix

package cli

import "io/fs"

// ownPrivateDir cannot check owners here, so no folder passes until the
// Windows port.
func ownPrivateDir(fs.FileInfo) bool { return false }
