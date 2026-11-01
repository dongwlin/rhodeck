//go:build linux

// This file drives the real core-worker binary over raw JSONL, without the
// supervision client, to prove the worker's own wire discipline: malformed
// payloads and unknown commands are answered with stable codes while the
// session survives, malicious image chunk requests never serve bytes outside
// the owned snapshot, and broken frames — invalid UTF-8, duplicate or unknown
// control fields, wrong protocol versions, repeated request IDs, oversized
// frames and an abrupt parent EOF — terminate the session loudly and reap the
// process. Nothing touches a device.
package worker

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	"github.com/stretchr/testify/require"
)

// rawWorker bundles one real worker process with its raw pipes for the
// adversarial wire tests.
type rawWorker struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	reader      *protocol.Reader
	diagnostics *bytes.Buffer
}

// launchRawWorker starts the real binary and consumes its greeting.
func launchRawWorker(t *testing.T) *rawWorker {
	t.Helper()
	cmd := exec.Command(workerBinary(t))
	diagnostics := &bytes.Buffer{}
	cmd.Stderr = diagnostics
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	cleaned := false
	t.Cleanup(func() {
		if !cleaned && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		_ = stdin.Close()
	})
	rw := &rawWorker{cmd: cmd, stdin: stdin, reader: protocol.NewReader(stdout), diagnostics: diagnostics}
	greeting := rw.read(t)
	// Hello frames carry their identity in Type, never in Name.
	require.Equal(t, protocol.Hello, greeting.Type, "first frame = %+v, want the hello greeting", greeting)
	return rw
}

// rawSend writes raw bytes to the worker's stdin.
func (rw *rawWorker) rawSend(t *testing.T, data []byte) {
	t.Helper()
	_, err := rw.stdin.Write(data)
	require.NoError(t, err, "write raw frame")
}

// request encodes and writes one protocol request.
func (rw *rawWorker) request(t *testing.T, id uint64, name string, payload any) {
	t.Helper()
	m, err := protocol.NewMessage(protocol.Request, name, id, payload)
	require.NoError(t, err)
	frame, err := protocol.Encode(m)
	require.NoError(t, err)
	rw.rawSend(t, frame)
}

// read consumes one frame with a deadline, failing on timeout.
func (rw *rawWorker) read(t *testing.T) protocol.Message {
	t.Helper()
	type outcome struct {
		m   protocol.Message
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		m, err := rw.reader.Read()
		done <- outcome{m, err}
	}()
	select {
	case result := <-done:
		// diagnostics is written concurrently by the child's stderr copier
		// until Wait drains it, so it may only be read on failure paths.
		if result.err != nil {
			require.NoError(t, result.err, "read frame; diagnostics: %s", rw.diagnostics.String())
		}
		return result.m
	case <-time.After(15 * time.Second):
		require.FailNow(t, fmt.Sprintf("timed out reading a frame; diagnostics: %s", rw.diagnostics.String()))
		return protocol.Message{}
	}
}

// readError consumes one frame and requires its stable error code.
func (rw *rawWorker) readError(t *testing.T, code string) {
	t.Helper()
	m := rw.read(t)
	require.Equal(t, protocol.Response, m.Type, "frame = %+v, want a response with error code %s", m, code)
	require.NotNil(t, m.Error, "frame = %+v, want a response with error code %s", m, code)
	require.Equal(t, code, m.Error.Code, "frame = %+v, want a response with error code %s", m, code)
}

// readOK consumes one response and requires it to carry no error.
func (rw *rawWorker) readOK(t *testing.T) protocol.Message {
	t.Helper()
	m := rw.read(t)
	// diagnostics is written concurrently by the child's stderr copier
	// until Wait drains it, so it may only be read on failure paths.
	if m.Type != protocol.Response || m.Error != nil {
		require.Equal(t, protocol.Response, m.Type, "frame = %+v, want an error-free response; diagnostics: %s", m, rw.diagnostics.String())
		require.Nil(t, m.Error, "frame = %+v, want an error-free response; diagnostics: %s", m, rw.diagnostics.String())
	}
	return m
}

// requireExit waits for the process to exit and requires the exit code,
// failing on a timeout instead of leaving the process behind.
func (rw *rawWorker) requireExit(t *testing.T, want int, bound time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- rw.cmd.Wait() }()
	select {
	case err := <-done:
		if want == 0 && err == nil {
			return
		}
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit, "exit = %v, want code %d; diagnostics: %s", err, want, rw.diagnostics.String())
		require.Equal(t, want, exit.ExitCode(), "exit = %v, want code %d; diagnostics: %s", err, want, rw.diagnostics.String())
	case <-time.After(bound):
		_ = rw.cmd.Process.Kill()
		<-done
		require.FailNow(t, fmt.Sprintf("worker did not exit within %s; diagnostics: %s", bound, rw.diagnostics.String()))
	}
}

// readResponseNamed consumes frames until one response with the wanted name
// arrives, skipping interleaved events. Responses may legitimately overtake
// each other: ping is answered on the read side so it never queues behind a
// stuck native call, and the terminal diagnostics event is enqueued between
// the last command's work and the close response.
func (rw *rawWorker) readResponseNamed(t *testing.T, name string) protocol.Message {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		m := rw.readDeadline(t, deadline)
		if m.Type == protocol.Response && m.Name == name {
			return m
		}
		require.Equal(t, protocol.Event, m.Type, "frame = %+v, want the %s response", m, name)
	}
}

// readDeadline consumes one frame with an externally owned deadline.
func (rw *rawWorker) readDeadline(t *testing.T, deadline <-chan time.Time) protocol.Message {
	t.Helper()
	type outcome struct {
		m   protocol.Message
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		m, err := rw.reader.Read()
		done <- outcome{m, err}
	}()
	select {
	case result := <-done:
		// diagnostics is written concurrently by the child's stderr copier
		// until Wait drains it, so it may only be read on failure paths.
		if result.err != nil {
			require.NoError(t, result.err, "read frame; diagnostics: %s", rw.diagnostics.String())
		}
		return result.m
	case <-deadline:
		require.FailNow(t, fmt.Sprintf("timed out reading a frame; diagnostics: %s", rw.diagnostics.String()))
		return protocol.Message{}
	}
}

// initializeRaw performs the raw handshake into the controlled native library.
func initializeRaw(t *testing.T, rw *rawWorker, library string, id uint64) {
	t.Helper()
	rw.request(t, id, protocol.Initialize, protocol.InitializeRequest{SnapshotID: "raw-matrix", LibraryPath: library, UserDir: t.TempDir(), ResourceRoots: []string{t.TempDir()}})
	m := rw.readOK(t)
	var info protocol.RuntimeInfo
	require.NoError(t, protocol.Decode(m.Payload, &info))
}

// TestWorkerRawProtocolMatrix proves the worker's own wire discipline over a
// real process.
func TestWorkerRawProtocolMatrix(t *testing.T) {
	library := buildAsyncFixture(t)

	t.Run("malformed payload answers invalid_input and survives", func(t *testing.T) {
		rw := launchRawWorker(t)
		// The frame itself stays decodable; only the payload schema breaks.
		rw.rawSend(t, []byte(`{"version":1,"type":"request","id":1,"name":"initialize","payload":{"snapshot_id":7,"resource_roots":"not-a-list"}}`+"\n"))
		rw.readError(t, protocol.InvalidInput)
		rw.request(t, 2, protocol.Ping, nil)
		rw.readOK(t)
		rw.request(t, 3, protocol.Close, nil)
		rw.readOK(t)
		rw.requireExit(t, 0, 10*time.Second)
	})

	t.Run("unknown command answers unsupported and survives", func(t *testing.T) {
		rw := launchRawWorker(t)
		initializeRaw(t, rw, library, 1)
		// Unknown commands answer unsupported only once the worker is
		// initialized; before that the admission gate answers invalid_state.
		rw.request(t, 2, "time_travel", struct{}{})
		rw.readError(t, protocol.Unsupported)
		rw.request(t, 3, protocol.Ping, nil)
		rw.readOK(t)
		rw.request(t, 4, protocol.Close, nil)
		rw.readResponseNamed(t, protocol.Close)
		rw.requireExit(t, 0, 10*time.Second)
	})

	t.Run("malicious image chunk requests never leave the owned snapshot", func(t *testing.T) {
		rw := launchRawWorker(t)
		initializeRaw(t, rw, library, 1)
		rw.request(t, 2, protocol.GetImageBGR, nil)
		m := rw.readOK(t)
		var transfer protocol.ImageTransfer
		require.NoError(t, protocol.Decode(m.Payload, &transfer))
		require.Equal(t, uint64(len(asyncfxBGR())), transfer.TotalBytes, "transfer: %+v", transfer)
		bad := []protocol.ImageChunkRequest{
			{TransferID: 0, Offset: 0, Length: 1},
			{TransferID: transfer.TransferID + 999, Offset: 0, Length: 1},
			{TransferID: transfer.TransferID, Offset: 0, Length: 0},
			{TransferID: transfer.TransferID, Offset: 0, Length: transfer.ChunkBytes + 1},
			{TransferID: transfer.TransferID, Offset: transfer.TotalBytes, Length: 1},
			{TransferID: transfer.TransferID, Offset: transfer.TotalBytes - 2, Length: 3},
		}
		id := uint64(3)
		for _, req := range bad {
			rw.request(t, id, protocol.ImageChunk, req)
			rw.readError(t, protocol.InvalidInput)
			id++
		}
		// A well-formed request still serves exactly the requested bytes from
		// the retained snapshot.
		rw.request(t, id, protocol.ImageChunk, protocol.ImageChunkRequest{TransferID: transfer.TransferID, Offset: 0, Length: uint32(transfer.TotalBytes)})
		chunk := rw.readOK(t)
		var response protocol.ImageChunkResponse
		require.NoError(t, protocol.Decode(chunk.Payload, &response))
		require.Equal(t, base64.StdEncoding.EncodeToString(asyncfxBGR()), response.Data, "chunk data diverged from the snapshot: %q", response.Data)
		id++
		rw.request(t, id, protocol.Ping, nil)
		rw.readOK(t)
		id++
		rw.request(t, id, protocol.Close, nil)
		// The terminal diagnostics snapshot is enqueued between the released
		// snapshot and the close response.
		closing := rw.readResponseNamed(t, protocol.Close)
		require.Nil(t, closing.Error, "close response: %+v", closing.Error)
		rw.requireExit(t, 0, 10*time.Second)
	})

	t.Run("duplicate request id terminates the session", func(t *testing.T) {
		rw := launchRawWorker(t)
		rw.request(t, 1, protocol.Ping, nil)
		rw.readOK(t)
		rw.request(t, 1, protocol.Ping, nil)
		rw.requireExit(t, 1, 10*time.Second)
	})

	t.Run("invalid utf-8 frame terminates the session", func(t *testing.T) {
		rw := launchRawWorker(t)
		rw.rawSend(t, []byte{'\xFF', '\xFE', '{', '}', '\n'})
		rw.requireExit(t, 1, 10*time.Second)
	})

	t.Run("oversized frame terminates the session", func(t *testing.T) {
		rw := launchRawWorker(t)
		rw.rawSend(t, append(bytes.Repeat([]byte{'x'}, protocol.MaxFrameBytes+1), '\n'))
		rw.requireExit(t, 1, 10*time.Second)
	})

	t.Run("unknown control field terminates the session", func(t *testing.T) {
		rw := launchRawWorker(t)
		rw.rawSend(t, []byte(`{"version":1,"type":"request","id":1,"name":"ping","payload":{},"bogus":0}`+"\n"))
		rw.requireExit(t, 1, 10*time.Second)
	})

	t.Run("duplicate control field terminates the session", func(t *testing.T) {
		rw := launchRawWorker(t)
		rw.rawSend(t, []byte(`{"version":1,"type":"request","id":1,"id":2,"name":"ping","payload":{}}`+"\n"))
		rw.requireExit(t, 1, 10*time.Second)
	})

	t.Run("protocol version mismatch terminates the session", func(t *testing.T) {
		rw := launchRawWorker(t)
		rw.rawSend(t, []byte(`{"version":2,"type":"request","id":1,"name":"ping","payload":{}}`+"\n"))
		rw.requireExit(t, 1, 10*time.Second)
	})

	t.Run("close after an instance fault reports the stable code and exits 1", func(t *testing.T) {
		rw := launchRawWorker(t)
		initializeRaw(t, rw, library, 1)
		rw.request(t, 2, protocol.Log, protocol.LogRequest{Level: "asyncfx", Message: "asyncfx-fire-unknown"})
		rw.readOK(t) // the log command's own response precedes the events
		// The unassociatable completion crosses as one Core callback and the
		// one-shot fault report as the fault event; their relative emission
		// order is not frozen, but the fault code is stable and the session
		// stays usable.
		sawFault, sawCompletion := false, false
		for !sawFault || !sawCompletion {
			m := rw.read(t)
			require.Equal(t, protocol.Event, m.Type, "frame = %+v, want an event", m)
			switch m.Name {
			case protocol.CoreEvent:
				var callback protocol.Callback
				require.NoError(t, protocol.Decode(m.Payload, &callback))
				if callback.MessageID == 4 && bytes.Contains(callback.JSON, []byte("2147483392")) {
					sawCompletion = true
				}
			case protocol.FaultEvent:
				var notice protocol.FaultNotice
				require.NoError(t, protocol.Decode(m.Payload, &notice))
				require.Equal(t, protocol.ProtocolError, notice.Error.Code, "fault code = %s, want protocol_error", notice.Error.Code)
				sawFault = true
			default:
				require.FailNow(t, fmt.Sprintf("unexpected event %s", m.Name))
			}
		}
		rw.request(t, 3, protocol.Ping, nil)
		rw.readOK(t)
		// The terminal snapshot is enqueued before the close response, and the
		// close response itself carries the classified fault as its stable
		// code — discrimination by code, never by text.
		rw.request(t, 4, protocol.Close, nil)
		diagnostics := rw.read(t)
		require.Equal(t, protocol.Event, diagnostics.Type, "frame = %+v, want the diagnostics event before the close response", diagnostics)
		require.Equal(t, protocol.DiagnosticsEvent, diagnostics.Name, "frame = %+v, want the diagnostics event before the close response", diagnostics)
		var snap protocol.InstanceDiagnostics
		require.NoError(t, protocol.Decode(diagnostics.Payload, &snap))
		require.NotNil(t, snap.FirstFault, "diagnostics: %+v", snap)
		require.Equal(t, protocol.ProtocolError, snap.FirstFault.Code, "diagnostics: %+v", snap)
		require.Zero(t, snap.LostEvents, "diagnostics: %+v", snap)
		closing := rw.read(t)
		require.Equal(t, protocol.Response, closing.Type, "close response = %+v, want error code protocol_error", closing)
		require.Equal(t, protocol.Close, closing.Name, "close response = %+v, want error code protocol_error", closing)
		require.NotNil(t, closing.Error, "close response = %+v, want error code protocol_error", closing)
		require.Equal(t, protocol.ProtocolError, closing.Error.Code, "close response = %+v, want error code protocol_error", closing)
		require.NoError(t, rw.stdin.Close())
		rw.requireExit(t, 1, 10*time.Second)
	})

	t.Run("parent eof without close exits bounded", func(t *testing.T) {
		rw := launchRawWorker(t)
		// A truncated frame followed by an abrupt stdin close simulates the
		// parent dying mid-frame: the worker must fail loudly and exit within
		// its own grace, without waiting for anything.
		rw.rawSend(t, []byte(`{"version":1,"type":"request"`))
		require.NoError(t, rw.stdin.Close())
		rw.requireExit(t, 1, 10*time.Second)
	})
}
