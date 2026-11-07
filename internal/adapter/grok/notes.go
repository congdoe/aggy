package grok

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// PolicyDenied starts a tool's result when Grok's permission gate refused it.
// Live, grok 1.0.70 (verification.md S4) and, in the 2026-11-06 agent audit,
// 1.0.77 (T6922): "Denied by permission policy: deny rule on read matching ...".
const PolicyDenied = "Denied by permission policy"

// RunNotes says when a review's read tools were refused, which the reply does
// not have to mention. aggy's "~*" and "~*/**" deny rules (readDeny) also
// catch every path under $HOME: on grok 1.0.77, read_file and list_dir in a
// repo under $HOME returned PolicyDenied, matching "~*/**", so such a review
// has only the diff and files in its prompt. Whether grep is refused the same
// way was not seen (it has the same rule), and neither was any Grok below
// 1.0.77 with these rules. A repo outside $HOME has the note only when the
// session's history shows a refusal, such as a path with a part that starts
// with "~"; the history's tool_result lines are source only (SelfTestEvidence),
// so a missing history shows nothing.
func (a Adapter) RunNotes(req adapter.HeadlessRequest, res adapter.HeadlessResult) []string {
	if req.Kind != adapter.Review {
		return nil
	}
	if a.underHome(req.Cwd) {
		return []string{"this repo is under your home folder, where aggy's deny rules refuse Grok's read tools (read_file and list_dir; grep has the same rule, unverified), so this review saw only the diff and files in its prompt"}
	}
	if a.historyShowsDenial(req.Cwd, res.SessionID) {
		return []string{"aggy's deny rules refused a read tool during this Grok review, so it may rest on the diff and files in its prompt alone"}
	}
	return nil
}

// underHome reports whether dir is $HOME or inside it, as spelled or as its
// physical path.
func (a Adapter) underHome(dir string) bool {
	getenv := a.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	home := getenv("HOME")
	if !filepath.IsAbs(home) || home == "/" || !filepath.IsAbs(dir) {
		return false
	}
	if inside(dir, home) {
		return true
	}
	realpath := a.Realpath
	if realpath == nil {
		realpath = safefs.RealPath
	}
	rd, err := realpath(dir)
	if err != nil {
		return false
	}
	rh, err := realpath(home)
	return err == nil && inside(rd, rh)
}

// inside reports whether p is dir or below it.
func inside(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// historyShowsDenial reports whether a tool_result line of the session's
// chat_history.jsonl holds PolicyDenied. Only those lines: the prompt holds
// the diff under review, which can quote anything.
func (a Adapter) historyShowsDenial(cwd, id string) bool {
	if adapter.ValidSessionID(adapter.Grok, id) != nil || !filepath.IsAbs(a.Home()) {
		return false
	}
	data, err := a.history(cwd, id)
	if err != nil {
		return false
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		var e struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &e) == nil && e.Type == "tool_result" && bytes.Contains(line, []byte(PolicyDenied)) {
			return true
		}
	}
	return false
}
