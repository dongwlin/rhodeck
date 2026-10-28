package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// imageSnapshot is one owned copy of the cached image, retained so that
// image_chunk requests can be served from an immutable snapshot: later cache
// updates never change a transfer that is already in flight. The command-loop
// goroutine creates, serves and releases every snapshot, so the registry
// needs no lock of its own.
type imageSnapshot struct {
	data   []byte
	served uint64
}

// admitImageSnapshot gates and registers one new image transfer read by the
// engine hook. The concurrency budget is rejected before any byte is read;
// the per-transfer byte budget is enforced after the read, before the
// snapshot is retained — the applied limit set was already rejected at
// initialize when it exceeded the wire budgets, so this post-read check is
// the last gate, never the only one. The response describes the snapshot
// exactly (total length, whole-payload SHA256 and the fixed chunk length);
// a zero-length image is a complete transfer at acceptance and consumes a
// transfer ID without retaining a snapshot.
func (s *session) admitImageSnapshot(read func() ([]byte, error)) (protocol.ImageTransfer, error) {
	if err := protocol.AdmitImageTransfer(0, len(s.transfers)); err != nil {
		return protocol.ImageTransfer{}, err
	}
	data, err := read()
	if err != nil {
		return protocol.ImageTransfer{}, err
	}
	if err := protocol.AdmitImageTransfer(uint64(len(data)), len(s.transfers)); err != nil {
		return protocol.ImageTransfer{}, err
	}
	if s.nextTransferID == math.MaxUint64 {
		err := errors.New("worker: image transfer IDs exhausted")
		s.fail(err)
		return protocol.ImageTransfer{}, err
	}
	s.nextTransferID++
	digest := sha256.Sum256(data)
	transfer := protocol.ImageTransfer{
		TransferID: s.nextTransferID,
		TotalBytes: uint64(len(data)),
		SHA256:     hex.EncodeToString(digest[:]),
		ChunkBytes: protocol.ImageChunkBytes,
	}
	if transfer.TotalBytes > 0 {
		s.transfers[transfer.TransferID] = &imageSnapshot{data: data}
	}
	return transfer, nil
}

// serveImageChunk answers one chunk request from a retained snapshot. The
// request must reference a live transfer and stay within its declared range;
// anything else is invalid_input, never a fabricated chunk. Under the frozen
// client contract every byte range is fetched once, so the served byte count
// reaches the total exactly when coverage is complete and the snapshot is
// released at that moment; a duplicate fetch after release is invalid_input
// instead of silently serving from freed state.
func (s *session) serveImageChunk(req *protocol.ImageChunkRequest) (protocol.ImageChunkResponse, error) {
	snapshot, ok := s.transfers[req.TransferID]
	if !ok {
		return protocol.ImageChunkResponse{}, wireError(protocol.InvalidInput, fmt.Sprintf("image transfer %d is unknown or already fully served", req.TransferID))
	}
	transfer := protocol.ImageTransfer{TransferID: req.TransferID, TotalBytes: uint64(len(snapshot.data)), ChunkBytes: protocol.ImageChunkBytes}
	if err := req.Validate(&transfer); err != nil {
		return protocol.ImageChunkResponse{}, wireError(protocol.InvalidInput, err.Error())
	}
	chunk := snapshot.data[req.Offset : uint64(req.Offset)+uint64(req.Length)]
	response := protocol.ImageChunkResponse{TransferID: req.TransferID, Offset: req.Offset, Data: base64.StdEncoding.EncodeToString(chunk)}
	snapshot.served += uint64(req.Length)
	if snapshot.served >= uint64(len(snapshot.data)) {
		snapshot.data = nil
		delete(s.transfers, req.TransferID)
	}
	return response, nil
}

// releaseImageTransfers drops every retained snapshot. The command loop calls
// it from its cleanup, so a close and every session-failure path free the
// aggregate in-flight image memory before the process exits.
func (s *session) releaseImageTransfers() {
	for id, snapshot := range s.transfers {
		snapshot.data = nil
		delete(s.transfers, id)
	}
}
