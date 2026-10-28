//go:build darwin

package safefs

import "syscall"

// IsAPFS reports whether name is on APFS, read from statfs (metadata
// only, after the path check). APFS never hands out an inode number
// again; exFAT, msdos, and SMB do. Any failure is false.
func IsAPFS(name string) bool {
	t, err := resolve("IsAPFS", name)
	if err != nil {
		return false
	}
	var st syscall.Statfs_t
	if syscall.Statfs(t.real, &st) != nil {
		return false
	}
	var b []byte
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b) == "apfs"
}
