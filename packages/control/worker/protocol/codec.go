package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

var ErrFrameTooLarge = errors.New("worker: frame exceeds byte limit")

// Decode rejects unknown control fields and trailing data. RawMessage members
// deliberately retain their own schema, unknown fields, and integer precision.
func Decode(raw []byte, target any) error {
	if !utf8.Valid(raw) {
		return errors.New("worker: invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("worker: trailing JSON data")
	}
	return nil
}

func (m Message) Validate() error {
	if m.Version != Version {
		return fmt.Errorf("worker: unsupported protocol version %d", m.Version)
	}
	if m.Error != nil && (m.Error.Code == "" || m.Error.Message == "" || len(m.Error.Message) > 4096) {
		return errors.New("worker: invalid error payload")
	}
	switch m.Type {
	case Hello:
		if m.ID != 0 || m.Name != "" || m.Sequence != 0 || m.Error != nil {
			return errors.New("worker: invalid greeting")
		}
	case Request:
		if m.ID == 0 || m.Name == "" || m.Sequence != 0 || m.Error != nil {
			return errors.New("worker: invalid request")
		}
	case Response:
		if m.ID == 0 || m.Name == "" || m.Sequence != 0 {
			return errors.New("worker: invalid response")
		}
	case Event:
		if m.ID != 0 || m.Name == "" || m.Sequence == 0 || m.Error != nil {
			return errors.New("worker: invalid event")
		}
	default:
		return fmt.Errorf("worker: invalid message type %q", m.Type)
	}
	if m.Payload != nil && (!json.Valid(m.Payload) || bytes.Equal(bytes.TrimSpace(m.Payload), []byte("null"))) {
		return errors.New("worker: invalid payload")
	}
	return nil
}

func NewMessage(kind, name string, id uint64, payload any) (Message, error) {
	m := Message{Version: Version, Type: kind, Name: name, ID: id}
	if payload != nil {
		var err error
		m.Payload, err = json.Marshal(payload)
		if err != nil {
			return Message{}, err
		}
	}
	return m, nil
}

func Encode(m Message) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	return append(data, '\n'), nil
}

// Reader requires a newline even at EOF. It bounds allocation before decoding,
// including on a peer that never terminates an oversized frame.
type Reader struct{ reader *bufio.Reader }

func NewReader(r io.Reader) *Reader { return &Reader{bufio.NewReaderSize(r, 64<<10)} }

func (r *Reader) Read() (Message, error) {
	var data []byte
	for {
		fragment, err := r.reader.ReadSlice('\n')
		limit := MaxFrameBytes
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			limit++
		}
		if len(data)+len(fragment) > limit {
			return Message{}, ErrFrameTooLarge
		}
		data = append(data, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(data) > 0 {
				err = io.ErrUnexpectedEOF
			}
			return Message{}, err
		}
		var m Message
		if err := Decode(data, &m); err != nil {
			return Message{}, fmt.Errorf("worker: decode frame: %w", err)
		}
		return m, m.Validate()
	}
}

func WriteFrame(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
