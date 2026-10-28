package borrow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
)

// taskKey is the task hash the redo rule compares (store.BorrowStats):
// what the borrow was asked and what it worked on. It covers the kind,
// the task and the constraints (normalized, constraints sorted), the
// cleaned paths, a review's base, diff, and files, an edit's base commit
// (editBase), and an image or video's --out, references, variant count,
// and resumed session. Only the hash is stored.
func taskKey(req Request, editBase string) string {
	h := sha256.New()
	// Each field is length-prefixed, so no two inputs run together.
	field := func(s string) { fmt.Fprintf(h, "%d:%s;", len(s), s) }
	list := func(xs []string, f func(string) string) {
		ys := make([]string, len(xs))
		for i, x := range xs {
			ys[i] = f(x)
		}
		sort.Strings(ys)
		field(strconv.Itoa(len(ys)))
		for _, y := range ys {
			field(y)
		}
	}
	field(string(req.Kind))
	field(norm(req.Task))
	list(req.Constraints, norm)
	list(req.Paths, filepath.Clean)
	if m := req.Material; m != nil {
		field(m.Base)
		field(m.Diff)
		field(strconv.FormatBool(m.DiffTruncated))
		field(m.Note)
		field(strconv.Itoa(len(m.Files)))
		for _, f := range m.Files {
			field(f.Path)
			field(f.Data)
			field(f.Note)
		}
	}
	field(editBase)
	if m := req.Media; m != nil {
		field(filepath.Clean(m.Out))
		list(m.Refs, filepath.Clean)
		field(strconv.Itoa(m.N))
		field(m.Resume)
	}
	return hex.EncodeToString(h.Sum(nil))
}
