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

// myers returns the shortest edit script from a to b.
func myers(a, b []string) []op {
	n, m := len(a), len(b)
	max := n + m
	off := max + 1
	v := make([]int, 2*max+3)
	var trace [][]int
	var dEnd int
search:
	for d := 0; d <= max; d++ {
		trace = append(trace, append([]int(nil), v...))
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
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[off+prevK]
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
	ops := make([]op, len(rev))
	for i, o := range rev {
		ops[len(rev)-1-i] = o
	}
	return ops
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
