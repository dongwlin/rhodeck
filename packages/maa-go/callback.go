package maa

import (
	"fmt"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

type callbackTarget struct {
	state         *instanceState
	disp          *eventDispatcher
	callbackBytes uint64
}

func newCallbackTarget(state *instanceState, disp *eventDispatcher, callbackBytes uint64) *callbackTarget {
	if state == nil {
		panic("maa: callback target created without instance state")
	}
	if disp == nil {
		panic("maa: callback target created without event dispatcher")
	}
	return &callbackTarget{state: state, disp: disp, callbackBytes: callbackBytes}
}

func (t *callbackTarget) deliver(msg int32, detailsJSON *byte) {
	defer func() {
		if r := recover(); r != nil {
			t.state.recordFault(fmt.Errorf(
				"maa: callback delivery panicked on message id %d: %v: %w",
				msg, r, native.ErrProtocol))
		}
	}()

	lease, ok := t.state.admitCallback()
	if !ok {
		return
	}
	defer lease.release()

	if detailsJSON == nil {
		t.state.recordFaultWithDetail(fmt.Errorf(
			"maa: callback message id %d delivered a nil details pointer: %w",
			msg, native.ErrProtocol), nil)
		t.state.recordLostEvents(1)
		return
	}

	length, found := native.CallbackDetailsLength(detailsJSON, int(t.callbackBytes))
	if !found {
		t.state.recordFaultWithDetail(fmt.Errorf(
			"maa: callback message id %d details have no NUL within Limits.CallbackBytes %d "+
				"(scan range %d bytes): %w",
			msg, t.callbackBytes, int(t.callbackBytes)+1, native.ErrEventOverflow), nil)
		t.state.recordLostEvents(1)
		return
	}

	slot, err := t.disp.admit(MessageID(msg), length)
	if err != nil {
		return
	}
	slot.commit(native.CopyCallbackDetails(detailsJSON, length))
}
