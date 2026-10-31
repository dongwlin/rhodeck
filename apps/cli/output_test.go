package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
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
		t.Fatal("output write never started")
	}
	if err := <-result; !errors.Is(err, errOutputTimeout) {
		t.Fatalf("write = %v, want output timeout", err)
	}
	writer.close()
	sink.unblock()
	select {
	case <-writer.done:
	case <-time.After(5 * time.Second):
		t.Fatal("output goroutine did not finish after abort and writer release")
	}
	if got := sink.writes.Load(); got != 1 {
		t.Fatalf("write count = %d, want one in-flight write", got)
	}
}
