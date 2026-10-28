// Package transcript reads the tail of a vendor CLI's session file: the
// latest turns of a Claude, Codex, or Grok session, for a handoff brief.
//
// Session files are vendor formats, and their text is untrusted: a model,
// a web page, or a tool wrote it. This package only locates and parses
// them. It reads through internal/safefs (which refuses credential files),
// reads at most MaxTail bytes from the end of the file, and refuses any
// symlink between the vendor's session folder and the file. Anything it
// cannot locate or recognize is adapter.ErrNoTranscript, and the brief
// then degrades to the task file alone.
//
// Formats and their sources (docs/adapters.md, "Transcript formats"):
//
//   - Codex: source-verified at rust-v0.164.0.
//   - Grok: source-verified at grok-build HEAD 2bdd1d6a, which may be newer
//     than the tested grok 1.0.70.
//   - Claude: user lines are read the way Grok's own Claude importer reads
//     them (grok-build 2bdd1d6a); assistant lines and the folder for a
//     working dir are unverified.
package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/safefs"
)

// MaxTail is how many bytes are read from the end of a session file. A
// line cut at the start of the window is dropped.
const MaxTail = 4 << 20

// ErrUnsafe means a path between the session folder and the session file
// is a symlink or not the kind of file it should be. Nothing is read.
var ErrUnsafe = errors.New("session path is a symlink or not a regular file")

// homeVars are the variables that relocate each vendor's config dir.
var homeVars = map[adapter.Vendor]string{
	adapter.Claude: "CLAUDE_CONFIG_DIR",
	adapter.Codex:  "CODEX_HOME",
	adapter.Grok:   "GROK_HOME",
}

// Home returns vendor v's config dir: its relocation variable, else
// $HOME/.<vendor>. It must be absolute.
func Home(v adapter.Vendor, getenv func(string) string) (string, error) {
	h := getenv(homeVars[v])
	if h == "" {
		if home := getenv("HOME"); home != "" {
			h = filepath.Join(home, "."+string(v))
		}
	}
	if h == "" || !filepath.IsAbs(h) {
		return "", fmt.Errorf("transcript: cannot locate the %s config dir", v)
	}
	return filepath.Clean(h), nil
}

// Tail returns up to n of the latest turns (all in the tail when n <= 0)
// of session ref, read from vendor v's config dir home.
func Tail(v adapter.Vendor, home string, ref adapter.SessionRef, n int) (adapter.Transcript, error) {
	if err := adapter.ValidSessionID(v, ref.ID); err != nil {
		return adapter.Transcript{}, err
	}
	if !filepath.IsAbs(home) {
		return adapter.Transcript{}, fmt.Errorf("transcript: %s config dir is not absolute", v)
	}
	root, path, err := locate(v, filepath.Clean(home), ref)
	if err != nil {
		return adapter.Transcript{}, err
	}
	data, err := readTail(root, path)
	if err != nil {
		return adapter.Transcript{}, err
	}
	turns, ok := parse(v, data)
	if !ok {
		return adapter.Transcript{}, fmt.Errorf("%w: %s session file format not recognized", adapter.ErrNoTranscript, v)
	}
	if n > 0 && len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	return adapter.Transcript{Path: path, Turns: turns}, nil
}

// locate returns the session folder and the session file in it.
func locate(v adapter.Vendor, home string, ref adapter.SessionRef) (root, path string, err error) {
	switch v {
	case adapter.Claude:
		// projects/<cwd with every non-alphanumeric as "-">/<id>.jsonl
		// (grok-build xai-grok-foreign-sessions/src/claude/projects.rs#L44).
		root = filepath.Join(home, "projects")
		file := ref.ID + ".jsonl"
		if ref.Cwd != "" {
			if dir := ClaudeProjectDir(ref.Cwd); len(dir) <= 200 {
				if p := filepath.Join(root, dir, file); isFile(p) {
					return root, p, nil
				}
			}
		}
		path, err = only(v, root, func(d string) []string { return []string{filepath.Join(root, d, file)} })
	case adapter.Codex:
		// sessions/YYYY/MM/DD/rollout-YYYY-MM-DDThh-mm-ss-<id>.jsonl
		// (codex-rs/rollout/src/list.rs#L423).
		root = filepath.Join(home, "sessions")
		path, err = codexFile(root, ref.ID)
	case adapter.Grok:
		// sessions/<encoded cwd>/<id>/chat_history.jsonl (grok-build
		// docs/user-guide/17-sessions.md, "Storage Layout").
		root = filepath.Join(home, "sessions")
		if ref.Cwd != "" {
			if p := filepath.Join(root, grok.EncodeCwd(ref.Cwd), ref.ID, "chat_history.jsonl"); isFile(p) {
				return root, p, nil
			}
		}
		path, err = only(v, root, func(d string) []string {
			return []string{filepath.Join(root, d, ref.ID, "chat_history.jsonl")}
		})
	default:
		err = fmt.Errorf("transcript: unknown vendor %q", v)
	}
	return root, path, err
}

// ClaudeProjectDir is the folder name Claude gives a working dir under
// projects/: every character that is not an ASCII letter or digit becomes
// "-". Taken from grok-build's Claude importer; unverified against Claude.
func ClaudeProjectDir(cwd string) string {
	b := []byte(cwd)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

func isFile(p string) bool {
	fi, err := safefs.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

// only returns the one existing regular file among the candidates each
// subfolder of root names. None is ErrNoTranscript; several are an error,
// never a guess.
func only(v adapter.Vendor, root string, cands func(dir string) []string) (string, error) {
	dirs, err := adapter.SubDirs(root)
	if err != nil {
		return "", err
	}
	var found []string
	for _, d := range dirs {
		for _, p := range cands(d) {
			if isFile(p) {
				found = append(found, p)
			}
		}
	}
	return one(v, found)
}

func one(v adapter.Vendor, found []string) (string, error) {
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w: no %s session file for this id", adapter.ErrNoTranscript, v)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("transcript: %d %s session files share this id", len(found), v)
}

// codexFile finds rollout-*-<id>.jsonl three date folders below root.
func codexFile(root, id string) (string, error) {
	var found []string
	years, err := adapter.SubDirs(root)
	if err != nil {
		return "", err
	}
	for _, y := range years {
		months, err := adapter.SubDirs(filepath.Join(root, y))
		if err != nil {
			return "", err
		}
		for _, m := range months {
			days, err := adapter.SubDirs(filepath.Join(root, y, m))
			if err != nil {
				return "", err
			}
			for _, d := range days {
				dir := filepath.Join(root, y, m, d)
				entries, err := safefs.ReadDir(dir)
				if err != nil {
					return "", err
				}
				for _, e := range entries {
					name := e.Name()
					if e.Type().IsRegular() && strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, "-"+id+".jsonl") {
						found = append(found, filepath.Join(dir, name))
					}
				}
			}
		}
	}
	return one(adapter.Codex, found)
}

// readTail reads the last MaxTail bytes of path, which must be a regular
// file under root reached through real directories only. The file opened
// must be the one checked, so a folder swapped for a symlink after the
// check is refused.
func readTail(root, path string) ([]byte, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("transcript: %s is outside %s", path, root)
	}
	var last fs.FileInfo
	p := root
	parts := append([]string{""}, strings.Split(rel, string(filepath.Separator))...)
	for i, part := range parts {
		p = filepath.Join(p, part)
		fi, err := safefs.Lstat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%w: %v", adapter.ErrNoTranscript, err)
			}
			return nil, err
		}
		final := i == len(parts)-1
		if final && !fi.Mode().IsRegular() || !final && !fi.IsDir() {
			return nil, &fs.PathError{Op: "transcript", Path: p, Err: ErrUnsafe}
		}
		last = fi
	}
	f, err := safefs.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(fi, last) {
		return nil, &fs.PathError{Op: "transcript", Path: path, Err: safefs.ErrChanged}
	}
	off := max(fi.Size()-MaxTail, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxTail))
	if err != nil {
		return nil, err
	}
	if off > 0 {
		// The window starts mid-line: drop the cut line.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	return data, nil
}

// parse reads the turns in JSON lines data. ok is false when data has
// lines but none in a shape this vendor's format is known to write.
func parse(v adapter.Vendor, data []byte) (turns []adapter.Turn, ok bool) {
	lines, known := 0, 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		lines++
		var t adapter.Turn
		var rec bool
		switch v {
		case adapter.Claude:
			t, rec = claudeTurn(line)
		case adapter.Codex:
			t, rec = codexTurn(line)
		case adapter.Grok:
			t, rec = grokTurn(line)
		}
		if rec {
			known++
		}
		if t.Text != "" {
			turns = append(turns, t)
		}
	}
	return turns, lines == 0 || known > 0
}

// textPart is one content block: {"type": ..., "text": ...}.
type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// joinText joins the text of the parts whose type is in types.
func joinText(parts []textPart, types ...string) string {
	var out []string
	for _, p := range parts {
		for _, t := range types {
			if p.Type == t && p.Text != "" {
				out = append(out, p.Text)
			}
		}
	}
	return strings.Join(out, "\n")
}

// claudeTurn reads one Claude line. User lines, per grok-build's Claude
// importer (xai-grok-foreign-sessions/src/claude.rs#L437): "type":"user",
// "message.content" a string or an array of blocks whose "text" blocks
// are read; isMeta, isCompactSummary, and isSidechain lines are skipped.
// Unverified: assistant lines are read the same way with "type":"assistant".
func claudeTurn(line []byte) (adapter.Turn, bool) {
	var l struct {
		Type             string `json:"type"`
		IsMeta           bool   `json:"isMeta"`
		IsCompactSummary bool   `json:"isCompactSummary"`
		IsSidechain      bool   `json:"isSidechain"`
		Message          struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type == "" {
		return adapter.Turn{}, false
	}
	if l.Type != "user" && l.Type != "assistant" || l.IsMeta || l.IsCompactSummary || l.IsSidechain {
		return adapter.Turn{}, true
	}
	var s string
	if json.Unmarshal(l.Message.Content, &s) != nil {
		var parts []textPart
		if json.Unmarshal(l.Message.Content, &parts) != nil {
			return adapter.Turn{}, true
		}
		s = joinText(parts, "text")
	}
	return adapter.Turn{Role: l.Type, Text: s}, true
}

// codexTurn reads one Codex rollout line: {"timestamp", "type":
// "response_item", "payload": {"type": "message", "role", "content":
// [{"type": "input_text"|"output_text", "text"}]}} (codex-rs
// history/src/lib.rs#L369, history/src/rollout_payload.rs#L32,
// protocol/src/models.rs#L1062 and #L883; persisted per
// rollout/src/policy.rs#L82). Developer and system messages are skipped.
func codexTurn(line []byte) (adapter.Turn, bool) {
	var l struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Type    string     `json:"type"`
			Role    string     `json:"role"`
			Content []textPart `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type == "" {
		return adapter.Turn{}, false
	}
	if l.Type != "response_item" || l.Payload.Type != "message" || l.Payload.Role != "user" && l.Payload.Role != "assistant" {
		return adapter.Turn{}, true
	}
	// The timestamp's layout is not recorded; an unreadable one stays zero.
	ts, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
	return adapter.Turn{Role: l.Payload.Role, Text: joinText(l.Payload.Content, "input_text", "output_text"), Time: ts}, true
}

// grokTurn reads one line of Grok's chat_history.jsonl, one
// ConversationItem per line (grok-build xai-grok-shell/src/session/
// storage/jsonl/mod.rs#L1333): {"type":"user","content":[{"type":"text",
// "text"}]} with "synthetic_reason" only on injected messages, which are
// skipped; {"type":"assistant","content":"<text>"} (xai-grok-sampling-types
// /src/conversation.rs#L71, #L239, #L266, #L379).
func grokTurn(line []byte) (adapter.Turn, bool) {
	var l struct {
		Type            string          `json:"type"`
		Content         json.RawMessage `json:"content"`
		SyntheticReason string          `json:"synthetic_reason"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type == "" {
		return adapter.Turn{}, false
	}
	switch l.Type {
	case "user":
		var parts []textPart
		if l.SyntheticReason != "" || json.Unmarshal(l.Content, &parts) != nil {
			return adapter.Turn{}, true
		}
		return adapter.Turn{Role: "user", Text: joinText(parts, "text")}, true
	case "assistant":
		var s string
		if json.Unmarshal(l.Content, &s) != nil {
			return adapter.Turn{}, true
		}
		return adapter.Turn{Role: "assistant", Text: s}, true
	}
	return adapter.Turn{}, true
}
