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
// answer is typed again whenever marker has not shown: a dropped line leaves
// the question waiting, and the next one answers it. It fails the test if
// marker never shows within timeout.
func (s *session) answerUntil(answer string, marker []byte) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Not send: once aggy has exited a second answer may find no one.
		_, _ = s.master.Write([]byte(answer))
		retry := time.Now().Add(250 * time.Millisecond)
		for time.Now().Before(retry) {
			s.mu.Lock()
			ok := bytes.Contains(s.out.Bytes(), marker)
			s.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	s.waitWithin(marker, 0) // fails, showing the screen
}
