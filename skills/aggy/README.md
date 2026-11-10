# aggy skill

`SKILL.md` is the skill aggy links into each vendor CLI. It tells the
on-screen agent which kinds of work to borrow, when, and how
(`aggy borrow`, `aggy inbox`), how the user switches (`/switch`,
`$switch`, Ctrl+\), and, as `/aggy <question>`, how to answer questions
about aggy from `aggy <command> --help` and `aggy diag` without changing
anything (T6911). One file serves all three CLIs: each
reads a `SKILL.md` with `name` and `description` frontmatter (Codex
requires both). It is invoked as `/aggy` in Claude Code and Grok Build,
and `$aggy` in Codex; each CLI may also load it on its own when the
description matches the task.

`aggy skill link` installs and links it, and `aggy setup` offers to; an
upgraded aggy rewrites a linked copy at launch, through the manifest. See
docs/design.md section 9. The command skills beside it (`switch`,
`borrow`, `inbox`) are linked with it.

## Token cost

Estimated offline as bytes / 4, rounded up (`skills.Estimate`; no
tokenizer or model call): about 75 tokens every turn for the frontmatter
a CLI keeps loaded, and about 1,440 more when the skill is invoked. A test
holds them under 150 and 1,500; `aggy skill status` prints the current
numbers.
