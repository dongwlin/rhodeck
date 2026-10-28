package main

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
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// decodeImageTransfer reads one get_image_* acceptance response.
func decodeImageTransfer(t *testing.T, m protocol.Message) protocol.ImageTransfer {
	t.Helper()
	var transfer protocol.ImageTransfer
	if err := protocol.Decode(m.Payload, &transfer); err != nil {
		t.Fatal(err)
	}
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
		if m.Error != nil {
			t.Fatal(m.Error)
		}
		var chunk protocol.ImageChunkResponse
		if err := protocol.Decode(m.Payload, &chunk); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil || chunk.TransferID != transfer.TransferID || chunk.Offset != offset || uint64(len(data)) != length {
			t.Fatalf("chunk mismatch at offset %d: %+v (%d bytes, %v)", offset, chunk, len(data), err)
		}
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
	if m.Error != nil {
		t.Fatal(m.Error)
	}
	transfer := decodeImageTransfer(t, m)
	if transfer.TransferID == 0 || transfer.TotalBytes != uint64(len(payload)) || transfer.ChunkBytes != protocol.ImageChunkBytes {
		t.Fatalf("transfer description: %+v", transfer)
	}
	if transfer.SHA256 != sha256Hex(payload) {
		t.Fatalf("transfer digest %s, want %s", transfer.SHA256, sha256Hex(payload))
	}
	assembled := fetchSnapshot(t, h, transfer)
	if !bytes.Equal(assembled, payload) {
		t.Fatalf("assembled payload diverged: %d bytes", len(assembled))
	}
	// Chunk responses are responses, not events: nothing entered the event
	// sequence domain.
	if len(h.events) != 0 {
		t.Fatalf("chunk traffic produced events: %+v", h.events)
	}
	// The last served byte released the snapshot: a duplicate fetch is
	// invalid_input, and the freed slot admits a new transfer.
	if m := h.call(protocol.ImageChunk, protocol.ImageChunkRequest{TransferID: transfer.TransferID, Offset: 0, Length: 1}); m.Error == nil || m.Error.Code != protocol.InvalidInput {
		t.Fatalf("fetch after release: %+v", m)
	}
	f.imagePNG = func() ([]byte, error) { return []byte{1}, nil }
	if m := h.call(protocol.GetImagePNG, nil); m.Error != nil {
		t.Fatalf("freed slot must admit a new transfer: %+v", m)
	}
	if m := h.call(protocol.Ping, nil); m.Error != nil {
		t.Fatalf("session unusable after transfers: %+v", m)
	}
	if m := h.call(protocol.Close, nil); m.Error != nil {
		t.Fatal(m.Error)
	}
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
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
			if m := h.call(protocol.GetImagePNG, nil); m.Error != nil {
				t.Fatal(m.Error)
			}
		}
		m := h.call(protocol.GetImagePNG, nil)
		if m.Error == nil || m.Error.Code != protocol.LimitExceeded {
			t.Fatalf("transfer beyond the concurrency budget: %+v", m)
		}
		if m.Error.Detail == nil || m.Error.Detail.Unit != "transfers" || m.Error.Detail.Maximum != protocol.ImageTransfersMax {
			t.Fatalf("effective maximum not reported: %+v", m.Error.Detail)
		}
		if reads.Load() != int32(protocol.ImageTransfersMax) {
			t.Fatalf("rejected request must not read image bytes: %d reads", reads.Load())
		}
		if m := h.call(protocol.Ping, nil); m.Error != nil {
			t.Fatalf("session unusable after the rejection: %+v", m)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("read failures classify and release the slot", func(t *testing.T) {
		f := newFakeEngine()
		f.imagePNG = func() ([]byte, error) { return nil, maa.ErrBufferLimit }
		f.imageBGR = func() ([]byte, error) { return nil, maa.ErrUnsupported }
		h := harness(t, fakeFactory(f))
		h.initialize()
		if m := h.call(protocol.GetImagePNG, nil); m.Error == nil || m.Error.Code != protocol.LimitExceeded {
			t.Fatalf("buffer exhaustion must stay limit_exceeded: %+v", m)
		}
		if m := h.call(protocol.GetImageBGR, nil); m.Error == nil || m.Error.Code != protocol.Unsupported {
			t.Fatalf("missing optional BGR symbol must stay unsupported: %+v", m)
		}
		// Failed reads retained nothing: the default empty read is admitted.
		f.imagePNG, f.imageBGR = nil, nil
		for _, name := range []string{protocol.GetImagePNG, protocol.GetImageBGR} {
			m := h.call(name, nil)
			if m.Error != nil {
				t.Fatalf("%s after failed reads: %+v", name, m)
			}
			transfer := decodeImageTransfer(t, m)
			if transfer.TotalBytes != 0 || transfer.SHA256 != sha256Hex(nil) {
				t.Fatalf("empty image must be a complete transfer: %+v", transfer)
			}
		}
		if m := h.call(protocol.Ping, nil); m.Error != nil {
			t.Fatalf("session damaged by read failures: %+v", m)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("snapshot beyond the byte budget is the last gate", func(t *testing.T) {
		f := newFakeEngine()
		f.imagePNG = func() ([]byte, error) { return make([]byte, protocol.ImageTransferMaxBytes+1), nil }
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.GetImagePNG, nil)
		if m.Error == nil || m.Error.Code != protocol.LimitExceeded {
			t.Fatalf("oversized snapshot: %+v", m)
		}
		if m.Error.Detail == nil || m.Error.Detail.Unit != "bytes" || m.Error.Detail.Maximum != protocol.ImageTransferMaxBytes {
			t.Fatalf("effective byte maximum not reported: %+v", m.Error.Detail)
		}
		// Nothing was retained: an ordinary read is admitted afterwards.
		f.imagePNG = func() ([]byte, error) { return []byte{2}, nil }
		if m := h.call(protocol.GetImagePNG, nil); m.Error != nil {
			t.Fatalf("rejected snapshot must not hold a slot: %+v", m)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("reads never screencap", func(t *testing.T) {
		f := newFakeEngine()
		f.imagePNG = func() ([]byte, error) { return []byte{3}, nil }
		f.imageBGR = func() ([]byte, error) { return []byte{4}, nil }
		f.screencapHook = func() (operation, error) {
			t.Error("image reads must not submit an implicit screencap")
			return nil, errors.New("unreachable")
		}
		h := harness(t, fakeFactory(f))
		h.initialize()
		for _, name := range []string{protocol.GetImagePNG, protocol.GetImageBGR} {
			if m := h.call(name, nil); m.Error != nil {
				t.Fatalf("%s: %+v", name, m)
			}
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
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
	if m.Error != nil {
		t.Fatal(m.Error)
	}
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
		if m := h.call(protocol.ImageChunk, req); m.Error == nil || m.Error.Code != protocol.InvalidInput {
			t.Fatalf("malformed chunk %+v: %+v", req, m)
		}
	}
	if m := h.call(protocol.ImageChunk, nil); m.Error == nil || m.Error.Code != protocol.InvalidInput {
		t.Fatalf("missing payload: %+v", m)
	}
	data := fetchSnapshot(t, h, transfer)
	if !bytes.Equal(data, payload) {
		t.Fatalf("valid chunk after rejections diverged: %q", data)
	}
	if m := h.call(protocol.Ping, nil); m.Error != nil {
		t.Fatalf("session unusable after rejections: %+v", m)
	}
	if m := h.call(protocol.Close, nil); m.Error != nil {
		t.Fatal(m.Error)
	}
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
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
		if m.Error != nil {
			t.Fatal(m.Error)
		}
		transfer := decodeImageTransfer(t, m)
		if transfer.TotalBytes != 0 || transfer.SHA256 != sha256Hex(nil) || transfer.TransferID == 0 {
			t.Fatalf("empty transfer: %+v", transfer)
		}
		// Nothing to fetch: a chunk request against the empty transfer is
		// invalid_input, and the two concurrent slots are still free.
		if m := h.call(protocol.ImageChunk, protocol.ImageChunkRequest{TransferID: transfer.TransferID, Offset: 0, Length: 1}); m.Error == nil || m.Error.Code != protocol.InvalidInput {
			t.Fatalf("chunk against an empty transfer: %+v", m)
		}
		f.imagePNG = func() ([]byte, error) { return bytes.Repeat([]byte{9}, 16), nil }
		for range protocol.ImageTransfersMax {
			if m := h.call(protocol.GetImagePNG, nil); m.Error != nil {
				t.Fatalf("empty transfer must not hold a slot: %+v", m)
			}
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("release helper drops retained snapshots", func(t *testing.T) {
		s := &session{transfers: map[uint64]*imageSnapshot{3: {data: []byte("snapshot")}}}
		s.releaseImageTransfers()
		if len(s.transfers) != 0 {
			t.Fatalf("release left %d snapshots behind", len(s.transfers))
		}
	})

	t.Run("close with retained snapshots exits cleanly", func(t *testing.T) {
		f := newFakeEngine()
		f.imagePNG = func() ([]byte, error) { return bytes.Repeat([]byte{1}, 32), nil }
		h := harness(t, fakeFactory(f))
		h.initialize()
		for range protocol.ImageTransfersMax {
			if m := h.call(protocol.GetImagePNG, nil); m.Error != nil {
				t.Fatal(m.Error)
			}
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("failing session with retained snapshots still exits", func(t *testing.T) {
		f := newFakeEngine()
		f.imagePNG = func() ([]byte, error) { return bytes.Repeat([]byte{2}, 32), nil }
		h := harness(t, fakeFactory(f))
		h.initialize()
		for range protocol.ImageTransfersMax {
			if m := h.call(protocol.GetImagePNG, nil); m.Error != nil {
				t.Fatal(m.Error)
			}
		}
		for i := range 128 {
			f.events <- protocol.Callback{MessageID: 1, CoreSequence: uint64(i + 1), JSON: []byte(`{}`)}
		}
		if err := <-h.done; !errors.Is(err, protocol.ErrQueueFull) {
			t.Fatalf("session failure with retained snapshots: %v", err)
		}
	})
}
