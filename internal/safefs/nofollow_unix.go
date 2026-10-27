//go:build unix

package safefs

import "syscall"

const oNoFollow = syscall.O_NOFOLLOW
