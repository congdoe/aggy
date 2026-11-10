// aggy's OpenCode plugin: the guard and the reporter, written fresh for each
// launch (docs/design.md, section 15). It catches mistakes only: it sees the
// raw tool input, and a same-user process can overwrite or unload it.
//
// It calls aggy as a process and opens no network connection: aggy hook is
// the only thing that reaches the supervisor's socket.
import { execFile } from "node:child_process"

const HOOK_MS = 3000
const GUARD_MS = 8000
const DECIDE_MS = 5000
const MAX_REASON = 600

// Only these reach the guard: the fields it reads, not a write's content.
const guardFields = (input) => {
  const i = input && typeof input === "object" ? input : {}
  const text = (v) => (typeof v === "string" ? v : undefined)
  return { path: text(i.path), command: text(i.command), workdir: text(i.workdir), patchText: text(i.patchText), pattern: text(i.pattern) }
}

// How each event is told to aggy (schema/src/session-event.ts,
// schema/src/permission.ts, schema/src/form.ts at v2.2.5).
const report = {
  "session.execution.succeeded": (d) => ["Stop", { sessionID: d.sessionID }],
  "session.execution.failed": (d) => ["StopFailure", { sessionID: d.sessionID, error: d.error && d.error.type }],
  "session.execution.interrupted": (d) => ["StopCancelled", { sessionID: d.sessionID, reason: d.reason }],
  "permission.asked": (d) => ["PermissionAsked", { sessionID: d.sessionID, requestID: d.id }],
  "permission.replied": (d) => ["PermissionReplied", { sessionID: d.sessionID, requestID: d.requestID }],
  "form.created": (d) => ["QuestionAsked", { sessionID: d.form && d.form.sessionID, requestID: d.form && d.form.id }],
  "form.replied": (d) => ["QuestionReplied", { sessionID: d.sessionID, requestID: d.id }],
  "form.cancelled": (d) => ["QuestionReplied", { sessionID: d.sessionID, requestID: d.id }],
}

const run = (aggy, args, payload, ms) =>
  new Promise((resolve) => {
    const child = execFile(aggy, args, { timeout: ms, killSignal: "SIGKILL" }, (error, stdout, stderr) =>
      resolve({ failed: Boolean(error), stdout: String(stdout || ""), stderr: String(stderr || "") }),
    )
    child.on("error", () => resolve({ failed: true, stdout: "", stderr: "" }))
    child.stdin.on("error", () => {})
    child.stdin.end(JSON.stringify(payload))
  })

export default {
  id: "aggy-guard",
  setup: async (ctx) => {
    const { aggy, root } = ctx.options
    const auto = ctx.options.auto === "1"
    // Always allow saves a rule aggy did not render, so the guard must stop
    // going by its list. aggy keeps this per repo too (aggy hook
    // opencode-always), as the rule outlives this plugin and this session.
    let always = false
    const directory = ctx.location.directory

    // Every call is checked. A check that crashes, hangs, or is missing
    // refuses: a throw here fails that tool call alone.
    await ctx.tool.hook("execute.before", async (e) => {
      const r = await run(aggy, ["hook", "opencode-guard"], { tool: e.tool, directory, root, auto, always, ...guardFields(e.input) }, GUARD_MS)
      if (r.failed) {
        const why = r.stderr.trim().slice(0, MAX_REASON)
        throw new Error(why || "aggy's guard did not answer, so this call was refused")
      }
    })

    // The safety policy's decision (T6976): OpenCode's rules decide first,
    // and this only moves an ask to allow for a command the person
    // pre-approved exactly, and an allow (an Always allow OpenCode saved) back
    // to ask for any other policy match. A configured deny never reaches it
    // (core/src/permission.ts at dev d4aeee5, read 2026-11-10; the hook
    // turning an allow back to ask held on 2.2.5, docs/verification.md).
    // Should aggy fail or not answer, or this throw, the effect stays as the
    // rules gave it. Unverified on 2.2.5: that setting e.effect is how the
    // hook answers, and what resources holds for a compound command.
    const decide = ctx.options.decide
    if (decide && ctx.permission && typeof ctx.permission.hook === "function") {
      await ctx.permission.hook("evaluate", async (e) => {
        try {
          if (!e || e.action !== "shell" || (e.effect !== "ask" && e.effect !== "allow")) return
          const r = await run(aggy, ["hook", "decide", "--vendor", "opencode", "--plan", decide], { action: e.action, resources: e.resources, effect: e.effect }, DECIDE_MS)
          if (r.failed) return
          const want = r.stdout.trim()
          if (want === "allow" && e.effect === "ask") e.effect = "allow"
          else if (want === "ask" && e.effect === "allow") e.effect = "ask"
          else if (want === "deny") e.effect = "deny"
        } catch {}
      })
    }

    // Sent one at a time, so the supervisor sees them in order.
    let sending = Promise.resolve()
    const send = (event, payload) => {
      sending = sending.then(() => run(aggy, ["hook", "--vendor", "opencode", event], payload, HOOK_MS))
    }

    // Reports per folder: a plugin that fails to load gives no error, so a
    // missing report is how aggy finds out.
    send("GuardLoaded", { directory })
    ;(async () => {
      for await (const event of ctx.event.subscribe({ types: Object.keys(report) })) {
        if (event.type === "permission.replied" && event.data.reply === "always") {
          always = true
          run(aggy, ["hook", "opencode-always"], { root }, HOOK_MS)
        }
        const make = report[event.type]
        if (make) send(...make(event.data))
      }
    })().catch(() => {})
  },
}
