// 本文件是有界输出缓冲区读取辅助的纯 Go 用例:白盒验证
// readOutputBytes/readOutputInt32s 的容量探测、NullSize 识别、固定校验
// 顺序、有界扩容、耗尽诊断,以及字节与 int32 元素两种单位互不混用。

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
// 非空——钉住 「每次探测分配实际非空 buffer,不用 nil/0 查询
// 容量,不对空切片取首元素地址」;再把返回值交给 respond 脚本决定。
func logRead[T any](t *testing.T, log *probeLog[T], respond func(buf []T) uint64) func(buf []T) uint64 {
	t.Helper()
	return func(buf []T) uint64 {
		log.calls++
		if len(buf) == 0 {
			t.Error("read received an empty buffer (len=0): violates the non-empty probe buffer contract")
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

// assertCaps 断言记录到的探测容量序列逐项相等,用于钉住的
// next = min(Max, 2*current) 扩容路径与各停止条件的调用次数。
func assertCaps[T any](t *testing.T, log *probeLog[T], want []int) {
	t.Helper()
	if !slices.Equal(log.caps, want) {
		t.Fatalf("probe capacity sequence = %v, want %v", log.caps, want)
	}
}

// TestReadOutputBytesReturnsIndependentCopy 钉住所有权边界:
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
		t.Fatalf("exact return must not fail: %v", err)
	}
	if !slices.Equal(got, []byte("ABCDEFGH")) {
		t.Fatalf("returned content = %q, want the known content byte-for-byte equal", got)
	}
	if &got[0] == log.firsts[0] {
		t.Fatal("returned value shares the first element address with a probe buffer: it must be copied into independent Go data before returning")
	}

	// 改写返回值后再调一次:第二次结果必须仍是 read 写入的已知
	// 内容,证明每次返回独立副本、改写不穿透。
	for i := range got {
		got[i] = 0
	}
	again, err := readOutputBytes("GetUUID", read, math.MaxUint64, limits)
	if err != nil {
		t.Fatalf("second read must not fail: %v", err)
	}
	if !slices.Equal(again, []byte("ABCDEFGH")) {
		t.Fatalf("second result was polluted = %q, want an independent copy", again)
	}
	if &again[0] == log.firsts[1] {
		t.Fatal("second returned value shares the first element address with a probe buffer")
	}
	assertCaps(t, &log, []int{8, 8})
}

// TestReadOutputBytesBoundaryAndZeroLength 钉住的两个合法返回
// 值边界:ret == capacity 恰好成功(不误判超容量);合法 0 在实际
// 非空 buffer 上返回非 nil 空切片且恰调用一次。
func TestReadOutputBytesBoundaryAndZeroLength(t *testing.T) {
	t.Run("ret==capacity exact success", func(t *testing.T) {
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 {
			for i := range buf {
				buf[i] = 0xAB
			}
			return uint64(len(buf))
		})
		got, err := readOutputBytes("GetImagePNG", read, math.MaxUint64, BufferLimits{Initial: 16, Max: 64, GrowthRetries: 2})
		if err != nil {
			t.Fatalf("ret==capacity is a legal upper bound and must not be misjudged as over-capacity: %v", err)
		}
		if len(got) != 16 {
			t.Fatalf("len = %d, want 16", len(got))
		}
		assertCaps(t, &log, []int{16})
	})

	t.Run("legal 0 returns a non-nil empty slice", func(t *testing.T) {
		// 契约「合法的 0 长度在实际非空 buffer 上返回空内容」:
		// buffer 非空已由 logRead 在 read 内断言,此处钉住结果端。
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 { return 0 })
		got, err := readOutputBytes("GetUUID", read, math.MaxUint64, BufferLimits{Initial: 8, Max: 8, GrowthRetries: 0})
		if err != nil {
			t.Fatalf("legal 0 must not fail: %v", err)
		}
		if got == nil {
			t.Fatal("legal 0 must return a non-nil empty slice, not nil")
		}
		if len(got) != 0 {
			t.Fatalf("len = %d, want 0", len(got))
		}
		if log.calls != 1 {
			t.Fatalf("legal 0 must not trigger growth, %d calls, want 1", log.calls)
		}
	})
}

// TestReadOutputBytesGrowthDoublesThenClampsToMax 钉住扩容
// 路径:只有 NullSize 触发下一次探测,next = min(Max, 2*current)——
// 正常翻倍;current > Max/2 时先用除法判断再直接夹到 Max。
func TestReadOutputBytesGrowthDoublesThenClampsToMax(t *testing.T) {
	t.Run("capacity sequence exactly 1,2,4,8 doubling", func(t *testing.T) {
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
			t.Fatalf("successful read after growth must not fail: %v", err)
		}
		assertCaps(t, &log, []int{1, 2, 4, 8})
		want := []byte{1, 2, 3, 4, 5, 6, 7, 8}
		if !slices.Equal(got, want) {
			t.Fatalf("content = %v, want %v (only the last valid range is copied)", got, want)
		}
	})

	t.Run("Initial > Max/2 clamps directly to Max", func(t *testing.T) {
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
			t.Fatalf("successful read on the clamping path must not fail: %v", err)
		}
		assertCaps(t, &log, []int{3, 5})
		if len(got) != 5 {
			t.Fatalf("len = %d, want 5", len(got))
		}
	})
}

// TestReadOutputBytesStopsAtRetryAndMaxLimits 钉住的两个停止
// 条件与调用计数语义(契约「达到最大容量或重试上限后停止,
// 不出现无限分配或重试」):GrowthRetries 是首次调用之后至多追加的
// 调用数,总调用数 ≤ 1 + GrowthRetries;已达 Max 即停,不消耗剩余
// 重试;Initial==Max 与 GrowthRetries==0 都只调用一次。
func TestReadOutputBytesStopsAtRetryAndMaxLimits(t *testing.T) {
	t.Run("GrowthRetries exhausted: exactly 1+3 calls", func(t *testing.T) {
		var log probeLog[byte]
		_, err := readOutputBytes("GetUUID", sentinelRead(t, &log, math.MaxUint64), math.MaxUint64, BufferLimits{Initial: 1, Max: 1 << 20, GrowthRetries: 3})
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		assertCaps(t, &log, []int{1, 2, 4, 8})
		if log.calls != 4 {
			t.Fatalf("%d calls, want exactly 4 (1+GrowthRetries), not more", log.calls)
		}
	})

	t.Run("reaching Max stops without consuming remaining retries", func(t *testing.T) {
		var log probeLog[byte]
		_, err := readOutputBytes("GetUUID", sentinelRead(t, &log, math.MaxUint64), math.MaxUint64, BufferLimits{Initial: 4, Max: 8, GrowthRetries: 16})
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		assertCaps(t, &log, []int{4, 8})
		if log.calls != 2 {
			t.Fatalf("stops once Max is reached, %d calls, want exactly 2, not continuing on remaining retries", log.calls)
		}
	})

	t.Run("Initial==Max only one call", func(t *testing.T) {
		var log probeLog[byte]
		_, err := readOutputBytes("GetUUID", sentinelRead(t, &log, math.MaxUint64), math.MaxUint64, BufferLimits{Initial: 4, Max: 4, GrowthRetries: 16})
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		if log.calls != 1 {
			t.Fatalf("Initial==Max must call exactly once, got %d", log.calls)
		}
	})

	t.Run("GrowthRetries==0 only one call", func(t *testing.T) {
		var log probeLog[byte]
		_, err := readOutputBytes("GetUUID", sentinelRead(t, &log, math.MaxUint64), math.MaxUint64, BufferLimits{Initial: 1, Max: 64, GrowthRetries: 0})
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		assertCaps(t, &log, []int{1})
		if log.calls != 1 {
			t.Fatalf("GrowthRetries==0 must call exactly once, got %d", log.calls)
		}
	})
}

// TestReadOutputBytesNullSizeCheckedBeforeConversion 验证
// 「NullSize 在整数转换与分配计算前识别,不触发巨大分配或切片越界」:
// nullSize=math.MaxUint64 远大于任何真实容量,若实现先做容量比较会
// 误报 ErrProtocol 甚至切片越界;契约要求哨兵判断在任何 int 转换
// 之前,最终走扩容/停止路径并以 ErrBufferLimit 结束。
func TestReadOutputBytesNullSizeCheckedBeforeConversion(t *testing.T) {
	var log probeLog[byte]
	read := logRead(t, &log, func(buf []byte) uint64 { return math.MaxUint64 })
	_, err := readOutputBytes("GetUUID", read, math.MaxUint64, BufferLimits{Initial: 8, Max: 16, GrowthRetries: 4})
	if err == nil {
		t.Fatal("a constant sentinel must end with an error")
	}
	if errors.Is(err, ErrProtocol) {
		t.Fatalf("sentinel return must not take the \"exceeds actual capacity\" branch: %v", err)
	}
	if !errors.Is(err, ErrBufferLimit) {
		t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
	}
	assertCaps(t, &log, []int{8, 16})
}

// TestReadOutputBytesRejectsOverCapacityReturn 验证「返回
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
		t.Fatalf("must terminate immediately after a protocol violation, %d calls, want 1 (no growth)", log.calls)
	}
}

// TestReadOutputBytesExhaustionDiagnostics 钉住耗尽诊断要素:
// 接口名、单位、尝试次数、最后容量、Max、原始返回值显示为 NullSize
// 字样,以及「NullSize 原因不明,可能为原生状态/参数拒绝或容量不足」
// 的固定语义;并且不把哨兵转换为十进制分配长度;两个接口的单位文本
// 各自正确、互不混用。
func TestReadOutputBytesExhaustionDiagnostics(t *testing.T) {
	t.Run("bytes interface diagnostics contain all elements", func(t *testing.T) {
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
				t.Errorf("diagnostic text lacks element %q:\n%s", want, text)
			}
		}
		// 哨兵不得被转换为十进制分配长度(2^64-1 的十进制串)。
		if strings.Contains(text, "18446744073709551615") {
			t.Errorf("diagnostics must not convert the sentinel into a decimal allocation length:\n%s", text)
		}
	})

	t.Run("int32 elements interface unit text", func(t *testing.T) {
		var log probeLog[int32]
		read := sentinelRead(t, &log, math.MaxUint64)
		_, err := readOutputInt32s("GetTasksList", read, math.MaxUint64, BufferLimits{Initial: 2, Max: 8, GrowthRetries: 1})
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		text := err.Error()
		for _, want := range []string{"GetTasksList", "int32 elements", "native return NullSize"} {
			if !strings.Contains(text, want) {
				t.Errorf("diagnostic text lacks element %q:\n%s", want, text)
			}
		}
		if strings.Contains(text, "bytes") {
			t.Errorf("element interface diagnostics must not mention the byte unit:\n%s", text)
		}
	})
}

// TestReadOutputInt32sMeasuresInElements 验证「任务列表按
// 元素计量,字节接口按字节计量,互不混用」,并给出元素侧的成功边界
// 镜像:read 收到恰为元素容量的 []int32、按元素写入与返回;
// ret==元素容量与合法 0 与字节侧一一对应。
func TestReadOutputInt32sMeasuresInElements(t *testing.T) {
	t.Run("read receives []int32 sized in element capacity", func(t *testing.T) {
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
			t.Fatalf("element interface read must not fail: %v", err)
		}
		assertCaps(t, &log, []int{4})
		want := []int32{1, 2, 3, 4}
		if !slices.Equal(got, want) {
			t.Fatalf("element content = %v, want %v", got, want)
		}
	})

	t.Run("ret==element capacity exact success", func(t *testing.T) {
		var log probeLog[int32]
		read := logRead(t, &log, func(buf []int32) uint64 { return uint64(len(buf)) })
		got, err := readOutputInt32s("GetTasksList", read, math.MaxUint64, BufferLimits{Initial: 6, Max: 12, GrowthRetries: 2})
		if err != nil {
			t.Fatalf("ret==element capacity is a legal upper bound and must not be misjudged as over-capacity: %v", err)
		}
		if len(got) != 6 {
			t.Fatalf("len = %d elements, want 6", len(got))
		}
		assertCaps(t, &log, []int{6})
	})

	t.Run("legal 0 returns a non-nil empty slice", func(t *testing.T) {
		var log probeLog[int32]
		read := logRead(t, &log, func(buf []int32) uint64 { return 0 })
		got, err := readOutputInt32s("GetTasksList", read, math.MaxUint64, BufferLimits{Initial: 4, Max: 8, GrowthRetries: 0})
		if err != nil {
			t.Fatalf("legal 0 must not fail: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("legal 0 must return a non-nil empty slice, got %#v", got)
		}
		if log.calls != 1 {
			t.Fatalf("legal 0 must not trigger growth, %d calls, want 1", log.calls)
		}
	})
}

// TestReadOutputGuardsGoIntBeforeAllocation 验证「返回长度
// ……超出 Go int 上限时返回诊断错误」与的「任务分配还先检查
// capacity*4 <= MaxInt」:Go int 守卫在分配之前,注入超大 limits 也
// 绝不真的分配、read 一次都不被调用——守卫若失效,下面的分配会
// 瞬间耗尽内存,本用例必然失败。
func TestReadOutputGuardsGoIntBeforeAllocation(t *testing.T) {
	t.Run("1<<61 int32 elements exceeds capacity*4 <= MaxInt", func(t *testing.T) {
		var log probeLog[int32]
		read := logRead(t, &log, func(buf []int32) uint64 { return 0 })
		_, err := readOutputInt32s("GetTasksList", read, math.MaxUint64, BufferLimits{Initial: 1 << 61, Max: 1 << 61, GrowthRetries: 0})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("errors.Is(err, ErrProtocol) = false; err = %v", err)
		}
		if log.calls != 0 {
			t.Fatalf("the guard must take effect before read, got %d calls", log.calls)
		}
	})

	t.Run("bytes Initial=1<<63 exceeds Go int", func(t *testing.T) {
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 { return 0 })
		_, err := readOutputBytes("GetImagePNG", read, math.MaxUint64, BufferLimits{Initial: 1 << 63, Max: 1 << 63, GrowthRetries: 0})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("errors.Is(err, ErrProtocol) = false; err = %v", err)
		}
		if log.calls != 0 {
			t.Fatalf("the guard must take effect before read, got %d calls", log.calls)
		}
	})
}

// TestReadOutputRejectsInvalidLimits 钉住前置守卫:Initial==0
// 或 Initial>Max 属内部不变量被破坏(Open 已按校验 Limits),
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
			t.Fatalf("read must never be called with invalid limits, got %d calls", log.calls)
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
			t.Fatalf("read must never be called with invalid limits, got %d calls", log.calls)
		}
	})
}

// TestReadOutputCustomNullSizeIsParameterNotConstant 钉住哨兵是调用
// 方传入的参数、本层不硬编码哨兵值(buffer.go 契约; 的锁定
// profile 哨兵在 Open 时核验后保存):非默认的小哨兵值同样触发
// NullSize 路径,而返回 0 仍为合法空结果。
func TestReadOutputCustomNullSizeIsParameterNotConstant(t *testing.T) {
	t.Run("small sentinel value takes the NullSize stop path", func(t *testing.T) {
		// nullSize=999 大于容量 8:若哨兵参数不被识别,999 会被误判
		// 为超容量 ErrProtocol;契约要求按哨兵进入扩容/停止路径。
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 { return 999 })
		_, err := readOutputBytes("GetUUID", read, 999, BufferLimits{Initial: 8, Max: 16, GrowthRetries: 4})
		if errors.Is(err, ErrProtocol) {
			t.Fatalf("the passed-in sentinel 999 must not be treated as over-capacity: %v", err)
		}
		if !errors.Is(err, ErrBufferLimit) {
			t.Fatalf("errors.Is(err, ErrBufferLimit) = false; err = %v", err)
		}
		assertCaps(t, &log, []int{8, 16})
		text := err.Error()
		if !strings.Contains(text, "NullSize") {
			t.Errorf("the sentinel return value should be shown as NullSize:\n%s", text)
		}
		if strings.Contains(text, "999") {
			t.Errorf("diagnostics must not convert the sentinel into a decimal number:\n%s", text)
		}
	})

	t.Run("returning 0 is still a legal empty result", func(t *testing.T) {
		var log probeLog[byte]
		read := logRead(t, &log, func(buf []byte) uint64 { return 0 })
		got, err := readOutputBytes("GetUUID", read, 999, BufferLimits{Initial: 8, Max: 16, GrowthRetries: 4})
		if err != nil {
			t.Fatalf("0 is not a sentinel and must be handled as a legal empty result: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("legal 0 must return a non-nil empty slice, got %#v", got)
		}
		if log.calls != 1 {
			t.Fatalf("legal 0 must not trigger growth, %d calls, want 1", log.calls)
		}
	})
}
