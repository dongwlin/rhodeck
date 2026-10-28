package maa

// InstanceDiagnostics 是实例的有界诊断快照。关闭完成前丢失计数与诊断
// 仍可能增加;观察 Done 后保持稳定。RawPrefix 每次独立复制,错误值只读,
// 保留原始错误身份以供 errors.Is 判别,不积累错误列表。
type InstanceDiagnostics struct {
	// LostEvents 是未能完整交付的事件数,饱和于 math.MaxUint64。
	LostEvents uint64
	// FirstFault 是首次事件溢出、协议错误等实例故障;正常关闭为 nil。
	FirstFault error
	// RawPrefix 是首次故障的原文前缀,至多 4096 字节,可能不是完整
	// JSON 或有效 UTF-8;没有保留原文时为 nil。调用方可安全修改它。
	RawPrefix []byte
	// StopDiagnostic 是关闭流程中 AsstStop 返回 0 的诊断;没有拒绝
	// 时为 nil。它不触发 Fault,且在 Destroy 返回前即可读取。
	StopDiagnostic error
}

// Events 返回实例的单消费者事件流。重复调用返回同一通道,只有一个
// 读取者应推进消费。事件包含独立的 Go 数据副本;未知合法消息按原文交付。
// 分发器不等待读者,消费者停读导致的溢出通过 Fault、Err 和 Diagnostics
// 可见。Close 不依赖读者;关闭后的缓冲事件仍可读取。
func (in *Instance) Events() <-chan Event {
	return in.inst.disp.events()
}

// Done 在实例关闭完成后关闭,不表示任务或 Operation 完成。届时全部
// callback 已退出、Operation 已结算、Events 已关闭、分发器已退出,
// Err 和 Diagnostics 已稳定。观察 Done 无需先消费 Events。
func (in *Instance) Done() <-chan struct{} {
	return in.inst.state.done()
}

// Err 返回当前可见的实例故障与关闭 Stop 诊断。正常 ready 阶段为 nil,
// 首次故障后即可读取原因,关闭期间可能加入停止诊断。Done 关闭或任意
// Close 返回后,Err 与 Close 返回同一份稳定最终错误,正常关闭为 nil。
func (in *Instance) Err() error {
	return in.inst.state.currentError()
}

// Fault 在首次实例故障时关闭,通知独立于 Events 的容量与消费进度。
// 它不表示关闭完成;调用方收到通知后仍须关闭实例。正常关闭或仅关闭
// Stop 拒绝不会关闭 Fault,观察者应同时选择 Done。
func (in *Instance) Fault() <-chan struct{} {
	return in.inst.state.faultNotify()
}

// Diagnostics 返回当前诊断快照,并独立复制有界的原文前缀。它与 Err
// 均可并发读取,不执行原生调用,也不要求实例仍接受业务调用。
func (in *Instance) Diagnostics() InstanceDiagnostics {
	snapshot := in.inst.state.faultDiagnostics()
	return InstanceDiagnostics{
		LostEvents:     snapshot.lostEvents,
		FirstFault:     snapshot.firstFault,
		RawPrefix:      snapshot.rawPrefix,
		StopDiagnostic: snapshot.stopDiagnostic,
	}
}
