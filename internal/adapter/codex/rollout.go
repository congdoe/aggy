package codex

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// Codex writes each session's rollout to
// <codex home>/sessions/<YYYY>/<MM>/<DD>/rollout-<timestamp>-<thread id>.jsonl,
// and creates it lazily, at the first persist rather than at session
// start (codex-rs/rollout/src/recorder.rs at 6aaac694:
// precompute_new_rollout_path and the RolloutRecorder doc comments;
// docs/verification.md, Hook payloads). Which clock names the date
// folder is not recorded, so RolloutDirs lists both.

// RolloutThreadID returns the thread id in a rollout file name, or false
// when name is not one.
func RolloutThreadID(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "rollout-")
	if !ok {
		return "", false
	}
	rest, ok = strings.CutSuffix(rest, ".jsonl")
	if !ok || len(rest) < 37 || rest[len(rest)-37] != '-' {
		return "", false
	}
	id := rest[len(rest)-36:]
	return id, adapter.ValidSessionID(adapter.Codex, id) == nil
}

// RolloutDirs returns the date folders under sessionsDir a session
// started at start and still running at now could write to, in local
// time and UTC, a day either side, without duplicates.
func RolloutDirs(sessionsDir string, start, now time.Time) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range []time.Time{start.Add(-24 * time.Hour), start, now, now.Add(24 * time.Hour)} {
		for _, u := range []time.Time{t.Local(), t.UTC()} {
			d := filepath.Join(sessionsDir, u.Format("2006"), u.Format("01"), u.Format("02"))
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}
