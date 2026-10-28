# aggy skill

`SKILL.md` is the skill aggy links into each vendor CLI. It tells the
on-screen agent which kinds of work to borrow, when, and how
(`aggy borrow`, `aggy inbox`). One file serves all three CLIs: each
reads a `SKILL.md` with `name` and `description` frontmatter (Codex
requires both). It is invoked as `/aggy` in Claude Code and Grok Build,
and `$aggy` in Codex; each CLI may also load it on its own when the
description matches the task.

`aggy skill link` installs and links it; see docs/design.md section 9.

## Token cost

Estimated offline as bytes / 4, rounded up (`skills.Estimate`; no
tokenizer or model call): about 60 tokens every turn for the frontmatter
a CLI keeps loaded, and about 640 more when the skill is invoked. A test
holds them under 150 and 1,500; `aggy skill status` prints the current
numbers.
