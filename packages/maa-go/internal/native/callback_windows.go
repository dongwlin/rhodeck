//go:build windows

package native

import (
	"fmt"
	"sync"

	"github.com/ebitengine/purego"
)

// nativeCallback 保持 Windows callback 的 uintptr 返回适配,对应的 C
// 契约仍为 void。它复用 token 分发与 Go panic 边界,不使用 CDecl。
func nativeCallback(msg int32, detailsJSON *byte, customArg uintptr) uintptr {
	defer func() {
		if r := recover(); r != nil {
			recordCallbackFault(callbackFault{
				Kind:   callbackFaultPanic,
				Msg:    msg,
				Token:  customArg,
				Detail: fmt.Sprintf("callback 内 Go panic 已被 native 边界 recover(不宣称 recover 原生崩溃或 C++ 异常): %v", r),
			})
		}
	}()
	dispatchCallback(msg, detailsJSON, customArg)
	return 0
}

// 一个进程只分配一个 trampoline,不随实例或任务数增加。
var nativeCallbackTrampoline = sync.OnceValue(func() uintptr {
	return purego.NewCallback(nativeCallback)
})

// CallbackTrampoline 返回进程唯一的 Windows callback trampoline 地址。
// 它供创建接缝使用;平台 loader 的可用性由 NewLoader 单独决定。
func CallbackTrampoline() uintptr {
	return nativeCallbackTrampoline()
}

var _ windowsCallback = nativeCallback
