// Package diff writes line-based unified diffs, the format of diff -u, for
// aggy's --dry-run previews. It uses the Myers algorithm and only the
// standard library.
package diff

import (
	"fmt"
	"strings"
)

// Context is the number of unchanged lines shown around each change.
const Context = 3

// DevNull is the label for a side that does not exist, as in a created or
// deleted file.
const DevNull = "/dev/null"

type opKind byte

const (
	opEq  opKind = ' '
	opDel opKind = '-'
	opIns opKind = '+'
)

type op struct {
	kind opKind
	a, b int // line index in a (eq, del) and b (eq, ins)
}

// Unified returns the unified diff from a to b, labeled aName and bName,
// with Context lines of context. It returns "" when a and b are equal.
func Unified(aName, bName string, a, b []byte) string {
	if string(a) == string(b) {
		return ""
	}
	al, bl := lines(a), lines(b)
	ops := myers(al, bl)
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", aName, bName)
	for _, h := range hunks(ops) {
		writeHunk(&out, h, al, bl)
	}
	return out.String()
}

// lines splits s into lines, each keeping its "\n". A last line without
// one is kept as is, so "a" and "a\n" differ.
func lines(s []byte) []string {
	var ls []string
	str := string(s)
	for str != "" {
		i := strings.IndexByte(str, '\n')
		if i < 0 {
			ls = append(ls, str)
			break
		}
		ls = append(ls, str[:i+1])
		str = str[i+1:]
	}
	return ls
}

// MaxEdits caps the edit distance the Myers search explores. Its trace
// keeps one snapshot per step d of width 2d+1, so memory is O(MaxEdits²)
// ints, about 8 MB, whatever the input size. Past the cap the diff falls
// back to deleting every differing line and inserting the new ones: still
// a correct diff, only not the shortest.
const MaxEdits = 1000

// myers returns an edit script from a to b: the shortest one when the
// files differ by at most MaxEdits lines, after trimming the common prefix
// and suffix.
func myers(a, b []string) []op {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []op
	for i := 0; i < pre; i++ {
		ops = append(ops, op{opEq, i, i})
	}
	mid, ok := myersCore(a[pre:len(a)-suf], b[pre:len(b)-suf], MaxEdits)
	if !ok {
		mid = nil
		for i := range len(a) - pre - suf {
			mid = append(mid, op{opDel, i, 0})
		}
		for j := range len(b) - pre - suf {
			mid = append(mid, op{opIns, len(a) - pre - suf, j})
		}
	}
	for _, o := range mid {
		o.a += pre
		o.b += pre
		ops = append(ops, o)
	}
	for i := 0; i < suf; i++ {
		ops = append(ops, op{opEq, len(a) - suf + i, len(b) - suf + i})
	}
	return ops
}

// myersCore is the Myers O(ND) search with a trace of only the diagonals
// each step can reach. It gives up (ok false) past maxD edits.
func myersCore(a, b []string, maxD int) (ops []op, ok bool) {
	n, m := len(a), len(b)
	max := n + m
	off := max + 1
	v := make([]int, 2*max+3)
	// trace[d][k+d] is v[k] at the start of step d, for k in [-d, d].
	var trace [][]int
	dEnd := -1
search:
	for d := 0; d <= max; d++ {
		if d > maxD {
			return nil, false
		}
		trace = append(trace, append([]int(nil), v[off-d:off+d+1]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				dEnd = d
				break search
			}
		}
	}
	// Walk back through the trace, collecting edits in reverse.
	var rev []op
	x, y := n, m
	for d := dEnd; d > 0; d-- {
		snap := trace[d]
		at := func(k int) int { return snap[k+d] }
		k := x - y
		var prevK int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, op{opEq, x, y})
		}
		if x == prevX {
			y--
			rev = append(rev, op{opIns, x, y})
		} else {
			x--
			rev = append(rev, op{opDel, x, y})
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		rev = append(rev, op{opEq, x, y})
	}
	ops = make([]op, len(rev))
	for i, o := range rev {
		ops[len(rev)-1-i] = o
	}
	return ops, true
}

// hunks splits ops into runs of changes with up to Context lines of
// context on each side, merging changes closer than 2*Context lines.
func hunks(ops []op) [][]op {
	var out [][]op
	i := 0
	for i < len(ops) {
		// Find the next change.
		for i < len(ops) && ops[i].kind == opEq {
			i++
		}
		if i == len(ops) {
			break
		}
		start := max(i-Context, 0)
		// Extend while the gap of equal lines to the next change is short.
		end := i
		for {
			for end < len(ops) && ops[end].kind != opEq {
				end++
			}
			gap := end
			for gap < len(ops) && ops[gap].kind == opEq {
				gap++
			}
			if gap < len(ops) && gap-end <= 2*Context {
				end = gap
				continue
			}
			end = min(end+Context, len(ops))
			break
		}
		out = append(out, ops[start:end])
		i = end
	}
	return out
}

func writeHunk(out *strings.Builder, h []op, a, b []string) {
	var aStart, bStart, aCount, bCount int
	aStart, bStart = -1, -1
	for _, o := range h {
		if o.kind != opIns {
			if aStart < 0 {
				aStart = o.a
			}
			aCount++
		}
		if o.kind != opDel {
			if bStart < 0 {
				bStart = o.b
			}
			bCount++
		}
	}
	// With no lines on a side, diff -u names the line before the hunk.
	if aStart < 0 {
		aStart = h[0].a - 1
	}
	if bStart < 0 {
		bStart = h[0].b - 1
	}
	fmt.Fprintf(out, "@@ -%s +%s @@\n", rangeOf(aStart, aCount), rangeOf(bStart, bCount))
	for _, o := range h {
		line := ""
		switch o.kind {
		case opEq, opDel:
			line = a[o.a]
		case opIns:
			line = b[o.b]
		}
		out.WriteByte(byte(o.kind))
		out.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			out.WriteString("\n\\ No newline at end of file\n")
		}
	}
}

// rangeOf formats a 0-based start and a count as diff -u does: 1-based,
// with the count left out when it is 1.
func rangeOf(start, count int) string {
	if count == 1 {
		return fmt.Sprint(start + 1)
	}
	return fmt.Sprintf("%d,%d", start+1, count)
}
