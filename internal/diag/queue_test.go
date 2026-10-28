package diag

import (
	"path/filepath"
	"strconv"
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
	if last.Event != EvQueueDropped.String() || last.Code != CodeQueueFull.String() || string(last.Attrs) != `{"dropped":`+strconv.FormatInt(q.Dropped(), 10)+`}` {
		t.Errorf("last record %+v", last)
	}
	var nilQ *Queue
	if nilQ.Log(Rec{Event: EvPickerOpened}) != "" || nilQ.Writer() != nil || nilQ.Dropped() != 0 {
		t.Error("a nil queue is not a no-op")
	}
	nilQ.Close(0)
}
