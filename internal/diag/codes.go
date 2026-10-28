package diag

import (
	"fmt"
	"strings"
)

// The registry: every event and reason code aggy writes, with its level,
// meaning, and what to check next. docs/logging.md's registry tables are
// generated from it (Markdown), and TestRegistry fails on an event or
// code used in code but not registered, or registered but never used.
// Code refers to these by the variables below only: Event and Code have
// an unexported field, so no other package can make one from a string.

// Level is a record's severity.
type Level string

// The levels, most severe first.
const (
	LevelError Level = "error"
	LevelWarn  Level = "warn"
	LevelInfo  Level = "info"
	LevelDebug Level = "debug"
)

// Levels lists the levels, most severe first.
var Levels = []Level{LevelError, LevelWarn, LevelInfo, LevelDebug}

// AtLeast returns the levels as severe as l or more.
func AtLeast(l Level) []Level {
	for i, x := range Levels {
		if x == l {
			return Levels[:i+1]
		}
	}
	return nil
}

// Event is a stable dotted name, <component>.<thing>.<what happened>.
// Only this file makes one.
type Event struct{ name string }

// String is the event's name.
func (e Event) String() string { return e.name }

// Code is a stable reason code, <area>.<reason>. Only this file makes
// one; the zero Code is no code.
type Code struct{ name string }

// String is the code's name.
func (c Code) String() string { return c.name }

// IsZero reports whether c is no code.
func (c Code) IsZero() bool { return c.name == "" }

// EventInfo is a registered event.
type EventInfo struct {
	Name      Event
	Level     Level
	Component string
	Meaning   string
	Next      string
}

// CodeInfo is a registered reason code. A Security code is a
// security-relevant refusal: it is also written to the index's events
// table as a refusal, which aggy log shows.
type CodeInfo struct {
	Name     Code
	Security bool
	Meaning  string
	Next     string
}

// Events.
var (
	EvBorrowRefused       = Event{"borrow.request.refused"}
	EvBorrowVendorSkipped = Event{"borrow.vendor.skipped"}
	EvBorrowChildStarted  = Event{"borrow.child.started"}
	EvBorrowChildExited   = Event{"borrow.child.exited"}
	EvBorrowFailed        = Event{"borrow.run.failed"}
	EvBorrowFinished      = Event{"borrow.run.finished"}
	EvClaudeLockdown      = Event{"adapter.claude.lockdown_rejected"}
	EvCodexLockdown       = Event{"adapter.codex.lockdown_rejected"}
	EvGrokLockdown        = Event{"adapter.grok.lockdown_rejected"}
	EvGrokSandboxRefused  = Event{"adapter.grok.sandbox_refused"}
	EvAcceptRefused       = Event{"accept.request.refused"}
	EvAcceptApplied       = Event{"accept.diff.applied"}
	EvAcceptDeclined      = Event{"accept.diff.declined"}
	EvGateRefused         = Event{"check.gate.refused"}
	EvGateDecided         = Event{"check.gate.decided"}
	EvCheckRefused        = Event{"check.run.refused"}
	EvCheckCommandFailed  = Event{"check.command.failed"}
	EvCheckFinished       = Event{"check.run.finished"}
	EvGCRefused           = Event{"gc.run.refused"}
	EvGCRemoved           = Event{"gc.item.removed"}
	EvGCRemoveFailed      = Event{"gc.item.failed"}
	EvSetupChecked        = Event{"setup.vendor.checked"}
	EvSetupFailed         = Event{"setup.run.failed"}
	EvDoctorError         = Event{"doctor.finding.error"}
	EvDoctorRefused       = Event{"doctor.run.refused"}
)

// Codes.
var (
	CodeUsage          = Code{"cli.usage"}
	CodeConfig         = Code{"cli.config_invalid"}
	CodeStore          = Code{"store.unavailable"}
	CodeOneHop         = Code{"borrow.one_hop"}
	CodeBorrowInvalid  = Code{"borrow.invalid_request"}
	CodeNoVendor       = Code{"borrow.no_vendor"}
	CodeBorrowStart    = Code{"borrow.start_failed"}
	CodeCLIMissing     = Code{"vendor.cli_missing"}
	CodeUnsupported    = Code{"adapter.unsupported"}
	CodeNoCapability   = Code{"adapter.no_capability"}
	CodeFileTools      = Code{"adapter.file_tools_off"}
	CodeNoSandbox      = Code{"sandbox.missing"}
	CodeUntested       = Code{"lockdown.untested_version"}
	CodeLockdown       = Code{"lockdown.rejected"}
	CodeRateLimit      = Code{"vendor.rate_limit"}
	CodeAuth           = Code{"vendor.auth"}
	CodeBilling        = Code{"vendor.billing"}
	CodeServer         = Code{"vendor.server"}
	CodeVendorInvalid  = Code{"vendor.invalid_request"}
	CodeMaxTurns       = Code{"borrow.max_turns"}
	CodeTimeout        = Code{"borrow.timeout"}
	CodeInterrupted    = Code{"borrow.interrupted"}
	CodeChildFailed    = Code{"borrow.child_failed"}
	CodeAcceptTarget   = Code{"accept.target_invalid"}
	CodeProtected      = Code{"accept.protected_path"}
	CodeTreeChanged    = Code{"accept.worktree_changed"}
	CodeDirty          = Code{"accept.checkout_dirty"}
	CodeAcceptFailed   = Code{"accept.failed"}
	CodeGateTempDir    = Code{"check.gate_temp_dir"}
	CodeChecksChanged  = Code{"check.tree_changed"}
	CodeGateFailed     = Code{"check.gate_failed"}
	CodeChecksFailed   = Code{"check.failed"}
	CodeNotGit         = Code{"check.not_git"}
	CodeEditWorktree   = Code{"check.edit_worktree"}
	CodeNoChecks       = Code{"check.no_checks"}
	CodeGCFailed       = Code{"gc.failed"}
	CodeGCRemoveFailed = Code{"gc.remove_failed"}
	CodeDoctorError    = Code{"doctor.error_finding"}
	CodeDiagWrite      = Code{"diag.write_failed"}
)

const showRun = "aggy diag show <run id> for what led to it"

var events = []EventInfo{
	{EvBorrowRefused, LevelError, "borrow", "aggy borrow refused or failed outside the child's own run: before a child started, or while recording one.", "The code's entry; " + showRun + "."},
	{EvBorrowVendorSkipped, LevelInfo, "borrow", "A candidate vendor could not start the borrow, and the next one was tried.", "The code says why; aggy setup and aggy doctor for that vendor."},
	{EvBorrowChildStarted, LevelInfo, "borrow", "The borrow was recorded and its child CLI started.", "A started borrow with no borrow.run.finished was cut short: aggy diag show <borrow id>."},
	{EvBorrowChildExited, LevelInfo, "borrow", "The child CLI exited; exit_code and duration_ms say how.", "A non-zero exit is classed by the next record (borrow.run.failed)."},
	{EvBorrowFailed, LevelWarn, "borrow", "The borrow ran and failed; the code is the vendor's error class.", "The code's entry; aggy inbox <borrow id> for the report."},
	{EvBorrowFinished, LevelInfo, "borrow", "The borrow ended and was recorded; attrs hold the outcome and tokens.", "aggy log and aggy stats show it too."},
	{EvClaudeLockdown, LevelError, "adapter.claude", "Claude Code rejected a lockdown flag; the run was discarded.", "aggy doctor for the version; attach aggy diag bundle to an issue."},
	{EvCodexLockdown, LevelError, "adapter.codex", "Codex rejected a lockdown flag; the run was discarded.", "aggy doctor for the version; attach aggy diag bundle to an issue."},
	{EvGrokLockdown, LevelError, "adapter.grok", "Grok rejected a lockdown flag, reported an unknown tool, or ran without its sandbox; the run was discarded.", "aggy doctor for the version; attach aggy diag bundle to an issue."},
	{EvGrokSandboxRefused, LevelWarn, "adapter.grok", "Grok was passed over: its sandbox is not available for this kind on this CLI or host.", "sandbox.missing."},
	{EvAcceptRefused, LevelError, "accept", "aggy accept refused or failed; nothing was applied.", "The code's entry; aggy diag show <borrow id>."},
	{EvAcceptApplied, LevelInfo, "accept", "An edit's diff was applied to the main checkout.", "git status in the main checkout."},
	{EvAcceptDeclined, LevelInfo, "accept", "The diff was not applied: no --yes without a terminal, or the person said no.", "Run aggy accept <borrow id> --yes."},
	{EvGateRefused, LevelError, "check", "The check gate on an edit's diff refused or failed.", "The code's entry."},
	{EvGateDecided, LevelInfo, "check", "The check gate recorded a result (attrs.result: pass, fail, or skipped).", "aggy accept shows it."},
	{EvCheckRefused, LevelError, "check", "aggy check refused to run the repo's checks.", "The code's entry."},
	{EvCheckCommandFailed, LevelWarn, "check", "One of the repo's checks failed (attrs.index is its place in the list).", "Run aggy check again to see its output tail; nothing of it is stored."},
	{EvCheckFinished, LevelInfo, "check", "The repo's checks ran; attrs hold how many passed and failed.", "check.command.failed records for the failures."},
	{EvGCRefused, LevelError, "gc", "aggy gc refused or could not plan; nothing was removed.", "The code's entry."},
	{EvGCRemoved, LevelInfo, "gc", "aggy gc removed one item (attrs.kind: worktree, child, inbox, research, or diag).", "Nothing; it is the record of the removal."},
	{EvGCRemoveFailed, LevelWarn, "gc", "aggy gc could not remove one item; it stays for the next run.", "The record's msg; aggy gc --dry-run."},
	{EvSetupChecked, LevelInfo, "setup", "aggy setup checked and recorded one vendor CLI (attrs: connected, version).", "aggy doctor for more."},
	{EvSetupFailed, LevelError, "setup", "aggy setup failed.", "The code's entry."},
	{EvDoctorError, LevelError, "doctor", "aggy doctor reported an error-level finding (attrs.finding is its doctor code).", "aggy doctor shows the finding in full."},
	{EvDoctorRefused, LevelError, "doctor", "aggy doctor did not run.", "The code's entry."},
}

var codes = []CodeInfo{
	{CodeUsage, false, "Bad arguments or flags.", "Run the command with --help."},
	{CodeConfig, false, "A config file could not be read or is invalid.", "The message names the file and key; fix it."},
	{CodeStore, false, "The index (~/.aggy/aggy.db) could not be opened, read, or written.", "aggy doctor; the state dir's permissions and free space."},
	{CodeOneHop, true, "aggy borrow or aggy gc ran inside a borrow child (AGGY_DELEGATED is set): borrows are one hop.", "Run it from your own shell. A child that tries this was steered to; read its task."},
	{CodeBorrowInvalid, false, "The borrow request failed validation: an empty or too long task, a bad path or constraint, or a flag the kind does not take.", "aggy borrow --help."},
	{CodeNoVendor, false, "No candidate vendor could start the borrow.", "aggy diag --run <run id> --event borrow.vendor.skipped lists why each was passed over; aggy setup."},
	{CodeBorrowStart, false, "The borrow failed after a vendor was chosen, while making its dirs, worktree, or records (the concurrency caps included).", showRun + "; aggy doctor."},
	{CodeCLIMissing, false, "The vendor's CLI is not on PATH, or its probe failed.", "aggy setup."},
	{CodeUnsupported, false, "The vendor's adapter does not run this kind or request on this CLI or in this repo.", "The README's vendor support table; another --vendor."},
	{CodeNoCapability, false, "The CLI's --help lacks a flag the lockdown needs.", "Update the CLI; aggy setup shows its version."},
	{CodeFileTools, false, "The request needs Grok's media file tools, which are off.", "[media] grok_file_tools in ~/.aggy/config.toml, after reading what it uploads."},
	{CodeNoSandbox, false, "The vendor's sandbox is not available for this kind on this CLI or host.", "Grok needs --sandbox strict in its --help and a kernel sandbox (macOS, or Linux with Landlock)."},
	{CodeUntested, false, "The CLI's version is outside the versions the borrow lockdown was verified on.", "aggy doctor; --allow-untested runs it anyway."},
	{CodeLockdown, true, "The CLI rejected a lockdown flag, reported a tool it does not know, or ran without its sandbox, so aggy discarded the run.", "aggy doctor for the version; attach aggy diag bundle to an issue."},
	{CodeRateLimit, false, "The vendor reported a plan or rate limit.", "aggy log shows the limit; routing skips the vendor until it lifts."},
	{CodeAuth, false, "The vendor CLI is not signed in.", "The vendor's own login; then aggy setup."},
	{CodeBilling, false, "The vendor reported a billing problem.", "The vendor's account page."},
	{CodeServer, false, "The vendor reported an outage or overload.", "Try again later, or another --vendor."},
	{CodeVendorInvalid, false, "The vendor refused the request as invalid.", "aggy inbox <borrow id> for the report; aggy doctor for the version."},
	{CodeMaxTurns, false, "The child used its turn budget.", "[budgets] in ~/.aggy/config.toml, or a smaller task."},
	{CodeTimeout, false, "The child ran past its time budget and was killed.", "[budgets] in ~/.aggy/config.toml, or a smaller task."},
	{CodeInterrupted, false, "aggy was interrupted (Ctrl-C, SIGTERM, SIGHUP) and killed the child.", "Nothing, unless you did not interrupt it."},
	{CodeChildFailed, false, "The child failed for another reason: unparsed output, an unknown error class, or a runner error.", "aggy inbox <borrow id> for the report."},
	{CodeAcceptTarget, false, "No such edit borrow, or it is running, already accepted or rejected, or its worktree was removed.", "aggy inbox; aggy diag show <borrow id>."},
	{CodeProtected, true, "The diff touches .git, a symlink or submodule, the hooks path, or a file that holds hooks, CI, config, or instructions.", "Review the worktree by hand; never accept such a diff blind."},
	{CodeTreeChanged, true, "The worktree changed after the borrow ended (a process the child left behind?).", "Review the worktree by hand."},
	{CodeDirty, false, "The main checkout has changes in the paths the diff touches.", "Commit or stash them, then accept again."},
	{CodeAcceptFailed, false, "Another accept error: git failed, or the diff no longer applies.", "The record's msg; git status."},
	{CodeGateTempDir, true, "The main checkout is under a temp dir an edit child's sandbox may write, so its check list may be the child's.", "Run edits from a repo outside temp dirs."},
	{CodeChecksChanged, true, "The checks changed files in the worktree, so it no longer matches the diff the child left.", "Review the worktree by hand."},
	{CodeGateFailed, false, "Another check gate error.", "The record's msg."},
	{CodeChecksFailed, false, "One or more of the repo's checks failed.", "aggy check prints each failure's output tail."},
	{CodeNotGit, false, "aggy check ran outside a git work tree.", "cd into the repo."},
	{CodeEditWorktree, true, "aggy check ran by hand inside an edit worktree, whose config the child wrote.", "aggy check <borrow id> from the main checkout."},
	{CodeNoChecks, false, "The repo lists no checks.", "Add check = [[...]] to <repo>/.aggy/config.toml."},
	{CodeGCFailed, false, "aggy gc could not plan.", "The record's msg; aggy doctor."},
	{CodeGCRemoveFailed, false, "aggy gc could not remove one item.", "The record's msg; aggy gc --dry-run."},
	{CodeDoctorError, false, "aggy doctor found an error-level finding.", "aggy doctor."},
	{CodeDiagWrite, false, "aggy could not write its diagnostic log; the command went on without it.", "The state dir's permissions and free space; aggy gc prunes the log."},
}

// LookupEvent returns event e's entry.
func LookupEvent(e Event) (EventInfo, bool) {
	for _, x := range events {
		if x.Name == e {
			return x, true
		}
	}
	return EventInfo{}, false
}

// LookupCode returns code c's entry.
func LookupCode(c Code) (CodeInfo, bool) {
	for _, x := range codes {
		if x.Name == c {
			return x, true
		}
	}
	return CodeInfo{}, false
}

// EventNamed returns the event named s.
func EventNamed(s string) (EventInfo, bool) { return LookupEvent(Event{s}) }

// CodeNamed returns the code named s.
func CodeNamed(s string) (CodeInfo, bool) { return LookupCode(Code{s}) }

// Events returns every registered event, in registry order.
func Events() []EventInfo { return append([]EventInfo(nil), events...) }

// Codes returns every registered code, in registry order.
func Codes() []CodeInfo { return append([]CodeInfo(nil), codes...) }

// Markdown renders the registry as docs/logging.md's two tables.
func Markdown() string {
	esc := strings.NewReplacer("<", "&lt;", ">", "&gt;", "|", `\|`).Replace
	var b strings.Builder
	b.WriteString("| Event | Level | Component | Meaning | What to check next |\n| --- | --- | --- | --- | --- |\n")
	for _, e := range events {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", e.Name, e.Level, e.Component, esc(e.Meaning), esc(e.Next))
	}
	b.WriteString("\n| Code | Security | Meaning | What to check next |\n| --- | --- | --- | --- |\n")
	for _, c := range codes {
		sec := ""
		if c.Security {
			sec = "yes"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", c.Name, sec, esc(c.Meaning), esc(c.Next))
	}
	return b.String()
}
