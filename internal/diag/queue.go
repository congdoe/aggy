package diag

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Queue is the writer's path for code that must never wait on the disk,
// such as the supervisor's terminal loop and its socket: Log only
// enqueues, and one goroutine writes, in order. When the queue is full a
// record is dropped and counted, and once the queue is closed one
// diag.queue.dropped record carries the count. A nil *Queue writes
// nothing.
type Queue struct {
	w       *Writer
	ch      chan Rec
	done    chan struct{}
	dropped atomic.Int64

	mu     sync.RWMutex // held to send, and to close ch
	closed bool
}

// Queue starts a queue of n records in front of w; nil for a nil w.
func (w *Writer) Queue(n int) *Queue {
	if w == nil {
		return nil
	}
	q := &Queue{w: w, ch: make(chan Rec, n), done: make(chan struct{})}
	go q.run()
	return q
}

func (q *Queue) run() {
	defer close(q.done)
	for r := range q.ch {
		q.w.Log(context.Background(), r)
	}
	if n := q.dropped.Load(); n > 0 {
		q.w.Log(context.Background(), Rec{Event: EvQueueDropped, Code: CodeQueueFull,
			Msg: "the diagnostic log fell behind and dropped records", Attrs: map[string]any{"dropped": n}})
	}
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
func (q *Queue) Log(r Rec) string {
	if q == nil {
		return ""
	}
	now := q.w.now()
	r.at = now
	r.id = q.w.ids.next(now)
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return ""
	}
	select {
	case q.ch <- r:
		return r.id
	default:
		q.dropped.Add(1)
		return ""
	}
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
	}
	q.mu.Unlock()
	select {
	case <-q.done:
	case <-time.After(wait):
	}
}
