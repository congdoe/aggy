// Package suggest finds commands worth proposing as allow rules, from Claude
// Code's session files, read locally with no model (docs/design.md, section 15,
// "Suggestions from Claude Code"). It counts Bash calls that ran in auto mode
// and did not end in an error, and keeps only a plain command with nothing
// secret-shaped in it.
//
// A count is a hint and never an approval: these files are plain text a model's
// shell can write, and a tricked model could repeat a command until it counted.
// Nothing here makes an allow: only a person at the keyboard does.
//
// The fields read are observed, not documented (docs/verification.md, "Claude
// Code session file fields"), so a line that does not fit is passed over with
// no error. is_error cannot tell a denied command from one that just failed, so
// both are left out. Scan reports what it saw, so aggy doctor can say when the
// format seems to have moved.
package suggest

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/redact"
)

const (
	// MinCount is how many times a command must have run to be proposed.
	MinCount = 3
	// MaxCommand is the longest command kept.
	MaxCommand = 200
	// maxLine is the longest line looked at; a longer one is passed over.
	maxLine = 4 << 20
	// maxPending bounds the calls remembered while waiting for their result.
	maxPending = 256
)

// Call is a shell call waiting for its result.
type Call struct {
	ID      string `json:"id"`
	Repo    string `json:"repo"`
	Command string `json:"command"`
}

// State is what is kept between scans of one file.
type State struct {
	Offset int64
	// Mode is the last permission mode the file named: not every line
	// carries one.
	Mode    string
	Pending []Call
}

// Count is how many times a command ran, in this stretch of a file.
type Count struct {
	Repo, Command string
	N             int
}

// Result is one stretch of a file.
type Result struct {
	State  State
	Counts []Count
	// EOF says the reader ran out, rather than the limit being reached.
	EOF bool
	// Shells counts Bash calls seen, and WithMode those with a mode known.
	Shells, WithMode int
}

// Accept says which repo a line's cwd is in, if it should be counted. Different
// folders can share a session folder's name, so it is checked per line.
type Accept func(cwd string) (repo string, ok bool)

// sessionLine is the part of a session line read here.
type sessionLine struct {
	Cwd            string  `json:"cwd"`
	PermissionMode *string `json:"permissionMode"`
	Message        struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input struct {
		Command string `json:"command"`
	} `json:"input"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
}

// Scan reads complete lines from r, which starts at st.Offset, up to about limit
// bytes. An unfinished last line is left for the next scan, as the CLI may still
// be writing it. The only errors are the reader's.
func Scan(r io.Reader, st State, accept Accept, limit int64) (Result, error) {
	res := Result{State: st}
	res.State.Pending = slices.Clone(st.Pending)
	counts := map[[2]string]int{}
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	var read int64
	for read < limit && !res.EOF {
		line = line[:0]
		var lineLen int64
		var err error
		for {
			var chunk []byte
			chunk, err = br.ReadSlice('\n')
			lineLen += int64(len(chunk))
			if lineLen <= maxLine {
				line = append(line, chunk...)
			}
			if !errors.Is(err, bufio.ErrBufferFull) {
				break
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			res.EOF = true
			continue
		case err != nil:
			return Result{}, err
		}
		read += lineLen
		res.State.Offset += lineLen
		if lineLen <= maxLine {
			res.addLine(line, accept, counts)
		}
	}
	for k, n := range counts {
		res.Counts = append(res.Counts, Count{k[0], k[1], n})
	}
	sort.Slice(res.Counts, func(i, j int) bool {
		a, b := res.Counts[i], res.Counts[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Command < b.Command
	})
	return res, nil
}

func (res *Result) addLine(raw []byte, accept Accept, counts map[[2]string]int) {
	var l sessionLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	if l.PermissionMode != nil {
		res.State.Mode = *l.PermissionMode
	}
	var blocks []block
	if len(l.Message.Content) == 0 || json.Unmarshal(l.Message.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			if b.Name != "Bash" {
				continue
			}
			res.Shells++
			if res.State.Mode != "" {
				res.WithMode++
			}
			if res.State.Mode != "auto" || b.ID == "" || !Plain(b.Input.Command) {
				continue
			}
			if repo, ok := accept(l.Cwd); ok {
				res.remember(Call{b.ID, repo, b.Input.Command})
			}
		case "tool_result":
			i := slices.IndexFunc(res.State.Pending, func(c Call) bool { return c.ID == b.ToolUseID })
			if i < 0 {
				continue
			}
			c := res.State.Pending[i]
			res.State.Pending = slices.Delete(res.State.Pending, i, i+1)
			if !b.IsError {
				counts[[2]string{c.Repo, c.Command}]++
			}
		}
	}
}

// remember keeps a call for its result, the oldest going first past the bound.
func (res *Result) remember(c Call) {
	if len(res.State.Pending) >= maxPending {
		res.State.Pending = slices.Delete(res.State.Pending, 0, 1)
	}
	res.State.Pending = append(res.State.Pending, c)
}

// Plain reports a command that can be proposed as one exact rule: one line and
// one simple command, nothing a filter drops, and nothing secret-shaped. A
// secret-shaped command is dropped whole, not stored edited.
func Plain(cmd string) bool {
	if cmd == "" || len(cmd) > MaxCommand || strings.TrimSpace(cmd) != cmd {
		return false
	}
	if strings.ContainsAny(cmd, "\n\r\x00;|&<>`$()*?{}\\'\"") {
		return false
	}
	first, _, _ := strings.Cut(cmd, " ")
	if strings.Contains(first, "=") {
		return false
	}
	if class, _ := permissions.CheckAllow("Bash(" + cmd + ")"); class == permissions.Drop {
		return false
	}
	return !redact.Secret(cmd)
}

// ProjectFile says whether a name in a session folder is a session file.
func ProjectFile(name string) bool { return strings.HasSuffix(name, ".jsonl") }
