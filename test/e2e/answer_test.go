//go:build unix

package e2e

import (
	"bytes"
	"time"
)

// answerUntil types answer to a question aggy asked in place and waits for
// marker, the sign the answer was taken (#133 review). aggy drops a line that
// arrives within its typed-ahead window of the question, and nothing on the
// screen says when that window ends, so instead of sleeping past it the
// answer is typed again while the question's "[y/N]" is still the last thing
// on screen: a dropped line leaves the question waiting, and the next one
// answers it. Once anything follows the question, it is never typed again, so
// a second answer cannot reach the CLI started after a yes (#133 re-review).
// It fails the test if marker never shows within timeout.
func (s *session) answerUntil(answer string, marker []byte) {
	s.t.Helper()
	// The terminal echoes each answer typed, dropped or not, so the echoes
	// are taken off the end before looking for the question.
	echo := bytes.TrimSpace([]byte(answer))
	waiting := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		b := s.out.Bytes()
		for {
			b = bytes.TrimRight(b, " \r\n")
			if len(echo) == 0 || !bytes.HasSuffix(b, echo) {
				break
			}
			b = b[:len(b)-len(echo)]
		}
		return bytes.HasSuffix(b, []byte("[y/N]"))
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && waiting() {
		s.send([]byte(answer))
		retry := time.Now().Add(250 * time.Millisecond)
		for time.Now().Before(retry) && waiting() {
			time.Sleep(10 * time.Millisecond)
		}
	}
	s.waitFor(marker)
}
