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
	EvBorrowRefused        = Event{"borrow.request.refused"}
	EvBorrowVendorSkipped  = Event{"borrow.vendor.skipped"}
	EvBorrowVersionChecked = Event{"borrow.version.checked"}
	EvBorrowChildStarted   = Event{"borrow.child.started"}
	EvBorrowChildExited    = Event{"borrow.child.exited"}
	EvBorrowFailed         = Event{"borrow.run.failed"}
	EvBorrowFinished       = Event{"borrow.run.finished"}
	EvClaudeLockdown       = Event{"adapter.claude.lockdown_rejected"}
	EvCodexLockdown        = Event{"adapter.codex.lockdown_rejected"}
	EvGrokLockdown         = Event{"adapter.grok.lockdown_rejected"}
	EvGrokSandboxRefused   = Event{"adapter.grok.sandbox_refused"}
	EvAcceptRefused        = Event{"accept.request.refused"}
	EvAcceptApplied        = Event{"accept.diff.applied"}
	EvAcceptDeclined       = Event{"accept.diff.declined"}
	EvGateRefused          = Event{"check.gate.refused"}
	EvGateDecided          = Event{"check.gate.decided"}
	EvCheckRefused         = Event{"check.run.refused"}
	EvCheckCommandFailed   = Event{"check.command.failed"}
	EvCheckFinished        = Event{"check.run.finished"}
	EvGCRefused            = Event{"gc.run.refused"}
	EvGCRemoved            = Event{"gc.item.removed"}
	EvGCRemoveFailed       = Event{"gc.item.failed"}
	EvSetupChecked         = Event{"setup.vendor.checked"}
	EvSetupFailed          = Event{"setup.run.failed"}
	EvDoctorError          = Event{"doctor.finding.error"}
	EvDoctorRefused        = Event{"doctor.run.refused"}
	EvToolRefused          = Event{"tool.request.refused"}
	EvToolDefRefused       = Event{"tool.def.refused"}
	EvToolSwitched         = Event{"tool.switch.changed"}
	EvToolsNotified        = Event{"tool.session.notified"}
	EvToolsChanged         = Event{"supervisor.tools.changed"}
	EvLaunchRefused        = Event{"supervisor.launch.refused"}
	EvCommandRefused       = Event{"cli.command.refused"}
	EvDirectRan            = Event{"cli.direct.exited"}
	EvLaunched             = Event{"supervisor.child.launched"}
	EvSessionExited        = Event{"supervisor.child.exited"}
	EvSwitchReturned       = Event{"supervisor.switch.returned"}
	EvSessionFollowed      = Event{"supervisor.session.followed"}
	EvCommandCaught        = Event{"supervisor.command.caught"}
	EvPromptRefused        = Event{"supervisor.command.refused"}
	EvCommandPassed        = Event{"supervisor.command.passed"}
	EvSwitchQueued         = Event{"switch.request.queued"}
	EvSwitchOffered        = Event{"switch.request.offered"}
	EvSwitchConfirmed      = Event{"switch.request.confirmed"}
	EvSwitchRefused        = Event{"switch.request.refused"}
	EvSwitchCancelled      = Event{"switch.request.cancelled"}
	EvOfferLapsed          = Event{"switch.offer.lapsed"}
	EvSwitchCompleted      = Event{"switch.request.completed"}
	EvSwitchExitLate       = Event{"switch.exit.timed_out"}
	EvPickerOpened         = Event{"switch.picker.opened"}
	EvPickerChosen         = Event{"switch.picker.chosen"}
	EvPickerDismissed      = Event{"switch.picker.dismissed"}
	EvLimitReceived        = Event{"switch.limit.received"}
	EvLimitNotOffered      = Event{"switch.limit.not_offered"}
	EvNearLimitWarned      = Event{"switch.near_limit.warned"}
	EvHookAccepted         = Event{"hook.event.accepted"}
	EvHookRejected         = Event{"hook.event.rejected"}
	EvHookIgnored          = Event{"hook.event.ignored"}
	EvHookForwardFailed    = Event{"hook.forward.failed"}
	EvQueueDropped         = Event{"diag.queue.dropped"}
	EvToolWireRefused      = Event{"tool.wire.refused"}
	EvToolWired            = Event{"tool.wire.done"}
	EvToolExecRefused      = Event{"tool.exec.refused"}
	EvToolIndexed          = Event{"tool.index.done"}
	EvToolIndexRefused     = Event{"tool.index.refused"}
	EvToolProxyStarted     = Event{"tool.proxy.started"}
	EvToolProxyRefused     = Event{"tool.proxy.refused"}
	EvToolProxyStopped     = Event{"tool.proxy.stopped"}
	EvToolProxyExited      = Event{"tool.proxy.exited"}
	EvHookToolFiltered     = Event{"hook.tool.filtered"}
	EvHookToolFailed       = Event{"hook.tool.failed"}
	EvGuardRefused         = Event{"hook.guard.refused"}
	EvGuardMissing         = Event{"hook.guard.missing"}
	EvDecideStarted        = Event{"hook.decision.started"}
	EvDecideEnded          = Event{"hook.decision.ended"}
	EvDecideFallback       = Event{"hook.decision.fallback"}
	EvHermesRefused        = Event{"hook.hermes.refused"}
	EvHermesProfile        = Event{"launch.hermes.profile"}
	EvTrustRefused         = Event{"trust.request.refused"}
	EvPermissionsRefused   = Event{"permissions.request.refused"}
	EvConfigRefused        = Event{"config.request.refused"}
	EvKeyRefused           = Event{"key.request.refused"}
	EvKeyAdded             = Event{"key.item.added"}
	EvKeyRemoved           = Event{"key.item.removed"}
	EvKeyPassed            = Event{"launch.key.passed"}
	EvKeyNotPassed         = Event{"launch.key.not_passed"}
)

// Codes.
var (
	CodeUsage            = Code{"cli.usage"}
	CodeConfig           = Code{"cli.config_invalid"}
	CodeFlagClash        = Code{"cli.flag_clash"}
	CodeFlagLoosens      = Code{"cli.flag_needs_no_policy"}
	CodeBorrowNoScreen   = Code{"borrow.no_screen"}
	CodeNotFound         = Code{"cli.not_found"}
	CodeAmbiguousID      = Code{"cli.ambiguous_id"}
	CodeNotAllowed       = Code{"cli.vendor_not_allowed"}
	CodeNotInRepo        = Code{"cli.not_in_repo"}
	CodeCommandFailed    = Code{"cli.failed"}
	CodeWireFailed       = Code{"wire.failed"}
	CodeUnwireRefused    = Code{"unwire.refused"}
	CodeUnwireFailed     = Code{"unwire.failed"}
	CodeStore            = Code{"store.unavailable"}
	CodeOneHop           = Code{"borrow.one_hop"}
	CodeBorrowInvalid    = Code{"borrow.invalid_request"}
	CodeGrokEdit         = Code{"borrow.grok_edit"}
	CodeNoVendor         = Code{"borrow.no_vendor"}
	CodeBorrowStart      = Code{"borrow.start_failed"}
	CodeCLIMissing       = Code{"vendor.cli_missing"}
	CodeUnsupported      = Code{"adapter.unsupported"}
	CodeNoCapability     = Code{"adapter.no_capability"}
	CodeFileTools        = Code{"adapter.file_tools_off"}
	CodeNoSandbox        = Code{"sandbox.missing"}
	CodeUntested         = Code{"lockdown.untested_version"}
	CodeVersionCheck     = Code{"lockdown.version_check_failed"}
	CodeLockdown         = Code{"lockdown.rejected"}
	CodeRateLimit        = Code{"vendor.rate_limit"}
	CodeAuth             = Code{"vendor.auth"}
	CodeBilling          = Code{"vendor.billing"}
	CodeServer           = Code{"vendor.server"}
	CodeVendorInvalid    = Code{"vendor.invalid_request"}
	CodeMaxTurns         = Code{"borrow.max_turns"}
	CodeTimeout          = Code{"borrow.timeout"}
	CodeInterrupted      = Code{"borrow.interrupted"}
	CodeChildFailed      = Code{"borrow.child_failed"}
	CodePermissionPrompt = Code{"borrow.permission_prompt"}
	CodeAcceptTarget     = Code{"accept.target_invalid"}
	CodeProtected        = Code{"accept.protected_path"}
	CodeTreeChanged      = Code{"accept.worktree_changed"}
	CodeDirty            = Code{"accept.checkout_dirty"}
	CodeAcceptFailed     = Code{"accept.failed"}
	CodeStateTempDir     = Code{"accept.state_temp_dir"}
	CodeAcceptOS         = Code{"accept.unsupported_os"}
	CodeGateTempDir      = Code{"check.gate_temp_dir"}
	CodeChecksChanged    = Code{"check.tree_changed"}
	CodeGateFailed       = Code{"check.gate_failed"}
	CodeChecksFailed     = Code{"check.failed"}
	CodeNotGit           = Code{"check.not_git"}
	CodeEditWorktree     = Code{"check.edit_worktree"}
	CodeNoChecks         = Code{"check.no_checks"}
	CodeGCFailed         = Code{"gc.failed"}
	CodeGCRemoveFailed   = Code{"gc.remove_failed"}
	CodeDoctorError      = Code{"doctor.error_finding"}
	CodeDiagWrite        = Code{"diag.write_failed"}
	CodeToolUnknown      = Code{"tool.unknown"}
	CodeToolScope        = Code{"tool.not_applicable"}
	CodeToolRepoOff      = Code{"tool.repo_off"}
	CodeToolDelegated    = Code{"tool.delegated"}
	CodeToolInvalid      = Code{"tool.def_invalid"}
	CodeToolUntrusted    = Code{"tool.def_untrusted"}
	CodeToolStillOn      = Code{"tool.still_on"}
	CodeToolFailed       = Code{"tool.failed"}
	CodeQueueFull        = Code{"diag.queue_full"}
	CodeNoTerminal       = Code{"supervisor.no_terminal"}
	CodeLaunchFailed     = Code{"supervisor.launch_failed"}
	CodeUnreachable      = Code{"supervisor.unreachable"}
	CodePolicy           = Code{"policy.unreachable"}
	CodeUnsentText       = Code{"switch.unsent_text"}
	CodePromptDoubt      = Code{"switch.prompt_in_doubt"}
	CodeNoTurnYet        = Code{"switch.no_turn_yet"}
	CodeNeverStarted     = Code{"switch.never_started"}
	CodeHooksMissing     = Code{"switch.hooks_missing"}
	CodeFollowRefused    = Code{"hook.follow_refused"}
	CodeAgentNoHold      = Code{"switch.agent_no_hold"}
	CodeAgentOptions     = Code{"switch.agent_options"}
	CodeSwitchVendor     = Code{"switch.vendor_unavailable"}
	CodeSwitchBusy       = Code{"switch.already_waiting"}
	CodeSwitchHeld       = Code{"switch.held"}
	CodeCommandNotTaken  = Code{"command.not_taken"}
	CodeCommandKeyboard  = Code{"command.keyboard_only"}
	CodeCommandNoEnter   = Code{"command.no_enter"}
	CodeNotSupervised    = Code{"switch.not_supervised"}
	CodeExitTimeout      = Code{"switch.exit_timeout"}
	CodeNoQuota          = Code{"switch.no_quota"}
	CodeLimitNoHold      = Code{"switch.limit_no_hold"}
	CodeNearNoQuota      = Code{"switch.near_limit_no_quota"}
	CodeChildGone        = Code{"switch.child_gone"}
	CodeSwitchBlocked    = Code{"switch.blocked"}
	CodeToolsNoHold      = Code{"switch.tools_no_hold"}
	CodeLineage          = Code{"peer.lineage_failed"}
	CodePeerUID          = Code{"peer.uid_mismatch"}
	CodeBadRequest       = Code{"socket.bad_request"}
	CodeForeignSession   = Code{"hook.foreign_session"}
	CodeNoSession        = Code{"hook.no_session"}
	CodeGrokShutdown     = Code{"hook.grok_shutdown"}
	CodePeerUnknown      = Code{"peer.cred_failed"}
	CodeWrongVendor      = Code{"hook.wrong_vendor"}
	CodeBadPayload       = Code{"hook.bad_payload"}
	CodeGuardRefused     = Code{"guard.refused"}
	CodeGuardMissing     = Code{"guard.no_report"}
	// The decision hook's end records (T6976).
	CodeDecideNoMatch     = Code{"decision.no_match"}
	CodeDecidePreApproved = Code{"decision.pre_approved"}
	CodeDecideAsked       = Code{"decision.policy_match"}
	CodeDecideGroupDenied = Code{"decision.group_denied"}
	CodeDecideNoPrompt    = Code{"decision.no_prompt"}
	CodeDecideUnreadable  = Code{"decision.call_unreadable"}
	CodeDecidePlan        = Code{"decision.plan_unreadable"}
	CodeDecidePlanChanged = Code{"decision.plan_changed"}
	CodeDecideProtected   = Code{"decision.protected_path"}
	CodeDecideOutside     = Code{"decision.outside_repo"}
	CodeDecideUnjudged    = Code{"decision.unjudged_tool"}
	CodeDecideTooLong     = Code{"decision.call_too_long"}
	CodeDecideOtherTool   = Code{"decision.other_tool"}
	CodeDecideFallback    = Code{"decision.fallback"}
	// Hermes Agent (T6933).
	CodeHermesNoLaunch   = Code{"hermes.no_launch"}
	CodeHermesProfile    = Code{"hermes.profile_failed"}
	CodeHermesUnclaimed  = Code{"hermes.launch_unclaimed"}
	CodeHermesClaimLate  = Code{"hermes.claim_late"}
	CodeHermesDirectBusy = Code{"hermes.direct_shared"}
	CodeHermesUntrusted  = Code{"hermes.not_ticked"}
	CodeTrustBadDir      = Code{"trust.bad_dir"}
	CodeTrustBadFile     = Code{"trust.bad_file"}
	CodeNotAtKeyboard    = Code{"permissions.not_at_keyboard"}
	CodePermBadRule      = Code{"permissions.bad_rule"}
	CodePermBadFile      = Code{"permissions.bad_file"}
	CodePermNotFound     = Code{"permissions.not_found"}
	CodePermScope        = Code{"permissions.no_repo"}
	CodeConfigBadValue   = Code{"config.bad_value"}
	CodeConfigByHand     = Code{"config.edit_by_hand"}
	CodeToolChanged      = Code{"tool.changed_since_add"}
	CodeToolNameClash    = Code{"tool.name_clash"}
	CodeToolRepoPath     = Code{"tool.repo_path"}
	CodeToolNoFlag       = Code{"tool.no_launch_flag"}
	CodeToolWireFailed   = Code{"tool.wire_failed"}
	CodeToolUnverified   = Code{"tool.vendor_unverified"}
	CodeToolExec         = Code{"tool.exec_refused"}
	CodeToolHookApproves = Code{"tool.hook_approves"}
	CodeToolHookOutput   = Code{"tool.hook_output"}
	CodeToolHookFailed   = Code{"tool.hook_failed"}
	CodeToolSkillMissing = Code{"tool.skill_missing"}
	CodeToolBorrowKind   = Code{"tool.borrow_kind"}
	CodeToolUnlisted     = Code{"tool.borrow_unlisted"}
	CodeToolNetwork      = Code{"tool.borrow_network"}
	CodeToolSkillChanged = Code{"tool.skill_changed"}
	CodeToolIndexFailed  = Code{"tool.index_failed"}
	CodeToolIndexCopy    = Code{"tool.index_copy_refused"}
	CodeToolSkillExists  = Code{"tool.skill_exists"}
	CodeToolSkillRefused = Code{"tool.skill_refused"}
	CodeToolSkillSwapped = Code{"tool.skill_swapped"}
	CodeProxyVendor      = Code{"tool.proxy_vendor_needed"}
	CodeProxyConfirm     = Code{"tool.proxy_not_confirmed"}
	CodeProxyPortTaken   = Code{"tool.proxy_port_taken"}
	CodeProxyUnhealthy   = Code{"tool.proxy_unhealthy"}
	CodeProxyFailed      = Code{"tool.proxy_failed"}
	CodeProxyConflict    = Code{"tool.proxy_conflict"}
	CodeProxyNotOwned    = Code{"tool.proxy_not_owned"}
	CodeProxyEnvProxy    = Code{"tool.proxy_env_proxy"}
	CodeProxyHoldFailed  = Code{"tool.proxy_hold_failed"}
	// aggy key (T6931).
	CodeKeyUnsupported   = Code{"key.unsupported"}
	CodeKeyBadName       = Code{"key.bad_name"}
	CodeKeyProviderTaken = Code{"key.provider_taken"}
	CodeKeyHelperFailed  = Code{"key.helper_failed"}
	CodeKeyNotPassed     = Code{"key.not_passed"}
)

const showRun = "aggy diag show <run id> for what led to it"

var events = []EventInfo{
	{EvBorrowRefused, LevelError, "borrow", "aggy borrow refused or failed outside the child's own run: before a child started, or while recording one.", "The code's entry; " + showRun + "."},
	{EvBorrowVendorSkipped, LevelInfo, "borrow", "A candidate vendor could not start the borrow, and the next one was tried.", "The code says why; aggy setup and aggy doctor for that vendor."},
	{EvBorrowVersionChecked, LevelInfo, "borrow", "A check of a CLI newer than the tested range ran; attrs hold the check, its result, and the version.", "A failed check is cached for that version; a new CLI version or aggy release checks again."},
	{EvBorrowChildStarted, LevelInfo, "borrow", "The borrow was recorded and its child CLI started.", "A started borrow with no borrow.run.finished was cut short: aggy diag show <borrow id>."},
	{EvBorrowChildExited, LevelInfo, "borrow", "The child CLI exited; exit_code and duration_ms say how.", "A non-zero exit is classed by the next record (borrow.run.failed)."},
	{EvBorrowFailed, LevelWarn, "borrow", "The borrow ran and failed; the code is the vendor's error class.", "The code's entry; aggy inbox <borrow id> for the report."},
	{EvBorrowFinished, LevelInfo, "borrow", "The borrow ended and was recorded; attrs hold the outcome and tokens.", "aggy log and aggy stats show it too."},
	{EvClaudeLockdown, LevelError, "adapter.claude", "Claude Code rejected a lockdown flag; the run was discarded.", "aggy doctor for the version; attach aggy diag bundle to an issue."},
	{EvCodexLockdown, LevelError, "adapter.codex", "Codex rejected a lockdown flag; the run was discarded.", "aggy doctor for the version; attach aggy diag bundle to an issue."},
	{EvGrokLockdown, LevelError, "adapter.grok", "Grok rejected a lockdown flag, reported an unknown tool, or ran without its sandbox; the run was discarded.", "aggy doctor for the version; attach aggy diag bundle to an issue."},
	{EvGuardRefused, LevelWarn, "hook", "aggy's OpenCode guard refused a tool call: a variable declaration in a shell command, a real path outside the repo, a call from another folder, or opencode auth export.", "The code's entry; the model saw the same reason."},
	{EvGuardMissing, LevelWarn, "hook", "aggy's OpenCode guard did not report in time, so its refusals may not be running; a plugin that fails to load gives no error.", "The code's entry."},
	{EvDecideStarted, LevelInfo, "hook", "aggy's policy decision hook started on one tool call (attrs.vendor_version). It never records the command or the path.", "A start with no hook.decision.ended (cause_id) is a hang or a kill: aggy doctor counts them per CLI version."},
	{EvDecideEnded, LevelInfo, "hook", "aggy's policy decision hook answered (attrs.decision: allow, ask, deny, no-opinion, or error; attrs.group; the code says why; duration_ms).", "The code's entry; aggy doctor sums them per CLI version."},
	{EvDecideFallback, LevelWarn, "hook", "aggy hook decide failed or was missing, and its wrapper's fixed answer (ask, or deny where no prompt can show) went to the CLI instead.", "aggy doctor; a hook.decision.ended with decision.plan_unreadable, or none, in the same trace says more."},
	{EvHermesRefused, LevelWarn, "hook", "aggy's Hermes hook found no aggy launch for the profile and session its payload named, so a tool call was refused (pre_tool_call) or a turn end was dropped. It never records the payload.", "The code's entry."},
	{EvHermesProfile, LevelInfo, "launch", "A Hermes launch made aggy's profile for the repo (hermes profile create) or rewrote aggy's block in its config.yaml; attrs.created and attrs.written say which.", "aggy unwire --run <id> takes the block out; hermes profile delete removes the profile."},
	{EvTrustRefused, LevelError, "trust", "aggy trust refused; nothing was changed.", "The code's entry."},
	{EvPermissionsRefused, LevelError, "permissions", "aggy permissions refused; nothing was changed.", "The code's entry."},
	{EvConfigRefused, LevelError, "config", "aggy config set refused; nothing was changed.", "The code's entry."},
	{EvKeyRefused, LevelError, "key", "aggy key add or remove refused or failed. The record never holds the key's name or value.", "The code's entry."},
	{EvKeyAdded, LevelInfo, "key", "aggy key add stored a key in the OS keychain and its name in the index (attrs: provider). The value went from the keyboard to the keychain tool and never through aggy.", "aggy key list."},
	{EvKeyRemoved, LevelInfo, "key", "aggy key remove deleted a key from the OS keychain and the index (attrs: provider).", "aggy key list."},
	{EvKeyPassed, LevelInfo, "launch", "An OpenCode launch read a key from the OS keychain and handed it to OpenCode in a 0600 file in the launch's own folder, removed when the CLI exits (attrs: provider). Any process OpenCode starts can read it.", "Nothing; aggy key remove <name> stops it."},
	{EvKeyNotPassed, LevelWarn, "launch", "An OpenCode launch did not hand a stored key to OpenCode, so its provider is not offered in that session (attrs: provider).", "The code's entry; the line aggy printed at launch."},
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
	{EvToolsNotified, LevelInfo, "tool", "aggy tool on or off told running sessions their tools may have changed (attrs: sessions told, failed).", "supervisor.tools.changed in each session's trace."},
	{EvToolsChanged, LevelInfo, "supervisor", "A running session's tools for its vendor and repo differ from its launch's; it offers a resume of the same session at the turn end (attrs: changed, how: queued, note, waiting, busy, or reverted).", "switch.request.offered follows at a quiet turn end."},
	{EvCommandRefused, LevelError, "cli", "A command refused its arguments or failed; the code says why.", "The code's entry; " + showRun + "."},
	{EvDirectRan, LevelInfo, "cli", "aggy ran a vendor CLI on your terminal with no supervisor and recorded no session: print mode, a stdin that is no terminal, a flag that starts no session here, or a vendor subcommand (attrs: mode, policy; exit_code).", "Nothing: what the CLI printed is its own. aggy -c cannot reopen such a run."},
	{EvLaunchRefused, LevelError, "supervisor", "aggy run, new, or resume did not put the CLI on screen, or the supervisor failed while it ran.", "The code's entry; " + showRun + "."},
	{EvLaunched, LevelInfo, "supervisor", "A CLI went on screen (attrs: new_child, brief, model_set, direct). Its trace is the one its hooks, switches, and borrows carry.", "aggy diag show <trace id> for the whole session."},
	{EvSessionFollowed, LevelInfo, "supervisor", "The on-screen CLI moved to another session (/clear, /resume), so turn ends of that one count now (attrs.via: the evidence).", "If aggy resume does not reopen it, its session file was not found: aggy keeps the id it had."},
	{EvSwitchReturned, LevelWarn, "supervisor", "A CLI switched to closed before it started, so aggy reopened the CLI before it (attrs.from, attrs.to, attrs.exit).", "Its code's fix; the child that never started is not kept."},
	{EvSessionExited, LevelInfo, "supervisor", "The on-screen CLI exited; exit_code and duration_ms say how, attrs.switch_to the vendor launched next, if any.", "A switch_to with no supervisor.child.launched after it: the next launch's supervisor.launch.refused."},
	{EvCommandCaught, LevelInfo, "supervisor", "A command inside the session (/switch, /inbox, /aggy ...) was taken before it reached the CLI, so it started no model turn (attrs.command: its fixed name, never its arguments; attrs.via: keys, typed in full at the prompt, list, picked from one of aggy's lists, or hook, a CLI's prompt hook; attrs.confirmed: the person pressed y to run what the page showed).", "Nothing. A command that went to the CLI instead was not typed in full: see docs/design.md, section 5."},
	{EvCommandPassed, LevelInfo, "supervisor", "An Enter ended a line that reads as one of aggy's commands, or a line aggy lost track of that began with / or $, and the key catch let it through to the CLI. attrs.command is its fixed name (absent for a lost line); attrs.why lists fixed reason names (docs/logging.md, \"Why a command went to the CLI\"); attrs.unsettled says why the last turn end left the composer unknown; attrs.skipped counts misses not recorded since the last one (at most 5 a minute). It never holds the typed text.", "Read attrs.why: docs/logging.md says what each reason means. A command picked from the CLI's menu is caught only by its prompt hook (aggy hooks install)."},
	{EvPromptRefused, LevelWarn, "supervisor", "A command a CLI's prompt hook reported was not taken, so aggy hook let the prompt through to the CLI, whose skill tells the model to change nothing; the code says why.", "The code's entry. Typed in full at the prompt, the command is caught from the keys instead."},
	{EvSwitchQueued, LevelInfo, "switch", "A switch waits for the turn end (attrs.origin: user, agent, or limit; brief, new, model_set).", "No switch.request.offered after it: the session never reached a quiet turn end; look for hook.event.accepted (Stop) and switch.request.refused."},
	{EvSwitchOffered, LevelInfo, "switch", "The turn ended and aggy asked the user to confirm the switch.", "switch.request.confirmed or switch.request.cancelled follows."},
	{EvSwitchConfirmed, LevelInfo, "switch", "The user pressed the switch key at the confirmation; aggy typed the CLI's exit command.", "switch.request.completed, or switch.exit.timed_out if the CLI did not exit."},
	{EvSwitchRefused, LevelWarn, "switch", "A switch was refused, or held back until the user acts; the code says why.", "The code's entry; " + showRun + "."},
	{EvSwitchCancelled, LevelInfo, "switch", "A waiting switch ended: the user cancelled it (attrs.how: esc_confirm or esc_list), or the CLI they were told to quit crashed or was signalled (unclean_exit), quit too long after the pick (stale_pick), or aggy got a signal before typing the exit command (signalled).", "Nothing, unless the user did not cancel it."},
	{EvOfferLapsed, LevelInfo, "switch", "A switch's status line closed with no yes or no, and the switch still waits (attrs.how: typed, other keys went to the CLI; picker_open or not_at_prompt, the check after a yes found aggy's list open or a turn running). The switch key brings the status line back.", "Press the switch key at the prompt to see the question again."},
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
	{EvHookToolFiltered, LevelInfo, "hook", "aggy hook-tool dropped part of a tool hook's answer: an allow, an updatedInput from a hook not declaring rewrites or other than its rewrite_prefix in front of the original command, a bad reason or input, or keys outside its allowlist (attrs: tool, dropped, fixed names only: permissionDecision_allow, permissionDecision_other, permissionDecisionReason_invalid, updatedInput_undeclared, updatedInput_not_object, updatedInput_unexpected, a known key, or other).", "Nothing; a tool's hook may never approve an action."},
	{EvHookToolFailed, LevelWarn, "hook", "aggy hook-tool passed nothing on for a tool's hook; the CLI went on as without the hook (attrs.tool).", "The code's entry."},
	{EvToolWired, LevelInfo, "tool", "A launch wired the tools that are on into the CLI it started (attrs: tools, the names wired).", "aggy tool status shows why each is on."},
	{EvToolIndexed, LevelInfo, "tool", "A built-in's per-repo index was built (aggy tool on --repo, attrs.step init) or copied into a linked worktree at a launch (attrs.step copy) (attrs: tool).", "Nothing; the tool's server keeps the index fresh."},
	{EvToolIndexRefused, LevelWarn, "tool", "A built-in's per-repo index was not built or not copied (attrs: tool, step: init or copy). aggy tool on refused for init; a launch went on without the copy.", "The code's entry; the warning aggy printed."},
	{EvToolProxyStarted, LevelInfo, "tool", "A launch started a tool's local proxy and pointed the on-screen CLI at it (attrs: tool). Every request of that CLI, its login token included, passes through the proxy until the CLI exits.", "aggy tool status shows where it is on; aggy tool off <name> --vendor <vendor> turns it off for the next launch."},
	{EvToolProxyRefused, LevelWarn, "tool", "A launch did not use a tool's proxy that is on (attrs: tool, class: the step); the CLI started without it, talking to its vendor directly.", "The code's entry; the warning aggy printed."},
	{EvToolProxyStopped, LevelInfo, "tool", "The CLI exited and aggy stopped the tool's proxy, its whole process group (attrs: tool).", "Nothing."},
	{EvToolProxyExited, LevelWarn, "tool", "A tool's proxy exited on its own while its CLI ran (attrs: tool, held). aggy took its loopback address with a placeholder that reads nothing, so the CLI's requests failed rather than reaching another process; code tool.proxy_hold_failed when it could not.", "Run the proxy by hand to see why it stops; accept the resume aggy offers, or restart the session."},
	{EvToolExecRefused, LevelError, "tool", "aggy tool exec, which a vendor CLI runs to start a tool's MCP server, refused to start it (attrs: tool, check: the step that failed). The CLI shows the server as failed.", "The code's entry; aggy tool status."},
}

var codes = []CodeInfo{
	{CodeUsage, false, "Bad arguments or flags.", "Run the command with --help."},
	{CodeFlagLoosens, false, "A flag for the vendor CLI loosens approvals, the sandbox, or hook trust, or aggy cannot tell that it does not.", "Add --no-policy to start the session that way; aggy notes it at launch."},
	{CodeBorrowNoScreen, false, "A research or news borrow's session does not go on screen: it has no file or MCP tools by rule, and its transcript may hold text from the web.", "aggy inbox shows its report; aggy borrow asks a follow-up."},
	{CodeFlagClash, false, "A flag for the vendor CLI clashes with what aggy passes it at launch.", "Drop the flag; the message says what aggy does instead. Run the CLI itself if you need it."},
	{CodeNotAllowed, false, "This repo's allowed_vendors does not list that vendor.", "The allowed_vendors key in the repo's .aggy/config.toml, which the refusal names."},
	{CodeAmbiguousID, false, "More than one session or borrow starts with that id.", "Give more of the id."},
	{CodeNotInRepo, false, "The command needs a git work tree, and this directory is in none.", "Run it inside the repo."},
	{CodeCommandFailed, false, "The command could not finish; the message says what failed.", "The message; " + showRun + "."},
	{CodeWireFailed, false, "aggy could not make or plan a config change (hooks, skill, memory, MCP servers, policy, or attribution); what was applied is in the manifest.", "The message; aggy unwire undoes what was applied."},
	{CodeUnwireRefused, false, "aggy unwire left a change as it is: the file changed after aggy changed it, or the change cannot be undone safely.", "The message says how to undo it by hand, or to see what --force would overwrite."},
	{CodeUnwireFailed, false, "aggy unwire tried to undo a change and could not write the file.", "The file's permissions and disk."},
	{CodeNotFound, false, "No session, borrow, change, or file has that id or name.", "List them: aggy sessions, aggy session, aggy inbox, or aggy log."},
	{CodeConfig, false, "A config file could not be read or is invalid.", "The message names the file and key; fix it."},
	{CodeStore, false, "The index (~/.aggy/aggy.db) could not be opened, read, or written.", "aggy doctor; the state dir's permissions and free space."},
	{CodeOneHop, true, "aggy borrow, aggy gc, or a launch of a vendor CLI ran inside a borrow child (AGGY_DELEGATED is set): borrows are one hop.", "Run it from your own shell. A child that tries this was steered to; read its task."},
	{CodeBorrowInvalid, false, "The borrow request failed validation: an empty or too long task, a bad path or constraint, or a flag the kind does not take.", "aggy borrow --help."},
	{CodeGrokEdit, false, "aggy image --edit named an image Grok made. Grok keeps a session under its working directory, and the borrow's temp dir is gone, so a resumed child cannot start; no child ran.", "Make a new image with a full brief, or make it with --vendor codex, whose images --edit can change."},
	{CodeNoVendor, false, "No candidate vendor could start the borrow.", "aggy diag --run <run id> --event borrow.vendor.skipped lists why each was passed over; aggy setup."},
	{CodeBorrowStart, false, "The borrow failed after a vendor was chosen, while making its dirs, worktree, or records (the concurrency caps included).", showRun + "; aggy doctor."},
	{CodeCLIMissing, false, "The vendor's CLI is not on PATH, or its probe failed.", "Install it from the page the refusal names (at a terminal aggy offers an installed vendor instead); aggy setup."},
	{CodeUnsupported, false, "The vendor's adapter does not run this kind or request on this CLI or in this repo.", "The README's vendor support table; another --vendor."},
	{CodeNoCapability, false, "The CLI's --help lacks a flag the lockdown needs.", "Update the CLI; aggy setup shows its version."},
	{CodeFileTools, false, "The request needs Grok's media file tools, which are off.", "[media] grok_file_tools in ~/.aggy/config.toml, after reading what it uploads."},
	{CodeNoSandbox, false, "The vendor's sandbox is not available on this CLI or host: for a borrow's kind, or for an on-screen Claude Code session whose folder chose it, when the person said not to start without it or no one was at a terminal to ask.", "Grok needs --sandbox strict in its --help and a kernel sandbox (macOS, or Linux with Landlock); Claude Code's needs macOS, or Linux or WSL2 with bubblewrap and socat installed (aggy doctor), or aggy trust to untick it."},
	{CodeUntested, false, "The CLI's version is below the tested range, a new major version, or unreadable, so aggy does not borrow on it.", "aggy doctor; at a terminal aggy asks to run it once with its lockdown still checked; --allow-untested runs it with no checks."},
	{CodeVersionCheck, false, "A CLI newer than the tested range failed or could not finish aggy's check on this machine (a lockdown flag gone from help, a Codex feature not reviewed, or the canary self-test), so routing passed it over.", "The passed over line names which; aggy diag --event borrow.version.checked; --allow-untested runs it anyway; a result that was not final is tried again on the next borrow."},
	{CodePermissionPrompt, false, "The child stopped a tool at a permission prompt, which a headless run cannot answer, so the tool did not run (Grok: \"was not run, as a headless run cannot ask for approval\").", "aggy doctor for the CLI's version; attach aggy diag bundle to an issue: aggy's allow rules for that kind may need a change."},
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
	{CodeProtected, true, "The diff touches .git, a symlink or submodule, the hooks path, or a file that holds hooks, CI, config, or instructions.", "At a terminal aggy accept shows those files and their hunks and asks; with none, review the worktree by hand with the commands the refusal prints. A .git path, a symlink, or a submodule is never accepted."},
	{CodeTreeChanged, true, "The worktree changed after the borrow ended (edits in an on-screen session of this child, or a process it left behind?).", "Review the worktree by hand."},
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
	{CodeToolDelegated, true, "aggy tool add, remove, on, or off, or aggy skill add or remove, ran inside a borrow child (AGGY_DELEGATED is set).", "Run it from your own shell. A child that tries this was steered to; read its task."},
	{CodeToolInvalid, false, "A tool definition failed validation; the message names the file and the key.", "Fix the key; docs/design.md section 9 has the rules."},
	{CodeToolUntrusted, true, "A user tool definition, its tools folder, or the state dir is a symlink, not the user's own, or writable by group or others.", "chmod go-w and check the owner of ~/.aggy, ~/.aggy/tools, and the file."},
	{CodeToolStillOn, false, "aggy tool remove or aggy skill remove refused, or aggy skill add (a fresh name too): the tool is still on somewhere, or its skill still linked into a CLI.", "aggy tool off <name>, then run the command again."},
	{CodeToolFailed, false, "Another aggy tool error: a file or the index could not be read or written.", "The record's msg; aggy doctor."},
	{CodeQueueFull, false, "The diagnostic log fell behind the supervisor and dropped records rather than hold up the terminal.", "The state dir's disk; aggy gc prunes the log."},
	{CodeNoTerminal, false, "aggy run, new, or resume needs a terminal on stdin.", "Run it in a terminal, not a pipe or a script."},
	{CodeLaunchFailed, false, "Another launch error: the parent or child could not be planned or recorded, the CLI could not start, or the supervisor failed.", "The record's msg; aggy doctor."},
	{CodeUnreachable, false, "The supervisor's socket did not answer: the session has ended, or the socket path is not aggy's.", "Is aggy still running the session? AGGY_SOCKET must be in ~/.aggy/run."},
	{CodePolicy, false, "The safety policy cannot be applied to the vendor's CLI, so aggy did not start it.", "aggy doctor; --no-policy or [policy] required = false start it anyway."},
	{CodePromptDoubt, false, "aggy cannot tell if the CLI's prompt is empty: a command, a popup, or Esc may have left text it did not count. The switch waits until the person has looked.", "Look at the prompt, and pick the vendor again to confirm."},
	{CodeUnsentText, false, "Text the user typed and did not send is at the CLI's prompt: the exit command would send it, so the switch waits.", "Send or clear the text, then press Enter on the empty prompt."},
	{CodeHooksMissing, false, "aggy's hooks are not installed for the CLI on screen, so aggy can see no turn end to switch at.", "aggy hooks install (aggy offers it on its page), then a new session. Until then, quit the CLI yourself after picking a vendor."},
	{CodeFollowRefused, false, "A hook said the CLI moved to another session, from outside where the CLI's own hooks run (a nested CLI, say), so aggy kept following the one it had.", "Nothing, unless the on-screen CLI's own /clear or /resume is refused: then a switch waits; pick the vendor again to say the turn is over."},
	{CodeNeverStarted, false, "The CLI switched to closed with no turn ended and no session saved (a dialog declined, or quit before the first message), so aggy went back to the CLI before it.", "Accept the CLI's dialog in that folder, or send it a message before quitting it."},
	{CodeNoTurnYet, false, "No turn has ended since the CLI launched, so it may be showing a dialog: aggy does not type the exit command into it.", "Quit the CLI yourself and the switch completes if it exits cleanly; after a message, aggy asks at that turn's end if it sees one."},
	{CodeAgentNoHold, false, "The agent asked for a switch away from a vendor with no hold signal (Codex), which only the user may start.", "Press aggy's switch key."},
	{CodeAgentOptions, false, "The agent asked for a new session or a model, which are the user's choice in the picker.", "Press aggy's switch key and choose them there."},
	{CodeSwitchVendor, false, "The vendor cannot be switched to now: on screen already, not offered, not allowed by the repo, not on PATH, or the safety policy cannot reach it.", "The record's msg; aggy setup; the repo's allowed_vendors."},
	{CodeSwitchBusy, false, "Another switch is already waiting.", "Wait for it, or cancel it in the picker."},
	{CodeCommandNotTaken, false, "A CLI's prompt hook reported a line that is not a command this session takes from a hook: not one at all, one with a model turn, another vendor's, or one from a CLI that has no such hook under aggy (Codex, OpenCode).", "Nothing; it went to the CLI as typed. Type the command in full at the prompt for aggy to catch it from the keys."},
	{CodeCommandNoEnter, false, "A command from a CLI's prompt hook came with no Enter of a / command at the prompt just before it (a second one for the same Enter, one after a key or a turn started, one whose name or arguments the keys typed before that Enter do not start, or one sent by some other process in the CLI's tree), so aggy did not take it.", "Nothing; the prompt went to the CLI. Type at least / and the command's first letter before picking it from the CLI's menu, or type the command in full at the prompt for aggy to catch it from the keys."},
	{CodeCommandKeyboard, false, "A command that changes something (accept, check, reject, tool, permissions, trust) came from a prompt hook, which any process in the CLI's tree can send, so aggy did not take it.", "Type it in full at the prompt: from the keys aggy takes it, and shows what it does before it runs."},
	{CodeSwitchHeld, false, "The CLI said it waits for the user (a permission prompt or a notice), so the switch waits.", "Answer the CLI, and the switch goes on at that turn's end. If nothing is waiting on screen, press Enter on the empty prompt and pick the vendor again, which spends no turn."},
	{CodeNotSupervised, false, "aggy switch ran outside a session aggy supervises, or in one started with --direct.", "Press aggy's switch key in a session started with aggy run."},
	{CodeExitTimeout, false, "The CLI did not exit within the wait after aggy typed its exit command.", "The CLI's screen; cancel with the switch key, then Esc."},
	{CodeNoQuota, false, "After a limit, no vendor the picker offers has quota left.", "aggy usage; aggy log shows the limits."},
	{CodeLimitNoHold, false, "After a limit on a vendor with no hold signal (Codex), aggy only suggests the switch; the user picks it.", "Press the switch key, then Enter."},
	{CodeNearNoQuota, false, "The near-limit warning found no vendor with quota to hand off to.", "aggy usage."},
	{CodeToolsNoHold, false, "The tools changed on a vendor with no hold signal (Codex), so aggy only shows a note; the user starts the resume with the switch key.", "Press the switch key at the note, then confirm at the turn end."},
	{CodeSwitchBlocked, false, "The next launch would refuse, as a config or rules file has a mistake, so the CLI on screen was not told to exit.", "aggy doctor; fix the file, then pick again."},
	{CodeChildGone, false, "The session picked in the switch list was no longer recorded when the switch was confirmed, so the CLI on screen was not told to exit.", "aggy sessions; pick again."},
	{CodeLineage, true, "A message on the supervisor's socket came from a process outside the on-screen CLI's process tree, so it was not acted on.", "Which process of yours writes to ~/.aggy/run; a CLI's tool that outlived it is reparented and falls out."},
	{CodePeerUID, true, "A process of another user reached the supervisor's socket; it got no answer.", "The run directory's owner and mode (0700)."},
	{CodeBadRequest, true, "A message on the supervisor's socket was not one aggy sends (bad JSON, an unknown type or field, a bad name).", "Which process writes to ~/.aggy/run; aggy hook and aggy switch send only valid messages."},
	{CodeForeignSession, false, "A turn end from the CLI's tree named another session (a nested CLI), so it did not end the on-screen turn.", "Nothing, unless the on-screen CLI's own turn ends are ignored: then its session id is not the one aggy recorded."},
	{CodeGrokShutdown, false, "Grok fires a stop hook with reason shutdown at every exit (grok 1.0.70); it ends no turn, so it did not put the CLI at its prompt or start a switch. Only one naming the on-screen session counts; any other is a turn end of another session or of none. Recorded once a minute, and when the session ends (attrs.repeats: how many more came since the last record; attrs.tail: the record holds only those).", "Nothing; it is expected once when Grok exits."},
	{CodeNoSession, true, "A turn end from the CLI's tree named no session, which the vendor's own hook always does, so it looks forged and was not acted on. Recorded once a minute, and when the session ends (attrs.repeats: how many more came since the last record; attrs.tail: the record holds only those).", "Which process in the session sends hook events: aggy hook forwards what the vendor gives it."},
	{CodePeerUnknown, false, "aggy could not tell which user or process opened a connection to the supervisor's socket, so it was closed unanswered.", "Usually a process that connected and exited at once; nothing, unless hooks stop arriving."},
	{CodeWrongVendor, true, "A hook event from the CLI's tree came from another vendor's hook than the one on screen, so it did not gate a switch.", "A nested CLI of another vendor in the session; nothing else."},
	{CodeGuardRefused, false, "aggy's OpenCode guard refused a tool call. It reads the raw input, so it catches mistakes only.", "Run the command another way, or at the terminal yourself."},
	{CodeDecideNoMatch, false, "No safety policy rule matched the call, so the hook gave no opinion and the CLI's own permission flow decided.", "Nothing."},
	{CodeDecidePreApproved, false, "The call matched the safety policy and was exactly one you pre-approved with aggy permissions add allow, so it ran without asking.", "aggy permissions list shows the pre-approvals; aggy permissions remove allow takes one back."},
	{CodeDecideAsked, false, "The call matched the safety policy and nothing pre-approved it as a whole, so the CLI asked.", "Approve it at the prompt, or pre-approve the exact command with aggy permissions add allow."},
	{CodeDecideGroupDenied, false, "The call matched a safety policy group you set to deny, so it was refused.", "aggy permissions policy <group> ask sets the group back to asking."},
	{CodeDecideNoPrompt, false, "The call matched the safety policy in a session that cannot show a prompt (bypassPermissions or dontAsk), so it was refused instead of asked.", "Start the session in a mode that asks."},
	{CodeDecideUnreadable, false, "The hook could not read the tool call (no payload, too large, not JSON, or a tool it does not know), so it asked, or refused where no prompt can show.", "The vendor CLI's version; aggy doctor."},
	{CodeDecidePlan, false, "The hook could not read its launch's decision plan (missing, unreadable, or not valid), so it asked about the call, or refused it where no prompt can show, and never allowed it.", "Quit and start the session again; aggy doctor."},
	{CodeDecidePlanChanged, false, "The launch's decision plan no longer matched the SHA-256 the launch put in the hook's command: something changed it during the session, so the hook asked about the call (or refused it where no prompt can show) and never allowed it.", "Quit and start the session again; if it happens again, look for what in the session writes to aggy's run folder."},
	{CodeDecideProtected, false, "The call writes, or a shell command names, aggy's own files (its run folder, the decision plan, its permissions file) or a Claude settings file, so the hook asked about it, whatever else it matched.", "Approve it at the prompt if you meant it."},
	{CodeDecideOutside, false, "An OpenCode read named a file outside the launch's repo, so the hook asked about it (T6979); aggy's guard then lets that one read through only if the prompt was answered and the path is the file itself with no link on the way. Vendor credential files, aggy's own state, links, and .env are refused whatever is answered; a private key file asks each time.", "Approve it at the prompt if you meant it, or copy the file into the repo."},
	{CodeDecideUnjudged, false, "On Hermes, a tool that runs code (execute_code, browser_exec, typed stdin to a background process) matched no rule, or a tool aggy has not classified as read-only (an MCP tool, cron, delegation, skills, memory) was called; Hermes asks nothing before these, so aggy's hook asked (#134 review).", "Approve it at Hermes' prompt if you meant it, or do that step another way."},
	{CodeDecideOtherTool, false, "A tool other than the shell or Claude's own file tools (an MCP tool, say) carried text you pre-approved; a pre-approval is not for that tool, so the hook gave no opinion and the CLI's own permission flow decided.", "Nothing; approve it at the CLI's prompt if it asks."},
	{CodeDecideTooLong, false, "The call's command or path was longer than the hook scans (64 KiB), so it asked at once rather than judge it.", "Approve it at the prompt, or run it as a script file."},
	{CodeDecideFallback, false, "aggy's policy decision hook failed or was missing, so its wrapper asked about the call (or refused it where no prompt can show) without judging it.", "aggy doctor for failures by CLI version; check that aggy is still installed at the path the launch used."},
	{CodeHermesNoLaunch, true, "aggy's Hermes hook found no live aggy launch for the profile and session id its payload named (a session started outside aggy, one whose aggy has exited, or two aggy sessions in one repo with a session id neither recorded), so it refused the tool call.", "Quit Hermes and start it with aggy run hermes; in a repo with two aggy Hermes sessions, start one at a time."},
	{CodeHermesProfile, false, "aggy could not make its Hermes profile for this repo, write aggy's block into its config.yaml, or read the block back as written, so Hermes was not started: without it, aggy's approvals and hooks would not reach the session.", "The message; aggy doctor. A config.yaml that sets approvals, hooks, auth, or updates outside aggy's block is refused: move those lines into aggy's block or delete them."},
	{CodeHermesUnclaimed, false, "Another aggy Hermes session in this repo has not named its session id to aggy yet (no tool call or turn end), so a second one could take its place; this launch was refused.", "Type a first message in the other session, or quit it, then start this one."},
	{CodeHermesClaimLate, true, "Another aggy Hermes session in this repo was live, and a new Hermes session's tool call came more than 60 s after the last Enter typed in the aggy session that has none yet (or under aggy --direct, where aggy sees no keys), so aggy could not tell it from a session the other one started (/new, a nested hermes) and refused the call.", "If this is the session you just typed in, type the message again there; else quit Hermes and start aggy run hermes, or run one aggy Hermes session per repo at a time."},
	{CodeHermesDirectBusy, false, "aggy --direct was asked to start a new Hermes session while another aggy Hermes session in this repo was live; aggy sees no keys under --direct, so it could not tell this session's first tool call from one the other started, and refused the launch.", "Quit the other aggy Hermes session, or start this one without --direct."},
	{CodeHermesUntrusted, false, "Hermes is not ticked in aggy's trust list for this folder, so aggy did not start it.", "aggy trust <dir> and tick Hermes, after reading its warning."},
	{CodeGuardMissing, false, "aggy's OpenCode guard did not report from the launch's folder in time. OpenCode's rules from aggy's config still apply; the guard's own refusals may not.", "Quit and start again; aggy doctor for the CLI's version. A same-user process can overwrite or unload the plugin."},
	{CodeTrustBadDir, false, "aggy trust was given a path that is not a folder aggy can resolve.", "The path; aggy resolves it to its repo root."},
	{CodeNotAtKeyboard, false, "A change to permissions or settings came from somewhere that is not a person at a terminal: a borrow child, a session's own tool shell, or no terminal on stdin.", "Run it yourself at a terminal outside the session."},
	{CodePermBadRule, false, "A rule is not one aggy can use: its form is wrong, or the filter never passes it on.", "The rule; aggy permissions --help names the filter."},
	{CodePermBadFile, false, "aggy could not read or change ~/.aggy/permissions.json.", "aggy doctor, which reports a mistake by line."},
	{CodePermNotFound, false, "aggy's own file has no such rule at that scope, or the rule is in a CLI's own file, which aggy does not edit.", "aggy permissions list, which names each rule's source."},
	{CodePermScope, false, "A rule for this repo, or a suggestion for it, was asked for outside any git repo.", "Name --everywhere, or --all for suggestions."},
	{CodeConfigBadValue, false, "aggy config set was given a key or value it does not take.", "aggy config --help."},
	{CodeKeyUnsupported, false, "aggy key needs the macOS Keychain (/usr/bin/security) or the Linux secret service (/usr/bin/secret-tool), and this system has neither at that path. aggy never falls back to a file.", "On Linux, install libsecret's secret-tool (libsecret-tools) at /usr/bin; elsewhere, sign in inside the harness itself."},
	{CodeKeyBadName, false, "aggy key was given a key name or provider it does not take.", "aggy key --help."},
	{CodeKeyProviderTaken, false, "aggy key add refused: another stored key is already bound to that provider, and a provider takes one key.", "aggy key list, then aggy key remove <name> for the old one."},
	{CodeKeyHelperFailed, false, "The OS keychain tool (security or secret-tool) exited with an error or timed out, so nothing was stored or removed. aggy never shows the tool's own text.", "Run aggy key list; on macOS, check the login keychain is unlocked; on Linux, check the secret service is running."},
	{CodeKeyNotPassed, false, "A stored key was not handed to OpenCode: the keychain refused or timed out (on macOS, Deny in the dialog), the item is missing or not a printable key, the repo's allowed_vendors does not list its provider, or the launch was not started by a person at a terminal.", "The line aggy printed at launch; aggy key list."},
	{CodeConfigByHand, false, "aggy config set could not make the setting take the value asked, as the config file spells it another way (a dotted key, an inline table, or another case), so nothing was changed.", "Edit that line of ~/.aggy/config.toml by hand; aggy config list shows the value in force."},
	{CodeTrustBadFile, false, "aggy could not read or change its list of trusted repos (trust.json or opencode-trust.json in aggy's state folder).", "The file's contents, or aggy unwire."},
	{CodeBadPayload, false, "aggy hook could not read the vendor's hook payload, or it was not a JSON object.", "The vendor CLI's version; aggy doctor."},
	{CodeToolNameClash, false, "A tool that is on was not wired: the CLI already loads an MCP server of that name from the user's or the repo's config, and aggy never replaces or merges into one.", "Rename or remove that server in the CLI's config, or turn the tool off."},
	{CodeToolRepoPath, false, "A tool that is on was not wired: its arguments hold {repo}, and the session is not in a repo, or the repo's path holds a character other than letters, digits, and / . _ + @ -.", "Run the session in a repo whose path has only those characters."},
	{CodeToolNoFlag, false, "Tools that are on were not wired: the CLI's --help lacks the launch flag they need (Codex -c).", "Update the CLI; aggy setup shows its version."},
	{CodeToolWireFailed, false, "Tools that are on were not wired: the CLI's own MCP config could not be read to check names, or the session's tool file could not be written.", "The warning aggy printed; the record's class attr names the step. aggy doctor."},
	{CodeToolUnverified, false, "Tools were not turned on or wired for a vendor whose launch-time wiring is not verified yet (Grok: what --agent changes besides the MCP servers).", "Use the tool with Claude or Codex; docs/design.md, Launch wiring."},
	{CodeToolHookApproves, true, "A tool's hook was not wired: its definition, or a verified fact about it, says it approves actions the person would be asked about.", "Nothing; aggy never wires such a hook. docs/verification.md, Managed tools."},
	{CodeToolHookOutput, true, "aggy hook-tool passed nothing on: the tool's hook printed more than 64 KiB, not one JSON object, or an answer for another event.", "Run the tool's hook by hand with the same input; aggy tool status."},
	{CodeToolHookFailed, false, "aggy hook-tool passed nothing on: the tool's hook timed out, exited non-zero, could not start, or its definition changed or went since the session started.", "The tool's install (aggy tool status); a new session picks up a changed definition."},
	{CodeToolSkillMissing, false, "aggy tool on refused: the tool's skill folder is not installed under ~/.aggy/skills, or resolves outside it. aggy never fetches a skill.", "Copy the skill folder into ~/.aggy/skills/<name> as aggy tool on says, then turn the tool on again."},
	{CodeToolBorrowKind, false, "aggy borrow --tool was given for a kind other than edit. aggy cannot tell which tools read files or write, so a review stays read-only and research, news, image, and video children get no tool.", "Drop --tool, or borrow an edit."},
	{CodeToolSkillChanged, true, "A tool's skill folder changed since it was pinned (by aggy skill add, or its first link): aggy tool on refused to link it, or a launch wired none of that tool's MCP servers or hooks. A linked skill is a symlink and reaches every session of that CLI.", "Read the folder in ~/.aggy/skills/<name>. aggy tool off <name> everywhere, then aggy skill add <the folder you read> --replace stores and pins it (for Caveman too, and for a user tool with more than the skill, whose definition it keeps); no aggy tool off forgets the pin."},
	{CodeToolNetwork, true, "An edit borrow's tool reaches the network (network = true in its definition, or a user definition without network = false), and routing.edit_network_tools is off. Its server runs outside the CLI's sandbox, so a child steered by a prompt injection could read a .env and send it out.", "Drop the tool from routing.edit_tools, or set routing.edit_network_tools = true if you accept that risk."},
	{CodeToolUnlisted, true, "aggy borrow --tool named a tool that routing.edit_tools in your ~/.aggy/config.toml does not list. An agent runs aggy borrow, so only the tools you listed there reach an edit child.", "Add the tool to routing.edit_tools if an edit child should get it."},
	{CodeToolIndexFailed, false, "aggy tool on --repo refused a built-in with a per-repo index: its program is not installed; the index folder is a symlink; an agent with no terminal asked for the first index; or the index build (codegraph init) failed, was interrupted, or timed out. Nothing was recorded.", "The message says which: install the pinned version yourself (aggy never installs anything), or run aggy tool on at a terminal, then turn it on again."},
	{CodeToolIndexCopy, false, "A launch in one of aggy's edit worktrees did not copy the main checkout's index: the index folder or a file in it is a symlink or not a regular file, it is over the size cap, or a source changed while being copied. No database was left; the session started without it.", "The warning names the file; the tool can still build its own index there."},
	{CodeToolExec, true, "aggy tool exec refused to start a tool's MCP server: its folder is not aggy's private launch folder, the definition changed or is gone, the command is not the definition's, or a variable could not be passed.", "aggy tool status; if the definition changed, aggy tool on <name> again after reading it."},
	{CodeProxyVendor, false, "aggy tool on refused a tool with a local proxy without --vendor: aggy never turns a proxy on for a vendor the person did not name.", "aggy tool on <name> --vendor <vendor>."},
	{CodeProxyConfirm, true, "aggy tool on refused a tool's local proxy: there was no terminal, the typed confirmation did not match, or it ran inside an aggy session or a borrow child. A proxy sees every request, the login token included, so only the person at a terminal turns one on.", "Run aggy tool on <name> --vendor <vendor> yourself, in a terminal outside any aggy session, and type the confirmation it asks for."},
	{CodeProxyPortTaken, true, "A launch did not start a tool's proxy: something already listens on its loopback port, and aggy never points a CLI at a process it did not start. The CLI started without the proxy.", "Find what holds the port (lsof -i :<port>); stop it if it is a proxy you left running."},
	{CodeProxyUnhealthy, false, "A launch did not use a tool's proxy: it did not answer its health check in time, answered other than 2xx, or exited. aggy stopped it and started the CLI without it.", "Run the tool's proxy by hand to see why; aggy doctor checks its program."},
	{CodeProxyFailed, false, "A launch did not start a tool's proxy: its program is not on PATH, the launch folder could not be made, or it could not start. The CLI started without it.", "The warning aggy printed; aggy doctor."},
	{CodeProxyConflict, false, "A launch started no tool proxy: more than one tool with a proxy is on for the vendor, and a CLI has one base URL.", "aggy tool off one of them for that vendor."},
	{CodeProxyNotOwned, true, "A launch did not use a tool's proxy: the process listening on its loopback address is not in the proxy's process group (another process took the port while the proxy started), or this system cannot tell. aggy never points a CLI at a process it did not start.", "Find what holds the port (lsof -i :<port>) and stop it; on a system other than macOS and Linux, aggy runs no proxy."},
	{CodeProxyEnvProxy, false, "A launch did not use a tool's proxy: HTTP_PROXY or ALL_PROXY is set and NO_PROXY does not list the proxy's loopback address, so the CLI would send its requests to that HTTP proxy first.", "Add 127.0.0.1 and ::1 to NO_PROXY, or unset the HTTP proxy for aggy."},
	{CodeProxyHoldFailed, true, "A tool's proxy exited mid-session and another process took its loopback address before aggy could, so aggy killed the CLI at once; a request it sent in between could have reached that process.", "Find what holds the port (lsof -i :<port>); sign in again if you do not trust it; aggy resume resumes the session."},
	{CodeToolChanged, true, "aggy tool on or a --tool refused a user definition that is not the file aggy tool add showed, or (--tool) one it never recorded.", "aggy tool remove <name>, then aggy tool add the file again and read what it runs."},
	{CodeToolSkillExists, false, "aggy skill add refused: a skill folder, a built-in tool, or a user tool definition already has that name.", "Pick another --name, or pass --replace once the tool is off everywhere (aggy tool off <name>)."},
	{CodeToolSkillRefused, true, "aggy skill add refused the folder: it is or holds a symlink, something other than a regular file or folder, a file with another hard link, no SKILL.md, or more than the entry or size cap.", "Copy the skill into a plain folder of regular files and add that."},
	{CodeToolSkillSwapped, true, "aggy skill remove refused: the stored skill folder is no longer the one it looked up (something replaced it, or swapped in a symlink), so nothing was removed and anything moved aside was put back.", "Look at ~/.aggy/skills/<name>, then run aggy skill remove again."},
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
