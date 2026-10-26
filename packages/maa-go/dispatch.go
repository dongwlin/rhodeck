// 本文件实现私有有界事件分发器(MAA-17;绑定设计 §7、§7.1、§8、
// §8.1):单一 eventDispatcher 串行承接全部 callback 复制入队的消
// 息,在 Go 线程校验、分配本地事件序号、解析可关联的完成信息,并
// 按公开预算向 Events 缓冲非阻塞投递,使原生消息接收不受外部消费
// 者速度影响。供 callback 准入接线(MAA-18)、Operation 结算(MAA-19)
// 与 Stop/Destroy 关闭流程(MAA-20)复用;公开面(导出符号)零变化。
//
// 容量契约(MAA-03 冻结,§7.1):callbackBytes、internalEvents、
// internalBytes、publicEvents、publicBytes 全部由构造参数显式传入。
// 这些值只能来自 Open 时 validateLimits 已校验并冻结的 Limits 快照
// (交叉校验保证 callbackBytes ≤ internalBytes、callbackBytes ≤
// publicBytes),分发器信任不重验;构造方传入界外值属于接线层编程
// 错误。预算只计 JSON payload 字节,对象、索引和队列节点的固定开
// 销由事件数上限约束(§7.1)。
//
// 并发契约(§7、§7.1):
//   - 入口先预留后复制(§7.1:先确定有界长度、预留内部队列的事件
//     和字节额度,再分配和复制,不让并发 callback 在容量检查前各自
//     分配大消息)。admit 返回 slot,由 callback 在复制原生 details
//     后 commit 入队或 abort 退回;commit/abort 各只能调用一次。
//   - 队列额度从预留开始持有,dispatcher 解析与生成公开副本期间仍
//     计在内部预算,成功转交公开通道或明确丢失后才释放(§7.1)。
//   - 唯一 dispatcher goroutine 串行解析与投递;公开通道只有它发送
//     和关闭,账本对账依赖这一点(§7.1:单发送者和通道 FIFO 保证
//     最老的 N-L 条已被接收)。
//   - dispatcher 解析与投递全程不等待读者:内部队列唤醒、公开通道
//     发送全部非阻塞,消费者停读时入口与 dispatcher 都不会无限阻塞
//     (§7、§8.1)。
//
// 故障语义(§7、§7.1):溢出/协议故障经注入的 instanceState 以
// recordFaultWithDetail 上报(溢出包装 native.ErrEventOverflow,协议
// 包装 native.ErrProtocol,均带队列类别、预算与实际值元数据;协议
// 故障保留至多 faultRawPrefixBytes 字节原文前缀),同时 recordLostEvents
// 计丢失。首因 sticky:重复故障只累计丢失数,不覆盖首因;故障后实
// 例进入 faulted,admitCall 自然被拒(MAA-16 已有),已入队的消息
// 仍被逐条处理恰好一次,不为每条丢失事件再入队诊断事件。
//
// panic 边界:closeIntake 后 admit、slot 重复 commit/abort、commit
// 长度与预留不符、drain 前置契约违反等属于接线层编程错误,一律
// panic,不允许静默继续污染预算或丢失消息(对照 state.go 先例)。

package maa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// asyncCallInfoMessageID 是 AsstMsg::AsyncCallInfo 的消息编号,冻结自
// MaaAssistantArknights src/MaaCore/Common/AsstMsg.h(InternalError=0、
// InitFailed=1、ConnectionInfo=2、AllTasksCompleted=3、AsyncCallInfo=4)。
// 只有该消息参与必要关联字段解析;其余编号一律按未知消息原样交付。
const asyncCallInfoMessageID = MessageID(4)

// completionInfo 是一条已完成解析、可交给结算的异步完成信息(§7:
// 最终结果由 AsyncCallInfo 的 async_call_id、what、details.ret 确认)。
// 仅当三个关联字段全部存在、类型正确、无重复 key 且 ID 在 int32
// 范围内才构造;解析失败的原始消息不得调用结算通知(§7.1:不能将
// 无法关联的完成信息视为成功)。What 允许是未知文本:未知 what 按
// 原样交付,由结算方(MAA-19)决定是否可关联。Details 是 details
// 值的原始字节(§5:AsyncResult 保留原始 details),逐字节保留值内
// 部原文含内部空白、不经 map[string]any 往返;校验只针对顶层 object
// 形状与 ret 字段,未知字段原样保留(§6.1)。
type completionInfo struct {
	ID      AsyncID
	What    string
	OK      bool
	Details json.RawMessage
}

// queuedEvent 是内部队列持有的一条已复制消息:eventRecord 复用
// event.go 的构造,拥有独立字节缓冲;sequence 留待 dispatcher 处理
// 时分配(首个事件为 1),构造记录时以 0 占位。
type queuedEvent struct {
	rec eventRecord
}

// eventDispatcher 是实例唯一的有界事件分发器:内部有界队列(计数
// +字节双预算,含复制预留与在途项)、公开 Events 缓冲(chan Event,
// 容量 publicEvents)、FIFO 字节账本与单调本地序号。状态内含锁,
// 只能经指针使用,不按值复制。
type eventDispatcher struct {
	// state 是注入的实例共享状态:溢出/协议故障经它上报。同包直接
	// 依赖具体类型,不设接口(MAA-16 先例)。
	state *instanceState
	// settle 是可关联完成信息的结算通知(MAA-19 接入点):解析成功
	// 的 AsyncCallInfo 在投递公开通道之前回调,可为 nil(本单元不实
	// 现真实关联,只保证调用点与顺序)。只由 dispatcher goroutine
	// 调用,无并发。
	settle func(completionInfo)

	// 预算配置:来自 Open 时 validateLimits 已校验的 Limits 快照,
	// 此处信任不重验(见文件头)。
	callbackBytes  uint64
	internalEvents uint64
	internalBytes  uint64
	publicEvents   uint64
	publicBytes    uint64

	mu   sync.Mutex
	cond *sync.Cond

	// 内部队列:已 commit、待 dispatcher 处理的 FIFO。take 用
	// queue[1:] 前移消费,槽位及时清零解除对已复制字节的引用;
	// append 由 Go 按需重分配,驻留规模始终受双预算约束。
	queue []queuedEvent
	// count/bytes 是内部预算占用:预留中的 slot、排队项与 dispatcher
	// 在途项都计入(§7.1:InternalEvents/InternalBytes 含复制预留与
	// dispatcher 在途项),转交公开通道或明确丢失后才释放。
	count uint64
	bytes uint64

	// intakeClosed 表示入口已关闭,此后 admit 即编程错误;已准入
	// slot 的 commit/abort 不受影响。drainRequested 表示关闭所有者
	// 已请求排空,此后 commit 即编程错误(消息将无法被处理)。
	intakeClosed   bool
	drainRequested bool
	// started 防 start 双重启动;exited 由 dispatcher goroutine 在
	// 关闭公开通道后关闭一次,drain 以它为返回边界。
	started bool
	exited  chan struct{}

	// public 是公开 Events 缓冲:容量 publicEvents,只有 dispatcher
	// 发送和关闭(§7.1)。
	public chan Event
	// ledger 是公开缓冲的 FIFO 字节账本:只记录已发送 payload 的
	// 长度,不持有 RawMessage 引用(§7.1);ledgerBytes 是其中尚未
	// 观察到被接收部分的总字节。条目数恒不超过 publicEvents。
	ledger      []uint64
	ledgerBytes uint64

	// seq 是已分配的最后一个本地事件序号:首个事件为 1,单调递增,
	// 绝不复用(§7.1:序号耗尽以 ErrProtocol 故障,不复用序号)。
	seq uint64
}

// newEventDispatcher 构造事件分发器:state 不得为 nil(故障上报是
// 核心语义),settle 可为 nil;预算参数取 Open 时 validateLimits 已
// 校验并冻结的 Limits 字段值,信任不重验。构造只建立状态与公开通
// 道,不启动 goroutine;start 由接线方显式调用。
func newEventDispatcher(
	state *instanceState,
	settle func(completionInfo),
	callbackBytes, internalEvents, internalBytes, publicEvents, publicBytes uint64,
) *eventDispatcher {
	if state == nil {
		panic("maa: event dispatcher created without instance state")
	}
	d := &eventDispatcher{
		state:          state,
		settle:         settle,
		callbackBytes:  callbackBytes,
		internalEvents: internalEvents,
		internalBytes:  internalBytes,
		publicEvents:   publicEvents,
		publicBytes:    publicBytes,
		// 冻结范围 PublicEvents ≤ 65,536 保证 int 转换无损。
		public: make(chan Event, int(publicEvents)),
		exited: make(chan struct{}),
	}
	d.cond = sync.NewCond(&d.mu)
	return d
}

// eventSlot 是一次已预留、尚未入队的内部队列额度(§7.1:先预留后
// 复制):admit 在锁内登记计数与字节预留并返回 slot,callback 在
// 复制原生 details 后 commit(payload) 入队,或 abort() 退回额度。
// 准入与结算一一对应,commit 与 abort 恰择其一;零值不可用,不按
// 值复制(复制副本再结算可能双重释放预算,纪律与 callLease 相同),
// 推荐 defer abort 或立即 commit。
type eventSlot struct {
	d       *eventDispatcher
	msgID   MessageID
	length  int
	settled bool
}

// admit 在锁内为一条消息预留内部队列额度(§7.1:先确定有界长度、
// 预留事件与字节额度,再由调用方分配和复制):
//   - 已 closeIntake 后到达即编程错误,panic(关闭流程保证 callback
//     先于入口关闭被准入门拦下,§8 第 4 步);
//   - 单条 payloadLen 超过 CallbackBytes,或队列计数/字节预算不足,
//     返回包装 native.ErrEventOverflow 的带元数据错误(队列、当前
//     占用、上限、实际值),同时按溢出契约上报故障并计 1 条丢失,
//     不预留任何额度;
//   - 成功则登记预留并返回 slot,额度持续持有到 commit/abort。
//
// 错误文本含队列与预算元数据,可经 errors.Is 判别 native 哨兵。
func (d *eventDispatcher) admit(messageID MessageID, payloadLen int) (eventSlot, error) {
	d.mu.Lock()
	if d.intakeClosed {
		d.mu.Unlock()
		panic("maa: event admitted after the internal queue intake is closed")
	}
	var overflow error
	switch {
	case uint64(payloadLen) > d.callbackBytes:
		overflow = fmt.Errorf("maa: internal event queue rejects a callback message: "+
			"message id %d, payload %d bytes exceed Limits.CallbackBytes %d: %w",
			messageID, payloadLen, d.callbackBytes, native.ErrEventOverflow)
	case d.count >= d.internalEvents:
		overflow = fmt.Errorf("maa: internal event queue is full: "+
			"%d/%d events reserved, message id %d, payload %d bytes: %w",
			d.count, d.internalEvents, messageID, payloadLen, native.ErrEventOverflow)
	case d.bytes > d.internalBytes-uint64(payloadLen):
		// 减法形式检查字节预算:payloadLen ≤ callbackBytes ≤
		// internalBytes 由 validateLimits 交叉校验保证,不减出下溢。
		overflow = fmt.Errorf("maa: internal event queue is full: "+
			"%d/%d bytes reserved, message id %d, payload %d bytes: %w",
			d.bytes, d.internalBytes, messageID, payloadLen, native.ErrEventOverflow)
	}
	if overflow != nil {
		d.mu.Unlock()
		d.reportOverflow(overflow)
		return eventSlot{}, overflow
	}
	d.count++
	d.bytes += uint64(payloadLen)
	d.mu.Unlock()
	return eventSlot{d: d, msgID: messageID, length: payloadLen}, nil
}

// commit 把已复制 payload 逐字节复制入队列自有记录并唤醒 dispatcher:
// newEventRecord 立即 clone,此后调用者对 payload 的任何修改(包括
// 原地改写)都不影响队列记录。契约:
//   - len(payload) 必须等于预留长度,不等即内部不变量破坏,panic
//     (预留额度按预留值持有,长度漂移会污染字节预算);
//   - commit 与 abort 各只能调用一次,重复即 panic;
//   - drain 请求之后 commit 即编程错误,panic——消息将无法再被
//     处理,不允许静默丢失(关闭流程保证 drain 前已准入 callback
//     全部退出,§8 第 4 步先于第 5 步)。
//
// 入队后内部预算不变(预留自 admit 起持续持有,§7.1)。
func (s *eventSlot) commit(payload []byte) {
	if s.d == nil {
		panic("maa: commit on a zero event slot")
	}
	if s.settled {
		panic("maa: event slot settled more than once")
	}
	if len(payload) != s.length {
		panic(fmt.Sprintf("maa: event slot commit length %d does not match the reserved length %d",
			len(payload), s.length))
	}
	d := s.d
	s.settled = true
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.drainRequested {
		panic("maa: event slot committed after the dispatcher drain was requested")
	}
	d.queue = append(d.queue, queuedEvent{rec: newEventRecord(s.msgID, payload, 0)})
	d.cond.Broadcast()
}

// abort 释放预留额度(计数+字节):callback 复制失败、实例已故障等
// 场景退回预留,配对语义与 callLease.release 同风格——额度在状态锁
// 内精确递减,下溢即内部不变量破坏,panic。commit 与 abort 各只能
// 调用一次,重复即 panic。
func (s *eventSlot) abort() {
	if s.d == nil {
		panic("maa: abort on a zero event slot")
	}
	if s.settled {
		panic("maa: event slot settled more than once")
	}
	d := s.d
	s.settled = true
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count == 0 {
		panic("maa: event slot aborted more times than reserved")
	}
	d.count--
	d.bytes -= uint64(s.length)
}

// start 启动唯一 dispatcher goroutine:双重启动即编程错误,panic。
func (d *eventDispatcher) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		panic("maa: event dispatcher started more than once")
	}
	d.started = true
	go d.run()
}

// events 返回公开 Events 缓冲的只读视图:容量 publicEvents,只有
// dispatcher 发送和关闭(§7.1);消费者只接收。关闭后已缓冲事件仍
// 可读(§8.1)。
func (d *eventDispatcher) events() <-chan Event {
	return d.public
}

// closeIntake 关闭内部队列入口,此后 admit 即编程错误。由关闭流程
// 在 callback 准入关闭、已准入 callback 全部退出后调用(§8 第 4
// 步);至多一次,重复关闭即编程错误,panic。已入队消息不受影响,
// 由 drain 排空。
func (d *eventDispatcher) closeIntake() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.intakeClosed {
		panic("maa: event intake closed twice")
	}
	d.intakeClosed = true
}

// drain 供关闭所有者在 closeIntake 之后请求排空并等待完成(§8 第 5
// 步、§8.1):dispatcher 把内部队列剩余每项「先结算通知、再非阻塞
// 投递」处理恰好一次,无法投递的计丢失并按溢出契约记录故障,然后
// 关闭公开 Events 通道并退出 goroutine。drain 返回以通道关闭为界,
// 不等待外部消费者读取——已缓冲事件关闭后仍可读,已交付副本仍有效。
//
// 前置契约(违反即编程错误,panic):start 已调用;intake 未关即
// drain;重复 drain。已准入 callback 必须全部退出后再调用(MAA-20
// 关闭流程第 4 步保证),否则迟到的 commit 会 panic。
func (d *eventDispatcher) drain() {
	d.requestDrain()
	<-d.exited
}

// requestDrain 在状态锁内登记排空请求并广播唤醒 dispatcher:前置契约
// 检查在锁内完成,锁经 defer 释放——契约违反的 panic 不得泄漏状态
// 锁,否则 dispatcher 收尾与后续任何入口操作都会永久阻塞。
func (d *eventDispatcher) requestDrain() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started {
		panic("maa: event dispatcher drained before start")
	}
	if !d.intakeClosed {
		panic("maa: event dispatcher drained before the intake is closed")
	}
	if d.drainRequested {
		panic("maa: event dispatcher drained more than once")
	}
	d.drainRequested = true
	d.cond.Broadcast()
}

// run 是 dispatcher goroutine 主循环:逐条取出排队消息串行处理,
// 直到 drain 已请求且队列清空;退出前按 §7.1「关闭前再核账一次」
// 对账,然后关闭公开通道并通知 drain。公开通道只在此处关闭。
func (d *eventDispatcher) run() {
	for {
		item, ok := d.take()
		if !ok {
			break
		}
		d.process(item)
	}
	d.mu.Lock()
	d.reconcileLedgerLocked()
	close(d.public)
	d.mu.Unlock()
	close(d.exited)
}

// take 阻塞取出下一条排队消息:队列为空且尚未请求 drain 时在 cond
// 上挂起(挂起期间释放 mu,commit 可正常入队并广播唤醒);drain
// 已请求且队列清空时返回 false,dispatcher 随即收尾退出。取出时
// 及时清零槽位,解除对已复制字节的引用。
func (d *eventDispatcher) take() (queuedEvent, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.queue) == 0 && !d.drainRequested {
		d.cond.Wait()
	}
	if len(d.queue) == 0 {
		return queuedEvent{}, false
	}
	item := d.queue[0]
	d.queue[0] = queuedEvent{}
	d.queue = d.queue[1:]
	return item, true
}

// process 串行处理一条已复制消息(§7.1 步骤顺序;内部额度在本条
// 转交公开通道或明确丢失后释放):
//
//  1. UTF-8 与 JSON 合法性检查:非法以 ErrProtocol 故障并保留原文
//     前缀,该条仍尝试按公开预算交付原文(§7.1:界内已完整复制的
//     原文仍按公开事件预算尝试交付,不生成成功结果);
//  2. 分配本地事件序号:单调递增、首个事件为 1,耗尽以 ErrProtocol
//     故障并计丢失,绝不复用;
//  3. 仅 AsyncCallInfo 解析必要关联字段:解析失败以 ErrProtocol 故
//     障并保留原文,且不得调用结算通知;解析成功先调用结算通知,
//     再进入投递(§8 第 5 步:先确认/结算可关联结果,再非阻塞投递);
//  4. 按公开预算非阻塞投递:失败退还预算、计丢失并按溢出契约记录
//     故障。
//
// 全程不等待读者;重复故障只累计丢失数,首因保留(§7.1)。
func (d *eventDispatcher) process(item queuedEvent) {
	defer d.releaseInternal(len(item.rec.json))
	raw := item.rec.json

	// 1. UTF-8 与 JSON 合法性:json.Valid 已含语法完整性,utf8.Valid
	// 拦下非法字节序列;非法原文仍原样交付(见步骤 4),不冒充完整
	// Event 之外的成功结果。
	valid := utf8.Valid(raw) && json.Valid(raw)
	if !valid {
		d.reportProtocol(fmt.Errorf("maa: callback message id %d is not valid UTF-8/JSON "+
			"(%d bytes retained): %w", item.rec.messageID, len(raw), native.ErrProtocol), raw)
	}

	// 2. 本地事件序号:序号耗尽的条目无法成为 Event,计 1 条丢失。
	sequence, ok := d.nextSequence()
	if !ok {
		d.reportProtocol(fmt.Errorf("maa: local event sequence numbers are exhausted, "+
			"message id %d dropped: %w", item.rec.messageID, native.ErrProtocol), raw)
		d.state.recordLostEvents(1)
		return
	}

	// 3. AsyncCallInfo 必要字段解析;未知 MessageID 不解析、不故障,
	// 按原样交付(§7.1:普通未知 MessageID 和未知字段保留)。
	if item.rec.messageID == asyncCallInfoMessageID && valid {
		info, perr := parseAsyncCallInfo(raw)
		if perr != nil {
			// 无法关联的完成信息不得视为成功:故障并保留原文,不调
			// 用结算通知(§7.1)。
			d.reportProtocol(perr, raw)
		} else if d.settle != nil {
			// 先结算可关联结果,再投递公开通道(§8 第 5 步)。
			d.settle(info)
		}
	}

	// 4. 公开预算投递:失败即丢失并按溢出契约记录故障(§7.1、§8.1)。
	if !d.tryDeliver(item.rec.messageID, sequence, raw) {
		d.reportOverflow(fmt.Errorf("maa: public events buffer is full: "+
			"message id %d, event %d bytes dropped, Limits.PublicEvents %d / PublicBytes %d: %w",
			item.rec.messageID, len(raw), d.publicEvents, d.publicBytes, native.ErrEventOverflow))
	}
}

// releaseInternal 释放一条消息的内部预算占用(预留、排队与在途同
// 一账本):仅在 process 与 abort 路径到达,下溢即内部不变量破坏,
// panic。
func (d *eventDispatcher) releaseInternal(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count == 0 {
		panic("maa: internal event queue accounting underflowed")
	}
	d.count--
	d.bytes -= uint64(n)
}

// nextSequence 分配下一个本地事件序号:首个事件为 1,自增将回绕时
// 返回 false,由调用方以 ErrProtocol 故障;序号绝不复用(§7.1)。
func (d *eventDispatcher) nextSequence() (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seq == math.MaxUint64 {
		return 0, false
	}
	d.seq++
	return d.seq, true
}

// tryDeliver 对账、预留并按公开预算非阻塞发送(§7.1 逐条):
//
//   - 对账:设账本条数为 N、len(ch)=L;单发送者和通道 FIFO 保证最
//     老的 N-L 条已被接收,删除这些长度记录并扣字节。观测后又发生
//     的接收只使计费保守偏大,绝不猜测未观测的接收而提前减字节;
//   - 预留:同时检查计数与字节(公开预算);payload 字节计入预算,
//     Event 固定结构开销不计;
//   - 复制:预留通过后才分配独立深拷贝 Event(§7.1:先同时预留计
//     数与字节,再复制 Event),与内部记录不共享字节;
//   - 发送:非阻塞 select;失败(仅防御性可达)不触碰账本,额度
//     自然未登记,由调用方计溢出故障+丢失,绝不等待消费者。
//
// 发送成功与账本追加在同一锁段内完成(§7.1:同一 dispatcher 步骤,
// 下次核账前不漏记);允许保守多计,绝不低估。仅 dispatcher goroutine
// 调用。
func (d *eventDispatcher) tryDeliver(messageID MessageID, sequence uint64, payload []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reconcileLedgerLocked()

	payloadLen := uint64(len(payload))
	// payloadLen ≤ callbackBytes ≤ publicBytes 由 validateLimits 交叉
	// 校验保证,减法不下溢;账本条目数 ≤ publicEvents 保证计数 +1
	// 不回绕。
	if uint64(len(d.ledger))+1 > d.publicEvents || d.ledgerBytes > d.publicBytes-payloadLen {
		return false
	}
	ev := Event{
		MessageID: messageID,
		JSON:      bytes.Clone(payload),
		Sequence:  sequence,
	}
	select {
	case d.public <- ev:
		d.ledger = append(d.ledger, payloadLen)
		d.ledgerBytes += payloadLen
		return true
	default:
		return false
	}
}

// reconcileLedgerLocked 执行 FIFO 账本对账并扣减已确认接收的字节;
// 要求持有 mu,仅 dispatcher goroutine 调用(账本与发送严格同 goroutine,
// 对账结论因此可靠,§7.1)。
func (d *eventDispatcher) reconcileLedgerLocked() {
	drop := len(d.ledger) - len(d.public)
	if drop <= 0 {
		return
	}
	for i := 0; i < drop; i++ {
		d.ledgerBytes -= d.ledger[i]
	}
	d.ledger = append(d.ledger[:0], d.ledger[drop:]...)
}

// reportOverflow 按溢出契约上报:记录溢出故障(无原文前缀——溢出
// 消息往往尚未复制,单条超限在预留点即拒绝)并计 1 条丢失。重复
// 调用只累计丢失数,首因保留(§7.1)。
func (d *eventDispatcher) reportOverflow(cause error) {
	d.state.recordFaultWithDetail(cause, nil)
	d.state.recordLostEvents(1)
}

// reportProtocol 上报协议故障并保留至多 faultRawPrefixBytes 字节原文
// 前缀(§7.1:无效 UTF-8/JSON 与关联字段异常必须保留原文进入故障
// 流程,不得丢弃矛盾证据伪装成功);条目本身仍按步骤 4 尝试交付,
// 是否丢失由投递结果独立计数。
func (d *eventDispatcher) reportProtocol(cause error, raw []byte) {
	d.state.recordFaultWithDetail(cause, raw)
}

// parseAsyncCallInfo 用 json.Decoder + Token 流式精确解析 AsyncCallInfo
// 的必要关联字段(§7.1:原始 JSON 不经 map[string]any 的 float64 往返,
// 数字用 json.Number 并精确检查关联 ID 的 int32 范围):
//   - 顶层必须是 JSON object;提取顶层 what(string)、
//     async_call_id(int32 范围内的精确整数)与 details.ret(bool),
//     并把 details 值的原始字节整体捕获进 completionInfo(§5);
//   - 关联字段缺失、重复 key、越界或类型错误返回包装
//     native.ErrProtocol 的错误(原文由调用方保留),不得视为成功;
//   - 其余字段(uuid、details.cost/error、未来新增字段)按未知字段
//     原样保留,只跳过不解释,不故障。
//
// 调用前置契约:raw 已通过 utf8.Valid 与 json.Valid(process 步骤 1),
// 解码错误分支仅作防御。
func parseAsyncCallInfo(raw json.RawMessage) (completionInfo, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber 使数字保持 json.Number:关联 ID 经 Int64 精确解析,
	// 不经 float64 往返(§7.1)。
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message is not decodable JSON (%v): %w", err, native.ErrProtocol)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message is not a JSON object: %w", native.ErrProtocol)
	}

	var (
		info                          completionInfo
		seenWhat, seenID, seenDetails bool
	)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object is truncated (%v): %w", err, native.ErrProtocol)
		}
		key, ok := keyTok.(string)
		if !ok {
			return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object has a non-string key: %w", native.ErrProtocol)
		}
		switch key {
		case "what":
			if seenWhat {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message has duplicate top-level key %q: %w", key, native.ErrProtocol)
			}
			tok, err := dec.Token()
			if err != nil {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object is truncated (%v): %w", err, native.ErrProtocol)
			}
			what, ok := tok.(string)
			if !ok {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is %v, want a string: %w", key, tok, native.ErrProtocol)
			}
			info.What = what
			seenWhat = true
		case "async_call_id":
			if seenID {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message has duplicate top-level key %q: %w", key, native.ErrProtocol)
			}
			tok, err := dec.Token()
			if err != nil {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object is truncated (%v): %w", err, native.ErrProtocol)
			}
			num, ok := tok.(json.Number)
			if !ok {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is %v, want a number: %w", key, tok, native.ErrProtocol)
			}
			id, err := num.Int64()
			if err != nil {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is %s, want an exact integer: %w", key, num, native.ErrProtocol)
			}
			if id < math.MinInt32 || id > math.MaxInt32 {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is %d, outside the int32 range: %w", key, id, native.ErrProtocol)
			}
			info.ID = AsyncID(id)
			seenID = true
		case "details":
			if seenDetails {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message has duplicate top-level key %q: %w", key, native.ErrProtocol)
			}
			// 先整值捕获原始字节(§5:AsyncResult 保留原始 details):
			// Decode 到 json.RawMessage 逐字节保留值的内部原文(含内部
			// 空白),不经 map[string]any 的 float64 往返(§6.1)。
			var rawDetails json.RawMessage
			if err := dec.Decode(&rawDetails); err != nil {
				// 上游 json.Valid 已保证可达,防御分支。
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is not decodable JSON (%v): %w", key, err, native.ErrProtocol)
			}
			// 既有校验复用:在新 decoder 上检查顶层 object 形状并精确
			// 解析 ret,缺失/重复/类型错误与顶层非 object 的既有错误
			// 语义全部保留(§7.1:只校验必要关联字段,不因未知字段拒
			// 绝完成消息)。
			inner := json.NewDecoder(bytes.NewReader(rawDetails))
			tok, err := inner.Token()
			if err != nil {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo details object is truncated (%v): %w", err, native.ErrProtocol)
			}
			if delim, ok := tok.(json.Delim); !ok || delim != '{' {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo top-level %q is %v, want an object: %w", key, tok, native.ErrProtocol)
			}
			ret, err := parseDetailsRet(inner)
			if err != nil {
				return completionInfo{}, err
			}
			info.OK = ret
			info.Details = rawDetails
			seenDetails = true
		default:
			// 未知字段原样保留:只消费其值,不解释、不故障(§7.1)。
			if err := skipJSONValue(dec); err != nil {
				return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object is truncated (%v): %w", err, native.ErrProtocol)
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		// 收尾 '}':上游 json.Valid 已保证可达,防御分支。
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message object is truncated (%v): %w", err, native.ErrProtocol)
	}
	switch {
	case !seenWhat:
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message is missing required top-level key %q: %w", "what", native.ErrProtocol)
	case !seenID:
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message is missing required top-level key %q: %w", "async_call_id", native.ErrProtocol)
	case !seenDetails:
		return completionInfo{}, fmt.Errorf("maa: AsyncCallInfo message is missing required top-level key %q: %w", "details", native.ErrProtocol)
	}
	return info, nil
}

// parseDetailsRet 从已开启的 details object 中精确解析必要字段 ret
// (bool):缺失、重复或类型错误返回包装 native.ErrProtocol 的错误;
// 其余字段(cost、error 等)按未知字段跳过保留。调用时 '{' 已消费,
// 返回时 '}' 已消费。
func parseDetailsRet(dec *json.Decoder) (bool, error) {
	var (
		ret     bool
		seenRet bool
	)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return false, fmt.Errorf("maa: AsyncCallInfo details object is truncated (%v): %w", err, native.ErrProtocol)
		}
		key, ok := keyTok.(string)
		if !ok {
			return false, fmt.Errorf("maa: AsyncCallInfo details object has a non-string key: %w", native.ErrProtocol)
		}
		if key != "ret" {
			if err := skipJSONValue(dec); err != nil {
				return false, fmt.Errorf("maa: AsyncCallInfo details object is truncated (%v): %w", err, native.ErrProtocol)
			}
			continue
		}
		if seenRet {
			return false, fmt.Errorf("maa: AsyncCallInfo details object has duplicate key %q: %w", key, native.ErrProtocol)
		}
		tok, err := dec.Token()
		if err != nil {
			return false, fmt.Errorf("maa: AsyncCallInfo details object is truncated (%v): %w", err, native.ErrProtocol)
		}
		b, ok := tok.(bool)
		if !ok {
			return false, fmt.Errorf("maa: AsyncCallInfo details %q is %v, want a bool: %w", key, tok, native.ErrProtocol)
		}
		ret = b
		seenRet = true
	}
	if _, err := dec.Token(); err != nil {
		// 收尾 '}':防御分支,语义同上。
		return false, fmt.Errorf("maa: AsyncCallInfo details object is truncated (%v): %w", err, native.ErrProtocol)
	}
	if !seenRet {
		return false, fmt.Errorf("maa: AsyncCallInfo details object is missing required key %q: %w", "ret", native.ErrProtocol)
	}
	return ret, nil
}

// skipJSONValue 消费并丢弃一个任意 JSON 值的 token 流(标量一个
// token;object/array 按配对定界符计深),用于未知字段的透传保留。
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if _, ok := tok.(json.Delim); ok {
		depth := 1
		for depth > 0 {
			tok, err := dec.Token()
			if err != nil {
				return err
			}
			switch delim, _ := tok.(json.Delim); delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}
