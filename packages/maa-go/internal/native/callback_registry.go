package native

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"
)

type CallbackDelivery func(msg int32, detailsJSON *byte)

type callbackFault struct {
	Kind   string
	Msg    int32
	Token  uintptr
	Detail string
}

const (
	callbackFaultPanic        = "panic"
	callbackFaultTokenZero    = "token-zero"
	callbackFaultUnknownToken = "unknown-token"
)

var lastCallbackFault atomic.Pointer[callbackFault]

func recordCallbackFault(fault callbackFault) {
	lastCallbackFault.Store(&fault)
}

type callbackTokenSource struct {
	mu   sync.Mutex
	last uintptr
}

var callbackTokens callbackTokenSource

func (s *callbackTokenSource) next() (uintptr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == ^uintptr(0) {
		return 0, fmt.Errorf("maa: callback token space is exhausted: %w", ErrProtocol)
	}
	s.last++
	return s.last, nil
}

type callbackTargetRegistry struct {
	mu      sync.RWMutex
	targets map[uintptr]CallbackDelivery
}

var callbackTargets = &callbackTargetRegistry{targets: make(map[uintptr]CallbackDelivery)}

func (r *callbackTargetRegistry) install(token uintptr, deliver CallbackDelivery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.targets[token]; exists {
		panic(fmt.Sprintf("maa: callback token %d registered more than once", token))
	}
	r.targets[token] = deliver
}

func (r *callbackTargetRegistry) remove(token uintptr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.targets[token]; !exists {
		panic(fmt.Sprintf("maa: callback token %d removed without a registration", token))
	}
	delete(r.targets, token)
}

func (r *callbackTargetRegistry) lookup(token uintptr) CallbackDelivery {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.targets[token]
}

func RegisterCallbackTarget(deliver CallbackDelivery) (uintptr, error) {
	if deliver == nil {
		return 0, fmt.Errorf("maa: callback target registered without a delivery function: %w", ErrInvalidInput)
	}
	token, err := callbackTokens.next()
	if err != nil {
		return 0, err
	}
	callbackTargets.install(token, deliver)
	return token, nil
}

func UnregisterCallbackTarget(token uintptr) {
	callbackTargets.remove(token)
}

func LookupCallbackDelivery(token uintptr) CallbackDelivery {
	return lookupCallbackTarget(token)
}

func lookupCallbackTarget(token uintptr) CallbackDelivery {
	return callbackTargets.lookup(token)
}

var callbackDispatchHookSlot atomic.Pointer[func(token uintptr)]

func dispatchCallback(msg int32, detailsJSON *byte, token uintptr) {
	if token == 0 {
		recordCallbackFault(callbackFault{
			Kind:   callbackFaultTokenZero,
			Msg:    msg,
			Token:  token,
			Detail: "callback carries token 0, no deliverable target; token 0 is never accepted by the registry",
		})
		return
	}
	deliver := lookupCallbackTarget(token)
	if deliver == nil {
		recordCallbackFault(callbackFault{
			Kind:   callbackFaultUnknownToken,
			Msg:    msg,
			Token:  token,
			Detail: "callback carries a token not registered in the registry (never registered or already removed), message not delivered; silent drops are not allowed",
		})
		return
	}
	if hook := callbackDispatchHookSlot.Load(); hook != nil {
		(*hook)(token)
	}
	deliver(msg, detailsJSON)
}

func CallbackDetailsLength(detailsJSON *byte, maxBytes int) (length int, found bool) {
	if detailsJSON == nil {
		return 0, false
	}
	for i := 0; i <= maxBytes; i++ {
		if *(*byte)(unsafe.Add(unsafe.Pointer(detailsJSON), i)) == 0 {
			return i, true
		}
	}
	return 0, false
}

func CopyCallbackDetails(detailsJSON *byte, length int) []byte {
	if length < 0 {
		panic(fmt.Sprintf("maa: callback details copied with a negative length %d", length))
	}
	out := make([]byte, length)
	if length > 0 {
		copy(out, unsafe.Slice(detailsJSON, length))
	}
	return out
}
