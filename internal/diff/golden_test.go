package diff

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var cases = []struct{ name, a, b string }{
	{"modify", "a\nb\nc\nd\ne\nf\ng\n", "a\nb\nc\nD\ne\nf\ng\n"},
	{"append", "model = \"o\"\n", "model = \"o\"\nproject_doc_fallback_filenames = [\"CLAUDE.md\"]\n"},
	{"create", "", "one\ntwo\n"},
	{"delete_all", "one\ntwo\n", ""},
	{"no_newline_at_end", "a\nb", "a\nb\n"},
	{"two_hunks", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n", "1\nX\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\nY\n15\n"},
	{"merged_hunk", "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "1\nX\n3\n4\n5\n6\n7\nY\n9\n"},
	{"insert_at_start", "b\nc\n", "a\nb\nc\n"},
	{"equal", "same\n", "same\n"},
}

func TestGolden(t *testing.T) {
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			aName, bName := "a/config.toml", "b/config.toml"
			if tt.a == "" {
				aName = DevNull
			}
			if tt.b == "" {
				bName = DevNull
			}
			got := Unified(aName, bName, []byte(tt.a), []byte(tt.b))
			path := filepath.Join("testdata", tt.name+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("diff mismatch\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// splitLines is an independent line splitter for the checker: it keeps
// each "\n", and a last line without one.
func splitLines(s string) []string {
	ls := strings.SplitAfter(s, "\n")
	if ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}

// apply rebuilds b from a and a unified diff, trusting only the @@ counts:
// each hunk must hold exactly as many old and new lines as its header
// says, start where it says, and match a's lines.
func apply(t *testing.T, a, d string) string {
	t.Helper()
	al := splitLines(a)
	dl := splitLines(d)
	if len(dl) < 2 || !strings.HasPrefix(dl[0], "--- ") || !strings.HasPrefix(dl[1], "+++ ") {
		t.Fatalf("bad header: %q", d)
	}
	var out []string
	pos := 0
	i := 2
	for i < len(dl) {
		var as, ac, bs, bc int
		if !strings.HasPrefix(dl[i], "@@ ") {
			t.Fatalf("want a hunk header, got %q", dl[i])
		}
		parseHunk(t, dl[i], &as, &ac, &bs, &bc)
		i++
		start := as - 1
		if ac == 0 {
			start = as
		}
		if start < pos {
			t.Fatalf("hunk at %d overlaps the previous one (at %d)", start, pos)
		}
		// The new side starts where the old one does, shifted by the
		// earlier hunks; with no new lines, diff -u names the line before.
		wantBS := len(out) + (start - pos)
		if bc > 0 {
			wantBS++
		}
		if bs != wantBS {
			t.Fatalf("hunk new start %d, want %d", bs, wantBS)
		}
		out = append(out, al[pos:start]...)
		pos = start
		oldN, newN := 0, 0
		for oldN < ac || newN < bc {
			if i >= len(dl) {
				t.Fatalf("hunk ends early: %d/%d old, %d/%d new", oldN, ac, newN, bc)
			}
			l := dl[i]
			i++
			text := l[1:]
			if i < len(dl) && strings.HasPrefix(dl[i], "\\ No newline") {
				text = strings.TrimSuffix(text, "\n")
				i++
			}
			switch l[0] {
			case ' ', '-':
				if pos >= len(al) || al[pos] != text {
					t.Fatalf("old line %d is not %q", pos+1, text)
				}
				pos++
				oldN++
				if l[0] == ' ' {
					out = append(out, text)
					newN++
				}
			case '+':
				out = append(out, text)
				newN++
			default:
				t.Fatalf("bad hunk line %q", l)
			}
		}
		if oldN != ac || newN != bc {
			t.Fatalf("hunk counts %d,%d, header says %d,%d", oldN, newN, ac, bc)
		}
	}
	out = append(out, al[pos:]...)
	return strings.Join(out, "")
}

func parseHunk(t *testing.T, l string, as, ac, bs, bc *int) {
	t.Helper()
	f := strings.Fields(l)
	parse := func(s string, start, count *int) {
		s = s[1:]
		*count = 1
		if p, c, ok := strings.Cut(s, ","); ok {
			s = p
			*count = atoi(t, c)
		}
		*start = atoi(t, s)
	}
	parse(f[1], as, ac)
	parse(f[2], bs, bc)
}

func atoi(t *testing.T, s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("bad number %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	words := []string{"a\n", "b\n", "c\n", "d\n", "x = 1\n", "\n"}
	gen := func(base []string) string {
		var s strings.Builder
		if base != nil && r.Intn(2) == 0 {
			// Long runs of equal lines with sparse edits, so hunks split
			// and merge around the context limit.
			for _, l := range base {
				switch r.Intn(12) {
				case 0:
				case 1:
					s.WriteString(words[r.Intn(len(words))])
					s.WriteString(l)
				case 2:
					s.WriteString(words[r.Intn(len(words))])
				default:
					s.WriteString(l)
				}
			}
			return s.String()
		}
		for range r.Intn(30) {
			s.WriteString(words[r.Intn(len(words))])
		}
		if r.Intn(4) == 0 {
			s.WriteString("tail")
		}
		return s.String()
	}
	long := func() []string {
		var ls []string
		for i := range 40 + r.Intn(80) {
			ls = append(ls, fmt.Sprintf("line %d\n", i))
		}
		return ls
	}
	for i := range 3000 {
		var a, b string
		if i%2 == 0 {
			base := long()
			a = strings.Join(base, "")
			b = gen(base)
		} else {
			a, b = gen(nil), gen(nil)
		}
		d := Unified("a", "b", []byte(a), []byte(b))
		if a == b {
			if d != "" {
				t.Fatalf("case %d: diff of equal inputs: %q", i, d)
			}
			continue
		}
		if got := apply(t, a, d); got != b {
			t.Fatalf("case %d: applying the diff gave %q, want %q\na=%q\ndiff:\n%s", i, got, b, a, d)
		}
	}
}

// TestLargeDiffMemory (C4): a whole-file change of 20,000 lines stays
// within a fixed memory budget and is still a correct diff.
func TestLargeDiffMemory(t *testing.T) {
	var a, b strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&a, "key_%d = %d\n", i, i)
		fmt.Fprintf(&b, "    key_%d = %d\n", i, i) // re-indented: every line differs
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	d := Unified("a", "b", []byte(a.String()), []byte(b.String()))
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Errorf("diff allocated %d MB, want under 64", alloc>>20)
	}
	if got := apply(t, a.String(), d); got != b.String() {
		t.Error("the large diff does not rebuild the target")
	}
	// Small edits in a large file still get the shortest diff.
	c := strings.Replace(a.String(), "key_100 = 100\n", "key_100 = 101\n", 1)
	if d := Unified("a", "b", []byte(a.String()), []byte(c)); strings.Count(d, "\n-") != 1 || strings.Count(d, "\n+") != 2 {
		t.Errorf("one-line change gave:\n%s", d)
	}
}
