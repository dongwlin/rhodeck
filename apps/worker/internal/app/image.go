package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

type imageSnapshot struct {
	data   []byte
	served uint64
}

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

func (s *session) releaseImageTransfers() {
	for id, snapshot := range s.transfers {
		snapshot.data = nil
		delete(s.transfers, id)
	}
}
