package brief

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/safefs"
)

// TaskFile is the task file's path relative to the working dir.
const TaskFile = ".aggy/task.md"

// MaxTaskLines is the task file's length limit, in lines.
const MaxTaskLines = 60

// maxTaskBytes bounds the read: 60 lines of prose fit well under it.
const maxTaskBytes = 16 << 10

// Headings are the task file's fixed sections, in order.
var Headings = []string{"Goal", "Done", "Next", "Decisions", "Files touched", "Open questions"}

// TaskInstruction is what the aggy skill tells every harness about the
// task file.
const TaskInstruction = `Keep .aggy/task.md current. At each step boundary (a subtask done, a
decision made, a plan changed), rewrite it in place under exactly these
headings: ## Goal, ## Done, ## Next, ## Decisions, ## Files touched,
## Open questions. Keep it under 60 lines: short bullets, the newest state
only, no history. Another CLI may pick up this work from it at any time.
Never put secrets, tokens, or the contents of credential files in it.`

// Nudge is what the Stop hook says when the task file is stale.
const Nudge = "aggy: .aggy/task.md has not been updated for 5 or more turns and files changed. Update it under its fixed headings before you continue."

// staleTurns is how many turns without a task-file update, with files
// changed, make the task file stale.
const staleTurns = 5

// TaskTemplate is an empty task file with the fixed headings.
func TaskTemplate() string {
	var b strings.Builder
	b.WriteString("# Task\n")
	for _, h := range Headings {
		b.WriteString("\n## " + h + "\n")
	}
	return b.String()
}

// ReadTask reads dir's task file. A missing file is "" with no error. The
// file is read through safefs, at most 16 KiB, and refused when it or its
// .aggy folder is a symlink (a cloned repo could point either anywhere).
func ReadTask(dir string) (string, error) {
	b, err := safefs.ReadConfig(filepath.Join(dir, TaskFile), maxTaskBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckTask lists what is wrong with a task file's text: a missing or
// out-of-order heading, or more than MaxTaskLines lines.
func CheckTask(text string) []string {
	var problems []string
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > MaxTaskLines {
		problems = append(problems, fmt.Sprintf("%d lines, over the %d-line limit", len(lines), MaxTaskLines))
	}
	next := 0
	for _, l := range lines {
		if next < len(Headings) && strings.TrimSpace(l) == "## "+Headings[next] {
			next++
		}
	}
	if next < len(Headings) {
		problems = append(problems, fmt.Sprintf("missing heading %q (or out of order)", "## "+Headings[next]))
	}
	return problems
}

// NeedsNudge reports whether the Stop hook should nudge the agent: the
// task file has gone turnsSinceUpdate turns without an update and files
// changed meanwhile.
//
// TODO(T6657): call it from the Stop hook once M2's hooks exist; the hook
// counts the turns and checks git for changed files.
func NeedsNudge(turnsSinceUpdate int, filesChanged bool) bool {
	return filesChanged && turnsSinceUpdate >= staleTurns
}
