// 本文件实现 Win32 窗口绑定(AsstAsyncAttachWindow)的公开入口与
// 方法掩码类型。该接口只在 Windows 平台生效:底层符号由符号表的
// windowsOnly 条目标注,仅 Windows 参与查找;非 Windows 平台一律
// 返回 ErrUnsupported,binding 不在其他系统上模拟成功,也不静默降级。
// Windows 上符号仍可能缺席(可选符号,部分发行包裁剪),由能力快照
// 二次把关,同样返回 ErrUnsupported。
//
// 绑定是异步提交:与 Connect/Click/Screencap 同一提交纪律,最终结果
// 由 AsyncCallInfo(what=AttachWindow)按 async_call_id 关联确认后经
// Operation.Wait 取得——非零 ID 只表示提交被接受,不是绑定已建立的
// 证明。

package maa

import (
	"fmt"
	"runtime"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// Win32ScreencapMethod 是 Win32 截图方式位掩码(仅 Windows 生效),
// 取值逐字对应上游 AsstWin32ScreencapMethodEnum;可按位组合,由 Core
// 解释组合语义,binding 不做业务枚举封闭。
type Win32ScreencapMethod uint64

// Win32 截图方式(逐字对应 AsstCaller.h:11-20)。
const (
	Win32ScreencapNone                 Win32ScreencapMethod = 0
	Win32ScreencapGDI                  Win32ScreencapMethod = 1
	Win32ScreencapFramePool            Win32ScreencapMethod = 1 << 1
	Win32ScreencapDXGIDesktopDup       Win32ScreencapMethod = 1 << 2
	Win32ScreencapDXGIDesktopDupWindow Win32ScreencapMethod = 1 << 3
	Win32ScreencapPrintWindow          Win32ScreencapMethod = 1 << 4
	Win32ScreencapScreenDC             Win32ScreencapMethod = 1 << 5
)

// Win32InputMethod 是 Win32 输入方式位掩码(仅 Windows 生效),取值
// 逐字对应上游 AsstWin32InputMethodEnum;鼠标与键盘各自取值,可按位
// 组合,由 Core 解释组合语义,binding 不做业务枚举封闭。
type Win32InputMethod uint64

// Win32 输入方式(逐字对应 AsstCaller.h:23-38)。
const (
	Win32InputNone                     Win32InputMethod = 0
	Win32InputSeize                    Win32InputMethod = 1
	Win32InputSendMessage              Win32InputMethod = 1 << 1
	Win32InputPostMessage              Win32InputMethod = 1 << 2
	Win32InputLegacyEvent              Win32InputMethod = 1 << 3
	Win32InputPostThreadMessage        Win32InputMethod = 1 << 4
	Win32InputSendMessageWithCursorPos Win32InputMethod = 1 << 5
	Win32InputPostMessageWithCursorPos Win32InputMethod = 1 << 6
	Win32InputSendMessageWithWindowPos Win32InputMethod = 1 << 7
	Win32InputPostMessageWithWindowPos Win32InputMethod = 1 << 8
	Win32InputInterception             Win32InputMethod = 1 << 9
	Win32InputAnchoredTouch            Win32InputMethod = 1 << 10
)

// AttachWindow 异步提交 Win32 窗口绑定(AsstAsyncAttachWindow,
// block=false):hwnd 是目标窗口句柄,三个 method 掩码原样透传(可
// 用 Win32ScreencapMethod/Win32InputMethod 常量按位组合),返回的
// *Operation 语义同 Connect——非零 ID 只表示提交被接受,最终结果
// (绑定成功与否的 details.ret)由 AsyncCallInfo(what=AttachWindow)
// 按 ID 关联确认后经 Wait 取得。
//
// 平台门:本接口只在 Windows 平台生效,非 Windows 平台一律返回包装
// ErrUnsupported 的错误(符号在非 Windows 平台不参与查找,binding 不
// 模拟成功)。能力门:Windows 上符号缺席(可选符号)同样返回包装
// ErrUnsupported 的错误。错误语义其余同 Connect:生命周期拒绝经错误
// 链可辨(ErrClosing/首因/ErrClosed);raw 返回 0 包装
// ErrNativeRejected(明确的提交失败,已回收预留);ID 复用/倒退等
// 协议破坏包装 ErrProtocol 并使实例故障。
func (in *Instance) AttachWindow(hwnd uintptr, screencapMethod Win32ScreencapMethod, mouseMethod, keyboardMethod Win32InputMethod) (*Operation, error) {
	if !in.rt.caps.AsyncAttachWindow() {
		return nil, fmt.Errorf("maa: AsstAsyncAttachWindow is not available on %s or in this capability set: %w",
			runtime.GOOS, native.ErrUnsupported)
	}
	return in.asyncCall(asyncWhatAttachWindow, func(handle uintptr) int32 {
		return in.caller.asyncAttachWindow(handle, hwnd, uint64(screencapMethod), uint64(mouseMethod), uint64(keyboardMethod), asyncBlockFalse)
	})
}
