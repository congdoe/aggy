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
	EvToolRefused         = Event{"tool.request.refused"}
	EvToolDefRefused      = Event{"tool.def.refused"}
	EvToolSwitched        = Event{"tool.switch.changed"}
	EvLaunchRefused       = Event{"supervisor.launch.refused"}
	EvLaunched            = Event{"supervisor.child.launched"}
	EvSessionExited       = Event{"supervisor.child.exited"}
	EvSwitchQueued        = Event{"switch.request.queued"}
	EvSwitchOffered       = Event{"switch.request.offered"}
	EvSwitchConfirmed     = Event{"switch.request.confirmed"}
	EvSwitchRefused       = Event{"switch.request.refused"}
	EvSwitchCancelled     = Event{"switch.request.cancelled"}
	EvSwitchCompleted     = Event{"switch.request.completed"}
	EvSwitchExitLate      = Event{"switch.exit.timed_out"}
	EvPickerOpened        = Event{"switch.picker.opened"}
	EvPickerChosen        = Event{"switch.picker.chosen"}
	EvPickerDismissed     = Event{"switch.picker.dismissed"}
	EvLimitReceived       = Event{"switch.limit.received"}
	EvLimitNotOffered     = Event{"switch.limit.not_offered"}
	EvNearLimitWarned     = Event{"switch.near_limit.warned"}
	EvHookAccepted        = Event{"hook.event.accepted"}
	EvHookRejected        = Event{"hook.event.rejected"}
	EvHookIgnored         = Event{"hook.event.ignored"}
	EvHookForwardFailed   = Event{"hook.forward.failed"}
	EvQueueDropped        = Event{"diag.queue.dropped"}
	EvToolWireRefused     = Event{"tool.wire.refused"}
	EvToolWired           = Event{"tool.wire.done"}
	EvToolExecRefused     = Event{"tool.exec.refused"}
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
	CodeStateTempDir   = Code{"accept.state_temp_dir"}
	CodeAcceptOS       = Code{"accept.unsupported_os"}
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
	CodeToolUnknown    = Code{"tool.unknown"}
	CodeToolScope      = Code{"tool.not_applicable"}
	CodeToolRepoOff    = Code{"tool.repo_off"}
	CodeToolDelegated  = Code{"tool.delegated"}
	CodeToolInvalid    = Code{"tool.def_invalid"}
	CodeToolUntrusted  = Code{"tool.def_untrusted"}
	CodeToolStillOn    = Code{"tool.still_on"}
	CodeToolFailed     = Code{"tool.failed"}
	CodeQueueFull      = Code{"diag.queue_full"}
	CodeNoTerminal     = Code{"supervisor.no_terminal"}
	CodeLaunchFailed   = Code{"supervisor.launch_failed"}
	CodeUnreachable    = Code{"supervisor.unreachable"}
	CodePolicy         = Code{"policy.unreachable"}
	CodeUnsentText     = Code{"switch.unsent_text"}
	CodeAgentNoHold    = Code{"switch.agent_no_hold"}
	CodeAgentOptions   = Code{"switch.agent_options"}
	CodeSwitchVendor   = Code{"switch.vendor_unavailable"}
	CodeSwitchBusy     = Code{"switch.already_waiting"}
	CodeSwitchHeld     = Code{"switch.held"}
	CodeNotSupervised  = Code{"switch.not_supervised"}
	CodeExitTimeout    = Code{"switch.exit_timeout"}
	CodeNoQuota        = Code{"switch.no_quota"}
	CodeLimitNoHold    = Code{"switch.limit_no_hold"}
	CodeNearNoQuota    = Code{"switch.near_limit_no_quota"}
	CodeChildGone      = Code{"switch.child_gone"}
	CodeLineage        = Code{"peer.lineage_failed"}
	CodePeerUID        = Code{"peer.uid_mismatch"}
	CodeBadRequest     = Code{"socket.bad_request"}
	CodeForeignSession = Code{"hook.foreign_session"}
	CodeNoSession      = Code{"hook.no_session"}
	CodePeerUnknown    = Code{"peer.cred_failed"}
	CodeWrongVendor    = Code{"hook.wrong_vendor"}
	CodeBadPayload     = Code{"hook.bad_payload"}
	CodeToolChanged    = Code{"tool.changed_since_add"}
	CodeToolNameClash  = Code{"tool.name_clash"}
	CodeToolRepoPath   = Code{"tool.repo_path"}
	CodeToolNoFlag     = Code{"tool.no_launch_flag"}
	CodeToolWireFailed = Code{"tool.wire_failed"}
	CodeToolUnverified = Code{"tool.vendor_unverified"}
	CodeToolExec       = Code{"tool.exec_refused"}
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
	{EvCheckCommandFailed, LevelWarn, "check", "One of the repo's checks failed (attrs.index is its place in the list; attrs.failure is start, timeout, or exit, with attrs.exit_code or, for start, attrs.error_class).", "Run aggy check again to see its output tail; nothing of it is stored."},
	{EvCheckFinished, LevelInfo, "check", "The repo's checks ran; attrs hold how many passed and failed.", "check.command.failed records for the failures."},
	{EvGCRefused, LevelError, "gc", "aggy gc refused or could not plan; nothing was removed.", "The code's entry."},
	{EvGCRemoved, LevelInfo, "gc", "aggy gc removed one item (attrs.kind: worktree, child, inbox, research, or diag).", "Nothing; it is the record of the removal."},
	{EvGCRemoveFailed, LevelWarn, "gc", "aggy gc could not remove one item; it stays for the next run.", "The record's msg; aggy gc --dry-run."},
	{EvSetupChecked, LevelInfo, "setup", "aggy setup checked and recorded one vendor CLI (attrs: connected, version).", "aggy doctor for more."},
	{EvSetupFailed, LevelError, "setup", "aggy setup failed.", "The code's entry."},
	{EvDoctorError, LevelError, "doctor", "aggy doctor reported an error-level finding (attrs.finding is its doctor code).", "aggy doctor shows the finding in full."},
	{EvDoctorRefused, LevelError, "doctor", "aggy doctor did not run.", "The code's entry."},
	{EvToolRefused, LevelError, "tool", "aggy tool refused or failed; no definition or switch changed.", "The code's entry."},
	{EvToolDefRefused, LevelWarn, "tool", "A user tool definition was left out: invalid, untrusted, or colliding (attrs: file, its base name in ~/.aggy/tools, and key; never its argv).", "aggy tool list names the file and the key; fix or remove it."},
	{EvToolSwitched, LevelInfo, "tool", "aggy tool on or off recorded a switch (attrs: tool, vendor, repo, on).", "aggy tool status shows what is on where."},
	{EvLaunchRefused, LevelError, "supervisor", "aggy run, new, or resume did not put the CLI on screen, or the supervisor failed while it ran.", "The code's entry; " + showRun + "."},
	{EvLaunched, LevelInfo, "supervisor", "A CLI went on screen (attrs: new_child, brief, model_set, direct). Its trace is the one its hooks, switches, and borrows carry.", "aggy diag show <trace id> for the whole session."},
	{EvSessionExited, LevelInfo, "supervisor", "The on-screen CLI exited; exit_code and duration_ms say how, attrs.switch_to the vendor launched next, if any.", "A switch_to with no supervisor.child.launched after it: the next launch's supervisor.launch.refused."},
	{EvSwitchQueued, LevelInfo, "switch", "A switch waits for the turn end (attrs.origin: user, agent, or limit; brief, new, model_set).", "No switch.request.offered after it: the session never reached a quiet turn end; look for hook.event.accepted (Stop) and switch.request.refused."},
	{EvSwitchOffered, LevelInfo, "switch", "The turn ended and aggy asked the user to confirm the switch.", "switch.request.confirmed or switch.request.cancelled follows."},
	{EvSwitchConfirmed, LevelInfo, "switch", "The user pressed the switch key at the confirmation; aggy typed the CLI's exit command.", "switch.request.completed, or switch.exit.timed_out if the CLI did not exit."},
	{EvSwitchRefused, LevelWarn, "switch", "A switch was refused, or held back until the user acts; the code says why.", "The code's entry; " + showRun + "."},
	{EvSwitchCancelled, LevelInfo, "switch", "The user cancelled a waiting switch (attrs.how: esc_confirm or esc_list).", "Nothing, unless the user did not cancel it."},
	{EvSwitchCompleted, LevelInfo, "switch", "The CLI exited after aggy typed its exit command; the next vendor is launched.", "The next supervisor.child.launched in the same trace."},
	{EvSwitchExitLate, LevelWarn, "switch", "The CLI had not exited a while after aggy typed its exit command; the switch still stands.", "The CLI's screen: a turn or a menu may have taken the exit command."},
	{EvPickerOpened, LevelInfo, "switch", "The switch picker opened on the vendor list (attrs.preselect: a vendor suggested after a limit or a near-limit warning).", "switch.picker.chosen or switch.picker.dismissed follows."},
	{EvPickerChosen, LevelInfo, "switch", "A vendor was picked in the list (attrs: brief, new, model_set, again).", "switch.request.queued, or switch.request.refused with the reason."},
	{EvPickerDismissed, LevelInfo, "switch", "The picker was closed with Esc; a waiting switch, if any, was cancelled.", "Nothing."},
	{EvLimitReceived, LevelWarn, "switch", "The on-screen vendor reported its plan limit (attrs.unverified: only a hook said so, which any process in the CLI's tree can send).", "aggy log shows the limit; switch.request.queued or switch.limit.not_offered follows."},
	{EvLimitNotOffered, LevelInfo, "switch", "After a limit, aggy did not queue a switch itself; the code says why.", "The code's entry."},
	{EvNearLimitWarned, LevelInfo, "switch", "The near-limit warning was shown at a turn end (attrs: used_pct, warn_at_pct, window, to).", "With switch.near_limit_no_quota no vendor had quota; aggy usage."},
	{EvHookAccepted, LevelInfo, "hook", "The supervisor took a hook event of the vendor on screen from the CLI's process tree (attrs.event: Stop, StopFailure, StopCancelled, SessionStart, Notification, or other). A turn end has attrs.counted: true when it named the on-screen session and so may let a switch go on, false while that session's id is not known yet; any other event holds a switch (attrs.holds).", "A counted turn end that did not move a switch on: the switch.request.refused records near it."},
	{EvHookRejected, LevelWarn, "hook", "The supervisor did not act on a message on its socket; the code says why.", "The code's entry; aggy log shows the security ones as refusals."},
	{EvHookIgnored, LevelInfo, "hook", "A turn end from the CLI's tree named another session, such as a nested claude -p, so it did not end the on-screen turn. Recorded once a minute per session, and when the session ends (attrs.repeats: how many more came since the last record; attrs.tail: the record holds only the repeats of a session let go or ended; attrs.over_cap: counted with every session past the first 64 seen in a minute; attrs.no_session: it named no session, which this vendor's hook may do).", "Nothing, unless every turn end of the on-screen CLI is ignored: then its session id is not the one aggy recorded."},
	{EvHookForwardFailed, LevelWarn, "hook", "aggy hook could not hand a vendor hook event to the supervisor; the CLI went on undisturbed.", "The code's entry; aggy diag --trace <trace id> for the session."},
	{EvQueueDropped, LevelWarn, "diag", "The supervisor's diagnostic queue was full, so records were dropped (attrs.dropped is how many).", "The state dir's disk: a slow or full disk makes the log fall behind."},
	{EvToolWireRefused, LevelWarn, "tool", "A launch did not wire a tool that is on (attrs: tool, or none for every tool of the vendor); the session started without it.", "The code's entry; aggy tool status."},
	{EvToolWired, LevelInfo, "tool", "A launch wired the tools that are on into the CLI it started (attrs: tools, the names wired).", "aggy tool status shows why each is on."},
	{EvToolExecRefused, LevelError, "tool", "aggy tool exec, which a vendor CLI runs to start a tool's MCP server, refused to start it (attrs: tool, check: the step that failed). The CLI shows the server as failed.", "The code's entry; aggy tool status."},
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
	{CodeStateTempDir, true, "aggy's state dir, where accept writes the patch, is under a temp dir an edit child's sandbox may write.", "Set AGGY_HOME to a dir outside temp dirs."},
	{CodeAcceptOS, false, "aggy accept is not supported on this OS yet.", "Review the worktree and apply its diff by hand."},
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
	{CodeToolUnknown, false, "No built-in or loaded user tool has that name.", "aggy tool list; a refused user file is named there."},
	{CodeToolScope, false, "The tool has nothing for that vendor, or is a per-repo tool turned on without --repo.", "aggy tool status <name>; aggy tool on <name> --repo ."},
	{CodeToolRepoOff, false, "The repo's .aggy/config.toml lists the tool in tools_off; a repo config always wins, and can only turn tools off.", "Edit that repo's tools_off if it is your repo."},
	{CodeToolDelegated, true, "aggy tool add, remove, on, or off ran inside a borrow child (AGGY_DELEGATED is set).", "Run it from your own shell. A child that tries this was steered to; read its task."},
	{CodeToolInvalid, false, "A tool definition failed validation; the message names the file and the key.", "Fix the key; docs/design.md section 9 has the rules."},
	{CodeToolUntrusted, true, "A user tool definition, its tools folder, or the state dir is a symlink, not the user's own, or writable by group or others.", "chmod go-w and check the owner of ~/.aggy, ~/.aggy/tools, and the file."},
	{CodeToolStillOn, false, "aggy tool remove refused: the tool is still on somewhere.", "aggy tool off <name>, then remove it."},
	{CodeToolFailed, false, "Another aggy tool error: a file or the index could not be read or written.", "The record's msg; aggy doctor."},
	{CodeQueueFull, false, "The diagnostic log fell behind the supervisor and dropped records rather than hold up the terminal.", "The state dir's disk; aggy gc prunes the log."},
	{CodeNoTerminal, false, "aggy run, new, or resume needs a terminal on stdin.", "Run it in a terminal, not a pipe or a script."},
	{CodeLaunchFailed, false, "Another launch error: the parent or child could not be planned or recorded, the CLI could not start, or the supervisor failed.", "The record's msg; aggy doctor."},
	{CodeUnreachable, false, "The supervisor's socket did not answer: the session has ended, or the socket path is not aggy's.", "Is aggy still running the session? AGGY_SOCKET must be in ~/.aggy/run."},
	{CodePolicy, false, "The safety policy cannot be applied to the vendor's CLI, so aggy did not start it.", "aggy doctor; --no-policy or [policy] required = false start it anyway."},
	{CodeUnsentText, false, "Text the user typed and did not send is at the CLI's prompt: the exit command would send it, so the switch waits.", "Send or clear the text, then press Enter on the empty prompt."},
	{CodeAgentNoHold, false, "The agent asked for a switch away from a vendor with no hold signal (Codex), which only the user may start.", "Press aggy's switch key."},
	{CodeAgentOptions, false, "The agent asked for a new session or a model, which are the user's choice in the picker.", "Press aggy's switch key and choose them there."},
	{CodeSwitchVendor, false, "The vendor cannot be switched to now: on screen already, not offered, not allowed by the repo, not on PATH, or the safety policy cannot reach it.", "The record's msg; aggy setup; the repo's allowed_vendors."},
	{CodeSwitchBusy, false, "Another switch is already waiting.", "Wait for it, or cancel it in the picker."},
	{CodeSwitchHeld, false, "The CLI said it waits for the user (a permission prompt or a notice), so the switch waits.", "Answer the CLI, or send a message; the switch goes on at the next turn end."},
	{CodeNotSupervised, false, "aggy switch ran outside a session aggy supervises, or in one started with --direct.", "Press aggy's switch key in a session started with aggy run."},
	{CodeExitTimeout, false, "The CLI did not exit within the wait after aggy typed its exit command.", "The CLI's screen; cancel with the switch key, then Esc."},
	{CodeNoQuota, false, "After a limit, no vendor the picker offers has quota left.", "aggy usage; aggy log shows the limits."},
	{CodeLimitNoHold, false, "After a limit on a vendor with no hold signal (Codex), aggy only suggests the switch; the user picks it.", "Press the switch key, then Enter."},
	{CodeNearNoQuota, false, "The near-limit warning found no vendor with quota to hand off to.", "aggy usage."},
	{CodeChildGone, false, "The session picked in the switch list was no longer recorded when the switch was confirmed, so the CLI on screen was not told to exit.", "aggy sessions; pick again."},
	{CodeLineage, true, "A message on the supervisor's socket came from a process outside the on-screen CLI's process tree, so it was not acted on.", "Which process of yours writes to ~/.aggy/run; a CLI's tool that outlived it is reparented and falls out."},
	{CodePeerUID, true, "A process of another user reached the supervisor's socket; it got no answer.", "The run directory's owner and mode (0700)."},
	{CodeBadRequest, true, "A message on the supervisor's socket was not one aggy sends (bad JSON, an unknown type or field, a bad name).", "Which process writes to ~/.aggy/run; aggy hook and aggy switch send only valid messages."},
	{CodeForeignSession, false, "A turn end from the CLI's tree named another session (a nested CLI), so it did not end the on-screen turn.", "Nothing, unless the on-screen CLI's own turn ends are ignored: then its session id is not the one aggy recorded."},
	{CodeNoSession, true, "A turn end from the CLI's tree named no session, which the vendor's own hook always does, so it looks forged and was not acted on. Recorded once a minute, and when the session ends (attrs.repeats: how many more came since the last record; attrs.tail: the record holds only those).", "Which process in the session sends hook events: aggy hook forwards what the vendor gives it."},
	{CodePeerUnknown, false, "aggy could not tell which user or process opened a connection to the supervisor's socket, so it was closed unanswered.", "Usually a process that connected and exited at once; nothing, unless hooks stop arriving."},
	{CodeWrongVendor, true, "A hook event from the CLI's tree came from another vendor's hook than the one on screen, so it did not gate a switch.", "A nested CLI of another vendor in the session; nothing else."},
	{CodeBadPayload, false, "aggy hook could not read the vendor's hook payload, or it was not a JSON object.", "The vendor CLI's version; aggy doctor."},
	{CodeToolNameClash, false, "A tool that is on was not wired: the CLI already loads an MCP server of that name from the user's or the repo's config, and aggy never replaces or merges into one.", "Rename or remove that server in the CLI's config, or turn the tool off."},
	{CodeToolRepoPath, false, "A tool that is on was not wired: its arguments hold {repo}, and the session is not in a repo, or the repo's path holds a character other than letters, digits, and / . _ + @ -.", "Run the session in a repo whose path has only those characters."},
	{CodeToolNoFlag, false, "Tools that are on were not wired: the CLI's --help lacks the launch flag they need (Codex -c).", "Update the CLI; aggy setup shows its version."},
	{CodeToolWireFailed, false, "Tools that are on were not wired: the CLI's own MCP config could not be read to check names, or the session's tool file could not be written.", "The warning aggy printed; the record's class attr names the step. aggy doctor."},
	{CodeToolUnverified, false, "Tools were not turned on or wired for a vendor whose launch-time wiring is not verified yet (Grok: what --agent changes besides the MCP servers).", "Use the tool with Claude or Codex; docs/design.md, Launch wiring."},
	{CodeToolExec, true, "aggy tool exec refused to start a tool's MCP server: its folder is not aggy's private launch folder, the definition changed or is gone, the command is not the definition's, or a variable could not be passed.", "aggy tool status; if the definition changed, aggy tool on <name> again after reading it."},
	{CodeToolChanged, true, "aggy tool on or a --tool refused a user definition that is not the file aggy tool add showed, or (--tool) one it never recorded.", "aggy tool remove <name>, then aggy tool add the file again and read what it runs."},
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
