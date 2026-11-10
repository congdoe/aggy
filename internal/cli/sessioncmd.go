package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/tooldef"
	"github.com/congdoe/aggy/internal/trustlist"
)

// sessionRunMu runs one session command at a time: each swaps keyboard.
var sessionRunMu sync.Mutex

// maxSessionOutput bounds what a session command's page holds.
const maxSessionOutput = 1 << 20

// sessionAggy runs one of aggy's own commands in-process for a command typed
// inside a session (T6909, docs/design.md section 5) and returns what it
// printed, output and errors together, for the supervisor to show on the
// alternate screen. The supervisor's reader owns the terminal, so nothing here
// reads it: each command gets an empty stdin and no terminal, and a question it
// asks the person (keyboard: trust) gets answer, y or no. The person typed the
// command, and answered y on the page that showed what it does.
//
// Only the commands sessioncmd parses run here; any other is refused, so a
// hook or a bug cannot reach unwire, a proxy, or a borrow through it.
func sessionAggy(args []string, answer bool) string {
	sessionRunMu.Lock()
	defer sessionRunMu.Unlock()
	var buf bytes.Buffer
	w := &capWriter{w: &buf, left: maxSessionOutput}
	if len(args) == 0 {
		return ""
	}
	defer withAnswer(answer)()
	none := strings.NewReader("")
	if u, ok := commandHelp[args[0]]; ok && wantsHelp(args[1:]) {
		fmt.Fprint(w, u)
		return buf.String()
	}
	switch args[0] {
	case "accept":
		acceptWith(args[1:], defaultBorrowEnv(), none, false, w, w)
	case "check":
		checkEditWith(context.Background(), args[1:], defaultBorrowEnv(), defaultRepoCheckEnv(), none, false, w, w)
	case "reject":
		rejectWith(args[1:], defaultBorrowEnv(), w, w)
	case "tool":
		if len(args) > 1 && (args[1] == "on" || args[1] == "off" || args[1] == "status") {
			// The page stands in for the terminal (T6975): a question tool on
			// asks there (a first index's y/N) is printed on the page and gets
			// answer, no on the first run and y on the page's y step
			// (sessioncmd's Confirm for tool on), never skipped as with no
			// terminal. --proxy, the other question, never comes here.
			toolWith(args[1:], keyboard, true, w, w)
			break
		}
		sessionRefuse(w, "tool", "aggy tool takes on, off, or status here")
	case "trust":
		// The list itself is the supervisor's (sessionTrustList); only the
		// shortcut that ticks OpenCode runs here, for this folder.
		if slices.Equal(args, []string{"trust", "--opencode", "."}) {
			Run(args, w, w)
			break
		}
		sessionRefuse(w, "trust", "inside a session, aggy trust --opencode takes only this folder; /aggy trust opens the list")
	case "sessions", "session", "usage", "stats", "log", "doctor", "diag", "brief", "inbox", "permissions":
		if args[0] == "diag" && len(args) > 1 && args[1] != "show" && args[1] != "-n" {
			sessionRefuse(w, "diag", "inside a session, aggy diag shows the log or one run")
			break
		}
		if args[0] == "permissions" && (len(args) != 2 || args[1] != "list" && args[1] != "show") {
			sessionRefuse(w, "permissions", "inside a session, aggy permissions lists and shows the rules; change them in a terminal")
			break
		}
		Run(args, w, w)
	default:
		sessionRefuse(w, args[0], "not a command for inside a session")
	}
	if w.left <= 0 {
		buf.WriteString("\n[cut: more than 1 MiB]\n")
	}
	return buf.String()
}

// sessionRefuse refuses a session command as every refusal does: logged, with
// its code and run id on the line (docs/logging.md).
func sessionRefuse(w io.Writer, cmd, msg string) {
	refuse(context.Background(), w, cmd, diag.EvCommandRefused, diag.CodeUsage, msg, diag.Rec{})
}

// withAnswer makes keyboard answer y (answer) or no, until the func it returns
// puts it back. Callers hold sessionRunMu.
func withAnswer(answer bool) func() {
	reply := "\n"
	if answer {
		reply = "y\n"
	}
	old := keyboard
	keyboard = strings.NewReader(reply)
	return func() { keyboard = old }
}

// capWriter writes up to left bytes and drops the rest.
type capWriter struct {
	w    io.Writer
	left int
}

func (c *capWriter) Write(b []byte) (int, error) {
	n := len(b)
	if c.left <= 0 {
		return n, nil
	}
	if len(b) > c.left {
		b = b[:c.left]
	}
	c.left -= len(b)
	_, err := c.w.Write(b)
	return n, err
}

// sessionTrustList reads the trust list of the session's folder, cwd, for
// /aggy trust (T6977), as aggy trust <dir> shows it.
func sessionTrustList(cwd string) func() (supervisor.TrustList, error) {
	return func() (supervisor.TrustList, error) {
		l, err := sessionTrustAt(cwd)
		if err != nil {
			return supervisor.TrustList{}, err
		}
		return supervisor.TrustList{Lines: strings.Split(strings.TrimRight(l.header(false), "\n"), "\n"), State: l.st}, nil
	}
}

func sessionTrustAt(cwd string) (*trustListAt, error) {
	cfg, err := config.LoadDefault()
	if err != nil {
		return nil, err
	}
	tg, err := trustTargetFor(cwd)
	if err != nil {
		return nil, err
	}
	return openTrustList(tg, cfg, true)
}

// sessionTrustAnswer records the answer the person gave on the list: the list
// is read again, the rows ticked as they ticked them, and written as aggy
// trust <dir> writes it. The supervisor calls it only for a list its keyboard
// opened and answered, never from a hook. It returns what it printed.
func sessionTrustAnswer(cwd string) func(ticked []string, c trustlist.Choice) string {
	return func(ticked []string, c trustlist.Choice) string {
		sessionRunMu.Lock()
		defer sessionRunMu.Unlock()
		var buf bytes.Buffer
		w := &capWriter{w: &buf, left: maxSessionOutput}
		l, err := sessionTrustAt(cwd)
		if err != nil {
			refuse(context.Background(), w, "trust", diag.EvTrustRefused, diag.CodeTrustBadFile, err.Error(), diag.Rec{})
			return buf.String()
		}
		l.tick(ticked)
		applyTrustList(context.Background(), l, c == trustlist.Never, true, false, w, w)
		return buf.String()
	}
}

// sessionToolNames lists the managed tools /aggy tool offers, built-ins and
// the person's own, by name. A registry that cannot load lists none.
func sessionToolNames() []string {
	r, _, err := tooldef.LoadDefault()
	if err != nil {
		return nil
	}
	var names []string
	for _, t := range r.List() {
		names = append(names, t.Name)
	}
	return names
}
