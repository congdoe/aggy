# Logging and reporting

This is aggy's convention for diagnostic records, for what it prints when it refuses or fails, and for finding out what happened afterwards. It is the troubleshooting guide for people and for agents. The code follows it: the event and code registry below is generated from `internal/diag/codes.go`, and a test fails when they drift apart.

## In short

- Every refusal or error aggy prints ends with its reason code and run id: `aggy borrow: no vendor could start the research borrow: ... [borrow.no_vendor · run 01JBX4M6T0Q2W8Y5C3N7R9D1FA]`.
- `aggy diag show <id>` prints what happened, as one timeline, for that run id or any other id aggy shows: a trace, borrow, child, parent, or record id.
- `aggy diag codes <code>` says what the code means and what to check next.
- `aggy diag bundle > aggy-bundle.md` makes one scrubbed file to hand to an agent or attach to an issue.

## Where records go

Records go to `~/.aggy/diag.db` (`$AGGY_HOME/diag.db`), a SQLite database of its own, apart from the index (`aggy.db`), so that logging never waits on the index's writers and is pruned on its own. It is made like the index: mode 0600 in the 0700 state directory, opened through `internal/store` with the same `safefs` checks on the file and its `-wal` and `-shm`, with its own `user_version` migrations (`internal/store/diagmigrations`).

- **Retention.** `[log]` in the user config (`~/.aggy/config.toml`; a repo's config has no such table):

  ```toml
  [log]
  retention_days = 14   # 1 to 3650
  max_mb = 50           # 1 to 10240
  level = "info"        # the lowest level stored: error, warn, info, or debug
  ```

  `aggy gc` deletes records older than `retention_days` and compacts the file. When it is still over `max_mb`, gc deletes, in one pass, the oldest records that hold the excess (their share of the rows' size, the pages in use less the empty schema's) and compacts again; should the compacted file still be over, it repeats, at most three passes in all. `aggy gc --dry-run` shows the count and changes nothing. A diag.db gc cannot read is skipped with one warning naming it, and the rest of gc goes on.
- **Debug.** Debug records are stored only with `AGGY_DEBUG=1` or `level = "debug"`.
- **A failed write never fails the command.** aggy warns once on stderr (`[diag.write_failed · run …]`) and goes on.
- **No state dir, no files.** When the state directory does not exist, aggy writes no record and makes nothing: no directory, no diag.db, no index. A dry run (`aggy gc --dry-run`) writes no record at all, refusals included; that is decided once its flags are parsed.
- **Reading never writes.** Every `aggy diag` form opens diag.db and the index read-only and never creates, migrates, or writes either file.

## The record

Each record is one row of the `records` table. The record schema is versioned (`schema`, now 1); a change to a field's meaning is a new schema version and a diag.db migration.

| Field | Type | Notes |
| --- | --- | --- |
| `seq` | int | the order records were written: an `INTEGER PRIMARY KEY AUTOINCREMENT` (diag.db version 2), which `VACUUM` never renumbers and a delete never reuses; `aggy diag --follow` resumes after the last `seq` it printed |
| `id` | text | a ULID: 26 characters that sort by time; unique |
| `schema` | int | the record schema version |
| `ts` | int | Unix milliseconds, UTC |
| `level` | text | `error`, `warn`, `info`, or `debug`, from the event's registry entry |
| `component` | text | `borrow`, `accept`, `check`, `gc`, `setup`, `doctor`, `adapter.claude`, `adapter.codex`, `adapter.grok`, `supervisor`, `switch`, `hook`, `diag`; later `tool` |
| `event` | text | the stable dotted event name |
| `code` | text | the stable reason code; empty for a plain info event |
| `msg` | text | short and scrubbed: aggy's own words. A refusal's or error's record holds its code's registry text, not the message aggy printed, which can quote argv, a path, an environment value, or git's or a vendor CLI's own text |
| `attrs` | JSON | a small object of typed values: strings (scrubbed), bools, numbers |
| `duration_ms` | int | when the event has a duration |
| `exit_code` | int | a child's or check's exit status |
| `signal` | text | the signal that ended a process, when known |
| `aggy_version`, `os` | text | the aggy version and `GOOS/GOARCH` |
| `run_id` | text | the aggy process that wrote it |
| `trace_id` | text | the user-visible flow it belongs to |
| `cause_id` | text | the record that led to this one |
| `parent_id`, `child_id`, `borrow_id`, `worktree_id`, `vendor`, `vendor_session_id` | text | the entities it is about |

Indexes: `ts`, `trace_id`, `run_id`, `borrow_id`, `child_id`, `code`.

### Naming

- **Events** are `<component>.<thing>.<what happened>`, lower case with `_` inside a word: `borrow.child.exited`, `check.gate.refused`, `adapter.grok.lockdown_rejected`. A component with a dot (`adapter.grok`) is one component.
- **Codes** are `<area>.<reason>`: `borrow.one_hop`, `lockdown.rejected`, `accept.protected_path`. A code names why, not where: the same code may come from two events.
- Both are stable. Rename one only with a new name in the registry and the old one kept until no supported version writes it.
- Code names events and codes only through the variables in `internal/diag/codes.go`, never as strings. `diag.Event` and `diag.Code` are structs with an unexported field, so a string or a literal outside the package does not compile; `diag.EventNamed` and `diag.CodeNamed` look a name up in the registry (for `aggy diag codes`). `TestRegistry` fails on an entry not in the registry, on a registry entry no code uses (comments do not count), and on a literal built inside the package outside `codes.go`.
- A **security** code is a security-relevant refusal. Its record also goes to the index's `events` table as a `refusal` row, which `aggy log` shows with the run and the record id. The row is written only when the index already exists: the index is then opened without being made or migrated.

## Correlation ids

Every record carries them, so any problem can be followed end to end with one query.

- **`run_id`**: a ULID per aggy process. It is the id printed with every refusal and error.
- **`trace_id`**: one per user-visible flow, inherited by everything the flow starts.
  - A run started by a person begins a new trace: its trace id is its run id.
  - aggy passes the trace to the children it starts in `AGGY_TRACE`, and an aggy that starts with a valid `AGGY_TRACE` (a ULID) joins that trace. `AGGY_TRACE` is on `internal/execrun`'s allowlist for headless children (borrows), so the guard hook or a refused `aggy borrow` inside a child records under the borrow's flow, and on its list for on-screen CLIs (`PTYEnv`), so a session's hooks and the borrows run from it carry the trace of the aggy that launched it. It is an id aggy made; aggy's own `AGGY_TRACE` is never passed on as is, only the writer's validated trace.
  - A supervised session (`aggy`, `aggy run`, `new`, `resume`) is one trace from its first launch to its last: every vendor it switches to is launched in the same process, and each CLI gets the trace in `AGGY_TRACE`. So the CLI's `aggy hook` calls (the hook handler adopts the trace of the supervisor it reports to), an `aggy switch` the agent runs, and the borrows run from the session all carry it, and the supervisor's own records (switches, the picker, the socket) do too. An `aggy run` started inside such a session (from the CLI's shell) inherits its `AGGY_TRACE` and so joins the outer trace; its own run id tells its records apart.
  - The trace is also written on the index's `borrows` and `children` rows when they are made (`trace_id`, index migration 0015; NULL on rows from before it, and on rows made with no trace).
  - A record about a borrow (`borrow_id` set) takes its trace from the borrow's row, which `aggy accept`, `aggy check <id>`, and `aggy gc` read, so they join the flow that started it whenever they run; when the row has none (a borrow from before 0015), from that borrow's first record in diag.db.
- **`cause_id`**: the record that led to this one: a child's exit points at its start, a failure at the exit, the end at the failure.
- **Entity ids**: `parent_id`, `child_id`, `borrow_id`, `worktree_id` (an edit worktree's directory name, which is its borrow id), `vendor`, and the vendor session id where aggy already stores it.

## What aggy prints

Every refusal or error aggy prints on the instrumented paths ends with `[<code> · run <run id>]`:

```text
aggy borrow: aggy borrow does not run inside a borrow child (AGGY_DELEGATED is set): borrows are one hop [borrow.one_hop · run 01JBX4M6T0Q2W8Y5C3N7R9D1FA]
aggy accept: refusing: the diff touches .github/workflows/ci.yml, ... [accept.protected_path · run 01JBX4N2D6V8C0E3G5J7K9M1PQ]
borrow 4f1c2a9e0b7d3c55: research on grok (default model): lockdown in 3s, 0 tokens [lockdown.rejected · run 01JBX4P...]
```

The message is the one aggy printed before; the code and run id are added. Usage errors print the usage text after the tagged line. `aggy doctor`'s error findings end with `[doctor.error_finding · run …]`, and `aggy check`'s failed commands and its summary line with `[check.failed · run …]`. A dry run prints its refusals without the tag, since it stores no record.

### Exit codes

| Status | Meaning |
| --- | --- |
| 0 | Done. |
| 1 | An error or a refusal; a borrow that ran and failed; a check that failed or was not run (`check.failed`, `check.no_checks`, and every `aggy check <id>` refusal); an accept, gc, or setup that did not finish. |
| 2 | A usage error. `aggy check` (no id) also exits 2 when it is not in a git work tree (`check.not_git`), when the state dir cannot be found (`store.unavailable`), when run in an edit worktree (`check.edit_worktree`), and on a repo config error (`cli.config_invalid`). |
| other | `aggy`, `aggy run`, `new`, and `resume` exit with the vendor CLI's own status. |

## Privacy

The hard rules hold for every record, the export, and the bundle.

- **Never stored:** prompt or task text, file contents, diffs, model output, environment values, credentials, vendor transcripts, a check's output or arguments, a child's reply or error text.
- **Fine:** ids, codes, aggy-state paths, exit codes, durations, counts, vendor names, error classes, and aggy's own messages.
- Every free-text value (`msg` and each string attr) goes through `internal/redact` (secret-shaped lines are removed) and `internal/termsafe` (control and format characters are escaped), and is capped (300 characters for `msg`, 200 for an attr).
- `aggy borrow` marks its task and constraints as forbidden text: the writer replaces them, and every line of them of 8 or more characters, with `[task text]` wherever they appear. The record of a refusal holds its code's registry text, never the printed error, which can quote a path or constraint.
- `aggy diag bundle` is also scrubbed as `aggy doctor --report` is (home paths, user and host names, emails, token-shaped text), leaves out vendor session ids, and lists config keys, never values.
- A test runs a fake borrow, accept, and gc with a planted secret and a distinctive task, and checks every column of diag.db, the export, and the bundle for both (`TestDiagPrivacyCanary`).

## Querying

| Command | What it does |
| --- | --- |
| `aggy diag` | the latest records as a table; filters `--since`/`--until` (`1h`, `2d`, `today`, a date, RFC 3339), `--level` (that level and more severe), `--component`, `--event`, `--code` (globs: `borrow.*`), `--vendor`, `--run`, `--trace`, `--parent`, `--child`, `--borrow`, `-n N` (default 100; 0 for all), `--follow`; `--json` or `--jsonl` |
| `aggy diag show <id>` | one timeline for a run, trace, borrow, child, parent, or record id: diag records, the index's `events` rows, and the borrow rows, by time, indented by `cause_id`; it says which kind of id it was given; `--json` |
| `aggy diag codes [code]` | the code registry, or one code or event, with its meaning and what to check next |
| `aggy diag sql "<SELECT …>"` | one read-only statement, its rows printed as they are read; `--json`; `--timeout D` (default 30s) stops it; `--limit N` (default 10000; 0 for no cap) stops after N rows and says so on stderr |
| `aggy diag export --jsonl` | the records as JSON lines (every record unless `-n`), with the same filters |
| `aggy diag bundle [--trace ID] [--since WHEN]` | scrubbed Markdown: versions, the timeline (default: the last 24 hours, at most 500 records), the codes in it with their registry text, `aggy doctor --report`, and the config keys |

### Tables and columns, for agents

`aggy diag sql` runs on an empty in-memory database with diag.db attached as `diag` and the index as `idx`, each only when its file exists. Unqualified table names resolve across both. The connection is `query_only`, and the same select-only SQLite authorizer as the index's read-only open is installed after the two attaches, so a statement cannot write, `ATTACH`, set a PRAGMA (other than reading `user_version`), or `VACUUM`.

- `diag.records`: the columns in [The record](#the-record). `attrs` is JSON: `json_extract(attrs, '$.outcome')`.
- `idx.events`: `id`, `at` (Unix ms), `parent_id`, `child_id`, `type` (`screen`, `switch`, `borrow`, `files`, `session`, `limit`, `refusal`), `detail` (JSON; a refusal's has `code`, `event`, `run`, `record`).
- `idx.borrows`: `id`, `child_id`, `kind`, `vendor`, `model`, `status` (`running`, `done`, `failed`, `killed`), `outcome`, `tokens`, `started_at`, `ended_at`, `report_path`, `trace_id`.
- `idx.children`: `id`, `parent_id`, `vendor`, `vendor_session_id`, `kind`, `status`, `worktree`, `branch`, `worktree_state`, `worktree_check`, `last_active`, `trace_id`.
- `idx.parents`: `id`, `dir`, `last_vendor`, `created_at`, `last_active`.
- `diag.sqlite_master` and `idx.sqlite_master` list the rest.

### Question to query

- **What happened in this run?** `aggy diag show <run id>` (the id printed with the error).
- **Why was a borrow refused?** `aggy diag --event 'borrow.*' --since 1h`, then `aggy diag show <run id>`; for each vendor passed over, `aggy diag --run <run id> --event borrow.vendor.skipped` (the code says why: `vendor.cli_missing`, `sandbox.missing`, `lockdown.untested_version`, …).
- **Why did a borrow fail?** `aggy diag show <borrow id>`: the child's start, its exit code and duration, the failure's code, and the borrow row.
- **Why did accept fail?** `aggy diag show <borrow id>` shows the check gate and the accept refusal with its code (`accept.protected_path`, `accept.checkout_dirty`, `accept.worktree_changed`, …).
- **Why didn't the switch happen?** `aggy diag --component switch --since 1h`, or the whole session with `aggy diag show <trace id>` (the trace of a `supervisor.child.launched` record). A switch goes `switch.request.queued`, `switch.request.offered` (its `cause_id` is the counted turn end that let it go, a `hook.event.accepted` with `counted: true`, or, when the switch was picked or asked for after that turn end, the `switch.request.queued` record), `switch.request.confirmed`, `switch.request.completed`. Where it stops says why:
  - a `switch.request.refused` names the reason: `switch.unsent_text` (text at the prompt), `switch.held` (the CLI waits for the user), `switch.vendor_unavailable`, `switch.agent_no_hold` (Codex), `switch.agent_options`, `switch.already_waiting`;
  - no `switch.request.offered` and no refusal: no turn end was counted. Look at the `hook.event.*` records: `hook.event.ignored` with `hook.foreign_session` (a nested CLI's turn end; once a minute per session, `attrs.repeats` counting the rest), `hook.no_session` (a turn end naming no session), `counted: false` (the session's id not known yet), `holds: true` (a Notification or another event that holds the switch until the user types), `peer.lineage_failed` (a hook from outside the CLI's tree), or none at all (the CLI's hooks are not installed: `aggy hooks status`; or `hook.forward.failed` from `aggy hook` itself);
  - `switch.exit.timed_out`: the CLI did not exit after aggy typed its exit command.

  ```sh
  aggy diag sql "SELECT ts, event, code, msg FROM records WHERE trace_id = '<trace id>' AND component IN ('switch', 'hook') ORDER BY seq"
  ```
- **Why did the agent's `aggy switch` fail?** Its message ends with the code and its own run id: `aggy diag show <run id>`. A refusal of `aggy switch` itself (`switch.agent_options`, `cli.usage`, `switch.not_supervised`, `supervisor.unreachable`) is recorded only there; one the supervisor made is recorded by both, in the session's trace, with the supervisor's record the one in `aggy log`: `aggy diag --trace <trace id> --event switch.request.refused`.
- **Which runs hit a vendor limit today?** A borrow's failure (`borrow.run.failed`) and a limit the on-screen CLI reported (`switch.limit.received`, `attrs.unverified` when only a hook said so) both carry `vendor.rate_limit`:

  ```sh
  aggy diag sql "SELECT DISTINCT run_id, trace_id, vendor, event, borrow_id FROM records WHERE code = 'vendor.rate_limit' AND ts >= unixepoch('now', 'start of day') * 1000"
  ```
- **Did the near-limit warning show?** `aggy diag --event switch.near_limit.warned --since today` (attrs: `used_pct`, `warn_at_pct`, `to`; code `switch.near_limit_no_quota` when no vendor had quota).
- **Why did a launch fail?** `aggy diag --event supervisor.launch.refused --since 1h`; the code says which step (`supervisor.no_terminal`, `policy.unreachable`, `vendor.cli_missing`, `cli.config_invalid`, `supervisor.launch_failed`).
- **Every security refusal this week:** `aggy log` for the `refusal` rows, or `aggy diag sql "SELECT ts, code, run_id, trace_id, borrow_id FROM records WHERE code IN ('borrow.one_hop', 'lockdown.rejected', 'accept.protected_path', 'accept.worktree_changed', 'accept.state_temp_dir', 'check.gate_temp_dir', 'check.tree_changed', 'check.edit_worktree', 'peer.lineage_failed', 'peer.uid_mismatch', 'socket.bad_request', 'hook.no_session', 'hook.wrong_vendor') AND ts >= unixepoch('now', '-7 days') * 1000"` (the registry's Security column lists them).
- **Borrows that started and never finished (aggy was killed):**

  ```sh
  aggy diag sql "SELECT borrow_id, run_id, ts FROM records WHERE event = 'borrow.child.started' AND borrow_id NOT IN (SELECT borrow_id FROM records WHERE event = 'borrow.run.finished')"
  ```
- **A borrow's record joined with its index row:** `aggy diag sql "SELECT r.event, r.code, b.status, b.outcome FROM records r JOIN idx.borrows b ON b.id = r.borrow_id WHERE r.borrow_id = '<id>'"`

## What is instrumented

- borrow (start, the child's exit, the end, vendors passed over, refusals), the adapters' lockdown rejections and Grok's sandbox refusal, accept and reject, the check gate, `aggy check`, `aggy gc`, `aggy setup`, `aggy doctor`, and `aggy tool` (refusals, refused user definitions, and each on or off);
- the supervisor: a launch and its refusals, the CLI's exit (with the vendor launched next);
- switching: a switch queued (by the user, the agent, or a limit), offered, confirmed, refused (each reason), cancelled, completed, and an exit that did not come; the picker opened, a vendor chosen, the picker dismissed; a limit received, verified or not, and when aggy did not offer the switch itself; the near-limit warning;
- the supervisor's socket: a hook event accepted from the session, and every message rejected (outside the CLI's tree, another user, not a message aggy sends, a turn end of a foreign session, another vendor's hook), as security refusals; an agent's `aggy switch` refused, in both processes; `aggy hook` failing to reach the supervisor.

Not yet: `aggy hooks`.

### The supervisor never waits on the log

The supervisor's terminal loop and its socket write records through `diag.Queue`: `Log` only puts the record on a queue of 256 (it gets its id and time then, so a later record can name it as its cause) and one goroutine writes them, in order. Records with a security code have a queue of their own (64), written first, so a flood of plain records cannot crowd one out. The terminal loop never waits; the socket's goroutines, which are not the terminal loop, wait up to 100 ms for room (`LogWait`). When a queue is still full the record is dropped and counted, and when the session ends one `diag.queue.dropped` record (`diag.queue_full`, `attrs.dropped`, `attrs.dropped_security`) says how many. At the end aggy waits at most 2 seconds for the queue to be written. A slow disk can lose records; it never holds up a key or the CLI's output. `TestQueueSlowWriter` checks it with a writer that takes 50 ms a record.

While the CLI owns the terminal, the writer's one warning (`diag.write_failed`) is held and printed once the terminal is back, so it never lands on the CLI's screen, and a panic while writing is caught and reported as a failed write. `aggy hook` writes its record only in the time left of its 3-second deadline, as the vendor waits for the hook.

### Never stored, in a session

The values of `AGGY_SOCKET` and `AGGY_HOME`, which any process in a CLI's tree can set, are replaced with `[env value]` wherever they show in a record; the records of a failed launch, `aggy switch`, and `aggy hook` hold aggy's own words, never the error text they print. The supervisor's records hold no keys the user typed, no text the CLI printed, and nothing of a hook's payload: not even a session id from one, since any process in the CLI's tree can send a payload. A hook event's name is kept only when it is one aggy acts on (else `other`), a switch's model only as `model_set`, and the on-screen session's vendor id only on the launch record, where the index has it already. `TestDiagSessionCanary` (test/e2e: the real binary on a pseudo-terminal, against the fake CLI) plants canaries and a secret in the composer, the hook payloads, and the CLI's stderr, and checks diag.db, the export, and the bundle, and that the records of `aggy hook` and an agent's `aggy switch` share the session's trace; `TestDiagSwitchRecords` and `TestDiagHookTrace` do the same in-process.

## Registry

Generated from `internal/diag/codes.go`; `AGGY_UPDATE_DOCS=1 go test ./internal/diag -run TestRegistryDoc` rewrites it.

<!-- registry:begin -->
| Event | Level | Component | Meaning | What to check next |
| --- | --- | --- | --- | --- |
| `borrow.request.refused` | error | borrow | aggy borrow refused or failed outside the child's own run: before a child started, or while recording one. | The code's entry; aggy diag show &lt;run id&gt; for what led to it. |
| `borrow.vendor.skipped` | info | borrow | A candidate vendor could not start the borrow, and the next one was tried. | The code says why; aggy setup and aggy doctor for that vendor. |
| `borrow.child.started` | info | borrow | The borrow was recorded and its child CLI started. | A started borrow with no borrow.run.finished was cut short: aggy diag show &lt;borrow id&gt;. |
| `borrow.child.exited` | info | borrow | The child CLI exited; exit_code and duration_ms say how. | A non-zero exit is classed by the next record (borrow.run.failed). |
| `borrow.run.failed` | warn | borrow | The borrow ran and failed; the code is the vendor's error class. | The code's entry; aggy inbox &lt;borrow id&gt; for the report. |
| `borrow.run.finished` | info | borrow | The borrow ended and was recorded; attrs hold the outcome and tokens. | aggy log and aggy stats show it too. |
| `adapter.claude.lockdown_rejected` | error | adapter.claude | Claude Code rejected a lockdown flag; the run was discarded. | aggy doctor for the version; attach aggy diag bundle to an issue. |
| `adapter.codex.lockdown_rejected` | error | adapter.codex | Codex rejected a lockdown flag; the run was discarded. | aggy doctor for the version; attach aggy diag bundle to an issue. |
| `adapter.grok.lockdown_rejected` | error | adapter.grok | Grok rejected a lockdown flag, reported an unknown tool, or ran without its sandbox; the run was discarded. | aggy doctor for the version; attach aggy diag bundle to an issue. |
| `adapter.grok.sandbox_refused` | warn | adapter.grok | Grok was passed over: its sandbox is not available for this kind on this CLI or host. | sandbox.missing. |
| `accept.request.refused` | error | accept | aggy accept refused or failed; nothing was applied. | The code's entry; aggy diag show &lt;borrow id&gt;. |
| `accept.diff.applied` | info | accept | An edit's diff was applied to the main checkout. | git status in the main checkout. |
| `accept.diff.declined` | info | accept | The diff was not applied: no --yes without a terminal, or the person said no. | Run aggy accept &lt;borrow id&gt; --yes. |
| `check.gate.refused` | error | check | The check gate on an edit's diff refused or failed. | The code's entry. |
| `check.gate.decided` | info | check | The check gate recorded a result (attrs.result: pass, fail, or skipped). | aggy accept shows it. |
| `check.run.refused` | error | check | aggy check refused to run the repo's checks. | The code's entry. |
| `check.command.failed` | warn | check | One of the repo's checks failed (attrs.index is its place in the list). | Run aggy check again to see its output tail; nothing of it is stored. |
| `check.run.finished` | info | check | The repo's checks ran; attrs hold how many passed and failed. | check.command.failed records for the failures. |
| `gc.run.refused` | error | gc | aggy gc refused or could not plan; nothing was removed. | The code's entry. |
| `gc.item.removed` | info | gc | aggy gc removed one item (attrs.kind: worktree, child, inbox, research, or diag). | Nothing; it is the record of the removal. |
| `gc.item.failed` | warn | gc | aggy gc could not remove one item; it stays for the next run. | The record's msg; aggy gc --dry-run. |
| `setup.vendor.checked` | info | setup | aggy setup checked and recorded one vendor CLI (attrs: connected, version). | aggy doctor for more. |
| `setup.run.failed` | error | setup | aggy setup failed. | The code's entry. |
| `doctor.finding.error` | error | doctor | aggy doctor reported an error-level finding (attrs.finding is its doctor code). | aggy doctor shows the finding in full. |
| `doctor.run.refused` | error | doctor | aggy doctor did not run. | The code's entry. |
| `tool.request.refused` | error | tool | aggy tool refused or failed; no definition or switch changed. | The code's entry. |
| `tool.def.refused` | warn | tool | A user tool definition was left out: invalid, untrusted, or colliding (attrs: file, its base name in ~/.aggy/tools, and key; never its argv). | aggy tool list names the file and the key; fix or remove it. |
| `tool.switch.changed` | info | tool | aggy tool on or off recorded a switch (attrs: tool, vendor, repo, on). | aggy tool status shows what is on where. |
| `supervisor.launch.refused` | error | supervisor | aggy run, new, or resume did not put the CLI on screen, or the supervisor failed while it ran. | The code's entry; aggy diag show &lt;run id&gt; for what led to it. |
| `supervisor.child.launched` | info | supervisor | A CLI went on screen (attrs: new_child, brief, model_set, direct). Its trace is the one its hooks, switches, and borrows carry. | aggy diag show &lt;trace id&gt; for the whole session. |
| `supervisor.child.exited` | info | supervisor | The on-screen CLI exited; exit_code and duration_ms say how, attrs.switch_to the vendor launched next, if any. | A switch_to with no supervisor.child.launched after it: the next launch's supervisor.launch.refused. |
| `switch.request.queued` | info | switch | A switch waits for the turn end (attrs.origin: user, agent, or limit; brief, new, model_set). | No switch.request.offered after it: the session never reached a quiet turn end; look for hook.event.accepted (Stop) and switch.request.refused. |
| `switch.request.offered` | info | switch | The turn ended and aggy asked the user to confirm the switch. | switch.request.confirmed or switch.request.cancelled follows. |
| `switch.request.confirmed` | info | switch | The user pressed the switch key at the confirmation; aggy typed the CLI's exit command. | switch.request.completed, or switch.exit.timed_out if the CLI did not exit. |
| `switch.request.refused` | warn | switch | A switch was refused, or held back until the user acts; the code says why. | The code's entry; aggy diag show &lt;run id&gt; for what led to it. |
| `switch.request.cancelled` | info | switch | The user cancelled a waiting switch (attrs.how: esc_confirm or esc_list). | Nothing, unless the user did not cancel it. |
| `switch.request.completed` | info | switch | The CLI exited after aggy typed its exit command; the next vendor is launched. | The next supervisor.child.launched in the same trace. |
| `switch.exit.timed_out` | warn | switch | The CLI had not exited a while after aggy typed its exit command; the switch still stands. | The CLI's screen: a turn or a menu may have taken the exit command. |
| `switch.picker.opened` | info | switch | The switch picker opened on the vendor list (attrs.preselect: a vendor suggested after a limit or a near-limit warning). | switch.picker.chosen or switch.picker.dismissed follows. |
| `switch.picker.chosen` | info | switch | A vendor was picked in the list (attrs: brief, new, model_set, again). | switch.request.queued, or switch.request.refused with the reason. |
| `switch.picker.dismissed` | info | switch | The picker was closed with Esc; a waiting switch, if any, was cancelled. | Nothing. |
| `switch.limit.received` | warn | switch | The on-screen vendor reported its plan limit (attrs.unverified: only a hook said so, which any process in the CLI's tree can send). | aggy log shows the limit; switch.request.queued or switch.limit.not_offered follows. |
| `switch.limit.not_offered` | info | switch | After a limit, aggy did not queue a switch itself; the code says why. | The code's entry. |
| `switch.near_limit.warned` | info | switch | The near-limit warning was shown at a turn end (attrs: used_pct, warn_at_pct, window, to). | With switch.near_limit_no_quota no vendor had quota; aggy usage. |
| `hook.event.accepted` | info | hook | The supervisor took a hook event of the vendor on screen from the CLI's process tree (attrs.event: Stop, StopFailure, StopCancelled, SessionStart, Notification, or other). A turn end has attrs.counted: true when it named the on-screen session and so may let a switch go on, false while that session's id is not known yet; any other event holds a switch (attrs.holds). | A counted turn end that did not move a switch on: the switch.request.refused records near it. |
| `hook.event.rejected` | warn | hook | The supervisor did not act on a message on its socket; the code says why. | The code's entry; aggy log shows the security ones as refusals. |
| `hook.event.ignored` | info | hook | A turn end from the CLI's tree named another session, such as a nested claude -p, so it did not end the on-screen turn. Recorded once a minute per session (attrs.repeats: how many more came since the last record). | Nothing, unless every turn end of the on-screen CLI is ignored: then its session id is not the one aggy recorded. |
| `hook.forward.failed` | warn | hook | aggy hook could not hand a vendor hook event to the supervisor; the CLI went on undisturbed. | The code's entry; aggy diag --trace &lt;trace id&gt; for the session. |
| `diag.queue.dropped` | warn | diag | The supervisor's diagnostic queue was full, so records were dropped (attrs.dropped is how many). | The state dir's disk: a slow or full disk makes the log fall behind. |
| `tool.wire.refused` | warn | tool | A launch did not wire a tool that is on (attrs: tool, or none for every tool of the vendor); the session started without it. | The code's entry; aggy tool status. |
| `hook.tool.filtered` | info | hook | aggy hook-tool dropped part of a tool hook's answer: an allow, or keys outside its allowlist (attrs: tool, dropped, the key names only). | Nothing; a tool's hook may never approve an action. |
| `hook.tool.failed` | warn | hook | aggy hook-tool passed nothing on for a tool's hook; the CLI went on as without the hook (attrs.tool). | The code's entry. |
| `tool.wire.done` | info | tool | A launch wired the tools that are on into the CLI it started (attrs: tools, the names wired). | aggy tool status shows why each is on. |
| `tool.exec.refused` | error | tool | aggy tool exec, which a vendor CLI runs to start a tool's MCP server, refused to start it (attrs: tool, check: the step that failed). The CLI shows the server as failed. | The code's entry; aggy tool status. |

| Code | Security | Meaning | What to check next |
| --- | --- | --- | --- |
| `cli.usage` |  | Bad arguments or flags. | Run the command with --help. |
| `cli.config_invalid` |  | A config file could not be read or is invalid. | The message names the file and key; fix it. |
| `store.unavailable` |  | The index (~/.aggy/aggy.db) could not be opened, read, or written. | aggy doctor; the state dir's permissions and free space. |
| `borrow.one_hop` | yes | aggy borrow or aggy gc ran inside a borrow child (AGGY_DELEGATED is set): borrows are one hop. | Run it from your own shell. A child that tries this was steered to; read its task. |
| `borrow.invalid_request` |  | The borrow request failed validation: an empty or too long task, a bad path or constraint, or a flag the kind does not take. | aggy borrow --help. |
| `borrow.no_vendor` |  | No candidate vendor could start the borrow. | aggy diag --run &lt;run id&gt; --event borrow.vendor.skipped lists why each was passed over; aggy setup. |
| `borrow.start_failed` |  | The borrow failed after a vendor was chosen, while making its dirs, worktree, or records (the concurrency caps included). | aggy diag show &lt;run id&gt; for what led to it; aggy doctor. |
| `vendor.cli_missing` |  | The vendor's CLI is not on PATH, or its probe failed. | aggy setup. |
| `adapter.unsupported` |  | The vendor's adapter does not run this kind or request on this CLI or in this repo. | The README's vendor support table; another --vendor. |
| `adapter.no_capability` |  | The CLI's --help lacks a flag the lockdown needs. | Update the CLI; aggy setup shows its version. |
| `adapter.file_tools_off` |  | The request needs Grok's media file tools, which are off. | [media] grok_file_tools in ~/.aggy/config.toml, after reading what it uploads. |
| `sandbox.missing` |  | The vendor's sandbox is not available for this kind on this CLI or host. | Grok needs --sandbox strict in its --help and a kernel sandbox (macOS, or Linux with Landlock). |
| `lockdown.untested_version` |  | The CLI's version is outside the versions the borrow lockdown was verified on. | aggy doctor; --allow-untested runs it anyway. |
| `lockdown.rejected` | yes | The CLI rejected a lockdown flag, reported a tool it does not know, or ran without its sandbox, so aggy discarded the run. | aggy doctor for the version; attach aggy diag bundle to an issue. |
| `vendor.rate_limit` |  | The vendor reported a plan or rate limit. | aggy log shows the limit; routing skips the vendor until it lifts. |
| `vendor.auth` |  | The vendor CLI is not signed in. | The vendor's own login; then aggy setup. |
| `vendor.billing` |  | The vendor reported a billing problem. | The vendor's account page. |
| `vendor.server` |  | The vendor reported an outage or overload. | Try again later, or another --vendor. |
| `vendor.invalid_request` |  | The vendor refused the request as invalid. | aggy inbox &lt;borrow id&gt; for the report; aggy doctor for the version. |
| `borrow.max_turns` |  | The child used its turn budget. | [budgets] in ~/.aggy/config.toml, or a smaller task. |
| `borrow.timeout` |  | The child ran past its time budget and was killed. | [budgets] in ~/.aggy/config.toml, or a smaller task. |
| `borrow.interrupted` |  | aggy was interrupted (Ctrl-C, SIGTERM, SIGHUP) and killed the child. | Nothing, unless you did not interrupt it. |
| `borrow.child_failed` |  | The child failed for another reason: unparsed output, an unknown error class, or a runner error. | aggy inbox &lt;borrow id&gt; for the report. |
| `accept.target_invalid` |  | No such edit borrow, or it is running, already accepted or rejected, or its worktree was removed. | aggy inbox; aggy diag show &lt;borrow id&gt;. |
| `accept.protected_path` | yes | The diff touches .git, a symlink or submodule, the hooks path, or a file that holds hooks, CI, config, or instructions. | Review the worktree by hand; never accept such a diff blind. |
| `accept.worktree_changed` | yes | The worktree changed after the borrow ended (a process the child left behind?). | Review the worktree by hand. |
| `accept.checkout_dirty` |  | The main checkout has changes in the paths the diff touches. | Commit or stash them, then accept again. |
| `accept.failed` |  | Another accept error: git failed, or the diff no longer applies. | The record's msg; git status. |
| `accept.state_temp_dir` | yes | aggy's state dir, where accept writes the patch, is under a temp dir an edit child's sandbox may write. | Set AGGY_HOME to a dir outside temp dirs. |
| `accept.unsupported_os` |  | aggy accept is not supported on this OS yet. | Review the worktree and apply its diff by hand. |
| `check.gate_temp_dir` | yes | The main checkout is under a temp dir an edit child's sandbox may write, so its check list may be the child's. | Run edits from a repo outside temp dirs. |
| `check.tree_changed` | yes | The checks changed files in the worktree, so it no longer matches the diff the child left. | Review the worktree by hand. |
| `check.gate_failed` |  | Another check gate error. | The record's msg. |
| `check.failed` |  | One or more of the repo's checks failed. | aggy check prints each failure's output tail. |
| `check.not_git` |  | aggy check ran outside a git work tree. | cd into the repo. |
| `check.edit_worktree` | yes | aggy check ran by hand inside an edit worktree, whose config the child wrote. | aggy check &lt;borrow id&gt; from the main checkout. |
| `check.no_checks` |  | The repo lists no checks. | Add check = [[...]] to &lt;repo&gt;/.aggy/config.toml. |
| `gc.failed` |  | aggy gc could not plan. | The record's msg; aggy doctor. |
| `gc.remove_failed` |  | aggy gc could not remove one item. | The record's msg; aggy gc --dry-run. |
| `doctor.error_finding` |  | aggy doctor found an error-level finding. | aggy doctor. |
| `diag.write_failed` |  | aggy could not write its diagnostic log; the command went on without it. | The state dir's permissions and free space; aggy gc prunes the log. |
| `tool.unknown` |  | No built-in or loaded user tool has that name. | aggy tool list; a refused user file is named there. |
| `tool.not_applicable` |  | The tool has nothing for that vendor, or is a per-repo tool turned on without --repo. | aggy tool status &lt;name&gt;; aggy tool on &lt;name&gt; --repo . |
| `tool.repo_off` |  | The repo's .aggy/config.toml lists the tool in tools_off; a repo config always wins, and can only turn tools off. | Edit that repo's tools_off if it is your repo. |
| `tool.delegated` | yes | aggy tool add, remove, on, or off ran inside a borrow child (AGGY_DELEGATED is set). | Run it from your own shell. A child that tries this was steered to; read its task. |
| `tool.def_invalid` |  | A tool definition failed validation; the message names the file and the key. | Fix the key; docs/design.md section 9 has the rules. |
| `tool.def_untrusted` | yes | A user tool definition, its tools folder, or the state dir is a symlink, not the user's own, or writable by group or others. | chmod go-w and check the owner of ~/.aggy, ~/.aggy/tools, and the file. |
| `tool.still_on` |  | aggy tool remove refused: the tool is still on somewhere. | aggy tool off &lt;name&gt;, then remove it. |
| `tool.failed` |  | Another aggy tool error: a file or the index could not be read or written. | The record's msg; aggy doctor. |
| `diag.queue_full` |  | The diagnostic log fell behind the supervisor and dropped records rather than hold up the terminal. | The state dir's disk; aggy gc prunes the log. |
| `supervisor.no_terminal` |  | aggy run, new, or resume needs a terminal on stdin. | Run it in a terminal, not a pipe or a script. |
| `supervisor.launch_failed` |  | Another launch error: the parent or child could not be planned or recorded, the CLI could not start, or the supervisor failed. | The record's msg; aggy doctor. |
| `supervisor.unreachable` |  | The supervisor's socket did not answer: the session has ended, or the socket path is not aggy's. | Is aggy still running the session? AGGY_SOCKET must be in ~/.aggy/run. |
| `policy.unreachable` |  | The safety policy cannot be applied to the vendor's CLI, so aggy did not start it. | aggy doctor; --no-policy or [policy] required = false start it anyway. |
| `switch.unsent_text` |  | Text the user typed and did not send is at the CLI's prompt: the exit command would send it, so the switch waits. | Send or clear the text, then press Enter on the empty prompt. |
| `switch.agent_no_hold` |  | The agent asked for a switch away from a vendor with no hold signal (Codex), which only the user may start. | Press aggy's switch key. |
| `switch.agent_options` |  | The agent asked for a new session or a model, which are the user's choice in the picker. | Press aggy's switch key and choose them there. |
| `switch.vendor_unavailable` |  | The vendor cannot be switched to now: on screen already, not offered, not allowed by the repo, not on PATH, or the safety policy cannot reach it. | The record's msg; aggy setup; the repo's allowed_vendors. |
| `switch.already_waiting` |  | Another switch is already waiting. | Wait for it, or cancel it in the picker. |
| `switch.held` |  | The CLI said it waits for the user (a permission prompt or a notice), so the switch waits. | Answer the CLI, or send a message; the switch goes on at the next turn end. |
| `switch.not_supervised` |  | aggy switch ran outside a session aggy supervises, or in one started with --direct. | Press aggy's switch key in a session started with aggy run. |
| `switch.exit_timeout` |  | The CLI did not exit within the wait after aggy typed its exit command. | The CLI's screen; cancel with the switch key, then Esc. |
| `switch.no_quota` |  | After a limit, no vendor the picker offers has quota left. | aggy usage; aggy log shows the limits. |
| `switch.limit_no_hold` |  | After a limit on a vendor with no hold signal (Codex), aggy only suggests the switch; the user picks it. | Press the switch key, then Enter. |
| `switch.near_limit_no_quota` |  | The near-limit warning found no vendor with quota to hand off to. | aggy usage. |
| `peer.lineage_failed` | yes | A message on the supervisor's socket came from a process outside the on-screen CLI's process tree, so it was not acted on. | Which process of yours writes to ~/.aggy/run; a CLI's tool that outlived it is reparented and falls out. |
| `peer.uid_mismatch` | yes | A process of another user reached the supervisor's socket; it got no answer. | The run directory's owner and mode (0700). |
| `socket.bad_request` | yes | A message on the supervisor's socket was not one aggy sends (bad JSON, an unknown type or field, a bad name). | Which process writes to ~/.aggy/run; aggy hook and aggy switch send only valid messages. |
| `hook.foreign_session` |  | A turn end from the CLI's tree named another session (a nested CLI), so it did not end the on-screen turn. | Nothing, unless the on-screen CLI's own turn ends are ignored: then its session id is not the one aggy recorded. |
| `hook.no_session` | yes | A turn end from the CLI's tree named no session, which the vendor's own hook always does, so it looks forged and was not acted on. Recorded once a minute (attrs.repeats: how many more came since the last record). | Which process in the session sends hook events: aggy hook forwards what the vendor gives it. |
| `peer.cred_failed` |  | aggy could not tell which user or process opened a connection to the supervisor's socket, so it was closed unanswered. | Usually a process that connected and exited at once; nothing, unless hooks stop arriving. |
| `hook.wrong_vendor` | yes | A hook event from the CLI's tree came from another vendor's hook than the one on screen, so it did not gate a switch. | A nested CLI of another vendor in the session; nothing else. |
| `hook.bad_payload` |  | aggy hook could not read the vendor's hook payload, or it was not a JSON object. | The vendor CLI's version; aggy doctor. |
| `tool.name_clash` |  | A tool that is on was not wired: the CLI already loads an MCP server of that name from the user's or the repo's config, and aggy never replaces or merges into one. | Rename or remove that server in the CLI's config, or turn the tool off. |
| `tool.repo_path` |  | A tool that is on was not wired: its arguments hold {repo}, and the session is not in a repo, or the repo's path holds a character other than letters, digits, and / . _ + @ -. | Run the session in a repo whose path has only those characters. |
| `tool.no_launch_flag` |  | Tools that are on were not wired: the CLI's --help lacks the launch flag they need (Codex -c). | Update the CLI; aggy setup shows its version. |
| `tool.wire_failed` |  | Tools that are on were not wired: the CLI's own MCP config could not be read to check names, or the session's tool file could not be written. | The warning aggy printed; the record's class attr names the step. aggy doctor. |
| `tool.vendor_unverified` |  | Tools were not turned on or wired for a vendor whose launch-time wiring is not verified yet (Grok: what --agent changes besides the MCP servers). | Use the tool with Claude or Codex; docs/design.md, Launch wiring. |
| `tool.hook_approves` | yes | A tool's hook was not wired: its definition, or a verified fact about it, says it approves actions the person would be asked about. | Nothing; aggy never wires such a hook. docs/verification.md, Managed tools. |
| `tool.hook_output` | yes | aggy hook-tool passed nothing on: the tool's hook printed more than 64 KiB, not one JSON object, or an answer for another event. | Run the tool's hook by hand with the same input; aggy tool status. |
| `tool.hook_failed` |  | aggy hook-tool passed nothing on: the tool's hook timed out, exited non-zero, could not start, or its definition changed or went since the session started. | The tool's install (aggy tool status); a new session picks up a changed definition. |
| `tool.skill_missing` |  | aggy tool on refused: the tool's skill folder is not installed under ~/.aggy/skills, or resolves outside it. aggy never fetches a skill. | Copy the skill folder into ~/.aggy/skills/&lt;name&gt; as aggy tool on says, then turn the tool on again. |
| `tool.exec_refused` | yes | aggy tool exec refused to start a tool's MCP server: its folder is not aggy's private launch folder, the definition changed or is gone, the command is not the definition's, or a variable could not be passed. | aggy tool status; if the definition changed, aggy tool on &lt;name&gt; again after reading it. |
| `tool.changed_since_add` | yes | aggy tool on refused: the user definition is not the file aggy tool add showed. | aggy tool remove &lt;name&gt;, then aggy tool add the file again and read what it runs. |
<!-- registry:end -->
