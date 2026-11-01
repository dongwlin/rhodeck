package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOutputAbortNeverStartsAnotherWrite(t *testing.T) {
	sink := newBlockingWriter()
	t.Cleanup(sink.unblock)
	writer := newRunOutput(json.NewEncoder(sink))
	result := make(chan error, 1)
	go func() {
		result <- writer.write(context.Background(), nil, envelope{SchemaVersion: 1, Type: "event"}, 20*time.Millisecond, func() {})
	}()
	select {
	case <-sink.started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "output write never started")
	}
	err := <-result
	require.ErrorIs(t, err, errOutputTimeout, "want output timeout")
	writer.close()
	sink.unblock()
	select {
	case <-writer.done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "output goroutine did not finish after abort and writer release")
	}
	require.Equal(t, int32(1), sink.writes.Load(), "want one in-flight write")
}
