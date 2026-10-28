package supervisor

import "sync"

// maxQueued caps the input waiting for a CLI that is not reading.
//
// shortcut: input past the cap is dropped, as a full terminal buffer
// would; nobody types or pastes 4 MiB into a stalled CLI on purpose.
// Once anything is dropped, everything is until the queue drains, so the
// CLI never gets input with a gap in it (B13-15).
const maxQueued = 4 << 20

// byteQueue is an unbounded-until-max FIFO of byte slices: push never
// blocks, pop waits for data or close.
type byteQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  [][]byte
	size   int
	max    int
	closed bool
	// dropping is set by an overflow and cleared once the queue is empty.
	dropping bool
}

func newByteQueue(max int) *byteQueue {
	q := &byteQueue{max: max}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// push queues b, or drops it when the queue is full or closed.
func (q *byteQueue) push(b []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.dropping || q.size+len(b) > q.max {
		q.dropping = !q.closed
		return
	}
	q.items = append(q.items, b)
	q.size += len(b)
	q.cond.Signal()
}

// pop returns the oldest slice, waiting for one; ok is false once the
// queue is closed and empty.
func (q *byteQueue) pop() (b []byte, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return nil, false
	}
	b = q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	q.size -= len(b)
	if len(q.items) == 0 {
		q.dropping = false
	}
	return b, true
}

func (q *byteQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}
