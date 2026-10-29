//go:build !(linux || darwin)

package safefs

import "os"

// renameNoReplaceAt renames from to to in dir after RenameNoReplace has
// checked that nothing is at to. shortcut: check then rename, so a name
// made in between can be replaced; upgrade if the platform gains a
// no-replace rename call.
func renameNoReplaceAt(dir *os.Root, from, to string, _ bool) error {
	return dir.Rename(from, to)
}
