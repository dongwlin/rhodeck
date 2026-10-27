// 本文件实现实例的 callback 准入与原生内存复制接缝:callbackTarget 连接
// native 包的 token 注册表与实例状态/事件分发器,在原生线程上按固定顺序
// 完成「取得 callback lease → 有界确定长度 → 预留内部队列额度 → 复制 →
// 入队」。
//
// 本类型不获取 raw 串行门禁(callback 使用独立准入门);closing 阶段准入
// 门仍开放,允许必要的原生消息和同步 Destroyed 复制入队。token 注册表
// 持有投递方法值的强引用,保证已准入 callback 执行期间实例状态与分发器
// 可达;token 移除只发生在 callback 准入门已关闭且全部已准入 callback
// 退出之后,因此 deliver 执行期间状态不会被关闭流程回收。注册表锁、状态
// 锁均不跨原生内存复制:deliver 只在持有 callback lease 期间读取
// detailsJSON 指向的原生缓冲,读到的内容立即复制为 Go 拥有的字节串。
//
// panic 边界:deliver 顶部的 defer/recover 把任何 Go panic 转为实例的
// sticky fault,不越过 native 边界(nativeCallback 的外层 recover 是最后
// 防线)。本层不宣称能 recover 原生崩溃或 C++ 异常——那类失败依赖 worker
// 进程隔离。

package maa

import (
	"fmt"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// callbackTarget 是一个实例的 callback 准入接缝:私有类型,连接
// token 注册表(注册的 CallbackDelivery 形态)与实例状态/事件分发
// 器。callbackBytes 来自 Open 时 validateLimits 已校验并冻结的
// Limits.CallbackBytes(1–16,777,216,int 转换安全),本类型信任不
// 重验(对照 dispatch.go 先例);构造方传入界外值属于接线层编程错
// 误。
type callbackTarget struct {
	// state 是注入的实例共享状态:callback 准入、故障记录与丢失计
	// 数都经它完成。同包直接依赖具体类型,不设接口(既有先例)。
	state *instanceState
	// disp 是注入的有界事件分发器:复制后的消息经 admit/commit 入队。
	disp *eventDispatcher
	// callbackBytes 是单条 details JSON 的字节上限(不含 NUL),即有
	// 界寻找 NUL 的扫描范围(至多读 callbackBytes+1 字节)。
	callbackBytes uint64
}

// newCallbackTarget 构造 callback 准入接缝:state 与 disp 均不得为
// nil(准入与故障上报是核心语义),违反即编程错误 panic(对照
// dispatch.go newEventDispatcher 先例)。
func newCallbackTarget(state *instanceState, disp *eventDispatcher, callbackBytes uint64) *callbackTarget {
	if state == nil {
		panic("maa: callback target created without instance state")
	}
	if disp == nil {
		panic("maa: callback target created without event dispatcher")
	}
	return &callbackTarget{state: state, disp: disp, callbackBytes: callbackBytes}
}

// deliver 在原生线程上执行(由 dispatchCallback 在注册表查到 token
// 之后调用),顺序严格固定:
//
//  1. 顶部 defer/recover:任何 Go panic 转为 sticky fault(含 msg 与
//     panic 值,包装 native.ErrProtocol),不越过 native 边界;
//  2. admitCallback 取得 callback lease:门已关闭(!ok)直接返回,
//     绝不读取原生 details(验收红线:查到 token 但未获 lease 的
//     路径不读取原生 details);
//  3. detailsJSON 为 nil:空指针按 ErrProtocol 故障,该消息
//     未能交付,计 1 条丢失;
//  4. CallbackDetailsLength 在至多 callbackBytes+1 字节内寻找 NUL:
//     未找到按 ErrEventOverflow 故障(带 CallbackBytes 与扫描范围元
//     数据,不冒充所需容量),不投递截断 JSON,计 1 条丢失;
//  5. disp.admit 预留内部队列额度:失败直接返回(admit 内部已按溢
//     出契约上报故障并计丢失);
//  6. CopyCallbackDetails 精确复制后立即 commit 入队:先预留后复制,
//     复制后马上入队,全程在 callback 返回之前(不让并发
//     callback 在容量检查前各自分配大消息)。
//
// defer 注册顺序与执行顺序相反:recover 最先注册,lease.release 后
// 注册,LIFO 保证 panic 时先释放 lease 再记故障——已准入 callback
// 在任何阶段都必须能正常退出,否则关闭等待无法收敛。
func (t *callbackTarget) deliver(msg int32, detailsJSON *byte) {
	defer func() {
		if r := recover(); r != nil {
			t.state.recordFault(fmt.Errorf(
				"maa: callback delivery panicked on message id %d: %v: %w",
				msg, r, native.ErrProtocol))
		}
	}()

	lease, ok := t.state.admitCallback()
	if !ok {
		return
	}
	defer lease.release()

	if detailsJSON == nil {
		t.state.recordFaultWithDetail(fmt.Errorf(
			"maa: callback message id %d delivered a nil details pointer: %w",
			msg, native.ErrProtocol), nil)
		t.state.recordLostEvents(1)
		return
	}

	length, found := native.CallbackDetailsLength(detailsJSON, int(t.callbackBytes))
	if !found {
		t.state.recordFaultWithDetail(fmt.Errorf(
			"maa: callback message id %d details have no NUL within Limits.CallbackBytes %d "+
				"(scan range %d bytes): %w",
			msg, t.callbackBytes, int(t.callbackBytes)+1, native.ErrEventOverflow), nil)
		t.state.recordLostEvents(1)
		return
	}

	slot, err := t.disp.admit(MessageID(msg), length)
	if err != nil {
		return
	}
	slot.commit(native.CopyCallbackDetails(detailsJSON, length))
}
