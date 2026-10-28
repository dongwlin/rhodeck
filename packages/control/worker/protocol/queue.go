package protocol

import (
	"context"
	"errors"
	"sync"
)

var ErrQueueFull = errors.New("worker: message queue overflow")

type Frame struct {
	Data    []byte
	Written chan error // The sole writer reports completion exactly once.
}

// Queue never waits on a producer. Both message count and retained encoded bytes
// are bounded. A consumer may additionally hold one frame during a blocking write.
type Queue struct {
	mu     sync.Mutex
	bytes  int
	frames chan Frame
}

func NewQueue() *Queue { return &Queue{frames: make(chan Frame, QueueMessages)} }

func (q *Queue) Send(m Message) (<-chan error, error) {
	data, err := Encode(m)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(data) > QueueBytes-q.bytes {
		return nil, ErrQueueFull
	}
	f := Frame{Data: data, Written: make(chan error, 1)}
	select {
	case q.frames <- f:
		q.bytes += len(data)
		return f.Written, nil
	default:
		return nil, ErrQueueFull
	}
}

func (q *Queue) Next(ctx context.Context) (Frame, error) {
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case f := <-q.frames:
		q.mu.Lock()
		q.bytes -= len(f.Data)
		q.mu.Unlock()
		return f, nil
	}
}
