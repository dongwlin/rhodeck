package maa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// AsstMsg::AsyncCallInfo 的编号,取自上游 src/MaaCore/Common/AsstMsg.h。
const asyncCallInfoMessageID = MessageID(4)

type completionInfo struct {
	ID      AsyncID
	What    string
	OK      bool
	Details json.RawMessage
	Bytes   int
}

type queuedEvent struct {
	rec eventRecord
}

type eventDispatcher struct {
	state  *instanceState
	settle func(completionInfo)

	callbackBytes  uint64
	internalEvents uint64
	internalBytes  uint64
	publicEvents   uint64
	publicBytes    uint64

	mu   sync.Mutex
	cond *sync.Cond

	queue []queuedEvent
	count uint64
	bytes uint64

	intakeClosed     bool
	drainRequested   bool
	started          bool
	exited           chan struct{}
	processed        chan struct{}
	releaseRequested bool
	releaseCond      *sync.Cond

	public      chan Event
	ledger      []uint64
	ledgerBytes uint64

	seq uint64
}

func newEventDispatcher(
	state *instanceState,
	settle func(completionInfo),
	callbackBytes, internalEvents, internalBytes, publicEvents, publicBytes uint64,
) *eventDispatcher {
	if state == nil {
		panic("maa: event dispatcher created without instance state")
	}
	d := &eventDispatcher{
		state:          state,
		settle:         settle,
		callbackBytes:  callbackBytes,
		internalEvents: internalEvents,
		internalBytes:  internalBytes,
		publicEvents:   publicEvents,
		publicBytes:    publicBytes,
		// Limits.PublicEvents 上限 65,536(runtime.go eventsCountMax)保证 int 转换无损。
		public:    make(chan Event, int(publicEvents)),
		exited:    make(chan struct{}),
		processed: make(chan struct{}),
	}
	d.cond = sync.NewCond(&d.mu)
	d.releaseCond = sync.NewCond(&d.mu)
	return d
}

type eventSlot struct {
	d       *eventDispatcher
	msgID   MessageID
	length  int
	settled bool
}

func (d *eventDispatcher) admit(messageID MessageID, payloadLen int) (eventSlot, error) {
	d.mu.Lock()
	if d.intakeClosed {
		d.mu.Unlock()
		panic("maa: event admitted after the internal queue intake is closed")
	}
	var overflow error
	switch {
	case uint64(payloadLen) > d.callbackBytes:
		overflow = fmt.Errorf("maa: internal event queue rejects a callback message: "+
			"message id %d, payload %d bytes exceed Limits.CallbackBytes %d: %w",
			messageID, payloadLen, d.callbackBytes, native.ErrEventOverflow)
	case d.count >= d.internalEvents:
		overflow = fmt.Errorf("maa: internal event queue is full: "+
			"%d/%d events reserved, message id %d, payload %d bytes: %w",
			d.count, d.internalEvents, messageID, payloadLen, native.ErrEventOverflow)
	case d.bytes > d.internalBytes-uint64(payloadLen):
		// payloadLen ≤ CallbackBytes ≤ InternalBytes 由 validateLimits 交叉校验保证,减法不下溢。
		overflow = fmt.Errorf("maa: internal event queue is full: "+
			"%d/%d bytes reserved, message id %d, payload %d bytes: %w",
			d.bytes, d.internalBytes, messageID, payloadLen, native.ErrEventOverflow)
	}
	if overflow != nil {
		d.mu.Unlock()
		d.reportOverflow(overflow)
		return eventSlot{}, overflow
	}
	d.count++
	d.bytes += uint64(payloadLen)
	d.mu.Unlock()
	return eventSlot{d: d, msgID: messageID, length: payloadLen}, nil
}

func (s *eventSlot) commit(payload []byte) {
	if s.d == nil {
		panic("maa: commit on a zero event slot")
	}
	if s.settled {
		panic("maa: event slot settled more than once")
	}
	if len(payload) != s.length {
		panic(fmt.Sprintf("maa: event slot commit length %d does not match the reserved length %d",
			len(payload), s.length))
	}
	d := s.d
	s.settled = true
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.drainRequested {
		panic("maa: event slot committed after the dispatcher drain was requested")
	}
	d.queue = append(d.queue, queuedEvent{rec: newEventRecord(s.msgID, payload, 0)})
	d.cond.Broadcast()
}

func (s *eventSlot) abort() {
	if s.d == nil {
		panic("maa: abort on a zero event slot")
	}
	if s.settled {
		panic("maa: event slot settled more than once")
	}
	d := s.d
	s.settled = true
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count == 0 {
		panic("maa: event slot aborted more times than reserved")
	}
	d.count--
	d.bytes -= uint64(s.length)
}

func (d *eventDispatcher) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		panic("maa: event dispatcher started more than once")
	}
	d.started = true
	go d.run()
}

func (d *eventDispatcher) events() <-chan Event {
	return d.public
}

func (d *eventDispatcher) closeIntake() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.intakeClosed {
		panic("maa: event intake closed twice")
	}
	d.intakeClosed = true
}

func (d *eventDispatcher) drain() {
	d.requestDrain()
	d.waitProcessed()
	d.closeEvents()
}

func (d *eventDispatcher) waitProcessed() {
	d.mu.Lock()
	drained := d.drainRequested
	d.mu.Unlock()
	if !drained {
		panic("maa: event dispatcher waited for processed before drain is requested")
	}
	<-d.processed
}

func (d *eventDispatcher) closeEvents() {
	d.mu.Lock()
	if !d.drainRequested {
		d.mu.Unlock()
		panic("maa: event dispatcher events closed before drain is requested")
	}
	if d.releaseRequested {
		d.mu.Unlock()
		panic("maa: event dispatcher events closed more than once")
	}
	d.releaseRequested = true
	d.releaseCond.Broadcast()
	d.mu.Unlock()
	<-d.exited
}

func (d *eventDispatcher) requestDrain() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started {
		panic("maa: event dispatcher drained before start")
	}
	if !d.intakeClosed {
		panic("maa: event dispatcher drained before the intake is closed")
	}
	if d.drainRequested {
		panic("maa: event dispatcher drained more than once")
	}
	d.drainRequested = true
	d.cond.Broadcast()
}

func (d *eventDispatcher) run() {
	for {
		item, ok := d.take()
		if !ok {
			break
		}
		d.process(item)
	}
	d.mu.Lock()
	d.reconcileLedgerLocked()
	close(d.processed)
	for !d.releaseRequested {
		d.releaseCond.Wait()
	}
	close(d.public)
	d.mu.Unlock()
	close(d.exited)
}

func (d *eventDispatcher) take() (queuedEvent, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.queue) == 0 && !d.drainRequested {
		d.cond.Wait()
	}
	if len(d.queue) == 0 {
		return queuedEvent{}, false
	}
	item := d.queue[0]
	d.queue[0] = queuedEvent{}
	d.queue = d.queue[1:]
	return item, true
}

func (d *eventDispatcher) process(item queuedEvent) {
	defer d.releaseInternal(len(item.rec.json))
	raw := item.rec.json

	valid := utf8.Valid(raw) && json.Valid(raw)
	if !valid {
		d.reportProtocol(fmt.Errorf("maa: callback message id %d is not valid UTF-8/JSON "+
			"(%d bytes retained): %w", item.rec.messageID, len(raw), native.ErrProtocol), raw)
	}

	sequence, ok := d.nextSequence()
	if !ok {
		d.reportProtocol(fmt.Errorf("maa: local event sequence numbers are exhausted, "+
			"message id %d dropped: %w", item.rec.messageID, native.ErrProtocol), raw)
		d.state.recordLostEvents(1)
		return
	}

	if item.rec.messageID == asyncCallInfoMessageID && valid {
		info, perr := parseAsyncCallInfo(raw)
		if perr != nil {
			d.reportProtocol(perr, raw)
		} else if d.settle != nil {
			d.settle(info)
		}
	}

	if !d.tryDeliver(item.rec.messageID, sequence, raw) {
		d.reportOverflow(fmt.Errorf("maa: public events buffer is full: "+
			"message id %d, event %d bytes dropped, Limits.PublicEvents %d / PublicBytes %d: %w",
			item.rec.messageID, len(raw), d.publicEvents, d.publicBytes, native.ErrEventOverflow))
	}
}

func (d *eventDispatcher) releaseInternal(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count == 0 {
		panic("maa: internal event queue accounting underflowed")
	}
	d.count--
	d.bytes -= uint64(n)
}

func (d *eventDispatcher) nextSequence() (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seq == math.MaxUint64 {
		return 0, false
	}
	d.seq++
	return d.seq, true
}

func (d *eventDispatcher) tryDeliver(messageID MessageID, sequence uint64, payload []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reconcileLedgerLocked()

	payloadLen := uint64(len(payload))
	// payloadLen ≤ CallbackBytes ≤ PublicBytes 由 validateLimits 交叉校验保证,减法不下溢。
	if uint64(len(d.ledger))+1 > d.publicEvents || d.ledgerBytes > d.publicBytes-payloadLen {
		return false
	}
	ev := Event{
		MessageID: messageID,
		JSON:      bytes.Clone(payload),
		Sequence:  sequence,
	}
	select {
	case d.public <- ev:
		d.ledger = append(d.ledger, payloadLen)
		d.ledgerBytes += payloadLen
		return true
	default:
		return false
	}
}

func (d *eventDispatcher) reconcileLedgerLocked() {
	drop := len(d.ledger) - len(d.public)
	if drop <= 0 {
		return
	}
	for i := 0; i < drop; i++ {
		d.ledgerBytes -= d.ledger[i]
	}
	d.ledger = append(d.ledger[:0], d.ledger[drop:]...)
}

func (d *eventDispatcher) reportOverflow(cause error) {
	d.state.recordFaultWithDetail(cause, nil)
	d.state.recordLostEvents(1)
}

func (d *eventDispatcher) reportProtocol(cause error, raw []byte) {
	d.state.recordFaultWithDetail(cause, raw)
}

func parseAsyncCallInfo(raw json.RawMessage) (completionInfo, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message is not decodable JSON (%v): %w", err, native.ErrProtocol)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message is not a JSON object: %w", native.ErrProtocol)
	}

	var (
		info                          completionInfo
		seenWhat, seenID, seenDetails bool
	)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object is truncated (%v): %w", err, native.ErrProtocol)
		}
		key, ok := keyTok.(string)
		if !ok {
			return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object has a non-string key: %w", native.ErrProtocol)
		}
		switch key {
		case "what":
			if seenWhat {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message has duplicate top-level key %q: %w", key, native.ErrProtocol)
			}
			tok, err := dec.Token()
			if err != nil {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object is truncated (%v): %w", err, native.ErrProtocol)
			}
			what, ok := tok.(string)
			if !ok {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is %v, want a string: %w", key, tok, native.ErrProtocol)
			}
			info.What = what
			seenWhat = true
		case "async_call_id":
			if seenID {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message has duplicate top-level key %q: %w", key, native.ErrProtocol)
			}
			tok, err := dec.Token()
			if err != nil {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object is truncated (%v): %w", err, native.ErrProtocol)
			}
			num, ok := tok.(json.Number)
			if !ok {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is %v, want a number: %w", key, tok, native.ErrProtocol)
			}
			id, err := num.Int64()
			if err != nil {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is %s, want an exact integer: %w", key, num, native.ErrProtocol)
			}
			if id < math.MinInt32 || id > math.MaxInt32 {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is %d, outside the int32 range: %w", key, id, native.ErrProtocol)
			}
			info.ID = AsyncID(id)
			seenID = true
		case "details":
			if seenDetails {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message has duplicate top-level key %q: %w", key, native.ErrProtocol)
			}
			var rawDetails json.RawMessage
			if err := dec.Decode(&rawDetails); err != nil {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is not decodable JSON (%v): %w", key, err, native.ErrProtocol)
			}
			inner := json.NewDecoder(bytes.NewReader(rawDetails))
			tok, err := inner.Token()
			if err != nil {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo details object is truncated (%v): %w", err, native.ErrProtocol)
			}
			if delim, ok := tok.(json.Delim); !ok || delim != '{' {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is %v, want an object: %w", key, tok, native.ErrProtocol)
			}
			ret, err := parseDetailsRet(inner)
			if err != nil {
				return completionInfo{}, err
			}
			info.OK = ret
			info.Details = rawDetails
			seenDetails = true
		default:
			if err := skipJSONValue(dec); err != nil {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object is truncated (%v): %w", err, native.ErrProtocol)
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object is truncated (%v): %w", err, native.ErrProtocol)
	}
	switch {
	case !seenWhat:
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message is missing required top-level key %q: %w", "what", native.ErrProtocol)
	case !seenID:
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message is missing required top-level key %q: %w", "async_call_id", native.ErrProtocol)
	case !seenDetails:
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message is missing required top-level key %q: %w", "details", native.ErrProtocol)
	}
	info.Bytes = len(raw)
	return info, nil
}

func parseDetailsRet(dec *json.Decoder) (bool, error) {
	var (
		ret     bool
		seenRet bool
	)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return false, fmt.Errorf("maa: AsyncCallInfo details object is truncated (%v): %w", err, native.ErrProtocol)
		}
		key, ok := keyTok.(string)
		if !ok {
			return false, fmt.Errorf("maa: AsyncCallInfo details object has a non-string key: %w", native.ErrProtocol)
		}
		if key != "ret" {
			if err := skipJSONValue(dec); err != nil {
				return false, fmt.Errorf("maa: AsyncCallInfo details object is truncated (%v): %w", err, native.ErrProtocol)
			}
			continue
		}
		if seenRet {
			return false, fmt.Errorf("maa: AsyncCallInfo details object has duplicate key %q: %w", key, native.ErrProtocol)
		}
		tok, err := dec.Token()
		if err != nil {
			return false, fmt.Errorf("maa: AsyncCallInfo details object is truncated (%v): %w", err, native.ErrProtocol)
		}
		b, ok := tok.(bool)
		if !ok {
			return false, fmt.Errorf("maa: AsyncCallInfo details %q is %v, want a bool: %w", key, tok, native.ErrProtocol)
		}
		ret = b
		seenRet = true
	}
	if _, err := dec.Token(); err != nil {
		return false, fmt.Errorf("maa: AsyncCallInfo details object is truncated (%v): %w", err, native.ErrProtocol)
	}
	if !seenRet {
		return false, fmt.Errorf("maa: AsyncCallInfo details object is missing required key %q: %w", "ret", native.ErrProtocol)
	}
	return ret, nil
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if _, ok := tok.(json.Delim); ok {
		depth := 1
		for depth > 0 {
			tok, err := dec.Token()
			if err != nil {
				return err
			}
			switch delim, _ := tok.(json.Delim); delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}
