// 本文件是图像、UUID 与任务列表读取的受控用例:验证实际非空 buffer、
// 按接口单位传容量、NullSize 与合法 0 语义、快照独立性、无隐式截图,
// 以及读取路径的调用准入与关闭等待。

package maa

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// newReadRig 构造收紧 Limits 的受控读取用例环境(复用
// newAsyncRig fake-caller 生产创建接线):PNG Initial=8 字节/Max=64/
// 重试 4,UUID Initial=16 字节/Max=64/重试 4,Tasks Initial=4 元素/
// Max=16 元素/重试 4——容量探测序列在断言中逐项可见,读取哨兵取
// Open 时核验保存的 Runtime.NullSize(锁定 profile 为 math.MaxUint64)。
func newReadRig(t *testing.T, handle uintptr) (*Instance, *createRig) {
	t.Helper()
	inst, rig := newAsyncRig(t, handle, func(l *Limits) {
		l.ImagePNG = BufferLimits{Initial: 8, Max: 64, GrowthRetries: 4}
		l.UUID = BufferLimits{Initial: 16, Max: 64, GrowthRetries: 4}
		l.Tasks = BufferLimits{Initial: 4, Max: 16, GrowthRetries: 4}
	})
	return inst, rig
}

// TestReadEntriesProbeWithNonEmptyBuffers 验证「三类 API 均
// 使用实际非空 buffer,不以 nil/0 查询所需容量」与「Tasks 的容量和
// 返回值均按 int32 元素数处理,不当作字节数」:每个接口首次探测收到
// 的容量恰为该接口 Limits.Initial(按各自单位),handle 来自已准入
// lease,fake 经 outputByteBuffer/outputInt32Buffer 守卫非空契约
// (空 buffer 即 panic);写入内容逐字返回,Payload 负 ID 保留符号位。
func TestReadEntriesProbeWithNonEmptyBuffers(t *testing.T) {
	const handle = uintptr(0x1A00)
	inst, rig := newReadRig(t, handle)

	t.Run("PNG 按字节传 Initial 容量并逐字返回内容", func(t *testing.T) {
		pngContent := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D}
		rig.caller.imageRespond = func(call fakeOutputCall) uint64 {
			// writeOutputBytes 内的 outputByteBuffer 守卫非空契约。
			return writeOutputBytes(call, pngContent)
		}
		png, err := inst.GetImagePNG()
		if err != nil {
			t.Fatalf("GetImagePNG = %v, want nil", err)
		}
		if !slices.Equal(png, pngContent) {
			t.Fatalf("GetImagePNG = %v, want 逐字节相等的已知内容", png)
		}
		rec := rig.caller.outputRecordByName(0, "getImage")
		if rec.handle != handle {
			t.Errorf("GetImagePNG 收到 handle %#x, want %#x", rec.handle, handle)
		}
		if rec.buffSize != 8 {
			t.Errorf("GetImagePNG 探测容量 = %d, want Initial 8(字节,不以 nil/0 查询容量)", rec.buffSize)
		}
	})

	t.Run("UUID 按字节传 Initial 容量", func(t *testing.T) {
		uuidContent := []byte("device-uuid")
		rig.caller.uuidRespond = func(call fakeOutputCall) uint64 {
			return writeOutputBytes(call, uuidContent)
		}
		uuid, err := inst.UUID()
		if err != nil {
			t.Fatalf("UUID = %v, want nil", err)
		}
		if uuid != string(uuidContent) {
			t.Fatalf("UUID = %q, want %q", uuid, string(uuidContent))
		}
		rec := rig.caller.outputRecordByName(0, "getUUID")
		if rec.handle != handle {
			t.Errorf("UUID 收到 handle %#x, want %#x", rec.handle, handle)
		}
		if rec.buffSize != 16 {
			t.Errorf("UUID 探测容量 = %d, want Initial 16(字节)", rec.buffSize)
		}
	})

	t.Run("Tasks 按元素传 Initial 容量且返回值按元素计", func(t *testing.T) {
		wantIDs := []int32{12, -5, 7}
		rig.caller.tasksRespond = func(call fakeOutputCall) uint64 {
			// 容量若被当作字节数,此处收到的将是 16(4 元素 ×4)而不是 4。
			return writeOutputInt32s(call, wantIDs)
		}
		tasks, err := inst.Tasks()
		if err != nil {
			t.Fatalf("Tasks = %v, want nil", err)
		}
		if !slices.Equal(tasks, []TaskID{12, -5, 7}) {
			t.Fatalf("Tasks = %v, want [12 -5 7](int32 符号位保留)", tasks)
		}
		rec := rig.caller.outputRecordByName(0, "getTasksList")
		if rec.handle != handle {
			t.Errorf("Tasks 收到 handle %#x, want %#x", rec.handle, handle)
		}
		if rec.buffSize != 4 {
			t.Errorf("Tasks 探测容量 = %d, want Initial 4(元素数,不是 16 字节)", rec.buffSize)
		}
		// 返回值 3 按元素计:结果恰 3 个 TaskID,而非 3 字节或 3×4。
		if len(tasks) != 3 {
			t.Errorf("Tasks 长度 = %d, want 3(返回值按元素数处理)", len(tasks))
		}
	})
}

// TestUUIDReadsOnlyReturnLength 验证「UUID 只读取返回长度,
// 无尾部 NUL 仍能保留完整有效内容」:fake 在应答内容之后把剩余容量
// 全部毒化且不写任何 NUL——binding 若在缓冲区内寻找 NUL 或按容量读
// 取,毒化字节即出现在结果中;ret==容量边界同样逐字成功。
func TestUUIDReadsOnlyReturnLength(t *testing.T) {
	const handle = uintptr(0x1A10)

	t.Run("短于容量的内容不夹带毒化尾部", func(t *testing.T) {
		inst, rig := newReadRig(t, handle)
		content := []byte("uuid-1234") // 9 字节,容量 16:毒化 7 字节且无 NUL
		rig.caller.uuidRespond = func(call fakeOutputCall) uint64 {
			return writeOutputBytes(call, content)
		}
		uuid, err := inst.UUID()
		if err != nil {
			t.Fatalf("UUID = %v, want nil", err)
		}
		if uuid != string(content) {
			t.Fatalf("UUID = %q(%d 字节), want 只含返回长度的 %q(9 字节)",
				uuid, len(uuid), string(content))
		}
		if strings.ContainsRune(uuid, rune(fakeOutputPoison)) {
			t.Fatalf("UUID 结果夹带毒化尾部 %q:超出返回长度读取", uuid)
		}
	})

	t.Run("ret==容量边界逐字成功", func(t *testing.T) {
		inst, rig := newReadRig(t, handle+1)
		content := []byte("0123456789abcdef") // 恰 16 字节 == Initial 容量
		rig.caller.uuidRespond = func(call fakeOutputCall) uint64 {
			return writeOutputBytes(call, content)
		}
		uuid, err := inst.UUID()
		if err != nil {
			t.Fatalf("ret==容量不得误判: %v", err)
		}
		if uuid != string(content) {
			t.Fatalf("UUID = %q, want %q", uuid, string(content))
		}
	})
}

// TestUUIDContentValidation 钉住 「内容含 NUL 或非法 UTF-8 为
// ErrProtocol」:按返回长度读取的有效区间里出现实际 NUL 或非法
// UTF-8 都是协议破坏,返回包装 ErrProtocol 的错误、不把矛盾证据伪
// 造成功;该破坏是本次调用的诊断错误(与冻结的探测层错误语
// 义一致),不使实例故障、不进入 native 重试。
func TestUUIDContentValidation(t *testing.T) {
	cases := []struct {
		what    string
		content []byte
	}{
		{"内容含 NUL", []byte("ab\x00cd")},
		{"非法 UTF-8", []byte{0xff, 0xfe, 0x01}},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			inst, rig := newReadRig(t, 0x1A20)
			rig.caller.uuidRespond = func(call fakeOutputCall) uint64 {
				return writeOutputBytes(call, tc.content)
			}
			uuid, err := inst.UUID()
			if uuid != "" {
				t.Fatalf("协议破坏不得返回内容: %q", uuid)
			}
			if !errors.Is(err, native.ErrProtocol) {
				t.Fatalf("%s: err = %v, want errors.Is ErrProtocol", tc.what, err)
			}
			if errors.Is(err, native.ErrBufferLimit) || errors.Is(err, native.ErrNativeRejected) {
				t.Errorf("%s: 错误混入相邻类别: %v", tc.what, err)
			}
			if got := rig.caller.count("getUUID"); got != 1 {
				t.Errorf("协议破坏后调用次数 = %d, want 1(立即终止,不扩容重试)", got)
			}
			// 本次调用的诊断错误不改变实例阶段:后续调用照常接受。
			if got := inst.inst.state.currentPhase(); got != phaseReady {
				t.Errorf("阶段 = %d, want 仍为 ready", got)
			}
			if inst.inst.state.currentFault() != nil {
				t.Errorf("首因 = %v, want nil(读取侧协议破坏不使实例故障)", inst.inst.state.currentFault())
			}
		})
	}
}

// TestReadEntriesNullSizeExhaustion 验证「NullSize 与扩容耗
// 尽返回明确错误」:原生恒返哨兵时按 next=min(Max, 2*current) 探测,
// 已达 Max 即停(不消耗剩余重试),返回包装 ErrBufferLimit 的诊断,
// 文本含接口名、单位与 NullSize 字样且不把哨兵转换为分配长度;三个
// 接口按各自单位与 limits 探测。
func TestReadEntriesNullSizeExhaustion(t *testing.T) {
	cases := []struct {
		what      string
		name      string
		configure func(inst *Instance, rig *createRig, nullSize uint64)
		wantCaps  []int
		wantText  []string
		banText   []string
	}{
		{
			what: "PNG", name: "getImage",
			configure: func(inst *Instance, rig *createRig, nullSize uint64) {
				rig.caller.imageRespond = func(fakeOutputCall) uint64 { return nullSize }
			},
			wantCaps: []int{8, 16, 32, 64},
			wantText: []string{"GetImage", "bytes", "after 4 attempt(s)", "last capacity 64 bytes", "Max 64", "native return NullSize"},
			banText:  []string{"18446744073709551615"},
		},
		{
			what: "UUID", name: "getUUID",
			configure: func(inst *Instance, rig *createRig, nullSize uint64) {
				rig.caller.uuidRespond = func(fakeOutputCall) uint64 { return nullSize }
			},
			wantCaps: []int{16, 32, 64},
			wantText: []string{"GetUUID", "bytes", "after 3 attempt(s)", "native return NullSize"},
			banText:  []string{"18446744073709551615"},
		},
		{
			what: "Tasks", name: "getTasksList",
			configure: func(inst *Instance, rig *createRig, nullSize uint64) {
				rig.caller.tasksRespond = func(fakeOutputCall) uint64 { return nullSize }
			},
			wantCaps: []int{4, 8, 16},
			wantText: []string{"GetTasksList", "int32 elements", "after 3 attempt(s)", "last capacity 16 int32 elements", "native return NullSize"},
			banText:  []string{"bytes"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			inst, rig := newReadRig(t, 0x1A30)
			nullSize := rig.rt.NullSize()
			tc.configure(inst, rig, nullSize)

			err := readVia(inst, tc.what)
			if !errors.Is(err, native.ErrBufferLimit) {
				t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
			}
			text := err.Error()
			for _, want := range tc.wantText {
				if !strings.Contains(text, want) {
					t.Errorf("诊断文本缺要素 %q:\n%s", want, text)
				}
			}
			for _, ban := range tc.banText {
				if strings.Contains(text, ban) {
					t.Errorf("诊断文本不得出现 %q(不把哨兵转换为分配长度/不混用单位):\n%s", ban, text)
				}
			}
			// 探测容量序列与调用次数:各接口按自身单位翻倍,达 Max 即停。
			var caps []int
			for i := 0; i < len(tc.wantCaps); i++ {
				caps = append(caps, int(rig.caller.outputRecordByName(i, tc.name).buffSize))
			}
			if !slices.Equal(caps, tc.wantCaps) {
				t.Errorf("探测容量序列 = %v, want %v", caps, tc.wantCaps)
			}
			if got := rig.caller.count(tc.name); got != len(tc.wantCaps) {
				t.Errorf("调用次数 = %d, want %d(达 Max 即停,不消耗剩余重试)", got, len(tc.wantCaps))
			}
		})
	}
}

// readVia 以接口名触达对应的公开读取入口并只取错误(供参数化用例
// 复用;结果内容由各专项用例断言)。
func readVia(inst *Instance, what string) error {
	switch what {
	case "PNG":
		_, err := inst.GetImagePNG()
		return err
	case "UUID":
		_, err := inst.UUID()
		return err
	case "Tasks":
		_, err := inst.Tasks()
		return err
	default:
		panic("unknown read entry: " + what)
	}
}

// TestReadEntriesRejectsOvercapacityReturn 验证「超长返回明
// 确错误」:原生返回值大于实际容量按 ErrProtocol 立即终止、不扩容
// (恰一次调用),不进入 ErrBufferLimit 的扩容路径。
func TestReadEntriesRejectsOvercapacityReturn(t *testing.T) {
	cases := []struct {
		what   string
		name   string
		script func(rig *createRig)
	}{
		{"PNG", "getImage", func(rig *createRig) {
			rig.caller.imageRespond = func(call fakeOutputCall) uint64 { return call.buffSize + 3 }
		}},
		{"UUID", "getUUID", func(rig *createRig) {
			rig.caller.uuidRespond = func(call fakeOutputCall) uint64 { return call.buffSize + 3 }
		}},
		{"Tasks", "getTasksList", func(rig *createRig) {
			rig.caller.tasksRespond = func(call fakeOutputCall) uint64 { return call.buffSize + 3 }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			inst, rig := newReadRig(t, 0x1A40)
			tc.script(rig)
			if err := readVia(inst, tc.what); !errors.Is(err, native.ErrProtocol) {
				t.Fatalf("%s: err = %v, want errors.Is ErrProtocol", tc.what, err)
			}
			if got := rig.caller.count(tc.name); got != 1 {
				t.Errorf("协议破坏后调用次数 = %d, want 1(立即终止,不扩容)", got)
			}
		})
	}
}

// TestReadEntriesLegalZeroLength 验证「合法 0 长度可正常返
// 回」:原生返回 0 在实际非空 buffer 上得到空结果——PNG/Tasks 为非
// nil 空切片、UUID 为空串,恰一次调用、不触发扩容;未连接设备时图
// 像 getter 也可能合法返回 0,binding 不将其伪造成截图成功或失败。
func TestReadEntriesLegalZeroLength(t *testing.T) {
	t.Run("PNG 返回非 nil 空切片", func(t *testing.T) {
		inst, rig := newReadRig(t, 0x1A50)
		png, err := inst.GetImagePNG()
		if err != nil {
			t.Fatalf("合法 0 不得失败: %v", err)
		}
		if png == nil || len(png) != 0 {
			t.Fatalf("合法 0 必须返回非 nil 空切片, got %#v", png)
		}
		if got := rig.caller.count("getImage"); got != 1 {
			t.Errorf("合法 0 不得触发扩容,调用 %d 次, want 1", got)
		}
	})
	t.Run("UUID 返回空串", func(t *testing.T) {
		inst, rig := newReadRig(t, 0x1A51)
		uuid, err := inst.UUID()
		if err != nil {
			t.Fatalf("合法 0 不得失败: %v", err)
		}
		if uuid != "" {
			t.Fatalf("合法 0 必须返回空串, got %q", uuid)
		}
		if got := rig.caller.count("getUUID"); got != 1 {
			t.Errorf("合法 0 不得触发扩容,调用 %d 次, want 1", got)
		}
	})
	t.Run("Tasks 返回非 nil 空切片", func(t *testing.T) {
		inst, rig := newReadRig(t, 0x1A52)
		tasks, err := inst.Tasks()
		if err != nil {
			t.Fatalf("合法 0 不得失败: %v", err)
		}
		if tasks == nil || len(tasks) != 0 {
			t.Fatalf("合法 0 必须返回非 nil 空切片, got %#v", tasks)
		}
		if got := rig.caller.count("getTasksList"); got != 1 {
			t.Errorf("合法 0 不得触发扩容,调用 %d 次, want 1", got)
		}
	})
}

// TestGetImagePNGDoesNotScreencapOrPinFrame 验证「读取 PNG
// 不触发隐式截图,也不承诺缓存采集时间」:读取只触达 getImage 一个
// 原生入口(fake 全调用序列无 asyncScreencap);扩容重试期间缓存可
// 能更新,第一次探测(旧帧)返回哨兵、第二次探测(新帧)返回有效
// 数据时,结果只含最后一次有效区间——binding 不发布首次旧内容、不
// 承诺重试固定同一帧。
func TestGetImagePNGDoesNotScreencapOrPinFrame(t *testing.T) {
	const handle = uintptr(0x1A60)
	inst, rig := newReadRig(t, handle)

	if _, err := inst.GetImagePNG(); err != nil {
		t.Fatalf("GetImagePNG = %v, want nil(未配置脚本即合法 0)", err)
	}
	if got := rig.caller.count("asyncScreencap"); got != 0 {
		t.Fatalf("读取 PNG 触发了隐式截图 %d 次, want 0", got)
	}
	if got := rig.caller.calls(); len(got) != 1 || got[0] != "getImage" {
		t.Fatalf("读取 PNG 的原生调用序列 = %v, want 恰 [getImage]", got)
	}

	// 扩容重试不承诺同一帧:第一次探测写入旧帧并返回哨兵,第二次探测
	// 写入新帧并返回其长度,结果必须恰为新帧(不含旧帧字节)。
	nullSize := rig.rt.NullSize()
	oldFrame := []byte("OLDFRAME")
	newFrame := []byte("NEW!F2")
	attempts := 0
	rig.caller.imageRespond = func(call fakeOutputCall) uint64 {
		attempts++
		if attempts == 1 {
			// 第一次探测写入旧帧后返回哨兵,触发一次扩容重试。
			writeOutputBytes(call, oldFrame)
			return nullSize
		}
		return writeOutputBytes(call, newFrame)
	}
	png, err := inst.GetImagePNG()
	if err != nil {
		t.Fatalf("重试后的读取 = %v, want nil", err)
	}
	if !slices.Equal(png, newFrame) {
		t.Fatalf("重试后的结果 = %q, want 恰为第二次有效区间 %q(不发布首次旧内容)", png, newFrame)
	}
	if attempts != 2 {
		t.Errorf("探测次数 = %d, want 2", attempts)
	}
}

// TestReadSnapshotsSurviveNativeAndCallerWrites 验证「返回后
// 原生内容改变或调用者修改切片,不影响已取得的 Go 快照或后续结果」:
// fake 经记录的 buffer 地址在调用返回后全容量改写原生内容(模拟
// Core 更新缓存),已取得的快照不变;调用者原地改写返回值,后续读取
// 仍取得原生写入的新内容;三个接口逐一覆盖。
func TestReadSnapshotsSurviveNativeAndCallerWrites(t *testing.T) {
	t.Run("PNG", func(t *testing.T) {
		inst, rig := newReadRig(t, 0x1A70)
		first := []byte("SNAPSHOT")
		rig.caller.imageRespond = func(call fakeOutputCall) uint64 {
			return writeOutputBytes(call, first)
		}
		snap, err := inst.GetImagePNG()
		if err != nil {
			t.Fatalf("首次读取 = %v, want nil", err)
		}
		if !slices.Equal(snap, first) {
			t.Fatalf("首次快照 = %q, want %q", snap, first)
		}
		// 原生在返回后改写缓存:快照不受影响。
		overwriteOutput(rig.caller.outputRecordByName(0, "getImage"))
		if !slices.Equal(snap, first) {
			t.Fatalf("原生改写后快照被穿透 = %q, want %q(返回前已复制)", snap, first)
		}

		// 后续读取取得新内容;调用者改写旧快照不影响它。
		second := []byte("SECOND!!")
		rig.caller.imageRespond = func(call fakeOutputCall) uint64 {
			return writeOutputBytes(call, second)
		}
		again, err := inst.GetImagePNG()
		if err != nil {
			t.Fatalf("第二次读取 = %v, want nil", err)
		}
		if !slices.Equal(again, second) {
			t.Fatalf("第二次结果被污染 = %q, want %q", again, second)
		}
		for i := range snap {
			snap[i] = 0
		}
		third, err := inst.GetImagePNG()
		if err != nil {
			t.Fatalf("第三次读取 = %v, want nil", err)
		}
		if !slices.Equal(third, second) {
			t.Fatalf("改写旧快照影响后续结果 = %q, want %q", third, second)
		}
	})

	t.Run("UUID", func(t *testing.T) {
		inst, rig := newReadRig(t, 0x1A71)
		first := []byte("uuid-first-16B")
		rig.caller.uuidRespond = func(call fakeOutputCall) uint64 {
			return writeOutputBytes(call, first)
		}
		snap, err := inst.UUID()
		if err != nil {
			t.Fatalf("首次读取 = %v, want nil", err)
		}
		if snap != string(first) {
			t.Fatalf("首次快照 = %q, want %q", snap, string(first))
		}
		overwriteOutput(rig.caller.outputRecordByName(0, "getUUID"))
		if snap != string(first) {
			t.Fatalf("原生改写后快照被穿透 = %q, want %q", snap, string(first))
		}
	})

	t.Run("Tasks", func(t *testing.T) {
		inst, rig := newReadRig(t, 0x1A72)
		rig.caller.tasksRespond = func(call fakeOutputCall) uint64 {
			return writeOutputInt32s(call, []int32{12, -5, 7})
		}
		snap, err := inst.Tasks()
		if err != nil {
			t.Fatalf("首次读取 = %v, want nil", err)
		}
		if !slices.Equal(snap, []TaskID{12, -5, 7}) {
			t.Fatalf("首次快照 = %v, want [12 -5 7]", snap)
		}
		// 原生在返回后改写缓存:快照不受影响。
		overwriteOutput(rig.caller.outputRecordByName(0, "getTasksList"))
		if !slices.Equal(snap, []TaskID{12, -5, 7}) {
			t.Fatalf("原生改写后快照被穿透 = %v, want [12 -5 7]", snap)
		}
		// 后续读取取得新列表;调用者改写旧快照不影响它。
		rig.caller.tasksRespond = func(call fakeOutputCall) uint64 {
			return writeOutputInt32s(call, []int32{1, 2})
		}
		again, err := inst.Tasks()
		if err != nil {
			t.Fatalf("第二次读取 = %v, want nil", err)
		}
		if !slices.Equal(again, []TaskID{1, 2}) {
			t.Fatalf("第二次结果被污染 = %v, want [1 2]", again)
		}
		snap[0] = 0
		third, err := inst.Tasks()
		if err != nil {
			t.Fatalf("第三次读取 = %v, want nil", err)
		}
		if !slices.Equal(third, []TaskID{1, 2}) {
			t.Fatalf("改写旧快照影响后续结果 = %v, want [1 2]", third)
		}
	})
}

// TestReadEntriesShareLeaseAndRawGate 钉住执行步骤 1「把三类读取接
// 入调用 lease 与 raw 串行门禁」:读取占据 raw 串行门禁并在 fake 内
// 挂起时,已准入的其他业务调用在同一门禁前排队,不得并发进入原生;
// 读取在途时关闭流程停在等待清零段,Stop/Destroy 不得发生,读取退出
// 后关闭才前进——lease 覆盖探测分配、raw 调用(含重试)与复制全过
// 程,在途读取不被关闭漏掉或撤销。
func TestReadEntriesShareLeaseAndRawGate(t *testing.T) {
	t.Run("读取占据 raw 门禁时其他调用排队", func(t *testing.T) {
		const handle = uintptr(0x1A80)
		inst, rig := newReadRig(t, handle)
		entered := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		rig.caller.imageRespond = func(call fakeOutputCall) uint64 {
			once.Do(func() { close(entered) })
			<-release
			return writeOutputBytes(call, []byte("OK!OK!OK"))
		}
		errRead := make(chan error, 1)
		go func() {
			_, err := inst.GetImagePNG()
			errRead <- err
		}()
		<-entered // 读取已在 fake 内挂起,占据 raw 门禁

		errRunning := make(chan error, 1)
		go func() { _, err := inst.Running(); errRunning <- err }()
		waitUntil(t, "Running 已准入并在门禁前排队", func() bool { return inFlightCalls(inst.inst.state) == 2 })
		<-time.After(notWithin)
		if got := rig.caller.count("running"); got != 0 {
			t.Fatalf("同一 raw 门禁被读取占据时 Running 已进入 native %d 次", got)
		}

		close(release)
		for _, wait := range []struct {
			what string
			ch   chan error
		}{
			{"GetImagePNG", errRead},
			{"Running", errRunning},
		} {
			select {
			case err := <-wait.ch:
				if err != nil {
					t.Fatalf("%s = %v, want nil", wait.what, err)
				}
			case <-time.After(hangGuard):
				t.Fatalf("%s 未在防挂死上限内完成", wait.what)
			}
		}
	})

	t.Run("在途读取退出前关闭不销毁", func(t *testing.T) {
		const handle = uintptr(0x1A81)
		inst, rig := newReadRig(t, handle)
		entered := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		rig.caller.imageRespond = func(call fakeOutputCall) uint64 {
			once.Do(func() { close(entered) })
			<-release
			return writeOutputBytes(call, []byte("OK!OK!OK"))
		}
		errRead := make(chan error, 1)
		go func() {
			_, err := inst.GetImagePNG()
			errRead <- err
		}()
		<-entered

		closeErr := make(chan error, 1)
		go func() { closeErr <- inst.Close() }()
		waitUntil(t, "close 完成选主转入 closing", func() bool { return inst.inst.state.currentPhase() == phaseClosing })
		<-time.After(notWithin)
		if got := rig.closer.count("stop"); got != 0 {
			t.Fatalf("在途读取未退出时 Stop 已被调用 %d 次", got)
		}
		if got := rig.closer.count("destroy"); got != 0 {
			t.Fatalf("在途读取未退出时 Destroy 已被调用 %d 次", got)
		}

		close(release)
		select {
		case err := <-errRead:
			if err != nil {
				t.Fatalf("先于关闭准入的读取 = %v, want nil(已准入调用不被关闭撤销)", err)
			}
		case <-time.After(hangGuard):
			t.Fatal("读取未在防挂死上限内完成")
		}
		select {
		case err := <-closeErr:
			if err != nil {
				t.Fatalf("Close = %v, want nil", err)
			}
		case <-time.After(hangGuard):
			t.Fatal("Close 未在防挂死上限内完成")
		}
		if seq := rig.closer.calls(); len(seq) != 2 || seq[0] != "stop" || seq[1] != "destroy" {
			t.Fatalf("native 调用序列 = %v, want [stop destroy]", seq)
		}
	})
}

// TestReadEntriesRejectByLifecycle 钉住生命周期拒绝纪律:closed 阶
// 段全部读取入口包装 ErrClosed 且无原生副作用;faulted 阶段包装首因
// (错误链可达);closing 阶段(关闭流程执行中)全部入口包装
// ErrClosing——三类读取不绕过状态调用。
func TestReadEntriesRejectByLifecycle(t *testing.T) {
	allReads := func(inst *Instance) []error {
		_, errPNG := inst.GetImagePNG()
		_, errUUID := inst.UUID()
		_, errTasks := inst.Tasks()
		return []error{errPNG, errUUID, errTasks}
	}

	t.Run("closed 阶段拒绝全部入口且无原生副作用", func(t *testing.T) {
		inst, rig := newReadRig(t, 0x1A90)
		if err := inst.Close(); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
		for i, err := range allReads(inst) {
			if !errors.Is(err, native.ErrClosed) {
				t.Errorf("读取入口 %d = %v, want errors.Is native.ErrClosed", i, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("closed 后业务调用次数 = %d, want 0(不绕过状态调用)", got)
		}
	})

	t.Run("faulted 阶段拒绝全部入口且错误链可达首因", func(t *testing.T) {
		inst, rig := newReadRig(t, 0x1A91)
		cause := errors.New("fake: first fault")
		inst.inst.state.recordFault(cause)
		for i, err := range allReads(inst) {
			if !errors.Is(err, cause) {
				t.Errorf("读取入口 %d = %v, want 沿错误链可达首因", i, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("faulted 后业务调用次数 = %d, want 0", got)
		}
	})

	t.Run("closing 阶段拒绝全部入口", func(t *testing.T) {
		inst, rig := newReadRig(t, 0x1A92)
		entered := make(chan struct{})
		release := make(chan struct{})
		rig.closer.onStop = func(uintptr) {
			close(entered)
			<-release
		}
		closeErr := make(chan error, 1)
		go func() { closeErr <- inst.Close() }()
		<-entered // 关闭所有者已进入一次 Stop(closing 已生效)。
		for i, err := range allReads(inst) {
			if !errors.Is(err, native.ErrClosing) {
				t.Errorf("closing 中读取入口 %d = %v, want errors.Is native.ErrClosing", i, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("closing 中业务调用次数 = %d, want 0", got)
		}
		close(release)
		select {
		case err := <-closeErr:
			if err != nil {
				t.Fatalf("Close = %v, want nil", err)
			}
		case <-time.After(hangGuard):
			t.Fatal("Close 未在防挂死上限内完成")
		}
	})
}
