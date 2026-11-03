package native

import "unsafe"

// mapLevelKey 的成员指针由 Core 借出:实现写入 thread_local 存储,只在
// 本次调用与同线程的下一次调用之间有效,调用方必须立即有界复制;依据
// 见 packages/maa-go/query.go 的同类说明与 abifixture_linux_test.go 的
// thread-local 用例。
type mapLevelKey struct {
	StageID *byte
	Code    *byte
	LevelID *byte
	Name    *byte
}

type api struct {
	SetUserDir          func(path *byte) uint8
	LoadResource        func(path *byte) uint8
	SetStaticOption     func(key int32, value *byte) uint8
	Create              func() uintptr
	CreateEx            func(callback uintptr, customArg uintptr) uintptr
	Destroy             func(handle uintptr)
	SetInstanceOption   func(handle uintptr, key int32, value *byte) uint8
	AppendTask          func(handle uintptr, taskType *byte, params *byte) int32
	SetTaskParams       func(handle uintptr, id int32, params *byte) uint8
	Start               func(handle uintptr) uint8
	Stop                func(handle uintptr) uint8
	Running             func(handle uintptr) uint8
	Connected           func(handle uintptr) uint8
	BackToHome          func(handle uintptr) uint8
	AsyncConnect        func(handle uintptr, adbPath *byte, address *byte, config *byte, block uint8) int32
	SetConnectionExtras func(name *byte, extras *byte)
	AsyncClick          func(handle uintptr, x int32, y int32, block uint8) int32
	AsyncScreencap      func(handle uintptr, block uint8) int32
	GetImage            func(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64
	GetImageBgr         func(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64
	GetUUID             func(handle uintptr, buff *byte, buffSize uint64) uint64
	GetTasksList        func(handle uintptr, buff *int32, buffSize uint64) uint64
	GetNullSize         func() uint64
	GetVersion          func() *byte
	Log                 func(level *byte, message *byte)
	AsyncAttachWindow   func(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32
	GetMapLevelKey      func(key *byte) mapLevelKey
	GetItemName         func(id *byte) *byte
}
