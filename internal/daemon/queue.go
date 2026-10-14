package daemon

import "sync"

// queue is an unbounded FIFO: producers (the capture loop) never block.
type queue[T any] struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []T
	closed bool
	busy   bool
}

func newQueue[T any]() *queue[T] {
	q := &queue[T]{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *queue[T]) put(v T) {
	q.mu.Lock()
	if !q.closed {
		q.items = append(q.items, v)
	}
	q.mu.Unlock()
	q.cond.Broadcast()
}

// get blocks for the next item; ok is false once closed and drained.
func (q *queue[T]) get() (v T, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.busy = false
	q.cond.Broadcast()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return v, false
	}
	v = q.items[0]
	var zero T
	q.items[0] = zero
	q.items = q.items[1:]
	q.busy = true
	return v, true
}

// close stops accepting items; queued items still drain.
func (q *queue[T]) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// wait blocks until the queue is empty and the consumer is between items.
func (q *queue[T]) wait() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) > 0 || q.busy {
		q.cond.Wait()
	}
}
