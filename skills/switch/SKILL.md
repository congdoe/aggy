---
name: switch
description: aggy's switch to another vendor's CLI, picked by the person. Not for the model.
disable-model-invocation: true
argument-hint: "[claude|codex|grok] [--brief] [--new] [--model ID]"
---

# /switch reached you

aggy catches this command before the model sees it: typed in full at the
prompt, or picked from the menu where aggy's prompt hook runs. It reached you,
so aggy did not catch it. Change nothing and run nothing: do not run
`aggy switch` for it, and do not end the session.

Answer in one short paragraph:

- If this session was started with `aggy` (`AGGY_SOCKET` is set in its
  environment), say: type `/switch` at the prompt and press Enter for aggy's
  picker, or `/switch <vendor>` to switch at once (`$switch` on Codex), or
  press Ctrl+\. Typed in full, it spends no turn.
- Otherwise, say that switching CLIs needs a session started with `aggy`
  (or `aggy claude`, `aggy codex`, `aggy grok`), and that aggy is not
  running this one.
