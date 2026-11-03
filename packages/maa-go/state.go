package maa

import (
	"bytes"
	"fmt"
	"math"
	"sync"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

const faultRawPrefixBytes = 4096

type instancePhase uint8

const (
	phaseInitializing instancePhase = iota
	phaseReady
	phaseClosing
	phaseClosed
	phaseFaulted
)

type instanceState struct {
	mu sync.Mutex

	callsDrained     *sync.Cond
	callbacksDrained *sync.Cond

	phase  instancePhase
	handle uintptr
	calls  uint64

	callbackGateClosed bool
	callbacks          uint64

	pending           map[AsyncID]*operation
	pendingOperations uint64
	pendingReserved   uint64

	asyncWatermark AsyncID

	earlyWindowOpen bool
	earlyResult     *completionInfo
	earlyBytes      uint64

	firstFault        error
	firstFaultPrefix  []byte
	lostEvents        uint64
	faultDone         chan struct{}
	faultNotified     bool
	stopDiagnostic    error
	finalErr          error
	finalErrPublished bool
	closeDone         chan struct{}

	raw rawGate
}

func newInstanceState(handle uintptr, pendingOperations uint64, earlyBytes uint64) *instanceState {
	s := &instanceState{
		phase:             phaseInitializing,
		handle:            handle,
		pending:           make(map[AsyncID]*operation),
		pendingOperations: pendingOperations,
		earlyBytes:        earlyBytes,
		closeDone:         make(chan struct{}),
		faultDone:         make(chan struct{}),
	}
	s.callsDrained = sync.NewCond(&s.mu)
	s.callbacksDrained = sync.NewCond(&s.mu)
	return s
}

func (s *instanceState) publishReady() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseInitializing {
		panic(fmt.Sprintf("maa: instance state publish from phase %d", s.phase))
	}
	s.phase = phaseReady
}

func (s *instanceState) publishHandle(handle uintptr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if handle == 0 {
		panic("maa: instance handle published as 0")
	}
	if s.handle != 0 {
		panic(fmt.Sprintf("maa: instance handle %#x published over existing handle %#x", handle, s.handle))
	}
	switch s.phase {
	case phaseInitializing, phaseFaulted:
		s.handle = handle
	default:
		panic(fmt.Sprintf("maa: instance handle published in phase %d", s.phase))
	}
}

func (s *instanceState) tryPublishReady() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseInitializing:
		s.phase = phaseReady
		return nil
	case phaseFaulted:
		if s.firstFault == nil {
			panic("maa: faulted instance state without a recorded first fault")
		}
		return s.firstFault
	default:
		panic(fmt.Sprintf("maa: instance state tries to publish from phase %d", s.phase))
	}
}

type callLease struct {
	s *instanceState
}

func (s *instanceState) admitCall(what string) (callLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseReady:
		s.calls++
		return callLease{s: s}, nil
	case phaseClosing:
		return callLease{}, fmt.Errorf("maa: instance is closing, %s rejected: %w", what, native.ErrClosing)
	case phaseFaulted:
		if s.firstFault == nil {
			panic("maa: faulted instance state without a recorded first fault")
		}
		return callLease{}, fmt.Errorf("maa: %s rejected by instance fault: %w", what, s.firstFault)
	case phaseClosed:
		return callLease{}, fmt.Errorf("maa: instance is closed, %s rejected: %w", what, native.ErrClosed)
	default:
		panic(fmt.Sprintf("maa: instance state admits a call in phase %d", s.phase))
	}
}

func (l callLease) release() {
	if l.s == nil {
		panic("maa: release of a zero call lease")
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if l.s.calls == 0 {
		panic("maa: call lease released more times than admitted")
	}
	l.s.calls--
	if l.s.calls == 0 {
		l.s.callsDrained.Broadcast()
	}
}

func (l callLease) handle() uintptr {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if l.s.handle == 0 {
		panic("maa: admitted call on an instance state without a valid handle")
	}
	return l.s.handle
}

type rawGate struct {
	mu sync.Mutex
}

func (g *rawGate) enter() { g.mu.Lock() }

func (g *rawGate) exit() { g.mu.Unlock() }

func (s *instanceState) electClose() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseReady, phaseFaulted:
		s.phase = phaseClosing
		return true
	case phaseClosing, phaseClosed:
		return false
	default:
		panic(fmt.Sprintf("maa: instance state elects close in phase %d", s.phase))
	}
}

func (s *instanceState) waitCallsDrained() {
	s.mu.Lock()
	for s.calls != 0 {
		s.callsDrained.Wait()
	}
	s.mu.Unlock()
}

func (s *instanceState) closeHandle() (uintptr, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls != 0 {
		panic(fmt.Sprintf("maa: close handle read with %d calls in flight", s.calls))
	}
	return s.handle, s.handle != 0
}

func (s *instanceState) recordFault(cause error) {
	s.recordFaultWithDetail(cause, nil)
}

func (s *instanceState) recordFaultWithDetail(cause error, prefix []byte) {
	if cause == nil {
		panic("maa: instance fault recorded without a cause")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faultLocked(cause, prefix)
}

func (s *instanceState) faultLocked(cause error, prefix []byte) {
	first := false
	switch s.phase {
	case phaseInitializing, phaseReady:
		first = s.keepFirstFaultLocked(cause, prefix)
		s.phase = phaseFaulted
	case phaseFaulted, phaseClosing:
		first = s.keepFirstFaultLocked(cause, prefix)
	case phaseClosed:
		panic(fmt.Sprintf("maa: instance fault recorded in phase %d", s.phase))
	default:
		panic(fmt.Sprintf("maa: instance state in unknown phase %d", s.phase))
	}
	if first {
		for id, op := range s.pending {
			delete(s.pending, id)
			op.settleFault(cause)
		}
	}
	if !s.faultNotified {
		s.faultNotified = true
		close(s.faultDone)
	}
}

func (s *instanceState) keepFirstFaultLocked(cause error, prefix []byte) bool {
	if s.firstFault != nil {
		return false
	}
	if len(prefix) > faultRawPrefixBytes {
		prefix = prefix[:faultRawPrefixBytes]
	}
	s.firstFault = cause
	s.firstFaultPrefix = bytes.Clone(prefix)
	return true
}

func (s *instanceState) publishFinalError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseClosing {
		panic(fmt.Sprintf("maa: instance final error published in phase %d", s.phase))
	}
	if !s.finalErrPublished {
		s.finalErr = err
		s.finalErrPublished = true
	}
}

func (s *instanceState) publishClosed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseClosing {
		panic(fmt.Sprintf("maa: instance closed published in phase %d", s.phase))
	}
	if !s.callbackGateClosed {
		panic("maa: instance closed published with the callback admission gate open")
	}
	if s.callbacks != 0 {
		panic(fmt.Sprintf("maa: instance closed published with %d callbacks in flight", s.callbacks))
	}
	s.phase = phaseClosed
}

func (s *instanceState) publishCloseDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseClosed || !s.finalErrPublished {
		panic("maa: instance close completion published before the final state")
	}
	close(s.closeDone)
}

func (s *instanceState) done() <-chan struct{} {
	return s.closeDone
}

func (s *instanceState) waitClose() error {
	<-s.closeDone
	return s.finalError()
}

func (s *instanceState) finalError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalErr
}

func (s *instanceState) recordStopDiagnostic(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseClosing || s.finalErrPublished || err == nil || s.stopDiagnostic != nil {
		panic("maa: invalid instance stop diagnostic publication")
	}
	s.stopDiagnostic = err
}

func (s *instanceState) currentError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalErrPublished {
		return s.finalErr
	}
	return composeCloseError(s.firstFault, s.stopDiagnostic)
}

func (s *instanceState) currentPhase() instancePhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

func (s *instanceState) currentFault() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstFault
}

func (s *instanceState) faultNotify() <-chan struct{} {
	return s.faultDone
}

func (s *instanceState) recordLostEvents(n uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total, ok := addChecked(s.lostEvents, n)
	if !ok {
		total = math.MaxUint64
	}
	s.lostEvents = total
}

func (s *instanceState) currentLostEvents() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lostEvents
}

type faultSnapshot struct {
	lostEvents     uint64
	firstFault     error
	rawPrefix      []byte
	stopDiagnostic error
}

func (s *instanceState) faultDiagnostics() faultSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return faultSnapshot{
		lostEvents:     s.lostEvents,
		firstFault:     s.firstFault,
		rawPrefix:      bytes.Clone(s.firstFaultPrefix),
		stopDiagnostic: s.stopDiagnostic,
	}
}

type callbackLease struct {
	s *instanceState
}

func (s *instanceState) admitCallback() (callbackLease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.callbackGateClosed {
		return callbackLease{}, false
	}
	s.callbacks++
	return callbackLease{s: s}, true
}

func (l callbackLease) release() {
	if l.s == nil {
		panic("maa: release of a zero callback lease")
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if l.s.callbacks == 0 {
		panic("maa: callback lease released more times than admitted")
	}
	l.s.callbacks--
	if l.s.callbacks == 0 {
		l.s.callbacksDrained.Broadcast()
	}
}

func (s *instanceState) closeCallbackGate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.callbackGateClosed {
		panic("maa: callback admission gate closed twice")
	}
	s.callbackGateClosed = true
}

func (s *instanceState) waitCallbacksDrained() {
	s.mu.Lock()
	for s.callbacks != 0 {
		s.callbacksDrained.Wait()
	}
	s.mu.Unlock()
}

func (s *instanceState) admitPendingOperation(id AsyncID, what string) (*operation, error) {
	if id == 0 {
		panic("maa: pending operation admitted with the zero async id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseReady:
		if s.pendingOccupiedLocked() >= s.pendingOperations {
			return nil, fmt.Errorf("maa: pending operation limit %d reached, %s rejected: %w",
				s.pendingOperations, what, native.ErrOperationLimit)
		}
		op := newOperation(id)
		s.pending[id] = op
		return op, nil
	case phaseClosing:
		return nil, fmt.Errorf("maa: instance is closing, %s rejected: %w", what, native.ErrClosing)
	case phaseFaulted:
		if s.firstFault == nil {
			panic("maa: faulted instance state without a recorded first fault")
		}
		return nil, fmt.Errorf("maa: %s rejected by instance fault: %w", what, s.firstFault)
	case phaseClosed:
		return nil, fmt.Errorf("maa: instance is closed, %s rejected: %w", what, native.ErrClosed)
	default:
		panic(fmt.Sprintf("maa: instance state admits a pending operation in phase %d", s.phase))
	}
}

func (s *instanceState) pendingOccupiedLocked() uint64 {
	occ, ok := addChecked(uint64(len(s.pending)), s.pendingReserved)
	if !ok {
		panic("maa: pending occupancy accounting overflowed")
	}
	return occ
}

func (s *instanceState) settleCompletion(info completionInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.pending[info.ID]; ok {
		delete(s.pending, info.ID)
		op.complete(newResultRecord(info.ID, info.What, info.OK, info.Details))
		return
	}
	if s.earlyWindowOpen {
		if s.earlyResult != nil {
			s.faultLocked(fmt.Errorf("maa: a second early completion (id %d, what %q) arrived while "+
				"the early-result window already holds id %d: %w",
				info.ID, info.What, s.earlyResult.ID, native.ErrProtocol), nil)
			return
		}
		if info.ID <= s.asyncWatermark {
			s.faultLocked(fmt.Errorf("maa: a completion for already-accepted id %d (what %q) arrived "+
				"again while a submission is inside the raw gate: %w", info.ID, info.What, native.ErrProtocol), nil)
			return
		}
		if uint64(info.Bytes) > s.earlyBytes {
			s.faultLocked(fmt.Errorf("maa: early completion (id %d) payload %d bytes exceed Limits.EarlyBytes %d: %w",
				info.ID, info.Bytes, s.earlyBytes, native.ErrEventOverflow), nil)
			return
		}
		stored := info
		s.earlyResult = &stored
		return
	}
	if info.ID <= s.asyncWatermark {
		s.faultLocked(fmt.Errorf("maa: a duplicate completion for already-accepted id %d (what %q) "+
			"arrived after settlement: %w", info.ID, info.What, native.ErrProtocol), nil)
		return
	}
	s.faultLocked(fmt.Errorf("maa: a completion for unknown id %d (what %q) arrived with no early-result "+
		"window open: %w", info.ID, info.What, native.ErrProtocol), nil)
}

func (s *instanceState) reservePendingOperation(what string) (*operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseReady:
		if s.pendingOccupiedLocked() >= s.pendingOperations {
			return nil, fmt.Errorf("maa: pending operation limit %d reached, %s rejected: %w",
				s.pendingOperations, what, native.ErrOperationLimit)
		}
		s.pendingReserved++
		return newPendingOperation(), nil
	case phaseClosing:
		return nil, fmt.Errorf("maa: instance is closing, %s rejected: %w", what, native.ErrClosing)
	case phaseFaulted:
		if s.firstFault == nil {
			panic("maa: faulted instance state without a recorded first fault")
		}
		return nil, fmt.Errorf("maa: %s rejected by instance fault: %w", what, s.firstFault)
	case phaseClosed:
		return nil, fmt.Errorf("maa: instance is closed, %s rejected: %w", what, native.ErrClosed)
	default:
		panic(fmt.Sprintf("maa: instance state reserves a pending operation in phase %d", s.phase))
	}
}

func (s *instanceState) releasePendingReservation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releasePendingReservationLocked()
}

func (s *instanceState) releasePendingReservationLocked() {
	if s.pendingReserved == 0 {
		panic("maa: pending reservation released more times than reserved")
	}
	s.pendingReserved--
}

func (s *instanceState) beginAsyncRaw() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.earlyWindowOpen {
		panic("maa: async raw window opened while another window is still open")
	}
	if s.asyncWatermark == math.MaxInt32 {
		err := fmt.Errorf("maa: async call id high watermark reached MaxInt32, refusing to let the native counter wrap around: %w",
			native.ErrProtocol)
		s.faultLocked(err, nil)
		s.releasePendingReservationLocked()
		return err
	}
	s.earlyWindowOpen = true
	return nil
}

func (s *instanceState) finishAsyncRaw(op *operation, expectedWhat string, id int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.earlyWindowOpen {
		panic("maa: async raw finished without an open early-result window")
	}
	early := s.earlyResult
	s.earlyResult = nil
	s.earlyWindowOpen = false

	switch {
	case id == 0:
		s.releasePendingReservationLocked()
		if early != nil {
			err := fmt.Errorf("maa: async submission (%s) returned 0 while the early-result window "+
				"already holds a completion (id %d, what %q): %w", expectedWhat, early.ID, early.What, native.ErrProtocol)
			s.faultLocked(err, nil)
			return err
		}
		return fmt.Errorf("maa: async submission (%s) returned 0 (InvalidId), submission rejected: %w",
			expectedWhat, native.ErrNativeRejected)
	case id < 0:
		s.releasePendingReservationLocked()
		err := fmt.Errorf("maa: async submission (%s) returned negative id %d: %w",
			expectedWhat, id, native.ErrProtocol)
		s.faultLocked(err, nil)
		return err
	case AsyncID(id) <= s.asyncWatermark:
		s.releasePendingReservationLocked()
		err := fmt.Errorf("maa: async submission (%s) returned id %d, not greater than the accepted "+
			"high watermark %d: %w", expectedWhat, id, s.asyncWatermark, native.ErrProtocol)
		s.faultLocked(err, nil)
		return err
	}

	if s.firstFault != nil {
		s.releasePendingReservationLocked()
		return fmt.Errorf("maa: async submission (%s) returned id %d but the instance is already faulted: %w",
			expectedWhat, id, s.firstFault)
	}
	if _, exists := s.pending[AsyncID(id)]; exists {
		s.releasePendingReservationLocked()
		err := fmt.Errorf("maa: async submission (%s) returned id %d which is already pending: %w",
			expectedWhat, id, native.ErrProtocol)
		s.faultLocked(err, nil)
		return err
	}
	op.bindAsyncID(AsyncID(id))
	s.releasePendingReservationLocked()
	s.pending[AsyncID(id)] = op
	s.asyncWatermark = AsyncID(id)

	if early != nil {
		if early.ID != AsyncID(id) {
			err := fmt.Errorf("maa: the early completion (id %d, what %q) does not match the accepted "+
				"submission (%s, id %d): %w", early.ID, early.What, expectedWhat, id, native.ErrProtocol)
			s.faultLocked(err, nil)
			return err
		}
		if early.What != expectedWhat {
			err := fmt.Errorf("maa: the early completion (id %d) reports what %q, want %q: %w",
				early.ID, early.What, expectedWhat, native.ErrProtocol)
			s.faultLocked(err, nil)
			return err
		}
		delete(s.pending, AsyncID(id))
		op.complete(newResultRecord(early.ID, early.What, early.OK, early.Details))
	}
	return nil
}

func (s *instanceState) settleRemainingPending() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseClosing {
		panic(fmt.Sprintf("maa: remaining pending operations settled in phase %d", s.phase))
	}
	for id, op := range s.pending {
		delete(s.pending, id)
		op.settleClosed()
	}
}
