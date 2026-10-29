package cli

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/store"
)

// TestDiagFollowReopenRetries (T6811, L4): a failed reopen of diag.db
// (say, while it is being created) keeps --follow running on its old
// view; it reads the records once a reopen works.
func TestDiagFollowReopenRetries(t *testing.T) {
	newBorrowFixture(t)
	path, err := store.DiagPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("diag.db exists before the test: %v", err)
	}
	oldPoll, oldTries, oldHook := diagPollEvery, diagReopenTries, diagPolled
	t.Cleanup(func() { diagPollEvery, diagReopenTries, diagPolled = oldPoll, oldTries, oldHook })
	var polls, fails atomic.Int32
	diagPollEvery, diagReopenTries = 10*time.Millisecond, 1000
	diagPolled = func(n int) { polls.Add(1); fails.Store(int32(n)) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuilder
	done := make(chan int, 1)
	go func() { done <- diagWith(ctx, []string{"--follow", "--jsonl"}, time.Now, &out, &out) }()
	deadline := time.Now().Add(10 * time.Second)
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for !ok() {
			select {
			case code := <-done:
				t.Fatalf("--follow ended (code %d) waiting for %s: %s", code, what, out.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %s", what, out.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("the first poll", func() bool { return polls.Load() > 0 })
	// Not a database yet: every reopen fails until it is removed.
	if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor("two failed reopens", func() bool { return fails.Load() >= 2 })
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	dlog.Log(context.Background(), diag.Rec{Event: diag.EvGCRemoved, Msg: "followed"})
	waitFor("the record", func() bool { return strings.Contains(out.String(), "followed") })
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("code %d after Ctrl-C", code)
	}
}
