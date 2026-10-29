# End-to-end tests

The supervisor driven in a pseudo-terminal against the fake CLIs.

CI runs them on Linux and macOS with `go test -race -p 1 ./test/e2e`. They wait on what the terminal shows or what the fakes record; the switch confirmation's guard is wall time by design, so a test waits past it on the clock.

| Flow | Test |
| --- | --- |
| Switch key: picker, brief toggle, next CLI with the brief | `TestSwitchAtTurnEnd` |
| Turn end: the Stop hook, `/exit` typed, the next child | `TestSwitchAtTurnEnd`, `TestSkillRequestedSwitch` |
| Plan limit to a switch | `TestLimitSwitchWithBrief` |
| Brief file: written, handed over, content and redaction | `TestLimitSwitchWithBrief` |
| Borrow from the on-screen agent, answer back | `TestBorrowRoundTrip` |
