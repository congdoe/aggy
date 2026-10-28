# aggy skill

`SKILL.md` is the skill aggy links into each vendor CLI. It tells the
on-screen agent which kinds of work to borrow, when, and how
(`aggy borrow`, `aggy inbox`). One file serves all three CLIs: each
reads a `SKILL.md` with `name` and `description` frontmatter (Codex
requires both). It is invoked as `/aggy` in Claude Code and Grok Build,
and `$aggy` in Codex; each CLI may also load it on its own when the
description matches the task.

`aggy skill link` installs and links it; see docs/design.md section 9.
