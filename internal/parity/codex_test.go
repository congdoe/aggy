package parity

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/patch"
)

// codexCases are realistic Codex configs: comments, CRLF, no trailing
// newline, tables, and the key in several shapes.
var codexCases = []struct {
	name string
	in   []byte // nil: the file does not exist
}{
	{"codex_missing", nil},
	{"codex_empty", []byte("")},
	{"codex_comments_crlf", []byte("# Codex settings\r\n# managed by hand\r\n\r\nmodel = \"gpt-5\"\r\napproval_policy = \"on-request\"\r\n\r\n[mcp_servers.docs]\r\ncommand = \"npx\"\r\nargs = [\"-y\", \"docs-mcp\"]\r\n")},
	{"codex_no_trailing_newline", []byte("model = \"o3\"\n[tui]\nnotifications = true")},
	{"codex_list", []byte("model = \"o\"\nproject_doc_fallback_filenames = [\"AGENTS.override.md\"] # mine\n")},
	{"codex_empty_list", []byte("project_doc_fallback_filenames = []\n")},
	{"codex_multiline_list", []byte("project_doc_fallback_filenames = [\n  \"GUIDE.md\", # team guide\n  'NOTES.md',\n]\n\n[profiles.fast]\nmodel = \"o4-mini\"\n")},
	{"codex_multiline_no_comma", []byte("project_doc_fallback_filenames = [\r\n  \"GUIDE.md\"\r\n]\r\n")},
	{"codex_key_only_in_table", []byte("# top comment\nmodel = \"o\"\n\n[profiles.x]\nproject_doc_fallback_filenames = [\"X.md\"]\n")},
	{"codex_bom", []byte("\xef\xbb\xbfmodel = \"o\"\n")},
	{"codex_bom_list", []byte("\xef\xbb\xbfproject_doc_fallback_filenames = [\"A.md\"]\r\nmodel = \"o\"\r\n")},
	{"codex_bom_header", []byte("\xef\xbb\xbf# Codex\n\nmodel = \"o\"\n")},
	{"codex_list_after_tricky", []byte("note = \"project_doc_fallback_filenames = [\\\"x\\\"]\"\nmulti = \"\"\"\nproject_doc_fallback_filenames = [\n\"\"\"\nproject_doc_fallback_filenames=[ 'A.md' , \"B.md\" ] # x = [\n")},
	{"codex_tricky_strings", []byte("note = \"project_doc_fallback_filenames = [\\\"x\\\"]\"\nmulti = '''\nproject_doc_fallback_filenames = []\n'''\n[t]\nk = 1\n")},
}

// TestCodexFallbackGolden: the edit for every case, as a diff.
func TestCodexFallbackGolden(t *testing.T) {
	for _, tc := range codexCases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := AddClaudeFallback(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tc.name, tc.in, out)
			again, err := AddClaudeFallback(out)
			if err != nil || !bytes.Equal(again, out) {
				t.Errorf("not idempotent: %q, %v", again, err)
			}
			if bytes.HasPrefix(tc.in, bom) && !bytes.HasPrefix(out, bom) {
				t.Errorf("byte order mark lost: %q", out)
			}
			if bytes.Contains(tc.in, []byte("\r\n")) && bytes.Count(out, []byte("\n")) != bytes.Count(out, []byte("\r\n")) {
				t.Errorf("line endings mixed: %q", out)
			}
		})
	}
}

func TestCodexFallbackUnchanged(t *testing.T) {
	for _, in := range []string{
		"project_doc_fallback_filenames = [\"CLAUDE.md\"]\n",
		"# x\r\nproject_doc_fallback_filenames = [\"A.md\", \"CLAUDE.md\"]",
	} {
		out, err := AddClaudeFallback([]byte(in))
		if err != nil || string(out) != in {
			t.Errorf("AddClaudeFallback(%q) = %q, %v, want it unchanged", in, out, err)
		}
	}
}

func TestCodexFallbackRefuses(t *testing.T) {
	for _, in := range []string{
		"model = \n", // not TOML
		"project_doc_fallback_filenames = \"CLAUDE\"\n",     // not a list
		"project_doc_fallback_filenames = [1, 2]\n",         // not strings
		"\"project_doc_fallback_filenames\" = [\"A.md\"]\n", // quoted key: not edited blind
	} {
		if out, err := AddClaudeFallback([]byte(in)); err == nil {
			t.Errorf("AddClaudeFallback(%q) = %q, want an error", in, out)
		}
	}
}

func TestCodexConfigPath(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("CODEX_HOME", "")
	if p, err := CodexConfigPath(); err != nil || p != filepath.Join(h, ".codex", "config.toml") {
		t.Errorf("default = %q, %v", p, err)
	}
	c := t.TempDir()
	t.Setenv("CODEX_HOME", c)
	if p, err := CodexConfigPath(); err != nil || p != filepath.Join(c, "config.toml") {
		t.Errorf("CODEX_HOME = %q, %v", p, err)
	}
	t.Setenv("CODEX_HOME", "rel")
	if _, err := CodexConfigPath(); err == nil {
		t.Error("a relative CODEX_HOME was accepted")
	}
}

func TestCodexFallbackPatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := "model = \"o\"\n"
	if err := os.WriteFile(path, []byte(orig), 0o640); err != nil {
		t.Fatal(err)
	}
	p, err := CodexFallbackPatch(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.ExpectedHash != patch.Hash([]byte(orig)) {
		t.Errorf("ExpectedHash = %s", p.ExpectedHash)
	}
	rec, err := patch.Apply(p, filepath.Join(t.TempDir(), "bk"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), FallbackKey+" = [\"CLAUDE.md\"]\n") || !strings.HasSuffix(string(b), orig) {
		t.Errorf("patched = %q", b)
	}
	if err := patch.Revert(rec); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != orig {
		t.Errorf("reverted = %q", b)
	}
}
