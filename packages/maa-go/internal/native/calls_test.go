// 本文件是 raw 调用包装边界的转发用例:把假函数注入临时 api 表,
// 断言 APITable 的 6 个初始化调用包装把参数与返回值逐字转发到正确
// 的 raw 字段,不做任何改写。全程不进入系统 loader、不调用真实
// 原生地址(假函数只记录参数),也不替换任何全局函数变量。

package native

import (
	"testing"
	"unsafe"
)

// cStringArg 解码调用方构造的 NUL 终止 C 字符串参数,供断言比对
// 传参内容。空指针记为 "<nil>"。
func cStringArg(p *byte) string {
	if p == nil {
		return "<nil>"
	}
	var out []byte
	for i := 0; ; i++ {
		b := *(*byte)(unsafe.Add(unsafe.Pointer(p), i))
		if b == 0 {
			return string(out)
		}
		out = append(out, b)
	}
}

// TestAPITableInitCallForwarding 验证 6 个初始化调用包装的转发:
// 恰好命中对应的 raw 字段,参数逐字传递,返回值原样带回;
// SetConnectionExtras 是 C void 函数,包装无返回值、只记录传参。
func TestAPITableInitCallForwarding(t *testing.T) {
	table := &APITable{api: &api{}}

	versionBytes := []byte("6.20.1\x00")
	type call struct {
		key    int32
		first  string
		second string
	}
	var (
		versionCalled  bool
		nullSizeCalled bool
		nullSizeValue  uint64
		userDirCalls   []string
		staticCalls    []call
		resourceCalls  []string
		extrasCalls    []call
	)

	table.api.GetVersion = func() *byte {
		versionCalled = true
		return &versionBytes[0]
	}
	table.api.GetNullSize = func() uint64 {
		nullSizeCalled = true
		nullSizeValue = ^uint64(0)
		return nullSizeValue
	}
	table.api.SetUserDir = func(path *byte) uint8 {
		userDirCalls = append(userDirCalls, cStringArg(path))
		return 1
	}
	table.api.SetStaticOption = func(key int32, value *byte) uint8 {
		staticCalls = append(staticCalls, call{key: key, first: cStringArg(value)})
		return 0 // 失败码也必须原样返回
	}
	table.api.LoadResource = func(path *byte) uint8 {
		resourceCalls = append(resourceCalls, cStringArg(path))
		return 1
	}
	table.api.SetConnectionExtras = func(name *byte, extras *byte) {
		extrasCalls = append(extrasCalls, call{first: cStringArg(name), second: cStringArg(extras)})
	}

	if got := table.GetVersion(); got != &versionBytes[0] {
		t.Errorf("GetVersion returned %p, want the borrowed pointer passed through as-is", got)
	}
	if !versionCalled {
		t.Error("GetVersion did not forward to the AsstGetVersion field")
	}
	if got := table.GetNullSize(); got != ^uint64(0) || !nullSizeCalled {
		t.Errorf("GetNullSize = %#x (called=%v), want the return value forwarded as-is", got, nullSizeCalled)
	}
	if ret := table.SetUserDir(cStringPtrForTest("/home/user/.maa")); ret != 1 {
		t.Errorf("SetUserDir returned %d, want 1", ret)
	}
	if len(userDirCalls) != 1 || userDirCalls[0] != "/home/user/.maa" {
		t.Errorf("SetUserDir received arguments = %v, want [/home/user/.maa]", userDirCalls)
	}
	if ret := table.SetStaticOption(1, cStringPtrForTest("cpu_ocr")); ret != 0 {
		t.Errorf("SetStaticOption returned %d, want 0 (failure code returned as-is)", ret)
	}
	if len(staticCalls) != 1 || staticCalls[0].key != 1 || staticCalls[0].first != "cpu_ocr" {
		t.Errorf("SetStaticOption received arguments = %+v, want [{key:1 first:cpu_ocr}]", staticCalls)
	}
	if ret := table.LoadResource(cStringPtrForTest("/opt/maa/resource")); ret != 1 {
		t.Errorf("LoadResource returned %d, want 1", ret)
	}
	if len(resourceCalls) != 1 || resourceCalls[0] != "/opt/maa/resource" {
		t.Errorf("LoadResource received arguments = %v, want [/opt/maa/resource]", resourceCalls)
	}
	table.SetConnectionExtras(cStringPtrForTest("sink"), cStringPtrForTest(`{"a":1}`))
	if len(extrasCalls) != 1 || extrasCalls[0].first != "sink" || extrasCalls[0].second != `{"a":1}` {
		t.Errorf("SetConnectionExtras received arguments = %+v, want [{first:sink second:{\"a\":1}}]", extrasCalls)
	}
}

// cStringPtrForTest 在测试内构造 NUL 终止的 C 字符串指针,模拟公开
// 层完成的输入编码。
func cStringPtrForTest(s string) *byte {
	buf := make([]byte, len(s)+1)
	copy(buf, s)
	return &buf[0]
}

// TestAPITableOptionalCallForwarding 验证四个可选 raw 包装的转发:
// GetImageBgr/AsyncAttachWindow/GetMapLevelKey/GetItemName 恰好命中
// 对应的 raw 字段,参数逐字传递,返回值原样带回——包括 GetMapLevelKey
// 的 struct 按值返回(成员指针原样透传,不解引用、不复制)与
// AsyncAttachWindow 的六参数形状。全程不进入系统 loader、不调用真实
// 原生地址。
func TestAPITableOptionalCallForwarding(t *testing.T) {
	table := &APITable{api: &api{}}

	var (
		bgrHandle    uintptr
		bgrBuff      unsafe.Pointer
		bgrSize      uint64
		attachCalls  []uintptr
		levelKeyGot  string
		itemNameGot  string
		returnedBgr  = []byte{0xAA, 0xBB}
		returnedName = []byte{'I', 't', 'e', 'm', 0x00}
	)

	table.api.GetImageBgr = func(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64 {
		bgrHandle, bgrBuff, bgrSize = handle, buff, buffSize
		return 12
	}
	table.api.AsyncAttachWindow = func(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32 {
		attachCalls = append(attachCalls, handle, hwnd, uintptr(screencapMethod), uintptr(mouseMethod), uintptr(keyboardMethod), uintptr(block))
		return 77
	}
	table.api.GetMapLevelKey = func(key *byte) mapLevelKey {
		levelKeyGot = cStringArg(key)
		return mapLevelKey{}
	}
	table.api.GetItemName = func(id *byte) *byte {
		itemNameGot = cStringArg(id)
		return &returnedName[0]
	}

	if got := table.GetImageBgr(0x31, unsafe.Pointer(&returnedBgr[0]), 12); got != 12 {
		t.Errorf("GetImageBgr returned %d, want 12", got)
	}
	if bgrHandle != 0x31 || bgrBuff != unsafe.Pointer(&returnedBgr[0]) || bgrSize != 12 {
		t.Errorf("GetImageBgr received arguments = %#x %p %d, want 0x31 same pointer 12", bgrHandle, bgrBuff, bgrSize)
	}

	if got := table.AsyncAttachWindow(0x32, 0x51, 1<<2, 1<<1, 1, 0); got != 77 {
		t.Errorf("AsyncAttachWindow returned %d, want 77", got)
	}
	wantAttach := []uintptr{0x32, 0x51, 1 << 2, 1 << 1, 1, 0}
	if len(attachCalls) != len(wantAttach) {
		t.Fatalf("AsyncAttachWindow received %d components, want %d", len(attachCalls), len(wantAttach))
	}
	for i := range wantAttach {
		if attachCalls[i] != wantAttach[i] {
			t.Errorf("AsyncAttachWindow component #%d = %#x, want %#x (six-parameter slots forwarded verbatim)", i, attachCalls[i], wantAttach[i])
		}
	}

	if got := table.GetMapLevelKey(cStringPtrForTest("abifx-hit")); got.StageID != nil {
		t.Errorf("GetMapLevelKey returned %+v, want the zero struct passed through as-is", got)
	}
	if levelKeyGot != "abifx-hit" {
		t.Errorf("GetMapLevelKey received key = %q, want abifx-hit", levelKeyGot)
	}

	if got := table.GetItemName(cStringPtrForTest("abifx-item")); got != &returnedName[0] {
		t.Errorf("GetItemName returned %p, want the borrowed pointer passed through as-is", got)
	}
	if itemNameGot != "abifx-item" {
		t.Errorf("GetItemName received id = %q, want abifx-item", itemNameGot)
	}
	_ = returnedBgr
}
