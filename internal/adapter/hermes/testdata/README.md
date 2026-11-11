# Hermes test data

`export.handbuilt.jsonl` is hand-built, not captured: no real Hermes ran. It
follows the writer at hermes-agent v0.23.1 (commit 3b42157858):
`hermes sessions export - --format jsonl` writes one JSON session per line
(`hermes_cli/sessions_cmd.py:114-118`), the dict `export_session` returns
(`hermes_state_portability.py:347-355`) with its messages as the rows of the
`messages` table (`hermes_state_common.py:537-547`) passed through
`_row_to_message_dict` (`hermes_state_messages.py:1308-1330`). The session id,
times, and text are made up. Replace it with a scrubbed capture once a live
export is recorded in docs/verification.md.
