//go:build linux || darwin || windows

package native

import (
	"math"
	"strings"
	"sync"
	"testing"
)

// 这些 Go 单测验证平台装配及分发接缝，不证明原生线程或 C ABI 往返。
func TestCallbackTrampolineStableAcrossConcurrentReads(t *testing.T) {
	const readers = 32
	const reads = 16
	addresses := make(chan uintptr, readers*reads)
	var wg sync.WaitGroup
	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			for range reads {
				addresses <- CallbackTrampoline()
			}
		}()
	}
	wg.Wait()
	close(addresses)
	want := CallbackTrampoline()
	if want == 0 {
		t.Fatal("callback trampoline 地址必须非零")
	}
	for got := range addresses {
		if got != want {
			t.Fatalf("并发读取 trampoline 地址 = %#x, want %#x", got, want)
		}
	}
}

func TestNativeCallbackDispatchAndPanicBoundary(t *testing.T) {
	// lastCallbackFault 是进程级槽，故本用例及子用例不使用 t.Parallel。
	saveFaultSlot(t)
	t.Run("精确分发注册 token", func(t *testing.T) {
		lastCallbackFault.Store(nil)
		recorder := &cbDeliveryRecorder{}
		token := mustRegister(t, recorder.deliver)
		t.Cleanup(func() { UnregisterCallbackTarget(token) })
		// 没有 NUL 的缓冲仍可安全透传：平台装配层不读取原生指针。
		buffer := []byte(`{"future":9007199254740993}`)
		pointer := &buffer[0]
		const msg = int32(math.MinInt32)
		nativeCallback(msg, pointer, token)
		deliveries := recorder.snapshot()
		if len(deliveries) != 1 || deliveries[0].msg != msg || deliveries[0].ptr != pointer {
			t.Fatalf("nativeCallback 分发 = %+v, want 精确透传消息与指针一次", deliveries)
		}
		if fault := lastCallbackFault.Load(); fault != nil {
			t.Fatalf("正常分发记录了边界故障：%+v", fault)
		}
	})
	t.Run("投递 panic 记录后正常返回", func(t *testing.T) {
		lastCallbackFault.Store(nil)
		const probe = "native-callback-go-panic-probe"
		const msg = int32(-97)
		pointer := cbTestCBytes(`{"panic":true}`)
		invoked := false
		token := mustRegister(t, func(gotMsg int32, gotPointer *byte) {
			invoked = true
			if gotMsg != msg || gotPointer != pointer {
				t.Errorf("panic 投递参数 = (%d, %p), want (%d, %p)", gotMsg, gotPointer, msg, pointer)
			}
			panic(probe)
		})
		t.Cleanup(func() { UnregisterCallbackTarget(token) })
		// 不在测试中 recover：panic 若越过装配层，本用例直接失败。
		nativeCallback(msg, pointer, token)
		if !invoked {
			t.Fatal("nativeCallback 未调用注册的 delivery")
		}
		fault := lastCallbackFault.Load()
		if fault == nil {
			t.Fatal("delivery panic 后没有可观察的边界故障")
		}
		if fault.Kind != callbackFaultPanic || fault.Msg != msg || fault.Token != token || !strings.Contains(fault.Detail, probe) {
			t.Fatalf("panic 故障 = %+v, want 原始消息/token/panic 内容", fault)
		}
	})
}
