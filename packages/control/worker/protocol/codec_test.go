package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRawTaskRoundTrip(t *testing.T) {
	params := json.RawMessage(`{"unknown":{"large":900719925474099312345,"text":"a\\b"}}`)
	m, err := NewMessage(Request, AppendTask, 1, TaskRequest{Type: "FutureTask", Params: params})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewReader(bytes.NewReader(encoded)).Read()
	if err != nil {
		t.Fatal(err)
	}
	var task TaskRequest
	if err := Decode(got.Payload, &task); err != nil {
		t.Fatal(err)
	}
	if task.Type != "FutureTask" || !bytes.Equal(task.Params, params) {
		t.Fatalf("task changed: %+v", task)
	}
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
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewReader(strings.NewReader(raw)).Read(); err == nil {
				t.Fatal("corrupt frame accepted")
			}
		})
	}
	if _, err := NewReader(strings.NewReader("")).Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream: %v", err)
	}
}

func TestFrameByteLimit(t *testing.T) {
	m, _ := NewMessage(Request, "x", 1, "")
	empty, _ := Encode(m)
	m.Payload, _ = json.Marshal(strings.Repeat("x", MaxFrameBytes-len(empty)+1))
	data, err := Encode(m)
	if err != nil || len(data) != MaxFrameBytes+1 {
		t.Fatalf("exact bound: %d, %v", len(data), err)
	}
	if _, err = NewReader(bytes.NewReader(data)).Read(); err != nil {
		t.Fatal(err)
	}
	m.Payload, _ = json.Marshal(strings.Repeat("x", MaxFrameBytes))
	if _, err = Encode(m); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize encode: %v", err)
	}
	if _, err = NewReader(strings.NewReader(strings.Repeat("x", MaxFrameBytes+1))).Read(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize read: %v", err)
	}
}

func TestQueueBoundsAndReclaimsBytes(t *testing.T) {
	m, _ := NewMessage(Request, Ping, 1, nil)
	q := NewQueue()
	for range QueueMessages {
		if _, err := q.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Send(m); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("count bound: %v", err)
	}
	if _, err := q.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Send(m); err != nil {
		t.Fatalf("slot not reclaimed: %v", err)
	}
	q = NewQueue()
	m, _ = NewMessage(Request, "large", 1, strings.Repeat("x", MaxFrameBytes-1024))
	for range 2 {
		if _, err := q.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Send(m); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("byte bound: %v", err)
	}
	if _, err := q.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Send(m); err != nil {
		t.Fatalf("bytes not reclaimed: %v", err)
	}
}
