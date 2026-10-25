// 本文件是唯一定义的 raw API 表类型:api struct 保存 MaaCore 首期
// 26 个 R/O raw 函数字段,每次加载构造自己的临时表,发布后只读;
// 不采用可被外部复制的包级全局函数变量,raw 函数与函数表不向公开
// 层导出。
//
// 字段名 = C 符号去掉 Asst 前缀(如 AsstGetUUID → GetUUID);字段
// 类型严格按绑定设计 §3 冻结的 C 宽度取用——AsstBool→uint8、
// AsstId/AsstOptionKey→int32、AsstSize→uint64、handle/callback
// 地址/customArg token/hwnd→uintptr、UTF-8 字符串与 UUID 缓冲区
// →*byte、任务列表→*int32。unsafe.Pointer 仅允许 GetImage 与
// GetImageBgr 的 buff 参数(C void* 图像缓冲区),其余指针不因此
// 取得映射许可;C void 返回(Destroy、SetConnectionExtras、Log)
// 写成无结果列表,不虚构成功码。
//
// 本文件无平台条件:26 个字段共同声明于所有平台,Windows 特有的
// AsyncAttachWindow 由 symbols.go 的 windowsOnly 条目标注适用
// 范围,非 Windows 平台不查找该符号。声明形式由
// symbols_static_test.go 以 AST 断言逐字钉死。

package native

import "unsafe"

// api 是私有 raw 函数表:每字段一个具名 func 类型,由符号表按
// R/O 策略注册;签名冻结,禁止自定义宽度别名。
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
}
