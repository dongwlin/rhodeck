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
		t.Errorf("GetVersion 返回 %p, want 借用指针原样透传", got)
	}
	if !versionCalled {
		t.Error("GetVersion 未转发到 AsstGetVersion 字段")
	}
	if got := table.GetNullSize(); got != ^uint64(0) || !nullSizeCalled {
		t.Errorf("GetNullSize = %#x(called=%v), want 原样转发返回值", got, nullSizeCalled)
	}
	if ret := table.SetUserDir(cStringPtrForTest("/home/user/.maa")); ret != 1 {
		t.Errorf("SetUserDir 返回 %d, want 1", ret)
	}
	if len(userDirCalls) != 1 || userDirCalls[0] != "/home/user/.maa" {
		t.Errorf("SetUserDir 收到的参数 = %v, want [/home/user/.maa]", userDirCalls)
	}
	if ret := table.SetStaticOption(1, cStringPtrForTest("cpu_ocr")); ret != 0 {
		t.Errorf("SetStaticOption 返回 %d, want 0(失败码原样返回)", ret)
	}
	if len(staticCalls) != 1 || staticCalls[0].key != 1 || staticCalls[0].first != "cpu_ocr" {
		t.Errorf("SetStaticOption 收到的参数 = %+v, want [{key:1 first:cpu_ocr}]", staticCalls)
	}
	if ret := table.LoadResource(cStringPtrForTest("/opt/maa/resource")); ret != 1 {
		t.Errorf("LoadResource 返回 %d, want 1", ret)
	}
	if len(resourceCalls) != 1 || resourceCalls[0] != "/opt/maa/resource" {
		t.Errorf("LoadResource 收到的参数 = %v, want [/opt/maa/resource]", resourceCalls)
	}
	table.SetConnectionExtras(cStringPtrForTest("sink"), cStringPtrForTest(`{"a":1}`))
	if len(extrasCalls) != 1 || extrasCalls[0].first != "sink" || extrasCalls[0].second != `{"a":1}` {
		t.Errorf("SetConnectionExtras 收到的参数 = %+v, want [{first:sink second:{\"a\":1}}]", extrasCalls)
	}
}

// cStringPtrForTest 在测试内构造 NUL 终止的 C 字符串指针,模拟公开
// 层完成的输入编码。
func cStringPtrForTest(s string) *byte {
	buf := make([]byte, len(s)+1)
	copy(buf, s)
	return &buf[0]
}
