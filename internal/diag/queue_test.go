package diag

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestQueueSlowWriter (T6753): with a writer that takes 50 ms a record,
// Log never waits: a burst of records returns at once, those past the
// queue are dropped and counted, the rest are written in order with the
// ids Log returned, and closing writes one diag.queue.dropped record with
// the count.
func TestQueueSlowWriter(t *testing.T) {
	w, _, dir := testWriter(t, "info")
	w.diagPath = func() (string, error) {
		time.Sleep(50 * time.Millisecond)
		return filepath.Join(dir, "diag.db"), nil
	}
	q := w.Queue(4)
	start := time.Now()
	var ids []string
	for i := 0; i < 100; i++ {
		ids = append(ids, q.Log(Rec{Event: EvPickerOpened, Attrs: map[string]any{"i": i}}))
	}
	if d := time.Since(start); d > time.Second { // blocking would take 5 s
		t.Errorf("100 Logs took %s; Log must not wait for the writer", d)
	}
	if q.Dropped() < 90 {
		t.Errorf("dropped %d, want most of 100", q.Dropped())
	}
	q.Close(10 * time.Second)
	if q.Log(Rec{Event: EvPickerOpened}) != "" {
		t.Error("a Log after Close was taken")
	}
	q.Close(time.Second) // twice is fine
	recs := records(t, dir)
	var kept []string
	for _, id := range ids {
		if id != "" {
			kept = append(kept, id)
		}
	}
	if len(recs) != len(kept)+1 {
		t.Fatalf("%d records, want %d kept and the drop count", len(recs), len(kept))
	}
	for i, id := range kept {
		if recs[i].ID != id {
			t.Errorf("record %d is %s, want %s", i, recs[i].ID, id)
		}
	}
	last := recs[len(recs)-1]
	if last.Event != EvQueueDropped.String() || last.Code != CodeQueueFull.String() || string(last.Attrs) != `{"dropped":`+strconv.FormatInt(q.Dropped(), 10)+`,"dropped_security":0}` {
		t.Errorf("last record %+v", last)
	}
	var nilQ *Queue
	if nilQ.Log(Rec{Event: EvPickerOpened}) != "" || nilQ.Writer() != nil || nilQ.Dropped() != 0 {
		t.Error("a nil queue is not a no-op")
	}
	nilQ.Close(0)
}

// TestQueueSecurityFirst (G67-3): a queue full of plain records still
// takes a record with a security code, writes it first, and counts a
// dropped one apart; LogWait waits for room.
func TestQueueSecurityFirst(t *testing.T) {
	w, _, dir := testWriter(t, "info")
	gate := make(chan struct{})
	w.diagPath = func() (string, error) {
		<-gate
		return filepath.Join(dir, "diag.db"), nil
	}
	q := w.Queue(2)
	for i := 0; i < 10; i++ {
		q.Log(Rec{Event: EvPickerOpened})
	}
	sec := q.Log(Rec{Event: EvHookRejected, Code: CodeLineage})
	if sec == "" {
		t.Fatal("a security record was dropped behind plain ones")
	}
	for i := 0; i < secQueue+5; i++ {
		q.Log(Rec{Event: EvHookRejected, Code: CodeLineage})
	}
	if q.droppedSec.Load() == 0 {
		t.Error("no dropped security record counted")
	}
	close(gate)
	q.Close(30 * time.Second)
	recs := records(t, dir)
	var secRow int64
	for _, r := range recs {
		if r.ID == sec {
			secRow = r.Row
		}
	}
	before := 0
	for _, r := range recs {
		if r.Code == "" && r.Row < secRow {
			before++
		}
	}
	// One plain record was already with the writer.
	if secRow == 0 || before > 1 {
		t.Errorf("the security record (row %d) was written after %d plain ones", secRow, before)
	}
	// G67-R1: the flood of security records lets a plain one through
	// after secBurst of them.
	var plain []int64
	for _, r := range recs {
		if r.Code == "" {
			plain = append(plain, r.Row)
		}
	}
	sort.Slice(plain, func(i, j int) bool { return plain[i] < plain[j] })
	if len(plain) < 2 || plain[1] > plain[0]+secBurst+1 {
		t.Errorf("plain records at rows %v: starved by security ones", plain)
	}
	last := recs[len(recs)-1]
	if last.Event != EvQueueDropped.String() || !strings.Contains(string(last.Attrs), `"dropped_security":`) {
		t.Errorf("last record %+v", last)
	}
}

// TestQueueLogWait: LogWait waits for room; Log does not.
func TestQueueLogWait(t *testing.T) {
	w, _, dir := testWriter(t, "info")
	gate := make(chan struct{})
	w.diagPath = func() (string, error) {
		<-gate
		return filepath.Join(dir, "diag.db"), nil
	}
	q := w.Queue(1)
	q.Log(Rec{Event: EvPickerOpened}) // taken by the writer, which waits
	time.Sleep(20 * time.Millisecond)
	q.Log(Rec{Event: EvPickerOpened}) // fills the queue
	if q.Log(Rec{Event: EvPickerOpened}) != "" {
		t.Fatal("Log took a record past a full queue")
	}
	go func() { time.Sleep(20 * time.Millisecond); close(gate) }()
	if q.LogWait(Rec{Event: EvPickerOpened}, 10*time.Second) == "" {
		t.Error("LogWait did not wait for room")
	}
	q.Close(30 * time.Second)
}

// TestQueuePanicAndHold (G67-5): a panic while writing is a failed write,
// not a crash, and its warning waits for Release.
func TestQueuePanicAndHold(t *testing.T) {
	w, warn, _ := testWriter(t, "info")
	w.diagPath = func() (string, error) { panic("disk on fire") }
	w.Hold()
	q := w.Queue(4)
	q.Log(Rec{Event: EvPickerOpened})
	q.Close(5 * time.Second)
	if warn.Len() != 0 {
		t.Fatalf("a warning reached the terminal while held: %q", warn.String())
	}
	w.Release()
	if !strings.Contains(warn.String(), "panic: disk on fire") {
		t.Errorf("warning %q", warn.String())
	}
}

// TestWriterScrubsEnv (G67-1): the values of AGGY_SOCKET and AGGY_HOME,
// which a CLI's tree sets at will, never reach a record.
func TestWriterScrubsEnv(t *testing.T) {
	t.Setenv("AGGY_SOCKET", "/tmp/sock-canary-heron-91.sock")
	t.Setenv("AGGY_HOME", "relative-canary-heron-92")
	w := New("v-test", nil)
	got := w.scrub("dial /tmp/sock-canary-heron-91.sock: no such file; AGGY_HOME relative-canary-heron-92 is not absolute", maxMsg)
	if strings.Contains(got, "canary") || strings.Count(got, "[env value]") != 2 {
		t.Errorf("scrubbed %q", got)
	}
}
