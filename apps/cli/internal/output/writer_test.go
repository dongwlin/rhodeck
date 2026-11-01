package output

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// blockingWriter blocks every write until it is released. It records how many
// writes were started.
type blockingWriter struct {
	started     chan struct{}
	release     chan struct{}
	returned    chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
	returnOnce  sync.Once
	writes      atomic.Int32
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{started: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.writes.Add(1)
	w.startOnce.Do(func() { close(w.started) })
	<-w.release
	w.returnOnce.Do(func() { close(w.returned) })
	return len(p), nil
}

func (w *blockingWriter) unblock() { w.releaseOnce.Do(func() { close(w.release) }) }

// TestOutputAbortNeverStartsAnotherWrite keeps the single writer contract: a
// blocked write is never retried and the writer goroutine ends after the
// abort.
func TestOutputAbortNeverStartsAnotherWrite(t *testing.T) {
	sink := newBlockingWriter()
	t.Cleanup(sink.unblock)
	writer := NewWriter(sink)
	writer.SetGrace(20 * time.Millisecond)
	result := make(chan error, 1)
	go func() {
		result <- writer.Emit(context.Background(), map[string]any{"schema_version": 1, "type": "event"}, nil, nil)
	}()
	select {
	case <-sink.started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "output write never started")
	}
	err := <-result
	require.ErrorIs(t, err, ErrTimeout, "want output timeout")
	writer.Close()
	sink.unblock()
	select {
	case <-writer.done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "output goroutine did not finish after abort and writer release")
	}
	require.Equal(t, int32(1), sink.writes.Load(), "want one in-flight write")
}
