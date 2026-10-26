// 本文件是有界输出缓冲区读取辅助的纯 Go 用例(MAA-15):对私有的
// readOutputBytes/readOutputInt32s 白盒验证 MAA-03 冻结(绑定设计
// §6.2)的容量探测语义——每次探测分配实际非空 buffer、NullSize
// 哨兵在任何转换之前识别、固定校验顺序、next = min(Max, 2*current)
// 的有界扩容、重试/Max 双停止条件、耗尽诊断要素,以及字节与 int32
// 元素两种单位互不混用。read 边界用可注入闭包模拟原生返回,不加载
// 任何原生库、不触碰 loader/Runtime,CGO_ENABLED=0 可跑。

package maa

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
)

// probeLog 记录一个 read 闭包被探测核心调用的全过程:调用次数、
// 每次收到的 buffer 容量(按该接口自身的单位计:字节或元素)与
// 首元素地址,供各用例断言容量序列、调用上限与「返回值不与探测
// buffer 别名」。
type probeLog[T any] struct {
	calls  int
	caps   []int
	firsts []*T
}

// logRead 把记录器接入 read 闭包:每次调用先断言收到的 buffer 实际
// 非空——钉住 §6.2「每次探测分配实际非空 buffer,不用 nil/0 查询
// 容量,不对空切片取首元素地址」;再把返回值交给 respond 脚本决定。
func logRead[T any](t *testing.T, log *probeLog[T], respond func(buf []T) uint64) func(buf []T) uint64 {
	t.Helper()
	return func(buf []T) uint64 {
		log.calls++
		if len(buf) == 0 {
			t.Error("read 收到空 buffer(len=0):违反 §6.2 非空探测 buffer 契约")
			return 0
		}
		log.caps = append(log.caps, len(buf))
		log.firsts = append(log.firsts, &buf[0])
		return respond(buf)
	}
}

// sentinelRead 返回恒返回哨兵值的 read 脚本(已接入记录器)。
func sentinelRead[T any](t *testing.T, log *probeLog[T], nullSize uint64) func(buf []T) uint64 {
	t.Helper()
	return logRead(t, log, func(buf []T) uint64 { return nullSize })
}

// assertCaps 断言记录到的探测容量序列逐项相等,用于钉住 §6.2 的
// next = min(Max, 2*current) 扩容路径与各停止条件的调用次数。
func assertCaps[T any](t *testing.T, log *probeLog[T], want []int) {
	t.Helper()
	if !slices.Equal(log.caps, want) {
		t.Fatalf("探测容量序列 = %v, want %v", log.caps, want)
	}
}

// TestReadOutputBytesReturnsIndependentCopy 钉住 §6.1 所有权边界:
// 成功返回是探测 buffer 有效区间的独立副本——内容逐字节相等、改写
// 返回值不影响下一次调用、返回值首元素地址不与任何一次探测 buffer
// 重合。
func TestReadOutputBytesReturnsIndependentCopy(t *testing.T) {
	var log probeLog[byte]
	read := logRead(t, &log, func(buf []byte) uint64 {
		for i := range buf {
			buf[i] = byte('A' + i)
		}
		return uint64(len(buf))
	})
	limits := BufferLimits{Initial: 8, Max: 8, GrowthRetries: 0}

	got, err := readOutputBytes("GetUUID", read, math.MaxUint64, limits)
	if err != nil {
		t.Fatalf("精确返回不得失败: %v", err)
	}
	if !slices.Equal(got, []byte("ABCDEFGH")) {
		t.Fatalf("返回内容 = %q, want 逐字节相等的已知内容", got)
	}
	if &got[0] == log.firsts[0] {
		t.Fatal("返回值与探测 buffer 共享首元素地址:§6.1 要求返回前复制为独立 Go 数据")
	}

	// 改写返回值后再调一次:第二次结果必须仍是 read 写入的已知
	// 内容,证明每次返回独立副本、改写不穿透。
	for i := range got {
		got[i] = 0
	}
	again, err := readOutputBytes("GetUUID", read, math.MaxUint64, limits)
	if err != nil {
		t.Fatalf("第二次读取不得失败: %v", err)
	}
	if !slices.Equal(again, []byte("ABCDEFGH")) {
		t.Fatalf("第二次结果被污染 = %q, want 独立副本", again)
	}
	if &again[0] == log.firsts[1] {
		t.Fatal("第二次返回值与探测 buffer 共享首元素地址")
	}
	assertCaps(t, &log, []int{8, 8})
}

// TestReadOutputBytesBoundaryAndZeroLength 钉住 §6.2 的两个合法返回
// 值边界:ret == capacity 恰好成功(不误判超容量);合法 0 在实际
// 非空 buffer 上返回非 nil 空切片且恰调用一次。
func TestReadOutputBytesBoundaryAndZeroLength(t *testing.T) {
	t.Run("ret==capacity 恰好成功", func(t *testing.T) {
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 {
			for i := range buf {
				buf[i] = 0xAB
			}
			return uint64(len(buf))
		})
		got, err := readOutputBytes("GetImagePNG", read, math.MaxUint64, BufferLimits{Initial: 16, Max: 64, GrowthRetries: 2})
		if err != nil {
			t.Fatalf("ret==capacity 是合法上限,不得误判超容量: %v", err)
		}
		if len(got) != 16 {
			t.Fatalf("len = %d, want 16", len(got))
		}
		assertCaps(t, &log, []int{16})
	})

	t.Run("合法 0 返回非 nil 空切片", func(t *testing.T) {
		// 验收条件「合法的 0 长度在实际非空 buffer 上返回空内容」:
		// buffer 非空已由 logRead 在 read 内断言,此处钉住结果端。
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 { return 0 })
		got, err := readOutputBytes("GetUUID", read, math.MaxUint64, BufferLimits{Initial: 8, Max: 8, GrowthRetries: 0})
		if err != nil {
			t.Fatalf("合法 0 不得失败: %v", err)
		}
		if got == nil {
			t.Fatal("合法 0 必须返回非 nil 空切片,不是 nil")
		}
		if len(got) != 0 {
			t.Fatalf("len = %d, want 0", len(got))
		}
		if log.calls != 1 {
			t.Fatalf("合法 0 不得触发扩容,调用 %d 次, want 1", log.calls)
		}
	})
}

// TestReadOutputBytesGrowthDoublesThenClampsToMax 钉住 §6.2 扩容
// 路径:只有 NullSize 触发下一次探测,next = min(Max, 2*current)——
// 正常翻倍;current > Max/2 时先用除法判断再直接夹到 Max。
func TestReadOutputBytesGrowthDoublesThenClampsToMax(t *testing.T) {
	t.Run("容量序列恰为 1,2,4,8 翻倍", func(t *testing.T) {
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 {
			if len(buf) < 8 {
				return math.MaxUint64
			}
			for i := 0; i < 8; i++ {
				buf[i] = byte(i + 1)
			}
			return 8
		})
		got, err := readOutputBytes("GetUUID", read, math.MaxUint64, BufferLimits{Initial: 1, Max: 16, GrowthRetries: 16})
		if err != nil {
			t.Fatalf("扩容后成功读取不得失败: %v", err)
		}
		assertCaps(t, &log, []int{1, 2, 4, 8})
		want := []byte{1, 2, 3, 4, 5, 6, 7, 8}
		if !slices.Equal(got, want) {
			t.Fatalf("内容 = %v, want %v(只复制最后一次有效区间)", got, want)
		}
	})

	t.Run("Initial > Max/2 时直接夹到 Max", func(t *testing.T) {
		// 3 > 5/2:不得先乘 2 得 6 越过 Max,必须直接夹到 5。
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 {
			if len(buf) < 5 {
				return math.MaxUint64
			}
			return 5
		})
		got, err := readOutputBytes("GetImagePNG", read, math.MaxUint64, BufferLimits{Initial: 3, Max: 5, GrowthRetries: 16})
		if err != nil {
			t.Fatalf("夹取路径上的成功读取不得失败: %v", err)
		}
		assertCaps(t, &log, []int{3, 5})
		if len(got) != 5 {
			t.Fatalf("len = %d, want 5", len(got))
		}
	})
}

// TestReadOutputBytesStopsAtRetryAndMaxLimits 钉住 §6.2 的两个停止
// 条件与调用计数语义(任务验收条件「达到最大容量或重试上限后停止,
// 不出现无限分配或重试」):GrowthRetries 是首次调用之后至多追加的
// 调用数,总调用数 ≤ 1 + GrowthRetries;已达 Max 即停,不消耗剩余
// 重试;Initial==Max 与 GrowthRetries==0 都只调用一次。
func TestReadOutputBytesStopsAtRetryAndMaxLimits(t *testing.T) {
	t.Run("GrowthRetries 耗尽后恰 1+3 次调用", func(t *testing.T) {
		var log probeLog[byte]
		_, err := readOutputBytes("GetUUID", sentinelRead(t, &log, math.MaxUint64), math.MaxUint64, BufferLimits{Initial: 1, Max: 1 << 20, GrowthRetries: 3})
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		assertCaps(t, &log, []int{1, 2, 4, 8})
		if log.calls != 4 {
			t.Fatalf("调用 %d 次, want 恰 4(1+GrowthRetries),不得再多", log.calls)
		}
	})

	t.Run("达到 Max 后不消耗剩余重试", func(t *testing.T) {
		var log probeLog[byte]
		_, err := readOutputBytes("GetUUID", sentinelRead(t, &log, math.MaxUint64), math.MaxUint64, BufferLimits{Initial: 4, Max: 8, GrowthRetries: 16})
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		assertCaps(t, &log, []int{4, 8})
		if log.calls != 2 {
			t.Fatalf("已达 Max 即停,调用 %d 次, want 恰 2,不因剩余重试继续", log.calls)
		}
	})

	t.Run("Initial==Max 只调用一次", func(t *testing.T) {
		var log probeLog[byte]
		_, err := readOutputBytes("GetUUID", sentinelRead(t, &log, math.MaxUint64), math.MaxUint64, BufferLimits{Initial: 4, Max: 4, GrowthRetries: 16})
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		if log.calls != 1 {
			t.Fatalf("Initial==Max 必须只调用一次,实际 %d", log.calls)
		}
	})

	t.Run("GrowthRetries==0 只调用一次", func(t *testing.T) {
		var log probeLog[byte]
		_, err := readOutputBytes("GetUUID", sentinelRead(t, &log, math.MaxUint64), math.MaxUint64, BufferLimits{Initial: 1, Max: 64, GrowthRetries: 0})
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		assertCaps(t, &log, []int{1})
		if log.calls != 1 {
			t.Fatalf("GrowthRetries==0 必须只调用一次,实际 %d", log.calls)
		}
	})
}

// TestReadOutputBytesNullSizeCheckedBeforeConversion 钉住验收条件
// 「NullSize 在整数转换与分配计算前识别,不触发巨大分配或切片越界」:
// nullSize=math.MaxUint64 远大于任何真实容量,若实现先做容量比较会
// 误报 ErrProtocol 甚至切片越界;契约要求哨兵判断在任何 int 转换
// 之前,最终走扩容/停止路径并以 ErrBufferLimit 结束。
func TestReadOutputBytesNullSizeCheckedBeforeConversion(t *testing.T) {
	var log probeLog[byte]
	read := logRead(t, &log, func(buf []byte) uint64 { return math.MaxUint64 })
	_, err := readOutputBytes("GetUUID", read, math.MaxUint64, BufferLimits{Initial: 8, Max: 16, GrowthRetries: 4})
	if err == nil {
		t.Fatal("恒哨兵必须以错误结束")
	}
	if errors.Is(err, ErrProtocol) {
		t.Fatalf("哨兵返回不得走「超过实际容量」分支: %v", err)
	}
	if !errors.Is(err, ErrBufferLimit) {
		t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
	}
	assertCaps(t, &log, []int{8, 16})
}

// TestReadOutputBytesRejectsOverCapacityReturn 钉住验收条件「返回
// 长度大于容量……返回诊断错误」:ret > capacity 按 ErrProtocol 立即
// 终止、不扩容——协议破坏后恰一次调用。
func TestReadOutputBytesRejectsOverCapacityReturn(t *testing.T) {
	var log probeLog[byte]
	read := logRead(t, &log, func(buf []byte) uint64 { return uint64(len(buf)) + 3 })
	_, err := readOutputBytes("GetUUID", read, math.MaxUint64, BufferLimits{Initial: 8, Max: 64, GrowthRetries: 4})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("errors.Is(err, ErrProtocol) = false; err = %v", err)
	}
	assertCaps(t, &log, []int{8})
	if log.calls != 1 {
		t.Fatalf("协议破坏后必须立即终止,调用 %d 次, want 1(不扩容)", log.calls)
	}
}

// TestReadOutputBytesExhaustionDiagnostics 钉住 §6.2 耗尽诊断要素:
// 接口名、单位、尝试次数、最后容量、Max、原始返回值显示为 NullSize
// 字样,以及「NullSize 原因不明,可能为原生状态/参数拒绝或容量不足」
// 的固定语义;并且不把哨兵转换为十进制分配长度;两个接口的单位文本
// 各自正确、互不混用。
func TestReadOutputBytesExhaustionDiagnostics(t *testing.T) {
	t.Run("bytes 接口诊断要素齐全", func(t *testing.T) {
		var log probeLog[byte]
		read := sentinelRead(t, &log, math.MaxUint64)
		_, err := readOutputBytes("GetUUID", read, math.MaxUint64, BufferLimits{Initial: 2, Max: 16, GrowthRetries: 1})
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		text := err.Error()
		for _, want := range []string{
			"GetUUID",                // 接口名
			"bytes",                  // 单位
			"after 2 attempt(s)",     // 尝试次数
			"last capacity 4 bytes",  // 最后容量
			"Max 16",                 // Max
			"native return NullSize", // 原始返回值显示为哨兵字样
			"reason is unknown",      // NullSize 原因不明
			"may be a native state/parameter rejection or insufficient capacity",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("诊断文本缺要素 %q:\n%s", want, text)
			}
		}
		// 哨兵不得被转换为十进制分配长度(2^64-1 的十进制串)。
		if strings.Contains(text, "18446744073709551615") {
			t.Errorf("诊断不得把哨兵转换为十进制分配长度:\n%s", text)
		}
	})

	t.Run("int32 elements 接口单位文本", func(t *testing.T) {
		var log probeLog[int32]
		read := sentinelRead(t, &log, math.MaxUint64)
		_, err := readOutputInt32s("GetTasksList", read, math.MaxUint64, BufferLimits{Initial: 2, Max: 8, GrowthRetries: 1})
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		text := err.Error()
		for _, want := range []string{"GetTasksList", "int32 elements", "native return NullSize"} {
			if !strings.Contains(text, want) {
				t.Errorf("诊断文本缺要素 %q:\n%s", want, text)
			}
		}
		if strings.Contains(text, "bytes") {
			t.Errorf("元素接口诊断不得出现字节单位:\n%s", text)
		}
	})
}

// TestReadOutputInt32sMeasuresInElements 钉住验收条件「任务列表按
// 元素计量,字节接口按字节计量,互不混用」,并给出元素侧的成功边界
// 镜像:read 收到恰为元素容量的 []int32、按元素写入与返回;
// ret==元素容量与合法 0 与字节侧一一对应。
func TestReadOutputInt32sMeasuresInElements(t *testing.T) {
	t.Run("read 收到元素容量的 []int32", func(t *testing.T) {
		// 闭包参数在编译期锁定为 []int32;容量按元素计:Initial=4
		// 意味着 len(buf)==4 个元素,而不是 4 字节(1 个元素)。
		var log probeLog[int32]
		read := logRead(t, &log, func(buf []int32) uint64 {
			for i := range buf {
				buf[i] = int32(i + 1)
			}
			return uint64(len(buf))
		})
		got, err := readOutputInt32s("GetTasksList", read, math.MaxUint64, BufferLimits{Initial: 4, Max: 8, GrowthRetries: 2})
		if err != nil {
			t.Fatalf("元素接口读取不得失败: %v", err)
		}
		assertCaps(t, &log, []int{4})
		want := []int32{1, 2, 3, 4}
		if !slices.Equal(got, want) {
			t.Fatalf("元素内容 = %v, want %v", got, want)
		}
	})

	t.Run("ret==元素容量恰好成功", func(t *testing.T) {
		var log probeLog[int32]
		read := logRead(t, &log, func(buf []int32) uint64 { return uint64(len(buf)) })
		got, err := readOutputInt32s("GetTasksList", read, math.MaxUint64, BufferLimits{Initial: 6, Max: 12, GrowthRetries: 2})
		if err != nil {
			t.Fatalf("ret==元素容量是合法上限,不得误判超容量: %v", err)
		}
		if len(got) != 6 {
			t.Fatalf("len = %d 元素, want 6", len(got))
		}
		assertCaps(t, &log, []int{6})
	})

	t.Run("合法 0 返回非 nil 空切片", func(t *testing.T) {
		var log probeLog[int32]
		read := logRead(t, &log, func(buf []int32) uint64 { return 0 })
		got, err := readOutputInt32s("GetTasksList", read, math.MaxUint64, BufferLimits{Initial: 4, Max: 8, GrowthRetries: 0})
		if err != nil {
			t.Fatalf("合法 0 不得失败: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("合法 0 必须返回非 nil 空切片, got %#v", got)
		}
		if log.calls != 1 {
			t.Fatalf("合法 0 不得触发扩容,调用 %d 次, want 1", log.calls)
		}
	})
}

// TestReadOutputGuardsGoIntBeforeAllocation 钉住验收条件「返回长度
// ……超出 Go int 上限时返回诊断错误」与 §6.2 的「任务分配还先检查
// capacity*4 <= MaxInt」:Go int 守卫在分配之前,注入超大 limits 也
// 绝不真的分配、read 一次都不被调用——守卫若失效,下面的分配会
// 瞬间耗尽内存,本用例必然失败。
func TestReadOutputGuardsGoIntBeforeAllocation(t *testing.T) {
	t.Run("int32 元素 1<<61 个超出 capacity*4 <= MaxInt", func(t *testing.T) {
		var log probeLog[int32]
		read := logRead(t, &log, func(buf []int32) uint64 { return 0 })
		_, err := readOutputInt32s("GetTasksList", read, math.MaxUint64, BufferLimits{Initial: 1 << 61, Max: 1 << 61, GrowthRetries: 0})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("errors.Is(err, ErrProtocol) = false; err = %v", err)
		}
		if log.calls != 0 {
			t.Fatalf("守卫必须在 read 之前生效,实际调用 %d 次", log.calls)
		}
	})

	t.Run("字节 Initial=1<<63 超出 Go int", func(t *testing.T) {
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 { return 0 })
		_, err := readOutputBytes("GetImagePNG", read, math.MaxUint64, BufferLimits{Initial: 1 << 63, Max: 1 << 63, GrowthRetries: 0})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("errors.Is(err, ErrProtocol) = false; err = %v", err)
		}
		if log.calls != 0 {
			t.Fatalf("守卫必须在 read 之前生效,实际调用 %d 次", log.calls)
		}
	})
}

// TestReadOutputRejectsInvalidLimits 钉住 §6.2 前置守卫:Initial==0
// 或 Initial>Max 属内部不变量被破坏(Open 已按 §5.1 校验 Limits),
// 按 ErrProtocol 处理,不夹取、不静默截断,read 绝不被调用。
func TestReadOutputRejectsInvalidLimits(t *testing.T) {
	t.Run("Initial==0", func(t *testing.T) {
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 { return 0 })
		_, err := readOutputBytes("GetUUID", read, math.MaxUint64, BufferLimits{Initial: 0, Max: 8, GrowthRetries: 4})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("errors.Is(err, ErrProtocol) = false; err = %v", err)
		}
		if log.calls != 0 {
			t.Fatalf("非法 limits 下 read 绝不被调用,实际 %d 次", log.calls)
		}
	})

	t.Run("Initial>Max", func(t *testing.T) {
		var log probeLog[int32]
		read := logRead(t, &log, func(buf []int32) uint64 { return 0 })
		_, err := readOutputInt32s("GetTasksList", read, math.MaxUint64, BufferLimits{Initial: 9, Max: 8, GrowthRetries: 4})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("errors.Is(err, ErrProtocol) = false; err = %v", err)
		}
		if log.calls != 0 {
			t.Fatalf("非法 limits 下 read 绝不被调用,实际 %d 次", log.calls)
		}
	})
}

// TestReadOutputCustomNullSizeIsParameterNotConstant 钉住哨兵是调用
// 方传入的参数、本层不硬编码哨兵值(buffer.go 契约;§6.2 的锁定
// profile 哨兵在 Open 时核验后保存):非默认的小哨兵值同样触发
// NullSize 路径,而返回 0 仍为合法空结果。
func TestReadOutputCustomNullSizeIsParameterNotConstant(t *testing.T) {
	t.Run("小哨兵值走 NullSize 停止路径", func(t *testing.T) {
		// nullSize=999 大于容量 8:若哨兵参数不被识别,999 会被误判
		// 为超容量 ErrProtocol;契约要求按哨兵进入扩容/停止路径。
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 { return 999 })
		_, err := readOutputBytes("GetUUID", read, 999, BufferLimits{Initial: 8, Max: 16, GrowthRetries: 4})
		if errors.Is(err, ErrProtocol) {
			t.Fatalf("传入哨兵 999 不得按超容量处理: %v", err)
		}
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		assertCaps(t, &log, []int{8, 16})
		text := err.Error()
		if !strings.Contains(text, "NullSize") {
			t.Errorf("哨兵返回值应显示为 NullSize 字样:\n%s", text)
		}
		if strings.Contains(text, "999") {
			t.Errorf("诊断不得把哨兵转换为十进制数值:\n%s", text)
		}
	})

	t.Run("返回 0 仍为合法空结果", func(t *testing.T) {
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 { return 0 })
		got, err := readOutputBytes("GetUUID", read, 999, BufferLimits{Initial: 8, Max: 16, GrowthRetries: 4})
		if err != nil {
			t.Fatalf("0 不是哨兵,必须按合法空结果处理: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("合法 0 必须返回非 nil 空切片, got %#v", got)
		}
		if log.calls != 1 {
			t.Fatalf("合法 0 不得触发扩容,调用 %d 次, want 1", log.calls)
		}
	})
}
