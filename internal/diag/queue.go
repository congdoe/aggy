package diag

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// secQueue is how many records with a security code a Queue keeps apart
// from the rest, so a flood of plain records cannot crowd one out.
const secQueue = 64

// secBurst is how many security records go in a row while plain ones
// wait.
const secBurst = 8

// Queue is the writer's path for code that must never wait on the disk,
// such as the supervisor's terminal loop and its socket: Log only
// enqueues, and one goroutine writes, records with a security code first.
// When the queue is full a record is dropped and counted, and once the
// queue is closed one diag.queue.dropped record carries the counts. A nil
// *Queue writes nothing.
type Queue struct {
	w       *Writer
	ch, sec chan Rec
	done    chan struct{}
	dropped atomic.Int64
	// droppedSec counts the dropped records with a security code.
	droppedSec atomic.Int64

	mu     sync.RWMutex // held to send, and to close the channels
	closed bool
}

// Queue starts a queue of n records in front of w; nil for a nil w.
func (w *Writer) Queue(n int) *Queue {
	if w == nil {
		return nil
	}
	q := &Queue{w: w, ch: make(chan Rec, n), sec: make(chan Rec, secQueue), done: make(chan struct{})}
	go q.run()
	return q
}

func (q *Queue) run() {
	defer close(q.done)
	ch, sec := q.ch, q.sec
	// burst counts the security records written in a row: after
	// secBurst, a waiting plain record goes next, so a flood of security
	// records cannot starve the rest (G67-R1).
	burst := 0
	for ch != nil || sec != nil {
		var r Rec
		var ok, isSec bool
		if burst >= secBurst && ch != nil {
			select {
			case r, ok = <-ch:
				burst = 0
				if !ok {
					ch = nil
					continue
				}
				q.write(r)
				continue
			default:
			}
		}
		// Security records first, whenever one is waiting.
		select {
		case r, ok = <-sec:
			isSec = true
		default:
			select {
			case r, ok = <-sec:
				isSec = true
			case r, ok = <-ch:
			}
		}
		switch {
		case !ok && isSec:
			sec = nil
			continue
		case !ok:
			ch = nil
			continue
		case isSec:
			burst++
		default:
			burst = 0
		}
		q.write(r)
	}
	if n := q.dropped.Load(); n > 0 {
		q.write(Rec{Event: EvQueueDropped, Code: CodeQueueFull, Msg: "the diagnostic log fell behind and dropped records",
			Attrs: map[string]any{"dropped": n, "dropped_security": q.droppedSec.Load()}})
	}
}

// write writes r. A panic in the writer is reported as a failed write
// and never ends aggy, whose terminal may be raw (G67-5).
func (q *Queue) write(r Rec) {
	defer func() {
		if p := recover(); p != nil {
			q.w.warnOnce(fmt.Errorf("panic: %v", p))
		}
	}()
	q.w.Log(context.Background(), r)
}

// Writer is the queue's writer, for its run id and tags; nil for a nil q.
func (q *Queue) Writer() *Writer {
	if q == nil {
		return nil
	}
	return q.w
}

// Log enqueues r without waiting and returns the id it will have, so a
// later record can name it as its cause. The record keeps the time it was
// enqueued. It returns "" when r was dropped: q is nil or closed, or full.
func (q *Queue) Log(r Rec) string { return q.LogWait(r, 0) }

// LogWait is Log, but waits up to d for room when the queue is full: for
// goroutines that are not the terminal loop, such as the socket's.
func (q *Queue) LogWait(r Rec, d time.Duration) string {
	if q == nil {
		return ""
	}
	now := q.w.now()
	r.at = now
	r.id = q.w.ids.next(now)
	ch := q.ch
	c, _ := LookupCode(r.Code)
	if c.Security {
		ch = q.sec
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return ""
	}
	select {
	case ch <- r:
		return r.id
	default:
	}
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case ch <- r:
			return r.id
		case <-t.C:
		}
	}
	q.dropped.Add(1)
	if c.Security {
		q.droppedSec.Add(1)
	}
	return ""
}

// Dropped is how many records were dropped so far.
func (q *Queue) Dropped() int64 {
	if q == nil {
		return 0
	}
	return q.dropped.Load()
}

// Close stops taking records and waits up to wait for the queued ones to
// be written; past it they are left to the goroutine, which a process
// that exits cuts short. It may be called more than once.
func (q *Queue) Close(wait time.Duration) {
	if q == nil {
		return
	}
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.ch)
		close(q.sec)
	}
	q.mu.Unlock()
	select {
	case <-q.done:
	case <-time.After(wait):
	}
}
