package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRawTaskRoundTrip(t *testing.T) {
	params := json.RawMessage(`{"unknown":{"large":900719925474099312345,"text":"a\\b"}}`)
	m, err := NewMessage(Request, AppendTask, 1, TaskRequest{Type: "FutureTask", Params: params})
	require.NoError(t, err)
	encoded, err := Encode(m)
	require.NoError(t, err)
	got, err := NewReader(bytes.NewReader(encoded)).Read()
	require.NoError(t, err)
	var task TaskRequest
	require.NoError(t, Decode(got.Payload, &task))
	require.True(t, task.Type == "FutureTask" && bytes.Equal(task.Params, params), "task changed: %+v", task)
}

func TestReaderRejectsCorruptFrames(t *testing.T) {
	for name, raw := range map[string]string{
		"version":                `{"version":2,"type":"request","id":1,"name":"ping"}` + "\n",
		"unknown field":          `{"version":1,"type":"request","id":1,"name":"ping","extra":1}` + "\n",
		"zero request":           `{"version":1,"type":"request","name":"ping"}` + "\n",
		"event without sequence": `{"version":1,"type":"event","name":"core"}` + "\n",
		"trailing data":          `{"version":1,"type":"request","id":1,"name":"ping"} {}` + "\n",
		"no newline":             `{"version":1,"type":"request","id":1,"name":"ping"}`,
		"utf8":                   "{\"version\":1,\"type\":\"\xff\"}\n",
		"null payload":           `{"version":1,"type":"request","id":1,"name":"ping","payload":null}` + "\n",
		"duplicate field":        `{"version":1,"type":"request","id":1,"id":2,"name":"ping"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewReader(strings.NewReader(raw)).Read()
			require.Error(t, err, "corrupt frame accepted")
		})
	}
	_, err := NewReader(strings.NewReader("")).Read()
	require.ErrorIs(t, err, io.EOF, "empty stream: %v", err)
}

func TestFrameByteLimit(t *testing.T) {
	m, _ := NewMessage(Request, "x", 1, map[string]string{"data": ""})
	empty, _ := Encode(m)
	m.Payload, _ = json.Marshal(map[string]string{"data": strings.Repeat("x", MaxFrameBytes-len(empty)+1)})
	data, err := Encode(m)
	require.NoError(t, err)
	require.Len(t, data, MaxFrameBytes+1, "exact bound")
	_, err = NewReader(bytes.NewReader(data)).Read()
	require.NoError(t, err)
	m.Payload, _ = json.Marshal(map[string]string{"data": strings.Repeat("x", MaxFrameBytes)})
	_, err = Encode(m)
	require.ErrorIs(t, err, ErrFrameTooLarge, "oversize encode: %v", err)
	_, err = NewReader(strings.NewReader(strings.Repeat("x", MaxFrameBytes+1))).Read()
	require.ErrorIs(t, err, ErrFrameTooLarge, "oversize read: %v", err)
}

func TestQueueBoundsAndReclaimsBytes(t *testing.T) {
	m, _ := NewMessage(Request, Ping, 1, nil)
	q := NewQueue()
	for range QueueMessages {
		_, err := q.Send(m)
		require.NoError(t, err)
	}
	_, err := q.Send(m)
	require.ErrorIs(t, err, ErrQueueFull, "count bound: %v", err)
	_, err = q.Next(context.Background())
	require.NoError(t, err)
	_, err = q.Send(m)
	require.NoError(t, err, "slot not reclaimed: %v", err)
	q = NewQueue()
	m, _ = NewMessage(Request, "large", 1, map[string]string{"data": strings.Repeat("x", MaxFrameBytes-1024)})
	for range 2 {
		_, err := q.Send(m)
		require.NoError(t, err)
	}
	_, err = q.Send(m)
	require.ErrorIs(t, err, ErrQueueFull, "byte bound: %v", err)
	_, err = q.Next(context.Background())
	require.NoError(t, err)
	_, err = q.Send(m)
	require.NoError(t, err, "bytes not reclaimed: %v", err)
}
