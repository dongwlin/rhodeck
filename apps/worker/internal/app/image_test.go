package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	maa "github.com/dongwlin/rhodeck/packages/maa-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// decodeImageTransfer reads one get_image_* acceptance response.
func decodeImageTransfer(t *testing.T, m protocol.Message) protocol.ImageTransfer {
	t.Helper()
	var transfer protocol.ImageTransfer
	require.NoError(t, protocol.Decode(m.Payload, &transfer))
	return transfer
}

// fetchSnapshot mimics the frozen client pull: sequential chunks, each within
// the declared chunk length, assembling and verifying the whole payload.
func fetchSnapshot(t *testing.T, h *sessionHarness, transfer protocol.ImageTransfer) []byte {
	t.Helper()
	var assembled []byte
	for offset := uint64(0); offset < transfer.TotalBytes; {
		length := uint64(transfer.ChunkBytes)
		if remaining := transfer.TotalBytes - offset; remaining < length {
			length = remaining
		}
		m := h.call(protocol.ImageChunk, protocol.ImageChunkRequest{TransferID: transfer.TransferID, Offset: offset, Length: uint32(length)})
		require.Nil(t, m.Error, "unexpected chunk error: %v", m.Error)
		var chunk protocol.ImageChunkResponse
		require.NoError(t, protocol.Decode(m.Payload, &chunk))
		data, err := base64.StdEncoding.DecodeString(chunk.Data)
		require.NoError(t, err, "chunk mismatch at offset %d: %+v", offset, chunk)
		require.Equal(t, transfer.TransferID, chunk.TransferID, "chunk mismatch at offset %d: %+v", offset, chunk)
		require.Equal(t, offset, chunk.Offset, "chunk mismatch at offset %d: %+v", offset, chunk)
		require.Equal(t, length, uint64(len(data)), "chunk mismatch at offset %d: %+v (%d bytes)", offset, chunk, len(data))
		assembled = append(assembled, data...)
		offset += length
	}
	return assembled
}

// TestSessionImageTransferServesBoundedChunks proves the frozen transfer
// contract on the worker side: the acceptance describes one owned snapshot
// exactly, chunks are served with backpressure-friendly ranges from that
// snapshot, the assembled payload matches byte for byte, the last served byte
// releases the snapshot, and chunk traffic never enters the event stream.
func TestSessionImageTransferServesBoundedChunks(t *testing.T) {
	f := newFakeEngine()
	payload := bytes.Repeat([]byte{0xA5, 0x5A, 0x00, 0xFF}, 8) // 32 bytes, binary-safe
	f.imagePNG = func() ([]byte, error) { return append([]byte(nil), payload...), nil }
	h := harness(t, fakeFactory(f))
	h.initialize()
	m := h.call(protocol.GetImagePNG, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	transfer := decodeImageTransfer(t, m)
	require.NotZero(t, transfer.TransferID, "transfer description: %+v", transfer)
	require.Equal(t, uint64(len(payload)), transfer.TotalBytes, "transfer description: %+v", transfer)
	require.Equal(t, uint32(protocol.ImageChunkBytes), transfer.ChunkBytes, "transfer description: %+v", transfer)
	require.Equal(t, sha256Hex(payload), transfer.SHA256, "transfer digest %s, want %s", transfer.SHA256, sha256Hex(payload))
	assembled := fetchSnapshot(t, h, transfer)
	require.True(t, bytes.Equal(assembled, payload), "assembled payload diverged: %d bytes", len(assembled))
	// Chunk responses are responses, not events: nothing entered the event
	// sequence domain.
	require.Empty(t, h.events, "chunk traffic produced events: %+v", h.events)
	// The last served byte released the snapshot: a duplicate fetch is
	// invalid_input, and the freed slot admits a new transfer.
	m = h.call(protocol.ImageChunk, protocol.ImageChunkRequest{TransferID: transfer.TransferID, Offset: 0, Length: 1})
	require.NotNil(t, m.Error, "fetch after release: %+v", m)
	require.Equal(t, protocol.InvalidInput, m.Error.Code, "fetch after release: %+v", m)
	f.imagePNG = func() ([]byte, error) { return []byte{1}, nil }
	m = h.call(protocol.GetImagePNG, nil)
	require.Nil(t, m.Error, "freed slot must admit a new transfer: %+v", m)
	m = h.call(protocol.Ping, nil)
	require.Nil(t, m.Error, "session unusable after transfers: %+v", m)
	m = h.call(protocol.Close, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	require.NoError(t, <-h.done)
}

// TestSessionImageTransferAdmissionAndFailures proves the budget gates and
// failure classification: the concurrency budget is rejected with
// limit_exceeded before any byte is read, a snapshot beyond the frozen byte
// budget never becomes a transfer, read failures keep their stable codes
// (buffer exhaustion vs. the optional BGR symbol), a failed read releases its
// slot, and image reads never submit a screencap.
func TestSessionImageTransferAdmissionAndFailures(t *testing.T) {
	t.Run("concurrency budget is rejected before the read", func(t *testing.T) {
		f := newFakeEngine()
		var reads atomic.Int32
		blob := bytes.Repeat([]byte{7}, 16)
		f.imagePNG = func() ([]byte, error) {
			reads.Add(1)
			return append([]byte(nil), blob...), nil
		}
		h := harness(t, fakeFactory(f))
		h.initialize()
		for range protocol.ImageTransfersMax {
			m := h.call(protocol.GetImagePNG, nil)
			require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		}
		m := h.call(protocol.GetImagePNG, nil)
		require.NotNil(t, m.Error, "transfer beyond the concurrency budget: %+v", m)
		require.Equal(t, protocol.LimitExceeded, m.Error.Code, "transfer beyond the concurrency budget: %+v", m)
		require.NotNil(t, m.Error.Detail, "effective maximum not reported: %+v", m.Error.Detail)
		require.Equal(t, "transfers", m.Error.Detail.Unit, "effective maximum not reported: %+v", m.Error.Detail)
		require.Equal(t, uint64(protocol.ImageTransfersMax), m.Error.Detail.Maximum, "effective maximum not reported: %+v", m.Error.Detail)
		require.Equal(t, int32(protocol.ImageTransfersMax), reads.Load(), "rejected request must not read image bytes: %d reads", reads.Load())
		m = h.call(protocol.Ping, nil)
		require.Nil(t, m.Error, "session unusable after the rejection: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("read failures classify and release the slot", func(t *testing.T) {
		f := newFakeEngine()
		f.imagePNG = func() ([]byte, error) { return nil, maa.ErrBufferLimit }
		f.imageBGR = func() ([]byte, error) { return nil, maa.ErrUnsupported }
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.GetImagePNG, nil)
		require.NotNil(t, m.Error, "buffer exhaustion must stay limit_exceeded: %+v", m)
		require.Equal(t, protocol.LimitExceeded, m.Error.Code, "buffer exhaustion must stay limit_exceeded: %+v", m)
		m = h.call(protocol.GetImageBGR, nil)
		require.NotNil(t, m.Error, "missing optional BGR symbol must stay unsupported: %+v", m)
		require.Equal(t, protocol.Unsupported, m.Error.Code, "missing optional BGR symbol must stay unsupported: %+v", m)
		// Failed reads retained nothing: the default empty read is admitted.
		f.imagePNG, f.imageBGR = nil, nil
		for _, name := range []string{protocol.GetImagePNG, protocol.GetImageBGR} {
			m := h.call(name, nil)
			require.Nil(t, m.Error, "%s after failed reads: %+v", name, m)
			transfer := decodeImageTransfer(t, m)
			require.Zero(t, transfer.TotalBytes, "empty image must be a complete transfer: %+v", transfer)
			require.Equal(t, sha256Hex(nil), transfer.SHA256, "empty image must be a complete transfer: %+v", transfer)
		}
		m = h.call(protocol.Ping, nil)
		require.Nil(t, m.Error, "session damaged by read failures: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("snapshot beyond the byte budget is the last gate", func(t *testing.T) {
		f := newFakeEngine()
		f.imagePNG = func() ([]byte, error) { return make([]byte, protocol.ImageTransferMaxBytes+1), nil }
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.GetImagePNG, nil)
		require.NotNil(t, m.Error, "oversized snapshot: %+v", m)
		require.Equal(t, protocol.LimitExceeded, m.Error.Code, "oversized snapshot: %+v", m)
		require.NotNil(t, m.Error.Detail, "effective byte maximum not reported: %+v", m.Error.Detail)
		require.Equal(t, "bytes", m.Error.Detail.Unit, "effective byte maximum not reported: %+v", m.Error.Detail)
		require.Equal(t, uint64(protocol.ImageTransferMaxBytes), m.Error.Detail.Maximum, "effective byte maximum not reported: %+v", m.Error.Detail)
		// Nothing was retained: an ordinary read is admitted afterwards.
		f.imagePNG = func() ([]byte, error) { return []byte{2}, nil }
		m = h.call(protocol.GetImagePNG, nil)
		require.Nil(t, m.Error, "rejected snapshot must not hold a slot: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("reads never screencap", func(t *testing.T) {
		f := newFakeEngine()
		f.imagePNG = func() ([]byte, error) { return []byte{3}, nil }
		f.imageBGR = func() ([]byte, error) { return []byte{4}, nil }
		f.screencapHook = func() (operation, error) {
			assert.Fail(t, "image reads must not submit an implicit screencap")
			return nil, errors.New("unreachable")
		}
		h := harness(t, fakeFactory(f))
		h.initialize()
		for _, name := range []string{protocol.GetImagePNG, protocol.GetImageBGR} {
			m := h.call(name, nil)
			require.Nil(t, m.Error, "%s: %+v", name, m)
		}
		m := h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})
}

// TestSessionImageChunkRequestsAreValidated proves every malformed chunk
// request is invalid_input while the session and the snapshot stay usable:
// unknown or wrong transfer ownership, zero or oversized lengths, ranges past
// the transfer end, and a missing payload. A well-formed request still serves
// exactly the requested bytes afterwards.
func TestSessionImageChunkRequestsAreValidated(t *testing.T) {
	f := newFakeEngine()
	payload := []byte("payload8")
	f.imagePNG = func() ([]byte, error) { return append([]byte(nil), payload...), nil }
	h := harness(t, fakeFactory(f))
	h.initialize()
	m := h.call(protocol.GetImagePNG, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	transfer := decodeImageTransfer(t, m)
	bad := []protocol.ImageChunkRequest{
		{TransferID: 0, Offset: 0, Length: 1},
		{TransferID: transfer.TransferID + 1000, Offset: 0, Length: 1},
		{TransferID: transfer.TransferID, Offset: 0, Length: 0},
		{TransferID: transfer.TransferID, Offset: 0, Length: transfer.ChunkBytes + 1},
		{TransferID: transfer.TransferID, Offset: transfer.TotalBytes, Length: 1},
		{TransferID: transfer.TransferID, Offset: transfer.TotalBytes - 2, Length: 3},
	}
	for _, req := range bad {
		m := h.call(protocol.ImageChunk, req)
		require.NotNil(t, m.Error, "malformed chunk %+v: %+v", req, m)
		require.Equal(t, protocol.InvalidInput, m.Error.Code, "malformed chunk %+v: %+v", req, m)
	}
	m = h.call(protocol.ImageChunk, nil)
	require.NotNil(t, m.Error, "missing payload: %+v", m)
	require.Equal(t, protocol.InvalidInput, m.Error.Code, "missing payload: %+v", m)
	data := fetchSnapshot(t, h, transfer)
	require.True(t, bytes.Equal(data, payload), "valid chunk after rejections diverged: %q", data)
	m = h.call(protocol.Ping, nil)
	require.Nil(t, m.Error, "session unusable after rejections: %+v", m)
	m = h.call(protocol.Close, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	require.NoError(t, <-h.done)
}

// TestSessionImageTransferReleaseAndClose proves the empty-image and release
// contracts: a zero-length image is a complete transfer that occupies no
// slot, the release helper drops retained snapshots, and both a clean close
// and a failing session finish with snapshots retained — resources never
// leak past the command loop.
func TestSessionImageTransferReleaseAndClose(t *testing.T) {
	t.Run("empty image occupies no slot", func(t *testing.T) {
		f := newFakeEngine()
		f.imageBGR = func() ([]byte, error) { return []byte{}, nil }
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.GetImageBGR, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		transfer := decodeImageTransfer(t, m)
		require.Zero(t, transfer.TotalBytes, "empty transfer: %+v", transfer)
		require.Equal(t, sha256Hex(nil), transfer.SHA256, "empty transfer: %+v", transfer)
		require.NotZero(t, transfer.TransferID, "empty transfer: %+v", transfer)
		// Nothing to fetch: a chunk request against the empty transfer is
		// invalid_input, and the two concurrent slots are still free.
		m = h.call(protocol.ImageChunk, protocol.ImageChunkRequest{TransferID: transfer.TransferID, Offset: 0, Length: 1})
		require.NotNil(t, m.Error, "chunk against an empty transfer: %+v", m)
		require.Equal(t, protocol.InvalidInput, m.Error.Code, "chunk against an empty transfer: %+v", m)
		f.imagePNG = func() ([]byte, error) { return bytes.Repeat([]byte{9}, 16), nil }
		for range protocol.ImageTransfersMax {
			m := h.call(protocol.GetImagePNG, nil)
			require.Nil(t, m.Error, "empty transfer must not hold a slot: %+v", m)
		}
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("release helper drops retained snapshots", func(t *testing.T) {
		s := &session{transfers: map[uint64]*imageSnapshot{3: {data: []byte("snapshot")}}}
		s.releaseImageTransfers()
		require.Empty(t, s.transfers, "release left %d snapshots behind", len(s.transfers))
	})

	t.Run("close with retained snapshots exits cleanly", func(t *testing.T) {
		f := newFakeEngine()
		f.imagePNG = func() ([]byte, error) { return bytes.Repeat([]byte{1}, 32), nil }
		h := harness(t, fakeFactory(f))
		h.initialize()
		for range protocol.ImageTransfersMax {
			m := h.call(protocol.GetImagePNG, nil)
			require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		}
		m := h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("failing session with retained snapshots still exits", func(t *testing.T) {
		f := newFakeEngine()
		f.imagePNG = func() ([]byte, error) { return bytes.Repeat([]byte{2}, 32), nil }
		h := harness(t, fakeFactory(f))
		h.initialize()
		for range protocol.ImageTransfersMax {
			m := h.call(protocol.GetImagePNG, nil)
			require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		}
		for i := range 128 {
			f.inst.sendEvent(maa.Event{MessageID: 1, Sequence: uint64(i + 1), JSON: []byte(`{}`)})
		}
		require.ErrorIs(t, <-h.done, protocol.ErrQueueFull, "session failure with retained snapshots")
	})
}
