package diff

import (
	"flag"
	"math/rand"
	"os"
	"path/filepath"
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

// apply rebuilds b from a and a unified diff, to check any diff is correct.
func apply(t *testing.T, a, d string) string {
	t.Helper()
	al := lines([]byte(a))
	var out []string
	pos := 0
	dl := strings.SplitAfter(d, "\n")
	for i := 2; i < len(dl); i++ {
		l := dl[i]
		if l == "" {
			continue
		}
		switch l[0] {
		case '@':
			var as, ac, bs, bc int
			parseHunk(t, l, &as, &ac, &bs, &bc)
			start := as - 1
			if ac == 0 {
				start = as
			}
			out = append(out, al[pos:start]...)
			pos = start
		case ' ', '-', '+':
			text := l[1:]
			if i+1 < len(dl) && strings.HasPrefix(dl[i+1], "\\ No newline") {
				text = strings.TrimSuffix(text, "\n")
				i++
			}
			if l[0] != '+' {
				if al[pos] != text {
					t.Fatalf("context mismatch at %d: %q vs %q", pos, al[pos], text)
				}
				pos++
			}
			if l[0] != '-' {
				out = append(out, text)
			}
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
	gen := func() string {
		var s strings.Builder
		for range r.Intn(30) {
			s.WriteString(words[r.Intn(len(words))])
		}
		if r.Intn(4) == 0 {
			s.WriteString("tail")
		}
		return s.String()
	}
	for i := range 2000 {
		a, b := gen(), gen()
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
