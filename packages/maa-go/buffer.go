// 本文件是公开层共享的有界输出缓冲区读取辅助:为字节接口(GetImagePNG、
// GetUUID)与 int32 元素接口(GetTasksList)提供统一的容量探测、长度校验、
// 有限扩容与耗尽诊断,两类接口共用一个泛型探测核心,不维护两份校验循环。
// 字节接口按字节计量、任务列表按 int32 元素计量(每元素 4 字节),两者互不
// 混用。
//
// 输出接口不是 nil/0 查询所需容量的协议:每次探测分配实际非空的 Go
// buffer 并传入真实容量,不对空切片取首元素地址;返回值校验顺序固定为
// NullSize 哨兵 → 实际容量 → Go MaxInt → 转换/切片,返回值大于实际容量
// 按 ErrProtocol 立即终止、不扩容;合法 0 在实际非空 buffer 上返回非 nil
// 空切片。
//
// 所有权边界:探测 buffer 由本层分配、属 Go 所有,只在本层内部使用;重试
// 过程不发布旧内容,只有最终成功才把最后一次有效区间复制为独立 Go 数据。
// 耗尽诊断保留接口、单位、尝试次数、最后容量、Max 与原始返回值,并明确
// NullSize 原因不明,不输出「所需容量」、不把哨兵转换为分配长度、不编造
// LastError。
//
// 本层不接 native:read 是可注入的 raw 调用边界,由接线方提供;接线实现
// 必须用 runtime.KeepAlive 保活本层分配的 buffer 至 raw 调用结束。
// nullSize 是调用方传入的、Open 时已核验并保存在 Runtime 的哨兵,本层不
// 硬编码哨兵值。

package maa

import (
	"fmt"
	"math"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// readOutputBytes 按字节计量读取一个字节型输出缓冲区(UUID、PNG
// 等):read 收到本层分配的实际非空 buffer(长度=当前探测容量,
// 字节),返回原生原始 uint64 返回值;nullSize 与 limits 来自
// Runtime 的初始化元数据。成功返回独立 Go 数据,失败语义见
// probeOutputBuffer。
func readOutputBytes(name string, read func(buf []byte) uint64, nullSize uint64, limits BufferLimits) ([]byte, error) {
	return probeOutputBuffer(name, "bytes", 1, read, nullSize, limits)
}

// readOutputInt32s 按 int32 元素计量读取任务列表缓冲区
// (GetTasksList):分配 []int32,容量与返回值都按元素计,不换算
// 成字节;每元素 4 字节是冻结值,只用于分配前的 Go int
// 守卫,其余语义与 readOutputBytes 一致。
func readOutputInt32s(name string, read func(buf []int32) uint64, nullSize uint64, limits BufferLimits) ([]int32, error) {
	return probeOutputBuffer(name, "int32 elements", 4, read, nullSize, limits)
}

// probeOutputBuffer 是两类读取辅助共享的泛型探测核心:统一长度
// 校验与扩容循环,不复制两份实现。elemSize 是每元素
// 字节数(字节 1、int32 元素 4),unit 是诊断错误中的单位文本。
//
// 探测语义(逐条):
//   - limits 前置守卫:Initial == 0 或 Initial > Max 返回
//     ErrProtocol——Open 已按校验 Limits,此处非法即内部
//     不变量被破坏,按矛盾证据处理,不 panic、不夹取、不静默
//     截断;此时 read 绝不被调用。
//   - 每次探测先以不溢出的除法执行 Go int 守卫(capacity ≤
//     MaxInt/elemSize,守卫在分配之前,注入超大 limits 也不会
//     真的分配),再分配实际非空 buffer 并调用 read。
//   - 返回值校验顺序固定:NullSize 哨兵 → 实际容量 → Go MaxInt →
//     转换/切片;哨兵判断在任何 int 转换之前。
//   - 只有哨兵触发扩容:next = min(Max, 2*current),先用
//     current > Max/2 判断再翻倍,避免乘法溢出;已达 Max 或
//     GrowthRetries 耗尽即停止——GrowthRetries 是首次调用之后
//     至多追加的调用数,不是总调用数,总调用数 ≤ 1 +
//     GrowthRetries,Initial == Max 或 GrowthRetries == 0 都只
//     调用一次。
func probeOutputBuffer[T any](
	name string,
	unit string,
	elemSize uint64,
	read func(buf []T) uint64,
	nullSize uint64,
	limits BufferLimits,
) ([]T, error) {
	if limits.Initial == 0 || limits.Initial > limits.Max {
		return nil, fmt.Errorf("maa: %s output buffer limits are invalid (Initial=%d, Max=%d): %w",
			name, limits.Initial, limits.Max, native.ErrProtocol)
	}

	capacity := limits.Initial
	attempts := 0
	for {
		attempts++
		// 分配前 Go int 守卫:capacity*elemSize ≤ MaxInt 以除法形式
		// 检查,不先乘法溢出;容量只增不减,每次探测都重查。
		if capacity > uint64(math.MaxInt)/elemSize {
			return nil, fmt.Errorf("maa: %s buffer capacity %d %s exceeds Go int range: %w",
				name, capacity, unit, native.ErrProtocol)
		}
		// 分配实际非空的 Go buffer(容量 ≥ 1 由前置守卫保证)并传入
		// 真实容量;不用 nil/0 查询容量,不对空切片取首元素地址。
		buf := make([]T, int(capacity))
		ret := read(buf)

		// 固定校验顺序(逐字):NullSize 哨兵 → 实际容量 → Go
		// MaxInt → 转换/切片。哨兵判断必须在任何 int 转换之前,不
		// 触发巨大分配或切片越界。
		if ret == nullSize {
			// 扩容/停止决策:已达 Max(current ≥ Max,不再增长)或
			// GrowthRetries 耗尽即停止,返回 ErrBufferLimit 诊断:
			// NullSize 原因不明,可能为原生状态/参数拒绝或容量不足,
			// 不把哨兵伪造成已确认容量不足。
			if capacity >= limits.Max || uint64(attempts-1) >= uint64(limits.GrowthRetries) {
				// 原始返回值:耗尽分支只在哨兵路径可达,固定显示为
				// NullSize 字样;不输出「所需容量」、不把哨兵转换为
				// 分配长度、不编造 LastError。
				retText := "NullSize"
				return nil, fmt.Errorf("maa: %s output buffer probe exhausted after %d attempt(s): "+
					"last capacity %d %s, Max %d, native return %s; "+
					"the NullSize reason is unknown and may be a native state/parameter rejection or insufficient capacity: %w",
					name, attempts, capacity, unit, limits.Max, retText, native.ErrBufferLimit)
			}
			// next = min(Max, 2*current):先用除法判断再翻倍,避免乘法溢出。
			if limits.Max/2 < capacity {
				capacity = limits.Max
			} else {
				capacity *= 2
			}
			continue
		}
		// 返回值大于实际容量:恶意/越界原生返回不能靠事后检查兜底,
		// 立即终止、不扩容。
		if ret > capacity {
			return nil, fmt.Errorf("maa: %s native return %d exceeds the actual buffer capacity %d %s: %w",
				name, ret, capacity, unit, native.ErrProtocol)
		}
		// ret > MaxInt 在容量检查之后结构上不可达(ret ≤ capacity ≤
		// MaxInt),仍按冻结顺序显式写出,保护后续 int 转换。
		if ret > uint64(math.MaxInt) {
			return nil, fmt.Errorf("maa: %s native return %d exceeds Go int range: %w",
				name, ret, native.ErrProtocol)
		}
		// 转换/切片:复制最后一次有效区间为独立 Go 数据,
		// 可长期持有、修改,不暴露内部探测 buffer。ret == 0 时
		// make([]T, 0) 是非 nil 空切片——合法 0 在实际非空 buffer
		// 上返回空内容。
		out := make([]T, int(ret))
		copy(out, buf)
		return out, nil
	}
}
